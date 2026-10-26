//go:build unix

package gitx

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup starts git in a process group of its own, so that an
// interrupt reaches every process it started (ssh, editors, hooks), as
// Ctrl-C in a terminal would, and a Ctrl-C meant for gitlet does not.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptGroup(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGINT) }

func killGroup(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGKILL) }

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
