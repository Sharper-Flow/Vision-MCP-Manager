package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
	"github.com/thejerf/suture/v4"
)

func TestEventHookConcurrentTerminate(t *testing.T) {
	hook := eventHook(slog.New(slog.NewTextHandler(io.Discard, nil)))
	const calls = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			<-start
			hook(suture.EventServiceTerminate{
				ServiceName: "concurrent",
				Err:         errors.New("exit status 1"),
			})
		}()
	}
	close(start)
	wg.Wait()
}

type fakeLeaseStore struct {
	mu            sync.Mutex
	recordStarted chan struct{}
	recordRelease chan struct{}
	recordErr     error
	lease         ownership.Lease
	records       int
	releases      []uint64
}

func (s *fakeLeaseStore) Record(_ string, lease ownership.Lease) error {
	s.mu.Lock()
	s.records++
	s.lease = lease
	started, release, err := s.recordStarted, s.recordRelease, s.recordErr
	s.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return err
}
func (s *fakeLeaseStore) Read(string) (ownership.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease, nil
}
func (s *fakeLeaseStore) ReleaseGeneration(_ string, generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, generation)
	return nil
}

type fakeProcReader struct {
	members []int
	mu      sync.Mutex
}

func (f *fakeProcReader) ReadStat(pid int) (ownership.ProcStat, error) {
	return ownership.ProcStat{PID: pid, PGRP: pid, StartTime: 11}, nil
}
func (f *fakeProcReader) ReadEnviron(int) ([]byte, error) {
	return []byte(ownership.OwnerTokenEnvKey + "=token"), nil
}
func (f *fakeProcReader) ReadExe(int) (string, error) { return "/bin/sh", nil }
func (f *fakeProcReader) ReadBootID() (string, error) { return "boot", nil }
func (f *fakeProcReader) ListMembers(int) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.members...), nil
}
func (f *fakeProcReader) setMembers(m []int) {
	f.mu.Lock()
	f.members = append([]int(nil), m...)
	f.mu.Unlock()
}

type fakeSignaler struct {
	mu    sync.Mutex
	pgids []int
}

func (f *fakeSignaler) Supported() bool { return true }
func (f *fakeSignaler) GroupSignal(pgid int, _ os.Signal) error {
	f.mu.Lock()
	f.pgids = append(f.pgids, pgid)
	f.mu.Unlock()
	return nil
}

func managedFixture(t *testing.T, store ownership.LeaseStore, reader ownership.ProcReader, options ...ProcessOption) *ManagedProcess {
	t.Helper()
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	cfg.ShutdownTimeout = config.Duration(40 * time.Millisecond)
	serverCfg := &config.ServerConfig{Command: "/bin/sh", Transport: config.TransportManagedHTTP, Args: []string{"-c", "sleep 1"}}
	return NewManagedProcessWithOwnership("fixture", serverCfg, cfg, slog.Default(), store, "daemon", append(options, WithProcReader(reader))...)
}

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

	// RestartCount tracks automatic restart attempts, not child exits.
	if proc.RestartCount() != 0 {
		t.Errorf("RestartCount = %d, want 0", proc.RestartCount())
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

func TestManagedProcessTokenGeneratorFailureBeforeStart(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	marker := filepath.Join(t.TempDir(), "spawned")
	serverCfg := &config.ServerConfig{Command: "sh", Transport: config.TransportManagedHTTP, Args: []string{"-c", "touch " + marker}}
	p := NewManagedProcessWithOwnership("token-failure", serverCfg, cfg, slog.Default(), nil, "daemon", WithTokenGenerator(func() (string, error) { return "", errors.New("entropy unavailable") }))
	if err := p.Serve(context.Background()); err == nil {
		t.Fatal("Serve succeeded")
	}
	if p.State() == StateRunning {
		t.Fatal("process published running")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spawn marker exists or cannot be checked: %v", err)
	}
}

func TestManagedProcessRecordBlocksRunningPublication(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	store := &fakeLeaseStore{recordStarted: started, recordRelease: release}
	p := managedFixture(t, store, &fakeProcReader{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- p.Serve(ctx) }()
	<-started
	state, generation := p.LifecycleSnapshot()
	if state != StateStarting || generation != 0 {
		t.Fatalf("published before record: %s/%d", state, generation)
	}
	close(release)
	deadline := time.After(time.Second)
	for {
		state, generation = p.LifecycleSnapshot()
		if state == StateRunning {
			if generation != 1 {
				t.Fatalf("generation=%d", generation)
			}
			p.mu.RLock()
			token := p.ownerToken
			p.mu.RUnlock()
			if token != "" {
				t.Fatal("raw ownership token retained after lease record")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("process did not run")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}

func TestManagedProcessRecordFailureKillsGroupAndNeverRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	store := &fakeLeaseStore{recordErr: errors.New("record failed")}
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	cfg.ShutdownTimeout = config.Duration(100 * time.Millisecond)
	serverCfg := &config.ServerConfig{Command: "/bin/sh", Transport: config.TransportManagedHTTP, Args: []string{"-c", "touch " + marker + "; sleep 1"}}
	p := NewManagedProcessWithOwnership("failure", serverCfg, cfg, slog.Default(), store, "daemon", WithProcReader(&fakeProcReader{}))
	err := p.Serve(context.Background())
	if err == nil || strings.Contains(err.Error(), ownership.OwnerTokenEnvKey) {
		t.Fatalf("bad error: %v", err)
	}
	if p.State() != StateCrashed {
		t.Fatalf("state=%s", p.State())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker exists after record failure cleanup")
	}
	p.mu.RLock()
	active := p.leaseActive
	processState := p.cmd.ProcessState
	p.mu.RUnlock()
	if active || processState == nil {
		t.Fatalf("cleanup state active=%v processState=%v", active, processState)
	}
}

func TestManagedProcessLeaseRetainedWhileDescendantLives(t *testing.T) {
	store := &fakeLeaseStore{}
	reader := &fakeProcReader{}
	p := managedFixture(t, store, reader)
	reader.setMembers([]int{99})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx) }()
	select {
	case <-p.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("process did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish")
	}
	store.mu.Lock()
	releases := len(store.releases)
	store.mu.Unlock()
	if releases != 0 {
		t.Fatal("lease released with descendant")
	}
}

func TestManagedProcessLeaseReleasedOnlyWhenGroupEmpty(t *testing.T) {
	store := &fakeLeaseStore{}
	reader := &fakeProcReader{}
	p := managedFixture(t, store, reader)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx) }()
	select {
	case <-p.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("process did not start")
	}
	reader.setMembers(nil)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.releases) != 1 || store.releases[0] != 1 {
		t.Fatalf("releases=%v", store.releases)
	}
}

