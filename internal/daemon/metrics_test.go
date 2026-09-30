package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	visionmcp "github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newMetricsTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")
	initial := []byte("servers:\n  echo:\n    port: 6276\n    command: echo\n    autostart: false\n")
	if err := os.WriteFile(configPath, initial, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(Config{ConfigPath: configPath, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// TestNewWiresDaemonMetricsIntoAdminServer proves the daemon hands one
// DaemonMetrics to the admin server so vision_metrics and GET /metrics stop
// reading a nil pointer.
func TestNewWiresDaemonMetricsIntoAdminServer(t *testing.T) {
	d := newMetricsTestDaemon(t)

	if d.adminServer == nil {
		t.Fatal("admin server is nil")
	}
	if d.adminServer.Metrics == nil {
		t.Fatal("daemon.New left admin.Config.Metrics unset; vision_metrics and GET /metrics read nil")
	}

	// Counters recorded on the daemon-wide metrics must be visible through the
	// exact instance the admin server holds.
	d.adminServer.Metrics.IncToolCalls()
	d.adminServer.Metrics.IncErrors()
	snap := d.adminServer.Metrics.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("ToolCallsTotal = %d, want 1 through the wired admin metrics", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("ErrorsTotal = %d, want 1 through the wired admin metrics", snap.ErrorsTotal)
	}
}

// TestDerivedActiveSessionsSumPerServerMetrics proves sessions_active is
// derived at read time by summing the per-server ServerMetrics owners.
func TestDerivedActiveSessionsSumPerServerMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		logger:        logger,
		portManager:   visionmcp.NewPortManager(logger),
		supervisor:    supervisor.New(config.SupervisionConfig{}, logger),
		serverMetrics: make(map[string]*metrics.ServerMetrics),
	}
	defer d.portManager.Close()

	first := metrics.NewServerMetrics()
	first.IncActiveSessions()
	first.IncActiveSessions()
	second := metrics.NewServerMetrics()
	second.IncActiveSessions()
	d.serverMetrics["first"] = first
	d.serverMetrics["second"] = second

	if got := d.activeSessions(); got != 3 {
		t.Fatalf("activeSessions() = %d, want 3 (2 + 1 from per-server owners)", got)
	}

	// Read-time derivation: the value moves when an owner moves.
	first.DecActiveSessions()
	first.DecActiveSessions()
	if got := d.activeSessions(); got != 1 {
		t.Fatalf("activeSessions() after owner decrement = %d, want 1", got)
	}
}

// TestDerivedActiveSubprocessesCountsLiveOwners proves subprocesses_active is
// derived at read time: live supervised processes count, stopped ones do not,
// and session managers contribute only live sessions.
func TestDerivedActiveSubprocessesCountsLiveOwners(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(config.SupervisionConfig{}, logger)

	sleepCfg := &config.ServerConfig{Command: "sleep", Args: []string{"30"}}
	sleepCfg.ApplyDefaults()
	proc, err := sup.AddServer("sleeper", sleepCfg)
	if err != nil {
		t.Fatalf("supervisor.AddServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = proc.Serve(ctx)
	}()
	stopSleeper := func() {
		cancel()
		<-serveDone
	}
	defer stopSleeper()
	waitForProcessState(t, proc, supervisor.StateRunning)

	d := &Daemon{
		logger:        logger,
		portManager:   visionmcp.NewPortManager(logger),
		supervisor:    sup,
		serverMetrics: make(map[string]*metrics.ServerMetrics),
	}
	defer d.portManager.Close()

	// A stdio session manager with no live sessions contributes zero.
	managerCfg := &config.ServerConfig{Command: "sleep", Args: []string{"30"}}
	managerCfg.ApplyDefaults()
	idleManager := session.NewManager("stdio-idle", managerCfg, logger)
	if err := d.portManager.AddStreamable("stdio-idle", 0, http.NotFoundHandler(), idleManager); err != nil {
		t.Fatalf("AddStreamable(idle manager): %v", err)
	}

	if got := d.activeSubprocesses(); got != 1 {
		t.Fatalf("activeSubprocesses() = %d, want 1 (the live supervised process)", got)
	}

	// A stopped process stops counting.
	stopSleeper()
	waitForProcessState(t, proc, supervisor.StateStopped)
	if got := d.activeSubprocesses(); got != 0 {
		t.Fatalf("activeSubprocesses() after stop = %d, want 0", got)
	}
}

func waitForProcessState(t *testing.T, proc *supervisor.ManagedProcess, want supervisor.ServiceState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for proc.State() != want {
		if time.Now().After(deadline) {
			t.Fatalf("process state = %q, want %q within deadline", proc.State(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func skipIfNoNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is unavailable; skipping live stdio fixture test")
	}
}

// TestExternalHTTPDoesNotCountAsSubprocess proves externally owned http/sse
// services (supervisor state running, no child process, PID 0) do not count
// as daemon subprocesses.
func TestExternalHTTPDoesNotCountAsSubprocess(t *testing.T) {
	for _, transport := range []config.TransportType{config.TransportHTTP, config.TransportSSE} {
		t.Run(string(transport), func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			sup := supervisor.New(config.SupervisionConfig{}, logger)
			cfg := &config.ServerConfig{Transport: transport, URL: "http://127.0.0.1:1/mcp"}
			cfg.ApplyDefaults()
			proc, err := sup.AddServer("external", cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = proc.Serve(ctx)
			}()
			defer func() { cancel(); <-done }()
			waitForProcessState(t, proc, supervisor.StateRunning)
			if proc.PID() != 0 {
				t.Fatalf("external transport unexpectedly has PID %d", proc.PID())
			}
			d := &Daemon{portManager: visionmcp.NewPortManager(logger), supervisor: sup}
			defer d.portManager.Close()
			if got := d.activeSubprocesses(); got != 0 {
				t.Fatalf("external %s with no child PID: subprocesses_active=%d, want 0", transport, got)
			}
		})
	}
}

// TestSlotGroupSessionAppearsInDaemonGauge proves a session created through
// the slot-group endpoint feeds its member server's metrics owner, so the
// read-time sessions_active derivation counts it.
func TestSlotGroupSessionAppearsInDaemonGauge(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fixture := `const rl=require('readline').createInterface({input:process.stdin,terminal:false});
rl.on('line',line=>{const m=JSON.parse(line); if(!('id' in m))return;
let result={};
if(m.method==='initialize')result={protocolVersion:'2025-03-26',capabilities:{tools:{}},serverInfo:{name:'fixture',version:'1'}};
if(m.method==='tools/list')result={tools:[{name:'echo',inputSchema:{type:'object'}}]};
if(m.method==='tools/call')result={content:[{type:'text',text:'ok'}]};
process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result})+'\n');});`
	cfg := &config.ServerConfig{Command: "node", Args: []string{"-e", fixture}, Stateful: true, SlotGroup: "group", SlotIndex: 1}
	cfg.ApplyDefaults()
	d := &Daemon{
		cfg: &config.Config{
			Servers:    map[string]*config.ServerConfig{"slot": cfg},
			SlotGroups: map[string]*config.SlotGroupConfig{"group": {Count: 1, GroupPort: 0}},
		},
		ctx:           ctx,
		logger:        logger,
		portManager:   visionmcp.NewPortManager(logger),
		supervisor:    supervisor.New(config.SupervisionConfig{}, logger),
		serverMetrics: make(map[string]*metrics.ServerMetrics),
		daemonMetrics: metrics.NewDaemonMetrics(),
	}
	d.daemonMetrics.SetGaugeProviders(d.activeSessions, d.activeSubprocesses)
	defer d.portManager.Close()
	if err := d.setupProxyForServer(&server.ManagedServer{Name: "slot", Config: cfg, State: server.StateRunning}); err != nil {
		t.Fatal(err)
	}
	listener := d.portManager.Get("slot_group:group")
	if listener == nil {
		t.Fatal("group listener missing")
	}
	ts := httptest.NewServer(listener.MCPHandler)
	defer ts.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-group", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	mgr := d.portManager.Get("slot").SessionManager.(*session.Manager)
	if mgr.SessionCount() != 1 {
		t.Fatalf("member manager sessions=%d, want 1", mgr.SessionCount())
	}
	snap := d.daemonMetrics.Snapshot()
	if snap.ToolCallsTotal != 1 || snap.SubprocessesActive != 1 {
		t.Fatalf("unexpected fixture counters: %+v", snap)
	}
	if snap.SessionsActive != 1 {
		t.Fatalf("one active slot-group session: sessions_active=%d, want 1", snap.SessionsActive)
	}
}

// TestDaemonWiresMetricsIntoAdminVisionMetricsNonzero proves the daemon hands
// one DaemonMetrics to the admin server and that a production counting path
// (the managed HTTP gateway) drives vision_metrics' read to nonzero.
func TestDaemonWiresMetricsIntoAdminVisionMetricsNonzero(t *testing.T) {
	d := newMetricsTestDaemon(t)
	if d.adminServer.Metrics != d.daemonMetrics {
		t.Fatal("daemon.New did not hand the admin server the daemon-wide metrics instance")
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", "wired-metrics-session")
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL + "/mcp")
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	gateway, err := visionmcp.NewManagedHTTPGateway(visionmcp.ManagedHTTPGatewayConfig{
		Target:        target,
		MaxSessions:   4,
		IdleTimeout:   30 * time.Minute,
		Transport:     backend.Client().Transport,
		DaemonMetrics: d.daemonMetrics,
	})
	if err != nil {
		t.Fatalf("NewManagedHTTPGateway: %v", err)
	}

	initReq := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"wired-metrics","version":"1"}}}`))
	initResp := httptest.NewRecorder()
	gateway.ServeHTTP(initResp, initReq)
	if initResp.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", initResp.Code, initResp.Body.String())
	}
	sessionID := initResp.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}

	callReq := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	callReq.Header.Set("Mcp-Session-Id", sessionID)
	callResp := httptest.NewRecorder()
	gateway.ServeHTTP(callResp, callReq)
	if callResp.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d, body = %s", callResp.Code, callResp.Body.String())
	}

	snap := d.daemonMetrics.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("daemon metrics tool_calls_total = %d, want 1 after one forwarded tools/call", snap.ToolCallsTotal)
	}
	// vision_metrics and GET /metrics read exactly this instance.
	if got := d.adminServer.Metrics.Snapshot().ToolCallsTotal; got != 1 {
		t.Errorf("admin metrics (vision_metrics read path) tool_calls_total = %d, want 1", got)
	}
}

// TestSessionsAndSubprocessesActiveDerivedFromOwners proves the daemon's
// gauge providers derive sessions_active from per-server ServerMetrics
// owners and subprocesses_active from live child owners only, moving at read
// time as the owners move.
func TestSessionsAndSubprocessesActiveDerivedFromOwners(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(config.SupervisionConfig{}, logger)

	d := &Daemon{
		logger:        logger,
		portManager:   visionmcp.NewPortManager(logger),
		supervisor:    sup,
		serverMetrics: make(map[string]*metrics.ServerMetrics),
		daemonMetrics: metrics.NewDaemonMetrics(),
	}
	defer d.portManager.Close()
	d.daemonMetrics.SetGaugeProviders(d.activeSessions, d.activeSubprocesses)

	owner := metrics.NewServerMetrics()
	owner.IncActiveSessions()
	d.serverMetricsMu.Lock()
	d.serverMetrics["stdio-owner"] = owner
	d.serverMetricsMu.Unlock()

	sleepCfg := &config.ServerConfig{Command: "sleep", Args: []string{"30"}}
	sleepCfg.ApplyDefaults()
	sleeper, err := sup.AddServer("sleeper", sleepCfg)
	if err != nil {
		t.Fatalf("supervisor.AddServer(sleeper): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = sleeper.Serve(ctx)
	}()
	stopSleeper := func() {
		cancel()
		<-serveDone
	}
	defer stopSleeper()
	waitForProcessState(t, sleeper, supervisor.StateRunning)

	extCfg := &config.ServerConfig{Transport: config.TransportHTTP, URL: "http://127.0.0.1:1/mcp"}
	extCfg.ApplyDefaults()
	external, err := sup.AddServer("external", extCfg)
	if err != nil {
		t.Fatalf("supervisor.AddServer(external): %v", err)
	}
	extCtx, extCancel := context.WithCancel(context.Background())
	extDone := make(chan struct{})
	go func() {
		defer close(extDone)
		_ = external.Serve(extCtx)
	}()
	stopExternal := func() {
		extCancel()
		<-extDone
	}
	defer stopExternal()
	waitForProcessState(t, external, supervisor.StateRunning)

	snap := d.daemonMetrics.Snapshot()
	if snap.SessionsActive != 1 {
		t.Errorf("sessions_active = %d, want 1 from the per-server owner", snap.SessionsActive)
	}
	if snap.SubprocessesActive != 1 {
		t.Errorf("subprocesses_active = %d, want 1 (the live child; the externally owned http service owns no process)", snap.SubprocessesActive)
	}

	// Read-time derivation: the gauges move when their owners move.
	owner.DecActiveSessions()
	stopSleeper()
	waitForProcessState(t, sleeper, supervisor.StateStopped)
	snap = d.daemonMetrics.Snapshot()
	if snap.SessionsActive != 0 {
		t.Errorf("sessions_active after owner decrement = %d, want 0", snap.SessionsActive)
	}
	if snap.SubprocessesActive != 0 {
		t.Errorf("subprocesses_active after child exit = %d, want 0", snap.SubprocessesActive)
	}
}
