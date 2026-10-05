package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

// A seeded deep failure must survive capacity-denied deep probes: a full
// admission gate answers before the backend is touched, which proves the
// listener and gateway but is inconclusive at backend depth. Only a deep
// probe that actually reaches the backend may clear deep-failure evidence,
// and clearing must never happen without that backend initialization.
func TestManagerCapacityDeniedDeepProbesRetainSeededDeepFailure(t *testing.T) {
	var backendInitializations atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		backendInitializations.Add(1)
		w.Header().Set("Mcp-Session-Id", "occupied")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	active := initializeManagedSession(t, gateway)
	defer func() {
		deleteRequest := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		deleteRequest.Header.Set("Mcp-Session-Id", active)
		gateway.ServeHTTP(httptest.NewRecorder(), deleteRequest)
	}()

	var denied atomic.Int64
	listener := httptest.NewServer(ListenerProbeMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			denied.Add(1)
		}
		gateway.ServeHTTP(w, r)
	})))
	defer listener.Close()

	store := reachability.NewStore()
	for range reachability.FailureThreshold {
		store.RecordProbe("denied-at-cap", reachability.ProbeResult{
			Depth:       reachability.DepthEndToEnd,
			Disposition: reachability.DispositionFailure,
			Error:       "end-to-end initialize: context deadline exceeded",
		})
	}
	if value, _ := store.Get("denied-at-cap"); value.State != reachability.StateUnreachable {
		t.Fatalf("seeded state = %v, want %v", value.State, reachability.StateUnreachable)
	}

	manager := reachability.NewManager(store, reachability.NewVersionSelector(NewListenerProbe(), NewEndToEndProbe()))
	target := reachability.Target{Name: "denied-at-cap", Port: listener.Listener.Addr().(*net.TCPAddr).Port, ProtocolVersion: reachability.ProtocolVersion2025_11_25}
	if err := manager.Start(context.Background(), target, 2*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	// Keep session evidence continuously fresh so every deep probe happens
	// only because the failed deep depth demands recovery.
	refreshDone := make(chan struct{})
	defer close(refreshDone)
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-refreshDone:
				return
			case <-ticker.C:
				store.RecordProbe("denied-at-cap", reachability.ProbeResult{Depth: reachability.DepthSession, Disposition: reachability.DispositionSuccess, AttemptedAt: time.Now()})
			}
		}
	}()

	// At least one deep probe must be denied by the full gate, and the deep
	// failure must survive it with no backend initialization beyond the
	// occupied session.
	waitForDeepDenial := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(waitForDeepDenial) {
		value, _ := store.Get("denied-at-cap")
		if denied.Load() >= 1 && value.Evidence[reachability.DepthEndToEnd].LastProbeOutcome != reachability.OutcomeFailure {
			t.Fatalf("denied probe cleared deep failure: state=%s deep=%+v denied=%d backend_initializations=%d",
				value.State, value.Evidence[reachability.DepthEndToEnd], denied.Load(), backendInitializations.Load())
		}
		if value.Evidence[reachability.DepthEndToEnd].ConsecutiveFailures != reachability.FailureThreshold {
			t.Fatalf("denied probe changed deep failure streak: deep=%+v", value.Evidence[reachability.DepthEndToEnd])
		}
		time.Sleep(time.Millisecond)
	}
	if denied.Load() < 1 {
		t.Fatalf("no deep probe reached the full admission gate: denied=%d", denied.Load())
	}
	if got := backendInitializations.Load(); got != 1 {
		t.Fatalf("backend initializations during denial phase = %d, want 1 (occupied session only)", got)
	}

	// Free capacity: the next deep probe must actually reach the backend, and
	// only that real initialization may clear the deep failure.
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	deleteRequest.Header.Set("Mcp-Session-Id", active)
	gateway.ServeHTTP(httptest.NewRecorder(), deleteRequest)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value, _ := store.Get("denied-at-cap")
		deep := value.Evidence[reachability.DepthEndToEnd]
		if deep.LastProbeOutcome == reachability.OutcomeSuccess && backendInitializations.Load() >= 2 {
			// The aggregate state may still report probing while the next
			// listener tick is in flight; wait for it to settle.
			settleDeadline := time.Now().Add(time.Second)
			for time.Now().Before(settleDeadline) {
				value, _ := store.Get("denied-at-cap")
				if value.State == reachability.StateReachable {
					t.Logf("recovered after real backend initialization: denied=%d backend_initializations=%d", denied.Load(), backendInitializations.Load())
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatalf("state never settled to reachable after real deep success: state=%s", value.State)
		}
		time.Sleep(time.Millisecond)
	}
	value, _ := store.Get("denied-at-cap")
	t.Fatalf("deep failure never cleared after capacity was freed: state=%s deep=%+v backend_initializations=%d",
		value.State, value.Evidence[reachability.DepthEndToEnd], backendInitializations.Load())
}
