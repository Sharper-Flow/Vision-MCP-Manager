package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The control socket provides handshakes independent of the MCP transport.
const lifecycleServerJS = `
const net = require('net'), readline = require('readline');
const control = net.connect({host:'127.0.0.1',port:Number(process.env.VSN30_CONTROL)});
let init, call;
const reply = (id, result) => process.stdout.write(JSON.stringify({jsonrpc:'2.0',id,result})+'\n');
const initialized = () => reply(init, {protocolVersion:'2025-03-26',capabilities:{tools:{}},serverInfo:{name:'gated',version:'1'}});
readline.createInterface({input:control}).on('line', line => {
  if(line === 'init') initialized();
  if(line === 'release') reply(call, {content:[{type:'text',text:'completed'}]});
});
readline.createInterface({input:process.stdin}).on('close',()=>process.exit(0)).on('line', line => {
 const m = JSON.parse(line);
 if(m.method === 'initialize') { init=m.id; control.write('initialize\n'); }
	if(m.method === 'tools/list') {
   control.write('list\n');
    if(process.env.VSN30_LIST === 'error') process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,error:{code:-32603,message:'probe failed'}})+'\n');
    else if(process.env.VSN30_LIST !== 'hang') setTimeout(()=>reply(m.id,{tools:[]}),3000);
 }
  if(m.method === 'tools/call') { if(m.params.name === 'new') {control.write('newcall\n');reply(m.id,{content:[{type:'text',text:'new completed'}]});} else {call=m.id; control.write('call\n');} }
});
`

type lifecycleEvent struct {
	method string
	conn   net.Conn
}
type lifecycleFixture struct {
	listener net.Listener
	events   chan lifecycleEvent
	init     chan struct{}
	once     sync.Once
}

func newLifecycleFixture(t *testing.T, gateInit bool) *lifecycleFixture {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &lifecycleFixture{listener: l, events: make(chan lifecycleEvent, 64), init: make(chan struct{})}
	if !gateInit {
		f.releaseInit()
	}
	t.Cleanup(func() { f.releaseInit(); _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					method := scanner.Text()
					f.events <- lifecycleEvent{method, c}
					if method == "initialize" {
						<-f.init
						_, _ = fmt.Fprintln(c, "init")
					}
				}
			}()
		}
	}()
	return f
}

func (f *lifecycleFixture) releaseInit() { f.once.Do(func() { close(f.init) }) }
func (f *lifecycleFixture) await(t *testing.T, method string) net.Conn {
	t.Helper()
	select {
	case event := <-f.events:
		if event.method != method {
			t.Fatalf("event = %s, want %s", event.method, method)
		}
		return event.conn
	case <-time.After(3 * time.Second):
		t.Fatalf("no %s handshake", method)
		return nil
	}
}

func (f *lifecycleFixture) manager(t *testing.T, list string) *SharedSessionManager {
	t.Helper()
	cfg := testSharedServerConfig()
	cfg.Args = []string{"-e", lifecycleServerJS}
	cfg.Env = map[string]string{"VSN30_CONTROL": fmt.Sprint(f.listener.Addr().(*net.TCPAddr).Port), "VSN30_LIST": list}
	sm := NewSharedSessionManager("lifecycle", cfg, testLogger(t), 0, nil)
	t.Cleanup(sm.CloseAll)
	return sm
}

type lifecycleResult struct {
	ds  *mcp.ClientSession
	err error
}

func admit(sm *SharedSessionManager, ctx context.Context, id string) <-chan lifecycleResult {
	ch := make(chan lifecycleResult, 1)
	go func() { ds, err := sm.GetOrCreateSession(ctx, id); ch <- lifecycleResult{ds, err} }()
	return ch
}
func awaitAdmission(t *testing.T, ch <-chan lifecycleResult) lifecycleResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("admission did not complete")
		return lifecycleResult{}
	}
}

