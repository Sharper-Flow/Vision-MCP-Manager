package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
)

func TestManagedHTTPGatewayPreservesDistinctSessionIdentity(t *testing.T) {
	var next atomic.Int32
	var mu sync.Mutex
	seen := make(map[string]int)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Fatalf("backend path = %q, want /mcp", r.URL.Path)
		}
		id := r.Header.Get("Mcp-Session-Id")
		if id == "" {
			id = fmt.Sprintf("downstream-%d", next.Add(1))
			w.Header().Set("Mcp-Session-Id", id)
		}
		mu.Lock()
		seen[id]++
		count := seen[id]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"session":%q,"count":%d}}`, id, count)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 2, backend.Client().Transport)
	first := initializeManagedSession(t, gateway)
	second := initializeManagedSession(t, gateway)
	if first == second {
		t.Fatalf("distinct initializes returned same session %q", first)
	}

	for _, id := range []string{first, second, first, second} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`))
		req.Header.Set("Mcp-Session-Id", id)
		resp := httptest.NewRecorder()
		gateway.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("session %q status = %d, body = %s", id, resp.Code, resp.Body.String())
		}
		if got := resp.Header().Get("Mcp-Session-Id"); got != "" {
			t.Fatalf("backend unexpectedly rewrote established session header to %q", got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if seen[first] != 3 || seen[second] != 3 { // initialize + two calls each
		t.Fatalf("backend counts = %#v, want three requests per isolated session", seen)
	}
}

func TestManagedHTTPGatewayRejectsUnknownSessionBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
	req.Header.Set("Mcp-Session-Id", "expired")
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("downstream calls = %d, want zero", calls.Load())
	}
}

