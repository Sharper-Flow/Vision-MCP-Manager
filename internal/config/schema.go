// Package config provides configuration loading, validation, and persistence
// for Vision MCP server registry.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Port range allocated for Vision MCP servers.
//
// The range is 6276–6325 (50 ports). It was originally 6276–6300 (25 ports)
// before slot groups existed; v1.1.1 doubled the range to give realistic
// deployments enough headroom for multiple slot groups (each group consumes
// count + 1 ports: slots + virtual group port).
const (
	MinPort = 6276
	MaxPort = 6325
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

	// TransportManagedHTTP supervises a local native Streamable HTTP MCP
	// process and exposes it through Vision's authenticated listener.
	// Requires: command and a loopback URL ending exactly in /mcp.
	TransportManagedHTTP TransportType = "managed-http"

	// TransportSSE connects to a legacy SSE-based MCP server.
	// Requires: url
	TransportSSE TransportType = "sse"
)

// IsReachabilityProbeable reports whether Vision owns a listener it can probe.
// Externally owned http and sse endpoints are reached through Config.URL, so
// Vision deliberately does not apply listener reachability evidence to them.
func (t TransportType) IsReachabilityProbeable() bool {
	return t == TransportStdio || t == TransportManagedHTTP
}

// RestartPolicy defines when a server should be restarted.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

// AvailabilityProfile defines opinionated resilience defaults for a server.
type AvailabilityProfile string

const (
	// AvailabilityProfileNetworked applies stronger timeout/retry/circuit defaults
	// for servers whose tool calls depend on upstream network providers.
	AvailabilityProfileNetworked AvailabilityProfile = "networked"
)

