//go:build !windows

package saver

import (
	"os/exec"
	"syscall"
)

func hideWindow(*exec.Cmd) {}

// detach keeps the proxy running if FastRouter stops (as 9Router does).
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
