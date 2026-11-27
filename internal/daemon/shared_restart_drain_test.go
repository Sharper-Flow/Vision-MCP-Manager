package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/admin"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const restartDrainFixture = `
const net = require('net');
const rl = require('readline').createInterface({input: process.stdin});
rl.on('close', () => process.exit(0));
function reply(id, result) { console.log(JSON.stringify({jsonrpc:'2.0', id, result})); }
rl.on('line', line => {
  const m = JSON.parse(line);
  if (m.method === 'initialize') reply(m.id, {protocolVersion:'2025-03-26', capabilities:{tools:{}}, serverInfo:{name:'drain-fixture',version:'1'}});
  if (m.method === 'tools/list') reply(m.id, {tools:[{name:'held',inputSchema:{type:'object'}}]});
  if (m.method === 'tools/call') {
    const signal = net.createConnection({port:Number(process.env.SIGNAL_PORT), host:'127.0.0.1'}, () => signal.write('accepted\n'));
    signal.once('data', () => { reply(m.id, {content:[{type:'text',text:'completed'}]}); signal.end(); });
  }
});
`

type restartDrainWriter struct {
	http.ResponseWriter
	reached chan<- struct{}
	release <-chan struct{}
}

func (w *restartDrainWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"text":"completed"`)) {
		close(w.reached)
		<-w.release
	}
	return w.ResponseWriter.Write(p)
}

func (w *restartDrainWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *restartDrainWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func TestTargetedRestartDrainsSharedStdioCall(t *testing.T) {
	testSharedRestartDrain(t, false)
}

func TestSharedRestartDrainReceiptTimeout(t *testing.T) {
	testSharedRestartDrain(t, true)
}

func testSharedRestartDrain(t *testing.T, timedOut bool) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	signal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer signal.Close()

	// Reserve the configured port so no daemon listener or probe can reach
	// another process. Requests use the pre-bound listener below instead.
	var reserved net.Listener
	for port := config.MinPort; port <= config.MaxPort; port++ {
		reserved, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
	}
	if reserved == nil {
		t.Fatal("no free Vision port")
	}
	defer reserved.Close()
	port := reserved.Addr().(*net.TCPAddr).Port
	path := filepath.Join(dir, "servers.yaml")
	data := fmt.Sprintf("servers:\n  held:\n    command: node\n    args: ['-e', %q]\n    env:\n      SIGNAL_PORT: %q\n    port: %d\n    autostart: true\n", restartDrainFixture, fmt.Sprint(signal.Addr().(*net.TCPAddr).Port), port)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{ConfigPath: path, OwnershipRoot: filepath.Join(dir, "ownership"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if timedOut {
		d.sharedRestartDrainTimeout = 500 * time.Millisecond
	}
	d.adminServer = nil
	d.probeManager.Close()
	d.probeManager = nil
	started := make(chan struct{}, 2)
	d.registry.SetEventHandler(func(event server.ServerEvent) {
		d.handleServerEvent(event)
		if event.Name == "held" && event.Type == server.EventServerStarted {
			started <- struct{}{}
		}
	})
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Stop(5 * time.Second)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial server start event did not complete")
	}
	listener := d.portManager.Get("held")
	if listener == nil {
		t.Fatal("daemon did not create shared proxy")
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	reached := make(chan struct{})
	releaseWrite := make(chan struct{})
	handler := listener.Server.Handler
	listener.Server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w = &restartDrainWriter{ResponseWriter: w, reached: reached, release: releaseWrite}
		}
		handler.ServeHTTP(w, r)
	})
	serveDone := make(chan error, 1)
	go func() { serveDone <- listener.Server.Serve(proxy) }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "restart-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + proxy.Addr().String() + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	restartComplete := make(chan struct{})
	var restartErr error
	var receipt *admin.RestartDrain
	restartStarted := false
	released := false
	defer func() {
		if !released {
			close(releaseWrite)
		}
		if restartStarted {
			<-restartComplete
			if restartErr == nil {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Error("restart start event did not complete during cleanup")
				}
			}
		}
	}()
	type callOutcome struct {
		result *mcp.CallToolResult
		err    error
	}
	callDone := make(chan callOutcome, 1)
	go func() {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "held"})
		callDone <- callOutcome{result, err}
	}()
	if err := signal.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := signal.Accept()
	if err != nil {
		t.Fatalf("fixture did not accept tools/call: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("accepted\n"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "accepted\n" {
		t.Fatalf("fixture acceptance = %q, %v", buf, err)
	}

	if _, err := conn.Write([]byte("release\n")); err != nil {
		t.Fatalf("release fixture: %v", err)
	}
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("successful gateway reply did not reach response writer")
	}
	select {
	case outcome := <-callDone:
		t.Fatalf("reply forwarded before response writer was released: result=%+v error=%v", outcome.result, outcome.err)
	default:
	}
	restartStarted = true
	go func() {
		receipt, restartErr = d.restartServer(ctx, "held")
		close(restartComplete)
	}()
	var lastProbe string
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		request, err := http.NewRequestWithContext(probeCtx, http.MethodPost, "http://"+proxy.Addr().String()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"held"}}`))
		if err != nil {
			probeCancel()
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Session-Id", "admission-probe")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			probeCancel()
			lastProbe = fmt.Sprintf("status=%d body=%q read=%v", response.StatusCode, body, readErr)
			if readErr == nil && response.StatusCode == http.StatusServiceUnavailable && bytes.Contains(body, []byte(`"reason":"restart_in_progress"`)) {
				break
			}
		} else {
			probeCancel()
			lastProbe = err.Error()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("new application call was not refused during drain: %s", lastProbe)
		case <-restartComplete:
			t.Fatalf("restart completed before the held response was released: %s, restart=%v", lastProbe, restartErr)
		default:
		}
	}
	if timedOut {
		select {
		case <-restartComplete:
			if restartErr != nil {
				t.Fatalf("restart: %v", restartErr)
			}
			if receipt == nil || receipt.Outcome != "timed_out" || receipt.TimeoutMS != 500 || len(receipt.RemainingRequests) != 1 || receipt.RemainingRequests[0] == 0 {
				t.Fatalf("invalid timeout receipt: %+v", receipt)
			}
		case <-ctx.Done():
			t.Fatal("timed-out restart did not complete")
		}
		return
	}
	close(releaseWrite)
	released = true
	select {
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("production HTTP server stopped unexpectedly: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("targeted restart did not close the production listener")
	}
	select {
	case outcome := <-callDone:
		if errors.Is(outcome.err, context.DeadlineExceeded) || errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("harness context ended instead of listener interrupting the response: %v", outcome.err)
		}
		if outcome.err != nil || outcome.result == nil || outcome.result.IsError || len(outcome.result.Content) != 1 {
			t.Fatalf("production listener closed while successful reply was pending; accepted call lost its reply: result=%+v error=%v", outcome.result, outcome.err)
		}
		text, ok := outcome.result.Content[0].(*mcp.TextContent)
		if !ok || text.Text != "completed" {
			t.Fatalf("accepted call content = %+v", outcome.result.Content)
		}
	case <-ctx.Done():
		t.Fatal("accepted call did not complete")
	}
	select {
	case <-restartComplete:
		if restartErr != nil {
			t.Fatalf("restart: %v", restartErr)
		}
		if receipt == nil || receipt.Outcome != "drained" || receipt.TimeoutMS <= 0 || len(receipt.RemainingRequests) != 0 {
			t.Fatalf("invalid drain receipt: %+v", receipt)
		}
	case <-ctx.Done():
		t.Fatal("targeted restart did not complete")
	}
}
