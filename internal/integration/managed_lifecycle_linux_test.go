//go:build linux

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

const (
	fixtureOwnerReadyFile = "VISION_FIXTURE_OWNER_READY_FILE"
	fixtureRoot           = "VISION_FIXTURE_OWNERSHIP_ROOT"
	fixtureBackendPort    = "VISION_FIXTURE_BACKEND_PORT"
	fixtureProxyPort      = "VISION_FIXTURE_PROXY_PORT"
	fixtureBinary         = "VISION_FIXTURE_BINARY"
	fixtureWatchdogPID    = "VISION_FIXTURE_WATCHDOG_PID"
	fixtureServerName     = "managed-lifecycle-fixture"
)

// TestManagedLifecycleBackend is a deliberately small native HTTP/MCP backend.
// It is launched by the product supervisor, not by the test process, so the
// tests exercise real process groups, procfs identity, and bind behavior.
func TestManagedLifecycleBackend(t *testing.T) {
	port, err := fixtureArg("-port")
	if err != nil {
		port, err = strconv.Atoi(os.Getenv(fixtureBackendPort))
		if err != nil || port <= 0 {
			t.Skip("fixture backend helper")
		}
	}
	watchdog := os.Getenv(fixtureWatchdogPID)
	if watchdog != "" {
		pid, err := strconv.Atoi(watchdog)
		if err != nil || pid <= 0 {
			t.Fatalf("invalid fixture watchdog pid %q", watchdog)
		}
		go stopWhenParentDies(pid)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

// TestManagedLifecycleOwner starts a managed backend through the real
// Supervisor/Registry ownership path, then waits for the parent to kill this
// owner abruptly. The backend is intentionally a shell descendant, proving the
// orphaned process-group case rather than only direct-child behavior.
func TestManagedLifecycleOwner(t *testing.T) {
	root := os.Getenv(fixtureRoot)
	backendPort, err := strconv.Atoi(os.Getenv(fixtureBackendPort))
	if err != nil || backendPort <= 0 {
		t.Skip("fixture owner helper")
	}
	proxyPort, err := strconv.Atoi(os.Getenv(fixtureProxyPort))
	if err != nil || proxyPort <= 0 {
		t.Skip("fixture owner helper")
	}
	readyFile := os.Getenv(fixtureOwnerReadyFile)
	if root == "" || readyFile == "" {
		t.Skip("fixture owner helper")
	}

	store, err := ownership.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := managedFixtureConfig(os.Getenv(fixtureBinary), backendPort, proxyPort, os.Getenv(fixtureWatchdogPID))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := supervisor.NewWithOptions(fixtureSupervision(), slog.New(slog.NewTextHandler(io.Discard, nil)), supervisor.WithLeaseStore(store, "fixture-owner"))
	reg := server.NewRegistry(sup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.ServeBackground(ctx)
	if err := reg.Add(fixtureServerName, cfg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Start(fixtureServerName); err != nil {
		t.Fatal(err)
	}
	managed := reg.Get(fixtureServerName).Process
	if managed == nil {
		t.Fatal("managed process was not registered")
	}
	waitForProcessState(t, managed, supervisor.StateRunning, 10*time.Second)
	if err := os.WriteFile(readyFile, []byte("OWNER_READY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestManagedLifecycle_ReclaimsAbruptOwnerAndReplacesBackend(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	backendPort := assignedLoopbackPort(t)
	proxyPort := assignedLoopbackPort(t)
	binary := os.Args[0]
	watchdogPID := strconv.Itoa(os.Getpid())
	cfg := managedFixtureConfig(binary, backendPort, proxyPort, watchdogPID)
	target := fmt.Sprintf("http://127.0.0.1:%d/mcp", backendPort)

	owner := startFixtureOwner(t, root, binary, backendPort, proxyPort, watchdogPID)
	waitForMarker(t, owner.readyFile, "OWNER_READY", 10*time.Second)
	waitForBackendReady(t, target, 15*time.Second)
	store, err := ownership.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.Read(fixtureServerName)
	if err != nil {
		t.Fatalf("owner did not publish a lease: %v", err)
	}
	registerEmergencyGroupCleanup(t, stale.LeaderPGID, "stale managed backend")

	// Kill only the owner group. Cleanup was registered in startFixtureOwner
	// immediately after Setpgid/Pdeathsig discovery; the managed group must
	// remain alive until the real reconciler acts.
	if err := syscall.Kill(-owner.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	if err := owner.wait(5 * time.Second); err != nil && !isKilledExit(err) {
		t.Fatalf("owner did not exit after SIGKILL: %v", err)
	}
	waitForBackendReady(t, target, 2*time.Second)
	if _, err := store.Read(fixtureServerName); err != nil {
		t.Fatalf("stale lease disappeared before reconciliation: %v", err)
	}

	comp := newFixtureComposition(t, root, "replacement-daemon")
	reconcileStarted := time.Now()
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	results := (&ownership.Reconciler{
		Store:       store,
		ProcReader:  ownership.NewLinuxProcReader(""),
		Signaler:    ownership.NewSignaler(),
		TermTimeout: 2 * time.Second,
		KillTimeout: 2 * time.Second,
	}).Reconcile(reconcileCtx, map[string]ownership.ServerIdentity{fixtureServerName: fixtureIdentity(cfg)})
	cancel()
	if len(results) != 1 || results[0].Status != "reclaimed" {
		t.Fatalf("reconciliation results = %#v, want one reclaimed result", results)
	}
	if _, err := store.Read(fixtureServerName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale lease after reconciliation = %v, want removed", err)
	}

	if err := comp.start(fixtureServerName, cfg); err != nil {
		t.Fatal(err)
	}
	waitForProcessState(t, comp.reg.Get(fixtureServerName).Process, supervisor.StateRunning, 10*time.Second)
	waitForBackendReadyBy(t, target, reconcileStarted.Add(15*time.Second))
	if elapsed := time.Since(reconcileStarted); elapsed > 15*time.Second {
		t.Fatalf("replacement became ready after reconciliation deadline: %s", elapsed)
	}
	replacement, err := store.Read(fixtureServerName)
	if err != nil {
		t.Fatalf("replacement did not publish a lease: %v", err)
	}
	if replacement.DaemonID == stale.DaemonID || replacement.LeaderPID == stale.LeaderPID || replacement.LeaderPGID == stale.LeaderPGID {
		t.Fatalf("replacement lease was not replaced: stale=%+v replacement=%+v", stale, replacement)
	}

	if err := comp.stopForAssertion(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLifecycle_UnknownOwnerCausesBindFailureWithoutKill(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	backendPort := assignedLoopbackPort(t)
	proxyPort := assignedLoopbackPort(t)
	binary := os.Args[0]
	watchdogPID := strconv.Itoa(os.Getpid())
	cfg := managedFixtureConfig(binary, backendPort, proxyPort, watchdogPID)
	target := fmt.Sprintf("http://127.0.0.1:%d/mcp", backendPort)

	unknown := startFixtureBackend(t, backendPort, watchdogPID)
	waitForBackendReady(t, target, 15*time.Second)
	store, err := ownership.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	comp := newFixtureComposition(t, root, "conflict-daemon")
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	results := (&ownership.Reconciler{
		Store:       store,
		ProcReader:  ownership.NewLinuxProcReader(""),
		Signaler:    ownership.NewSignaler(),
		TermTimeout: time.Second,
		KillTimeout: time.Second,
	}).Reconcile(reconcileCtx, map[string]ownership.ServerIdentity{fixtureServerName: fixtureIdentity(cfg)})
	cancel()
	if len(results) != 0 {
		t.Fatalf("empty store reconciliation = %#v, want no kill target", results)
	}

	if err := comp.start(fixtureServerName, cfg); err != nil {
		t.Fatal(err)
	}
	managed := comp.reg.Get(fixtureServerName).Process
	waitForProcessState(t, managed, supervisor.StateFailed, 10*time.Second)
	if managed.LastError() == nil {
		t.Fatal("configured backend bind conflict had no startup error")
	}
	// The unknown process remains the sole listener and remains observable;
	// no port-owner kill path is used by startup or reconciliation.
	waitForBackendReady(t, target, 2*time.Second)
	if members, err := ownership.NewLinuxProcReader("").ListMembers(unknown.pgid); err != nil || len(members) == 0 {
		t.Fatalf("unknown process group disappeared: members=%v err=%v", members, err)
	}
	if _, err := store.Read(fixtureServerName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bind-conflict path left a lease: %v", err)
	}
	if err := comp.stopForAssertion(); err != nil {
		t.Fatal(err)
	}
}

type fixtureOwner struct {
	cmd       *exec.Cmd
	pgid      int
	readyFile string
	waitOnce  sync.Once
	waitDone  chan error
}

func startFixtureOwner(t *testing.T, root, binary string, backendPort, proxyPort int, watchdogPID string) *fixtureOwner {
	t.Helper()
	readyFile := filepath.Join(t.TempDir(), "owner.ready")
	ready, err := os.OpenFile(readyFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=TestManagedLifecycleOwner", "-test.v")
	cmd.Stdout = ready
	cmd.Stderr = io.Discard
	cmd.Env = append(os.Environ(),
		fixtureRoot+"="+root,
		fixtureBackendPort+"="+strconv.Itoa(backendPort),
		fixtureProxyPort+"="+strconv.Itoa(proxyPort),
		fixtureBinary+"="+binary,
		fixtureWatchdogPID+"="+watchdogPID,
		fixtureOwnerReadyFile+"="+readyFile,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		ready.Close()
		t.Fatal(err)
	}
	_ = ready.Close()
	owner := &fixtureOwner{cmd: cmd, pgid: cmd.Process.Pid, readyFile: readyFile, waitDone: make(chan error, 1)}
	// Register parent cleanup immediately after process-group discovery, before
	// reading readiness or inspecting any lease.
	t.Cleanup(func() {
		_ = syscall.Kill(-owner.pgid, syscall.SIGKILL)
		_ = owner.wait(5 * time.Second)
	})
	go func() { owner.waitDone <- cmd.Wait() }()
	return owner
}

func registerEmergencyGroupCleanup(t *testing.T, pgid int, label string) {
	t.Helper()
	if pgid <= 0 {
		t.Fatalf("invalid %s process group: %d", label, pgid)
	}
	// This is deliberately registered only after a valid lease proves the
	// group is test-owned. It never discovers a process through the port.
	t.Cleanup(func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("emergency cleanup %s group %d: SIGKILL: %v", label, pgid, err)
		}
		if err := waitForGroupEmpty(pgid, 5*time.Second); err != nil {
			t.Errorf("emergency cleanup %s group %d: %v", label, pgid, err)
		}
	})
}

func (o *fixtureOwner) wait(timeout time.Duration) error {
	var err error
	o.waitOnce.Do(func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err = <-o.waitDone:
		case <-timer.C:
			err = fmt.Errorf("process wait timed out after %s", timeout)
		}
	})
	return err
}

func isKilledExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

func startFixtureBackend(t *testing.T, port int, watchdogPID string) *fixtureOwner {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestManagedLifecycleBackend", "-test.v", "--", "-port", strconv.Itoa(port))
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), fixtureWatchdogPID+"="+watchdogPID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	backend := &fixtureOwner{cmd: cmd, pgid: cmd.Process.Pid, waitDone: make(chan error, 1)}
	t.Cleanup(func() {
		_ = syscall.Kill(-backend.pgid, syscall.SIGKILL)
		_ = backend.wait(5 * time.Second)
	})
	go func() { backend.waitDone <- cmd.Wait() }()
	return backend
}

type fixtureComposition struct {
	store                 *ownership.Store
	reg                   *server.Registry
	cancel                context.CancelFunc
	assertionStopComplete bool
	once                  sync.Once
}

func newFixtureComposition(t *testing.T, root, daemonID string) *fixtureComposition {
	t.Helper()
	store, err := ownership.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sup := supervisor.NewWithOptions(fixtureSupervision(), slog.New(slog.NewTextHandler(io.Discard, nil)), supervisor.WithLeaseStore(store, daemonID))
	reg := server.NewRegistry(sup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.ServeBackground(ctx)
	comp := &fixtureComposition{store: store, reg: reg, cancel: cancel}
	t.Cleanup(func() {
		if !comp.assertionStopComplete {
			comp.emergencyCleanup()
		}
	})
	return comp
}

func (c *fixtureComposition) start(name string, cfg *config.ServerConfig) error {
	if err := c.reg.Add(name, cfg); err != nil {
		return err
	}
	return c.reg.Start(name)
}

func (c *fixtureComposition) stopForAssertion() error {
	var stopErr error
	c.once.Do(func() {
		leaseBeforeStop, err := c.store.Read(fixtureServerName)
		hadLease := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			stopErr = fmt.Errorf("read lease before production cleanup: %w", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.reg.StopAll(ctx); err != nil {
			stopErr = fmt.Errorf("production registry cleanup failed: %w", err)
		}
		// StopAll waits for each managed process to finish its own lifecycle
		// cleanup. Cancel the supervisor only after that completion is observed.
		c.cancel()
		if stopErr != nil {
			return
		}
		if hadLease {
			if err := waitForGroupEmpty(leaseBeforeStop.LeaderPGID, 5*time.Second); err != nil {
				stopErr = fmt.Errorf("managed backend group remained after production cleanup: %w", err)
				return
			}
		}
		if err := waitForLeaseAbsent(c.store, fixtureServerName, 5*time.Second); err != nil {
			stopErr = fmt.Errorf("production cleanup left managed backend lease: %w", err)
			return
		}
		c.assertionStopComplete = true
	})
	return stopErr
}

func (c *fixtureComposition) emergencyCleanup() {
	lease, err := c.store.Read(fixtureServerName)
	if err != nil {
		return
	}
	_ = syscall.Kill(-lease.LeaderPGID, syscall.SIGKILL)
	_ = waitForGroupEmpty(lease.LeaderPGID, 5*time.Second)
}

func managedFixtureConfig(binary string, backendPort, proxyPort int, watchdogPID string) *config.ServerConfig {
	return &config.ServerConfig{
		Port:          proxyPort,
		Transport:     config.TransportManagedHTTP,
		Command:       "sh",
		Args:          []string{"-c", `"$VISION_FIXTURE_BINARY" -test.run=TestManagedLifecycleBackend -test.v -- -port "$VISION_FIXTURE_BACKEND_PORT" & child=$!; wait "$child"`},
		URL:           fmt.Sprintf("http://127.0.0.1:%d/mcp", backendPort),
		Env:           map[string]string{fixtureBinary: binary, fixtureBackendPort: strconv.Itoa(backendPort), fixtureWatchdogPID: watchdogPID},
		RestartPolicy: config.RestartNever,
	}
}

func fixtureIdentity(cfg *config.ServerConfig) ownership.ServerIdentity {
	return ownership.ServerIdentity{Name: fixtureServerName, Command: cfg.Command, Args: cfg.Args, URL: cfg.URL, Transport: string(cfg.InferTransport()), Env: cfg.Env}
}

func fixtureSupervision() config.SupervisionConfig {
	return config.SupervisionConfig{ShutdownTimeout: config.Duration(2 * time.Second), RestartDelay: config.Duration(10 * time.Millisecond), MaxRestartDelay: config.Duration(100 * time.Millisecond)}
}

func fixtureArg(name string) (int, error) {
	for i, arg := range os.Args {
		if arg == "--" && i+2 < len(os.Args) && os.Args[i+1] == name {
			port, err := strconv.Atoi(os.Args[i+2])
			if err != nil || port <= 0 {
				return 0, fmt.Errorf("invalid %s", name)
			}
			return port, nil
		}
	}
	return 0, fmt.Errorf("missing %s", name)
}

func assignedLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitForBackendReady(t *testing.T, target string, timeout time.Duration) {
	t.Helper()
	waitForBackendReadyBy(t, target, time.Now().Add(timeout))
}

func waitForBackendReadyBy(t *testing.T, target string, deadline time.Time) {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := probeManagedBackend(probeCtx, target)
		probeCancel()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("backend %s did not become ready: %v", target, err)
		case <-ticker.C:
		}
	}
}

func probeManagedBackend(ctx context.Context, target string) error {
	url, err := netUrl(target)
	if err != nil {
		return err
	}
	return mcp.ProbeManagedHTTPBackend(ctx, url, nil)
}

func netUrl(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

func waitForProcessState(t *testing.T, process *supervisor.ManagedProcess, want supervisor.ServiceState, timeout time.Duration) {
	t.Helper()
	if process == nil {
		t.Fatal("nil managed process")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if process.State() == want {
			return
		}
		select {
		case <-process.LifecycleEvents():
		case <-deadline.C:
			t.Fatalf("managed process state=%s, want %s", process.State(), want)
		}
	}
}

func waitForMarker(t *testing.T, path, marker string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), marker) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("marker %q not written to %s", marker, path)
		case <-ticker.C:
		}
	}
}

func waitForGroupEmpty(pgid int, timeout time.Duration) error {
	reader := ownership.NewLinuxProcReader("")
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		members, err := reader.ListMembers(pgid)
		if err == nil && len(members) == 0 {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("process group %d did not become empty", pgid)
		case <-ticker.C:
		}
	}
}

func waitForLeaseAbsent(store *ownership.Store, name string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := store.Read(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-deadline.C:
			return errors.New("lease still present at deadline")
		case <-ticker.C:
		}
	}
}

func stopWhenParentDies(pid int) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			os.Exit(0)
		}
	}
}
