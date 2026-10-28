//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// detach starts the daemon in a new session, away from the go command's
// process group, so Ctrl-C on a build does not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
