//go:build windows

package supervisor

import (
	"errors"
	"os"
	"os/exec"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
)

func configureProcessGroup(_ *exec.Cmd) {}

func signalManagedProcessGroup(_ ownership.Signaler, _ int, _ bool) error { return nil }

func terminateProcessGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func forceKillProcessGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func isProcessNotFound(err error) bool { return errors.Is(err, os.ErrProcessDone) }
