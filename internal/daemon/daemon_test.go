package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jrede/vision/internal/config"
)

// --- PID File Tests ---

func TestPIDFile_AcquireRelease(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf := NewPIDFile(pidPath)

	// Acquire
	if err := pf.Acquire(); err != nil {
		t.Fatalf("failed to acquire: %v", err)
	}

	// Verify PID file exists
	pid, err := pf.Read()
	if err != nil {
		t.Fatalf("failed to read PID: %v", err)
	}

	if pid != os.Getpid() {
		t.Errorf("expected PID %d, got %d", os.Getpid(), pid)
	}

	// Release
	if err := pf.Release(); err != nil {
		t.Fatalf("failed to release: %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected PID file to be removed")
	}
}

func TestPIDFile_PreventDuplicateAcquisition(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf1 := NewPIDFile(pidPath)
	pf2 := NewPIDFile(pidPath)

	// First acquire succeeds
	if err := pf1.Acquire(); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer pf1.Release()

	// Second acquire should fail
	err := pf2.Acquire()
	if err == nil {
		t.Error("expected second acquire to fail")
	}
}

func TestPIDFile_StalePIDDetection(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	// Write a stale PID (non-existent process)
	stalePID := 999999 // Unlikely to be a real process
	if err := os.WriteFile(pidPath, []byte("999999"), 0644); err != nil {
		t.Fatalf("failed to write stale PID: %v", err)
	}

	pf := NewPIDFile(pidPath)

	// Acquire should succeed because the PID is stale
	if err := pf.Acquire(); err != nil {
		// Note: This might fail if PID 999999 happens to exist
		// In that case, skip the test
		if os.IsPermission(err) {
			t.Skipf("PID %d exists, skipping stale PID test", stalePID)
		}
		t.Fatalf("failed to acquire with stale PID: %v", err)
	}
	defer pf.Release()

	// Verify it's our PID now
	pid, _ := pf.Read()
	if pid != os.Getpid() {
		t.Errorf("expected PID %d, got %d", os.Getpid(), pid)
	}
}

func TestPIDFile_IsRunning(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf := NewPIDFile(pidPath)

	// No PID file
	running, pid := pf.IsRunning()
	if running {
		t.Error("expected not running when no PID file")
	}
	if pid != 0 {
		t.Errorf("expected pid 0, got %d", pid)
	}

	// Create PID file
	pf.Acquire()
	defer pf.Release()

	running, pid = pf.IsRunning()
	if !running {
		t.Error("expected running after acquire")
	}
	if pid != os.Getpid() {
		t.Errorf("expected PID %d, got %d", os.Getpid(), pid)
	}
}

func TestPIDFile_DefaultPath(t *testing.T) {
	path := DefaultPIDPath()
	if path == "" {
		t.Error("expected non-empty default path")
	}
}

// --- Signal Handler Tests ---

func TestSignalHandler_Creation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// We can't easily test signal handling without a real daemon,
	// but we can verify the handler is created correctly
	handler := NewSignalHandler(nil, logger)

	if handler.logger != logger {
		t.Error("expected logger to be set")
	}
}

func TestSignalHandler_StartStop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewSignalHandler(nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	handler.Start(ctx)

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Cancel context should stop the handler
	cancel()

	// Give it a moment to stop
	time.Sleep(10 * time.Millisecond)

	// Handler should be stopped (done channel closed)
	select {
	case <-handler.done:
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("handler did not stop")
	}
}

// --- Daemon Status Tests ---

func TestDaemonStatus_Structure(t *testing.T) {
	status := DaemonStatus{
		Running:    true,
		ConfigPath: "/test/path",
	}

	if !status.Running {
		t.Error("expected running")
	}

	if status.ConfigPath != "/test/path" {
		t.Errorf("expected /test/path, got %s", status.ConfigPath)
	}
}

func TestReload_ReplacesUpdatedServerConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")

	initial := []byte("servers:\n  echo:\n    port: 6276\n    command: echo\n    autostart: false\n    request_timeout: 30s\n")
	if err := os.WriteFile(configPath, initial, 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(Config{ConfigPath: configPath, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.registry.Add("echo", d.cfg.Servers["echo"]); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	d.running = true

	updated := []byte("servers:\n  echo:\n    port: 6276\n    command: echo\n    autostart: false\n    request_timeout: 45s\n")
	if err := os.WriteFile(configPath, updated, 0o644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	if err := d.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	srv := d.registry.Get("echo")
	if srv == nil {
		t.Fatal("expected server to remain in registry")
	}
	if got := srv.Config.RequestTimeout.Duration(); got != 45*time.Second {
		t.Fatalf("RequestTimeout = %v, want 45s", got)
	}
	if got := d.cfg.Servers["echo"].RequestTimeout.Duration(); got != 45*time.Second {
		t.Fatalf("daemon cfg RequestTimeout = %v, want 45s", got)
	}
	if srv.Config.RequestTimeout != config.Duration(45*time.Second) {
		t.Fatalf("registry server config timeout = %v, want 45s", srv.Config.RequestTimeout)
	}
}

// Note: Full daemon tests require a valid config file and would be integration tests.
// The daemon.New() function loads config from disk, so we test components separately.
