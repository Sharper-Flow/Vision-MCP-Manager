package config

import (
	"strings"
	"testing"
)

func validManagedHTTPConfig() ServerConfig {
	return ServerConfig{
		Port:        6287,
		Transport:   TransportManagedHTTP,
		Command:     "node",
		Args:        []string{"playwright-mcp", "--isolated", "--port", "16287", "--host", "127.0.0.1"},
		URL:         "http://127.0.0.1:16287/mcp",
		MaxSessions: 6,
	}
}

func TestManagedHTTPConfigValidates(t *testing.T) {
	cfg := validManagedHTTPConfig()
	if got := cfg.InferTransport(); got != TransportManagedHTTP {
		t.Fatalf("InferTransport() = %q, want %q", got, TransportManagedHTTP)
	}
	if err := cfg.Validate("playwright"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestManagedHTTPConfigRejectsUnsafeTargets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ServerConfig)
		want   string
	}{
		{name: "missing command", mutate: func(c *ServerConfig) { c.Command = "" }, want: "command"},
		{name: "missing url", mutate: func(c *ServerConfig) { c.URL = "" }, want: "url"},
		{name: "https", mutate: func(c *ServerConfig) { c.URL = "https://127.0.0.1:16287/mcp" }, want: "http"},
		{name: "non loopback", mutate: func(c *ServerConfig) { c.URL = "http://example.com/mcp" }, want: "loopback"},
		{name: "wrong path", mutate: func(c *ServerConfig) { c.URL = "http://127.0.0.1:16287/api" }, want: "/mcp"},
		{name: "query", mutate: func(c *ServerConfig) { c.URL = "http://127.0.0.1:16287/mcp?x=1" }, want: "query"},
		{name: "shared context", mutate: func(c *ServerConfig) { c.Args = append(c.Args, "--shared-browser-context") }, want: "shared-browser-context"},
		{name: "stateful conflict", mutate: func(c *ServerConfig) { c.Stateful = true }, want: "stateful"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validManagedHTTPConfig()
			tt.mutate(&cfg)
			err := cfg.Validate("playwright")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
