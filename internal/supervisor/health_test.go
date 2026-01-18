package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// mockBridge implements BridgeProvider for testing.
type mockBridge struct {
	response  []byte
	err       error
	callCount atomic.Int32
}

func (m *mockBridge) ForwardRequest(ctx context.Context, data []byte) ([]byte, error) {
	m.callCount.Add(1)
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

// --- HealthChecker Tests ---

func TestHealthChecker_InitiallyHealthy(t *testing.T) {
	bridge := &mockBridge{
		response: []byte(`{"jsonrpc":"2.0","id":"health-check","result":{"tools":[]}}`),
	}

	hc := NewHealthChecker(bridge, HealthCheckerConfig{
		CheckInterval: 100 * time.Millisecond,
		CheckTimeout:  50 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !hc.IsHealthy() {
		t.Error("expected initially healthy")
	}
}

func TestHealthChecker_BecomesUnhealthy(t *testing.T) {
	bridge := &mockBridge{
		err: errors.New("connection refused"),
	}

	unhealthyCalled := make(chan struct{}, 1)

	hc := NewHealthChecker(bridge, HealthCheckerConfig{
		CheckInterval:    50 * time.Millisecond,
		CheckTimeout:     25 * time.Millisecond,
		FailureThreshold: 2,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnUnhealthy: func() {
			select {
			case unhealthyCalled <- struct{}{}:
			default:
			}
		},
	})

	hc.Start()
	defer hc.Stop()

	// Wait for callback
	select {
	case <-unhealthyCalled:
		// Expected
	case <-time.After(500 * time.Millisecond):
		t.Error("expected OnUnhealthy to be called")
	}

	if hc.IsHealthy() {
		t.Error("expected unhealthy after failures")
	}

	status := hc.Status()
	if status.ConsecutiveFails < 2 {
		t.Errorf("expected at least 2 consecutive fails, got %d", status.ConsecutiveFails)
	}
}

func TestHealthChecker_RecoverAfterFailure(t *testing.T) {
	bridge := &mockBridge{
		err: errors.New("temporary failure"),
	}

	hc := NewHealthChecker(bridge, HealthCheckerConfig{
		CheckInterval:    50 * time.Millisecond,
		CheckTimeout:     25 * time.Millisecond,
		FailureThreshold: 5, // High threshold so we don't trigger unhealthy
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	hc.Start()

	// Wait for some failures
	time.Sleep(150 * time.Millisecond)

	// Now fix the bridge
	bridge.err = nil
	bridge.response = []byte(`{"jsonrpc":"2.0","id":"health-check","result":{"tools":[]}}`)

	// Wait for recovery
	time.Sleep(100 * time.Millisecond)

	hc.Stop()

	status := hc.Status()
	if status.ConsecutiveFails != 0 {
		t.Errorf("expected consecutive fails to reset, got %d", status.ConsecutiveFails)
	}
}

func TestHealthChecker_DetectsErrorResponse(t *testing.T) {
	// Server returns an error in the JSON-RPC response
	bridge := &mockBridge{
		response: []byte(`{"jsonrpc":"2.0","id":"health-check","error":{"code":-32600,"message":"Invalid Request"}}`),
	}

	hc := NewHealthChecker(bridge, HealthCheckerConfig{
		CheckInterval:    50 * time.Millisecond,
		CheckTimeout:     25 * time.Millisecond,
		FailureThreshold: 2,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	hc.Start()
	defer hc.Stop()

	// Wait for failures
	time.Sleep(200 * time.Millisecond)

	if hc.IsHealthy() {
		t.Error("expected unhealthy when server returns error")
	}
}

func TestHealthChecker_Status(t *testing.T) {
	bridge := &mockBridge{
		response: []byte(`{"jsonrpc":"2.0","id":"health-check","result":{"tools":[]}}`),
	}

	hc := NewHealthChecker(bridge, HealthCheckerConfig{
		CheckInterval: 50 * time.Millisecond,
		CheckTimeout:  25 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	hc.Start()
	time.Sleep(100 * time.Millisecond)
	hc.Stop()

	status := hc.Status()

	if !status.Healthy {
		t.Error("expected healthy status")
	}

	if status.LastCheck.IsZero() {
		t.Error("expected LastCheck to be set")
	}

	if status.LastError != "" {
		t.Errorf("expected no error, got %q", status.LastError)
	}
}

// --- StartupProbe Tests ---

func TestStartupProbe_SucceedsImmediately(t *testing.T) {
	bridge := &mockBridge{
		response: []byte(`{"jsonrpc":"2.0","id":"startup-probe","result":{"protocolVersion":"2024-11-05"}}`),
	}

	probe := NewStartupProbe(bridge, StartupProbeConfig{
		Timeout:      1 * time.Second,
		PollInterval: 50 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	err := probe.Wait(context.Background())
	if err != nil {
		t.Errorf("expected success, got error: %v", err)
	}
}

func TestStartupProbe_RetriesUntilSuccess(t *testing.T) {
	failCount := 0
	bridge := &mockBridge{}

	// Simulate temporary failure
	originalForward := bridge.ForwardRequest
	_ = originalForward // unused, but keeping for documentation

	// Use a custom mock that fails first 2 times
	customBridge := &retryingMockBridge{
		failsRemaining:  2,
		successResponse: []byte(`{"jsonrpc":"2.0","id":"startup-probe","result":{"protocolVersion":"2024-11-05"}}`),
		failError:       errors.New("not ready yet"),
		callCount:       &failCount,
	}

	probe := NewStartupProbe(customBridge, StartupProbeConfig{
		Timeout:      2 * time.Second,
		PollInterval: 50 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	err := probe.Wait(context.Background())
	if err != nil {
		t.Errorf("expected success after retries, got error: %v", err)
	}

	if failCount < 2 {
		t.Errorf("expected at least 2 failed attempts, got %d", failCount)
	}
}

func TestStartupProbe_TimesOut(t *testing.T) {
	bridge := &mockBridge{
		err: errors.New("server not ready"),
	}

	probe := NewStartupProbe(bridge, StartupProbeConfig{
		Timeout:      200 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	err := probe.Wait(context.Background())
	if err == nil {
		t.Error("expected timeout error")
	}

	var timeoutErr *StartupTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Errorf("expected StartupTimeoutError, got %T", err)
	}
}

func TestStartupProbe_RespectsContext(t *testing.T) {
	bridge := &mockBridge{
		err: errors.New("server not ready"),
	}

	probe := NewStartupProbe(bridge, StartupProbeConfig{
		Timeout:      5 * time.Second,
		PollInterval: 50 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := probe.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}
}

// --- Metrics Tests ---

func TestMetrics_RecordRequest(t *testing.T) {
	m := NewMetrics()

	m.RecordRequest(100*time.Millisecond, false)
	m.RecordRequest(200*time.Millisecond, false)
	m.RecordRequest(50*time.Millisecond, true) // Error

	snap := m.Snapshot()

	if snap.RequestCount != 3 {
		t.Errorf("expected 3 requests, got %d", snap.RequestCount)
	}

	if snap.ErrorCount != 1 {
		t.Errorf("expected 1 error, got %d", snap.ErrorCount)
	}
}

func TestMetrics_AvgResponseTime(t *testing.T) {
	m := NewMetrics()

	m.RecordRequest(100*time.Millisecond, false)
	m.RecordRequest(200*time.Millisecond, false)
	m.RecordRequest(300*time.Millisecond, false)

	snap := m.Snapshot()

	// Average should be 200ms
	expectedAvg := 200 * time.Millisecond
	tolerance := 10 * time.Millisecond

	if snap.AvgResponseTime < expectedAvg-tolerance || snap.AvgResponseTime > expectedAvg+tolerance {
		t.Errorf("expected avg ~%v, got %v", expectedAvg, snap.AvgResponseTime)
	}
}

func TestMetrics_P95ResponseTime(t *testing.T) {
	m := NewMetrics()

	// Add 100 samples
	for i := 0; i < 100; i++ {
		m.RecordRequest(time.Duration(i)*time.Millisecond, false)
	}

	snap := m.Snapshot()

	// P95 should be around 95ms
	if snap.P95ResponseTime < 90*time.Millisecond || snap.P95ResponseTime > 99*time.Millisecond {
		t.Errorf("expected p95 ~95ms, got %v", snap.P95ResponseTime)
	}
}

func TestMetrics_Reset(t *testing.T) {
	m := NewMetrics()

	m.RecordRequest(100*time.Millisecond, false)
	m.RecordRequest(200*time.Millisecond, true)

	m.Reset()

	snap := m.Snapshot()

	if snap.RequestCount != 0 {
		t.Errorf("expected 0 requests after reset, got %d", snap.RequestCount)
	}

	if snap.ErrorCount != 0 {
		t.Errorf("expected 0 errors after reset, got %d", snap.ErrorCount)
	}
}

func TestMetrics_Uptime(t *testing.T) {
	m := NewMetrics()

	time.Sleep(50 * time.Millisecond)

	snap := m.Snapshot()

	if snap.Uptime < 50*time.Millisecond {
		t.Errorf("expected uptime >= 50ms, got %v", snap.Uptime)
	}
}

func TestMetrics_SlidingWindow(t *testing.T) {
	m := &Metrics{
		responseTimes: make([]int64, 0, 10),
		maxSamples:    10,
		startTime:     time.Now(),
	}

	// Add more than maxSamples
	for i := 0; i < 15; i++ {
		m.RecordRequest(time.Duration(i)*time.Millisecond, false)
	}

	m.mu.RLock()
	sampleCount := len(m.responseTimes)
	m.mu.RUnlock()

	if sampleCount > 10 {
		t.Errorf("expected max 10 samples, got %d", sampleCount)
	}
}

func TestMetricsSnapshot_JSON(t *testing.T) {
	snap := MetricsSnapshot{
		RequestCount:    100,
		ErrorCount:      5,
		Uptime:          1 * time.Hour,
		AvgResponseTime: 50 * time.Millisecond,
		P95ResponseTime: 100 * time.Millisecond,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded MetricsSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.RequestCount != snap.RequestCount {
		t.Errorf("expected RequestCount %d, got %d", snap.RequestCount, decoded.RequestCount)
	}
}

// --- Helper Types ---

type retryingMockBridge struct {
	failsRemaining  int
	successResponse []byte
	failError       error
	callCount       *int
}

func (m *retryingMockBridge) ForwardRequest(ctx context.Context, data []byte) ([]byte, error) {
	*m.callCount++

	if m.failsRemaining > 0 {
		m.failsRemaining--
		return nil, m.failError
	}

	return m.successResponse, nil
}
