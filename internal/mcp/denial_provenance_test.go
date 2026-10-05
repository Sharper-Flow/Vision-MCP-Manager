package mcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

// A genuine gateway capacity denial must carry deterministic gateway-owned
// provenance on the HTTP response Vision itself writes, so the end-to-end
// probe can recognize a pre-backend denial without guessing from the status
// or body. The backend must not be touched by the denied initialize.
func TestGatewayCapacityDenialCarriesDeterministicProvenance(t *testing.T) {
	var initializations atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		initializations.Add(1)
		w.Header().Set("Mcp-Session-Id", "occupied")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	active := initializeManagedSession(t, gateway)
	occupied := initializations.Load()
	defer func() {
		deleteRequest := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		deleteRequest.Header.Set("Mcp-Session-Id", active)
		gateway.ServeHTTP(httptest.NewRecorder(), deleteRequest)
	}()

	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, newInitializeRequest())
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("denied initialize status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get(gatewayDenialHeader); got != gatewayDenialAdmissionCapacity {
		t.Fatalf("denied initialize provenance = %q, want deterministic marker %q", got, gatewayDenialAdmissionCapacity)
	}
	if got := initializations.Load(); got != occupied {
		t.Fatalf("denied initialize reached the backend: initializations = %d, want %d", got, occupied)
	}
}

// A backend that answers HTTP 429 itself is a deep failure, not a pre-backend
// denial: capacity was available, the backend was reached, and it refused.
// Threshold consecutive backend refusals must make the server unreachable.
func TestBackendFourTwentyNineRecordsDeepFailure(t *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(gateway)
	defer listener.Close()

	probe := NewEndToEndProbe()
	store := reachability.NewStore()
	store.RecordProbe("backend-429", reachability.ProbeResult{Depth: reachability.DepthEndToEnd, Disposition: reachability.DispositionSuccess})
	target := reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port}
	for range reachability.FailureThreshold {
		attempt := probe.Probe(context.Background(), target)
		if attempt.Disposition != reachability.DispositionFailure || attempt.Err == nil {
			t.Fatalf("backend 429 attempt = %+v, want deep failure", attempt)
		}
		store.RecordProbe("backend-429", reachability.ProbeResult{
			Depth:       reachability.DepthEndToEnd,
			Disposition: attempt.Disposition,
			Error:       attempt.Err.Error(),
		})
	}
	if calls.Load() != reachability.FailureThreshold {
		t.Fatalf("backend calls = %d, want %d (each probe must exercise the backend)", calls.Load(), reachability.FailureThreshold)
	}
	value, _ := store.Get("backend-429")
	if value.State != reachability.StateUnreachable {
		t.Fatalf("state after backend 429 streak = %s, want %s (actual backend failures classified as pre-backend denials)", value.State, reachability.StateUnreachable)
	}
	deep := value.Evidence[reachability.DepthEndToEnd]
	if deep.LastProbeOutcome != reachability.OutcomeFailure || deep.ConsecutiveFailures != reachability.FailureThreshold {
		t.Fatalf("deep evidence after backend 429 streak = %+v", deep)
	}
}

// A backend cannot forge gateway denial provenance: the reverse proxy strips
// the gateway-owned marker from every forwarded backend response before the
// client sees it, so a forged 429 stays a deep failure.
func TestBackendCannotForgeGatewayDenialProvenance(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(gatewayDenialHeader, gatewayDenialAdmissionCapacity)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(gateway)
	defer listener.Close()

	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, newInitializeRequest())
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("forwarded backend 429 status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get(gatewayDenialHeader); got != "" {
		t.Fatalf("forwarded response carried forged provenance %q, want it stripped", got)
	}

	probe := NewEndToEndProbe()
	attempt := probe.Probe(context.Background(), reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port})
	if attempt.Disposition != reachability.DispositionFailure || attempt.Err == nil {
		t.Fatalf("forged-provenance backend 429 attempt = %+v, want deep failure", attempt)
	}
}

// Recovery: once the backend answers successfully again, an actual deep probe
// must clear the stale deep failure the backend 429 streak left behind.
func TestBackendFourTwentyNineDeepFailureRecoversAfterBackendSuccess(t *testing.T) {
	var healthy atomic.Bool
	var next atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !healthy.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Mcp-Session-Id", fmt.Sprintf("recovered-%d", next.Add(1)))
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(gateway)
	defer listener.Close()

	probe := NewEndToEndProbe()
	target := reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port}
	store := reachability.NewStore()
	for range reachability.FailureThreshold {
		attempt := probe.Probe(context.Background(), target)
		if attempt.Disposition != reachability.DispositionFailure || attempt.Err == nil {
			t.Fatalf("unhealthy-phase attempt = %+v, want deep failure", attempt)
		}
		store.RecordProbe("recovery", reachability.ProbeResult{
			Depth:       reachability.DepthEndToEnd,
			Disposition: attempt.Disposition,
			Error:       attempt.Err.Error(),
		})
	}
	if value, _ := store.Get("recovery"); value.State != reachability.StateUnreachable {
		t.Fatalf("state before recovery = %s, want %s", value.State, reachability.StateUnreachable)
	}

	healthy.Store(true)
	attempt := probe.Probe(context.Background(), target)
	if attempt.Disposition != reachability.DispositionSuccess || attempt.Err != nil {
		t.Fatalf("recovery attempt = %+v, want deep success", attempt)
	}
	store.RecordProbe("recovery", reachability.ProbeResult{Depth: reachability.DepthEndToEnd, Disposition: reachability.DispositionSuccess})
	value, _ := store.Get("recovery")
	if value.State != reachability.StateReachable {
		t.Fatalf("state after recovery = %s, want %s (successful deep reprobe must clear the stale deep failure)", value.State, reachability.StateReachable)
	}
	deep := value.Evidence[reachability.DepthEndToEnd]
	if deep.LastProbeOutcome != reachability.OutcomeSuccess || deep.ConsecutiveFailures != 0 {
		t.Fatalf("deep evidence after recovery = %+v", deep)
	}
}
