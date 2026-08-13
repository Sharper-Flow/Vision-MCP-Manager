package mcp

import "testing"

func TestBearerAuthWarningUsesStructuralListenerExposure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    string
		exposure ListenerExposure
		warn     bool
	}{
		{name: "loopback without token supported", exposure: ListenerLoopback},
		{name: "loopback with token", token: "configured", exposure: ListenerLoopback},
		{name: "network without token warns", exposure: ListenerNetwork, warn: true},
		{name: "network with token", token: "configured", exposure: ListenerNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warning := BearerAuthWarning(tc.token, tc.exposure)
			if (warning != "") != tc.warn {
				t.Fatalf("warning=%q, want warning=%v", warning, tc.warn)
			}
			if warning != "" && (warning == tc.token || tc.token != "" && containsSecret(warning, tc.token)) {
				t.Fatal("warning exposed token")
			}
		})
	}
}

func containsSecret(text, secret string) bool {
	for i := 0; i+len(secret) <= len(text); i++ {
		if text[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}
