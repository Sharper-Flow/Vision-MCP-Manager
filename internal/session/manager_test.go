package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoMCPServerJS is a minimal Node.js MCP server that responds to
// tools/list and tools/call via stdio JSON-RPC.
const echoMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
let initialized = false;
rl.on('line', (line) => {
  try {
    const msg = JSON.parse(line);
    if (msg.method === 'initialize') {
      const resp = {
        jsonrpc: '2.0',
        id: msg.id,
        result: {
          protocolVersion: '2025-03-26',
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: 'echo-test', version: '1.0.0' }
        }
      };
      process.stdout.write(JSON.stringify(resp) + '\n');
    } else if (msg.method === 'notifications/initialized') {
      initialized = true;
    } else if (msg.method === 'tools/list') {
      const resp = {
        jsonrpc: '2.0',
        id: msg.id,
        result: {
          tools: [{
            name: 'echo',
            description: 'Echoes input',
            inputSchema: { type: 'object', properties: { message: { type: 'string' } } }
          }]
        }
      };
      process.stdout.write(JSON.stringify(resp) + '\n');
    } else if (msg.method === 'tools/call') {
      const resp = {
        jsonrpc: '2.0',
        id: msg.id,
        result: {
          content: [{ type: 'text', text: 'echo: ' + (msg.params?.arguments?.message || '') + ' pid=' + process.pid }]
        }
      };
      process.stdout.write(JSON.stringify(resp) + '\n');
    } else if (msg.id) {
      // Unknown method with ID - return empty result
      process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: {} }) + '\n');
    }
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

func skipIfNoNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testServerConfig() *config.ServerConfig {
	return &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16280,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}
}

// TestManager_SpawnSession verifies that a new session spawns a subprocess,
// connects via CommandTransport, performs the initialize handshake,
// and maps the session ID to the downstream ClientSession.
func TestManager_SpawnSession(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := NewManager("test-server", cfg, logger)

	// Spawn a session
	sessionID := "sess-001"
	downstream, err := mgr.SpawnSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}
	if downstream == nil {
		t.Fatal("SpawnSession returned nil downstream session")
	}

	// Verify the session is tracked
	got := mgr.GetSession(sessionID)
	if got == nil {
		t.Fatal("session not found after spawn")
	}
	if got.SessionID != sessionID {
		t.Errorf("session ID = %q, want %q", got.SessionID, sessionID)
	}
	if got.Downstream == nil {
		t.Error("downstream ClientSession is nil")
	}

	// Verify we can call tools through the downstream session
	toolsResult, err := downstream.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(toolsResult.Tools) == 0 {
		t.Fatal("no tools returned from downstream")
	}
	if toolsResult.Tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want %q", toolsResult.Tools[0].Name, "echo")
	}

	// Verify CallTool works
	callResult, err := downstream.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if len(callResult.Content) == 0 {
		t.Fatal("no content in CallTool result")
	}

	// Count active sessions
	if count := mgr.SessionCount(); count != 1 {
		t.Errorf("session count = %d, want 1", count)
	}

	// Clean up
	if err := mgr.RemoveSession(sessionID); err != nil {
		t.Errorf("RemoveSession failed: %v", err)
	}

	// Verify session is gone
	if got := mgr.GetSession(sessionID); got != nil {
		t.Error("session still found after removal")
	}
	if count := mgr.SessionCount(); count != 0 {
		t.Errorf("session count after removal = %d, want 0", count)
	}
}

// TestManager_ConcurrentSessions verifies that two concurrent sessions
// each get isolated subprocesses with different PIDs.
func TestManager_ConcurrentSessions(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := NewManager("test-server", cfg, logger)

	// Spawn two sessions concurrently
	var wg sync.WaitGroup
	sessions := make([]*mcp.ClientSession, 2)
	errs := make([]error, 2)

	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessionID := fmt.Sprintf("sess-%03d", i)
			sessions[i], errs[i] = mgr.SpawnSession(ctx, sessionID)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("SpawnSession[%d] failed: %v", i, err)
		}
	}

	// Verify each session has a different PID (proving subprocess isolation)
	results := make([]string, 2)
	for i, sess := range sessions {
		callResult, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name:      "echo",
			Arguments: map[string]any{"message": "test"},
		})
		if err != nil {
			t.Fatalf("CallTool[%d] failed: %v", i, err)
		}
		if len(callResult.Content) > 0 {
			if tc, ok := callResult.Content[0].(*mcp.TextContent); ok {
				results[i] = tc.Text
			}
		}
	}

	if results[0] == results[1] {
		t.Errorf("both sessions returned same result (same subprocess?): %q", results[0])
	}

	// Verify both sessions are tracked
	if count := mgr.SessionCount(); count != 2 {
		t.Errorf("session count = %d, want 2", count)
	}

	// Clean up
	mgr.CloseAll()
	if count := mgr.SessionCount(); count != 0 {
		t.Errorf("session count after CloseAll = %d, want 0", count)
	}
}