func TestManagedProcessUnsupportedIdentityFailsClosed(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	cfg.ShutdownTimeout = config.Duration(100 * time.Millisecond)
	p := NewManagedProcessWithOwnership("unsupported", &config.ServerConfig{Command: "/bin/sh", Transport: config.TransportManagedHTTP, Args: []string{"-c", "sleep 1"}}, cfg, slog.Default(), &fakeLeaseStore{}, "daemon", WithProcReader(&unsupportedProcReader{}))
	if err := p.Serve(context.Background()); err == nil {
		t.Fatal("unsupported identity accepted")
	}
	if p.State() == StateRunning {
		t.Fatal("unsupported backend published running")
	}
}

type unsupportedProcReader struct{}

func (*unsupportedProcReader) ReadStat(int) (ownership.ProcStat, error) {
	return ownership.ProcStat{}, errors.New("unsupported")
}
func (*unsupportedProcReader) ReadEnviron(int) ([]byte, error) { return nil, errors.New("unsupported") }
func (*unsupportedProcReader) ReadExe(int) (string, error)     { return "", errors.New("unsupported") }
func (*unsupportedProcReader) ReadBootID() (string, error)     { return "", errors.New("unsupported") }
func (*unsupportedProcReader) ListMembers(int) ([]int, error)  { return nil, errors.New("unsupported") }

func TestManagedProcessConfigOwnerTokenOverride(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	p := NewManagedProcessWithOwnership("token", &config.ServerConfig{Transport: config.TransportManagedHTTP, Env: map[string]string{ownership.OwnerTokenEnvKey: "spoof"}}, cfg, slog.Default(), nil, "daemon", WithTokenGenerator(func() (string, error) { return "generated", nil }))
	if err := p.prepareOwnerToken(); err != nil {
		t.Fatal(err)
	}
	env := p.buildEnv()
	count := 0
	for _, item := range env {
		if strings.HasPrefix(item, ownership.OwnerTokenEnvKey+"=") {
			count++
			if item != ownership.OwnerTokenEnvKey+"=generated" {
				t.Fatal(item)
			}
		}
	}
	if count != 1 {
		t.Fatalf("token count=%d", count)
	}
}

func TestManagedProcessSignalsRecordedPGID(t *testing.T) {
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	signaler := &fakeSignaler{}
	p := NewManagedProcessWithOwnership("pgid", &config.ServerConfig{Transport: config.TransportManagedHTTP}, cfg, slog.Default(), nil, "daemon", WithSignaler(signaler))
	p.cmd = &exec.Cmd{Process: &os.Process{Pid: 41}}
	p.lease = ownership.Lease{LeaderPGID: 99}
	p.leaseActive = true
	if err := p.terminate(); err != nil {
		t.Fatal(err)
	}
	if err := p.forceKill(); err != nil {
		t.Fatal(err)
	}
	signaler.mu.Lock()
	defer signaler.mu.Unlock()
	if len(signaler.pgids) != 2 || signaler.pgids[0] != 99 || signaler.pgids[1] != 99 {
		t.Fatalf("pgids=%v", signaler.pgids)
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
