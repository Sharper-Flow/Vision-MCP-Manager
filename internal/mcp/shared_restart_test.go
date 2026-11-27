package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sharedRestartFixtureJS answers initialize and tools/list, and holds every
// tools/call until the test releases it over the control connection. Each call
// reports itself on a new control connection first.
const sharedRestartFixtureJS = `
const net = require('net');
const rl = require('readline').createInterface({input: process.stdin});
rl.on('close', () => process.exit(0));
const reply = (id, result) => process.stdout.write(JSON.stringify({jsonrpc: '2.0', id, result}) + '\n');
rl.on('line', line => {
  const m = JSON.parse(line);
  if (m.method === 'initialize') {
    reply(m.id, {protocolVersion: '2025-03-26', capabilities: {tools: {}}, serverInfo: {name: 'restart-fixture', version: '1'}});
  } else if (m.method === 'tools/list') {
    reply(m.id, {tools: [{name: 'held', inputSchema: {type: 'object'}}]});
  } else if (m.method === 'tools/call') {
    const control = net.createConnection({host: '127.0.0.1', port: Number(process.env.CONTROL_PORT)}, () => control.write('call\n'));
    control.once('data', () => { reply(m.id, {content: [{type: 'text', text: 'completed'}]}); control.end(); });
  } else if (m.id !== undefined) {
    reply(m.id, {});
  }
});
`

const sharedRestartWait = 5 * time.Second

func sharedRestartRefs(refs ...SharedRestartRef) []SharedRestartRef {
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func TestSharedRestartGateHoldsSequentialRequestsUntilReleased(t *testing.T) {
	gate := NewSharedRestartGate()
	var releases [3]func()
	for i := range releases {
		ref, release, err := gate.enter()
		if err != nil {
			t.Fatalf("enter %d: %v", i, err)
		}
		if ref != SharedRestartRef(i+1) {
			t.Errorf("request %d reference = %d, want %d", i, ref, i+1)
		}
		releases[i] = release
	}
	refs, err := gate.WaitDrained(context.Background(), 0)
	if !errors.Is(err, ErrSharedRestartDrainTimeout) || !slices.Equal(refs, sharedRestartRefs(1, 2, 3)) {
		t.Fatalf("wait with three requests held = %v, %v; want timeout with 1 2 3", refs, err)
	}

	if err := gate.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := gate.Close(); !errors.Is(err, ErrSharedRestartInProgress) {
		t.Errorf("second Close = %v, want ErrSharedRestartInProgress", err)
	}
	if _, _, err := gate.enter(); !errors.Is(err, ErrSharedRestartClosed) {
		t.Errorf("enter after Close = %v, want ErrSharedRestartClosed", err)
	}

	releases[1]()
	releases[1]()
	refs, err = gate.WaitDrained(context.Background(), 0)
	if !errors.Is(err, ErrSharedRestartDrainTimeout) || !slices.Equal(refs, sharedRestartRefs(1, 3)) {
		t.Fatalf("wait after one release = %v, %v; want timeout with 1 3", refs, err)
	}
	releases[0]()
	releases[2]()
	if refs, err := gate.WaitDrained(context.Background(), 0); err != nil || refs != nil {
		t.Fatalf("wait after every release = %v, %v; want drained", refs, err)
	}
	if _, _, err := gate.enter(); !errors.Is(err, ErrSharedRestartClosed) {
		t.Errorf("enter after drain completed = %v, want ErrSharedRestartClosed: a completed drain must not reopen admission", err)
	}

	gate.Abort()
	gate.Abort()
	ref, release, err := gate.enter()
	if err != nil {
		t.Fatalf("enter after Abort: %v", err)
	}
	if ref != 4 {
		t.Errorf("reference after Abort = %d, want 4", ref)
	}
	release()
}

func TestSharedRestartGateConcurrentRestartsHaveOneOwner(t *testing.T) {
	gate := NewSharedRestartGate()
	const restarts = 16
	start := make(chan struct{})
	results := make(chan error, restarts)
	for range restarts {
		go func() {
			<-start
			results <- gate.Close()
		}()
	}
	close(start)
	owners := 0
	for range restarts {
		switch err := <-results; {
		case err == nil:
			owners++
		case !errors.Is(err, ErrSharedRestartInProgress):
			t.Errorf("concurrent Close = %v, want nil or ErrSharedRestartInProgress", err)
		}
	}
	if owners != 1 {
		t.Fatalf("concurrent restarts accepted %d owners, want 1", owners)
	}
}

func TestSharedRestartGateDrainWaitEndsWithLastRelease(t *testing.T) {
	gate := NewSharedRestartGate()
	_, releaseFirst, err := gate.enter()
	if err != nil {
		t.Fatal(err)
	}
	_, releaseLast, err := gate.enter()
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedRestartWait)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := gate.WaitDrained(ctx, time.Minute)
		done <- err
	}()

	releaseFirst()
	select {
	case err := <-done:
		t.Fatalf("drain ended while one request was still held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseLast()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drain after last release = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("drain did not end after the last release")
	}
}

