package client

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts the daemon without a console and in a new process group,
// so Ctrl-C on a build and closing the terminal do not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