func TestManagedHTTPGatewayRejectsInitializeNotificationBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"initialize","params":{}}`))
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound || calls.Load() != 0 || gateway.Snapshot(10).CapacityUsed != 0 {
		t.Fatalf("initialize notification status=%d calls=%d capacity=%d", resp.Code, calls.Load(), gateway.Snapshot(10).CapacityUsed)
	}
}

func TestManagedHTTPGatewayRejectsNonLoopbackTarget(t *testing.T) {
	target, err := url.Parse("http://example.com/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{Target: target}); err == nil {
		t.Fatal("expected non-loopback target rejection")
	}
}

func TestManagedHTTPGatewayCapacityRejectsWithoutEviction(t *testing.T) {
	var next atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", next.Add(1)))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	first := initializeManagedSession(t, gateway)

	req := newInitializeRequest()
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("second initialize status = %d, want 429; body = %s", resp.Code, resp.Body.String())
	}
	if next.Load() != 1 {
		t.Fatalf("backend initializes = %d, want one", next.Load())
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	request.Header.Set("Mcp-Session-Id", first)
	active := httptest.NewRecorder()
	gateway.ServeHTTP(active, request)
	if active.Code != http.StatusOK {
		t.Fatalf("existing session status = %d, want 200", active.Code)
	}
}

func TestManagedHTTPGatewayPublishesBoundedLifecycleMetrics(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", "metrics-session")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL + "/mcp")
	serverMetrics := metrics.NewServerMetrics()
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: 30 * time.Minute,
		Transport: backend.Client().Transport, Metrics: serverMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := initializeManagedSession(t, gateway)
	denied := httptest.NewRecorder()
	gateway.ServeHTTP(denied, newInitializeRequest())
	deleted := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	deleted.Header.Set("Mcp-Session-Id", id)
	gateway.ServeHTTP(httptest.NewRecorder(), deleted)

	snapshot := serverMetrics.Snapshot()
	if snapshot.ActiveSessions != 0 || snapshot.AdmissionDenied != 1 || snapshot.ReapedByReason[metrics.ReapReasonUpstreamDelete] != 1 {
		t.Fatalf("metrics snapshot = %#v", snapshot)
	}
}

func TestManagedHTTPGatewayExpiresByApplicationIdleNotSSE(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	var deletes atomic.Int32
	var toolCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.Header.Get("Mcp-Session-Id") == "" {
				w.Header().Set("Mcp-Session-Id", "idle-session")
			} else {
				toolCalls.Add(1)
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": keepalive\n\n")
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: 30 * time.Minute,
		Clock: clock, Transport: backend.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := initializeManagedSession(t, gateway)

	clock.Advance(29 * time.Minute)
	sse := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	sse.Header.Set("Mcp-Session-Id", id)
	sse.Header.Set("Accept", "text/event-stream")
	gateway.ServeHTTP(httptest.NewRecorder(), sse)
	clock.Advance(time.Minute)
	gateway.pruneIdle(context.Background())

	if deletes.Load() != 1 || gateway.Snapshot(10).CapacityUsed != 0 {
		t.Fatalf("expiry delete=%d capacity=%d, want 1 and 0", deletes.Load(), gateway.Snapshot(10).CapacityUsed)
	}
	reuse := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	reuse.Header.Set("Mcp-Session-Id", id)
	reuseResp := httptest.NewRecorder()
	gateway.ServeHTTP(reuseResp, reuse)
	if reuseResp.Code != http.StatusNotFound || toolCalls.Load() != 0 {
		t.Fatalf("expired reuse status=%d downstream tool calls=%d", reuseResp.Code, toolCalls.Load())
	}
}

func TestManagedHTTPGatewayReapsDisconnectedSSEAfterGrace(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "disconnected-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": keepalive\n\n")
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	serverMetrics := metrics.NewServerMetrics()
	target, _ := url.Parse(backend.URL + "/mcp")
	grace := 2 * time.Minute
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: time.Hour, DisconnectGracePeriod: grace,
		Clock: clock, Transport: backend.Client().Transport, Metrics: serverMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := initializeManagedSession(t, gateway)
	openManagedSSE(t, gateway, id)
	clock.Advance(grace + time.Nanosecond)
	gateway.pruneIdle(context.Background())

	snapshot := serverMetrics.Snapshot()
	if deletes.Load() != 1 || gateway.Snapshot(10).CapacityUsed != 0 {
		t.Fatalf("delete=%d capacity=%d, want one delete and released capacity", deletes.Load(), gateway.Snapshot(10).CapacityUsed)
	}
	if snapshot.ReapedByReason[metrics.ReapReasonClientDisconnected] != 1 {
		t.Fatalf("reaped metrics = %#v, want client_disconnected=1", snapshot.ReapedByReason)
	}
}

func TestManagedHTTPGatewayReconnectCancelsDisconnectGrace(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "reconnect-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": keepalive\n\n")
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL + "/mcp")
	grace := 2 * time.Minute
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: time.Hour, DisconnectGracePeriod: grace,
		Clock: clock, Transport: backend.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := initializeManagedSession(t, gateway)
	openManagedSSE(t, gateway, id)
	clock.Advance(time.Minute)
	openManagedSSE(t, gateway, id)
	clock.Advance(time.Minute + time.Nanosecond)
	gateway.pruneIdle(context.Background())

	if deletes.Load() != 0 || gateway.Snapshot(10).CapacityUsed != 1 {
		t.Fatalf("delete=%d capacity=%d, want no delete and active lease", deletes.Load(), gateway.Snapshot(10).CapacityUsed)
	}
}

func TestManagedHTTPGatewayZeroDisconnectGraceRestoresIdleOnlyReaping(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "zero-grace-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": keepalive\n\n")
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	serverMetrics := metrics.NewServerMetrics()
	target, _ := url.Parse(backend.URL + "/mcp")
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: time.Hour, DisconnectGracePeriod: 0,
		Clock: clock, Transport: backend.Client().Transport, Metrics: serverMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := initializeManagedSession(t, gateway)
	openManagedSSE(t, gateway, id)
	clock.Advance(time.Hour)
	gateway.pruneIdle(context.Background())

	snapshot := serverMetrics.Snapshot()
	if deletes.Load() != 1 || snapshot.ReapedByReason[metrics.ReapReasonIdleTimeout] != 1 || gateway.Snapshot(10).CapacityUsed != 0 {
		t.Fatalf("delete=%d metrics=%#v capacity=%d, want idle_timeout reap", deletes.Load(), snapshot.ReapedByReason, gateway.Snapshot(10).CapacityUsed)
	}
	if snapshot.ReapedByReason[metrics.ReapReasonClientDisconnected] != 0 {
		t.Fatalf("unexpected disconnect reap metrics = %#v", snapshot.ReapedByReason)
	}
}

func TestManagedHTTPGatewayNeverStreamedReasonReachesMetrics(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "never-streamed-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	serverMetrics := metrics.NewServerMetrics()
	target, _ := url.Parse(backend.URL + "/mcp")
	grace := time.Minute
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: time.Hour, DisconnectGracePeriod: grace,
		Clock: clock, Transport: backend.Client().Transport, Metrics: serverMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	initializeManagedSession(t, gateway)
	clock.Advance(5*grace + time.Nanosecond)
	gateway.pruneIdle(context.Background())

	snapshot := serverMetrics.Snapshot()
	if deletes.Load() != 1 || snapshot.ReapedByReason[metrics.ReapReasonNeverStreamed] != 1 {
		t.Fatalf("delete=%d metrics=%#v, want never_streamed reap", deletes.Load(), snapshot.ReapedByReason)
	}
	if snapshot.ReapedByReason[metrics.ReapReasonUnknown] != 0 {
		t.Fatalf("never_streamed reason collapsed to unknown: %#v", snapshot.ReapedByReason)
	}
}

func TestManagedHTTPGatewayExpiryDeleteIsAtMostOnce(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "once-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL + "/mcp")
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: time.Minute, Clock: clock,
		Transport: backend.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	initializeManagedSession(t, gateway)
	clock.Advance(time.Minute + time.Nanosecond)
	gateway.pruneIdle(context.Background())
	gateway.pruneIdle(context.Background())

	if deletes.Load() != 1 || gateway.Snapshot(10).CapacityUsed != 0 {
		t.Fatalf("delete=%d capacity=%d, want one delete and released capacity", deletes.Load(), gateway.Snapshot(10).CapacityUsed)
	}
}

func openManagedSSE(t *testing.T, gateway http.Handler, sessionID string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	req.Header.Set("Accept", "text/event-stream")
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("SSE status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestManagedHTTPGatewayDoesNotExpireInflightApplicationRequest(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	started := make(chan struct{})
	release := make(chan struct{})
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.Header.Get("Mcp-Session-Id") == "" {
				w.Header().Set("Mcp-Session-Id", "inflight-session")
			} else {
				close(started)
				<-release
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL + "/mcp")
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target: target, MaxSessions: 1, IdleTimeout: 30 * time.Minute,
		Clock: clock, Transport: backend.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := initializeManagedSession(t, gateway)

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`))
		req.Header.Set("Mcp-Session-Id", id)
		gateway.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started
	clock.Advance(31 * time.Minute)
	gateway.pruneIdle(context.Background())
	if deletes.Load() != 0 {
		t.Fatalf("deleted while application request in flight")
	}
	close(release)
	<-done
	clock.Advance(30 * time.Minute)
	gateway.pruneIdle(context.Background())
	if deletes.Load() != 1 {
		t.Fatalf("deletes after completion = %d, want 1", deletes.Load())
	}
}

