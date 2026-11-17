//go:build !windows

package tunnel

import (
	"os/exec"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {}

func bindToParent(pid int) {}

func setPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	setDeathSignal(cmd.SysProcAttr)
}