// TestManager_RemoveSession_KillsSubprocess verifies that removing a session
// terminates the downstream subprocess gracefully.
func TestManager_RemoveSession_KillsSubprocess(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := NewManager("test-server", cfg, logger)

	sessionID := "sess-kill"
	downstream, err := mgr.SpawnSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}

	// Remove the session (should kill subprocess)
	if err := mgr.RemoveSession(sessionID); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	// Attempting to use the closed downstream should fail
	_, err = downstream.ListTools(ctx, nil)
	if err == nil {
		t.Error("expected error calling ListTools on closed session, got nil")
	}
}

// TestManager_MaxSessions verifies admission control rejects new sessions
// when the limit is reached.
func TestManager_MaxSessions(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()
	cfg.MaxSessions = 1 // Only allow one session

	mgr := NewManager("test-server", cfg, logger)

	// First session should succeed
	_, err := mgr.SpawnSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("first SpawnSession failed: %v", err)
	}

	// Second session should be rejected
	_, err = mgr.SpawnSession(ctx, "sess-2")
	if err == nil {
		t.Fatal("expected error for second session (max reached), got nil")
	}

	// Clean up first session, then second should succeed
	if err := mgr.RemoveSession("sess-1"); err != nil {
		t.Fatalf("RemoveSession failed: %v", err)
	}

	_, err = mgr.SpawnSession(ctx, "sess-2")
	if err != nil {
		t.Fatalf("SpawnSession after removal failed: %v", err)
	}

	mgr.CloseAll()
}

// TestManager_TouchSession verifies that TouchSession updates LastActivity
// and that the tracked session records creation time.
func TestManager_TouchSession(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := NewManager("test-server", cfg, logger)

	sessionID := "sess-touch"
	_, err := mgr.SpawnSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}
	defer mgr.CloseAll()

	tracked := mgr.GetSession(sessionID)
	if tracked == nil {
		t.Fatal("session not found")
	}

	// CreatedAt should be set to approximately now.
	if tracked.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if tracked.LastActivity.IsZero() {
		t.Error("LastActivity is zero after spawn")
	}

	// Record the initial activity time.
	initialActivity := tracked.LastActivity

	// Wait a small amount and touch.
	time.Sleep(10 * time.Millisecond)
	mgr.TouchSession(sessionID)

	tracked = mgr.GetSession(sessionID)
	if !tracked.LastActivity.After(initialActivity) {
		t.Error("LastActivity was not updated by TouchSession")
	}
}

// TestManager_Reaper_IdleTimeout verifies that the reaper removes sessions
// that exceed the idle timeout.
func TestManager_Reaper_IdleTimeout(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(100 * time.Millisecond) // 100ms idle timeout
	cfg.SessionTTL = config.Duration(0)                          // No TTL

	mgr := NewManager("test-server", cfg, logger)
	mgr.StartReaper(ctx, 50*time.Millisecond) // Check every 50ms

	sessionID := "sess-idle"
	_, err := mgr.SpawnSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}

	// Session should exist immediately.
	if mgr.GetSession(sessionID) == nil {
		t.Fatal("session not found right after spawn")
	}

	// Wait for the idle timeout to pass + reaper cycle.
	time.Sleep(300 * time.Millisecond)

	// Session should have been reaped.
	if mgr.GetSession(sessionID) != nil {
		t.Error("session was NOT reaped after idle timeout")
	}
	if mgr.SessionCount() != 0 {
		t.Errorf("session count = %d, want 0 after reap", mgr.SessionCount())
	}
}

// TestManager_Reaper_TTLTimeout verifies that the reaper removes sessions
// that exceed their absolute TTL, even if they are actively touched.
func TestManager_Reaper_TTLTimeout(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(10 * time.Second) // Long idle (won't trigger)
	cfg.SessionTTL = config.Duration(200 * time.Millisecond)

	mgr := NewManager("test-server", cfg, logger)
	mgr.StartReaper(ctx, 50*time.Millisecond)

	sessionID := "sess-ttl"
	_, err := mgr.SpawnSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}

	// Keep touching to prevent idle timeout.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Millisecond):
				mgr.TouchSession(sessionID)
			}
		}
	}()

	// Wait for TTL to pass.
	time.Sleep(400 * time.Millisecond)

	if mgr.GetSession(sessionID) != nil {
		t.Error("session was NOT reaped after TTL expiry despite being touched")
	}
}

// TestManager_Reaper_StopsOnCloseAll verifies that CloseAll stops
// cleanly even when reaper is running.
func TestManager_Reaper_StopsOnCloseAll(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()
	cfg.SessionTimeout = config.Duration(5 * time.Second) // Long timeout

	mgr := NewManager("test-server", cfg, logger)
	mgr.StartReaper(ctx, 100*time.Millisecond)

	_, err := mgr.SpawnSession(ctx, "sess-reaper-close")
	if err != nil {
		t.Fatalf("SpawnSession failed: %v", err)
	}

	// CloseAll should work with reaper running.
	mgr.CloseAll()

	if mgr.SessionCount() != 0 {
		t.Errorf("session count = %d, want 0 after CloseAll", mgr.SessionCount())
	}
}