func TestManagedHTTPGatewayNeverReplaysAmbiguousDispatch(t *testing.T) {
	transport := &failingManagedTransport{}
	gateway := newTestManagedGateway(t, "http://127.0.0.1:16287/mcp", 1, transport)
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newInitializeRequest())

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.Code)
	}
	if transport.calls.Load() != 1 {
		t.Fatalf("RoundTrip calls = %d, want exactly one", transport.calls.Load())
	}
	if got := gateway.Snapshot(100).CapacityUsed; got != 1 {
		t.Fatalf("capacity used = %d, want quarantined reservation", got)
	}
}

func TestManagedHTTPGatewaySecurityRunsBeforeAdmission(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Mcp-Session-Id", "should-not-exist")
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	handler := SecurityMiddleware(SecurityConfig{BearerToken: "secret", AllowedOrigins: []string{"https://allowed.example"}})(gateway)

	for _, req := range []*http.Request{
		newInitializeRequest(),
		func() *http.Request {
			r := newInitializeRequest()
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("Origin", "https://denied.example")
			return r
		}(),
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized && resp.Code != http.StatusForbidden {
			t.Fatalf("security status = %d, want 401 or 403", resp.Code)
		}
	}

	if calls.Load() != 0 || gateway.Snapshot(100).CapacityUsed != 0 {
		t.Fatalf("security denial reached admission/backend: calls=%d capacity=%d", calls.Load(), gateway.Snapshot(100).CapacityUsed)
	}
}

func TestManagedHTTPGatewayPreservesSSEAndLastEventID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", "sse-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		if got := r.Header.Get("Last-Event-ID"); got != "event-7" {
			t.Fatalf("Last-Event-ID = %q, want event-7", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "id: event-8\ndata: ready\n\n")
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	id := initializeManagedSession(t, gateway)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", id)
	req.Header.Set("Last-Event-ID", "event-7")
	req.Header.Set("Accept", "text/event-stream")
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK || resp.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE response = %d %q", resp.Code, resp.Header().Get("Content-Type"))
	}
	if got := resp.Body.String(); got != "id: event-8\ndata: ready\n\n" {
		t.Fatalf("SSE body = %q", got)
	}
}

func TestManagedHTTPGatewayReadinessProbeInitializesAndDeletesOnce(t *testing.T) {
	var initializes atomic.Int32
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			initializes.Add(1)
			w.Header().Set("Mcp-Session-Id", "probe-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"vision-readiness","result":{}}`)
		case http.MethodDelete:
			deletes.Add(1)
			if got := r.Header.Get("Mcp-Session-Id"); got != "probe-session" {
				t.Fatalf("probe cleanup session = %q", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := ProbeManagedHTTPBackend(context.Background(), target, backend.Client().Transport); err != nil {
		t.Fatal(err)
	}
	if initializes.Load() != 1 || deletes.Load() != 1 {
		t.Fatalf("probe calls initialize=%d delete=%d, want one each", initializes.Load(), deletes.Load())
	}
}

type failingManagedTransport struct{ calls atomic.Int32 }

func (t *failingManagedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("response lost after dispatch")
}

func newTestManagedGateway(t *testing.T, target string, max int, transport http.RoundTripper) *ManagedHTTPGateway {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target:      u,
		MaxSessions: max,
		IdleTimeout: 30 * time.Minute,
		Transport:   transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func initializeManagedSession(t *testing.T, handler http.Handler) string {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, newInitializeRequest())
	if resp.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", resp.Code, resp.Body.String())
	}
	id := resp.Header().Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}
	return id
}

func newInitializeRequest() *http.Request {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"gateway-test","version":"1.0.0"}}}`
	return httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", bytes.NewBufferString(body))
}
