package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jrede/vision/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testSharedServerConfig() *config.ServerConfig {
	return &config.ServerConfig{
		Command:             "node",
		Args:                []string{"-e", echoMCPServerJS},
		Port:                16280,
		Autostart:           true,
		RestartPolicy:       config.RestartOnFailure,
		HealthCheckInterval: config.Duration(30 * time.Second),
		MaxSessions:         10,
	}
}

// TestSharedManager_SpawnOnFirstUse verifies that the first GetOrCreateSession
// lazily spawns the shared downstream subprocess.
func TestSharedManager_SpawnOnFirstUse(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-shared", cfg, logger)
	defer sm.CloseAll()

	// Initially no downstream
	if sm.HasDownstream() {
		t.Error("expected no downstream before first use")
	}

	// First call spawns
	ds, err := sm.GetOrCreateSession(ctx, "sess-001")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	if ds == nil {
		t.Fatal("GetOrCreateSession returned nil")
	}

	// Verify downstream is active
	if !sm.HasDownstream() {
		t.Error("expected downstream after first use")
	}

	// Verify we can call tools
	toolsResult, err := ds.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(toolsResult.Tools) == 0 {
		t.Fatal("no tools returned")
	}
	if toolsResult.Tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want echo", toolsResult.Tools[0].Name)
	}
}

// TestSharedManager_ConcurrentSpawnSafety verifies that concurrent calls to
// GetOrCreateSession only spawn a single subprocess.
func TestSharedManager_ConcurrentSpawnSafety(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-shared", cfg, logger)
	defer sm.CloseAll()

	var wg sync.WaitGroup
	sessions := make([]*mcp.ClientSession, 5)
	errs := make([]error, 5)

	for i := 0; i < 5; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions[i], errs[i] = sm.GetOrCreateSession(ctx, fmt.Sprintf("sess-%03d", i))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("GetOrCreateSession[%d] failed: %v", i, err)
		}
	}

	// All should point to the same downstream session
	for i := 1; i < 5; i++ {
		if sessions[i] != sessions[0] {
			t.Error("concurrent spawns created different downstream sessions")
		}
	}

	// Verify only one upstream session counted... wait, we created 5 different session IDs
	// Actually in shared mode, all upstream sessions share one downstream
	// Refcount should be 5
	if sm.RefCount() != 5 {
		t.Errorf("refcount = %d, want 5", sm.RefCount())
	}
}

// TestSharedManager_RefcountLifecycle verifies that addRef/removeRef work
// correctly and the subprocess stays alive while refs > 0.
func TestSharedManager_RefcountLifecycle(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-shared", cfg, logger)
	defer sm.CloseAll()

	// Add refs
	for i := 0; i < 3; i++ {
		_, err := sm.GetOrCreateSession(ctx, fmt.Sprintf("sess-%d", i))
		if err != nil {
			t.Fatalf("GetOrCreateSession failed: %v", err)
		}
	}

	if sm.RefCount() != 3 {
		t.Errorf("refcount = %d, want 3", sm.RefCount())
	}

	// Remove one ref - subprocess should still be alive
	if err := sm.RemoveSession("sess-0"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}
	if sm.RefCount() != 2 {
		t.Errorf("refcount after remove = %d, want 2", sm.RefCount())
	}
	if !sm.HasDownstream() {
		t.Error("downstream should still be alive with refs > 0")
	}

	// Remove remaining refs
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}
	if err := sm.RemoveSession("sess-2"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	if sm.RefCount() != 0 {
		t.Errorf("refcount after all removed = %d, want 0", sm.RefCount())
	}

	// Subprocess should still be alive (shared mode doesn't kill on zero refs)
	if !sm.HasDownstream() {
		t.Error("downstream should still be alive even with 0 refs in shared mode")
	}
}

