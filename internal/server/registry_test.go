package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

func newTestRegistry() (*Registry, *supervisor.Supervisor) {
	supCfg := config.SupervisionConfig{}
	supCfg.ApplyDefaults()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sup := supervisor.New(supCfg, logger)
	reg := NewRegistry(sup, logger)

	return reg, sup
}

func TestRegistry_Add(t *testing.T) {
	reg, _ := newTestRegistry()

	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
	}

	// Add should succeed
	if err := reg.Add("test", cfg); err != nil {
		t.Fatalf("Add() error: %v", err)
	}

	// Duplicate add should fail
	err := reg.Add("test", cfg)
	if err == nil {
		t.Error("Add() should fail for duplicate name")
	}
}

func TestRegistryStartupGuardRetriesBlockedStart(t *testing.T) {
	reg, _ := newTestRegistry()
	cfg := &config.ServerConfig{Port: 6276, Command: "echo", Transport: config.TransportManagedHTTP}
	if err := reg.Add("guarded", cfg); err != nil {
		t.Fatal(err)
	}
	reg.BlockStartup("guarded", errors.New("conflict"))
	calls := 0
	reg.SetStartupGuard(func(string) error { calls++; return nil })
	if err := reg.Start("guarded"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if calls != 1 {
		t.Fatalf("guard calls=%d", calls)
	}
}

func TestRegistryStartupGuardFailureRetainsBlock(t *testing.T) {
	reg, _ := newTestRegistry()
	cfg := &config.ServerConfig{Port: 6276, Command: "echo", Transport: config.TransportManagedHTTP}
	if err := reg.Add("blocked", cfg); err != nil {
		t.Fatal(err)
	}
	reg.BlockStartup("blocked", errors.New("conflict"))
	calls := 0
	reg.SetStartupGuard(func(string) error { calls++; return errors.New("still blocked") })
	if err := reg.Start("blocked"); err == nil {
		t.Fatal("Start succeeded")
	}
	if calls != 1 || reg.Get("blocked").State == StateRunning {
		t.Fatal("block not retained")
	}
}

func TestRegistryStartAllReportsBlockedRequiredAndAdvisory(t *testing.T) {
	reg, _ := newTestRegistry()
	for _, item := range []struct {
		name     string
		required bool
	}{{"advisory", false}, {"required", true}} {
		if err := reg.Add(item.name, &config.ServerConfig{Command: "echo", Transport: config.TransportManagedHTTP, Autostart: true, Required: item.required}); err != nil {
			t.Fatal(err)
		}
		reg.BlockStartup(item.name, errors.New("conflict"))
	}
	err := reg.StartAll(context.Background())
	var required *RequiredStartupError
	if !errors.As(err, &required) {
		t.Fatalf("error=%v", err)
	}
	if len(required.Failures) != 1 || required.Failures[0] != "required" {
		t.Fatalf("failures=%v", required.Failures)
	}
}

func TestRegistry_Remove(t *testing.T) {
	reg, _ := newTestRegistry()

	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
	}

	reg.Add("test", cfg)

	// Remove should succeed
	if err := reg.Remove("test"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}

	// Remove non-existent should fail
	err := reg.Remove("nonexistent")
	if err == nil {
		t.Error("Remove() should fail for non-existent server")
	}
}

func TestRegistry_Get(t *testing.T) {
	reg, _ := newTestRegistry()

	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
	}

	reg.Add("test", cfg)

	// Get existing
	srv := reg.Get("test")
	if srv == nil {
		t.Fatal("Get() returned nil for existing server")
	}
	if srv.Name != "test" {
		t.Errorf("Name = %q, want %q", srv.Name, "test")
	}
	if srv.State != StateStopped {
		t.Errorf("State = %q, want %q", srv.State, StateStopped)
	}

	// Get non-existent
	if reg.Get("nonexistent") != nil {
		t.Error("Get() should return nil for non-existent server")
	}
}

func TestRegistry_List(t *testing.T) {
	reg, _ := newTestRegistry()

	// Empty list
	if len(reg.List()) != 0 {
		t.Error("List() should be empty initially")
	}

	// Add some servers
	for i, name := range []string{"charlie", "alpha", "beta"} {
		cfg := &config.ServerConfig{
			Port:    6276 + i,
			Command: "echo",
		}
		reg.Add(name, cfg)
	}

	list := reg.List()
	if len(list) != 3 {
		t.Fatalf("List() length = %d, want 3", len(list))
	}

	// Should be sorted
	expected := []string{"alpha", "beta", "charlie"}
	for i, srv := range list {
		if srv.Name != expected[i] {
			t.Errorf("list[%d].Name = %q, want %q", i, srv.Name, expected[i])
		}
	}
}

