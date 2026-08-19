// Package runner: Windows process handling for isolated execution.

//go:build windows

package runner

import "os/exec"

// configureProcess on Windows kills only the direct child on timeout.
// Windows has no POSIX process groups; descendants of the child are not
// reaped. Acceptable reduced scope for the first Windows build — revisit
// with Job Objects if grandchild leakage becomes a real problem.
func configureProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return cmd.Process.Kill()
	}
}
