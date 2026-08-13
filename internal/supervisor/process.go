package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
	"github.com/thejerf/suture/v4"
)

// ManagedProcess represents a supervised MCP server process.
// It implements suture.Service for integration with the supervision tree.
type ManagedProcess struct {
	name      string
	config    *config.ServerConfig
	supConfig config.SupervisionConfig
	logger    *slog.Logger

	// Runtime state (protected by mu)
	mu                sync.RWMutex
	state             ServiceState
	cmd               *exec.Cmd
	pid               int
	startedAt         time.Time
	restartCount      int
	lastError         error
	generation        uint64
	pendingGeneration uint64
	lifecycle         chan struct{}
	leaseStore        ownership.LeaseStore
	daemonID          string
	lease             ownership.Lease
	leaseActive       bool
	ownerToken        string
	tokenGenerator    func() (string, error)
	procReader        ownership.ProcReader
	signaler          ownership.Signaler

	// Suture integration
	token suture.ServiceToken

	// Process I/O (protected by ioMu - separate from mu to avoid holding mu during I/O)
	ioMu   sync.RWMutex
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

// NewManagedProcess creates a new managed process for the given server config.
// Managed-http ownership is Linux-only for now: non-Linux proc identity reads
// fail closed before publication, so the backend is not silently unleased.
func NewManagedProcess(name string, cfg *config.ServerConfig, supCfg config.SupervisionConfig, logger *slog.Logger) *ManagedProcess {
	return NewManagedProcessWithOwnership(name, cfg, supCfg, logger, nil, "")
}

type ProcessOption func(*ManagedProcess)

func WithTokenGenerator(generator func() (string, error)) ProcessOption {
	return func(p *ManagedProcess) { p.tokenGenerator = generator }
}

func WithProcReader(reader ownership.ProcReader) ProcessOption {
	return func(p *ManagedProcess) { p.procReader = reader }
}

func WithSignaler(signaler ownership.Signaler) ProcessOption {
	return func(p *ManagedProcess) { p.signaler = signaler }
}

func NewManagedProcessWithOwnership(name string, cfg *config.ServerConfig, supCfg config.SupervisionConfig, logger *slog.Logger, store ownership.LeaseStore, daemonID string, options ...ProcessOption) *ManagedProcess {
	p := &ManagedProcess{
		name:           name,
		config:         cfg,
		supConfig:      supCfg,
		logger:         logger.With(slog.String("server", name)),
		state:          StateStopped,
		lifecycle:      make(chan struct{}, 1),
		leaseStore:     store,
		daemonID:       daemonID,
		tokenGenerator: ownership.GenerateOwnerToken,
		procReader:     ownership.NewLinuxProcReader(""),
		signaler:       ownership.NewSignaler(),
	}
	for _, option := range options {
		option(p)
	}
	return p
}

// Serve implements suture.Service.
// It spawns the subprocess and blocks until it exits or context is cancelled.
func (p *ManagedProcess) Serve(ctx context.Context) error {
	p.mu.Lock()
	p.state = StateStarting
	p.mu.Unlock()
	p.notifyLifecycle()

	// HTTP/SSE entries point at externally owned servers and need no child.
	// Managed HTTP owns both a command and loopback URL, so it follows the
	// supervised subprocess path while retaining native HTTP transport.
	transport := p.config.InferTransport()
	if transport != config.TransportStdio && transport != config.TransportManagedHTTP {
		return p.serveProxy(ctx)
	}

	if transport == config.TransportManagedHTTP {
		if err := p.prepareOwnerToken(); err != nil {
			p.mu.Lock()
			p.state, p.lastError = StateCrashed, err
			p.mu.Unlock()
			return err
		}
	}
	// Spawn the subprocess
	if err := p.spawn(ctx); err != nil {
		p.mu.Lock()
		p.state = StateCrashed
		p.lastError = err
		p.mu.Unlock()
		p.notifyLifecycle()
		return err
	}
	if transport == config.TransportManagedHTTP && p.leaseStore != nil {
		if err := p.recordLease(); err != nil {
			_ = p.forceKill()
			_ = p.cmd.Wait()
			_ = p.waitGroupEmpty(p.supConfig.ShutdownTimeout.Duration())
			p.mu.Lock()
			p.state = StateCrashed
			p.lastError = err
			p.mu.Unlock()
			return err
		}
	}

	p.mu.Lock()
	p.state = StateRunning
	p.startedAt = time.Now()
	if p.pendingGeneration > 0 {
		p.generation = p.pendingGeneration
		p.pendingGeneration = 0
	}
	p.mu.Unlock()
	p.notifyLifecycle()

	p.logger.Info("server started",
		slog.Int("pid", p.pid),
		slog.Int("port", p.config.Port),
	)

	// Start stderr collector to prevent stream corruption
	go p.collectStderr()
	if transport == config.TransportManagedHTTP {
		// Native HTTP servers do not use stdout for protocol traffic. Drain and
		// log it so a verbose child cannot block on a full pipe.
		go p.collectStdout()
	}

	// Wait for process exit or context cancellation
	return p.wait(ctx)
}

// spawn starts the subprocess with proper configuration.
func (p *ManagedProcess) spawn(_ context.Context) error {
	// Resolve command path
	cmdPath, err := exec.LookPath(p.config.Command)
	if err != nil {
		return fmt.Errorf("command not found: %s: %w", p.config.Command, err)
	}

	// Create command (not using CommandContext - we handle termination ourselves)
	p.cmd = exec.Command(cmdPath, p.config.Args...)

	// Set up process group for clean termination
	p.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // Create new process group
	}

	// Merge environment variables
	p.cmd.Env = p.buildEnv()

	// Set up pipes for stdio (protected by ioMu)
	var err1, err2, err3 error
	stdin, err1 := p.cmd.StdinPipe()
	stdout, err2 := p.cmd.StdoutPipe()
	stderr, err3 := p.cmd.StderrPipe()
	if err := errors.Join(err1, err2, err3); err != nil {
		return fmt.Errorf("failed to create pipes: %w", err)
	}
	p.ioMu.Lock()
	p.stdin = stdin
	p.stdout = stdout
	p.stderr = stderr
	p.ioMu.Unlock()

	// Start the process
	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	p.mu.Lock()
	p.pid = p.cmd.Process.Pid
	p.mu.Unlock()

	return nil
}

