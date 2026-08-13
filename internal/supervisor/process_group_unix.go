//go:build !windows

package supervisor

import (
	"errors"
	"os/exec"
	"syscall"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalManagedProcessGroup(signaler ownership.Signaler, pid int, force bool) error {
	if force {
		return signaler.GroupSignal(pid, syscall.SIGKILL)
	}
	return signaler.GroupSignal(pid, syscall.SIGTERM)
}

func terminateProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func forceKillProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func isProcessNotFound(err error) bool { return errors.Is(err, syscall.ESRCH) }
