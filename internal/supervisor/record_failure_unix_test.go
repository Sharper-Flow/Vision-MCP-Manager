//go:build !windows

package supervisor

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
)

// signalCall records one process-group signal observation.
type signalCall struct {
	pgid   int
	signal os.Signal
}

// recordingSignaler delegates every group signal to the real platform signaler
// and records the call so a test can prove which group received which signal.
// Unlike fakeSignaler it performs the actual OS kill through the delegate.
type recordingSignaler struct {
	mu    sync.Mutex
	calls []signalCall
	real  ownership.Signaler
}

func (r *recordingSignaler) Supported() bool { return r.real.Supported() }

func (r *recordingSignaler) GroupSignal(pgid int, signal os.Signal) error {
	r.mu.Lock()
	r.calls = append(r.calls, signalCall{pgid: pgid, signal: signal})
	r.mu.Unlock()
	return r.real.GroupSignal(pgid, signal)
}

func (r *recordingSignaler) recorded() []signalCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]signalCall(nil), r.calls...)
}

// fixtureHandshakeTimeout bounds every event handshake and disposal wait. It
// is a failure detector, never a correctness synchronization: each handshake
// completes by an event (a channel close, a FIFO open pairing, or a read EOF).
const fixtureHandshakeTimeout = 5 * time.Second

// recordFailureBodyWaitDeadline drives the timed-out body-wait branch of the
// disposal regression. It is a failure-detector deadline, never correctness
// synchronization: the record barrier is provably held, so Serve cannot
// return and the deadline always expires first.
const recordFailureBodyWaitDeadline = 2 * time.Millisecond

// fifoPump owns every read from the FIFO read end. One goroutine pumps
// completed lines into a buffered channel and closes both channels at EOF, so
// handshakes are events and cleanup joins a single known reader before the
// file is closed.
type fifoPump struct {
	lines  chan string
	exited chan struct{}
}

func startFixturePump(f *os.File) *fifoPump {
	p := &fifoPump{lines: make(chan string, 2), exited: make(chan struct{})}
	go func() {
		defer close(p.exited)
		defer close(p.lines)
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			p.lines <- strings.TrimSpace(line)
		}
	}()
	return p
}

// awaitFixtureLine receives the next pumped line. The wait is an event: the
// pump publishes a line or closes its channels, it never polls.
func awaitFixtureLine(t *testing.T, p *fifoPump, deadline time.Duration) string {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case line := <-p.lines:
		return line
	case <-p.exited:
		return ""
	case <-timer.C:
		t.Fatal("fixture handshake did not complete within deadline")
		return ""
	}
}

// awaitFixtureEOF proves the FIFO write end is gone. A delivered line means a
// group member outlived the kill; a closed channel means every holder of the
// write end is dead.
func awaitFixtureEOF(t *testing.T, p *fifoPump, deadline time.Duration) {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case line, ok := <-p.lines:
		if !ok {
			return
		}
		t.Fatalf("fixture channel delivered %q after kill: a group member survived", line)
	case <-p.exited:
		return
	case <-timer.C:
		t.Fatal("fixture channel never closed after kill: descendant survived, kill was not group-wide")
	}
}

// openFixtureFIFO opens the FIFO read end. The open blocks until the fixture
// child opens its write end, so completion is an event, not a poll. The
// deadline branch unblocks the opener deterministically before failing.
func openFixtureFIFO(t *testing.T, path string, deadline time.Duration) *os.File {
	t.Helper()
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			f = nil
		}
		opened <- f
	}()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case f := <-opened:
		if f == nil {
			t.Fatal("fixture FIFO read end could not be opened")
		}
		return f
	case <-timer.C:
		// Disposal: pair the blocked read-open with a writer open, then close
		// the late read end, so no goroutine stays blocked on the FIFO.
		if late := unblockFIFOOpener(t, path, opened, deadline); late != nil {
			_ = late.Close()
		}
		t.Fatal("fixture FIFO was never opened by the child")
		return nil
	}
}

