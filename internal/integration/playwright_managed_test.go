package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/daemon"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const realPlaywrightMCPVersion = "0.0.77"

// TestManagedPlaywrightNativeHTTP verifies the installed Playwright MCP rather
// than a fake backend. It is opt-in because it requires npx, a matching cached
// browser, and roughly one minute of local browser process time.
func TestManagedPlaywrightNativeHTTP(t *testing.T) {
	if os.Getenv("VISION_PLAYWRIGHT_REAL_TEST") != "1" {
		t.Skip("set VISION_PLAYWRIGHT_REAL_TEST=1 for pinned real Playwright verification")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Fatal("npx is required for real Playwright verification")
	}
	externalPort := freeVisionPort(t)
	internalPort := freeTCPPort(t)
	adminPort := freeTCPPort(t)

	pageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := html.EscapeString(r.URL.Query().Get("owner"))
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><body data-owner=%q>%s</body></html>`, owner, owner)
	}))
	defer pageServer.Close()

	configPath := filepath.Join(t.TempDir(), "servers.yaml")
	configBody := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s
servers:
  playwright-real:
    transport: managed-http
    command: %q
    args: ["-y", "@playwright/mcp@%s", "--browser", "chromium", "--headless", "--isolated", "--host", "127.0.0.1", "--allowed-hosts", "127.0.0.1:%d", "--port", "%d"]
    url: "http://127.0.0.1:%d/mcp"
    port: %d
    autostart: true
    restart_policy: on-failure
    max_sessions: 6
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

	adminURL := fmt.Sprintf("http://127.0.0.1:%d/v1/servers/playwright-real", adminPort)
	waitForLifecycle(t, adminURL, 45*time.Second, func(s lifecycleDetail) bool {
		return s.SessionLifecycle != nil && s.SessionLifecycle.BackendState == "ready"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", externalPort)
	clientA, sessionA := connectPlaywright(t, ctx, endpoint, "real-a")
	clientB, sessionB := connectPlaywright(t, ctx, endpoint, "real-b")
	_ = clientA
	_ = clientB

	for i := 0; i < 10; i++ {
		markerA := fmt.Sprintf("agent-a-%02d", i)
		markerB := fmt.Sprintf("agent-b-%02d", i)
		assertIsolatedRound(t, ctx, sessionA, pageServer.URL, markerA, markerB)
		assertIsolatedRound(t, ctx, sessionB, pageServer.URL, markerB, markerA)
	}

	// Capacity is six: keep A/B, add four, and prove seventh admission fails
	// without disturbing established sessions.
	extra := make([]*sdkmcp.ClientSession, 0, 4)
	for i := 0; i < 4; i++ {
		_, session := connectPlaywright(t, ctx, endpoint, fmt.Sprintf("capacity-%d", i))
		extra = append(extra, session)
	}
	seventh := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "capacity-denied", Version: "1"}, nil)
	if session, err := seventh.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: endpoint}, nil); err == nil {
		session.Close()
		t.Fatal("seventh Playwright session unexpectedly admitted")
	}
	callPlaywright(t, ctx, sessionA, "browser_snapshot", nil)
	callPlaywright(t, ctx, sessionB, "browser_snapshot", nil)
	for _, session := range extra {
		session.Close()
	}
	sessionA.Close()
	sessionB.Close()
	waitForLifecycle(t, adminURL, 15*time.Second, func(s lifecycleDetail) bool {
		return s.SessionLifecycle != nil && s.SessionLifecycle.CapacityUsed == 0
	})

	// A request crossing the scaled two-second idle boundary remains attached.
	_, inflight := connectPlaywright(t, ctx, endpoint, "inflight")
	callDone := make(chan error, 1)
	go func() {
		_, err := inflight.CallTool(ctx, &sdkmcp.CallToolParams{Name: "browser_wait_for", Arguments: map[string]any{"time": 4}})
		callDone <- err
	}()
	waitForLifecycle(t, adminURL, 10*time.Second, func(s lifecycleDetail) bool {
		return s.SessionLifecycle != nil && len(s.SessionLifecycle.Sessions) == 1 && s.SessionLifecycle.Sessions[0].InFlight == 1
	})
	time.Sleep(3 * time.Second)
	state := getLifecycle(t, adminURL)
	if state.SessionLifecycle == nil || state.SessionLifecycle.CapacityUsed != 1 {
		t.Fatalf("in-flight session expired early: %#v", state.SessionLifecycle)
	}
	if err := <-callDone; err != nil {
		t.Fatalf("browser_wait_for crossing idle boundary: %v", err)
	}
	inflight.Close()

	// Transport can remain connected while application-idle expiry reclaims the
	// context. Reuse of the stale session must fail before a browser action.
	_, idle := connectPlaywright(t, ctx, endpoint, "idle-with-transport")
	waitForLifecycle(t, adminURL, 15*time.Second, func(s lifecycleDetail) bool {
		return s.SessionLifecycle != nil && s.SessionLifecycle.CapacityUsed == 0
	})
	if _, err := idle.CallTool(ctx, &sdkmcp.CallToolParams{Name: "browser_snapshot", Arguments: map[string]any{}}); err == nil {
		t.Fatal("expired real Playwright session unexpectedly remained usable")
	}
	idle.Close()

	// Forced process loss invalidates old identities and gates new dispatch until
	// the replacement passes initialize/delete readiness.
	_, old := connectPlaywright(t, ctx, endpoint, "pre-restart")
	managed := d.Registry().Get("playwright-real")
	if managed == nil || managed.PID() == 0 {
		t.Fatal("managed Playwright PID unavailable")
	}
	beforeRestart := managed.Process.RestartCount()
	if err := syscall.Kill(managed.PID(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForLifecycle(t, adminURL, 30*time.Second, func(s lifecycleDetail) bool {
		return s.RestartCount > beforeRestart && s.SessionLifecycle != nil && s.SessionLifecycle.BackendState == "ready"
	})
	if _, err := old.CallTool(ctx, &sdkmcp.CallToolParams{Name: "browser_snapshot", Arguments: map[string]any{}}); err == nil {
		t.Fatal("pre-restart session unexpectedly survived process loss")
	}
	old.Close()
	_, replacement := connectPlaywright(t, ctx, endpoint, "post-restart")
	callPlaywright(t, ctx, replacement, "browser_navigate", map[string]any{"url": pageServer.URL + "/?owner=replacement"})
	replacement.Close()

	final := getLifecycle(t, adminURL)
	if final.SessionLifecycle == nil || len(final.SessionLifecycle.Closed) == 0 {
		t.Fatalf("missing closed lifecycle diagnostics: %#v", final.SessionLifecycle)
	}
	encoded, _ := json.Marshal(final.SessionLifecycle)
	if strings.Contains(string(encoded), "Mcp-Session-Id") || strings.Contains(string(encoded), "raw-private") {
		t.Fatalf("unsafe lifecycle diagnostics: %s", encoded)
	}
	t.Logf("verified @playwright/mcp=%s sessions_closed=%d restart_count=%d", realPlaywrightMCPVersion, len(final.SessionLifecycle.Closed), final.RestartCount)
}

func TestPlaywrightStatefulStdioRollback(t *testing.T) {
	if os.Getenv("VISION_PLAYWRIGHT_REAL_TEST") != "1" {
		t.Skip("set VISION_PLAYWRIGHT_REAL_TEST=1 for pinned rollback rehearsal")
	}
	started := time.Now()
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Fatal(err)
	}
	externalPort := freeVisionPort(t)
	adminPort := freeTCPPort(t)
	pageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<!doctype html><body>rollback-ready</body>")
	}))
	defer pageServer.Close()

	configPath := filepath.Join(t.TempDir(), "rollback-servers.yaml")
	configBody := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
servers:
  playwright-real:
    port: %d
    transport: stdio
    command: %q
    args: ["-y", "@playwright/mcp@%s", "--browser", "chromium", "--headless", "--isolated"]
    stateful: true
    max_sessions: 6
    session_timeout: 30m
    autostart: true
`, externalPort, npx, realPlaywrightMCPVersion)
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
	defer func() { _ = d.Stop(10 * time.Second) }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", externalPort)
	_, session := connectPlaywright(t, ctx, endpoint, "rollback-rehearsal")
	callPlaywright(t, ctx, session, "browser_navigate", map[string]any{"url": pageServer.URL})
	result := callPlaywright(t, ctx, session, "browser_snapshot", map[string]any{})
	if !strings.Contains(result, "rollback-ready") {
		t.Fatalf("rollback browser result missing marker: %s", result)
	}
	session.Close()
	if elapsed := time.Since(started); elapsed >= 10*time.Minute {
		t.Fatalf("rollback rehearsal exceeded 10 minutes: %s", elapsed)
	} else {
		t.Logf("stateful-stdio rollback rehearsal ready in %s", elapsed.Round(time.Millisecond))
	}
}

