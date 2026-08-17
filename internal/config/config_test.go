package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// Isolate HOME so a developer's live Vision configuration cannot affect it.
	t.Setenv("HOME", t.TempDir())
	_, err := Load("")
	// The isolated default config must not exist.
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

func TestParseYAML_RejectsInvalidSupervisionHealthCheckInterval(t *testing.T) {
	_, err := ParseYAML(`
servers:
  test:
    port: 6276
    command: echo
supervision:
  health_check_interval: 1s
`)
	if !errors.Is(err, ErrInvalidHealthCheckInterval) {
		t.Fatalf("ParseYAML() error = %v, want ErrInvalidHealthCheckInterval", err)
	}
}

func TestExpandSlotGroups_ExpandsSlotGroups(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{},
		SlotGroups: map[string]*SlotGroupConfig{
			"playwright": {
				Template:  "playwright-slot",
				BasePort:  6287,
				Count:     2,
				GroupPort: 6286,
				Defaults: &ServerConfig{
					Command: "npx",
					Args:    []string{"@playwright/mcp@latest", "--isolated"},
				},
			},
		},
	}

	err := expandSlotGroups(cfg)
	if err != nil {
		t.Fatalf("expandSlotGroups() error: %v", err)
	}

	if got := len(cfg.Servers); got != 2 {
		t.Fatalf("len(cfg.Servers) = %d, want 2", got)
	}
	if cfg.SlotGroups["playwright"] == nil {
		t.Fatal("SlotGroups[playwright] missing")
	}

	first := cfg.GetServer("playwright-slot-1")
	if first == nil {
		t.Fatal("playwright-slot-1 missing")
	}
	if first.Port != 6287 {
		t.Fatalf("playwright-slot-1 port = %d, want 6287", first.Port)
	}
	if first.SlotGroup != "playwright" {
		t.Fatalf("playwright-slot-1 SlotGroup = %q, want playwright", first.SlotGroup)
	}
	if first.SlotIndex != 1 {
		t.Fatalf("playwright-slot-1 SlotIndex = %d, want 1", first.SlotIndex)
	}
	if first.Command != "npx" {
		t.Fatalf("playwright-slot-1 Command = %q, want npx", first.Command)
	}

	second := cfg.GetServer("playwright-slot-2")
	if second == nil {
		t.Fatal("playwright-slot-2 missing")
	}
	if second.Port != 6288 {
		t.Fatalf("playwright-slot-2 port = %d, want 6288", second.Port)
	}
	if second.SlotGroup != "playwright" {
		t.Fatalf("playwright-slot-2 SlotGroup = %q, want playwright", second.SlotGroup)
	}
	if second.SlotIndex != 2 {
		t.Fatalf("playwright-slot-2 SlotIndex = %d, want 2", second.SlotIndex)
	}
}

func TestExpandSlotGroups_SlotGroupTemplateCollision(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"playwright-slot-1": {Port: 6287, Command: "echo"},
		},
		SlotGroups: map[string]*SlotGroupConfig{
			"playwright": {
				Template:  "playwright-slot",
				BasePort:  6288,
				Count:     2,
				GroupPort: 6286,
				Defaults:  &ServerConfig{Command: "npx"},
			},
		},
	}

	err := expandSlotGroups(cfg)
	if err == nil {
		t.Fatal("expandSlotGroups() expected slot-group collision error, got nil")
	}
}

func TestExpandSlotGroups_AssignsContiguousPortsFromBasePort(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{},
		SlotGroups: map[string]*SlotGroupConfig{
			"playwright": {
				Template:  "playwright-slot",
				BasePort:  6290,
				Count:     3,
				GroupPort: 6289,
				Defaults:  &ServerConfig{Command: "echo"},
			},
		},
	}

	if err := expandSlotGroups(cfg); err != nil {
		t.Fatalf("expandSlotGroups() error: %v", err)
	}

	cases := []struct {
		name string
		port int
	}{
		{"playwright-slot-1", 6290},
		{"playwright-slot-2", 6291},
		{"playwright-slot-3", 6292},
	}
	for _, tc := range cases {
		srv, ok := cfg.Servers[tc.name]
		if !ok {
			t.Fatalf("missing synthesized server %q", tc.name)
		}
		if srv.Port != tc.port {
			t.Fatalf("%s port = %d, want %d", tc.name, srv.Port, tc.port)
		}
	}
}