// unblockFIFOOpener releases a still-blocked FIFO read opener and returns the
// late opened file for the caller to close. The writer-end open pairs with
// the blocked reader open, so both complete as events.
func unblockFIFOOpener(t *testing.T, path string, opened <-chan *os.File, deadline time.Duration) *os.File {
	t.Helper()
	pw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Errorf("FIFO disposal writer open failed; opener goroutine may remain blocked: %v", err)
		return nil
	}
	_ = pw.Close()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case f := <-opened:
		return f
	case <-timer.C:
		t.Errorf("FIFO opener goroutine did not exit after disposal")
		return nil
	}
}

// readFixturePGID reads a process-group id the fixture child recorded for
// itself through ps at runtime.
func readFixturePGID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture pgid file %s: %v", path, err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pgid <= 0 {
		t.Fatalf("fixture pgid file %s holds %q: %v", path, string(b), err)
	}
	return pgid
}

// recordFailureFixture owns every leak-prone resource of the failed-record
// regression: the Serve goroutine, the record barrier, the FIFO channel, its
// pump, and the fixture process group. dispose is idempotent and runs on
// every path, including paths that fail before the group identity is known.
type recordFailureFixture struct {
	t *testing.T

	process  *ManagedProcess
	store    *fakeLeaseStore
	signaler *recordingSignaler

	marker         string
	leaderPGIDFile string
	descMarker     string
	descPGIDFile   string
	fifoPath       string

	recordStarted chan struct{}
	recordRelease chan struct{}
	releaseOnce   sync.Once

	serveDone     chan error
	serveErr      error
	serveReturned bool

	fifoFile   *os.File
	pump       *fifoPump
	pumpExited bool

	wantDelegatedCalls int
}

