package supervisor

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

func TestNew(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	sup := New(cfg, nil)
	if sup == nil {
		t.Fatal("New() returned nil")
	}
	if sup.Supervisor == nil {
		t.Error("Supervisor field is nil")
	}
	if sup.services == nil {
		t.Error("services map is nil")
	}
}

func TestSupervisor_AddServer(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	sup := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
		Args:    []string{"hello"},
	}

	proc, err := sup.AddServer("test", serverCfg)
	if err != nil {
		t.Fatalf("AddServer() error: %v", err)
	}
	if proc == nil {
		t.Fatal("AddServer() returned nil process")
	}
	if proc.Name() != "test" {
		t.Errorf("Process name = %q, want %q", proc.Name(), "test")
	}

	// Adding same server again should fail
	_, err = sup.AddServer("test", serverCfg)
	if err == nil {
		t.Error("AddServer() should fail for duplicate name")
	}
}

func TestSupervisor_RemoveServer(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	sup := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
		Args:    []string{"hello"},
	}

	sup.AddServer("test", serverCfg)

	// Remove should work
	if err := sup.RemoveServer("test"); err != nil {
		t.Fatalf("RemoveServer() error: %v", err)
	}

	// Get should return nil
	if sup.GetServer("test") != nil {
		t.Error("GetServer() should return nil after removal")
	}

	// Remove non-existent should fail
	if err := sup.RemoveServer("nonexistent"); err == nil {
		t.Error("RemoveServer() should fail for non-existent server")
	}
}

func TestSupervisor_GetServer(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	sup := New(cfg, nil)

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
	}

	sup.AddServer("test", serverCfg)

	proc := sup.GetServer("test")
	if proc == nil {
		t.Fatal("GetServer() returned nil")
	}
	if proc.Name() != "test" {
		t.Errorf("Name = %q, want %q", proc.Name(), "test")
	}

	// Non-existent returns nil
	if sup.GetServer("nonexistent") != nil {
		t.Error("GetServer() should return nil for non-existent")
	}
}

func TestSupervisor_Servers(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	sup := New(cfg, nil)

	// Empty initially
	if len(sup.Servers()) != 0 {
		t.Errorf("Servers() should be empty initially, got %d", len(sup.Servers()))
	}

	// Add some servers
	for i, name := range []string{"alpha", "beta", "gamma"} {
		serverCfg := &config.ServerConfig{
			Port:    6276 + i,
			Command: "echo",
		}
		sup.AddServer(name, serverCfg)
	}

	servers := sup.Servers()
	if len(servers) != 3 {
		t.Errorf("Servers() = %d, want 3", len(servers))
	}
}

func TestManagedProcess_InitialState(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
		Args:    []string{"hello"},
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.Default())

	if proc.State() != StateStopped {
		t.Errorf("Initial state = %q, want %q", proc.State(), StateStopped)
	}
	if proc.PID() != 0 {
		t.Errorf("Initial PID = %d, want 0", proc.PID())
	}
	if proc.RestartCount() != 0 {
		t.Errorf("Initial RestartCount = %d, want 0", proc.RestartCount())
	}
	if proc.Uptime() != 0 {
		t.Errorf("Initial Uptime = %v, want 0", proc.Uptime())
	}
	if proc.LastError() != nil {
		t.Errorf("Initial LastError = %v, want nil", proc.LastError())
	}
}

func TestManagedProcess_Status(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
		Args:    []string{"hello"},
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.Default())

	status := proc.Status()
	if status.Name != "test" {
		t.Errorf("Status.Name = %q, want %q", status.Name, "test")
	}
	if status.State != StateStopped {
		t.Errorf("Status.State = %q, want %q", status.State, StateStopped)
	}
	if status.Port != 6276 {
		t.Errorf("Status.Port = %d, want %d", status.Port, 6276)
	}
}

func TestManagedProcess_Serve_Echo(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	// Use "true" command which exits immediately with success
	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "true",
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Serve should return quickly since "true" exits immediately
	err := proc.Serve(ctx)

	// "true" exits with code 0, so no error
	if err != nil {
		t.Errorf("Serve() returned error: %v", err)
	}

	// State should be stopped (exited normally)
	if proc.State() != StateStopped {
		t.Errorf("State = %q, want %q", proc.State(), StateStopped)
	}

	// Should have been restarted once (exited)
	if proc.RestartCount() != 1 {
		t.Errorf("RestartCount = %d, want 1", proc.RestartCount())
	}
}

