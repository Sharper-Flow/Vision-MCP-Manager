package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// spawnWindowHookHandler wraps a slog handler and runs a hook on every record
// so tests can act at the instant the manager logs an event.
type spawnWindowHookHandler struct {
	base slog.Handler
	hook func(slog.Record)
}

func (h *spawnWindowHookHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *spawnWindowHookHandler) Handle(ctx context.Context, r slog.Record) error {
	h.hook(r)
	return h.base.Handle(ctx, r)
}

func (h *spawnWindowHookHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &spawnWindowHookHandler{base: h.base.WithAttrs(a), hook: h.hook}
}

func (h *spawnWindowHookHandler) WithGroup(g string) slog.Handler {
	return &spawnWindowHookHandler{base: h.base.WithGroup(g), hook: h.hook}
}

// lifecycleOwnerIndex emulates the removal-owner index that NewProxyHandler
// wires around newPerSessionServer: onSpawned registers by downstream session
// ID, onDownstreamClosed removes unconditionally by downstream session ID,
// and the manager removal callback closes the owner it finds.
type lifecycleOwnerIndex struct {
	mu     sync.Mutex
	owners map[string]*proxySession
}

func (idx *lifecycleOwnerIndex) register(ps *proxySession) {
	idx.mu.Lock()
	idx.owners[ps.sessionID] = ps
	idx.mu.Unlock()
}

func (idx *lifecycleOwnerIndex) rekey(oldSessionID, newSessionID string, ps *proxySession) {
	idx.mu.Lock()
	delete(idx.owners, oldSessionID)
	idx.owners[newSessionID] = ps
	idx.mu.Unlock()
}

func (idx *lifecycleOwnerIndex) lookup(sessionID string) *proxySession {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.owners[sessionID]
}

func (idx *lifecycleOwnerIndex) unregister(sessionID string) {
	idx.mu.Lock()
	delete(idx.owners, sessionID)
	idx.mu.Unlock()
}

func (idx *lifecycleOwnerIndex) closeOwner(sessionID string) {
	ps := idx.lookup(sessionID)
	idx.unregister(sessionID)
	if ps != nil {
		ps.closeDownstream("session removed by manager")
	}
}

func (idx *lifecycleOwnerIndex) count() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return len(idx.owners)
}

// TestStatefulRemovalInsideSpawnWindowLeavesNoOwner proves a manager removal
// that lands between SpawnSession publishing the tracked session and the
// onSpawned owner registration ends with zero retained owners once the initial
// tools/list fails. The manager deletes the tracked session before it fires
// removal callbacks, so the callback finds no owner; the failed-discovery
// closeDownstream must then remove the late registration through the
// unconditional downstream-keyed cleanup, or every rejected initialization
// leaks one orphan owner.
func TestStatefulRemovalInsideSpawnWindowLeavesNoOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	var mgr *session.Manager
	// The hook fires when the manager logs the connected session, between the
	// map publication inside SpawnSession and the caller's onSpawned
	// registration: exactly the window under test.
	logger := slog.New(&spawnWindowHookHandler{
		base: slog.NewTextHandler(io.Discard, nil),
		hook: func(r slog.Record) {
			if r.Message != "downstream subprocess connected" {
				return
			}
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "session_id" {
					if err := mgr.RemoveSession(a.Value.String()); err != nil {
						t.Errorf("removal inside spawn window: %v", err)
					}
				}
				return true
			})
		},
	})
	mgr = session.NewManager("lifecycle-spawn-window", testServerConfig(), logger)
	defer mgr.CloseAll()
	mgr.SetOnSessionRemoved(idx.closeOwner)

	_, err := newPerSessionServer(ctx, "lifecycle-spawn-window", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		metrics.NewServerMetrics(), nil, nil,
		idx.register, nil, nil, idx.rekey, idx.unregister,
	)
	if err == nil {
		t.Fatal("fixture did not remove the initial downstream before discovery")
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager population after rejected initialization = %d, want 0", n)
	}
	if got := idx.count(); got != 0 {
		t.Errorf("removal-owner index retains %d rejected initialization(s) after a removal inside the spawn window, want 0", got)
	}
}