func TestRegistry_Names(t *testing.T) {
	reg, _ := newTestRegistry()

	for i, name := range []string{"charlie", "alpha", "beta"} {
		cfg := &config.ServerConfig{
			Port:    6276 + i,
			Command: "echo",
		}
		reg.Add(name, cfg)
	}

	names := reg.Names()
	expected := []string{"alpha", "beta", "charlie"}

	if len(names) != len(expected) {
		t.Fatalf("Names() length = %d, want %d", len(names), len(expected))
	}

	for i, name := range names {
		if name != expected[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

func TestRegistry_Count(t *testing.T) {
	reg, _ := newTestRegistry()

	if reg.Count() != 0 {
		t.Errorf("Count() = %d, want 0", reg.Count())
	}

	cfg := &config.ServerConfig{Port: 6276, Command: "echo"}
	reg.Add("test", cfg)

	if reg.Count() != 1 {
		t.Errorf("Count() = %d, want 1", reg.Count())
	}
}

func TestRegistry_LoadFromConfig(t *testing.T) {
	reg, _ := newTestRegistry()

	cfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"server1": {Port: 6276, Command: "echo"},
			"server2": {Port: 6277, Command: "cat"},
		},
	}

	if err := reg.LoadFromConfig(cfg); err != nil {
		t.Fatalf("LoadFromConfig() error: %v", err)
	}

	if reg.Count() != 2 {
		t.Errorf("Count() = %d, want 2", reg.Count())
	}

	if reg.Get("server1") == nil {
		t.Error("server1 not loaded")
	}
	if reg.Get("server2") == nil {
		t.Error("server2 not loaded")
	}
}

func TestRegistry_Status(t *testing.T) {
	reg, _ := newTestRegistry()

	cfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"server1": {Port: 6276, Command: "echo", Autostart: true},
			"server2": {Port: 6277, Command: "cat", Autostart: false},
		},
	}
	reg.LoadFromConfig(cfg)

	status := reg.Status()

	if status.TotalServers != 2 {
		t.Errorf("TotalServers = %d, want 2", status.TotalServers)
	}
	if status.StoppedServers != 2 {
		t.Errorf("StoppedServers = %d, want 2", status.StoppedServers)
	}
	if len(status.Servers) != 2 {
		t.Errorf("len(Servers) = %d, want 2", len(status.Servers))
	}

	// Should be sorted
	if status.Servers[0].Name != "server1" {
		t.Errorf("First server = %q, want %q", status.Servers[0].Name, "server1")
	}
}

func TestManagedServer_State(t *testing.T) {
	srv := &ManagedServer{
		Name:  "test",
		State: StateStopped,
		Config: &config.ServerConfig{
			Port:    6276,
			Command: "echo",
		},
	}

	if srv.IsRunning() {
		t.Error("IsRunning() should be false when stopped")
	}
	if !srv.IsStopped() {
		t.Error("IsStopped() should be true when stopped")
	}

	srv.State = StateRunning
	if !srv.IsRunning() {
		t.Error("IsRunning() should be true when running")
	}
	if srv.IsStopped() {
		t.Error("IsStopped() should be false when running")
	}
}

func TestManagedServer_Uptime(t *testing.T) {
	srv := &ManagedServer{
		Name:  "test",
		State: StateStopped,
	}

	// Stopped server has 0 uptime
	if srv.Uptime() != 0 {
		t.Errorf("Uptime() = %v, want 0 for stopped server", srv.Uptime())
	}

	// Running server has positive uptime
	srv.State = StateRunning
	srv.StartedAt = time.Now().Add(-10 * time.Second)

	uptime := srv.Uptime()
	if uptime < 9*time.Second || uptime > 11*time.Second {
		t.Errorf("Uptime() = %v, want ~10s", uptime)
	}
}

func TestManagedServer_Status(t *testing.T) {
	srv := &ManagedServer{
		Name:  "test",
		State: StateRunning,
		Config: &config.ServerConfig{
			Port:      6276,
			Command:   "echo",
			Autostart: true,
		},
		StartedAt: time.Now(),
	}

	status := srv.Status()

	if status.Name != "test" {
		t.Errorf("Name = %q, want %q", status.Name, "test")
	}
	if status.State != StateRunning {
		t.Errorf("State = %q, want %q", status.State, StateRunning)
	}
	if status.Port != 6276 {
		t.Errorf("Port = %d, want 6276", status.Port)
	}
	if !status.Autostart {
		t.Error("Autostart should be true")
	}
}

