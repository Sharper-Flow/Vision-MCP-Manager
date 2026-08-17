package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
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

type testServerMetricsReporter struct {
	mu      sync.Mutex
	reasons map[string]int
}

func newTestServerMetricsReporter() *testServerMetricsReporter {
	return &testServerMetricsReporter{reasons: make(map[string]int)}
}

func (r *testServerMetricsReporter) IncActiveSessions()  {}
func (r *testServerMetricsReporter) DecActiveSessions()  {}
func (r *testServerMetricsReporter) IncAdmissionDenied() {}

func (r *testServerMetricsReporter) IncReaped(reason string) {
	r.mu.Lock()
	r.reasons[reason]++
	r.mu.Unlock()
}

func (r *testServerMetricsReporter) reapCount(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasons[reason]
}

// TestSharedManager_SpawnOnFirstUse verifies that the first GetOrCreateSession
// lazily spawns the shared downstream subprocess.
func TestSharedManager_SpawnOnFirstUse(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)
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

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)
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

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)
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

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)
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

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)
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

func TestSharedManager_HealthCheckRecordsReachability(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()
	store := reachability.NewStore()
	sm := NewSharedSessionManager("test-reachability", cfg, logger, 0, nil)
	defer sm.CloseAll()
	sm.SetReachabilityStore(store)

	if _, err := sm.GetOrCreateSession(ctx, "sess-reachability"); err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	sm.StartHealthProbe(ctx)
	sm.healthCheck()

	got, ok := store.Get("test-reachability")
	if !ok {
		t.Fatal("successful health check did not create reachability evidence")
	}
	evidence, ok := got.Evidence[reachability.DepthSession]
	if !ok {
		t.Fatal("successful health check did not record session-depth evidence")
	}
	if evidence.LastProbeOutcome != reachability.OutcomeSuccess {
		t.Fatalf("probe outcome = %q, want %q", evidence.LastProbeOutcome, reachability.OutcomeSuccess)
	}

	ds := sm.Downstream()
	if ds == nil {
		t.Fatal("expected downstream session")
	}
	if err := ds.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	sm.healthCheck()

	got, ok = store.Get("test-reachability")
	if !ok {
		t.Fatal("failed health check removed reachability evidence")
	}
	evidence = got.Evidence[reachability.DepthSession]
	if evidence.LastProbeOutcome != reachability.OutcomeFailure {
		t.Fatalf("failed probe outcome = %q, want %q", evidence.LastProbeOutcome, reachability.OutcomeFailure)
	}
	if evidence.LastProbeError == "" {
		t.Fatal("failed probe did not record an error")
	}
}

func TestSharedManager_HealthCheckNilDownstreamLeavesUnprobed(t *testing.T) {
	store := reachability.NewStore()
	sm := NewSharedSessionManager("test-idle-unprobed", testSharedServerConfig(), testLogger(t), 0, nil)
	defer sm.CloseAll()
	sm.SetReachabilityStore(store)
	sm.healthCtx = context.Background()

	sm.healthCheck()

	got, ok := store.Get("test-idle-unprobed")
	if ok {
		t.Fatalf("nil-downstream health check recorded evidence: %#v", got)
	}
	if got.State != reachability.StateUnprobed {
		t.Fatalf("unseen server state = %q, want %q", got.State, reachability.StateUnprobed)
	}
}

func TestSharedManager_HealthCheckNilStoreIsSafe(t *testing.T) {
	sm := NewSharedSessionManager("test-nil-store", testSharedServerConfig(), testLogger(t), 0, nil)
	defer sm.CloseAll()
	sm.healthCtx = context.Background()
	sm.SetReachabilityStore(nil)
	sm.healthCheck()
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

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)
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

	sm := NewSharedSessionManager("test-shared", cfg, logger, 0, nil)

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

// TestIdleReap_ZeroRefsTriggersReap verifies that the downstream subprocess is
// torn down after the idle timeout when all upstream sessions are removed.
func TestIdleReap_ZeroRefsTriggersReap(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()
	reporter := newTestServerMetricsReporter()

	sm := NewSharedSessionManager("test-idle-reap", cfg, logger, 50*time.Millisecond, reporter)
	defer sm.CloseAll()

	// Create session
	_, err := sm.GetOrCreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}

	if !sm.HasDownstream() {
		t.Fatal("expected downstream after session creation")
	}

	// Remove session → refCount == 0 → idle timer starts
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	// Downstream should still be alive immediately after removal
	if !sm.HasDownstream() {
		t.Error("downstream should still be alive immediately after zero refs (timer not fired)")
	}

	// Wait for idle timer to fire
	time.Sleep(150 * time.Millisecond)

	// Downstream should be reaped
	if sm.HasDownstream() {
		t.Error("expected downstream to be reaped after idle timeout")
	}
	if got := reporter.reapCount(metrics.ReapReasonIdleTimeout); got != 1 {
		t.Errorf("idle timeout reap count = %d, want 1", got)
	}
}

