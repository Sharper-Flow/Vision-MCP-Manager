package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
	"github.com/thejerf/suture/v4"
)

func TestManagedServerStatusFollowsTerminalProcessState(t *testing.T) {
	zero := 0
	cfg := &config.ServerConfig{Command: "false", Transport: config.TransportStdio, RestartPolicy: config.RestartOnFailure, MaxRestarts: &zero}
	proc := supervisor.NewManagedProcess("terminal", cfg, config.SupervisionConfig{}, slog.Default())
	if err := proc.Serve(context.Background()); err == nil || errors.Is(err, suture.ErrDoNotRestart) {
		t.Fatalf("initial exit=%v", err)
	}
	if err := proc.Serve(context.Background()); !errors.Is(err, suture.ErrDoNotRestart) {
		t.Fatalf("terminal exit=%v", err)
	}

	srv := &ManagedServer{Name: "terminal", Config: cfg, State: StateRunning, Process: proc}
	status := srv.Status()
	if status.State != StateFailed {
		t.Fatalf("status state=%s, want failed", status.State)
	}
	if !strings.Contains(status.LastError, "restart limit") {
		t.Fatalf("last error=%q", status.LastError)
	}
}