func TestSharedLifecycleSlowListDoesNotProbeOrReplaceActiveCall(t *testing.T) {
	f := newLifecycleFixture(t, false)
	sm := f.manager(t, "")
	owner := metrics.NewServerMetrics()
	sm.metrics = owner
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first := awaitAdmission(t, admit(sm, ctx, "one"))
	if first.err != nil {
		t.Fatal(first.err)
	}
	f.await(t, "initialize")
	release, err := sm.BeginRequest(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	callDone := make(chan error, 1)
	go func() { _, err := first.ds.CallTool(ctx, &mcp.CallToolParams{Name: "long"}); callDone <- err }()
	conn := f.await(t, "call")
	defer fmt.Fprintln(conn, "release")
	listDone := make(chan error, 1)
	go func() { _, err := first.ds.ListTools(ctx, nil); listDone <- err }()
	f.await(t, "list")
	second := admit(sm, ctx, "two")
	var admitted lifecycleResult
	received := false
	select {
	case r := <-second:
		admitted, received = r, true
	case <-time.After(time.Second):
		t.Error("second admission blocked on a per-lookup tools/list probe")
	}
	if err := <-listDone; err != nil {
		t.Error(err)
	}
	_, _ = fmt.Fprintln(conn, "release")
	if err := <-callDone; err != nil {
		t.Errorf("active call failed: %v", err)
	}
	if !received {
		admitted = awaitAdmission(t, second)
	}
	if admitted.err != nil || admitted.ds != first.ds {
		t.Errorf("second admission replaced live transport: %v", admitted.err)
	}
	if sm.Downstream() != first.ds || owner.Snapshot().SpawnedByReason[metrics.SpawnReasonLookupRespawn] != 0 {
		t.Error("lookup respawned a live downstream")
	}
}

func TestSharedLifecycleHealthThresholdDefersWhileActive(t *testing.T) {
	f := newLifecycleFixture(t, false)
	sm := f.manager(t, "hang")
	decision := &healthDecisionHandler{Handler: testLogger(t).Handler(), decided: make(chan struct{})}
	sm.logger = slog.New(decision)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := awaitAdmission(t, admit(sm, ctx, "one"))
	if first.err != nil {
		t.Fatal(first.err)
	}
	f.await(t, "initialize")
	release, err := sm.BeginRequest(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	callDone := make(chan error, 1)
	go func() { _, err := first.ds.CallTool(ctx, &mcp.CallToolParams{Name: "long"}); callDone <- err }()
	conn := f.await(t, "call")
	sm.mu.Lock()
	sm.healthCtx = ctx
	sm.healthCheckFailures = maxConsecutiveHealthCheckTimeouts - 1
	sm.mu.Unlock()
	healthDone := make(chan struct{})
	go func() { sm.healthCheck(); close(healthDone) }()
	f.await(t, "list")
	select {
	case <-decision.decided:
		if sm.Downstream() != first.ds {
			t.Error("threshold probe closed transport with active call")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("threshold probe did not reserve recovery")
	}
	select {
	case <-healthDone:
		t.Error("threshold recovery ended before draining the active call")
	default:
	}
	_, _ = fmt.Fprintln(conn, "release")
	if err := <-callDone; err != nil {
		t.Errorf("active call failed: %v", err)
	}
	release()
	select {
	case <-healthDone:
	case <-time.After(3 * time.Second):
		t.Error("health recovery did not finish")
	}
	f.await(t, "initialize")
	if sm.Downstream() == first.ds {
		t.Error("drained downstream did not recover after threshold failure")
	}
}

type healthDecisionHandler struct {
	slog.Handler
	decided chan struct{}
	once    sync.Once
}

func (h *healthDecisionHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "event" && a.Value.String() == "shared_session.health_check_failed" {
			h.once.Do(func() { close(h.decided) })
		}
		return true
	})
	return h.Handler.Handle(ctx, r)
}

type requestWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *requestWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSharedLifecycleSustainedLoadDrainsBeforeReplacement(t *testing.T) {
	f := newLifecycleFixture(t, false)
	sm := f.manager(t, "error")
	owner := metrics.NewServerMetrics()
	sm.metrics = owner
	decision := &healthDecisionHandler{Handler: testLogger(t).Handler(), decided: make(chan struct{})}
	sm.logger = slog.New(decision)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := awaitAdmission(t, admit(sm, ctx, "one"))
	if first.err != nil {
		t.Fatal(first.err)
	}
	f.await(t, "initialize")
	if second := awaitAdmission(t, admit(sm, ctx, "two")); second.err != nil {
		t.Fatal(second.err)
	}
	release, err := sm.BeginRequest(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	callDone := make(chan error, 1)
	go func() { _, err := first.ds.CallTool(ctx, &mcp.CallToolParams{Name: "long"}); callDone <- err }()
	conn := f.await(t, "call")
	defer fmt.Fprintln(conn, "release")
	sm.mu.Lock()
	sm.healthCheckFailures = maxConsecutiveHealthCheckTimeouts - 1
	sm.mu.Unlock()
	healthDone := make(chan struct{})
	go func() { sm.healthCheck(); close(healthDone) }()
	f.await(t, "list")
	select {
	case <-decision.decided:
	case <-ctx.Done():
		t.Fatal("health decision not observed")
	}
	const incoming = 8
	results := make([]chan lifecycleResult, incoming)
	for i := range incoming {
		observed := &requestWaitContext{Context: ctx, entered: make(chan struct{})}
		results[i] = make(chan lifecycleResult, 1)
		go func() {
			end, err := sm.BeginRequest(observed, "two")
			if err != nil {
				results[i] <- lifecycleResult{err: err}
				return
			}
			defer end()
			ds := sm.Downstream()
			_, err = ds.CallTool(observed, &mcp.CallToolParams{Name: "new"})
			results[i] <- lifecycleResult{ds, err}
		}()
		// Done is consulted either by the admission wait or by the SDK call.
		// The result's generation below distinguishes those two paths.
		select {
		case <-observed.entered:
		case <-ctx.Done():
			t.Fatal("incoming request did not reach a context boundary")
		}
	}
	select {
	case <-healthDone:
		t.Error("health recovery returned without reserving a drain for active requests")
	default:
	}
	sm.refMu.Lock()
	active := sm.activeRequests
	sm.sessionTimeout = time.Nanosecond
	sm.sessions["two"].lastActivity = time.Now().Add(-time.Hour)
	sm.refMu.Unlock()
	if active != 1 {
		t.Errorf("queued calls contributed to drain count: %d", active)
	}
	sm.reapExpiredSessions()
	if sm.RefCount() != 2 {
		t.Error("session expiry removed queued application requests")
	}
	_, _ = fmt.Fprintln(conn, "release")
	if err := <-callDone; err != nil {
		t.Errorf("valid old call failed during drain: %v", err)
	}
	release()
	select {
	case <-healthDone:
	case <-ctx.Done():
		t.Fatal("recovery did not finish after old call drained")
	}
	for i, result := range results {
		select {
		case got := <-result:
			if got.err != nil || got.ds == first.ds || got.ds != sm.Downstream() {
				t.Errorf("incoming request %d entered old generation or failed: old=%v err=%v", i, got.ds == first.ds, got.err)
			}
		case <-ctx.Done():
			t.Fatal("waiting application call did not resume")
		}
	}
	if got := owner.Snapshot().SpawnedByReason[metrics.SpawnReasonHealthRespawn]; got != 1 {
		t.Errorf("health replacements under load = %d, want 1", got)
	}
}

func TestSharedLifecycleDrainWaitBoundaries(t *testing.T) {
	for _, mode := range []string{"caller_deadline", "manager_closed", "transport_closed"} {
		t.Run(mode, func(t *testing.T) {
			f := newLifecycleFixture(t, false)
			sm := f.manager(t, "error")
			owner := metrics.NewServerMetrics()
			sm.metrics = owner
			decision := &healthDecisionHandler{Handler: testLogger(t).Handler(), decided: make(chan struct{})}
			sm.logger = slog.New(decision)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			first := awaitAdmission(t, admit(sm, ctx, "one"))
			if first.err != nil {
				t.Fatal(first.err)
			}
			f.await(t, "initialize")
			endOld, err := sm.BeginRequest(ctx, "one")
			if err != nil {
				t.Fatal(err)
			}
			defer endOld()
			sm.mu.Lock()
			sm.healthCheckFailures = maxConsecutiveHealthCheckTimeouts - 1
			cmd := sm.cmd
			sm.mu.Unlock()
			healthDone := make(chan struct{})
			go func() { sm.healthCheck(); close(healthDone) }()
			f.await(t, "list")
			select {
			case <-decision.decided:
			case <-ctx.Done():
				t.Fatal("drain reservation not observed")
			}
			waitCtx := ctx
			if mode == "caller_deadline" {
				var cancelWait context.CancelFunc
				waitCtx, cancelWait = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancelWait()
			}
			observed := &requestWaitContext{Context: waitCtx, entered: make(chan struct{})}
			waiting := make(chan error, 1)
			go func() {
				end, err := sm.BeginRequest(observed, "one")
				if err == nil {
					end()
				}
				waiting <- err
			}()
			select {
			case <-observed.entered:
			case <-ctx.Done():
				t.Fatal("new call did not wait for drain")
			}
			switch mode {
			case "caller_deadline":
				err := <-waiting
				var exhausted *SharedStartingError
				if !errors.As(err, &exhausted) || !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("wait deadline lost identity: %v", err)
				}
				if sm.Downstream() != first.ds {
					t.Error("caller deadline interrupted the old generation")
				}
				endOld()
			case "manager_closed":
				closed := make(chan struct{})
				go func() { sm.CloseAll(); close(closed) }()
				select {
				case err := <-waiting:
					if !errors.Is(err, ErrSharedClosed) {
						t.Errorf("lifetime cancellation = %v", err)
					}
				case <-ctx.Done():
					t.Fatal("lifetime cancellation did not release admission wait")
				}
				endOld()
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("CloseAll did not finish")
				}
			case "transport_closed":
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-waiting:
					if err != nil {
						t.Errorf("dead-transport recovery = %v", err)
					}
				case <-ctx.Done():
					t.Fatal("transport closure waited on dead application calls")
				}
				if sm.Downstream() == first.ds {
					t.Error("transport closure did not replace the generation")
				}
				endOld()
			}
			select {
			case <-healthDone:
			case <-ctx.Done():
				t.Fatal("health recovery did not complete")
			}
			want := int64(1)
			if mode == "manager_closed" {
				want = 0
			}
			if got := owner.Snapshot().SpawnedByReason[metrics.SpawnReasonHealthRespawn]; got != want {
				t.Errorf("health spawns = %d, want %d", got, want)
			}
		})
	}
}

