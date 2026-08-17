package integration

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/daemon"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
)

func TestManagedHTTPGatewayRealClientDisconnectReapsLease(t *testing.T) {
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "real-disconnect-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer backend.Close()

	const grace = 120 * time.Millisecond
	gateway := newRealClockManagedGateway(t, backend.URL+"/mcp", 1, time.Hour, grace)
	public := httptest.NewServer(gateway)
	defer public.Close()

	reaperCtx, stopReaper := context.WithCancel(context.Background())
	defer stopReaper()
	gateway.StartReaper(reaperCtx, 10*time.Millisecond)

	sessionID := initializeManagedGatewaySession(t, public.URL+"/mcp")
	conn := openManagedGatewaySSE(t, public.Listener.Addr().String(), sessionID)
	if got := gateway.Snapshot(10); len(got.Rows) != 1 || got.Rows[0].SSEConnections != 1 {
		t.Fatalf("connected SSE snapshot = %#v, want one active stream", got)
	}

	// Closing the real TCP connection, rather than issuing DELETE or merely
	// calling the handler directly, must cancel the server request context.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitForManagedGateway(t, gateway, 2*time.Second, func(snapshot mcp.LeaseSnapshot) bool {
		return len(snapshot.Rows) == 1 && snapshot.Rows[0].SSEConnections == 0
	})
	waitForManagedGateway(t, gateway, 2*time.Second, func(snapshot mcp.LeaseSnapshot) bool {
		if snapshot.CapacityUsed != 0 || len(snapshot.Closed) == 0 {
			return false
		}
		for _, row := range snapshot.Closed {
			if row.LifecycleReason == "client_disconnected" {
				return deletes.Load() == 1
			}
		}
		return false
	})
}

func TestManagedHTTPGatewayDelayedHandshakeSurvivesNeverStreamedBound(t *testing.T) {
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "delayed-handshake-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer backend.Close()

	// This configuration would derive a 500ms never-streamed bound from the
	// 100ms disconnect grace, but the enabled bound is raised to the 30s
	// handshake floor, so a 250ms real handshake delay is safely inside it.
	// The short grace still exercises the disconnect path promptly.
	const grace = 100 * time.Millisecond
	gateway := newRealClockManagedGateway(t, backend.URL+"/mcp", 1, 500*time.Millisecond, grace)
	public := httptest.NewServer(gateway)
	defer public.Close()
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	defer stopReaper()
	gateway.StartReaper(reaperCtx, 10*time.Millisecond)

	sessionID := initializeManagedGatewaySession(t, public.URL+"/mcp")
	time.Sleep(250 * time.Millisecond)
	snapshot := gateway.Snapshot(10)
	if snapshot.CapacityUsed != 1 || len(snapshot.Rows) != 1 || snapshot.Rows[0].State != mcp.LeaseStateActive || len(snapshot.Closed) != 0 {
		t.Fatalf("lease reaped during delayed handshake: %#v", snapshot)
	}

	conn := openManagedGatewaySSE(t, public.Listener.Addr().String(), sessionID)
	defer conn.Close()
	waitForManagedGateway(t, gateway, time.Second, func(snapshot mcp.LeaseSnapshot) bool {
		return snapshot.CapacityUsed == 1 && len(snapshot.Rows) == 1 && snapshot.Rows[0].SSEConnections == 1
	})
	if deletes.Load() != 0 {
		t.Fatalf("delayed handshake caused premature cleanup: deletes=%d", deletes.Load())
	}
}