// TestSharedManager_RespawnOnCrash verifies that when the subprocess crashes,
// the manager respawns it and notifies subscribers.
func TestSharedManager_RespawnOnCrash(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-shared", cfg, logger)
	defer sm.CloseAll()

	// Spawn initial session
	ds1, err := sm.GetOrCreateSession(ctx, "sess-respawn")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}

	// Subscribe to respawn notifications
	respawned := make(chan *mcp.ClientSession, 1)
	sm.Subscribe("sess-respawn", func(newDs *mcp.ClientSession) {
		respawned <- newDs
	})

	// Crash the subprocess by closing the downstream directly
	if err := ds1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Trigger respawn by calling GetOrCreateSession (should detect dead subprocess)
	// Give a moment for the process to actually die
	time.Sleep(100 * time.Millisecond)

	ds2, err := sm.GetOrCreateSession(ctx, "sess-respawn")
	if err != nil {
		t.Fatalf("GetOrCreateSession after crash failed: %v", err)
	}

	if ds2 == ds1 {
		t.Error("respawn returned same downstream session")
	}

	// Verify subscriber was notified
	select {
	case newDs := <-respawned:
		if newDs != ds2 {
			t.Error("subscriber received different session than GetOrCreateSession")
		}
	case <-time.After(5 * time.Second):
		t.Error("subscriber was not notified of respawn")
	}
}

// TestSharedManager_HealthProbe verifies that the health probe runs and can
// detect a dead subprocess, triggering respawn.
func TestSharedManager_HealthProbe(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()
	cfg.HealthCheckInterval = config.Duration(200 * time.Millisecond)

	sm := NewSharedSessionManager("test-shared", cfg, logger)
	defer sm.CloseAll()

	// Spawn and start health probe
	_, err := sm.GetOrCreateSession(ctx, "sess-health")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}

	sm.StartHealthProbe(ctx)

	// Subscribe to respawn
	respawned := make(chan *mcp.ClientSession, 1)
	sm.Subscribe("sess-health", func(newDs *mcp.ClientSession) {
		respawned <- newDs
	})

	// Kill the subprocess
	ds := sm.Downstream()
	if err := ds.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Wait for health probe to detect and respawn
	select {
	case <-respawned:
		// Success - health probe detected crash and respawned
	case <-time.After(5 * time.Second):
		t.Error("health probe did not trigger respawn within timeout")
	}

	// Verify downstream is alive again
	if !sm.HasDownstream() {
		t.Error("downstream should be alive after health probe respawn")
	}
}

// TestSharedManager_AdmissionControl verifies that MaxSessions limits the
// number of upstream sessions.
func TestSharedManager_AdmissionControl(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()
	cfg.MaxSessions = 2

	sm := NewSharedSessionManager("test-shared", cfg, logger)
	defer sm.CloseAll()

	// First two should succeed
	_, err := sm.GetOrCreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("first session failed: %v", err)
	}
	_, err = sm.GetOrCreateSession(ctx, "sess-2")
	if err != nil {
		t.Fatalf("second session failed: %v", err)
	}

	// Third should fail
	_, err = sm.GetOrCreateSession(ctx, "sess-3")
	if err == nil {
		t.Fatal("expected error for third session (max reached), got nil")
	}

	// Verify admission status
	atCapacity, current, max := sm.AdmissionStatus()
	if !atCapacity {
		t.Error("expected atCapacity=true")
	}
	if current != 2 {
		t.Errorf("current = %d, want 2", current)
	}
	if max != 2 {
		t.Errorf("max = %d, want 2", max)
	}

	// Remove one, then third should succeed
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	_, err = sm.GetOrCreateSession(ctx, "sess-3")
	if err != nil {
		t.Fatalf("third session after removal failed: %v", err)
	}
}

// TestSharedManager_CloseAll terminates the shared subprocess and clears state.
func TestSharedManager_CloseAll(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-shared", cfg, logger)

	// Spawn some sessions
	for i := 0; i < 3; i++ {
		_, err := sm.GetOrCreateSession(ctx, fmt.Sprintf("sess-%d", i))
		if err != nil {
			t.Fatalf("GetOrCreateSession failed: %v", err)
		}
	}

	if !sm.HasDownstream() {
		t.Fatal("expected downstream before CloseAll")
	}

	// CloseAll
	sm.CloseAll()

	// Downstream should be gone
	if sm.HasDownstream() {
		t.Error("expected no downstream after CloseAll")
	}

	// Refcount should be reset
	if sm.RefCount() != 0 {
		t.Errorf("refcount after CloseAll = %d, want 0", sm.RefCount())
	}

	// Sessions should be empty
	if sm.SessionCount() != 0 {
		t.Errorf("session count after CloseAll = %d, want 0", sm.SessionCount())
	}

	// Calling CloseAll again should be safe
	sm.CloseAll()
}
