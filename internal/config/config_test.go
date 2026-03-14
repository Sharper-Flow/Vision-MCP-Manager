package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	cfg, err := Load("../../testdata/configs/valid-servers.yaml")
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// Check servers were loaded
	if len(cfg.Servers) != 2 {
		t.Errorf("Expected 2 servers, got %d", len(cfg.Servers))
	}

	// Check time server
	time := cfg.GetServer("time")
	if time == nil {
		t.Fatal("Server 'time' not found")
	}
	if time.Port != 6276 {
		t.Errorf("time.Port = %d, want 6276", time.Port)
	}
	if time.Command != "npx" {
		t.Errorf("time.Command = %q, want %q", time.Command, "npx")
	}
	if !time.Autostart {
		t.Error("time.Autostart should be true")
	}
	if time.RestartPolicy != RestartAlways {
		t.Errorf("time.RestartPolicy = %q, want %q", time.RestartPolicy, RestartAlways)
	}

	// Check echo server
	echo := cfg.GetServer("echo")
	if echo == nil {
		t.Fatal("Server 'echo' not found")
	}
	if echo.Port != 6277 {
		t.Errorf("echo.Port = %d, want 6277", echo.Port)
	}
	if echo.Autostart {
		t.Error("echo.Autostart should be false")
	}

	// Check supervision settings
	if cfg.Supervision.HealthCheckInterval.String() != "30s" {
		t.Errorf("HealthCheckInterval = %s, want 30s", cfg.Supervision.HealthCheckInterval)
	}
	if cfg.Supervision.ShutdownTimeout.String() != "10s" {
		t.Errorf("ShutdownTimeout = %s, want 10s", cfg.Supervision.ShutdownTimeout)
	}
}

func TestLoad_InvalidConfig(t *testing.T) {
	_, err := Load("../../testdata/configs/invalid-servers.yaml")
	if err == nil {
		t.Fatal("Load() expected error for invalid config, got nil")
	}
	// Should fail validation (missing command, invalid port, etc.)
	t.Logf("Expected error: %v", err)
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/to/config.yaml")
	if err == nil {
		t.Fatal("Load() expected error for nonexistent file, got nil")
	}
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("Expected ErrConfigNotFound, got: %v", err)
	}
}

func TestLoad_EmptyPath_UsesDefault(t *testing.T) {
	// This test verifies DefaultConfigPath is used when path is empty
	// Will fail if default config doesn't exist, which is expected
	_, err := Load("")
	if err == nil {
		// If it succeeds, the user has a config at the default location
		t.Log("Default config exists, skipping file-not-found check")
		return
	}
	// Should be file not found (unless user has a config there)
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("Expected ErrConfigNotFound, got: %v", err)
	}
}

