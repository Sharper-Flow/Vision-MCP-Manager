package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// RetryConfig controls retry behavior for retryable downstream failures.
type RetryConfig struct {
	MaxAttempts     int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	RetryableErrors []string
}

// CircuitBreakerConfig controls fast-fail behavior after repeated failures.
type CircuitBreakerConfig struct {
	FailureThreshold int
	RecoveryTimeout  time.Duration
}

type circuitState string

const (
	circuitStateClosed   circuitState = "closed"
	circuitStateOpen     circuitState = "open"
	circuitStateHalfOpen circuitState = "half-open"
)

// CircuitOpenError reports that the downstream circuit is open and currently fast-failing.
type CircuitOpenError struct {
	Server  string
	RetryIn time.Duration
}

func (e *CircuitOpenError) Error() string {
	if e == nil {
		return "downstream circuit breaker open"
	}
	if e.RetryIn > 0 {
		return fmt.Sprintf("downstream circuit breaker open for %s, retry in %s", e.Server, e.RetryIn)
	}
	return fmt.Sprintf("downstream circuit breaker open for %s", e.Server)
}

type circuitBreaker struct {
	mu         sync.Mutex
	status     circuitState
	failures   int
	openedAt   time.Time
	probeInFly bool
	config     CircuitBreakerConfig
	now        func() time.Time
}

func newCircuitBreaker(cfg CircuitBreakerConfig, now func() time.Time) *circuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.RecoveryTimeout <= 0 {
		cfg.RecoveryTimeout = 60 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &circuitBreaker{
		status: circuitStateClosed,
		config: cfg,
		now:    now,
	}
}

func (cb *circuitBreaker) allow() bool {
	if cb == nil {
		return true
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.status {
	case circuitStateClosed:
		return true
	case circuitStateOpen:
		if cb.now().Sub(cb.openedAt) < cb.config.RecoveryTimeout {
			return false
		}
		cb.status = circuitStateHalfOpen
		cb.probeInFly = true
		return true
	case circuitStateHalfOpen:
		if cb.probeInFly {
			return false
		}
		cb.probeInFly = true
		return true
	default:
		return true
	}
}

func (cb *circuitBreaker) recordSuccess() {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.status = circuitStateClosed
	cb.failures = 0
	cb.probeInFly = false
	cb.openedAt = time.Time{}
}

func (cb *circuitBreaker) recordFailure() {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.status == circuitStateHalfOpen {
		cb.status = circuitStateOpen
		cb.probeInFly = false
		cb.failures = cb.config.FailureThreshold
		cb.openedAt = cb.now()
		return
	}

	cb.failures++
	cb.probeInFly = false
	if cb.failures >= cb.config.FailureThreshold {
		cb.status = circuitStateOpen
		cb.openedAt = cb.now()
	}
}

func (cb *circuitBreaker) state() circuitState {
	if cb == nil {
		return circuitStateClosed
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.status
}

func (cb *circuitBreaker) retryIn() time.Duration {
	if cb == nil {
		return 0
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.status != circuitStateOpen {
		return 0
	}
	remaining := cb.config.RecoveryTimeout - cb.now().Sub(cb.openedAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func effectiveRequestTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return remaining
		}
		if fallback <= 0 || remaining < fallback {
			return remaining
		}
	}
	return fallback
}

func isRetryableToolCallError(err error, patterns []string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDownstreamUnavailable) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, pattern := range patterns {
		if pattern != "" && strings.Contains(msg, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func computeBackoffDelay(attempt int, initialDelay, maxDelay time.Duration) time.Duration {
	if attempt <= 1 {
		if initialDelay <= 0 {
			return 0
		}
		return initialDelay
	}
	if initialDelay <= 0 {
		return 0
	}
	delay := initialDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if maxDelay > 0 && delay >= maxDelay {
			return maxDelay
		}
	}
	if maxDelay > 0 && delay > maxDelay {
		return maxDelay
	}
	return delay
}