func TestManagedPlaywrightPOSTStreamHeadersArePrompt(t *testing.T) {
	if os.Getenv("VISION_PLAYWRIGHT_REAL_TEST") != "1" {
		t.Skip("set VISION_PLAYWRIGHT_REAL_TEST=1 for pinned Playwright header verification")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Fatal("npx is required for real Playwright verification")
	}
	externalPort := freeVisionPort(t)
	internalPort := freeTCPPort(t)
	adminPort := freeTCPPort(t)

	configPath := filepath.Join(t.TempDir(), "servers.yaml")
	configBody := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s
servers:
  playwright-header-probe:
    transport: managed-http
    command: %q
    args: ["-y", "@playwright/mcp@%s", "--browser", "chromium", "--headless", "--isolated", "--host", "127.0.0.1", "--allowed-hosts", "127.0.0.1:%d", "--port", "%d"]
    url: "http://127.0.0.1:%d/mcp"
    port: %d
    autostart: true
    restart_policy: on-failure
    max_sessions: 1
    session_timeout: 2s
`, npx, realPlaywrightMCPVersion, internalPort, internalPort, internalPort, externalPort)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := daemon.New(daemon.Config{ConfigPath: configPath, ManagementPort: adminPort})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Stop(10 * time.Second); err != nil {
			t.Errorf("stop daemon: %v", err)
		}
	}()

	adminURL := fmt.Sprintf("http://127.0.0.1:%d/v1/servers/playwright-header-probe", adminPort)
	waitForLifecycle(t, adminURL, 45*time.Second, func(s lifecycleDetail) bool {
		return s.SessionLifecycle != nil && s.SessionLifecycle.BackendState == "ready"
	})

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", internalPort)

	const headerBound = time.Second
	transport := &http.Transport{ResponseHeaderTimeout: headerBound}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"vision-header-probe","version":"1.0.0"}}}`
	resp := postPlaywrightMCP(t, client, endpoint, initialize, "")
	sessionID := resp.Header.Get("Mcp-Session-Id")
	_ = resp.Body.Close()
	if sessionID == "" {
		t.Fatal("Playwright initialize response missing Mcp-Session-Id")
	}

	// A tool call with an SSE-capable Accept header is the POST-SSE path. The
	// body may remain open while the backend works, so only header arrival is
	// measured here. A buffered backend would hit headerBound before returning.
	notification := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	notificationResp := postPlaywrightMCP(t, client, endpoint, notification, sessionID)
	_ = notificationResp.Body.Close()
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"browser_wait_for","arguments":{"time":2}}}`
	started := time.Now()
	callResp := postPlaywrightMCP(t, client, endpoint, call, sessionID)
	elapsed := time.Since(started)
	_ = callResp.Body.Close()
	t.Logf("Playwright POST response headers arrived in %s (bound %s)", elapsed, headerBound)
	if elapsed >= headerBound/2 {
		t.Fatalf("Playwright POST response headers took %s, want well under %s", elapsed, headerBound)
	}
}

func newRealClockManagedGateway(t *testing.T, target string, maxSessions int, idleTimeout, grace time.Duration) *mcp.ManagedHTTPGateway {
	t.Helper()
	parsed, err := mcpTargetURL(target)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := mcp.NewManagedHTTPGateway(mcp.ManagedHTTPGatewayConfig{
		Target: parsed, MaxSessions: maxSessions, IdleTimeout: idleTimeout, DisconnectGracePeriod: grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func mcpTargetURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

func initializeManagedGatewaySession(t *testing.T, endpoint string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"integration","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize status=%d body=%s", resp.StatusCode, body)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}
	return sessionID
}

func openManagedGatewaySSE(t *testing.T, address, sessionID string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf("GET /mcp HTTP/1.1\r\nHost: %s\r\nMcp-Session-Id: %s\r\nAccept: text/event-stream\r\nConnection: keep-alive\r\n\r\n", address, sessionID)
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if !strings.Contains(status, " 200 ") {
		_ = conn.Close()
		t.Fatalf("SSE status line = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		if line == "\r\n" {
			return conn
		}
	}
}

func waitForManagedGateway(t *testing.T, gateway *mcp.ManagedHTTPGateway, timeout time.Duration, condition func(mcp.LeaseSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition(gateway.Snapshot(10)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("managed gateway condition timed out after %s", timeout)
}

func postPlaywrightMCP(t *testing.T, client *http.Client, endpoint, body, sessionID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("Playwright POST status=%d body=%s", resp.StatusCode, body)
	}
	return resp
}