// newRecordFailureFixture builds the deterministic failed-record fixture.
// The leader records its own pgid, opens the FIFO write end, then spawns a
// descendant that inherits both the write end and the process group. The
// descendant announces readiness only after recording its own pgid, then
// blocks in sleep while the leader stays alive in wait. Every group member
// therefore holds the FIFO write end, so a read EOF on the test-owned read
// end proves the whole group is dead; a surviving descendant keeps the write
// end open and the read blocks.
func newRecordFailureFixture(t *testing.T) *recordFailureFixture {
	psPath, err := exec.LookPath("ps")
	if err != nil {
		t.Fatalf("ps required for fixture group evidence: %v", err)
	}
	dir := t.TempDir()
	fx := &recordFailureFixture{
		t:              t,
		marker:         filepath.Join(dir, "marker"),
		leaderPGIDFile: filepath.Join(dir, "leader_pgid"),
		descMarker:     filepath.Join(dir, "descendant_marker"),
		descPGIDFile:   filepath.Join(dir, "descendant_pgid"),
		fifoPath:       filepath.Join(dir, "ready.fifo"),
		recordStarted:  make(chan struct{}),
		recordRelease:  make(chan struct{}),
		serveDone:      make(chan error, 1),
	}
	if out, err := exec.Command("mkfifo", fx.fifoPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	script := `touch "$MARKER"; "$PS" -o pgid= -p $$ > "$LEADER_PGID"; exec 3>"$FIFO"; sh -c 'touch "$DESC_MARKER"; "$PS" -o pgid= -p $$ > "$DESC_PGID"; echo ready >&3; exec sleep 60' & wait`
	serverCfg := &config.ServerConfig{
		Command:   "/bin/sh",
		Transport: config.TransportManagedHTTP,
		Args:      []string{"-c", script},
		Env: map[string]string{
			"MARKER":      fx.marker,
			"LEADER_PGID": fx.leaderPGIDFile,
			"DESC_MARKER": fx.descMarker,
			"DESC_PGID":   fx.descPGIDFile,
			"FIFO":        fx.fifoPath,
			"PS":          psPath,
		},
	}
	fx.store = &fakeLeaseStore{
		recordStarted: fx.recordStarted,
		recordRelease: fx.recordRelease,
		recordErr:     errors.New("lease store rejected record"),
	}
	fx.signaler = &recordingSignaler{real: ownership.NewSignaler()}
	cfg := config.SupervisionConfig{}
	cfg.ApplyDefaults()
	cfg.ShutdownTimeout = config.Duration(100 * time.Millisecond)
	fx.process = NewManagedProcessWithOwnership("failure", serverCfg, cfg, slog.Default(), fx.store, "daemon",
		WithProcReader(&fakeProcReader{}),
		WithSignaler(fx.signaler),
		WithTokenGenerator(func() (string, error) { return "vsn27-owner-token", nil }),
	)
	fx.wantDelegatedCalls = expectedDelegatedGroupSignals()
	return fx
}

// start registers disposal before any failure-prone assertion and launches
// Serve. Cleanup is armed first, so a failure between start and the first
// handshake still disposes of every fixture resource.
func (fx *recordFailureFixture) start() {
	fx.t.Cleanup(fx.dispose)
	go func() { fx.serveDone <- fx.process.Serve(context.Background()) }()
}

// awaitRecordStarted waits a bounded, event-driven interval for the lease
// record to begin. A Serve exit before the record is itself the failure, so
// that outcome has its own select branch instead of an unbounded receive.
func (fx *recordFailureFixture) awaitRecordStarted() {
	fx.t.Helper()
	timer := time.NewTimer(fixtureHandshakeTimeout)
	defer timer.Stop()
	select {
	case <-fx.recordStarted:
	case err := <-fx.serveDone:
		fx.serveErr, fx.serveReturned = err, true
		fx.t.Fatalf("Serve returned before the lease record started: %v", err)
	case <-timer.C:
		fx.t.Fatal("lease record never started within deadline")
	}
}

// attachChannel hands the opened FIFO channel and its pump to the fixture so
// dispose can join the pump and close the file on every path.
func (fx *recordFailureFixture) attachChannel(f *os.File, pump *fifoPump) {
	fx.fifoFile, fx.pump = f, pump
}

// releaseRecordBarrier releases the lease record exactly once on any path.
func (fx *recordFailureFixture) releaseRecordBarrier() {
	fx.releaseOnce.Do(func() { close(fx.recordRelease) })
}

// awaitServe waits bounded for Serve to return and records the outcome. Only
// an actual receive from serveDone marks the result owned: a timed-out wait
// consumes nothing, so disposal performs its own bounded receive after the
// group kill and still joins a late Serve result. The return reports whether
// the result arrived; callers decide whether a timeout fails the test. Every
// caller runs on the test goroutine: the test body and t.Cleanup are
// sequential, so the plain serveReturned marker needs no lock.
func (fx *recordFailureFixture) awaitServe(deadline time.Duration) bool {
	if fx.serveReturned {
		return true
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case err := <-fx.serveDone:
		fx.serveErr, fx.serveReturned = err, true
		return true
	case <-timer.C:
		return false
	}
}

// dispose releases every fixture barrier and process on any path. The child
// leads its own process group (Setpgid), so a negative kill on the child pid
// covers the leader and every descendant even when a body assertion failed
// before the pgid files were verified. dispose is safe to call twice: a
// direct early-failure call and the t.Cleanup call are both no-ops after the
// first run.
func (fx *recordFailureFixture) dispose() {
	fx.releaseRecordBarrier()
	if pid := fx.process.PID(); pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if fx.pump != nil {
		timer := time.NewTimer(fixtureHandshakeTimeout)
		select {
		case <-fx.pump.exited:
		case <-timer.C:
			fx.t.Errorf("fixture FIFO pump did not exit after group kill")
		}
		timer.Stop()
		_ = fx.fifoFile.Close()
		fx.pumpExited = true
	}
	if !fx.awaitServe(fixtureHandshakeTimeout) {
		fx.t.Errorf("disposal could not join the Serve goroutine after the group kill")
	}
	// Reap the leader when a failed path skipped production cleanup. The read
	// follows the serveDone handoff, so the Serve goroutine has exited.
	if fx.serveReturned {
		fx.process.mu.RLock()
		cmd := fx.process.cmd
		fx.process.mu.RUnlock()
		if cmd != nil && cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	}
}

// TestManagedProcessRecordFailureKillsGroupAndNeverRuns pins the failed
// lease-record lifecycle: the record stays blocked until the child is ready,
// the marker proves execution before the failure, the failure kills the
// child's actual process group through the delegated platform signaler, the
// leader is reaped before Serve returns, and StateRunning is never published.
func TestManagedProcessRecordFailureKillsGroupAndNeverRuns(t *testing.T) {
	fx := newRecordFailureFixture(t)
	fx.start()

	// While the record is blocked the lifecycle is exactly StateStarting/0.
	fx.awaitRecordStarted()
	if state, generation := fx.process.LifecycleSnapshot(); state != StateStarting || generation != 0 {
		t.Fatalf("lifecycle while record blocked = %s/%d, want %s/0", state, generation, StateStarting)
	}

	// Child readiness before the record failure: FIFO open, readiness line,
	// markers, and both pgid identities are positive execution evidence.
	fifo := openFixtureFIFO(t, fx.fifoPath, fixtureHandshakeTimeout)
	pump := startFixturePump(fifo)
	fx.attachChannel(fifo, pump)
	if line := awaitFixtureLine(t, pump, fixtureHandshakeTimeout); line != "ready" {
		t.Fatalf("fixture readiness = %q, want ready", line)
	}
	if _, err := os.Stat(fx.marker); err != nil {
		t.Fatalf("marker missing at readiness; child execution not evidenced before failed record: %v", err)
	}
	if _, err := os.Stat(fx.descMarker); err != nil {
		t.Fatalf("descendant marker missing at readiness: %v", err)
	}
	leaderPGID := readFixturePGID(t, fx.leaderPGIDFile)
	descPGID := readFixturePGID(t, fx.descPGIDFile)
	if leaderPGID != fx.process.PID() {
		t.Fatalf("leader pgid %d does not match spawned pid %d", leaderPGID, fx.process.PID())
	}
	if descPGID != leaderPGID {
		t.Fatalf("descendant pgid %d != leader pgid %d: descendant not in the child process group", descPGID, leaderPGID)
	}

	fx.releaseRecordBarrier()
	if !fx.awaitServe(fixtureHandshakeTimeout) {
		t.Fatal("Serve did not return after failed lease record")
	}
	serveErr := fx.serveErr
	if serveErr == nil {
		t.Fatal("Serve returned nil for failed lease record")
	}
	if strings.Contains(serveErr.Error(), ownership.OwnerTokenEnvKey) || strings.Contains(serveErr.Error(), "vsn27-owner-token") {
		t.Fatalf("returned error exposes ownership secret: %v", serveErr)
	}

	calls := fx.signaler.recorded()
	if len(calls) != fx.wantDelegatedCalls {
		t.Fatalf("delegated group signal calls = %d (%v), want %d on this platform's production route", len(calls), calls, fx.wantDelegatedCalls)
	}
	if fx.wantDelegatedCalls > 0 {
		if calls[0].pgid != leaderPGID {
			t.Fatalf("kill targeted pgid %d, want the child's actual process group %d", calls[0].pgid, leaderPGID)
		}
		if calls[0].signal != syscall.SIGKILL {
			t.Fatalf("recorded signal = %v, want SIGKILL", calls[0].signal)
		}
	}

	fx.process.mu.RLock()
	processState := fx.process.cmd.ProcessState
	leaseActive := fx.process.leaseActive
	fx.process.mu.RUnlock()
	if processState == nil {
		t.Fatal("leader not reaped before Serve returned: ProcessState missing")
	}
	if processState.Success() {
		t.Fatal("leader exit status reports success; expected signal death")
	}
	if leaseActive {
		t.Fatal("lease still active after failed record")
	}
	if state, generation := fx.process.LifecycleSnapshot(); state != StateCrashed || generation != 0 {
		t.Fatalf("lifecycle after failure = %s/%d, want %s/0", state, generation, StateCrashed)
	}

	// Group-kill discrimination: every group member holds the FIFO write
	// end, so the read end only reaches EOF when the kill was group-wide.
	awaitFixtureEOF(t, pump, fixtureHandshakeTimeout)
}

// TestManagedProcessRecordFailureFixtureCleanupOnEarlyFailure exercises the
// disposal exactly where an earlier revision stranded resources: a body
// failure before the group identity is verified. Calling dispose directly is
// what t.Cleanup would do on that early fatal path; the assertions prove the
// barrier was released, Serve finished, the pump read was unblocked and
// joined, and the leader was reaped, with no fixture process left behind.
func TestManagedProcessRecordFailureFixtureCleanupOnEarlyFailure(t *testing.T) {
	fx := newRecordFailureFixture(t)
	fx.start()
	fx.awaitRecordStarted()

	// Attach the channel, then dispose before any group assertion: the
	// disposal must still kill the fixture group and unblock the pump read.
	fifo := openFixtureFIFO(t, fx.fifoPath, fixtureHandshakeTimeout)
	pump := startFixturePump(fifo)
	fx.attachChannel(fifo, pump)
	fx.dispose()

	if !fx.serveReturned {
		t.Fatal("early-failure disposal did not return Serve")
	}
	if fx.serveErr == nil {
		t.Fatal("early-failure disposal lost the lease record error")
	}
	if !fx.pumpExited {
		t.Fatal("early-failure disposal did not join the FIFO pump")
	}
	fx.process.mu.RLock()
	processState := fx.process.cmd.ProcessState
	fx.process.mu.RUnlock()
	if processState == nil {
		t.Fatal("early-failure disposal left the leader unreaped")
	}
}

// TestManagedProcessRecordFailureDisposalJoinsLateServeAfterBodyTimeout pins
// the join-ownership rule of the disposal: a body wait whose deadline expires
// before Serve returns must consume nothing, so the disposal still performs
// its own bounded receive after the group kill, joins the late Serve result,
// and observes the reaped leader.
func TestManagedProcessRecordFailureDisposalJoinsLateServeAfterBodyTimeout(t *testing.T) {
	fx := newRecordFailureFixture(t)
	fx.start()
	fx.awaitRecordStarted()

	// The record barrier is still held, so Serve is blocked in the lease
	// record and cannot return: the short body deadline always expires.
	if state, generation := fx.process.LifecycleSnapshot(); state != StateStarting || generation != 0 {
		t.Fatalf("lifecycle during body wait = %s/%d, want %s/0", state, generation, StateStarting)
	}
	if fx.awaitServe(recordFailureBodyWaitDeadline) {
		t.Fatal("Serve returned while the lease record was still blocked")
	}
	if fx.serveReturned {
		t.Fatal("timed-out body wait consumed the Serve result before disposal")
	}

	// Disposal releases the barrier, kills the group, and must still join
	// the late Serve result and observe the reaped leader.
	fx.dispose()

	if !fx.serveReturned {
		t.Fatal("disposal after a timed-out body wait did not join the late Serve result")
	}
	if fx.serveErr == nil {
		t.Fatal("disposal after a timed-out body wait lost the lease record error")
	}
	fx.process.mu.RLock()
	processState := fx.process.cmd.ProcessState
	fx.process.mu.RUnlock()
	if processState == nil {
		t.Fatal("disposal after a timed-out body wait left the leader unreaped")
	}
}

// TestOpenFixtureFIFOTimeoutDisposal pins the FIFO open deadline disposal. A
// read opener blocked on a FIFO that no child will ever open must be released
// by the writer-end pairing, and its late file must be closeable, leaving no
// goroutine blocked on the FIFO.
func TestOpenFixtureFIFOTimeoutDisposal(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "never-opened.fifo")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
		if err != nil {
			f = nil
		}
		opened <- f
	}()
	late := unblockFIFOOpener(t, fifo, opened, fixtureHandshakeTimeout)
	if late == nil {
		t.Fatal("disposal did not recover the late FIFO read end")
	}
	if err := late.Close(); err != nil {
		t.Fatalf("late FIFO read end not disposable: %v", err)
	}
}