// TestIdleReap_NewSessionCancelsTimer verifies that a new GetOrCreateSession
// during the idle period cancels the reap timer.
func TestIdleReap_NewSessionCancelsTimer(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-idle-cancel", cfg, logger, 200*time.Millisecond, nil)
	defer sm.CloseAll()

	// Create and remove session → idle timer starts
	_, err := sm.GetOrCreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	// New session before timeout → timer cancelled
	_, err = sm.GetOrCreateSession(ctx, "sess-2")
	if err != nil {
		t.Fatalf("GetOrCreateSession during idle period failed: %v", err)
	}

	// Wait past original timeout
	time.Sleep(350 * time.Millisecond)

	// Downstream should still be alive (timer was cancelled)
	if !sm.HasDownstream() {
		t.Error("expected downstream to survive — new session should have cancelled idle timer")
	}
}

// TestIdleReap_DisabledNegative verifies that a negative idle timeout disables
// idle reaping entirely.
func TestIdleReap_DisabledNegative(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-idle-disabled", cfg, logger, 0, nil) // 0 = disabled
	defer sm.CloseAll()

	_, err := sm.GetOrCreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	// Wait past what would be a short timeout
	time.Sleep(150 * time.Millisecond)

	// Downstream should still be alive (idle reaping disabled)
	if !sm.HasDownstream() {
		t.Error("expected downstream to survive — idle reaping disabled (timeout=0)")
	}
}

// TestIdleReap_CloseAllDuringTimer verifies that CloseAll during a pending
// idle timer does not deadlock and cleans up correctly.
func TestIdleReap_CloseAllDuringTimer(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-idle-closeall", cfg, logger, 5*time.Second, nil)
	defer sm.CloseAll()

	_, err := sm.GetOrCreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	// CloseAll while timer is pending — should not deadlock
	done := make(chan struct{})
	go func() {
		sm.CloseAll()
		close(done)
	}()

	select {
	case <-done:
		// Success — no deadlock
	case <-time.After(3 * time.Second):
		t.Fatal("CloseAll deadlocked during pending idle timer")
	}

	if sm.HasDownstream() {
		t.Error("expected no downstream after CloseAll")
	}
}

// TestIdleReap_RespawnAfterReap verifies that GetOrCreateSession spawns a
// fresh downstream after idle reaping.
func TestIdleReap_RespawnAfterReap(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-idle-respawn", cfg, logger, 50*time.Millisecond, nil)
	defer sm.CloseAll()

	// Create → remove → wait for reap
	ds1, err := sm.GetOrCreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	if err := sm.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if sm.HasDownstream() {
		t.Fatal("expected downstream to be reaped")
	}

	// New session should respawn
	ds2, err := sm.GetOrCreateSession(ctx, "sess-2")
	if err != nil {
		t.Fatalf("GetOrCreateSession after reap failed: %v", err)
	}
	if ds2 == nil {
		t.Fatal("expected non-nil downstream after respawn")
	}
	if ds2 == ds1 {
		t.Error("respawn should produce a different downstream session")
	}
}

// TestSharedManager_RefcountLifecycle_WithIdleReap is the updated version of
// TestSharedManager_RefcountLifecycle that accounts for idle reaping.
// With idle reaping enabled, the downstream is alive at t=0 after zero refs,
// then reaped after the idle timeout.
func TestSharedManager_RefcountLifecycle_WithIdleReap(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testSharedServerConfig()

	sm := NewSharedSessionManager("test-lifecycle", cfg, logger, 50*time.Millisecond, nil)
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

	// Subprocess should still be alive immediately after zero refs (timer hasn't fired)
	if !sm.HasDownstream() {
		t.Error("downstream should still be alive immediately after zero refs")
	}

	// After idle timeout, subprocess should be reaped
	time.Sleep(150 * time.Millisecond)

	if sm.HasDownstream() {
		t.Error("expected downstream to be reaped after idle timeout")
	}
}
