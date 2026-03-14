// Package config provides configuration loading, validation, and persistence
// for Vision MCP server registry.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Port range allocated for Vision MCP servers
const (
	MinPort = 6276
	MaxPort = 6300
)

// TransportType defines how Vision connects to an MCP server.
type TransportType string

const (
	// TransportStdio spawns a subprocess and bridges stdio to HTTP.
	// Requires: command (and optionally args, env)
	TransportStdio TransportType = "stdio"

	// TransportHTTP proxies to a native Streamable HTTP MCP server.
	// Requires: url (must end with /mcp)
	TransportHTTP TransportType = "http"

	// TransportSSE connects to a legacy SSE-based MCP server.
	// Requires: url
	TransportSSE TransportType = "sse"
)

// RestartPolicy defines when a server should be restarted.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

// ServerConfig defines a single MCP server's configuration.
type ServerConfig struct {
	// Port is the HTTP port Vision exposes this server on (6276-6300).
	Port int `yaml:"port" env:"PORT"`

	// Transport is "stdio", "http", or "sse". Inferred if not set.
	Transport TransportType `yaml:"transport,omitempty" env:"TRANSPORT"`

	// Command is the executable to run (for stdio transport).
	Command string `yaml:"command,omitempty" env:"COMMAND"`

	// Args are command-line arguments passed to Command.
	Args []string `yaml:"args,omitempty"`

	// Env are environment variables passed to the subprocess.
	// Values support ${VAR} expansion from the host environment.
	Env map[string]string `yaml:"env,omitempty"`

	// URL is the upstream MCP server URL (for http/sse transports).
	URL string `yaml:"url,omitempty" env:"URL"`

	// Headers are HTTP headers to forward (for http transport).
	Headers map[string]string `yaml:"headers,omitempty"`

	// Autostart determines if this server starts with the daemon.
	Autostart bool `yaml:"autostart,omitempty" env:"AUTOSTART" env-default:"false"`

	// RestartPolicy controls automatic restart behavior.
	RestartPolicy RestartPolicy `yaml:"restart_policy,omitempty" env:"RESTART_POLICY" env-default:"on-failure"`

	// MaxRestarts is the maximum restart attempts within a 5-minute window.
	MaxRestarts int `yaml:"max_restarts,omitempty" env:"MAX_RESTARTS" env-default:"5"`

	// Stateful enables process-per-session mode for isolated state.
	Stateful bool `yaml:"stateful,omitempty" env:"STATEFUL" env-default:"false"`

	// SessionTimeout is how long an idle session lives (for stateful servers).
	SessionTimeout Duration `yaml:"session_timeout,omitempty" env:"SESSION_TIMEOUT" env-default:"5m"`

	// MaxSessions limits concurrent sessions (for stateful servers, 0 = unlimited).
	MaxSessions int `yaml:"max_sessions,omitempty" env:"MAX_SESSIONS" env-default:"0"`

	// SessionTTL is the absolute maximum lifetime of a session regardless of activity.
	// 0 means no TTL (sessions only expire via idle timeout or explicit close).
	SessionTTL Duration `yaml:"session_ttl,omitempty" env:"SESSION_TTL" env-default:"0s"`

	// HealthCheckInterval is how often to probe idle downstream sessions for liveness.
	// When set, the proxy layer sends periodic tools/list calls to detect dead
	// subprocesses before a tool call hits the failure path. Must be >= 5s if set.
	// 0 means use default (30s).
	HealthCheckInterval Duration `yaml:"health_check_interval,omitempty" env:"HEALTH_CHECK_INTERVAL" env-default:"30s"`
}

// SupervisionConfig holds global supervisor settings.
type SupervisionConfig struct {
	// HealthCheckInterval is how often to check server health.
	HealthCheckInterval Duration `yaml:"health_check_interval" env:"HEALTH_CHECK_INTERVAL" env-default:"30s"`

	// ShutdownTimeout is the grace period for graceful shutdown.
	ShutdownTimeout Duration `yaml:"shutdown_timeout" env:"SHUTDOWN_TIMEOUT" env-default:"10s"`

	// RestartDelay is the initial delay before restarting a crashed server.
	RestartDelay Duration `yaml:"restart_delay" env:"RESTART_DELAY" env-default:"1s"`

	// MaxRestartDelay is the maximum delay (for exponential backoff).
	MaxRestartDelay Duration `yaml:"max_restart_delay" env:"MAX_RESTART_DELAY" env-default:"60s"`
}

// SecurityConfig holds daemon-wide security settings for MCP endpoints.
type SecurityConfig struct {
	// BearerToken is the shared secret for Authorization header validation.
	// When empty, authentication is not enforced (open access).
	// Supports ${VAR} expansion from the environment.
	BearerToken string `yaml:"bearer_token,omitempty" env:"VISION_BEARER_TOKEN"`

	// AllowedOrigins is the list of permitted Origin header values.
	// When empty, origin checking is not enforced.
	// Wildcard ("*") is explicitly rejected and treated as disallowed.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty"`
}

// Config is the root configuration structure for Vision.
type Config struct {
	// Servers maps server names to their configurations.
	Servers map[string]*ServerConfig `yaml:"servers"`

	// Security holds daemon-wide security settings for MCP endpoints.
	Security SecurityConfig `yaml:"security"`

	// Supervision holds global supervisor settings.
	Supervision SupervisionConfig `yaml:"supervision"`
}

