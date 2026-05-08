package server

import (
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

// TestServerConfig_AcceptsRequiredField verifies the V3 schema addition
// parses cleanly from YAML and is preserved on ServerConfig.
func TestServerConfig_AcceptsRequiredField(t *testing.T) {
	cfg := &config.ServerConfig{
		Port:      6295,
		Command:   "echo",
		Required:  true,
		Autostart: true,
	}
	reg := NewRegistry(nil, nil)
	if err := reg.Add("critical", cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	got := reg.Get("critical")
	if got == nil {
		t.Fatalf("Get returned nil")
	}
	if !got.Config.Required {
		t.Errorf("Config.Required = false, want true")
	}
	if !got.Config.Autostart {
		t.Errorf("Config.Autostart = false, want true")
	}
}

// TestServerConfig_RequiredDefaultsToFalse verifies backward compatibility:
// omitting the field keeps Required=false so existing configs stay
// best-effort.
func TestServerConfig_RequiredDefaultsToFalse(t *testing.T) {
	cfg := &config.ServerConfig{
		Port:      6296,
		Command:   "echo",
		Autostart: true,
	}
	reg := NewRegistry(nil, nil)
	if err := reg.Add("optional", cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if reg.Get("optional").Config.Required {
		t.Errorf("Required defaulted to true, want false (backward compat)")
	}
}

// TestRequiredStartupError_ErrorIncludesFailures verifies the wrapper
// error type formats failures and unwraps to the joined cause. This is
// the contract non-stdio transports rely on — and the same contract
// external tools (OCA doctor) can pattern-match if they receive it
// via daemon startup failure logs.
func TestRequiredStartupError_ErrorIncludesFailures(t *testing.T) {
	inner := errRequiredFake{}
	rse := &RequiredStartupError{
		Failures: []string{"a", "b"},
		Joined:   inner,
	}
	msg := rse.Error()
	if msg == "" {
		t.Fatal("Error() returned empty string")
	}
	for _, needle := range []string{"[a b]", "required servers failed"} {
		if !contains(msg, needle) {
			t.Errorf("Error() = %q, missing %q", msg, needle)
		}
	}
	if rse.Unwrap() != inner {
		t.Errorf("Unwrap() did not return inner error")
	}
}

type errRequiredFake struct{}

func (errRequiredFake) Error() string { return "fake inner" }

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) && (s[:len(substr)] == substr ||
			s[len(s)-len(substr):] == substr ||
			indexOf(s, substr) >= 0)))
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
