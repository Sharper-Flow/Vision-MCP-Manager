package config

import (
	"errors"
	"testing"
	"time"
)

func TestTransportType_InferTransport(t *testing.T) {
	tests := []struct {
		name     string
		config   ServerConfig
		expected TransportType
	}{
		{
			name:     "explicit stdio",
			config:   ServerConfig{Transport: TransportStdio, Command: "echo"},
			expected: TransportStdio,
		},
		{
			name:     "explicit http",
			config:   ServerConfig{Transport: TransportHTTP, URL: "http://localhost:8080/mcp"},
			expected: TransportHTTP,
		},
		{
			name:     "explicit sse",
			config:   ServerConfig{Transport: TransportSSE, URL: "http://localhost:8080"},
			expected: TransportSSE,
		},
		{
			name:     "infer stdio from command",
			config:   ServerConfig{Command: "npx"},
			expected: TransportStdio,
		},
		{
			name:     "infer http from url ending in /mcp",
			config:   ServerConfig{URL: "http://localhost:8080/mcp"},
			expected: TransportHTTP,
		},
		{
			name:     "infer sse from url not ending in /mcp",
			config:   ServerConfig{URL: "http://localhost:8080"},
			expected: TransportSSE,
		},
		{
			name:     "default to stdio when neither set",
			config:   ServerConfig{},
			expected: TransportStdio,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.config.InferTransport()
			if result != tt.expected {
				t.Errorf("InferTransport() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestServerConfig_Validate(t *testing.T) {
	tests := []struct {
		name       string
		config     ServerConfig
		serverName string
		wantErr    error
	}{
		{
			name: "valid stdio server",
			config: ServerConfig{
				Port:    6276,
				Command: "npx",
				Args:    []string{"-y", "@anthropic/mcp-time"},
			},
			serverName: "time",
			wantErr:    nil,
		},
		{
			name: "valid http server",
			config: ServerConfig{
				Port:      6277,
				Transport: TransportHTTP,
				URL:       "http://localhost:8080/mcp",
			},
			serverName: "remote",
			wantErr:    nil,
		},
		{
			name: "valid sse server",
			config: ServerConfig{
				Port:      6278,
				Transport: TransportSSE,
				URL:       "http://localhost:8080",
			},
			serverName: "legacy",
			wantErr:    nil,
		},
		{
			name: "port too low",
			config: ServerConfig{
				Port:    6000,
				Command: "echo",
			},
			serverName: "bad-port",
			wantErr:    ErrInvalidPort,
		},
		{
			name: "port too high",
			config: ServerConfig{
				Port:    99999,
				Command: "echo",
			},
			serverName: "bad-port",
			wantErr:    ErrInvalidPort,
		},
		{
			name: "missing command for stdio",
			config: ServerConfig{
				Port: 6276,
			},
			serverName: "no-command",
			wantErr:    ErrMissingCommand,
		},
		{
			name: "empty command",
			config: ServerConfig{
				Port:    6276,
				Command: "",
			},
			serverName: "empty-command",
			wantErr:    ErrMissingCommand,
		},
		{
			name: "whitespace-only command",
			config: ServerConfig{
				Port:    6276,
				Command: "   ",
			},
			serverName: "whitespace-command",
			wantErr:    ErrEmptyCommand,
		},
		{
			name: "missing url for http",
			config: ServerConfig{
				Port:      6276,
				Transport: TransportHTTP,
			},
			serverName: "no-url",
			wantErr:    ErrMissingURL,
		},
		{
			name: "http url must end with /mcp",
			config: ServerConfig{
				Port:      6276,
				Transport: TransportHTTP,
				URL:       "http://localhost:8080",
			},
			serverName: "bad-http-url",
			wantErr:    ErrInvalidHTTPURL,
		},
		{
			name: "missing url for sse",
			config: ServerConfig{
				Port:      6276,
				Transport: TransportSSE,
			},
			serverName: "no-url",
			wantErr:    ErrMissingURL,
		},
		{
			name: "conflicting command and url",
			config: ServerConfig{
				Port:    6276,
				Command: "echo",
				URL:     "http://localhost:8080/mcp",
			},
			serverName: "conflict",
			wantErr:    ErrConflictingConfig,
		},
		{
			name: "invalid transport type",
			config: ServerConfig{
				Port:      6276,
				Transport: "websocket",
				Command:   "echo",
			},
			serverName: "bad-transport",
			wantErr:    ErrInvalidTransport,
		},
		{
			name: "invalid restart policy",
			config: ServerConfig{
				Port:          6276,
				Command:       "echo",
				RestartPolicy: "sometimes",
			},
			serverName: "bad-restart",
			wantErr:    ErrInvalidRestartPolicy,
		},
		{
			name: "valid restart policy always",
			config: ServerConfig{
				Port:          6276,
				Command:       "echo",
				RestartPolicy: RestartAlways,
			},
			serverName: "restart-always",
			wantErr:    nil,
		},
		{
			name: "valid restart policy never",
			config: ServerConfig{
				Port:          6276,
				Command:       "echo",
				RestartPolicy: RestartNever,
			},
			serverName: "restart-never",
			wantErr:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate(tt.serverName)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("Validate() expected error %v, got nil", tt.wantErr)
				} else if !errors.Is(err, tt.wantErr) {
					t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
				}
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr error
	}{
		{
			name:    "empty config is valid",
			config:  Config{},
			wantErr: nil, // Empty config is valid for first-run scenarios
		},
		{
			name: "valid single server",
			config: Config{
				Servers: map[string]*ServerConfig{
					"time": {Port: 6276, Command: "npx", Args: []string{"-y", "@anthropic/mcp-time"}},
				},
			},
			wantErr: nil,
		},
		{
			name: "valid multiple servers",
			config: Config{
				Servers: map[string]*ServerConfig{
					"time":   {Port: 6276, Command: "npx"},
					"echo":   {Port: 6277, Command: "echo"},
					"remote": {Port: 6278, Transport: TransportHTTP, URL: "http://localhost/mcp"},
				},
			},
			wantErr: nil,
		},
		{
			name: "duplicate ports",
			config: Config{
				Servers: map[string]*ServerConfig{
					"server1": {Port: 6276, Command: "echo"},
					"server2": {Port: 6276, Command: "cat"},
				},
			},
			wantErr: ErrDuplicatePort,
		},
		{
			name: "invalid server in config",
			config: Config{
				Servers: map[string]*ServerConfig{
					"valid":   {Port: 6276, Command: "echo"},
					"invalid": {Port: 99999, Command: "cat"},
				},
			},
			wantErr: ErrInvalidPort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("Validate() expected error %v, got nil", tt.wantErr)
				} else if !errors.Is(err, tt.wantErr) {
					t.Errorf("Config.Validate() error = %v, want %v", err, tt.wantErr)
				}
			}
		})
	}
}

func TestServerConfig_ApplyDefaults(t *testing.T) {
	s := &ServerConfig{
		Port:    6276,
		Command: "echo",
	}

	s.ApplyDefaults()

	if s.RestartPolicy != RestartOnFailure {
		t.Errorf("RestartPolicy = %q, want %q", s.RestartPolicy, RestartOnFailure)
	}
	if s.MaxRestarts != 5 {
		t.Errorf("MaxRestarts = %d, want 5", s.MaxRestarts)
	}
	if s.SessionTimeout.Duration() != 5*time.Minute {
		t.Errorf("SessionTimeout = %v, want 5m", s.SessionTimeout)
	}
}

func TestServerConfig_HealthCheckInterval(t *testing.T) {
	t.Run("default is 30s when not set", func(t *testing.T) {
		s := &ServerConfig{Port: 6276, Command: "echo"}
		s.ApplyDefaults()
		if s.HealthCheckInterval.Duration() != 30*time.Second {
			t.Errorf("HealthCheckInterval = %v, want 30s", s.HealthCheckInterval)
		}
	})

	t.Run("custom value is preserved", func(t *testing.T) {
		s := &ServerConfig{
			Port:                6276,
			Command:             "echo",
			HealthCheckInterval: Duration(60 * time.Second),
		}
		s.ApplyDefaults()
		if s.HealthCheckInterval.Duration() != 60*time.Second {
			t.Errorf("HealthCheckInterval = %v, want 60s", s.HealthCheckInterval)
		}
	})

	t.Run("validation rejects interval less than 5s", func(t *testing.T) {
		s := &ServerConfig{
			Port:                6276,
			Command:             "echo",
			HealthCheckInterval: Duration(1 * time.Second),
		}
		err := s.Validate("test")
		if !errors.Is(err, ErrInvalidHealthCheckInterval) {
			t.Errorf("expected ErrInvalidHealthCheckInterval, got %v", err)
		}
	})

	t.Run("validation accepts interval of 5s", func(t *testing.T) {
		s := &ServerConfig{
			Port:                6276,
			Command:             "echo",
			HealthCheckInterval: Duration(5 * time.Second),
		}
		err := s.Validate("test")
		if err != nil {
			t.Errorf("unexpected validation error for HealthCheckInterval = 5s: %v", err)
		}
	})

	t.Run("validation accepts zero (uses default)", func(t *testing.T) {
		s := &ServerConfig{
			Port:    6276,
			Command: "echo",
		}
		err := s.Validate("test")
		if err != nil {
			t.Errorf("unexpected validation error for zero HealthCheckInterval: %v", err)
		}
	})
}

func TestServerConfig_RequestResilience(t *testing.T) {
	t.Run("defaults are applied when resilience settings are omitted", func(t *testing.T) {
		s := &ServerConfig{Port: 6276, Command: "echo"}

		s.ApplyDefaults()

		if s.RequestTimeout.Duration() != 30*time.Second {
			t.Errorf("RequestTimeout = %v, want 30s", s.RequestTimeout)
		}
		if s.Retry == nil {
			t.Fatal("Retry = nil, want defaults")
		}
		if s.Retry.MaxAttempts != 1 {
			t.Errorf("Retry.MaxAttempts = %d, want 1", s.Retry.MaxAttempts)
		}
		if s.Retry.InitialDelay.Duration() != 100*time.Millisecond {
			t.Errorf("Retry.InitialDelay = %v, want 100ms", s.Retry.InitialDelay)
		}
		if s.Retry.MaxDelay.Duration() != 5*time.Second {
			t.Errorf("Retry.MaxDelay = %v, want 5s", s.Retry.MaxDelay)
		}
		if s.CircuitBreaker == nil {
			t.Fatal("CircuitBreaker = nil, want defaults")
		}
		if s.CircuitBreaker.FailureThreshold != 5 {
			t.Errorf("CircuitBreaker.FailureThreshold = %d, want 5", s.CircuitBreaker.FailureThreshold)
		}
		if s.CircuitBreaker.RecoveryTimeout.Duration() != 60*time.Second {
			t.Errorf("CircuitBreaker.RecoveryTimeout = %v, want 60s", s.CircuitBreaker.RecoveryTimeout)
		}
	})

	t.Run("custom resilience settings are preserved", func(t *testing.T) {
		s := &ServerConfig{
			Port:           6276,
			Command:        "echo",
			RequestTimeout: Duration(45 * time.Second),
			Retry: &RetryConfig{
				MaxAttempts:     3,
				InitialDelay:    Duration(250 * time.Millisecond),
				MaxDelay:        Duration(12 * time.Second),
				RetryableErrors: []string{"timeout", "429"},
			},
			CircuitBreaker: &CircuitBreakerConfig{
				FailureThreshold: 3,
				RecoveryTimeout:  Duration(30 * time.Second),
			},
		}

		s.ApplyDefaults()

		if s.RequestTimeout.Duration() != 45*time.Second {
			t.Errorf("RequestTimeout = %v, want 45s", s.RequestTimeout)
		}
		if s.Retry.MaxAttempts != 3 {
			t.Errorf("Retry.MaxAttempts = %d, want 3", s.Retry.MaxAttempts)
		}
		if len(s.Retry.RetryableErrors) != 2 {
			t.Errorf("Retry.RetryableErrors len = %d, want 2", len(s.Retry.RetryableErrors))
		}
		if s.CircuitBreaker.FailureThreshold != 3 {
			t.Errorf("CircuitBreaker.FailureThreshold = %d, want 3", s.CircuitBreaker.FailureThreshold)
		}
	})

	t.Run("validation rejects request timeout less than one second", func(t *testing.T) {
		s := &ServerConfig{
			Port:           6276,
			Command:        "echo",
			RequestTimeout: Duration(500 * time.Millisecond),
		}

		err := s.Validate("test")
		if !errors.Is(err, ErrInvalidRequestTimeout) {
			t.Errorf("expected ErrInvalidRequestTimeout, got %v", err)
		}
	})

	t.Run("validation rejects retry max attempts less than one", func(t *testing.T) {
		s := &ServerConfig{
			Port:    6276,
			Command: "echo",
			Retry: &RetryConfig{
				MaxAttempts: 0,
			},
		}

		err := s.Validate("test")
		if !errors.Is(err, ErrInvalidRetryMaxAttempts) {
			t.Errorf("expected ErrInvalidRetryMaxAttempts, got %v", err)
		}
	})

	t.Run("validation rejects retry max delay below initial delay", func(t *testing.T) {
		s := &ServerConfig{
			Port:    6276,
			Command: "echo",
			Retry: &RetryConfig{
				MaxAttempts:  2,
				InitialDelay: Duration(2 * time.Second),
				MaxDelay:     Duration(1 * time.Second),
			},
		}

		err := s.Validate("test")
		if !errors.Is(err, ErrInvalidRetryDelayRange) {
			t.Errorf("expected ErrInvalidRetryDelayRange, got %v", err)
		}
	})

	t.Run("validation rejects circuit breaker threshold less than one", func(t *testing.T) {
		s := &ServerConfig{
			Port:    6276,
			Command: "echo",
			CircuitBreaker: &CircuitBreakerConfig{
				FailureThreshold: 0,
			},
		}

		err := s.Validate("test")
		if !errors.Is(err, ErrInvalidCircuitFailureThreshold) {
			t.Errorf("expected ErrInvalidCircuitFailureThreshold, got %v", err)
		}
	})
}

func TestServerConfig_AvailabilityProfile(t *testing.T) {
	t.Run("networked profile applies stronger availability defaults", func(t *testing.T) {
		s := &ServerConfig{
			Port:                6279,
			Command:             "uvx",
			AvailabilityProfile: AvailabilityProfileNetworked,
		}

		s.ApplyDefaults()

		if s.RequestTimeout.Duration() != 60*time.Second {
			t.Errorf("RequestTimeout = %v, want 60s", s.RequestTimeout)
		}
		if s.HealthCheckInterval.Duration() != 60*time.Second {
			t.Errorf("HealthCheckInterval = %v, want 60s", s.HealthCheckInterval)
		}
		if s.SessionTimeout.Duration() != 30*time.Minute {
			t.Errorf("SessionTimeout = %v, want 30m", s.SessionTimeout)
		}
		if s.Retry == nil {
			t.Fatal("Retry = nil, want defaults")
		}
		if s.Retry.MaxAttempts != 2 {
			t.Errorf("Retry.MaxAttempts = %d, want 2", s.Retry.MaxAttempts)
		}
		if s.Retry.InitialDelay.Duration() != 500*time.Millisecond {
			t.Errorf("Retry.InitialDelay = %v, want 500ms", s.Retry.InitialDelay)
		}
		if s.CircuitBreaker == nil {
			t.Fatal("CircuitBreaker = nil, want defaults")
		}
		if s.CircuitBreaker.FailureThreshold != 3 {
			t.Errorf("CircuitBreaker.FailureThreshold = %d, want 3", s.CircuitBreaker.FailureThreshold)
		}
		if s.CircuitBreaker.RecoveryTimeout.Duration() != 45*time.Second {
			t.Errorf("CircuitBreaker.RecoveryTimeout = %v, want 45s", s.CircuitBreaker.RecoveryTimeout)
		}
	})

	t.Run("validation rejects unknown availability profile", func(t *testing.T) {
		s := &ServerConfig{
			Port:                6279,
			Command:             "uvx",
			AvailabilityProfile: "mystery",
		}

		err := s.Validate("kagi")
		if !errors.Is(err, ErrInvalidAvailabilityProfile) {
			t.Errorf("expected ErrInvalidAvailabilityProfile, got %v", err)
		}
	})

	t.Run("validation rejects negative shared cache or concurrency limits", func(t *testing.T) {
		s := &ServerConfig{
			Port:                  6279,
			Command:               "uvx",
			AvailabilityProfile:   AvailabilityProfileNetworked,
			SharedResultCacheSize: -1,
		}

		err := s.Validate("kagi")
		if !errors.Is(err, ErrInvalidSharedResultCacheSize) {
			t.Fatalf("expected ErrInvalidSharedResultCacheSize, got %v", err)
		}

		s.SharedResultCacheSize = 1
		s.MaxInFlightRequests = -1
		err = s.Validate("kagi")
		if !errors.Is(err, ErrInvalidMaxInFlightRequests) {
			t.Fatalf("expected ErrInvalidMaxInFlightRequests, got %v", err)
		}
	})
}

func TestSupervisionConfig_ApplyDefaults(t *testing.T) {
	sup := &SupervisionConfig{}

	sup.ApplyDefaults()

	if sup.HealthCheckInterval.Duration() != 30*time.Second {
		t.Errorf("HealthCheckInterval = %v, want 30s", sup.HealthCheckInterval)
	}
	if sup.ShutdownTimeout.Duration() != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 10s", sup.ShutdownTimeout)
	}
	if sup.RestartDelay.Duration() != 1*time.Second {
		t.Errorf("RestartDelay = %v, want 1s", sup.RestartDelay)
	}
	if sup.MaxRestartDelay.Duration() != 60*time.Second {
		t.Errorf("MaxRestartDelay = %v, want 60s", sup.MaxRestartDelay)
	}
}