type lifecycleDetail struct {
	RestartCount     int `json:"restart_count"`
	SessionLifecycle *struct {
		BackendState string `json:"backend_state"`
		CapacityUsed int    `json:"capacity_used"`
		Sessions     []struct {
			InFlight int `json:"in_flight"`
		} `json:"sessions"`
		Closed []map[string]any `json:"closed"`
	} `json:"session_lifecycle"`
}

func connectPlaywright(t *testing.T, ctx context.Context, endpoint, name string) (*sdkmcp.Client, *sdkmcp.ClientSession) {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: name, Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("list tools %s: tools=%d err=%v", name, len(tools.Tools), err)
	}
	return client, session
}

func assertIsolatedRound(t *testing.T, ctx context.Context, session *sdkmcp.ClientSession, pageURL, own, other string) {
	t.Helper()
	callPlaywright(t, ctx, session, "browser_navigate", map[string]any{"url": pageURL + "/?owner=" + own})
	fn := fmt.Sprintf(`() => { document.cookie = %q; window.__visionOwner = %q; document.body.textContent = %q; return JSON.stringify({cookie: document.cookie, state: window.__visionOwner, text: document.body.textContent}); }`, "vision_owner="+own+"; path=/", own, own)
	evaluated := callPlaywright(t, ctx, session, "browser_evaluate", map[string]any{"function": fn})
	if !strings.Contains(evaluated, own) || strings.Contains(evaluated, other) {
		t.Fatalf("evaluate isolation own=%q other=%q result=%s", own, other, evaluated)
	}
	snapshot := callPlaywright(t, ctx, session, "browser_snapshot", nil)
	if !strings.Contains(snapshot, own) || strings.Contains(snapshot, other) {
		t.Fatalf("snapshot isolation own=%q other=%q result=%s", own, other, snapshot)
	}
}

