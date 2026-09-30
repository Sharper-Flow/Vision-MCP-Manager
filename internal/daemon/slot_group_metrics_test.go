package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/slots"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// slotGroupEchoConfig builds a node-backed echo server config for direct
// session-manager use in slot-group gauge tests.
func slotGroupEchoConfig() *config.ServerConfig {
	return &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", acceptanceEchoMCPServerJS},
		Port:           16290,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}
}

// slotGroupTestLogger discards proxy lifecycle logs.
func slotGroupTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// connectSlotProxy initializes one upstream session against a proxy handler
// and proves the session is live with one tool call.
func connectSlotProxy(t *testing.T, handler http.Handler, name string) *sdkmcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: name, Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": name}}); err != nil {
		t.Fatalf("CallTool after connect failed: %v", err)
	}
	return s
}

// reapDownstream removes the single tracked downstream session, mirroring a
// reaper take, and fails when the manager tracks a different count.
func reapDownstream(t *testing.T, mgr *session.Manager) {
	t.Helper()
	ids := mgr.Sessions()
	if len(ids) != 1 {
		t.Fatalf("manager sessions = %v, want exactly one", ids)
	}
	if err := mgr.RemoveSession(ids[0]); err != nil {
		t.Fatalf("RemoveSession (reap): %v", err)
	}
}

// assertGaugesBalanced proves both derived gauges returned to zero after a
// reap: sessions_active through the per-server owner, subprocesses_active
// through the manager's own count.
func assertGaugesBalanced(t *testing.T, dm *metrics.DaemonMetrics, mgr *session.Manager, when string) {
	t.Helper()
	snap := dm.Snapshot()
	if snap.SessionsActive != 0 {
		t.Errorf("%s: sessions_active = %d, want 0 after the reap", when, snap.SessionsActive)
	}
	if snap.SubprocessesActive != 0 {
		t.Errorf("%s: subprocesses_active = %d, want 0 after the reap", when, snap.SubprocessesActive)
	}
	if mgr.SessionCount() != 0 {
		t.Errorf("%s: manager session count = %d, want 0 after the reap", when, mgr.SessionCount())
	}
}

// TestSlotMemberEndpointReapBalancesSessionGauge proves a session created
// through a slot member's own endpoint is closed by the member manager's
// removal callback: the group proxy's observer must not displace it. With
// overwrite semantics the group callback searched only the group index,
// missed the member-endpoint session, and left sessions_active stranded at 1
// while subprocesses_active dropped to 0.
func TestSlotMemberEndpointReapBalancesSessionGauge(t *testing.T) {
	skipIfNoNodeDaemon(t)
	logger := slotGroupTestLogger()
	cfg := slotGroupEchoConfig()

	mgr := session.NewManager("slot-member-1", cfg, logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(
		func() int64 { return owner.Snapshot().ActiveSessions },
		func() int64 { return int64(mgr.SessionCount()) },
	)

	memberHandler := mcp.NewProxyHandler(mcp.ProxyConfig{
		ServerName:     "slot-member-1",
		SessionManager: mgr,
		Logger:         logger,
		Metrics:        owner,
	})
	selector := slots.NewMultiplexer("playwright", logger, []slots.Entry{
		{SlotName: "slot-member-1", Index: 1, Manager: mgr, Reporter: owner},
	})
	_ = mcp.NewProxyHandler(mcp.ProxyConfig{
		ServerName: "playwright",
		Selector:   selector,
		Logger:     logger,
	})

	connectSlotProxy(t, memberHandler, "member-endpoint-client")
	if got := dm.Snapshot().SessionsActive; got != 1 {
		t.Fatalf("after member-endpoint initialize: sessions_active = %d, want 1", got)
	}

	reapDownstream(t, mgr)
	assertGaugesBalanced(t, dm, mgr, "after member-endpoint reap")
}

// TestSlotGroupEndpointReapBalancesSessionGauge proves the working direction
// survives the observer refactor: a session created through the group
// endpoint is closed by the group proxy's observer registered on the member
// manager, with the member endpoint's callback a no-op for it.
func TestSlotGroupEndpointReapBalancesSessionGauge(t *testing.T) {
	skipIfNoNodeDaemon(t)
	logger := slotGroupTestLogger()
	cfg := slotGroupEchoConfig()

	mgr := session.NewManager("slot-member-1", cfg, logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(
		func() int64 { return owner.Snapshot().ActiveSessions },
		func() int64 { return int64(mgr.SessionCount()) },
	)

	// The member endpoint keeps its own primary removal callback registered
	// on the shared manager, as production wiring does.
	_ = mcp.NewProxyHandler(mcp.ProxyConfig{
		ServerName:     "slot-member-1",
		SessionManager: mgr,
		Logger:         logger,
		Metrics:        owner,
	})
	selector := slots.NewMultiplexer("playwright", logger, []slots.Entry{
		{SlotName: "slot-member-1", Index: 1, Manager: mgr, Reporter: owner},
	})
	groupHandler := mcp.NewProxyHandler(mcp.ProxyConfig{
		ServerName: "playwright",
		Selector:   selector,
		Logger:     logger,
	})

	connectSlotProxy(t, groupHandler, "group-endpoint-client")
	if got := dm.Snapshot().SessionsActive; got != 1 {
		t.Fatalf("after group-endpoint initialize: sessions_active = %d, want 1", got)
	}

	reapDownstream(t, mgr)
	assertGaugesBalanced(t, dm, mgr, "after group-endpoint reap")
}

// TestSlotReplacementMemberManagerGetsRemovalHook proves the group proxy's
// removal observer reaches replacement member managers: after ReplaceEntries
// swaps in a restarted member's manager, a reap of a group-endpoint session
// on that manager still closes the proxy session and balances the gauges.
func TestSlotReplacementMemberManagerGetsRemovalHook(t *testing.T) {
	skipIfNoNodeDaemon(t)
	logger := slotGroupTestLogger()
	cfg := slotGroupEchoConfig()

	oldMgr := session.NewManager("slot-member-1", cfg, logger)
	defer oldMgr.CloseAll()
	restartedMgr := session.NewManager("slot-member-1", cfg, logger)
	defer restartedMgr.CloseAll()
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(
		func() int64 { return owner.Snapshot().ActiveSessions },
		func() int64 { return int64(restartedMgr.SessionCount()) },
	)

	// Pre-restart wiring: one group handler over the original member manager.
	selector := slots.NewMultiplexer("playwright", logger, []slots.Entry{
		{SlotName: "slot-member-1", Index: 1, Manager: oldMgr, Reporter: owner},
	})
	groupHandler := mcp.NewProxyHandler(mcp.ProxyConfig{
		ServerName: "playwright",
		Selector:   selector,
		Logger:     logger,
	})

	// Restart: the member endpoint re-registers on a fresh manager, then the
	// group replaces its entries with the restarted member's manager.
	_ = mcp.NewProxyHandler(mcp.ProxyConfig{
		ServerName:     "slot-member-1",
		SessionManager: restartedMgr,
		Logger:         logger,
		Metrics:        owner,
	})
	selector.ReplaceEntries([]slots.Entry{
		{SlotName: "slot-member-1", Index: 1, Manager: restartedMgr, Reporter: owner},
	})

	connectSlotProxy(t, groupHandler, "restarted-member-client")
	if got := dm.Snapshot().SessionsActive; got != 1 {
		t.Fatalf("after initialize on replacement member: sessions_active = %d, want 1", got)
	}

	reapDownstream(t, restartedMgr)
	assertGaugesBalanced(t, dm, restartedMgr, "after replacement-member reap")
}