// ServerConfig defines a single MCP server's configuration.
type ServerConfig struct {
	// Port is the HTTP port Vision exposes this server on (6276-6300).
	Port int `yaml:"port"`

	// Transport is "stdio", "managed-http", "http", or "sse". Inferred if not set,
	// except managed-http which must be explicit because it owns command + URL.
	Transport TransportType `yaml:"transport,omitempty"`

	// Command is the executable to run (for stdio transport).
	Command string `yaml:"command,omitempty"`

	// Args are command-line arguments passed to Command.
	Args []string `yaml:"args,omitempty"`

	// Env are environment variables passed to the subprocess.
	// Values support ${VAR} expansion from the host environment.
	Env map[string]string `yaml:"env,omitempty"`

	// URL is the upstream MCP server URL (for http/sse transports).
	URL string `yaml:"url,omitempty"`

	// Headers are HTTP headers to forward (for http transport).
	Headers map[string]string `yaml:"headers,omitempty"`

	// Autostart determines if this server starts with the daemon.
	Autostart bool `yaml:"autostart,omitempty"`

	// RestartPolicy controls automatic restart behavior.
	RestartPolicy RestartPolicy `yaml:"restart_policy,omitempty"`

	// MaxRestarts is the maximum restart attempts within a 5-minute window.
	// A pointer preserves omitted versus explicit zero at the YAML boundary.
	MaxRestarts *int `yaml:"max_restarts,omitempty"`

	// Stateful enables process-per-session mode for isolated state.
	Stateful bool `yaml:"stateful,omitempty"`

	// AvailabilityProfile selects opinionated resilience defaults for the server.
	AvailabilityProfile AvailabilityProfile `yaml:"availability_profile,omitempty"`

	// SessionTimeout is how long an idle session lives (for stateful servers).
	SessionTimeout Duration `yaml:"session_timeout,omitempty"`

	// MaxSessions limits concurrent sessions (for stateful servers, 0 = unlimited).
	MaxSessions int `yaml:"max_sessions,omitempty"`

	// SessionTTL is the absolute maximum lifetime of a session regardless of activity.
	// 0 means no TTL (sessions only expire via idle timeout or explicit close).
	SessionTTL Duration `yaml:"session_ttl,omitempty"`

	// HealthCheckInterval is how often to probe idle downstream sessions for liveness.
	// When set, the proxy layer sends periodic tools/list calls to detect dead
	// subprocesses before a tool call hits the failure path. Must be >= 5s if set.
	// 0 means use default (30s).
	HealthCheckInterval Duration `yaml:"health_check_interval,omitempty"`

	// RequestTimeout is the default deadline Vision applies to downstream tool calls
	// when the upstream request does not already specify one. 0 means use default (30s).
	RequestTimeout Duration `yaml:"request_timeout,omitempty"`

	// Retry configures retry/backoff behavior for retryable downstream tool-call failures.
	Retry *RetryConfig `yaml:"retry,omitempty"`

	// CircuitBreaker configures fast-fail behavior after repeated downstream failures.
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuit_breaker,omitempty"`

	// SharedReadOnlyTools lists tool names that are safe to coalesce/cache across
	// concurrent sessions for this server.
	SharedReadOnlyTools []string `yaml:"shared_read_only_tools,omitempty"`

	// SharedResultCacheTTL is how long successful shared read-only results stay cached.
	// 0 disables caching while still allowing in-flight coalescing.
	SharedResultCacheTTL Duration `yaml:"shared_result_cache_ttl,omitempty"`

	// SharedResultCacheSize caps cached shared read-only results per server.
	// 0 uses the default for the selected profile.
	SharedResultCacheSize int `yaml:"shared_result_cache_size,omitempty"`

	// MaxInFlightRequests caps concurrent downstream tool calls per server.
	// 0 means unlimited.
	MaxInFlightRequests int `yaml:"max_in_flight_requests,omitempty"`

	// Required indicates the server MUST be running for Vision (and any
	// dependent agent) to function correctly. When both Autostart and
	// Required are true, initial-start failure is fatal to daemon startup
	// (daemon.StartAll returns a non-nil error). When Required is true but
	// Autostart is false, the field is informational only — external tools
	// such as OCA may surface it in doctor output.
	Required bool `yaml:"required,omitempty"`

	// Source is an informational URL describing where this server comes
	// from (homepage, repo). Accepted and preserved on round-trip; not
	// interpreted by Vision. Populated by external configuration tools
	// such as OpenCode Advance.
	Source string `yaml:"source,omitempty"`

	// Description is a short human-readable summary of the server.
	// Accepted and preserved on round-trip; not interpreted by Vision.
	// Populated by external configuration tools.
	Description string `yaml:"description,omitempty"`

	// DisconnectGracePeriod controls how long to wait after the last HTTP connection
	// closes before removing an upstream session. 0 uses default (60s).
	// Negative values disable disconnect detection entirely.
	//
	// Honored by shared-mode servers using SharedSessionManager AND by
	// managed-http, whose gateway receives it through
	// ResolvedDisconnectGracePeriod(). Despite sitting next to IdleReapTimeout
	// below, this setting is NOT inert on managed-http -- do not "fix" it.
	DisconnectGracePeriod Duration `yaml:"disconnect_grace_period,omitempty"`

	// IdleReapTimeout controls how long a shared-mode backend subprocess lives after
	// the last upstream session disconnects. 0 uses default (5m). Negative values
	// disable idle reaping entirely (subprocess lives forever).
	//
	// Applies only to non-stateful servers using SharedSessionManager, and that
	// limit is enforced: setting it on a managed-http server is a validation
	// error rather than a silently ignored value. Managed-http governs idle
	// session lifetime through SessionTimeout instead, which its lease manager
	// and reaper consume.
	IdleReapTimeout Duration `yaml:"idle_reap_timeout,omitempty"`

	// SlotGroup is internal metadata set when this server was synthesized from a
	// slot_groups entry. It is not persisted to YAML.
	SlotGroup string `yaml:"-"`

	// SlotIndex is the 1-based position of the synthesized server within its
	// slot group. It is not persisted to YAML.
	SlotIndex int `yaml:"-"`
}

// SlotGroupConfig defines a pool of identical servers plus one virtual group
// endpoint that routes sessions to the least-loaded healthy slot.
type SlotGroupConfig struct {
	Template  string        `yaml:"template"`
	BasePort  int           `yaml:"base_port"`
	Count     int           `yaml:"count"`
	GroupPort int           `yaml:"group_port"`
	Defaults  *ServerConfig `yaml:"defaults,omitempty"`
}

// RetryConfig controls retry behavior for retryable downstream failures.
type RetryConfig struct {
	MaxAttempts     int      `yaml:"max_attempts,omitempty"`
	InitialDelay    Duration `yaml:"initial_delay,omitempty"`
	MaxDelay        Duration `yaml:"max_delay,omitempty"`
	RetryableErrors []string `yaml:"retryable_errors,omitempty"`
}

// CircuitBreakerConfig controls fast-fail behavior after repeated failures.
type CircuitBreakerConfig struct {
	FailureThreshold int      `yaml:"failure_threshold,omitempty"`
	RecoveryTimeout  Duration `yaml:"recovery_timeout,omitempty"`
}

