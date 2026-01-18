package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default paths
const (
	// DefaultConfigDir is the XDG-compliant config directory
	DefaultConfigDir = ".config/vision"
	// DefaultConfigFile is the default filename
	DefaultConfigFile = "servers.yaml"
)

var (
	// ErrConfigNotFound is returned when the config file doesn't exist
	ErrConfigNotFound = errors.New("config: file not found")
	// ErrConfigParse is returned when the config file can't be parsed
	ErrConfigParse = errors.New("config: parse error")
)

// envVarPattern matches ${VAR} or ${VAR:-default} patterns
var envVarPattern = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

// DefaultConfigPath returns the default config file path: ~/.config/vision/servers.yaml
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory if home is unavailable
		return DefaultConfigFile
	}
	return filepath.Join(home, DefaultConfigDir, DefaultConfigFile)
}

// Load reads and parses a config file from the given path.
// If path is empty, it uses DefaultConfigPath().
// Environment variables in the format ${VAR} or ${VAR:-default} are expanded.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrConfigNotFound, path)
		}
		return nil, fmt.Errorf("config: read error: %w", err)
	}

	// Expand environment variables before parsing
	expanded := ExpandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigParse, err)
	}

	// Apply defaults for missing values
	cfg.ApplyDefaults()

	// Validate the config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// LoadOrCreate loads a config file, creating a default one if it doesn't exist.
// This is useful for first-run scenarios.
func LoadOrCreate(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}

	// Only create if file not found
	if !errors.Is(err, ErrConfigNotFound) {
		return nil, err
	}

	// Create default config
	cfg = DefaultConfig()

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("config: create directory: %w", err)
	}

	// Save default config
	if err := Save(cfg, path); err != nil {
		return nil, err
	}

	return cfg, nil
}

// DefaultConfig returns a minimal default configuration.
func DefaultConfig() *Config {
	cfg := &Config{
		Servers: make(map[string]*ServerConfig),
	}
	cfg.ApplyDefaults()
	return cfg
}

// ExpandEnvVars expands environment variables in the format ${VAR} or ${VAR:-default}.
func ExpandEnvVars(input string) string {
	return envVarPattern.ReplaceAllStringFunc(input, func(match string) string {
		// Extract variable name and optional default
		parts := envVarPattern.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}

		varName := parts[1]
		defaultVal := ""
		if len(parts) > 2 {
			defaultVal = parts[2]
		}

		// Look up environment variable
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}

		// Return default if provided, otherwise empty string
		return defaultVal
	})
}

// Save writes a config to the given path using atomic write.
// It writes to a temp file first, then renames for atomicity.
func Save(cfg *Config, path string) error {
	// Marshal config to YAML
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal error: %w", err)
	}

	// Add header comment
	header := "# Vision Server Registry\n# See: https://github.com/jrede/vision\n\n"
	fullData := []byte(header + string(data))

	// Write atomically: create temp file, write, rename
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".vision-config-*.yaml")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on error
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(fullData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("config: write temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("config: close temp file: %w", err)
	}

	// Set appropriate permissions
	if err := os.Chmod(tmpPath, 0644); err != nil {
		return fmt.Errorf("config: chmod temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: rename temp file: %w", err)
	}

	// Clear tmpPath so deferred cleanup doesn't remove the final file
	tmpPath = ""

	return nil
}

// MustLoad loads a config file, panicking on error.
// Useful for tests and initialization where errors should be fatal.
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(fmt.Sprintf("config: failed to load %s: %v", path, err))
	}
	return cfg
}

// ParseYAML parses a YAML string into a Config.
// Useful for testing or loading embedded configs.
func ParseYAML(data string) (*Config, error) {
	expanded := ExpandEnvVars(data)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigParse, err)
	}

	cfg.ApplyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// GetServer returns a server config by name, or nil if not found.
func (c *Config) GetServer(name string) *ServerConfig {
	if c.Servers == nil {
		return nil
	}
	return c.Servers[name]
}

// ServerNames returns a sorted list of server names.
func (c *Config) ServerNames() []string {
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	// Sort for consistent ordering
	sortStrings(names)
	return names
}

// sortStrings sorts a slice of strings in place.
func sortStrings(s []string) {
	for i := 0; i < len(s)-1; i++ {
		for j := i + 1; j < len(s); j++ {
			if strings.Compare(s[i], s[j]) > 0 {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
