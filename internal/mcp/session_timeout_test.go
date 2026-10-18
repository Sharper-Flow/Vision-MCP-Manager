package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

func initializeRawSession(t *testing.T, client *http.Client, endpoint string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"timeout-test","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatalf("create initialize request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("initialize request failed: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response has no Mcp-Session-Id")
	}
	return sessionID
}

func sendSessionPost(t *testing.T, client *http.Client, endpoint, sessionID, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("session POST failed: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read session POST response: %v", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		t.Fatalf("session POST status = %d", resp.StatusCode)
	}
}

func openSessionSSE(t *testing.T, client *http.Client, endpoint, sessionID string) (context.CancelFunc, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		cancel()
		t.Fatalf("create GET request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)
	responses := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			responses <- nil
			return
		}
		responses <- resp
	}()
	select {
	case resp := <-responses:
		if resp == nil {
			cancel()
			t.Fatal("GET/SSE request failed")
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			t.Fatalf("GET/SSE status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		return cancel, resp
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("GET/SSE request did not establish")
		return nil, nil
	}
}

func newSharedTimeoutTestServer(t *testing.T, script string, timeout time.Duration) (*session.SharedSessionManager, *httptest.Server, func()) {
	t.Helper()
	cfg := testServerConfigWithScript(script)
	cfg.SessionTimeout = config.Duration(timeout)
	sm := session.NewSharedSessionManager("shared-timeout", cfg, testLogger(t), 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	handler := NewProxyHandler(ProxyConfig{
		ServerName:    "shared-timeout",
		SharedManager: sm,
		Logger:        testLogger(t),
	})
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		cancel()
		server.Close()
		sm.CloseAll()
	})
	return sm, server, func() { sm.StartReaper(ctx, 10*time.Millisecond) }
}

func TestSharedSessionTimeout_EndToEndProtocolTrafficDoesNotRefresh(t *testing.T) {
	skipIfNoNode(t)
	sm, server, startReaper := newSharedTimeoutTestServer(t, echoMCPServerJS, 500*time.Millisecond)
	startReaper()
	client := server.Client()
	sessionID := initializeRawSession(t, client, server.URL)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	time.Sleep(250 * time.Millisecond)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	cancelSSE, sseResp := openSessionSSE(t, client, server.URL, sessionID)
	defer cancelSSE()
	defer sseResp.Body.Close()

	time.Sleep(300 * time.Millisecond)
	if got := sm.SessionCount(); got != 0 {
		t.Fatalf("protocol traffic kept shared session alive, count = %d", got)
	}
}

func TestSharedSessionTimeout_EndToEndApplicationPostRefreshes(t *testing.T) {
	skipIfNoNode(t)
	sm, server, startReaper := newSharedTimeoutTestServer(t, echoMCPServerJS, 500*time.Millisecond)
	startReaper()
	client := server.Client()
	sessionID := initializeRawSession(t, client, server.URL)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	time.Sleep(250 * time.Millisecond)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	time.Sleep(150 * time.Millisecond)
	if got := sm.SessionCount(); got != 1 {
		t.Fatalf("application POST did not refresh shared session, count = %d", got)
	}

	time.Sleep(400 * time.Millisecond)
	if got := sm.SessionCount(); got != 0 {
		t.Fatalf("refreshed shared session did not expire, count = %d", got)
	}
}