// buildEnv creates the environment for the subprocess.
// It merges the current environment with server-specific variables.
func (p *ManagedProcess) prepareOwnerToken() error {
	generator := p.tokenGenerator
	if generator == nil {
		generator = ownership.GenerateOwnerToken
	}
	token, err := generator()
	if err != nil {
		return errors.New("failed to generate backend ownership token")
	}
	p.mu.Lock()
	p.ownerToken = token
	p.pendingGeneration = p.generation + 1
	p.mu.Unlock()
	return nil
}

func (p *ManagedProcess) buildEnv() []string {
	// Start with current environment
	env := os.Environ()

	// Add server-specific environment variables
	for key, value := range p.config.Env {
		if key == ownership.OwnerTokenEnvKey {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	if p.config.InferTransport() == config.TransportManagedHTTP {
		p.mu.RLock()
		token := p.ownerToken
		p.mu.RUnlock()
		env = append(env, ownership.OwnerTokenEnvKey+"="+token)
	}

	return env
}

func (p *ManagedProcess) recordLease() error {
	p.mu.RLock()
	token := p.ownerToken
	reader := p.procReader
	cmd := p.cmd
	p.mu.RUnlock()
	if token == "" || reader == nil || cmd == nil || cmd.Process == nil {
		return errors.New("backend process identity unavailable")
	}
	stat, err := reader.ReadStat(cmd.Process.Pid)
	if err != nil {
		return errors.New("failed to read backend process identity")
	}
	boot, err := reader.ReadBootID()
	if err != nil {
		return errors.New("failed to read backend boot identity")
	}
	exe, err := reader.ReadExe(cmd.Process.Pid)
	if err != nil {
		return errors.New("failed to read backend executable identity")
	}
	p.mu.RLock()
	generation := p.pendingGeneration
	p.mu.RUnlock()
	identity := ownership.ServerIdentity{Name: p.name, Command: p.config.Command, Args: p.config.Args, URL: p.config.URL, Transport: string(p.config.InferTransport()), Env: p.config.Env}
	lease := ownership.Lease{Version: ownership.LeaseSchemaVersion, Generation: generation, ServerName: p.name, DaemonID: p.daemonID, OwnerTokenHash: ownership.TokenHash(token), ConfigHash: ownership.ConfigHash(identity), LeaderPID: cmd.Process.Pid, LeaderPGID: stat.PGRP, LeaderStart: stat.StartTime, BootID: boot, Executable: exe, CreatedAt: time.Now()}
	if err := p.leaseStore.Record(p.name, lease); err != nil {
		return errors.New("failed to record backend ownership lease")
	}
	p.mu.Lock()
	p.lease = lease
	p.leaseActive = true
	p.generation = generation
	p.pendingGeneration = 0
	p.ownerToken = ""
	p.mu.Unlock()
	return nil
}

// wait blocks until the process exits or context is cancelled.
func (p *ManagedProcess) wait(ctx context.Context) error {
	// Channel for process exit
	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case err := <-done:
		cleanupErr := p.releaseLeaseIfEmpty(p.supConfig.ShutdownTimeout.Duration())
		// Process exited
		p.mu.Lock()
		p.restartCount++
		if err != nil {
			p.state = StateCrashed
			p.lastError = err
			p.logger.Warn("server exited with error",
				slog.String("error", err.Error()),
				slog.Int("restarts", p.restartCount),
			)
		} else {
			p.state = StateStopped
			p.logger.Info("server exited normally")
		}
		p.mu.Unlock()
		p.notifyLifecycle()
		if cleanupErr != nil && err == nil {
			return cleanupErr
		}
		if cleanupErr != nil && err != nil {
			return errors.Join(err, cleanupErr)
		}
		return err

	case <-ctx.Done():
		// Graceful shutdown requested
		p.logger.Info("shutdown requested, terminating server")
		if err := p.terminate(); err != nil {
			p.logger.Warn("error during termination",
				slog.String("error", err.Error()),
			)
		}

		// Wait for graceful exit or force kill after timeout
		timeout := p.supConfig.ShutdownTimeout.Duration()
		select {
		case <-done:
			p.logger.Debug("process terminated gracefully")
		case <-time.After(timeout):
			p.logger.Warn("process did not terminate gracefully, sending SIGKILL",
				slog.Duration("timeout", timeout),
			)
			_ = p.forceKill()
			<-done
		}
		if cleanupErr := p.releaseLeaseIfEmpty(timeout); cleanupErr != nil {
			p.logger.Warn("backend ownership cleanup incomplete", slog.String("error", cleanupErr.Error()))
			return errors.Join(ctx.Err(), cleanupErr)
		}

		p.mu.Lock()
		p.state = StateStopped
		p.mu.Unlock()
		p.notifyLifecycle()
		return ctx.Err()
	}
}

func (p *ManagedProcess) waitGroupEmpty(timeout time.Duration) error {
	p.mu.RLock()
	lease := p.lease
	inspector := p.procReader
	active := p.leaseActive
	p.mu.RUnlock()
	if !active || inspector == nil {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		members, err := inspector.ListMembers(lease.LeaderPGID)
		if err == nil && len(members) == 0 {
			return nil
		}
		select {
		case <-deadline.C:
			return errors.New("process group did not become empty")
		case <-ticker.C:
		}
	}
}

func (p *ManagedProcess) releaseLeaseIfEmpty(timeout time.Duration) error {
	p.mu.RLock()
	active, lease, store := p.leaseActive, p.lease, p.leaseStore
	p.mu.RUnlock()
	if !active || store == nil {
		return nil
	}
	if err := p.waitGroupEmpty(timeout); err != nil {
		return err
	}
	if err := store.ReleaseGeneration(p.name, lease.Generation); err != nil {
		return errors.New("failed to release backend ownership lease")
	}
	p.mu.Lock()
	p.leaseActive = false
	p.mu.Unlock()
	return nil
}

// terminate gracefully stops the process tree by sending SIGTERM.
// The caller is responsible for waiting on the process to exit.
func (p *ManagedProcess) terminate() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	p.mu.RLock()
	pid := p.cmd.Process.Pid
	if p.leaseActive {
		pid = p.lease.LeaderPGID
	}
	p.mu.RUnlock()

	// Send SIGTERM to process group (negative PID)
	if err := p.signaler.GroupSignal(pid, syscall.SIGTERM); err != nil {
		// Process might already be dead
		if !errors.Is(err, syscall.ESRCH) {
			p.logger.Debug("failed to send SIGTERM to process group",
				slog.String("error", err.Error()),
			)
		}
	}

	return nil
}