func TestExpandEnvVars(t *testing.T) {
	// Set test environment variables
	os.Setenv("TEST_VAR", "test_value")
	os.Setenv("API_KEY", "secret123")
	defer os.Unsetenv("TEST_VAR")
	defer os.Unsetenv("API_KEY")

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple variable",
			input:    "value: ${TEST_VAR}",
			expected: "value: test_value",
		},
		{
			name:     "multiple variables",
			input:    "key: ${API_KEY}, other: ${TEST_VAR}",
			expected: "key: secret123, other: test_value",
		},
		{
			name:     "unset variable becomes empty",
			input:    "value: ${UNSET_VAR}",
			expected: "value: ",
		},
		{
			name:     "default value for unset",
			input:    "value: ${UNSET_VAR:-default}",
			expected: "value: default",
		},
		{
			name:     "default ignored when set",
			input:    "value: ${TEST_VAR:-ignored}",
			expected: "value: test_value",
		},
		{
			name:     "empty default",
			input:    "value: ${UNSET_VAR:-}",
			expected: "value: ",
		},
		{
			name:     "no variables",
			input:    "plain: value",
			expected: "plain: value",
		},
		{
			name:     "partial match not expanded",
			input:    "value: $TEST_VAR",
			expected: "value: $TEST_VAR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExpandEnvVars(tt.input)
			if result != tt.expected {
				t.Errorf("ExpandEnvVars(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseYAML(t *testing.T) {
	yaml := `
servers:
  test:
    port: 6276
    command: echo
    args: ["hello"]
supervision:
  health_check_interval: 1m
`
	cfg, err := ParseYAML(yaml)
	if err != nil {
		t.Fatalf("ParseYAML() error: %v", err)
	}

	if len(cfg.Servers) != 1 {
		t.Errorf("Expected 1 server, got %d", len(cfg.Servers))
	}

	test := cfg.GetServer("test")
	if test == nil {
		t.Fatal("Server 'test' not found")
	}
	if test.Port != 6276 {
		t.Errorf("Port = %d, want 6276", test.Port)
	}
	if cfg.Supervision.HealthCheckInterval.String() != "1m0s" {
		t.Errorf("HealthCheckInterval = %s, want 1m0s", cfg.Supervision.HealthCheckInterval)
	}
}

func TestParseYAML_Invalid(t *testing.T) {
	yaml := `
servers:
  bad:
    port: 99999
    command: echo
`
	_, err := ParseYAML(yaml)
	if err == nil {
		t.Fatal("ParseYAML() expected validation error, got nil")
	}
	if !errors.Is(err, ErrInvalidPort) {
		t.Errorf("Expected ErrInvalidPort, got: %v", err)
	}
}

func TestSave(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test-config.yaml")

	// Create config
	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"test": {
				Port:    6276,
				Command: "echo",
				Args:    []string{"hello"},
			},
		},
	}
	cfg.ApplyDefaults()

	// Save
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("Save() did not create file")
	}

	// Load and verify
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Save() error: %v", err)
	}

	test := loaded.GetServer("test")
	if test == nil {
		t.Fatal("Server 'test' not found after reload")
	}
	if test.Port != 6276 {
		t.Errorf("Port = %d after reload, want 6276", test.Port)
	}
	if test.Command != "echo" {
		t.Errorf("Command = %q after reload, want %q", test.Command, "echo")
	}
}

func TestLoadOrCreate(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "subdir", "config.yaml")

	// Should create parent directories and default config
	cfg, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate() error: %v", err)
	}

	// Should return empty but valid config
	if cfg == nil {
		t.Fatal("LoadOrCreate() returned nil config")
	}
	if len(cfg.Servers) != 0 {
		t.Errorf("Expected 0 servers in default config, got %d", len(cfg.Servers))
	}

	// File should exist
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("LoadOrCreate() did not create file")
	}

	// Calling again should load existing file
	cfg2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate() second call error: %v", err)
	}
	if len(cfg2.Servers) != 0 {
		t.Errorf("Second call: expected 0 servers, got %d", len(cfg2.Servers))
	}
}

func TestServerNames(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"zebra": {Port: 6278, Command: "echo"},
			"alpha": {Port: 6276, Command: "echo"},
			"beta":  {Port: 6277, Command: "echo"},
		},
	}

	names := cfg.ServerNames()
	if len(names) != 3 {
		t.Fatalf("Expected 3 names, got %d", len(names))
	}

	// Should be sorted
	expected := []string{"alpha", "beta", "zebra"}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

func TestDefaultConfigPath(t *testing.T) {
	path := DefaultConfigPath()
	if path == "" {
		t.Error("DefaultConfigPath() returned empty string")
	}

	// Should contain the expected components
	if !filepath.IsAbs(path) && path != DefaultConfigFile {
		// If not absolute, should be the fallback
		if path != DefaultConfigFile {
			t.Errorf("DefaultConfigPath() = %q, expected absolute or fallback", path)
		}
	}

	// Should end with servers.yaml
	if filepath.Base(path) != DefaultConfigFile {
		t.Errorf("DefaultConfigPath() should end with %q, got %q", DefaultConfigFile, filepath.Base(path))
	}
}

func TestGetServer_NilServers(t *testing.T) {
	cfg := &Config{Servers: nil}
	if cfg.GetServer("test") != nil {
		t.Error("GetServer() on nil map should return nil")
	}
}

func TestGetServer_NotFound(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"exists": {Port: 6276, Command: "echo"},
		},
	}
	if cfg.GetServer("missing") != nil {
		t.Error("GetServer() for missing key should return nil")
	}
}