func TestSharedSessionTimeout_EndToEndInflightApplicationCallDelaysReap(t *testing.T) {
	skipIfNoNode(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseCall := func() { once.Do(func() { close(release) }) }
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	defer gate.Close()
	defer releaseCall()
	script := strings.Replace(echoMCPServerJS, "rl.on('line', (line) => {", "rl.on('line', async (line) => {", 1)
	script = strings.Replace(script, "} else if (msg.method === 'tools/call') {",
		fmt.Sprintf("} else if (msg.method === 'tools/call') { await fetch(%q);", gate.URL), 1)
	sm, server, startReaper := newSharedTimeoutTestServer(t, script, 100*time.Millisecond)
	client := server.Client()
	client.Timeout = 5 * time.Second
	sessionID := initializeRawSession(t, client, server.URL)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	idleID := initializeRawSession(t, client, server.URL)
	sendSessionPost(t, client, server.URL, idleID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	result := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"slow"}}}`))
		if err != nil {
			result <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Session-Id", sessionID)
		resp, err := client.Do(req)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil && (resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "echo: slow")) {
				err = fmt.Errorf("slow call status = %d, body = %q", resp.StatusCode, body)
			}
		}
		result <- err
	}()

	deadline := time.After(5 * time.Second)
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("call completed before reaching the gate: %v", err)
	case <-deadline:
		t.Fatal("application call did not reach the gate")
	}
	// The idle control proves the reaper actually ran past the lease while
	// the downstream call stayed blocked. Startup time cannot consume it.
	startReaper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	waitCount := func(want int) {
		t.Helper()
		for sm.SessionCount() != want {
			select {
			case <-ticker.C:
			case <-deadline:
				t.Fatalf("session count = %d, want %d", sm.SessionCount(), want)
			}
		}
	}
	waitCount(1)
	status, body := postSessionStatus(t, client, server.URL, idleID,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if status != http.StatusNotFound {
		t.Fatalf("idle control status = %d, body = %q; want 404", status, body)
	}
	select {
	case err := <-result:
		t.Fatalf("call completed before release: %v", err)
	default:
	}
	releaseCall()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("slow application call failed: %v", err)
		}
	case <-deadline:
		t.Fatal("application call did not complete after release")
	}
	waitCount(0)
}

// postSessionStatus sends one POST on sessionID and returns the HTTP status and body.
func postSessionStatus(t *testing.T, client *http.Client, endpoint, sessionID, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("session POST failed: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read session POST response: %v", err)
	}
	return resp.StatusCode, string(data)
}

// A reaped shared-mode session must be terminated upstream. The MCP
// streamable HTTP transport requires HTTP 404 for a terminated session ID,
// which tells the client to initialize a new session. An in-band tool error
// on a session that can never recover leaves the client stuck.
func TestSharedSessionTimeout_ReapedSessionReturnsNotFound(t *testing.T) {
	skipIfNoNode(t)
	sm, server, startReaper := newSharedTimeoutTestServer(t, echoMCPServerJS, 300*time.Millisecond)
	startReaper()
	client := server.Client()
	sessionID := initializeRawSession(t, client, server.URL)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	// Refresh activity after initialization so the reap cannot precede the
	// proxy session's registration, even when the subprocess spawn is slow.
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	deadline := time.Now().Add(3 * time.Second)
	for sm.SessionCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("shared session was not reaped, count = %d", sm.SessionCount())
		}
		time.Sleep(10 * time.Millisecond)
	}

	var status int
	var body string
	for {
		status, body = postSessionStatus(t, client, server.URL, sessionID,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"after-reap"}}}`)
		if status == http.StatusNotFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != http.StatusNotFound {
		t.Fatalf("call on reaped session: status = %d, body = %q; want %d", status, body, http.StatusNotFound)
	}

	newID := initializeRawSession(t, client, server.URL)
	if newID == sessionID {
		t.Fatal("re-initialize reused the terminated session ID")
	}
	sendSessionPost(t, client, server.URL, newID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	status, body = postSessionStatus(t, client, server.URL, newID,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"message":"fresh"}}}`)
	if status != http.StatusOK || !strings.Contains(body, "fresh") {
		t.Fatalf("call on re-initialized session: status = %d, body = %q", status, body)
	}
}

func TestStatefulSessionTimeout_EndToEndPingDoesNotRefresh(t *testing.T) {
	skipIfNoNode(t)
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(500 * time.Millisecond)
	mgr := session.NewManager("stateful-timeout", cfg, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	mgr.StartReaper(ctx, 10*time.Millisecond)
	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "stateful-timeout",
		SessionManager: mgr,
		Logger:         testLogger(t),
	})
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		cancel()
		server.Close()
		mgr.CloseAll()
	})

	client := server.Client()
	sessionID := initializeRawSession(t, client, server.URL)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(250 * time.Millisecond)
	sendSessionPost(t, client, server.URL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	time.Sleep(300 * time.Millisecond)
	if got := mgr.SessionCount(); got != 0 {
		t.Fatalf("stateful ping refreshed session, count = %d", got)
	}
}
