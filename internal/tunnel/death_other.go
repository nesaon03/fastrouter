//go:build !linux && !windows

package tunnel

import "syscall"

func setDeathSignal(a *syscall.SysProcAttr) {}
