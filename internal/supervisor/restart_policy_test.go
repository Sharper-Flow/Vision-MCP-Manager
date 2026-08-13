package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/thejerf/suture/v4"
)

type restartTestClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *restartTestClock) Now() time.Time { return c.now }

func (c *restartTestClock) Sleep(ctx context.Context, delay time.Duration) error {
	c.sleeps = append(c.sleeps, delay)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		c.now = c.now.Add(delay)
		return nil
	}
}

func TestRestartPolicyDecisionMatrix(t *testing.T) {
	failed := errors.New("exit 1")
	for _, tc := range []struct {
		name   string
		policy config.RestartPolicy
		exit   error
		want   bool
		state  ServiceState
	}{
		{name: "never clean", policy: config.RestartNever, want: false, state: StateStopped},
		{name: "never failed", policy: config.RestartNever, exit: failed, want: false, state: StateFailed},
		{name: "on-failure clean", policy: config.RestartOnFailure, want: false, state: StateStopped},
		{name: "on-failure failed", policy: config.RestartOnFailure, exit: failed, want: true, state: StateCrashed},
		{name: "always clean", policy: config.RestartAlways, want: true, state: StateStopped},
		{name: "always failed", policy: config.RestartAlways, exit: failed, want: true, state: StateCrashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restart, state := restartDisposition(tc.policy, tc.exit)
			if restart != tc.want || state != tc.state {
				t.Fatalf("restart=%v state=%s, want %v/%s", restart, state, tc.want, tc.state)
			}
		})
	}
}

func TestManagedProcessAppliesRestartPolicyToCleanAndFailedExit(t *testing.T) {
	max := 2
	for _, tc := range []struct {
		name      string
		policy    config.RestartPolicy
		command   string
		wantStop  bool
		wantState ServiceState
	}{
		{name: "never clean", policy: config.RestartNever, command: "true", wantStop: true, wantState: StateStopped},
		{name: "never failed", policy: config.RestartNever, command: "false", wantStop: true, wantState: StateFailed},
		{name: "on-failure clean", policy: config.RestartOnFailure, command: "true", wantStop: true, wantState: StateStopped},
		{name: "on-failure failed", policy: config.RestartOnFailure, command: "false", wantState: StateCrashed},
		{name: "always clean", policy: config.RestartAlways, command: "true", wantState: StateStopped},
		{name: "always failed", policy: config.RestartAlways, command: "false", wantState: StateCrashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := NewManagedProcess("policy", &config.ServerConfig{
				Command:       tc.command,
				Transport:     config.TransportStdio,
				RestartPolicy: tc.policy,
				MaxRestarts:   &max,
			}, config.SupervisionConfig{}, slog.Default())
			err := proc.Serve(context.Background())
			if errors.Is(err, suture.ErrDoNotRestart) != tc.wantStop {
				t.Fatalf("error=%v, stop=%v", err, tc.wantStop)
			}
			if proc.State() != tc.wantState {
				t.Fatalf("state=%s, want %s", proc.State(), tc.wantState)
			}
		})
	}
}

func TestRestartTrackerInitialLaunchExcludedAndExactLimit(t *testing.T) {
	clock := &restartTestClock{now: time.Unix(1000, 0)}
	tracker := newRestartTracker(2, time.Second, 10*time.Second, clock.Now, clock.Sleep)
	ctx := context.Background()

	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	if tracker.Count() != 0 || len(clock.sleeps) != 0 {
		t.Fatalf("initial launch count=%d sleeps=%v", tracker.Count(), clock.sleeps)
	}
	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	if tracker.Count() != 2 {
		t.Fatalf("restart count=%d, want 2", tracker.Count())
	}
	err := tracker.BeforeStart(ctx)
	var limit *RestartLimitError
	if !errors.As(err, &limit) || !errors.Is(err, suture.ErrDoNotRestart) {
		t.Fatalf("exhaustion error=%v", err)
	}
}

func TestRestartTrackerSlidingWindowAndCappedBackoff(t *testing.T) {
	clock := &restartTestClock{now: time.Unix(2000, 0)}
	tracker := newRestartTracker(3, time.Second, 3*time.Second, clock.Now, clock.Sleep)
	ctx := context.Background()
	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := tracker.BeforeStart(ctx); err != nil {
			t.Fatal(err)
		}
	}
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(clock.sleeps) != len(wantDelays) {
		t.Fatalf("sleeps=%v", clock.sleeps)
	}
	for i := range wantDelays {
		if clock.sleeps[i] != wantDelays[i] {
			t.Fatalf("sleeps=%v, want %v", clock.sleeps, wantDelays)
		}
	}

	clock.now = clock.now.Add(5*time.Minute + time.Second)
	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatalf("expired window remained exhausted: %v", err)
	}
	if tracker.Count() != 1 {
		t.Fatalf("post-expiry count=%d, want 1", tracker.Count())
	}
	if got := clock.sleeps[len(clock.sleeps)-1]; got != time.Second {
		t.Fatalf("post-expiry delay=%v, want 1s", got)
	}
}

func TestRestartTrackerWindowCountPrunesButTotalIsCumulative(t *testing.T) {
	clock := &restartTestClock{now: time.Unix(4000, 0)}
	tracker := newRestartTracker(10, time.Second, time.Second, clock.Now, clock.Sleep)
	ctx := context.Background()
	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := tracker.BeforeStart(ctx); err != nil {
			t.Fatal(err)
		}
	}
	clock.now = clock.now.Add(5*time.Minute + time.Second)
	if err := tracker.BeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	if got := tracker.Count(); got != 1 {
		t.Fatalf("current-window count=%d, want 1", got)
	}
	if got := tracker.Total(); got != 4 {
		t.Fatalf("cumulative total=%d, want 4", got)
	}
}

func TestRestartTrackerCancellationDoesNotConsumeAttempt(t *testing.T) {
	clock := &restartTestClock{now: time.Unix(3000, 0)}
	tracker := newRestartTracker(1, time.Second, time.Second, clock.Now, clock.Sleep)
	if err := tracker.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tracker.BeforeStart(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
	if tracker.Count() != 0 {
		t.Fatalf("cancelled restart consumed attempt: %d", tracker.Count())
	}
}

func TestManagedProcessRestartExhaustionIsTerminalAndFreshProcessResetsBudget(t *testing.T) {
	zero := 0
	cfg := &config.ServerConfig{
		Command:       "false",
		Transport:     config.TransportStdio,
		RestartPolicy: config.RestartOnFailure,
		MaxRestarts:   &zero,
	}
	supCfg := config.SupervisionConfig{RestartDelay: config.Duration(time.Millisecond), MaxRestartDelay: config.Duration(time.Millisecond)}
	proc := NewManagedProcess("terminal", cfg, supCfg, slog.Default())
	if err := proc.Serve(context.Background()); err == nil || errors.Is(err, suture.ErrDoNotRestart) {
		t.Fatalf("initial failed launch should remain restart-eligible: %v", err)
	}
	err := proc.Serve(context.Background())
	if !errors.Is(err, suture.ErrDoNotRestart) || proc.State() != StateFailed || proc.RestartCount() != 0 {
		t.Fatalf("terminal error=%v state=%s restarts=%d", err, proc.State(), proc.RestartCount())
	}

	fresh := NewManagedProcess("terminal", cfg, supCfg, slog.Default())
	if fresh.State() != StateStopped || fresh.RestartCount() != 0 {
		t.Fatalf("fresh process did not reset budget: state=%s restarts=%d", fresh.State(), fresh.RestartCount())
	}
}
