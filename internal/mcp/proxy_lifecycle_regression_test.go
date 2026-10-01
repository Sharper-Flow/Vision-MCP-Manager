package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
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
	idx.owners[ps.downstreamID()] = ps
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

// keys returns the sorted owner keys for precise assertions about which
// registrations survive a lifecycle transition.
func (idx *lifecycleOwnerIndex) keys() []string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := make([]string, 0, len(idx.owners))
	for k := range idx.owners {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// TestSharedExpiryInsideSpawnWindowRefusesPhantomCredit proves a shared-lease
// expiry that lands during startup — after the manager tracks the lease but
// before the proxy finishes initializing — cannot credit a phantom session.
// The removal owner is registered before the lease exists, so the expiry
// reaches closeDownstream, marks the generation closed, and late
// initialization refuses credit: gauge 0 with manager population 0. When the
// owner was registered only after GetOrCreateSession returned, the expiry
// found no owner, discovery still succeeded against the live shared
// downstream, and publishInitialized granted a phantom credit.
func TestSharedExpiryInsideSpawnWindowRefusesPhantomCredit(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := metrics.NewServerMetrics()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(time.Nanosecond)
	var sm *session.SharedSessionManager
	// The shared manager holds sm.mu while it spawns, but the reaper retires
	// the fresh lease under refMu independently. Hold the spawn inside the
	// hook until the lease is gone, so the expiry provably lands inside the
	// startup window.
	logger := slog.New(&spawnWindowHookHandler{
		base: slog.NewTextHandler(io.Discard, nil),
		hook: func(r slog.Record) {
			if r.Message != "spawning shared downstream subprocess" {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for sm.SessionCount() != 0 {
				if time.Now().After(deadline) {
					t.Error("real reaper did not remove the pending lease")
					return
				}
				time.Sleep(time.Millisecond)
			}
		},
	})
	sm = session.NewSharedSessionManager("shared-expiry-window", cfg, logger, 0, owner)
	defer sm.CloseAll()
	// Mirror NewProxyHandler's expiry callback over the test index.
	sm.SetOnSessionExpired(func(sessionID string) {
		ps := idx.lookup(sessionID)
		idx.unregister(sessionID)
		if ps != nil {
			ps.closeDownstream("idle_timeout")
		}
		_ = sm.RemoveSession(sessionID)
	})
	sm.StartReaper(ctx, time.Millisecond)

	var ps *proxySession
	_, err := newSharedModeServer(ctx, "shared-expiry-window", sm, logger,
		nil, nil, nil, time.Second, RetryConfig{}, CircuitBreakerConfig{}, owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) }, nil, nil, idx.unregister,
	)
	if err != nil {
		t.Fatalf("shared startup with an expiring lease failed: %v", err)
	}
	if n := sm.SessionCount(); n != 0 {
		t.Fatalf("manager population after the startup expiry = %d, want 0", n)
	}
	// Late initialization of the expired generation must not credit it.
	ps.mu.Lock()
	ps.upstreamSessionID = "late-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(nil)
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("late initialization acquired a phantom shared credit: gauge=%d manager=%d, want 0/0",
			got, sm.SessionCount())
	}
	ps.closeDownstream("test cleanup")
}