func callPlaywright(t *testing.T, ctx context.Context, session *sdkmcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if value, ok := content.(*sdkmcp.TextContent); ok {
			text.WriteString(value.Text)
		}
	}
	if result.IsError {
		t.Fatalf("%s returned tool error: %s", name, text.String())
	}
	return text.String()
}

func waitForLifecycle(t *testing.T, url string, timeout time.Duration, ready func(lifecycleDetail) bool) lifecycleDetail {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var latest lifecycleDetail
	var lastErr error
	for time.Now().Before(deadline) {
		latest, lastErr = readLifecycle(url)
		if lastErr == nil && ready(latest) {
			return latest
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("lifecycle condition timed out after %s: state=%#v err=%v", timeout, latest, lastErr)
	return lifecycleDetail{}
}

func getLifecycle(t *testing.T, url string) lifecycleDetail {
	t.Helper()
	state, err := readLifecycle(url)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func readLifecycle(url string) (lifecycleDetail, error) {
	resp, err := http.Get(url)
	if err != nil {
		return lifecycleDetail{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return lifecycleDetail{}, fmt.Errorf("admin status %d: %s", resp.StatusCode, body)
	}
	var detail lifecycleDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return lifecycleDetail{}, err
	}
	return detail, nil
}

func freeVisionPort(t *testing.T) int {
	t.Helper()
	for port := 6276; port <= 6325; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port
	}
	t.Fatal("no free Vision MCP port in 6276-6325")
	return 0
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