func TestSharedLifecycleStartupFailurePreservesCause(t *testing.T) {
	cfg := testSharedServerConfig()
	cfg.Command = "vision-nonexistent-shared-downstream"
	sm := NewSharedSessionManager("missing", cfg, testLogger(t), 0, nil)
	defer sm.CloseAll()
	err := sm.EnsureReady(context.Background())
	var failed *SharedStartupError
	if !errors.As(err, &failed) || !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("startup error lost wrapped cause: %v", err)
	}
}

func TestSharedLifecycleDownstreamOmitsObservedClosure(t *testing.T) {
	f := newLifecycleFixture(t, false)
	sm := f.manager(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := awaitAdmission(t, admit(sm, ctx, "one"))
	if first.err != nil {
		t.Fatal(first.err)
	}
	f.await(t, "initialize")
	sm.mu.RLock()
	done := sm.downstreamDone
	sm.mu.RUnlock()
	if err := first.ds.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("closure observer did not finish")
	}
	if sm.Downstream() != nil || sm.HasDownstream() {
		t.Error("Downstream returned a transport whose closure was already observed")
	}
}

func TestSharedLifecycleColdStartupCancellationIsolated(t *testing.T) {
	f := newLifecycleFixture(t, true)
	sm := f.manager(t, "")
	owner := metrics.NewServerMetrics()
	sm.metrics = owner
	ctx, cancel := context.WithCancel(context.Background())
	first := admit(sm, ctx, "cancelled")
	f.await(t, "initialize")
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	observed := &requestWaitContext{Context: requestCtx, entered: make(chan struct{})}
	requestDone := make(chan error, 1)
	go func() {
		release, err := sm.BeginRequest(observed, "cancelled")
		if err == nil {
			release()
		}
		requestDone <- err
	}()
	select {
	case <-observed.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("application call did not wait on cold startup")
	}
	cancelRequest()
	var waiting *SharedStartingError
	if err := <-requestDone; !errors.As(err, &waiting) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled request wait lost identity: %v", err)
	}
	cancel()
	r := awaitAdmission(t, first)
	var starting *SharedStartingError
	if !errors.As(r.err, &starting) || !errors.Is(r.err, context.Canceled) {
		t.Errorf("cancelled waiter error = %v, want typed retryable starting refusal", r.err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	const callers = 5
	results := make([]<-chan lifecycleResult, callers)
	for i := range results {
		results[i] = admit(sm, ctx2, fmt.Sprint(i))
	}
	f.releaseInit()
	var ds *mcp.ClientSession
	for _, ch := range results {
		r := awaitAdmission(t, ch)
		if r.err != nil {
			t.Fatal(r.err)
		}
		if ds != nil && r.ds != ds {
			t.Error("concurrent admission spawned multiple transports")
		}
		ds = r.ds
	}
	if got := sm.RefCount(); got != callers {
		t.Errorf("refcount = %d, want %d", got, callers)
	}
	// A second initialize event proves the cancelled caller destroyed cold startup.
	select {
	case e := <-f.events:
		if e.method == "initialize" {
			t.Error("cancelled waiter destroyed shared startup and forced a second spawn")
		}
		if strings.Contains(e.method, "list") {
			t.Error("concurrent admission performed a lookup probe")
		}
	default:
	}
	if got := owner.Snapshot().SpawnedByReason[metrics.SpawnReasonFirstUse]; got != 1 {
		t.Errorf("first-use spawns = %d, want one single-flight spawn", got)
	}
}

func TestSharedLifecycleSpawnIdentityAndReasons(t *testing.T) {
	f := newLifecycleFixture(t, false)
	sm := f.manager(t, "")
	owner := metrics.NewServerMetrics()
	sm.metrics = owner
	sm.config.Env["SECRET_TOKEN"] = "must-not-appear-in-telemetry"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	before := time.Now()
	first := awaitAdmission(t, admit(sm, ctx, "one"))
	if first.err != nil {
		t.Fatal(first.err)
	}
	f.await(t, "initialize")
	pid, started := sm.ProcessStatus()
	if pid <= 0 || started.Before(before) || started.After(time.Now()) {
		t.Fatalf("invalid first process identity: %d %s", pid, started)
	}
	replaced := make(chan *mcp.ClientSession, 4)
	sm.Subscribe("one", func(ds *mcp.ClientSession) { replaced <- ds }, nil)
	sm.mu.RLock()
	cmd, done := sm.cmd, sm.downstreamDone
	sm.mu.RUnlock()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("transport did not observe process exit")
	}
	second := awaitAdmission(t, admit(sm, ctx, "two"))
	if second.err != nil {
		t.Fatal(second.err)
	}
	f.await(t, "initialize")
	if second.ds == first.ds {
		t.Error("crash recovery reused closed transport")
	}
	if got := <-replaced; got != second.ds {
		t.Error("subscriber did not receive replacement")
	}
	pid2, started2 := sm.ProcessStatus()
	if pid2 == pid || !started2.After(started) {
		t.Errorf("replacement identity unchanged: %d %s", pid2, started2)
	}
	if err := sm.RemoveSession("one"); err != nil {
		t.Fatal(err)
	}
	if err := sm.RemoveSession("two"); err != nil {
		t.Fatal(err)
	}
	sm.reapDownstream()
	if pid, started := sm.ProcessStatus(); pid != 0 || !started.IsZero() {
		t.Errorf("reap retained process identity: %d %s", pid, started)
	}
	third := awaitAdmission(t, admit(sm, ctx, "three"))
	if third.err != nil {
		t.Fatal(third.err)
	}
	f.await(t, "initialize")
	<-replaced
	if err := third.ds.Close(); err != nil {
		t.Fatal(err)
	}
	sm.healthCheck()
	f.await(t, "initialize")
	<-replaced
	for _, reason := range []string{metrics.SpawnReasonFirstUse, metrics.SpawnReasonLookupRespawn, metrics.SpawnReasonIdleReapRespawn, metrics.SpawnReasonHealthRespawn} {
		if got := owner.Snapshot().SpawnedByReason[reason]; got != 1 {
			t.Errorf("spawned_by_reason[%s] = %d, want 1", reason, got)
		}
	}
	encoded, err := json.Marshal(owner.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must-not-appear") || strings.Contains(string(encoded), "SECRET_TOKEN") {
		t.Errorf("metrics exposed credentials: %s", encoded)
	}
	owner.IncSpawned("must-not-appear-in-telemetry")
	if len(owner.Snapshot().SpawnedByReason) != 4 {
		t.Error("unknown spawn reason expanded counter cardinality")
	}
}