// forceKill sends SIGKILL to the process group.
func (p *ManagedProcess) forceKill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	p.mu.RLock()
	pid := p.cmd.Process.Pid
	if p.leaseActive {
		pid = p.lease.LeaderPGID
	}
	p.mu.RUnlock()

	if err := p.signaler.GroupSignal(pid, syscall.SIGKILL); err != nil {
		if !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to send SIGKILL: %w", err)
		}
	}

	return nil
}

// collectStderr reads stderr and logs it to prevent stream corruption.
// MCP uses stdout for JSON-RPC, so stderr must be captured separately.
func (p *ManagedProcess) collectStderr() {
	p.ioMu.RLock()
	stderr := p.stderr
	p.ioMu.RUnlock()
	if stderr == nil {
		return
	}
	defer func() { _ = stderr.Close() }()

	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		p.logger.Debug("server stderr", slog.String("line", line))
	}
	if err := scanner.Err(); err != nil {
		p.logger.Debug("stderr scanner error", slog.String("error", err.Error()))
	}
}

func (p *ManagedProcess) collectStdout() {
	p.ioMu.RLock()
	stdout := p.stdout
	p.ioMu.RUnlock()
	if stdout == nil {
		return
	}
	defer func() { _ = stdout.Close() }()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		p.logger.Debug("server stdout", slog.String("line", scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		p.logger.Debug("stdout scanner error", slog.String("error", err.Error()))
	}
}

// serveProxy handles http/sse transports by proxying to existing servers.
// This is a placeholder - full implementation in Phase 4 (stdio-http bridge).
func (p *ManagedProcess) serveProxy(ctx context.Context) error {
	p.mu.Lock()
	p.state = StateRunning
	p.startedAt = time.Now()
	p.mu.Unlock()

	p.logger.Info("proxy server started",
		slog.Int("port", p.config.Port),
		slog.String("url", p.config.URL),
	)

	// Block until context cancelled
	<-ctx.Done()

	p.mu.Lock()
	p.state = StateStopped
	p.mu.Unlock()

	return ctx.Err()
}

// String implements fmt.Stringer for logging.
func (p *ManagedProcess) String() string {
	return fmt.Sprintf("ManagedProcess[%s]", p.name)
}

// --- Status accessors ---

// Name returns the server name.
func (p *ManagedProcess) Name() string {
	return p.name
}

// State returns the current service state.
func (p *ManagedProcess) State() ServiceState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// PID returns the process ID, or 0 if not running.
func (p *ManagedProcess) PID() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pid
}