func TestRegistry_StartStop(t *testing.T) {
	reg, sup := newTestRegistry()

	// Start the supervisor in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	// Use "true" command that exits immediately
	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "sleep",
		Args:    []string{"10"}, // Long enough to test
	}

	if err := reg.Add("test", cfg); err != nil {
		t.Fatalf("Add() error: %v", err)
	}

	// Start
	if err := reg.Start("test"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	srv := reg.Get("test")
	if srv.State != StateRunning {
		t.Errorf("State after Start = %q, want %q", srv.State, StateRunning)
	}

	// Start again should be no-op
	if err := reg.Start("test"); err != nil {
		t.Errorf("Start() again error: %v", err)
	}

	// Stop
	if err := reg.Stop("test"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	srv = reg.Get("test")
	if srv.State != StateStopped {
		t.Errorf("State after Stop = %q, want %q", srv.State, StateStopped)
	}

	// Stop again should be no-op
	if err := reg.Stop("test"); err != nil {
		t.Errorf("Stop() again error: %v", err)
	}
}

func TestRegistry_Restart(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "sleep",
		Args:    []string{"10"},
	}

	reg.Add("test", cfg)
	reg.Start("test")

	// Restart
	if err := reg.Restart("test"); err != nil {
		t.Fatalf("Restart() error: %v", err)
	}

	srv := reg.Get("test")
	if srv.State != StateRunning {
		t.Errorf("State after Restart = %q, want %q", srv.State, StateRunning)
	}

	// Cleanup
	reg.Stop("test")
}

func TestRegistry_StartNotFound(t *testing.T) {
	reg, _ := newTestRegistry()

	err := reg.Start("nonexistent")
	if err == nil {
		t.Error("Start() should fail for non-existent server")
	}
}

func TestRegistry_StopNotFound(t *testing.T) {
	reg, _ := newTestRegistry()

	err := reg.Stop("nonexistent")
	if err == nil {
		t.Error("Stop() should fail for non-existent server")
	}
}

func TestRegistry_RemoveRunning(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "sleep",
		Args:    []string{"10"},
	}

	reg.Add("test", cfg)
	reg.Start("test")

	// Should fail to remove running server
	err := reg.Remove("test")
	if err == nil {
		t.Error("Remove() should fail for running server")
	}

	// Stop then remove should work
	reg.Stop("test")
	if err := reg.Remove("test"); err != nil {
		t.Errorf("Remove() after Stop() error: %v", err)
	}
}

// TestRegistry_RemoveStopping verifies that Remove() is rejected while a server
// is mid-Stop() (StateStopping). This prevents a race where Remove() deletes
// the server entry while Stop() is still transitioning state.
func TestRegistry_RemoveStopping(t *testing.T) {
	reg, _ := newTestRegistry()

	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "echo",
	}

	reg.Add("test", cfg)

	// Manually set state to StateStopping (simulating mid-Stop)
	srv := reg.Get("test")
	srv.State = StateStopping

	err := reg.Remove("test")
	if err == nil {
		t.Error("Remove() should fail for server in StateStopping")
	}

	// After transition to Stopped, Remove should succeed
	srv.State = StateStopped
	if err := reg.Remove("test"); err != nil {
		t.Errorf("Remove() after StateStopped error: %v", err)
	}
}

func TestRegistry_StartAllStopAll(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	// Add servers, some with autostart
	cfg1 := &config.ServerConfig{
		Port:      6276,
		Command:   "sleep",
		Args:      []string{"10"},
		Autostart: true,
	}
	cfg2 := &config.ServerConfig{
		Port:      6277,
		Command:   "sleep",
		Args:      []string{"10"},
		Autostart: false,
	}
	cfg3 := &config.ServerConfig{
		Port:      6278,
		Command:   "sleep",
		Args:      []string{"10"},
		Autostart: true,
	}

	reg.Add("server1", cfg1)
	reg.Add("server2", cfg2)
	reg.Add("server3", cfg3)

	// StartAll should start only autostart servers
	if err := reg.StartAll(ctx); err != nil {
		t.Fatalf("StartAll() error: %v", err)
	}

	status := reg.Status()
	if status.RunningServers != 2 {
		t.Errorf("RunningServers = %d, want 2", status.RunningServers)
	}

	// server2 should still be stopped
	if reg.Get("server2").State != StateStopped {
		t.Error("server2 should still be stopped")
	}

	// StopAll should stop all running servers
	if err := reg.StopAll(ctx); err != nil {
		t.Fatalf("StopAll() error: %v", err)
	}

	status = reg.Status()
	if status.RunningServers != 0 {
		t.Errorf("RunningServers after StopAll = %d, want 0", status.RunningServers)
	}
}

