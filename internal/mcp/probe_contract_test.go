package mcp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

// probeRecordingTransport records the protocol response that the probe sees.
// It keeps capacity-denial evidence observable without weakening the public
// Probe interface to expose HTTP implementation details.
type probeRecordingTransport struct {
	base     http.RoundTripper
	statuses []int
	deletes  atomic.Int32
}

func (t *probeRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if req.Method == http.MethodDelete {
		t.deletes.Add(1)
	}
	if resp != nil && req.Method == http.MethodPost {
		t.statuses = append(t.statuses, resp.StatusCode)
	}
	return resp, err
}

func TestListenerProbeBypassesAdmissionRateLimitAndMCPHandler(t *testing.T) {
	var backendRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests.Add(1)
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", "normal-client")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	var handlerCalls atomic.Int32
	countingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		gateway.ServeHTTP(w, r)
	})
	listener := httptest.NewServer(
		ListenerProbeMiddleware()(RateLimitMiddleware(1, time.Hour)(countingHandler)),
	)
	defer listener.Close()

	target := reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port}
	probe := NewListenerProbe()
	baseline := gateway.Snapshot(0)
	for i := 0; i < 20; i++ {
		reachable, err := probe.Probe(context.Background(), target)
		if err != nil || !reachable {
			t.Fatalf("listener probe %d = %t, %v; want reachable", i, reachable, err)
		}
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("listener probes reached MCP handler %d times, want 0", got)
	}
	if got := backendRequests.Load(); got != 0 {
		t.Fatalf("listener probes reached downstream %d times, want 0", got)
	}
	if snapshot := gateway.Snapshot(0); snapshot.CapacityUsed != baseline.CapacityUsed || len(snapshot.Rows) != len(baseline.Rows) {
		t.Fatalf("listener probe residue: snapshot=%#v baseline=%#v", snapshot, baseline)
	}

	// A normal initialize must still consume the untouched rate-limit token.
	initialize, err := http.NewRequest(http.MethodPost, listener.URL+"/mcp", strings.NewReader(managedProbeInitialize))
	if err != nil {
		t.Fatal(err)
	}
	initialize.Header.Set("Content-Type", "application/json")
	initialize.Header.Set("Accept", "application/json, text/event-stream")
	initialize.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	response, err := listener.Client().Do(initialize)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("normal initialize after listener probes = HTTP %d, want %d", response.StatusCode, http.StatusOK)
	}
	cleanup, err := http.NewRequest(http.MethodDelete, listener.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup.Header.Set("Mcp-Session-Id", "normal-client")
	cleanup.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	cleanupResponse, err := listener.Client().Do(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	cleanupResponse.Body.Close()
}

func TestEndToEndProbeCleanupSurvivesParentCancellation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", "cancelled-parent")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(gateway)
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	transport := &cancelAfterInitializeTransport{base: http.DefaultTransport, cancel: cancel}
	probe := &EndToEndProbe{Client: &http.Client{Transport: transport}}
	baseline := gateway.Snapshot(0)
	reachable, err := probe.Probe(ctx, reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port})
	if err != nil || !reachable {
		t.Fatalf("cancelled-parent probe = %t, %v; want reachable", reachable, err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("parent context error = %v, want context canceled", ctx.Err())
	}
	if got := transport.deletes.Load(); got != 1 {
		t.Fatalf("cleanup DELETE count after parent cancellation = %d, want 1", got)
	}
	if snapshot := gateway.Snapshot(0); snapshot.CapacityUsed != baseline.CapacityUsed || len(snapshot.Rows) != len(baseline.Rows) {
		t.Fatalf("cancelled-parent residue: snapshot=%#v baseline=%#v", snapshot, baseline)
	}
}

type cancelAfterInitializeTransport struct {
	base    http.RoundTripper
	cancel  context.CancelFunc
	deletes atomic.Int32
}

func (t *cancelAfterInitializeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if req.Method == http.MethodPost && err == nil {
		t.cancel()
	}
	if req.Method == http.MethodDelete {
		t.deletes.Add(1)
	}
	return resp, err
}

func TestListenerProbeDoesNotSpawnIdleSharedStdioDownstream(t *testing.T) {
	shared := session.NewSharedSessionManager(
		"idle-stdio-contract",
		&config.ServerConfig{Command: "node", Args: []string{"-e", "process.stdin.resume()"}, MaxSessions: 1},
		slog.Default(),
		10*time.Millisecond,
		nil,
	)
	defer shared.CloseAll()

	proxy := NewProxyHandler(ProxyConfig{ServerName: "idle-stdio-contract", SharedManager: shared})
	listener := httptest.NewServer(ListenerProbeMiddleware()(proxy))
	defer listener.Close()

	probe := NewListenerProbe()
	target := reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port}
	for i := 0; i < 20; i++ {
		reachable, err := probe.Probe(context.Background(), target)
		if err != nil || !reachable {
			t.Fatalf("idle shared listener probe %d = %t, %v; want reachable", i, reachable, err)
		}
	}
	if got := shared.SessionCount(); got != 0 {
		t.Fatalf("idle shared stdio session count after L1 ticks = %d, want 0", got)
	}
	if shared.HasDownstream() {
		t.Fatal("idle shared stdio L1 ticks spawned or respawned a downstream")
	}
}
