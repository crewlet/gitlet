//go:build !unix

package gitx

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

// interruptGroup kills git outright: there is no SIGINT to send.
func interruptGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func killGroup(*exec.Cmd) {}