func TestSharedRestartGateDrainWaitEndsOnCallerCancellation(t *testing.T) {
	gate := NewSharedRestartGate()
	_, release, err := gate.enter()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		refs, err := gate.WaitDrained(ctx, time.Minute)
		if !slices.Equal(refs, sharedRestartRefs(1)) {
			t.Errorf("held references at cancellation = %v, want 1", refs)
		}
		done <- err
	}()
	// The wait must be blocked on the caller's context before it is cancelled.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("drain wait after caller cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(sharedRestartWait):
		t.Fatal("caller cancellation did not end the drain wait")
	}
	if _, _, err := gate.enter(); !errors.Is(err, ErrSharedRestartClosed) {
		t.Errorf("enter after cancelled drain wait = %v, want ErrSharedRestartClosed: a cancelled wait must not reopen admission", err)
	}
}

func TestSharedRestartGateDrainWaitEndsOnTimeout(t *testing.T) {
	gate := NewSharedRestartGate()
	_, release, err := gate.enter()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	const timeout = 40 * time.Millisecond
	started := time.Now()
	refs, err := gate.WaitDrained(context.Background(), timeout)
	if elapsed := time.Since(started); elapsed < timeout {
		t.Fatalf("drain wait returned after %v, before its %v duration", elapsed, timeout)
	}
	if !errors.Is(err, ErrSharedRestartDrainTimeout) || !slices.Equal(refs, sharedRestartRefs(1)) {
		t.Fatalf("drain wait at timeout = %v, %v; want 1, ErrSharedRestartDrainTimeout", refs, err)
	}
}

func TestSharedRestartGateCancelPendingCancelsForwardingContexts(t *testing.T) {
	gate := NewSharedRestartGate()
	if refs := gate.CancelPending(); refs != nil {
		t.Fatalf("CancelPending on an open gate returned %v, want none", refs)
	}
	_, release, err := gate.enter()
	if err != nil {
		t.Fatal(err)
	}
	forwarding, endForwarding := gate.bindCutover(context.Background())
	defer endForwarding()
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}

	if refs := gate.CancelPending(); !slices.Equal(refs, sharedRestartRefs(1)) {
		t.Fatalf("CancelPending returned %v, want 1", refs)
	}
	select {
	case <-forwarding.Done():
		if !errors.Is(forwarding.Err(), context.Canceled) {
			t.Fatalf("forwarding context ended with %v, want context.Canceled", forwarding.Err())
		}
	case <-time.After(sharedRestartWait):
		t.Fatal("CancelPending did not end a forwarding context bound before the commit")
	}
	late, endLate := gate.bindCutover(context.Background())
	if !errors.Is(late.Err(), context.Canceled) {
		t.Fatalf("forwarding context bound after the commit has error %v, want context.Canceled at once", late.Err())
	}
	endLate()
	if refs := gate.CancelPending(); !slices.Equal(refs, sharedRestartRefs(1)) {
		t.Errorf("repeated CancelPending returned %v, want the same held reference 1", refs)
	}

	release()
	if refs, err := gate.WaitDrained(context.Background(), 0); err != nil || refs != nil {
		t.Fatalf("wait after the held request ended = %v, %v, want drained", refs, err)
	}
	gate.Abort()
	fresh, endFresh := gate.bindCutover(context.Background())
	defer endFresh()
	if refs := gate.CancelPending(); refs != nil {
		t.Errorf("CancelPending on a reopened gate returned %v, want none", refs)
	}
	select {
	case <-fresh.Done():
		t.Fatalf("forwarding context after Abort ended with %v, want it to stay open", fresh.Err())
	case <-time.After(20 * time.Millisecond):
	}
	if ref, release, err := gate.enter(); err != nil || ref != 2 {
		t.Errorf("enter after Abort = %d, %v, want reference 2", ref, err)
	} else {
		release()
	}
}

