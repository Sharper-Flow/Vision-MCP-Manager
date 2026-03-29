package mcp

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEffectiveRequestTimeout(t *testing.T) {
	t.Run("uses fallback when upstream has no deadline", func(t *testing.T) {
		timeout := effectiveRequestTimeout(context.Background(), 30*time.Second)
		if timeout != 30*time.Second {
			t.Fatalf("effectiveRequestTimeout = %v, want 30s", timeout)
		}
	})

	t.Run("uses earlier upstream deadline when present", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		timeout := effectiveRequestTimeout(ctx, 30*time.Second)
		if timeout > 5*time.Second || timeout <= 0 {
			t.Fatalf("effectiveRequestTimeout = %v, want >0 and <=5s", timeout)
		}
	})

	t.Run("uses fallback when fallback is stricter than upstream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		timeout := effectiveRequestTimeout(ctx, 10*time.Second)
		if timeout != 10*time.Second {
			t.Fatalf("effectiveRequestTimeout = %v, want 10s", timeout)
		}
	})
}

func TestWithRequestTimeoutBudget(t *testing.T) {
	ctx, cancel := withRequestTimeoutBudget(context.Background(), 50*time.Millisecond)
	defer cancel()

	time.Sleep(30 * time.Millisecond)
	remaining := effectiveRequestTimeout(ctx, 50*time.Millisecond)
	if remaining <= 0 || remaining > 20*time.Millisecond {
		t.Fatalf("remaining timeout budget = %v, want >0 and <=20ms", remaining)
	}
}

func TestIsRetryableToolCallError(t *testing.T) {
	retryable := []string{"timeout", "429", "ECONNRESET"}

	if !isRetryableToolCallError(context.DeadlineExceeded, retryable) {
		t.Fatal("expected context deadline exceeded to be retryable")
	}

	if !isRetryableToolCallError(errors.New("upstream returned 429 rate limit"), retryable) {
		t.Fatal("expected 429 error to be retryable")
	}

	if isRetryableToolCallError(errors.New("invalid params"), retryable) {
		t.Fatal("expected invalid params to be non-retryable")
	}
}

func TestShouldRecordCircuitFailure(t *testing.T) {
	retryable := []string{"timeout", "429", "ECONNRESET"}

	if !shouldRecordCircuitFailure(context.DeadlineExceeded, retryable) {
		t.Fatal("expected retryable timeout to count toward the circuit")
	}

	if shouldRecordCircuitFailure(errors.New("invalid params"), retryable) {
		t.Fatal("expected invalid params to not count toward the circuit")
	}
}

func TestComputeBackoffDelay(t *testing.T) {
	if got := computeBackoffDelay(1, 100*time.Millisecond, 5*time.Second); got != 100*time.Millisecond {
		t.Fatalf("attempt 1 delay = %v, want 100ms", got)
	}

	if got := computeBackoffDelay(3, 100*time.Millisecond, 5*time.Second); got != 400*time.Millisecond {
		t.Fatalf("attempt 3 delay = %v, want 400ms", got)
	}

	if got := computeBackoffDelay(10, 100*time.Millisecond, 5*time.Second); got != 5*time.Second {
		t.Fatalf("attempt 10 delay = %v, want 5s cap", got)
	}
}

func TestCircuitBreakerLifecycle(t *testing.T) {
	now := time.Unix(0, 0)
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  5 * time.Second,
	}, func() time.Time {
		return now
	})

	if !cb.allow() {
		t.Fatal("expected closed circuit to allow requests")
	}

	cb.recordFailure()
	if cb.state() != circuitStateClosed {
		t.Fatalf("state after one failure = %v, want closed", cb.state())
	}

	cb.recordFailure()
	if cb.state() != circuitStateOpen {
		t.Fatalf("state after threshold failures = %v, want open", cb.state())
	}
	if cb.allow() {
		t.Fatal("expected open circuit to block requests before recovery timeout")
	}

	now = now.Add(6 * time.Second)
	if !cb.allow() {
		t.Fatal("expected circuit to allow a half-open probe after recovery timeout")
	}
	if cb.state() != circuitStateHalfOpen {
		t.Fatalf("state after recovery timeout = %v, want half-open", cb.state())
	}

	cb.recordSuccess()
	if cb.state() != circuitStateClosed {
		t.Fatalf("state after successful probe = %v, want closed", cb.state())
	}
}

func TestClassifyToolCallError(t *testing.T) {
	t.Run("classifies circuit open", func(t *testing.T) {
		err := classifyToolCallError(&CircuitOpenError{Server: "kagi"}, false, nil)
		var availabilityErr *AvailabilityError
		if !errors.As(err, &availabilityErr) || availabilityErr.Category != FailureCategoryCircuitOpen {
			t.Fatalf("expected circuit_open availability error, got %v", err)
		}
	})

	t.Run("classifies provider timeout before retry exhaustion", func(t *testing.T) {
		err := classifyToolCallError(context.DeadlineExceeded, false, []string{"timeout"})
		var availabilityErr *AvailabilityError
		if !errors.As(err, &availabilityErr) || availabilityErr.Category != FailureCategoryProviderTimeout {
			t.Fatalf("expected provider_timeout availability error, got %v", err)
		}
	})

	t.Run("classifies retry exhaustion", func(t *testing.T) {
		err := classifyToolCallError(errors.New("upstream returned 503"), true, []string{"503"})
		var availabilityErr *AvailabilityError
		if !errors.As(err, &availabilityErr) || availabilityErr.Category != FailureCategoryRetryExhausted {
			t.Fatalf("expected retry_exhausted availability error, got %v", err)
		}
	})

	t.Run("classifies config drift or unavailable downstream", func(t *testing.T) {
		err := classifyToolCallError(ErrDownstreamUnavailable, false, nil)
		var availabilityErr *AvailabilityError
		if !errors.As(err, &availabilityErr) || availabilityErr.Category != FailureCategoryConfigDrift {
			t.Fatalf("expected config_drift availability error, got %v", err)
		}
	})
}
