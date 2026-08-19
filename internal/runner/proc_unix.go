// Package runner: Unix process-group handling for isolated execution.

//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the child in its own process group so that a
// timeout kills the entire process tree, not just the group leader.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