func TestExpandSlotGroups_SlotGroupCountMustBeAtLeastTwo(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{},
		SlotGroups: map[string]*SlotGroupConfig{
			"playwright": {
				Template:  "playwright-slot",
				BasePort:  6287,
				Count:     1,
				GroupPort: 6286,
				Defaults:  &ServerConfig{Command: "npx"},
			},
		},
	}

	err := expandSlotGroups(cfg)
	if err == nil {
		t.Fatal("expandSlotGroups() expected slot-group count error, got nil")
	}
}

func TestParseYAML_WiresExpandSlotGroups(t *testing.T) {
	yaml := `
slot_groups:
  playwright:
    template: playwright-slot
    base_port: 6287
    count: 2
    group_port: 6286
    defaults:
      command: npx
`

	cfg, err := ParseYAML(yaml)
	if err != nil {
		t.Fatalf("ParseYAML() error: %v", err)
	}
	if got := len(cfg.Servers); got != 2 {
		t.Fatalf("len(cfg.Servers) = %d, want 2", got)
	}
	if cfg.GetServer("playwright-slot-2") == nil {
		t.Fatal("playwright-slot-2 missing after ParseYAML wiring")
	}
}

func TestLoad_WiresExpandSlotGroups(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "slot-groups.yaml")

	data := `
slot_groups:
  playwright:
    template: playwright-slot
    base_port: 6287
    count: 2
    group_port: 6286
    defaults:
      command: npx
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := len(cfg.Servers); got != 2 {
		t.Fatalf("len(cfg.Servers) = %d, want 2", got)
	}
	if cfg.GetServer("playwright-slot-1") == nil {
		t.Fatal("playwright-slot-1 missing after Load wiring")
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
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		t.Fatal("Save() did not create file")
	}
	if err != nil {
		t.Fatalf("Stat() after Save() error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
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

func TestSave_RoundTripsSlotGroupsWithoutExpandedServers(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "slot-groups-config.yaml")

	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"playwright-slot-1": {
				Port:      6287,
				Command:   "npx",
				Args:      []string{"@playwright/mcp@latest", "--isolated"},
				SlotGroup: "playwright",
				SlotIndex: 1,
			},
			"playwright-slot-2": {
				Port:      6288,
				Command:   "npx",
				Args:      []string{"@playwright/mcp@latest", "--isolated"},
				SlotGroup: "playwright",
				SlotIndex: 2,
			},
		},
		SlotGroups: map[string]*SlotGroupConfig{
			"playwright": {
				Template:  "playwright-slot",
				BasePort:  6287,
				Count:     2,
				GroupPort: 6286,
				Defaults: &ServerConfig{
					Command: "npx",
					Args:    []string{"@playwright/mcp@latest", "--isolated"},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "slot_groups:") {
		t.Fatalf("saved config missing slot_groups section:\n%s", text)
	}
	if strings.Contains(text, "playwright-slot-1:") {
		t.Fatalf("saved config should not persist expanded slot server entries:\n%s", text)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Save() error: %v", err)
	}
	if len(loaded.Servers) != 2 {
		t.Fatalf("len(loaded.Servers) = %d, want 2", len(loaded.Servers))
	}
	if loaded.SlotGroups["playwright"] == nil {
		t.Fatal("SlotGroups[playwright] missing after reload")
	}
}

func TestSave_RoundTripsNetworkedManagedHTTPWithoutRefusedProfileDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "managed-http.yaml")
	const source = `servers:
  managed:
    port: 6290
    transport: managed-http
    command: npx
    url: http://127.0.0.1:16290/mcp
    availability_profile: networked
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	for _, key := range []string{
		"            shared_result_cache_ttl:",
		"            shared_result_cache_size:",
		"            max_in_flight_requests:",
		"            retry:",
		"            circuit_breaker:",
		"            health_check_interval:",
		"            session_ttl:",
	} {
		if strings.Contains(string(data), key) {
			t.Errorf("Save() materialized refused managed-http profile default %q:\n%s", key, data)
		}
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Save() error: %v", err)
	}
	managed := loaded.GetServer("managed")
	if managed == nil {
		t.Fatal("managed server missing after reload")
	}
	if managed.Retry != nil || managed.CircuitBreaker != nil || managed.HealthCheckInterval != 0 || managed.SessionTTL != 0 {
		t.Fatalf("reloaded managed-http server retained refused settings: %#v", managed)
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
