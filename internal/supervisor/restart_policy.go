package supervisor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/thejerf/suture/v4"
)

const restartWindow = 5 * time.Minute

var errCleanRestart = fmt.Errorf("clean exit; restart requested")

// RestartLimitError is terminal for a suture supervisor, while retaining the
// typed exhaustion details for status and callers.
type RestartLimitError struct {
	Limit    int
	Attempts int
}

func (e *RestartLimitError) Error() string {
	return fmt.Sprintf("restart limit exceeded: %d attempts in %s (limit %d)", e.Attempts, restartWindow, e.Limit)
}

func (e *RestartLimitError) Unwrap() error { return suture.ErrDoNotRestart }

type restartTracker struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	base     time.Duration
	maxDelay time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	initial  bool
	attempts []time.Time
	total    int
}

func newRestartTracker(limit int, base, maxDelay time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) *restartTracker {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepWithContext
	}
	return &restartTracker{limit: limit, window: restartWindow, base: base, maxDelay: maxDelay, now: now, sleep: sleep}
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *restartTracker) prune(now time.Time) {
	first := 0
	for first < len(t.attempts) && now.Sub(t.attempts[first]) >= t.window {
		first++
	}
	if first > 0 {
		t.attempts = append([]time.Time(nil), t.attempts[first:]...)
	}
}

// BeforeStart authorizes one Serve invocation. The initial launch consumes no
// restart attempt. An attempt is committed only after its cancellation-aware
// delay completes.
func (t *restartTracker) BeforeStart(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.initial {
		t.initial = true
		return nil
	}
	now := t.now()
	t.prune(now)
	if len(t.attempts) >= t.limit {
		return &RestartLimitError{Limit: t.limit, Attempts: len(t.attempts)}
	}
	if err := t.sleep(ctx, t.backoff(len(t.attempts))); err != nil {
		return err
	}
	t.attempts = append(t.attempts, t.now())
	t.total++
	return nil
}

func (t *restartTracker) backoff(attempt int) time.Duration {
	if t.base <= 0 || t.maxDelay <= 0 {
		return 0
	}
	delay := t.base
	for i := 0; i < attempt && delay < t.maxDelay; i++ {
		if delay > t.maxDelay/2 {
			return t.maxDelay
		}
		delay *= 2
	}
	if delay > t.maxDelay {
		return t.maxDelay
	}
	return delay
}

func (t *restartTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(t.now())
	return len(t.attempts)
}

// Total returns the cumulative number of automatic restart attempts. Unlike
// Count, it is not limited to the current sliding window.
func (t *restartTracker) Total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}

func restartDisposition(policy config.RestartPolicy, exit error) (bool, ServiceState) {
	if policy == config.RestartAlways {
		if exit != nil {
			return true, StateCrashed
		}
		return true, StateStopped
	}
	if (policy == config.RestartOnFailure || policy == "") && exit != nil {
		return true, StateCrashed
	}
	if exit != nil {
		return false, StateFailed
	}
	return false, StateStopped
}
