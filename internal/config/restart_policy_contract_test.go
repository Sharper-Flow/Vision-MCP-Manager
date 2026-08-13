package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMaxRestartsDistinguishesOmittedFromExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		{name: "omitted defaults to five", yaml: "servers:\n  test:\n    port: 6276\n    command: echo\n", want: 5},
		{name: "explicit zero disables retries", yaml: "servers:\n  test:\n    port: 6276\n    command: echo\n    max_restarts: 0\n", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "servers.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			server := cfg.Servers["test"]
			if server.MaxRestarts == nil || *server.MaxRestarts != tc.want {
				t.Fatalf("MaxRestarts=%v, want %d", server.MaxRestarts, tc.want)
			}
		})
	}
}

func TestServerConfigMaxRestartCountHandlesNilBoundary(t *testing.T) {
	var server ServerConfig
	if got := server.MaxRestartCount(); got != 5 {
		t.Fatalf("nil MaxRestarts count=%d, want 5", got)
	}
	zero := 0
	server.MaxRestarts = &zero
	if got := server.MaxRestartCount(); got != 0 {
		t.Fatalf("explicit zero count=%d, want 0", got)
	}
}