// SupervisionConfig holds global supervisor settings.
type SupervisionConfig struct {
	// HealthCheckInterval is how often to run reachability probes for managed
	// servers. Must be >= 5s if set; 0 means use the default (30s).
	HealthCheckInterval Duration `yaml:"health_check_interval"`

	// ShutdownTimeout is the grace period for graceful shutdown.
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`

	// RestartDelay is the initial delay before restarting a crashed server.
	RestartDelay Duration `yaml:"restart_delay"`

	// MaxRestartDelay is the maximum delay (for exponential backoff).
	MaxRestartDelay Duration `yaml:"max_restart_delay"`
}

// SecurityConfig holds daemon-wide security settings for MCP endpoints.
type SecurityConfig struct {
	// BearerToken is the shared secret for Authorization header validation.
	// When empty, authentication is not enforced (open access).
	// Supports ${VAR} expansion from the environment.
	BearerToken string `yaml:"bearer_token,omitempty"`

	// AllowedOrigins is the list of permitted Origin header values.
	// When empty, origin checking is not enforced.
	// Wildcard ("*") is explicitly rejected and treated as disallowed.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty"`
}

// Config is the root configuration structure for Vision.
type Config struct {
	// Servers maps server names to their configurations.
	Servers map[string]*ServerConfig `yaml:"servers"`

	// SlotGroups declaratively define pools of identical servers that may be
	// expanded into flat server entries at load time.
	SlotGroups map[string]*SlotGroupConfig `yaml:"slot_groups,omitempty"`

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
	ErrInvalidPort                    = errors.New("config: port must be between 6276 and 6325")
	ErrDuplicatePort                  = errors.New("config: duplicate port assignment")
	ErrMissingCommand                 = errors.New("config: stdio transport requires 'command' field")
	ErrEmptyCommand                   = errors.New("config: command cannot be empty string")
	ErrMissingURL                     = errors.New("config: http/sse transport requires 'url' field")
	ErrConflictingConfig              = errors.New("config: cannot specify both 'command' and 'url'")
	ErrInvalidTransport               = errors.New("config: invalid transport type")
	ErrInvalidRestartPolicy           = errors.New("config: invalid restart_policy (must be 'always', 'on-failure', or 'never')")
	ErrInvalidMaxRestarts             = errors.New("config: max_restarts must be >= 0")
	ErrInvalidAvailabilityProfile     = errors.New("config: invalid availability_profile")
	ErrInvalidHTTPURL                 = errors.New("config: http transport url must end with '/mcp'")
	ErrInvalidHealthCheckInterval     = errors.New("config: health_check_interval must be >= 5s")
	ErrInvalidRequestTimeout          = errors.New("config: request_timeout must be >= 1s")
	ErrInvalidRetryMaxAttempts        = errors.New("config: retry.max_attempts must be >= 1")
	ErrInvalidRetryInitialDelay       = errors.New("config: retry.initial_delay must be >= 1ms")
	ErrInvalidRetryMaxDelay           = errors.New("config: retry.max_delay must be >= 1ms")
	ErrInvalidRetryDelayRange         = errors.New("config: retry.max_delay must be >= retry.initial_delay")
	ErrInvalidCircuitFailureThreshold = errors.New("config: circuit_breaker.failure_threshold must be >= 1")
	ErrInvalidCircuitRecoveryTimeout  = errors.New("config: circuit_breaker.recovery_timeout must be >= 1s")
	ErrInvalidSharedResultCacheSize   = errors.New("config: shared_result_cache_size must be >= 0")
	ErrInvalidMaxInFlightRequests     = errors.New("config: max_in_flight_requests must be >= 0")
	ErrSettingNotSupportedByTransport = errors.New("config: setting is not supported by transport")
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

const defaultDisconnectGracePeriod = 60 * time.Second

// ResolvedDisconnectGracePeriod returns the effective disconnect grace period.
// Negative values return 0 (disabled). Zero returns the default (60s).
func (s *ServerConfig) ResolvedDisconnectGracePeriod() time.Duration {
	d := time.Duration(s.DisconnectGracePeriod)
	if d < 0 {
		return 0
	}
	if d == 0 {
		return defaultDisconnectGracePeriod
	}
	return d
}

const defaultIdleReapTimeout = 5 * time.Minute

// ResolvedIdleReapTimeout returns the effective idle reap timeout.
// Negative values return 0 (disabled). Zero returns the default (5m).
func (s *ServerConfig) ResolvedIdleReapTimeout() time.Duration {
	d := time.Duration(s.IdleReapTimeout)
	if d < 0 {
		return 0
	}
	if d == 0 {
		return defaultIdleReapTimeout
	}
	return d
}

func (s *ServerConfig) validateTransportSettings(name string, transport TransportType) error {
	for _, setting := range GovernedSettingKeys {
		disposition, reason, ok := lookupCapability(transport, setting)
		if !ok || disposition != DispositionRefused || !s.hasNonZeroSetting(setting) {
			continue
		}

		return fmt.Errorf(
			"%w: %s is not supported by transport %q on server %q: %s",
			ErrSettingNotSupportedByTransport,
			setting,
			transport,
			name,
			refusalReasonText(reason),
		)
	}
	return nil
}

func (s *ServerConfig) hasNonZeroSetting(setting SettingKey) bool {
	switch setting {
	case SettingSharedReadOnlyTools:
		return len(s.SharedReadOnlyTools) > 0
	case SettingSharedResultCacheTTL:
		return s.SharedResultCacheTTL != 0
	case SettingSharedResultCacheSize:
		return s.SharedResultCacheSize != 0
	case SettingMaxInFlightRequests:
		return s.MaxInFlightRequests != 0
	case SettingIdleReapTimeout:
		return s.IdleReapTimeout != 0
	case SettingDisconnectGracePeriod:
		return s.DisconnectGracePeriod != 0
	case SettingRetry:
		return s.Retry != nil
	case SettingCircuitBreaker:
		return s.CircuitBreaker != nil
	case SettingHealthCheckInterval:
		return s.HealthCheckInterval != 0
	case SettingSessionTTL:
		return s.SessionTTL != 0
	default:
		return false
	}
}

func (s *ServerConfig) transportHonors(setting SettingKey) bool {
	disposition, _, known := lookupCapability(s.InferTransport(), setting)
	return !known || disposition != DispositionRefused
}

func refusalReasonText(reason RefusalReason) string {
	switch reason {
	case ReasonSessionIsolation:
		return "cross-session result sharing contradicts per-session isolation (see docs/adr/0001-managed-native-http-playwright.md)"
	case ReasonSupersededKnob:
		return "idle session lifetime is governed by session_timeout on this transport"
	case ReasonNotImplemented:
		return "the managed-http path does not implement this setting"
	default:
		return "the transport does not implement this setting"
	}
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
	case TransportStdio, TransportHTTP, TransportManagedHTTP, TransportSSE:
		// valid
	default:
		return fmt.Errorf("%w: server %q has transport %q", ErrInvalidTransport, name, transport)
	}

	// Only managed HTTP has a typed reason to own both a process and URL.
	if s.Command != "" && s.URL != "" && transport != TransportManagedHTTP {
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
	case TransportManagedHTTP:
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("%w: managed HTTP server %q requires command", ErrMissingCommand, name)
		}
		if s.URL == "" {
			return fmt.Errorf("%w: managed HTTP server %q requires url", ErrMissingURL, name)
		}
		if err := validateManagedHTTPURL(s.URL); err != nil {
			return fmt.Errorf("%w: managed HTTP server %q: %v", ErrInvalidHTTPURL, name, err)
		}
		if s.Stateful {
			return fmt.Errorf("%w: managed HTTP server %q cannot also set stateful", ErrConflictingConfig, name)
		}
		for _, arg := range s.Args {
			if arg == "--shared-browser-context" {
				return fmt.Errorf("%w: managed HTTP server %q forbids --shared-browser-context", ErrConflictingConfig, name)
			}
		}
	case TransportSSE:
		if s.URL == "" {
			return fmt.Errorf("%w: server %q", ErrMissingURL, name)
		}
	}

	if err := s.validateTransportSettings(name, transport); err != nil {
		return err
	}

	// Health check interval validation (0 means use default, >0 must be >= 5s)
	if s.HealthCheckInterval > 0 && time.Duration(s.HealthCheckInterval) < 5*time.Second {
		return fmt.Errorf("%w: server %q has health_check_interval %v", ErrInvalidHealthCheckInterval, name, time.Duration(s.HealthCheckInterval))
	}

	// Request timeout validation (0 means use default, >0 must be >= 1s)
	if s.RequestTimeout > 0 && time.Duration(s.RequestTimeout) < time.Second {
		return fmt.Errorf("%w: server %q has request_timeout %v", ErrInvalidRequestTimeout, name, time.Duration(s.RequestTimeout))
	}

	if s.Retry != nil {
		if s.Retry.MaxAttempts < 1 {
			return fmt.Errorf("%w: server %q has retry.max_attempts %d", ErrInvalidRetryMaxAttempts, name, s.Retry.MaxAttempts)
		}
		if s.Retry.InitialDelay > 0 && time.Duration(s.Retry.InitialDelay) < time.Millisecond {
			return fmt.Errorf("%w: server %q has retry.initial_delay %v", ErrInvalidRetryInitialDelay, name, time.Duration(s.Retry.InitialDelay))
		}
		if s.Retry.MaxDelay > 0 && time.Duration(s.Retry.MaxDelay) < time.Millisecond {
			return fmt.Errorf("%w: server %q has retry.max_delay %v", ErrInvalidRetryMaxDelay, name, time.Duration(s.Retry.MaxDelay))
		}
		if s.Retry.InitialDelay > 0 && s.Retry.MaxDelay > 0 && s.Retry.MaxDelay < s.Retry.InitialDelay {
			return fmt.Errorf("%w: server %q has retry.initial_delay %v and retry.max_delay %v", ErrInvalidRetryDelayRange, name, time.Duration(s.Retry.InitialDelay), time.Duration(s.Retry.MaxDelay))
		}
	}

	if s.CircuitBreaker != nil {
		if s.CircuitBreaker.FailureThreshold < 1 {
			return fmt.Errorf("%w: server %q has circuit_breaker.failure_threshold %d", ErrInvalidCircuitFailureThreshold, name, s.CircuitBreaker.FailureThreshold)
		}
		if s.CircuitBreaker.RecoveryTimeout > 0 && time.Duration(s.CircuitBreaker.RecoveryTimeout) < time.Second {
			return fmt.Errorf("%w: server %q has circuit_breaker.recovery_timeout %v", ErrInvalidCircuitRecoveryTimeout, name, time.Duration(s.CircuitBreaker.RecoveryTimeout))
		}
	}

	if s.SharedResultCacheSize < 0 {
		return fmt.Errorf("%w: server %q has shared_result_cache_size %d", ErrInvalidSharedResultCacheSize, name, s.SharedResultCacheSize)
	}
	if s.MaxInFlightRequests < 0 {
		return fmt.Errorf("%w: server %q has max_in_flight_requests %d", ErrInvalidMaxInFlightRequests, name, s.MaxInFlightRequests)
	}

	// Restart policy validation
	if s.MaxRestarts != nil && *s.MaxRestarts < 0 {
		return fmt.Errorf("%w: server %q has max_restarts %d", ErrInvalidMaxRestarts, name, *s.MaxRestarts)
	}
	switch s.RestartPolicy {
	case "", RestartAlways, RestartOnFailure, RestartNever:
		// valid (empty defaults to on-failure)
	default:
		return fmt.Errorf("%w: server %q has restart_policy %q", ErrInvalidRestartPolicy, name, s.RestartPolicy)
	}

	// Availability profile validation
	switch s.AvailabilityProfile {
	case "", AvailabilityProfileNetworked:
		// valid
	default:
		return fmt.Errorf("%w: server %q has availability_profile %q", ErrInvalidAvailabilityProfile, name, s.AvailabilityProfile)
	}

	return nil
}

func validateManagedHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("scheme must be http")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("host is required")
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("host must be loopback")
		}
	}
	if u.Path != "/mcp" {
		return fmt.Errorf("path must be exactly /mcp")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("query is not allowed")
	}
	if u.Fragment != "" || u.User != nil {
		return fmt.Errorf("userinfo and fragment are not allowed")
	}
	return nil
}

// Validate checks that the entire Config is valid.
// An empty config (no servers) is considered valid for first-run scenarios.
func (c *Config) Validate() error {
	if err := c.Supervision.Validate(); err != nil {
		return err
	}

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

// Validate checks global supervision settings that have bounded values.
func (sup *SupervisionConfig) Validate() error {
	if sup.HealthCheckInterval > 0 && time.Duration(sup.HealthCheckInterval) < 5*time.Second {
		return fmt.Errorf("%w: supervision has health_check_interval %v", ErrInvalidHealthCheckInterval, time.Duration(sup.HealthCheckInterval))
	}
	return nil
}

// ApplyDefaults sets default values for missing optional fields.
func (s *ServerConfig) ApplyDefaults() {
	s.applyAvailabilityProfileDefaults()

	if s.RestartPolicy == "" {
		s.RestartPolicy = RestartOnFailure
	}
	if s.MaxRestarts == nil {
		maxRestarts := 5
		s.MaxRestarts = &maxRestarts
	}
	if s.SessionTimeout == 0 {
		s.SessionTimeout = Duration(5 * time.Minute)
	}
	if s.transportHonors(SettingHealthCheckInterval) && s.HealthCheckInterval == 0 {
		s.HealthCheckInterval = Duration(30 * time.Second)
	}
	if s.RequestTimeout == 0 {
		s.RequestTimeout = Duration(30 * time.Second)
	}
	if s.transportHonors(SettingRetry) {
		if s.Retry == nil {
			s.Retry = &RetryConfig{}
		}
		if s.Retry.MaxAttempts == 0 {
			s.Retry.MaxAttempts = 1
		}
		if s.Retry.InitialDelay == 0 {
			s.Retry.InitialDelay = Duration(100 * time.Millisecond)
		}
		if s.Retry.MaxDelay == 0 {
			s.Retry.MaxDelay = Duration(5 * time.Second)
		}
		if len(s.Retry.RetryableErrors) == 0 {
			s.Retry.RetryableErrors = []string{"timeout", "429", "502", "503", "ECONNRESET", "ECONNREFUSED", "ENETUNREACH"}
		}
	}
	if s.transportHonors(SettingCircuitBreaker) {
		if s.CircuitBreaker == nil {
			s.CircuitBreaker = &CircuitBreakerConfig{}
		}
		if s.CircuitBreaker.FailureThreshold == 0 {
			s.CircuitBreaker.FailureThreshold = 5
		}
		if s.CircuitBreaker.RecoveryTimeout == 0 {
			s.CircuitBreaker.RecoveryTimeout = Duration(60 * time.Second)
		}
	}
}

// MaxRestartCount returns the configured limit, defaulting safely for
// programmatically constructed configs that have not had defaults applied.
func (s *ServerConfig) MaxRestartCount() int {
	if s == nil || s.MaxRestarts == nil {
		return 5
	}
	return *s.MaxRestarts
}

func (s *ServerConfig) applyAvailabilityProfileDefaults() {
	switch s.AvailabilityProfile {
	case AvailabilityProfileNetworked:
		if s.SessionTimeout == 0 {
			s.SessionTimeout = Duration(30 * time.Minute)
		}
		if s.transportHonors(SettingHealthCheckInterval) && s.HealthCheckInterval == 0 {
			s.HealthCheckInterval = Duration(60 * time.Second)
		}
		if s.RequestTimeout == 0 {
			s.RequestTimeout = Duration(60 * time.Second)
		}
		if s.transportHonors(SettingRetry) {
			if s.Retry == nil {
				s.Retry = &RetryConfig{}
			}
			if s.Retry.MaxAttempts == 0 {
				s.Retry.MaxAttempts = 2
			}
			if s.Retry.InitialDelay == 0 {
				s.Retry.InitialDelay = Duration(500 * time.Millisecond)
			}
			if s.Retry.MaxDelay == 0 {
				s.Retry.MaxDelay = Duration(5 * time.Second)
			}
			if len(s.Retry.RetryableErrors) == 0 {
				s.Retry.RetryableErrors = []string{"timeout", "429", "502", "503", "504", "ECONNRESET", "ECONNREFUSED", "ENETUNREACH"}
			}
		}
		if s.transportHonors(SettingCircuitBreaker) {
			if s.CircuitBreaker == nil {
				s.CircuitBreaker = &CircuitBreakerConfig{}
			}
			if s.CircuitBreaker.FailureThreshold == 0 {
				s.CircuitBreaker.FailureThreshold = 3
			}
			if s.CircuitBreaker.RecoveryTimeout == 0 {
				s.CircuitBreaker.RecoveryTimeout = Duration(45 * time.Second)
			}
		}

		// These guards realize the capability-table invariant that refused
		// settings can only be non-zero at validation time when the user wrote
		// them, never when this profile supplied a default.
		if s.transportHonors(SettingSharedResultCacheTTL) && s.SharedResultCacheTTL == 0 {
			s.SharedResultCacheTTL = Duration(10 * time.Second)
		}
		if s.transportHonors(SettingSharedResultCacheSize) && s.SharedResultCacheSize == 0 {
			s.SharedResultCacheSize = 128
		}
		if s.transportHonors(SettingMaxInFlightRequests) && s.MaxInFlightRequests == 0 {
			s.MaxInFlightRequests = 4
		}
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
