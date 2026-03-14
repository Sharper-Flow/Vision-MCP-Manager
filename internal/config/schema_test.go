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

func TestDuration_String(t *testing.T) {
	d := Duration(30 * time.Second)
	if d.String() != "30s" {
		t.Errorf("Duration.String() = %q, want %q", d.String(), "30s")
	}
}