// TestAbandonedRespawnDetachesAndNextCallRespawns proves the full
// respawn-abandonment lifecycle: a manager removal that lands while a respawn
// generation is being admitted must leave the proxy detached with no manager
// session and gauge 0, the next tools/call must respawn and succeed (gauge 1
// with one respawned manager session), and the upstream DELETE must balance
// back to gauge 0 with no manager session. Publishing the generation before
// the survival check instead leaves a closed SDK pointer marked live with a
// consumed closeOnce, and the next call fails against it instead of
// respawning.
func TestAbandonedRespawnDetachesAndNextCallRespawns(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("lifecycle-abandoned-respawn", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()

	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	mgr.SetOnSessionRemoved(idx.closeOwner)
	var ps *proxySession
	_, err := newPerSessionServer(ctx, "lifecycle-abandoned-respawn", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) },
		nil, nil, idx.rekey, idx.unregister,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a completed upstream initialization: the session holds its
	// active-session credit.
	ps.mu.Lock()
	ps.upstreamSessionID = "lifecycle-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(nil)
	if got := owner.Snapshot().ActiveSessions; got != 1 {
		t.Fatalf("active sessions after initialization = %d, want 1", got)
	}

	// Reap the downstream the way the idle reaper does: the credit releases
	// and the proxy detaches.
	if err := mgr.RemoveSession(ps.sessionID); err != nil {
		t.Fatal(err)
	}

	// Hold the close transition and start the respawn. It proceeds through
	// spawn, owner rekey, and tool discovery, then blocks on closeMu at
	// generation admission — the point where the survival check runs.
	ps.closeMu.Lock()
	respawnDone := make(chan error, 1)
	go func() { _, err := ps.respawnDownstream(ctx, "lifecycle"); respawnDone <- err }()
	// Wait until the respawn generation is fully connected in the manager —
	// GetSession is nil while SpawnSession has only reserved the slot.
	var respawnID string
	deadline := time.Now().Add(15 * time.Second)
	for {
		ids := mgr.Sessions()
		if len(ids) == 1 && strings.Contains(ids[0], "-respawn-") && mgr.GetSession(ids[0]) != nil {
			respawnID = ids[0]
			break
		}
		if time.Now().After(deadline) {
			ps.closeMu.Unlock()
			t.Fatalf("respawn never reached generation admission; manager population %v", mgr.Sessions())
		}
		time.Sleep(time.Millisecond)
	}

	// Remove the fresh generation the way the manager does: the tracked
	// session is deleted first, then the removal callback closes the owner it
	// finds. Signal before that close so the test can release closeMu and let
	// both critical sections settle.
	removalEntered := make(chan struct{})
	mgr.SetOnSessionRemoved(func(id string) {
		p := idx.lookup(id)
		idx.unregister(id)
		close(removalEntered)
		if p != nil {
			p.closeDownstream("session removed by manager")
		}
	})
	removalDone := make(chan error, 1)
	go func() { removalDone <- mgr.RemoveSession(respawnID) }()
	<-removalEntered
	ps.closeMu.Unlock()
	if err := <-removalDone; err != nil {
		t.Fatal(err)
	}
	// The signaling callback served its one removal; restore the default
	// owner-closing callback for the rest of the lifecycle.
	mgr.SetOnSessionRemoved(idx.closeOwner)

	// The removed generation must not be admitted: the respawn reports
	// abandonment, the manager holds no session, the gauge is 0, and the
	// proxy stays detached.
	if err := <-respawnDone; err == nil {
		t.Fatal("a manager-removed respawn generation was admitted as live")
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager sessions after abandoned respawn = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Fatalf("active sessions after abandoned respawn = %d, want 0 before the next call", got)
	}
	ps.downstreamMu.RLock()
	retained := ps.downstream != nil && !ps.downstreamClosed
	ps.downstreamMu.RUnlock()
	if retained {
		t.Error("abandoned respawn retains a closed SDK downstream as live; later calls dispatch to it instead of respawning")
	}

	// The next real tools/call must respawn a live generation and succeed.
	result, callErr := makeProxyToolHandler(ps, "echo")(ctx, &sdkmcp.CallToolRequest{
		Params: &sdkmcp.CallToolParamsRaw{Name: "echo"},
	})
	if callErr != nil {
		t.Fatalf("next tools/call failed instead of respawning: %v", callErr)
	}
	if result == nil || result.IsError {
		t.Fatalf("next tools/call returned an error result: %+v", result)
	}

	// One respawned manager session, and the initialized upstream session
	// holds exactly one active-session credit again.
	ids := mgr.Sessions()
	if len(ids) != 1 || !strings.Contains(ids[0], "-respawn-") {
		t.Fatalf("manager population after successful call = %v, want one respawned session", ids)
	}
	if got := owner.Snapshot().ActiveSessions; got != 1 {
		t.Errorf("active sessions after successful call = %d, want 1", got)
	}

	// The upstream DELETE must balance the session and its credit.
	ps.closeDownstream("upstream delete")
	if err := mgr.RemoveSession(ps.sessionID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatal(err)
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Errorf("manager sessions after DELETE = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("active sessions after DELETE = %d, want 0", got)
	}
	if got := idx.count(); got != 0 {
		t.Errorf("removal-owner index after DELETE retains %d owner(s), want 0", got)
	}
}