func TestConfig_ApplyDefaults(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"test": {Port: 6276, Command: "echo"},
		},
	}

	cfg.ApplyDefaults()

	// Check server defaults were applied
	if cfg.Servers["test"].RestartPolicy != RestartOnFailure {
		t.Errorf("Server RestartPolicy not defaulted")
	}

	// Check supervision defaults were applied
	if cfg.Supervision.HealthCheckInterval.Duration() != 30*time.Second {
		t.Errorf("Supervision HealthCheckInterval not defaulted")
	}
}

func TestServerConfig_SlotMetadataIgnoredByDefaults(t *testing.T) {
	cfg := &ServerConfig{
		Command:   "echo",
		SlotGroup: "playwright",
		SlotIndex: 3,
	}

	cfg.ApplyDefaults()

	if cfg.SlotGroup != "playwright" {
		t.Fatalf("SlotGroup = %q, want %q", cfg.SlotGroup, "playwright")
	}
	if cfg.SlotIndex != 3 {
		t.Fatalf("SlotIndex = %d, want %d", cfg.SlotIndex, 3)
	}
	if cfg.RestartPolicy != RestartOnFailure {
		t.Fatalf("RestartPolicy = %q, want %q", cfg.RestartPolicy, RestartOnFailure)
	}
}

func TestServerConfig_PortsAbove6300ValidateThrough6325(t *testing.T) {
	cases := []struct {
		name string
		port int
		err  error
	}{
		{"boundary 6300 ok", 6300, nil},
		{"extended 6301 ok", 6301, nil},
		{"extended 6325 ok", 6325, nil},
		{"extended 6326 rejected", 6326, ErrInvalidPort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ServerConfig{Port: tc.port, Command: "echo"}
			err := cfg.Validate("test")
			if tc.err == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestExpandSlotGroups_AllowsBasePortInExtendedRange(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{},
		SlotGroups: map[string]*SlotGroupConfig{
			"pool": {
				Template:  "pool-slot",
				BasePort:  6315,
				Count:     4,
				GroupPort: 6314,
				Defaults:  &ServerConfig{Command: "echo"},
			},
		},
	}
	if err := expandSlotGroups(cfg); err != nil {
		t.Fatalf("expandSlotGroups() error: %v", err)
	}
	if got := cfg.Servers["pool-slot-4"].Port; got != 6318 {
		t.Fatalf("pool-slot-4 port = %d, want 6318", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestConfig_AllowsSlotGroupsField(t *testing.T) {
	cfg := &Config{
		Servers: map[string]*ServerConfig{
			"playwright-slot-1": {Command: "echo", SlotGroup: "playwright", SlotIndex: 1},
		},
		SlotGroups: map[string]*SlotGroupConfig{
			"playwright": {
				Template:  "playwright-slot",
				BasePort:  6287,
				Count:     8,
				GroupPort: 6286,
				Defaults:  &ServerConfig{Command: "echo"},
			},
		},
	}

	if got := cfg.SlotGroups["playwright"].Template; got != "playwright-slot" {
		t.Fatalf("Template = %q, want %q", got, "playwright-slot")
	}
	if got := cfg.Servers["playwright-slot-1"].SlotGroup; got != "playwright" {
		t.Fatalf("SlotGroup = %q, want %q", got, "playwright")
	}
}

func TestDuration_String(t *testing.T) {
	d := Duration(30 * time.Second)
	if d.String() != "30s" {
		t.Errorf("Duration.String() = %q, want %q", d.String(), "30s")
	}
}

func TestServerConfig_ResolvedDisconnectGracePeriod(t *testing.T) {
	tests := []struct {
		name     string
		input    Duration
		expected time.Duration
	}{
		{
			name:     "zero returns default 60s",
			input:    Duration(0),
			expected: 60 * time.Second,
		},
		{
			name:     "positive value returned as-is",
			input:    Duration(120 * time.Second),
			expected: 120 * time.Second,
		},
		{
			name:     "small positive value returned as-is",
			input:    Duration(5 * time.Second),
			expected: 5 * time.Second,
		},
		{
			name:     "negative returns zero (disabled)",
			input:    Duration(-1),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ServerConfig{DisconnectGracePeriod: tt.input}
			got := cfg.ResolvedDisconnectGracePeriod()
			if got != tt.expected {
				t.Errorf("ResolvedDisconnectGracePeriod() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestServerConfig_ResolvedIdleReapTimeout(t *testing.T) {
	tests := []struct {
		name     string
		input    Duration
		expected time.Duration
	}{
		{
			name:     "zero returns default 5m",
			input:    Duration(0),
			expected: 5 * time.Minute,
		},
		{
			name:     "positive value returned as-is",
			input:    Duration(10 * time.Minute),
			expected: 10 * time.Minute,
		},
		{
			name:     "small positive value returned as-is",
			input:    Duration(30 * time.Second),
			expected: 30 * time.Second,
		},
		{
			name:     "negative returns zero (disabled)",
			input:    Duration(-1),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ServerConfig{IdleReapTimeout: tt.input}
			got := cfg.ResolvedIdleReapTimeout()
			if got != tt.expected {
				t.Errorf("ResolvedIdleReapTimeout() = %v, want %v", got, tt.expected)
			}
		})
	}
}
