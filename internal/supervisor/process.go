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

	"github.com/jrede/vision/internal/config"
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
	mu           sync.RWMutex
	state        ServiceState
	cmd          *exec.Cmd
	pid          int
	startedAt    time.Time
	restartCount int
	lastError    error

	// Suture integration
	token suture.ServiceToken

	// Process I/O (protected by ioMu - separate from mu to avoid holding mu during I/O)
	ioMu   sync.RWMutex
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

// NewManagedProcess creates a new managed process for the given server config.
func NewManagedProcess(name string, cfg *config.ServerConfig, supCfg config.SupervisionConfig, logger *slog.Logger) *ManagedProcess {
	return &ManagedProcess{
		name:      name,
		config:    cfg,
		supConfig: supCfg,
		logger:    logger.With(slog.String("server", name)),
		state:     StateStopped,
	}
}

// Serve implements suture.Service.
// It spawns the subprocess and blocks until it exits or context is cancelled.
func (p *ManagedProcess) Serve(ctx context.Context) error {
	p.mu.Lock()
	p.state = StateStarting
	p.mu.Unlock()

	// Only handle stdio transport for now
	transport := p.config.InferTransport()
	if transport != config.TransportStdio {
		return p.serveProxy(ctx)
	}

	// Spawn the subprocess
	if err := p.spawn(ctx); err != nil {
		p.mu.Lock()
		p.state = StateCrashed
		p.lastError = err
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	p.state = StateRunning
	p.startedAt = time.Now()
	p.mu.Unlock()

	p.logger.Info("server started",
		slog.Int("pid", p.pid),
		slog.Int("port", p.config.Port),
	)

	// Start stderr collector to prevent stream corruption
	go p.collectStderr()

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
func (p *ManagedProcess) buildEnv() []string {
	// Start with current environment
	env := os.Environ()

	// Add server-specific environment variables
	for key, value := range p.config.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
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

		p.mu.Lock()
		p.state = StateStopped
		p.mu.Unlock()
		return ctx.Err()
	}
}

// terminate gracefully stops the process tree by sending SIGTERM.
// The caller is responsible for waiting on the process to exit.
func (p *ManagedProcess) terminate() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	pid := p.cmd.Process.Pid

	// Send SIGTERM to process group (negative PID)
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
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

	pid := p.cmd.Process.Pid

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to send SIGKILL: %w", err)
		}
	}

	return nil
}

// collectStderr reads stderr and logs it to prevent stream corruption.
// MCP uses stdout for JSON-RPC, so stderr must be captured separately.
func (p *ManagedProcess) collectStderr() {
	if p.stderr == nil {
		return
	}
	defer p.stderr.Close()

	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		p.logger.Debug("server stderr", slog.String("line", line))
	}
	if err := scanner.Err(); err != nil {
		p.logger.Debug("stderr scanner error", slog.String("error", err.Error()))
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