// Duration is a time.Duration that supports YAML unmarshaling from strings like "30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler for Duration.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML implements yaml.Marshaler for Duration.
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Duration returns the underlying time.Duration value.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// String implements fmt.Stringer.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Validation errors
var (
	ErrInvalidPort                = errors.New("config: port must be between 6276 and 6300")
	ErrDuplicatePort              = errors.New("config: duplicate port assignment")
	ErrMissingCommand             = errors.New("config: stdio transport requires 'command' field")
	ErrEmptyCommand               = errors.New("config: command cannot be empty string")
	ErrMissingURL                 = errors.New("config: http/sse transport requires 'url' field")
	ErrConflictingConfig          = errors.New("config: cannot specify both 'command' and 'url'")
	ErrInvalidTransport           = errors.New("config: invalid transport type")
	ErrInvalidRestartPolicy       = errors.New("config: invalid restart_policy (must be 'always', 'on-failure', or 'never')")
	ErrInvalidHTTPURL             = errors.New("config: http transport url must end with '/mcp'")
	ErrInvalidHealthCheckInterval = errors.New("config: health_check_interval must be >= 5s")
)

// InferTransport determines the transport type from config fields.
// Returns TransportStdio if command is set, or infers from URL pattern.
func (s *ServerConfig) InferTransport() TransportType {
	// Explicit transport takes precedence
	if s.Transport != "" {
		return s.Transport
	}

	// Infer from fields
	if s.Command != "" {
		return TransportStdio
	}
	if s.URL != "" {
		// HTTP if URL ends with /mcp, otherwise assume SSE (legacy)
		if strings.HasSuffix(s.URL, "/mcp") {
			return TransportHTTP
		}
		return TransportSSE
	}

	// Default to stdio (will fail validation if command is missing)
	return TransportStdio
}

// Validate checks that the ServerConfig is valid.
func (s *ServerConfig) Validate(name string) error {
	// Port validation
	if s.Port < MinPort || s.Port > MaxPort {
		return fmt.Errorf("%w: server %q has port %d", ErrInvalidPort, name, s.Port)
	}

	// Resolve transport
	transport := s.InferTransport()

	// Validate transport type
	switch transport {
	case TransportStdio, TransportHTTP, TransportSSE:
		// valid
	default:
		return fmt.Errorf("%w: server %q has transport %q", ErrInvalidTransport, name, transport)
	}

	// Conflicting config check
	if s.Command != "" && s.URL != "" {
		return fmt.Errorf("%w: server %q specifies both command and url", ErrConflictingConfig, name)
	}

	// Transport-specific validation
	switch transport {
	case TransportStdio:
		if s.Command == "" {
			return fmt.Errorf("%w: server %q", ErrMissingCommand, name)
		}
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("%w: server %q", ErrEmptyCommand, name)
		}
	case TransportHTTP:
		if s.URL == "" {
			return fmt.Errorf("%w: server %q", ErrMissingURL, name)
		}
		if !strings.HasSuffix(s.URL, "/mcp") {
			return fmt.Errorf("%w: server %q has url %q", ErrInvalidHTTPURL, name, s.URL)
		}
	case TransportSSE:
		if s.URL == "" {
			return fmt.Errorf("%w: server %q", ErrMissingURL, name)
		}
	}

	// Health check interval validation (0 means use default, >0 must be >= 5s)
	if s.HealthCheckInterval > 0 && time.Duration(s.HealthCheckInterval) < 5*time.Second {
		return fmt.Errorf("%w: server %q has health_check_interval %v", ErrInvalidHealthCheckInterval, name, time.Duration(s.HealthCheckInterval))
	}

	// Restart policy validation
	switch s.RestartPolicy {
	case "", RestartAlways, RestartOnFailure, RestartNever:
		// valid (empty defaults to on-failure)
	default:
		return fmt.Errorf("%w: server %q has restart_policy %q", ErrInvalidRestartPolicy, name, s.RestartPolicy)
	}

	return nil
}

// Validate checks that the entire Config is valid.
// An empty config (no servers) is considered valid for first-run scenarios.
func (c *Config) Validate() error {
	// Empty config is valid (first-run scenario)
	if len(c.Servers) == 0 {
		return nil
	}

	// Track ports to detect duplicates
	usedPorts := make(map[int]string)

	for name, server := range c.Servers {
		if err := server.Validate(name); err != nil {
			return err
		}

		// Check for duplicate ports
		if existing, ok := usedPorts[server.Port]; ok {
			return fmt.Errorf("%w: port %d used by both %q and %q", ErrDuplicatePort, server.Port, existing, name)
		}
		usedPorts[server.Port] = name
	}

	return nil
}

// ApplyDefaults sets default values for missing optional fields.
func (s *ServerConfig) ApplyDefaults() {
	if s.RestartPolicy == "" {
		s.RestartPolicy = RestartOnFailure
	}
	if s.MaxRestarts == 0 {
		s.MaxRestarts = 5
	}
	if s.SessionTimeout == 0 {
		s.SessionTimeout = Duration(5 * time.Minute)
	}
	if s.HealthCheckInterval == 0 {
		s.HealthCheckInterval = Duration(30 * time.Second)
	}
}

// ApplyDefaults sets default values for the supervision config.
func (sup *SupervisionConfig) ApplyDefaults() {
	if sup.HealthCheckInterval == 0 {
		sup.HealthCheckInterval = Duration(30 * time.Second)
	}
	if sup.ShutdownTimeout == 0 {
		sup.ShutdownTimeout = Duration(10 * time.Second)
	}
	if sup.RestartDelay == 0 {
		sup.RestartDelay = Duration(1 * time.Second)
	}
	if sup.MaxRestartDelay == 0 {
		sup.MaxRestartDelay = Duration(60 * time.Second)
	}
}

// ApplyDefaults sets default values for all servers and supervision.
func (c *Config) ApplyDefaults() {
	for _, server := range c.Servers {
		server.ApplyDefaults()
	}
	c.Supervision.ApplyDefaults()
}