// Uptime returns how long the process has been running.
func (p *ManagedProcess) Uptime() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state != StateRunning || p.startedAt.IsZero() {
		return 0
	}
	return time.Since(p.startedAt)
}

// RestartCount returns the number of times this process has been restarted.
func (p *ManagedProcess) RestartCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.restartCount
}

// LastError returns the last error that caused the process to exit.
func (p *ManagedProcess) LastError() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastError
}

// LifecycleEvents coalesces process lifecycle transitions. Consumers must read
// LifecycleSnapshot after each signal; the snapshot is the durable truth.
func (p *ManagedProcess) LifecycleEvents() <-chan struct{} { return p.lifecycle }

func (p *ManagedProcess) LifecycleSnapshot() (ServiceState, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state, p.generation
}

func (p *ManagedProcess) notifyLifecycle() {
	select {
	case p.lifecycle <- struct{}{}:
	default:
	}
}

// RequestRestart terminates the current child process. Suture owns the
// subsequent restart and lifecycle generation.
func (p *ManagedProcess) RequestRestart() error { return p.terminate() }

// Status returns a snapshot of the current service status.
func (p *ManagedProcess) Status() ServiceStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var uptime time.Duration
	if p.state == StateRunning && !p.startedAt.IsZero() {
		uptime = time.Since(p.startedAt)
	}

	return ServiceStatus{
		Name:         p.name,
		State:        p.state,
		PID:          p.pid,
		Port:         p.config.Port,
		Uptime:       uptime,
		RestartCount: p.restartCount,
		LastError:    p.lastError,
		StartedAt:    p.startedAt,
	}
}

// Stdin returns the stdin pipe for the process.
// Used by the bridge to send MCP messages to the subprocess.
func (p *ManagedProcess) Stdin() io.WriteCloser {
	p.ioMu.RLock()
	defer p.ioMu.RUnlock()
	return p.stdin
}

// Stdout returns the stdout pipe for the process.
// Used by the bridge to receive MCP messages from the subprocess.
func (p *ManagedProcess) Stdout() io.ReadCloser {
	p.ioMu.RLock()
	defer p.ioMu.RUnlock()
	return p.stdout
}

// Config returns the server configuration.
func (p *ManagedProcess) Config() *config.ServerConfig {
	return p.config
}
