package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// PIDFile manages a PID file for the daemon.
type PIDFile struct {
	path string
}

// Errors for PID file operations.
var (
	ErrDaemonRunning = errors.New("daemon already running")
	ErrStalePID      = errors.New("stale PID file")
)

// DefaultPIDPath returns the default PID file path.
func DefaultPIDPath() string {
	// Try runtime directory first (systemd-friendly)
	if runDir := os.Getenv("XDG_RUNTIME_DIR"); runDir != "" {
		return filepath.Join(runDir, "vision.pid")
	}
	// Fall back to /tmp
	return "/tmp/vision.pid"
}

// NewPIDFile creates a new PID file manager.
func NewPIDFile(path string) *PIDFile {
	if path == "" {
		path = DefaultPIDPath()
	}
	return &PIDFile{path: path}
}

// Acquire creates the PID file if no daemon is running.
// Returns ErrDaemonRunning if another daemon instance is active.
func (p *PIDFile) Acquire() error {
	// Check for existing PID file
	if existingPID, err := p.Read(); err == nil {
		// PID file exists, check if process is running
		if isProcessRunning(existingPID) {
			return fmt.Errorf("%w: pid %d", ErrDaemonRunning, existingPID)
		}
		// Stale PID file, remove it
		if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove stale PID file: %w", err)
		}
	}

	// Create parent directory if needed
	if err := os.MkdirAll(filepath.Dir(p.path), 0755); err != nil {
		return fmt.Errorf("failed to create PID directory: %w", err)
	}

	// Write PID file
	pid := os.Getpid()
	if err := os.WriteFile(p.path, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	return nil
}

// Release removes the PID file.
func (p *PIDFile) Release() error {
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove PID file: %w", err)
	}
	return nil
}

// Read returns the PID from the file.
func (p *PIDFile) Read() (int, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("invalid PID file content: %w", err)
	}

	return pid, nil
}

// Path returns the PID file path.
func (p *PIDFile) Path() string {
	return p.path
}

// IsRunning checks if a daemon is currently running.
func (p *PIDFile) IsRunning() (bool, int) {
	pid, err := p.Read()
	if err != nil {
		return false, 0
	}

	if isProcessRunning(pid) {
		return true, pid
	}

	return false, 0
}

// isProcessRunning checks if a process with the given PID is running.
func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// On Unix, FindProcess always succeeds, so we need to send signal 0
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// AcquireOrFail is a helper that acquires the PID file or exits with an error message.
func (p *PIDFile) AcquireOrFail() {
	if err := p.Acquire(); err != nil {
		if errors.Is(err, ErrDaemonRunning) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			fmt.Fprintf(os.Stderr, "Use 'vision stop' to stop the running daemon.\n")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Error acquiring PID file: %v\n", err)
		os.Exit(1)
	}
}
