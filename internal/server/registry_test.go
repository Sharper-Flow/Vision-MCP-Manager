package server

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/supervisor"
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