func TestManagedProcess_Serve_Failure(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	// Use "false" command which exits immediately with failure
	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "false",
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := proc.Serve(ctx)

	// "false" exits with code 1, so should have error
	if err == nil {
		t.Error("Serve() should return error for failed process")
	}

	// State should be crashed
	if proc.State() != StateCrashed {
		t.Errorf("State = %q, want %q", proc.State(), StateCrashed)
	}

	// LastError should be set
	if proc.LastError() == nil {
		t.Error("LastError should be set")
	}
}

func TestManagedProcess_Serve_NotFound(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "this-command-does-not-exist-12345",
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := proc.Serve(ctx)

	if err == nil {
		t.Error("Serve() should return error for non-existent command")
	}

	if proc.State() != StateCrashed {
		t.Errorf("State = %q, want %q", proc.State(), StateCrashed)
	}
}

func TestManagedProcess_Serve_Cancellation(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	cfg.ShutdownTimeout = config.Duration(2 * time.Second)

	// Use "sleep" to have a process that runs long enough to cancel
	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "sleep",
		Args:    []string{"60"},
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := proc.Serve(ctx)
	duration := time.Since(start)

	// Should return context.DeadlineExceeded
	if err != context.DeadlineExceeded {
		t.Errorf("Serve() error = %v, want context.DeadlineExceeded", err)
	}

	// Should not have taken 60 seconds
	if duration > 5*time.Second {
		t.Errorf("Serve() took %v, should have been cancelled much sooner", duration)
	}

	// State should be stopped (graceful shutdown)
	if proc.State() != StateStopped {
		t.Errorf("State = %q, want %q", proc.State(), StateStopped)
	}
}

func TestManagedProcess_BuildEnv(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	serverCfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
		Env: map[string]string{
			"CUSTOM_VAR": "custom_value",
			"API_KEY":    "secret",
		},
	}

	proc := NewManagedProcess("test", serverCfg, cfg, slog.Default())

	env := proc.buildEnv()

	// Should contain custom vars
	hasCustom := false
	hasAPIKey := false
	for _, e := range env {
		if e == "CUSTOM_VAR=custom_value" {
			hasCustom = true
		}
		if e == "API_KEY=secret" {
			hasAPIKey = true
		}
	}

	if !hasCustom {
		t.Error("buildEnv() missing CUSTOM_VAR")
	}
	if !hasAPIKey {
		t.Error("buildEnv() missing API_KEY")
	}

	// Should also include system environment
	if len(env) < 2 {
		t.Error("buildEnv() should include system environment")
	}
}

func TestNew_FailureThresholdSet(t *testing.T) {
	// Verify that the suture spec has explicit failure thresholds configured.
	// Without these, suture defaults to FailureThreshold=5 which is reasonable,
	// but we set them explicitly for operational control and to prevent one
	// crashing service from causing global backoff.
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()

	sup := New(cfg, nil)

	// The supervisor should be created successfully and the spec should have
	// been applied. We verify this indirectly: a service that crashes
	// repeatedly should NOT prevent other services from being restarted
	// promptly.
	//
	// We test this by running two services: one that crashes immediately
	// ("false") and one that runs stably ("sleep 60"). The stable service
	// should remain unaffected by the crashing service's restart loop.

	crashingCfg := &config.ServerConfig{
		Port:    6276,
		Command: "false",
	}

	stableCfg := &config.ServerConfig{
		Port:    6277,
		Command: "sleep",
		Args:    []string{"60"},
	}

	crashing, err := sup.AddServer("crasher", crashingCfg)
	if err != nil {
		t.Fatalf("AddServer crasher: %v", err)
	}
	stable, err := sup.AddServer("stable", stableCfg)
	if err != nil {
		t.Fatalf("AddServer stable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start the supervisor
	go sup.Serve(ctx)

	// Wait a bit for the services to start and the crasher to restart a few times
	time.Sleep(2 * time.Second)

	// The stable service should still be running despite the crasher failing
	stableState := stable.State()
	if stableState != StateRunning && stableState != StateStarting {
		t.Errorf("stable service state = %q, want running or starting (crasher should not affect it)", stableState)
	}

	// The crashing service should have restarted at least once
	crashRestarts := crashing.RestartCount()
	if crashRestarts < 1 {
		t.Errorf("crasher restarts = %d, want at least 1", crashRestarts)
	}

	// Context cancel will shut down the supervisor (suture uses Serve(ctx))
}

func TestServiceState_Constants(t *testing.T) {
	// Verify state constants are distinct
	states := []ServiceState{StateStopped, StateStarting, StateRunning, StateCrashed, StateFailed}
	seen := make(map[ServiceState]bool)

	for _, s := range states {
		if seen[s] {
			t.Errorf("Duplicate state: %q", s)
		}
		seen[s] = true
	}
}
