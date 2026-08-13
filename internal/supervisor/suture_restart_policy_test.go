package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/thejerf/suture/v4"
)

type activeSibling struct {
	started chan struct{}
	calls   atomic.Int32
}

func (s *activeSibling) Serve(ctx context.Context) error {
	s.calls.Add(1)
	select {
	case <-s.started:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestSutureRemovesOnlyTerminalRestartService(t *testing.T) {
	zero := 0
	proc := NewManagedProcess("terminal", &config.ServerConfig{
		Command:       "false",
		Transport:     config.TransportStdio,
		RestartPolicy: config.RestartOnFailure,
		MaxRestarts:   &zero,
	}, config.SupervisionConfig{}, nilLogger())
	sibling := &activeSibling{started: make(chan struct{})}
	close(sibling.started)

	firstFailure := make(chan struct{})
	sup := suture.New("restart-policy-proof", suture.Spec{
		Timeout: time.Second,
		EventHook: func(event suture.Event) {
			if terminated, ok := event.(suture.EventServiceTerminate); ok {
				if terminated.Service == proc && terminated.Restarting {
					close(firstFailure)
				}
			}
		},
	})
	sup.Add(proc)
	sup.Add(sibling)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Serve(ctx) }()

	select {
	case <-firstFailure:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial failed exit")
	}
	lifecycleDeadline := time.NewTimer(2 * time.Second)
	defer lifecycleDeadline.Stop()
	for proc.State() != StateFailed {
		select {
		case <-proc.LifecycleEvents():
		case <-lifecycleDeadline.C:
			t.Fatalf("timed out waiting for terminal state, got %s", proc.State())
		}
	}
	if !errors.Is(proc.LastError(), suture.ErrDoNotRestart) {
		t.Fatalf("last error=%v, want wrapped ErrDoNotRestart", proc.LastError())
	}
	if sibling.calls.Load() != 1 {
		t.Fatalf("sibling Serve calls=%d, want 1 active sibling", sibling.calls.Load())
	}
	drained := false
	for !drained {
		select {
		case <-proc.LifecycleEvents():
		default:
			drained = true
		}
	}
	// A short negative window catches an accidental third Serve invocation while
	// keeping the proof independent of scheduler timing for the positive path.
	select {
	case <-proc.LifecycleEvents():
		t.Fatal("terminal service emitted a lifecycle transition after removal")
	case <-time.After(50 * time.Millisecond):
	}
	if proc.RestartCount() != 0 {
		t.Fatalf("terminal process restart count=%d, want 0", proc.RestartCount())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop after cancellation")
	}
}

func nilLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