func TestSharedRestartAbortDoesNotCancelHeldForwarding(t *testing.T) {
	gate := NewSharedRestartGate()
	_, release, err := gate.enter()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	forwarding, cancel := gate.bindCutover(context.Background())
	defer cancel()
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	gate.Abort()
	select {
	case <-forwarding.Done():
		t.Fatalf("aborted restart cancelled a held forwarding context: %v", forwarding.Err())
	default:
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	gate.CancelPending()
	// A later cutover must also cancel requests admitted before the abort.
	select {
	case <-forwarding.Done():
	case <-time.After(sharedRestartWait):
		t.Fatal("later cutover did not cancel a request held across abort")
	}
}

// sharedRestartProxy returns a shared proxy with an injected restart gate and
// the channel on which the fixture reports each downstream tools/call.
func sharedRestartProxy(t *testing.T, name string) (*SharedRestartGate, *session.SharedSessionManager, *metrics.ServerMetrics, chan net.Conn, http.Handler) {
	t.Helper()
	skipIfNoNode(t)
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	conns := make(chan net.Conn, 16)
	go func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			conns <- conn
		}
	}()
	cfg := testServerConfigWithScript(sharedRestartFixtureJS)
	cfg.Env = map[string]string{"CONTROL_PORT": fmt.Sprint(control.Addr().(*net.TCPAddr).Port)}
	owner := metrics.NewServerMetrics()
	sm := session.NewSharedSessionManager(name, cfg, testLogger(t), 0, owner)
	t.Cleanup(sm.CloseAll)
	gate := NewSharedRestartGate()
	handler := NewProxyHandler(ProxyConfig{ServerName: name, SharedManager: sm, SharedRestartGate: gate, Logger: testLogger(t), Metrics: owner})
	return gate, sm, owner, conns, handler
}

func sharedRestartRequest(t *testing.T, method, sessionID, accept, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	return req
}

const sharedRestartInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"restart-test","version":"1"}}}`

func TestSharedRestartProxyRefusesApplicationRequestsWhileClosed(t *testing.T) {
	gate, sm, _, conns, handler := sharedRestartProxy(t, "restart-refusal")
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	assertRestartRefusal := func(w *httptest.ResponseRecorder, id float64) {
		t.Helper()
		var refused struct {
			ID    float64 `json:"id"`
			Error struct {
				Code    int `json:"code"`
				Message string
				Data    struct {
					Type      string `json:"type"`
					Reason    string `json:"reason"`
					Retryable bool   `json:"retryable"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil {
			t.Fatalf("refusal is not a JSON-RPC envelope: %d %s", w.Code, w.Body.String())
		}
		if w.Code != http.StatusServiceUnavailable || w.Header().Get(gatewayDenialHeader) != "shared-restarting" || w.Header().Get("Retry-After") != "1" {
			t.Errorf("refusal = %d %v; want 503 with gateway denial shared-restarting and Retry-After 1", w.Code, w.Header())
		}
		if refused.ID != id || refused.Error.Code != -32000 || refused.Error.Data.Type != "restarting" || refused.Error.Data.Reason != "restart_in_progress" || !refused.Error.Data.Retryable {
			t.Errorf("refusal envelope = %+v; want id %v, code -32000, restarting, retryable", refused, id)
		}
	}

	// A refused initialization creates no session and no downstream.
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	assertRestartRefusal(serve(sharedRestartRequest(t, http.MethodPost, "", "application/json, text/event-stream", sharedRestartInitialize)), 1)
	if sm.RefCount() != 0 {
		t.Errorf("refused initialize reached the shared session: refcount %d", sm.RefCount())
	}
	select {
	case conn := <-conns:
		conn.Close()
		t.Error("refused initialize reached the downstream")
	default:
	}

	gate.Abort()
	initialized := serve(sharedRestartRequest(t, http.MethodPost, "", "application/json, text/event-stream", sharedRestartInitialize))
	sessionID := initialized.Header().Get("Mcp-Session-Id")
	if initialized.Code != http.StatusOK || sessionID == "" {
		t.Fatalf("initialize after Abort = %d %v; want 200 with a session", initialized.Code, initialized.Header())
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	assertRestartRefusal(serve(sharedRestartRequest(t, http.MethodPost, sessionID, "application/json, text/event-stream", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"held","arguments":{}}}`)), 2)
	select {
	case conn := <-conns:
		conn.Close()
		t.Error("refused tools/call reached the downstream")
	default:
	}

	// Only application requests are held. Each request below creates no
	// downstream work or belongs to an established session, so the closed gate
	// lets it through to the SDK.
	assertNotRefused := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Header().Get(gatewayDenialHeader) == "shared-restarting" || w.Code == http.StatusServiceUnavailable {
			t.Errorf("%s while closed = %d %v; want it to pass the restart gate", name, w.Code, w.Header())
		}
	}
	assertNotRefused("ping", serve(sharedRestartRequest(t, http.MethodPost, sessionID, "application/json, text/event-stream", `{"jsonrpc":"2.0","id":3,"method":"ping"}`)))
	assertNotRefused("notification", serve(sharedRestartRequest(t, http.MethodPost, sessionID, "application/json, text/event-stream", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	assertNotRefused("response", serve(sharedRestartRequest(t, http.MethodPost, sessionID, "application/json, text/event-stream", `{"jsonrpc":"2.0","id":4,"result":{}}`)))
	assertNotRefused("unclassifiable body", serve(sharedRestartRequest(t, http.MethodPost, sessionID, "application/json, text/event-stream", `{"jsonrpc"`)))

	ctx, cancel := context.WithCancel(context.Background())
	get := sharedRestartRequest(t, http.MethodGet, sessionID, "text/event-stream", "")
	get = get.WithContext(ctx)
	streamed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, get)
		streamed <- w
	}()
	cancel()
	select {
	case w := <-streamed:
		assertNotRefused("GET/SSE", w)
	case <-time.After(sharedRestartWait):
		t.Fatal("GET/SSE did not return after the caller left")
	}
	assertNotRefused("DELETE", serve(sharedRestartRequest(t, http.MethodDelete, sessionID, "", "")))
}

func TestSharedRestartGateDoesNotAffectNonSharedProxy(t *testing.T) {
	gate := NewSharedRestartGate()
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager("non-shared", testServerConfig(), testLogger(t))
	handler := NewProxyHandler(ProxyConfig{
		ServerName: "non-shared", SessionManager: mgr, SharedRestartGate: gate,
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, sharedRestartRequest(t, http.MethodPost, "unknown", "application/json, text/event-stream",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if w.Header().Get(gatewayDenialHeader) == "shared-restarting" || w.Code == http.StatusServiceUnavailable {
		t.Fatalf("non-shared proxy consulted restart gate: %d %s", w.Code, w.Body.String())
	}
}

func TestSharedRestartProxyDrainSpansInFlightCall(t *testing.T) {
	gate, _, _, conns, handler := sharedRestartProxy(t, "restart-drain")
	ts := httptest.NewServer(handler)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "restart-test", Version: "1"}, nil)
	upstream, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer upstream.Close()

	awaitDownstreamCall := func(step string) net.Conn {
		t.Helper()
		select {
		case conn := <-conns:
			t.Cleanup(func() { conn.Close() })
			if err := conn.SetReadDeadline(time.Now().Add(sharedRestartWait)); err != nil {
				t.Fatal(err)
			}
			if reported, err := bufio.NewReader(conn).ReadString('\n'); err != nil || reported != "call\n" {
				t.Fatalf("%s: downstream report = %q, %v; want call", step, reported, err)
			}
			return conn
		case <-time.After(sharedRestartWait):
			t.Fatalf("%s: downstream did not receive tools/call", step)
			return nil
		}
	}
	assertNoDownstreamCall := func(step string) {
		t.Helper()
		select {
		case conn := <-conns:
			conn.Close()
			t.Fatalf("%s: unexpected downstream tools/call", step)
		case <-time.After(150 * time.Millisecond):
		}
	}
	call := func() chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			result, err := upstream.CallTool(ctx, &mcp.CallToolParams{Name: "held"})
			if err == nil && (result == nil || result.IsError || len(result.Content) != 1) {
				err = fmt.Errorf("unexpected held result %+v", result)
			}
			done <- err
		}()
		return done
	}

	// The drain waits for an accepted call until its reply is written.
	first := call()
	firstConn := awaitDownstreamCall("in-flight call")
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	if refs, err := gate.WaitDrained(ctx, 40*time.Millisecond); !errors.Is(err, ErrSharedRestartDrainTimeout) || !slices.Equal(refs, sharedRestartRefs(2)) {
		t.Fatalf("drain with the call in flight = %v, %v; want timeout holding 2", refs, err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"held","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", upstream.ID())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(gatewayDenialHeader) != "shared-restarting" {
		t.Errorf("tools/call while draining = %d %v; want 503 shared-restarting before dispatch", resp.StatusCode, resp.Header)
	}
	assertNoDownstreamCall("tools/call refused before dispatch")
	select {
	case err := <-first:
		t.Fatalf("in-flight call ended before its reply was released: %v", err)
	default:
	}

	if _, err := fmt.Fprintln(firstConn, "release"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("drained call = %v, want its reply delivered", err)
		}
	case <-ctx.Done():
		t.Fatal("drained call did not complete")
	}
	if refs, err := gate.WaitDrained(ctx, sharedRestartWait); err != nil || refs != nil {
		t.Fatalf("drain after the reply was written = %v, %v, want drained", refs, err)
	}

	// At the committed deadline the cutover ends the forwarding context. The
	// caller gets an error; the downstream call is neither replayed nor killed.
	gate.Abort()
	second := call()
	awaitDownstreamCall("call to cancel")
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	if refs, err := gate.WaitDrained(ctx, 20*time.Millisecond); !errors.Is(err, ErrSharedRestartDrainTimeout) || !slices.Equal(refs, sharedRestartRefs(3)) {
		t.Fatalf("drain before the cutover = %v, %v; want timeout holding 3", refs, err)
	}
	if refs := gate.CancelPending(); !slices.Equal(refs, sharedRestartRefs(3)) {
		t.Fatalf("CancelPending returned %v, want 3", refs)
	}
	select {
	case err := <-second:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("cancelled call = %v, want a context canceled error", err)
		}
	case <-ctx.Done():
		t.Fatal("cutover did not end the call still held at the deadline")
	}
	assertNoDownstreamCall("replay after the cutover")
	if refs, err := gate.WaitDrained(ctx, sharedRestartWait); err != nil || refs != nil {
		t.Fatalf("drain after the cutover = %v, %v, want drained", refs, err)
	}

	gate.Abort()
	third := call()
	thirdConn := awaitDownstreamCall("call after the cutover")
	if _, err := fmt.Fprintln(thirdConn, "release"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("call after the cutover = %v, want the downstream still serving", err)
		}
	case <-ctx.Done():
		t.Fatal("call after the cutover did not complete")
	}
}