func TestSharedLifecycleCloseAllCancelsColdStartup(t *testing.T) {
	f := newLifecycleFixture(t, true)
	sm := f.manager(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := admit(sm, ctx, "one")
	f.await(t, "initialize")
	closed := make(chan struct{})
	go func() { sm.CloseAll(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("CloseAll did not cancel startup")
	}
	if r := awaitAdmission(t, result); r.err == nil || r.ds != nil {
		t.Errorf("startup survived CloseAll: %v", r.err)
	}
	if sm.RefCount() != 0 || sm.HasDownstream() {
		t.Error("CloseAll retained startup population")
	}
}

func TestSharedLifecycleIdleTimerWithoutProcessKeepsSpawnReason(t *testing.T) {
	cfg := testSharedServerConfig()
	cfg.Args = []string{"-e", "process.exit(1)"}
	owner := metrics.NewServerMetrics()
	sm := NewSharedSessionManager("failing", cfg, testLogger(t), 20*time.Millisecond, owner)
	t.Cleanup(sm.CloseAll)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := sm.GetOrCreateSession(ctx, "one"); err == nil {
		t.Fatal("failing downstream admitted")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := sm.GetOrCreateSession(ctx, "two"); err == nil {
		t.Fatal("failing downstream admitted")
	}
	snap := owner.Snapshot()
	if snap.SpawnedByReason[metrics.SpawnReasonFirstUse] != 2 || snap.SpawnedByReason[metrics.SpawnReasonIdleReapRespawn] != 0 {
		t.Errorf("spawn reasons after failed startups = %v", snap.SpawnedByReason)
	}
	if snap.ReapedByReason[metrics.ReapReasonIdleTimeout] != 0 {
		t.Errorf("idle reap counted without a process: %v", snap.ReapedByReason)
	}
}
