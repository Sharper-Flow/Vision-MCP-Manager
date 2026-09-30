package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	visionmcp "github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
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
