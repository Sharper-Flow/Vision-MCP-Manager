package supervisor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

func TestManagedHTTPProcessOwnsSupervisedChild(t *testing.T) {
	cfg := &config.ServerConfig{
		Port:      6287,
		Transport: config.TransportManagedHTTP,
		Command:   "sh",
		Args:      []string{"-c", "sleep 30"},
		URL:       "http://127.0.0.1:16287/mcp",
	}
	proc := NewManagedProcess("managed-test", cfg, config.SupervisionConfig{
		ShutdownTimeout: config.Duration(time.Second),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- proc.Serve(ctx) }()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		state, generation := proc.LifecycleSnapshot()
		if state == StateRunning {
			if generation != 1 || proc.PID() == 0 {
				t.Fatalf("running snapshot generation=%d pid=%d", generation, proc.PID())
			}
			break
		}
		select {
		case <-proc.LifecycleEvents():
		case <-deadline.C:
			t.Fatalf("managed child did not start; state=%s", state)
		}
	}

	if err := proc.RequestRestart(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("managed child did not exit after restart request")
	}
}
