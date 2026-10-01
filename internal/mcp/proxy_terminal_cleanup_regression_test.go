package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/slots"
)

// TestLateStatefulInitializationDeleteCleansOwner proves the terminal close
// of a stateful proxy whose generation close was already consumed: a manager
// removal lands before notifications/initialized, the late initialization
// publishes the proxy anyway so the next tools/call can respawn, and the
// production DELETE must then remove the removal-owner entry even though the
// generation close cannot run again. Skipping the terminal cleanup strands
// the published owner in the index with manager population 0 and gauge 0.
func TestLateStatefulInitializationDeleteCleansOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("late-delete-cleanup", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	mgr.SetOnSessionRemoved(idx.closeOwner)
	var ps *proxySession
	_, err := newPerSessionServer(ctx, "late-delete-cleanup", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) }, nil, nil, idx.rekey, idx.unregister)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveSession(ps.downstreamID()); err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	ps.upstreamSessionID = "late-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(func(_ string, p *proxySession) { idx.register(p) })
	// The production DELETE close: it must end tracking without relying on a
	// respawn to re-arm the consumed generation close.
	ps.closeDownstream("upstream delete")
	_ = mgr.RemoveSession(ps.downstreamID())
	if mgr.SessionCount() != 0 || owner.Snapshot().ActiveSessions != 0 {
		t.Fatal("fixture did not reach the zero-manager/zero-gauge state")
	}
	if n := idx.count(); n != 0 {
		t.Fatalf("late stateful initialization followed by DELETE retains %d downstream owner(s), want 0", n)
	}
}

// TestSlotLateInitializationDeleteReleasesAdmission drives the real
// slot-group HTTP surface: a manager removal before notifications/initialized
// leaves the group reservation held, the late initialization rebinds it to the
// upstream session id, and the DELETE must release it through the terminal
// cleanup. A retained reservation answers the next initialize with 429 even
// though the manager holds no session and the gauge is 0.
func TestSlotLateInitializationDeleteReleasesAdmission(t *testing.T) {
	reviewSlotDeleteAdmission(t, true)
}

// TestSlotNormalDeleteReleasesAdmission is the control for the late
// initialization probe: a normal initialize/notifications/initialized/tools
// cycle followed by DELETE must keep the slot-group admission released.
func TestSlotNormalDeleteReleasesAdmission(t *testing.T) {
	reviewSlotDeleteAdmission(t, false)
}

func reviewSlotDeleteAdmission(t *testing.T, removeBeforeInitialized bool) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger := testLogger(t)
	cfg := testServerConfig()
	cfg.MaxSessions = 1
	mgr := session.NewManager("slot-delete-admission", cfg, logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	selector := slots.NewMultiplexer("slot-delete-group", logger, []slots.Entry{
		{SlotName: "slot-delete-slot", Index: 1, Manager: mgr, Reporter: owner},
	})
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "slot-delete-group", Selector: selector, Logger: logger, Metrics: owner,
	}))
	defer ts.Close()
	request := func(method, body, sid string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), string(data)
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"review","version":"1"}}}`
	status, sid, body := request(http.MethodPost, initialize, "")
	if status != 200 || sid == "" {
		t.Fatalf("initial initialize: %d %s", status, body)
	}
	ids := mgr.Sessions()
	if len(ids) != 1 {
		t.Fatalf("initial manager population: %v", ids)
	}
	if removeBeforeInitialized {
		if err := mgr.RemoveSession(ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	request(http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid)
	status, _, body = request(http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, sid)
	if status != 200 {
		t.Fatalf("late initialization barrier: %d %s", status, body)
	}
	status, _, body = request(http.MethodDelete, "", sid)
	if status >= 400 {
		t.Fatalf("DELETE: %d %s", status, body)
	}
	if mgr.SessionCount() != 0 || owner.Snapshot().ActiveSessions != 0 {
		t.Fatal("fixture did not reach zero-manager/zero-gauge after DELETE")
	}
	atCapacity, current, max := selector.AdmissionStatus()
	status, _, body = request(http.MethodPost, initialize, "")
	if status != 200 {
		t.Fatalf("initialize after DELETE: HTTP %d; manager=0 gauge=0 selector=(full=%v current=%d max=%d); body=%s", status, atCapacity, current, max, body)
	}
}

// TestSharedRemovalBeforePublicationCleansOwner proves the shared-mode close
// that lands inside the publication window: publishInitialized releases
// closeMu before the publish callback takes the index lock, so a manager
// expiry can finish its real close path in that gap and the callback then
// reinserts the closed proxy. The consumed generation close can never run
// again and no lease remains to expire, so publishInitialized itself must
// detect the lost generation after the publication and run the terminal
// cleanup — with no later DELETE supplying it.
func TestSharedRemovalBeforePublicationCleansOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger := testLogger(t)
	owner := metrics.NewServerMetrics()
	sm := session.NewSharedSessionManager("shared-publish-cleanup", testServerConfig(), logger, 0, owner)
	defer sm.CloseAll()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	var ps *proxySession
	_, err := newSharedModeServer(ctx, "shared-publish-cleanup", sm, logger,
		nil, nil, nil, time.Second, RetryConfig{}, CircuitBreakerConfig{}, owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) }, nil, nil, idx.unregister)
	if err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	ps.upstreamSessionID = "shared-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(func(_ string, p *proxySession) {
		// The close lands before the callback takes the index lock; reproduce
		// that ordering, then perform the production insert.
		idx.closeOwner(p.downstreamID())
		idx.register(p)
	})
	if sm.SessionCount() != 0 || owner.Snapshot().ActiveSessions != 0 {
		t.Fatal("fixture did not reach the zero-manager/zero-gauge state")
	}
	if n := idx.count(); n != 0 {
		t.Fatalf("shared removal inside the publication window retains %d downstream owner(s) without a later close, want 0", n)
	}
}

// TestStatefulRemovalInsidePublicationWindowCleansOwner is the stateful twin
// of the shared publication-window proof: a manager removal completes inside
// the gap between publishInitialized's liveness check and the publish
// callback, the callback reinserts the closed proxy into the owner index, and
// no later DELETE arrives. The publication-window recheck must close the lost
// generation and remove the reinserted owner; a retained owner pins the
// upstream session's tracking until an unrelated close.
func TestStatefulRemovalInsidePublicationWindowCleansOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("stateful-publish-window", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	idx := &lifecycleOwnerIndex{owners: map[string]*proxySession{}}
	mgr.SetOnSessionRemoved(idx.closeOwner)
	var ps *proxySession
	_, err := newPerSessionServer(ctx, "stateful-publish-window", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		owner, nil, nil,
		func(p *proxySession) { ps = p; idx.register(p) }, nil, nil, idx.rekey, idx.unregister)
	if err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	ps.upstreamSessionID = "stateful-window-upstream"
	ps.mu.Unlock()
	ps.publishInitialized(func(_ string, p *proxySession) {
		// The manager removal lands inside the publication window: the real
		// removal path deletes the tracked session, fires the owner close,
		// and only then does the callback perform the production insert.
		if err := mgr.RemoveSession(p.downstreamID()); err != nil {
			t.Errorf("removal inside publication window: %v", err)
		}
		idx.register(p)
	})
	if mgr.SessionCount() != 0 || owner.Snapshot().ActiveSessions != 0 {
		t.Fatal("fixture did not reach the zero-manager/zero-gauge state")
	}
	if n := idx.count(); n != 0 {
		t.Fatalf("stateful removal inside the publication window retains %d downstream owner(s) without a later close, want 0", n)
	}
}