// TestRespawnRemovalInsideSpawnWindowLeavesNoStaleOwner proves a manager
// removal that lands inside the respawn's SpawnSession — before the
// removal-owner rekey — leaves no retained owner after the failed discovery.
// The removal finds no owner, the rekey then installs the dead generation,
// and the failure path must remove the rekeyed registration unconditionally,
// including when RemoveSession reports ErrSessionNotFound because the
// manager already dropped the removed generation.
func TestRespawnRemovalInsideSpawnWindowLeavesNoStaleOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	var mgr *session.Manager
	logger := slog.New(&spawnWindowHookHandler{
		base: slog.NewTextHandler(io.Discard, nil),
		hook: func(r slog.Record) {
			if r.Message != "downstream subprocess connected" {
				return
			}
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "session_id" && strings.Contains(a.Value.String(), "-respawn-") {
					if err := mgr.RemoveSession(a.Value.String()); err != nil {
						t.Error(err)
					}
				}
				return true
			})
		},
	})
	mgr = session.NewManager("generation-window", testServerConfig(), logger)
	defer mgr.CloseAll()
	mgr.SetOnSessionRemoved(idx.closeOwner)
	var ps *proxySession
	_, err := newPerSessionServer(ctx, "generation-window", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		metrics.NewServerMetrics(), nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) }, nil, nil, idx.rekey, idx.unregister,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveSession(ps.downstreamID()); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.respawnDownstream(ctx, "test"); err == nil {
		t.Fatal("expected removed respawn to fail discovery")
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager population = %d, want 0", n)
	}
	if n := idx.count(); n != 0 {
		t.Errorf("removed respawn retained %d owner(s) after failed discovery, want 0", n)
	}
}

