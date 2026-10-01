package session

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syntheticSecret stands in for a credential that ${VAR} expansion places in
// a server's args. It is never a real credential.
const syntheticSecret = "vision-test-secret-3b29af57"

// lockedBuffer serializes writes so the logger and the test can share it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func capturingLogger() (*slog.Logger, *lockedBuffer) {
	out := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})), out
}

func assertSpawnLogOmitsSecret(t *testing.T, logs, spawnEvent string) {
	t.Helper()
	if !strings.Contains(logs, "event="+spawnEvent) {
		t.Fatalf("spawn log event %q not emitted; logs:\n%s", spawnEvent, logs)
	}
	if strings.Contains(logs, syntheticSecret) {
		t.Fatalf("spawn log contains a resolved argument value:\n%s", logs)
	}
}

func TestSharedManager_SpawnLogOmitsArgValues(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger, logs := capturingLogger()
	cfg := testSharedServerConfig()
	cfg.Args = append(cfg.Args, "--", "--header", "Authorization: Bearer "+syntheticSecret)

	sm := NewSharedSessionManager("test-spawn-log", cfg, logger, 0, nil)
	defer sm.CloseAll()

	if _, err := sm.GetOrCreateSession(ctx, "sess-log"); err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	assertSpawnLogOmitsSecret(t, logs.String(), "shared_session.spawn")
}

func TestManager_SpawnLogOmitsArgValues(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger, logs := capturingLogger()
	cfg := testServerConfig()
	cfg.Args = append(cfg.Args, "https://remote.invalid/mcp?userToken="+syntheticSecret)

	m := NewManager("test-spawn-log", cfg, logger)
	defer m.CloseAll()

	if _, err := m.SpawnSession(ctx, "sess-log"); err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}
	assertSpawnLogOmitsSecret(t, logs.String(), "session.spawn")
}
