//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setSetsid configures cmd to run in a new session (Setsid=true) so the
// supervised child survives the parent's terminal and doesn't receive
// signals meant for the shell.
func setSetsid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