// waitForTerminalClose404 polls a raw tools/call carrying the expired
// session id until the terminal close has retired the upstream session and
// the streamable handler answers 404, which is the lifecycle evidence that
// initialization processing settled and the dead generation is unreachable.
// A 200 means the session is still registered and is retried; a transport
// error is the close cutting the in-flight request and is retried too. Any
// other status fails immediately. A 200 persisted until the deadline proves
// the dead generation still dispatches.
func waitForTerminalClose404(t *testing.T, ctx context.Context, ts *httptest.Server, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":100,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := ts.Client().Do(req)
		if err != nil {
			lastErr = err
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("raw tools/call on the expired session returned status %d, want 404", resp.StatusCode)
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("terminal close never retired the expired session; last raw call failed with %v", lastErr)
			}
			t.Fatal("expired session remains reusable: status 200 persisted until the deadline, want 404")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSharedReaperDuringSpawnDoesNotCreditSession proves the end-to-end
// shared lifecycle: a real reaper (session timeout 1ns, 1ms checks) expires
// the upstream lease while the shared downstream is still spawning. The
// registration before the lease makes the expiry reach closeDownstream, so
// the completed initialization reports no credit and the manager holds no
// session.
func TestSharedReaperDuringSpawnDoesNotCreditSession(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(time.Nanosecond)
	owner := metrics.NewServerMetrics()
	var sm *session.SharedSessionManager
	logger := slog.New(&spawnWindowHookHandler{
		base: slog.NewTextHandler(io.Discard, nil),
		hook: func(r slog.Record) {
			if r.Message != "spawning shared downstream subprocess" {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for sm.SessionCount() != 0 {
				if time.Now().After(deadline) {
					t.Error("real reaper did not remove the pending lease")
					return
				}
				time.Sleep(time.Millisecond)
			}
		},
	})
	sm = session.NewSharedSessionManager("real-shared-expiry", cfg, logger, 0, owner)
	defer sm.CloseAll()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:    "real-shared-expiry",
		SharedManager: sm,
		Metrics:       owner,
		Logger:        logger,
	}))
	defer ts.Close()
	sm.StartReaper(ctx, time.Millisecond)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "probe", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// Lifecycle completion evidence in place of a request barrier: the
	// terminal close of the expired generation runs asynchronously after
	// initialization processing and may cut an in-flight request, so the
	// 404 that follows it is the deterministic signal that processing
	// settled. A 200 persisted to the deadline would mean the dead
	// generation still dispatches.
	waitForTerminalClose404(t, ctx, ts, cs.ID())
	if n := sm.SessionCount(); n != 0 {
		t.Fatalf("manager population = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("real shared reaper left phantom gauge=%d with manager population=0", got)
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
	if err := mgr.RemoveSession(ps.downstreamID()); err != nil {
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
	if err := mgr.RemoveSession(ps.downstreamID()); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
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

// TestExpiredSharedStartupTerminalCloseInvalidatesUpstream proves the
// terminal finalization of a shared proxy whose downstream expired during
// startup: late initialization must not publish the dead owner back into the
// indexes, and the late-initialized upstream session is closed through the
// re-armed close path, so a raw tools/call reusing the expired session id is
// answered 404 instead of dispatching into the dead generation. Publishing
// the dead owner used to leave the expired upstream usable, answering 200
// with an isError result on every attempt.
func TestExpiredSharedStartupTerminalCloseInvalidatesUpstream(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(time.Nanosecond)
	owner := metrics.NewServerMetrics()
	var sm *session.SharedSessionManager
	// The shared manager holds sm.mu while it spawns, but the reaper retires
	// the fresh lease under refMu independently. Hold the spawn inside the
	// hook until the lease is gone, so the expiry provably lands inside the
	// startup window.
	logger := slog.New(&spawnWindowHookHandler{
		base: slog.NewTextHandler(io.Discard, nil),
		hook: func(r slog.Record) {
			if r.Message != "spawning shared downstream subprocess" {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for sm.SessionCount() != 0 {
				if time.Now().After(deadline) {
					t.Error("real reaper did not remove the pending lease")
					return
				}
				time.Sleep(time.Millisecond)
			}
		},
	})
	sm = session.NewSharedSessionManager("expired-startup-terminal", cfg, logger, 0, owner)
	defer sm.CloseAll()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:    "expired-startup-terminal",
		SharedManager: sm,
		Metrics:       owner,
		Logger:        logger,
	}))
	defer ts.Close()
	sm.StartReaper(ctx, time.Millisecond)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "probe", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if n := sm.SessionCount(); n != 0 {
		t.Fatalf("manager population = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Fatalf("active sessions = %d, want 0 before the raw call", got)
	}

	// Lifecycle completion evidence in place of a request barrier: the
	// terminal close removes the session from the streamable handler a
	// moment after initialization completes, so the raw call is answered
	// 404. The close may cut an in-flight raw call while it retires the
	// session; a 200 means the dead generation still dispatches, which the
	// unfixed proxy returned on every attempt until the deadline.
	waitForTerminalClose404(t, ctx, ts, cs.ID())
	if n := sm.SessionCount(); n != 0 {
		t.Errorf("manager population after the terminal close = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("active sessions after the terminal close = %d, want 0", got)
	}
}

// TestRemovalAfterRespawnKeysPublishedGeneration proves the respawn
// publication contract: once respawnDownstream returns, the proxy's
// downstream identifier is the rekeyed generation the manager holds, the
// removal-owner index holds exactly that one registration, and a manager
// removal of the published generation closes the proxy keyed on it, leaving
// no entry for either the old or the new identifier.
func TestRemovalAfterRespawnKeysPublishedGeneration(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("respawn-keying", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	mgr.SetOnSessionRemoved(idx.closeOwner)
	var ps *proxySession
	_, err := newPerSessionServer(ctx, "respawn-keying", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) },
		nil, nil, idx.rekey, idx.unregister,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Initialize the upstream session so the proxy holds one credit.
	ps.mu.Lock()
	ps.upstreamSessionID = "keying-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(nil)
	if got := owner.Snapshot().ActiveSessions; got != 1 {
		t.Fatalf("active sessions after initialization = %d, want 1", got)
	}

	// Reap the original downstream the way the idle reaper does.
	oldID := ps.downstreamID()
	if err := mgr.RemoveSession(oldID); err != nil {
		t.Fatal(err)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Fatalf("active sessions after the reap = %d, want 0", got)
	}

	// Respawn and prove the publication keyed everything on the new
	// generation.
	if _, err := ps.respawnDownstream(ctx, "test"); err != nil {
		t.Fatalf("respawn failed: %v", err)
	}
	newID := ps.downstreamID()
	if newID == oldID || !strings.Contains(newID, "-respawn-") {
		t.Fatalf("published identifier %q does not name the respawned generation (old %q)", newID, oldID)
	}
	if ids := mgr.Sessions(); len(ids) != 1 || ids[0] != newID {
		t.Fatalf("manager population after respawn = %v, want [%s]", ids, newID)
	}
	if keys := idx.keys(); len(keys) != 1 || keys[0] != newID {
		t.Fatalf("removal-owner index after respawn = %v, want [%s]", keys, newID)
	}
	if got := owner.Snapshot().ActiveSessions; got != 1 {
		t.Fatalf("active sessions after respawn = %d, want 1", got)
	}

	// A manager removal of the published generation must reach the proxy and
	// tear it down keyed on that generation.
	if err := mgr.RemoveSession(newID); err != nil {
		t.Fatal(err)
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Errorf("manager population after removal = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("active sessions after removal = %d, want 0", got)
	}
	if got := idx.count(); got != 0 {
		t.Errorf("removal-owner index after removal retains %v, want none", idx.keys())
	}
	ps.downstreamMu.RLock()
	retained := ps.downstream != nil && !ps.downstreamClosed
	ps.downstreamMu.RUnlock()
	if retained {
		t.Error("removal of the published generation left the proxy attached")
	}
}

// TestRemovalConcurrentWithRespawnPublicationLeavesNoStaleOwner drives a
// manager removal against the tail of a respawn publication and proves no
// stale removal-owner entry survives. The respawn publishes the new
// identifier inside the closeMu critical section before the credit
// reacquisition flips the gauge, so the gauge observation proves the
// identifier is published; the removal's closeDownstream must then key on
// the published generation and clean the rekeyed entry. Under -race this
// pins the identifier write/read pair that used to collide outside every
// lock and leave the rekeyed registration behind.
func TestRemovalConcurrentWithRespawnPublicationLeavesNoStaleOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("respawn-publication-race", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	mgr.SetOnSessionRemoved(idx.closeOwner)
	var ps *proxySession
	_, err := newPerSessionServer(ctx, "respawn-publication-race", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) },
		nil, nil, idx.rekey, idx.unregister,
	)
	if err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	ps.upstreamSessionID = "publication-race-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(nil)
	if got := owner.Snapshot().ActiveSessions; got != 1 {
		t.Fatalf("active sessions after initialization = %d, want 1", got)
	}
	if err := mgr.RemoveSession(ps.downstreamID()); err != nil {
		t.Fatal(err)
	}

	// Respawn in the background. The credit reacquisition inside the
	// publication critical section flips the gauge back to 1 the moment the
	// new generation is admitted.
	respawnDone := make(chan error, 1)
	go func() { _, err := ps.respawnDownstream(ctx, "test"); respawnDone <- err }()
	deadline := time.Now().Add(20 * time.Second)
	for owner.Snapshot().ActiveSessions != 1 {
		select {
		case err := <-respawnDone:
			t.Fatalf("respawn did not publish a generation; gauge stayed 0: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("respawn never published a live generation")
		}
		time.Sleep(time.Millisecond)
	}

	// Remove the published generation while the respawn goroutine is still
	// finishing its tail. The removal's closeDownstream must key on the
	// published generation.
	newID := ps.downstreamID()
	if !strings.Contains(newID, "-respawn-") {
		t.Fatalf("published identifier %q does not name a respawned generation", newID)
	}
	removalDone := make(chan error, 1)
	go func() { removalDone <- mgr.RemoveSession(newID) }()
	if err := <-removalDone; err != nil {
		t.Fatal(err)
	}
	if err := <-respawnDone; err != nil {
		t.Fatalf("respawn failed after a clean publication: %v", err)
	}

	if n := mgr.SessionCount(); n != 0 {
		t.Errorf("manager population after removal = %d, want 0", n)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("active sessions after removal = %d, want 0", got)
	}
	if got := idx.count(); got != 0 {
		t.Errorf("stale removal-owner entries survived the concurrent removal: %v", idx.keys())
	}
	ps.downstreamMu.RLock()
	retained := ps.downstream != nil && !ps.downstreamClosed
	ps.downstreamMu.RUnlock()
	if retained {
		t.Error("concurrent removal left the published generation attached")
	}
}
