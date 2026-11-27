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
	"sync"
	"sync/atomic"
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

// restartDrainScenario selects what one run of the shared-restart harness
// adds to a targeted restart while a call's reply is blocked at the response
// writer.
type restartDrainScenario struct {
	timedOut       bool // drain deadline of 500ms
	concurrentStop bool // ordinary Stop attempted during the drain
	betweenGates   bool // stop handler held after it removed the old gate
	cancelDrain    bool // daemon shutdown signalled during the drain
	checkShutdown  bool // daemon Stop instead of a restart
}

func TestTargetedRestartDrainsSharedStdioCall(t *testing.T) {
	testSharedRestartDrain(t, restartDrainScenario{})
}

func TestSharedRestartDrainReceiptTimeout(t *testing.T) {
	testSharedRestartDrain(t, restartDrainScenario{timedOut: true})
}

func TestSharedRestartRejectsConcurrentStop(t *testing.T) {
	testSharedRestartDrain(t, restartDrainScenario{concurrentStop: true})
}

func TestSharedRestartRejectsRestartBetweenGates(t *testing.T) {
	testSharedRestartDrain(t, restartDrainScenario{betweenGates: true})
}

func TestSharedRestartShutdownBeforeCutoverReopensGate(t *testing.T) {
	testSharedRestartDrain(t, restartDrainScenario{cancelDrain: true})
}

func TestShutdownKeepsDaemonContextUntilServerStop(t *testing.T) {
	testSharedRestartDrain(t, restartDrainScenario{checkShutdown: true})
}

func TestSharedRestartRejectsMissingRunningProxy(t *testing.T) {
	registry := server.NewRegistry(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer registry.Close()
	if err := registry.Add("held", &config.ServerConfig{Command: "node"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Start("held"); err != nil {
		t.Fatal(err)
	}
	if err := registry.WaitLifecycle(context.Background(), "held"); err != nil {
		t.Fatal(err)
	}
	var events atomic.Int32
	registry.SetEventHandler(func(server.ServerEvent) { events.Add(1) })
	d := &Daemon{registry: registry, restartCtx: context.Background()}
	if _, err := d.restartServer(context.Background(), "held"); err == nil {
		t.Fatal("restart succeeded without a running shared proxy")
	}
	if state := registry.Get("held").State; state != server.StateRunning {
		t.Fatalf("state = %s, want running", state)
	}
	if count := events.Load(); count != 0 {
		t.Fatalf("rejected restart emitted %d lifecycle events", count)
	}
}

func testSharedRestartDrain(t *testing.T, scenario restartDrainScenario) {
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
	if scenario.timedOut {
		d.sharedRestartDrainTimeout = 500 * time.Millisecond
	}
	d.adminServer = nil
	d.probeManager.Close()
	d.probeManager = nil
	started := make(chan struct{}, 2)
	stopped := make(chan struct{})
	releaseStop := make(chan struct{})
	shutdownContext := make(chan error, 2)
	var holdStop sync.Once
	d.registry.SetEventHandler(func(event server.ServerEvent) {
		d.handleServerEvent(event)
		if scenario.checkShutdown && event.Name == "held" && event.Type == server.EventServerStopped {
			shutdownContext <- d.ctx.Err()
			shutdownContext <- d.restartCtx.Err()
		}
		if scenario.betweenGates && event.Name == "held" && event.Type == server.EventServerStopped {
			holdStop.Do(func() {
				close(stopped)
				<-releaseStop
			})
		}
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
	if scenario.checkShutdown {
		if err := d.Stop(5 * time.Second); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-shutdownContext:
			if err != nil {
				t.Fatalf("daemon context cancelled before stop handler: %v", err)
			}
		default:
			t.Fatal("shutdown did not deliver stop handler")
		}
		select {
		case err := <-shutdownContext:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("restart cancellation at stop handler = %v, want cancelled", err)
			}
		default:
			t.Fatal("shutdown did not report the restart cancellation")
		}
		return
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
	stopReleased := false
	defer func() {
		if !stopReleased {
			close(releaseStop)
		}
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
	if scenario.concurrentStop {
		if err := d.registry.Stop("held"); !errors.Is(err, server.ErrLifecycleBusy) {
			t.Fatalf("concurrent stop during drain = %v, want busy", err)
		}
	}
	if scenario.cancelDrain {
		oldGate, ok := d.sharedRestartGates.Load("held")
		if !ok {
			t.Fatal("shared proxy gate missing before shutdown")
		}
		d.restartCancel()
		select {
		case <-restartComplete:
			if !errors.Is(restartErr, context.Canceled) {
				t.Fatalf("restart error = %v, want cancellation", restartErr)
			}
		case <-ctx.Done():
			t.Fatal("restart did not release reservation after shutdown")
		}
		currentGate, ok := d.sharedRestartGates.Load("held")
		if !ok || currentGate != oldGate {
			t.Fatal("precommit shutdown replaced the proxy gate")
		}
		if state := d.registry.Get("held").State; state != server.StateRunning {
			t.Fatalf("state after abandoned restart = %s, want running", state)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+proxy.Addr().String()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":45,"method":"tools/call","params":{"name":"held"}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Session-Id", "admission-probe")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || (response.StatusCode == http.StatusServiceUnavailable && bytes.Contains(body, []byte(`"reason":"restart_in_progress"`))) {
			t.Fatalf("gate did not reopen: status=%d body=%q error=%v", response.StatusCode, body, err)
		}
		close(releaseWrite)
		released = true
		select {
		case outcome := <-callDone:
			if outcome.err != nil || outcome.result == nil || outcome.result.IsError {
				t.Fatalf("held call failed after precommit shutdown: %+v", outcome)
			}
		case <-ctx.Done():
			t.Fatal("held call did not complete after precommit shutdown")
		}
		return
	}
	if scenario.timedOut {
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
	if scenario.betweenGates {
		select {
		case <-stopped:
		case <-ctx.Done():
			t.Fatal("stop handler did not reach between-gates interval")
		}
		if _, ok := d.sharedRestartGates.Load("held"); ok {
			t.Fatal("old proxy gate still present in between-gates interval")
		}
		secondDone := make(chan error, 1)
		go func() {
			_, err := d.restartServer(ctx, "held")
			secondDone <- err
		}()
		select {
		case err := <-secondDone:
			if !errors.Is(err, server.ErrLifecycleBusy) {
				t.Fatalf("second restart between proxy generations = %v, want busy", err)
			}
		case <-time.After(time.Second):
			t.Fatal("second restart did not promptly refuse between proxy generations")
		}
		close(releaseStop)
		stopReleased = true
	}
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
	if scenario.betweenGates {
		if _, ok := d.sharedRestartGates.Load("held"); !ok {
			t.Fatal("replacement proxy published no restart gate")
		}
		if state := d.registry.Get("held").State; state != server.StateRunning {
			t.Fatalf("state after restart = %s, want running", state)
		}
	}
}