func TestRegistry_StopAllStopsFailedOrCrashedProcess(t *testing.T) {
	for _, state := range []State{StateFailed, StateCrashed} {
		t.Run(string(state), func(t *testing.T) {
			reg, sup := newTestRegistry()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go sup.Serve(ctx)

			if err := reg.Add("owned-process", &config.ServerConfig{
				Port:      6276,
				Command:   "sleep",
				Args:      []string{"10"},
				Transport: config.TransportManagedHTTP,
				URL:       "http://127.0.0.1:16276/mcp",
				Autostart: true,
			}); err != nil {
				t.Fatal(err)
			}
			if err := reg.StartAll(ctx); err != nil {
				t.Fatal(err)
			}

			reg.mu.Lock()
			srv := reg.servers["owned-process"]
			if srv.Process == nil {
				reg.mu.Unlock()
				t.Fatal("started server has no owned process")
			}
			srv.State = state
			reg.mu.Unlock()

			if err := reg.StopAll(ctx); err != nil {
				t.Fatalf("StopAll() error: %v", err)
			}
			got := reg.Get("owned-process")
			if got.Process != nil || got.State != StateStopped {
				t.Fatalf("after StopAll: process=%v state=%s", got.Process, got.State)
			}
		})
	}
}

func TestRegistry_EventHandler_StartStop(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	// Track events
	events := make(chan ServerEvent, 10)
	reg.SetEventHandler(func(event ServerEvent) {
		events <- event
	})

	cfg := &config.ServerConfig{
		Port:    6279,
		Command: "sleep",
		Args:    []string{"10"},
	}

	reg.Add("test-events", cfg)

	// Start should fire EventServerStarted
	if err := reg.Start("test-events"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	select {
	case event := <-events:
		if event.Type != EventServerStarted {
			t.Errorf("Event type = %v, want EventServerStarted", event.Type)
		}
		if event.Name != "test-events" {
			t.Errorf("Event name = %q, want %q", event.Name, "test-events")
		}
		if event.Server == nil {
			t.Error("Event.Server is nil")
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for EventServerStarted")
	}

	// Stop should fire EventServerStopped
	if err := reg.Stop("test-events"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	select {
	case event := <-events:
		if event.Type != EventServerStopped {
			t.Errorf("Event type = %v, want EventServerStopped", event.Type)
		}
		if event.Name != "test-events" {
			t.Errorf("Event name = %q, want %q", event.Name, "test-events")
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for EventServerStopped")
	}
}

func TestRegistry_EventHandler_Nil(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	// No event handler set - should not panic
	cfg := &config.ServerConfig{
		Port:    6280,
		Command: "sleep",
		Args:    []string{"1"},
	}

	reg.Add("test-nil-handler", cfg)

	// These should not panic with nil handler
	if err := reg.Start("test-nil-handler"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := reg.Stop("test-nil-handler"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func TestRegistry_EventHandler_Replace(t *testing.T) {
	reg, _ := newTestRegistry()

	events1 := make(chan string, 1)
	events2 := make(chan string, 1)

	// Set first handler
	reg.SetEventHandler(func(event ServerEvent) {
		events1 <- "handler1"
	})

	// Replace with second handler
	reg.SetEventHandler(func(event ServerEvent) {
		events2 <- "handler2"
	})

	// Fire an event manually via fireEvent (testing internal behavior)
	reg.fireEvent(ServerEvent{Type: EventServerStarted, Name: "test"})

	// Only handler2 should receive the event
	select {
	case <-events1:
		t.Error("Handler1 should not receive events after replacement")
	case msg := <-events2:
		if msg != "handler2" {
			t.Errorf("Received %q, want %q", msg, "handler2")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Timeout waiting for handler2")
	}
}

// TestRegistry_StdioStart_SkipsSupervisor verifies that stdio transport servers
// skip supervisor registration and do not accumulate generations of subprocesses.
func TestRegistry_StdioStart_SkipsSupervisor(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	cfg := &config.ServerConfig{
		Port:    6281,
		Command: "sleep",
		Args:    []string{"60"},
	}

	reg.Add("stdio-skip", cfg)

	// Start stdio server
	if err := reg.Start("stdio-skip"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	srv := reg.Get("stdio-skip")
	if srv == nil {
		t.Fatal("Get() returned nil for added server")
	}

	// State should be Running
	if srv.State != StateRunning {
		t.Errorf("State = %q, want %q", srv.State, StateRunning)
	}

	// Process should be nil (no supervisor process)
	if srv.Process != nil {
		t.Errorf("Process = %v, want nil (stdio servers skip supervisor)", srv.Process)
	}

	// PID should be 0
	if pid := srv.PID(); pid != 0 {
		t.Errorf("PID() = %d, want 0 for stdio server", pid)
	}

	// Supervisor should not have registered this server
	if sup.GetServer("stdio-skip") != nil {
		t.Error("Supervisor.GetServer() should return nil for stdio server (skipped supervisor)")
	}

	// Clean up
	if err := reg.Stop("stdio-skip"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	if srv.State != StateStopped {
		t.Errorf("State after Stop = %q, want %q", srv.State, StateStopped)
	}
}

// TestRegistry_StdioStop_NoSupervisor verifies that Stop() handles nil Process
// gracefully (no supervisor RemoveServer call needed).
func TestRegistry_StdioStop_NoSupervisor(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	cfg := &config.ServerConfig{
		Port:    6282,
		Command: "sleep",
		Args:    []string{"60"},
	}

	reg.Add("stdio-stop", cfg)

	// Start and stop (both skip supervisor)
	if err := reg.Start("stdio-stop"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	srv := reg.Get("stdio-stop")
	if srv.Process != nil {
		t.Fatalf("Process = %v, want nil before Stop", srv.Process)
	}

	// Stop should succeed even though Process is nil
	if err := reg.Stop("stdio-stop"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	// State should be Stopped
	if srv.State != StateStopped {
		t.Errorf("State after Stop = %q, want %q", srv.State, StateStopped)
	}

	// Server should still be in registry (not removed)
	if reg.Get("stdio-stop") == nil {
		t.Error("Server should still be in registry after Stop")
	}
}

// TestRegistry_HttpStart_UsesSupervisor verifies that HTTP transport servers
// still use the supervisor normally (control group for stdio skip behavior).
func TestRegistry_HttpStart_UsesSupervisor(t *testing.T) {
	reg, sup := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Serve(ctx)

	cfg := &config.ServerConfig{
		Port:      6283,
		URL:       "http://localhost:9999/mcp",
		Transport: config.TransportHTTP,
	}

	reg.Add("http-server", cfg)

	if err := reg.Start("http-server"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	srv := reg.Get("http-server")
	if srv == nil {
		t.Fatal("Get() returned nil for added server")
	}

	// HTTP servers should still have a supervisor process
	if srv.Process == nil {
		t.Error("Process should not be nil for HTTP server (uses supervisor)")
	}

	// Clean up
	if err := reg.Stop("http-server"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// TestRegistry_FireEvent_PreservesOrder pins the ordering invariant directly.
//
// Root cause of two confirmed production outages (pokeedge-query-ops, playwright):
// fireEvent dispatched every event via a bare `go handler(event)`, and Go
// guarantees no FIFO ordering across separate `go` statements. Handlers therefore
// observed lifecycle events in an order that never occurred.
//
// Firing a long run of distinct events and requiring byte-identical arrival order
// makes the defect deterministic rather than probabilistic: unordered dispatch has
// a vanishing chance of reproducing the exact input sequence.
func TestRegistry_FireEvent_PreservesOrder(t *testing.T) {
	reg, _ := newTestRegistry()
	defer reg.Close()

	const eventCount = 100

	got := make(chan string, eventCount*2)
	reg.SetEventHandler(func(event ServerEvent) {
		got <- event.Name
	})

	want := make([]string, 0, eventCount)
	for i := range eventCount {
		name := fmt.Sprintf("srv-%03d", i)
		want = append(want, name)

		// Alternate types so an implementation cannot pass by coalescing or
		// deduplicating on type alone.
		evType := EventServerStarted
		if i%2 == 1 {
			evType = EventServerStopped
		}
		reg.fireEvent(ServerEvent{Type: evType, Name: name})
	}

	for i, wantName := range want {
		select {
		case gotName := <-got:
			if gotName != wantName {
				t.Fatalf("event %d delivered out of order: got %q, want %q", i, gotName, wantName)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %d (%q); delivery stalled", i, wantName)
		}
	}
}

// TestRegistry_Restart_EventOrderingAlternates covers the real production path
// that produced incident 1.
//
// Restart is Stop-then-Start, and each fires an event. When the Start handler ran
// first it saw the stale listener and silently returned success; the late Stop
// handler then tore that listener down by name. Nothing was bound afterwards, yet
// State remained Running and nothing was logged — a silent 15h59m outage.
//
// Strict Stopped -> Started alternation is the invariant that makes the
// interleaving impossible.
func TestRegistry_Restart_EventOrderingAlternates(t *testing.T) {
	reg, _ := newTestRegistry()
	defer reg.Close()

	const restarts = 25

	// Buffer generously so the handler never blocks and never itself becomes the
	// source of a stall.
	events := make(chan ServerEventType, restarts*2+8)
	reg.SetEventHandler(func(event ServerEvent) {
		events <- event.Type
	})

	// stdio transport: Process stays nil and the supervisor is bypassed
	// (registry.go stdio branch), so restarts are cheap and deterministic.
	// This is also incident 1's transport.
	cfg := &config.ServerConfig{
		Port:    6296,
		Command: "sleep",
		Args:    []string{"60"},
	}
	if err := reg.Add("restart-order", cfg); err != nil {
		t.Fatalf("Add() error: %v", err)
	}

	if err := reg.Start("restart-order"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	expectEvent := func(step int, want ServerEventType) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("step %d: event = %v, want %v (lifecycle events delivered out of order)", step, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("step %d: timed out waiting for %v", step, want)
		}
	}

	expectEvent(0, EventServerStarted)

	for i := range restarts {
		if err := reg.Restart("restart-order"); err != nil {
			t.Fatalf("Restart() #%d error: %v", i, err)
		}
		// Restart is Stop then Start; the handler must observe that same order.
		expectEvent(i, EventServerStopped)
		expectEvent(i, EventServerStarted)
	}

	if err := reg.Stop("restart-order"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	expectEvent(restarts, EventServerStopped)
}

// TestRegistry_Close_DrainsPendingEvents verifies Close is a real teardown
// boundary: queued events are delivered before it returns, and the drain
// goroutine does not outlive the registry.
func TestRegistry_Close_DrainsPendingEvents(t *testing.T) {
	reg, _ := newTestRegistry()

	var mu sync.Mutex
	var delivered []string
	reg.SetEventHandler(func(event ServerEvent) {
		mu.Lock()
		delivered = append(delivered, event.Name)
		mu.Unlock()
	})

	const eventCount = 20
	for i := range eventCount {
		reg.fireEvent(ServerEvent{Type: EventServerStarted, Name: fmt.Sprintf("srv-%02d", i)})
	}

	reg.Close()

	mu.Lock()
	count := len(delivered)
	mu.Unlock()

	if count != eventCount {
		t.Fatalf("delivered %d events, want %d — Close() must drain the queue", count, eventCount)
	}

	// Close must be idempotent; daemon shutdown paths can reach it more than once.
	reg.Close()

	// Firing after Close must not panic on a closed channel.
	reg.fireEvent(ServerEvent{Type: EventServerStopped, Name: "after-close"})
}

// TestRegistry_FireEvent_SlowHandlerDoesNotStallForever ensures the serialized
// drain cannot become a permanent daemon stall. Ordering is restored by putting
// events through one queue, which introduces head-of-line blocking; a per-event
// handler timeout is what bounds it.
func TestRegistry_FireEvent_SlowHandlerDoesNotStallForever(t *testing.T) {
	reg, _ := newTestRegistry()
	reg.handlerTimeout = 200 * time.Millisecond
	defer reg.Close()

	release := make(chan struct{})
	defer close(release)

	var seen atomic.Int32
	reg.SetEventHandler(func(event ServerEvent) {
		if event.Name == "slow" {
			<-release // blocks until the test finishes
			return
		}
		seen.Add(1)
	})

	reg.fireEvent(ServerEvent{Type: EventServerStarted, Name: "slow"})
	reg.fireEvent(ServerEvent{Type: EventServerStarted, Name: "fast"})

	deadline := time.After(10 * time.Second)
	for seen.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("a wedged handler blocked event delivery indefinitely; per-event timeout did not fire")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
