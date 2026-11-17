//go:build windows

package hrtime

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	k32    = syscall.NewLazyDLL("kernel32.dll")
	qpc    = k32.NewProc("QueryPerformanceCounter")
	qpf    = k32.NewProc("QueryPerformanceFrequency")
	freq   int64
	start  = time.Now()
	useQPC bool
)

func init() {
	if r, _, _ := qpf.Call(uintptr(unsafe.Pointer(&freq))); r != 0 && freq > 0 {
		useQPC = true
	}
}

// Now returns monotonic nanoseconds from QueryPerformanceCounter (~100 ns).
func Now() int64 {
	if !useQPC {
		return int64(time.Since(start))
	}
	var c int64
	qpc.Call(uintptr(unsafe.Pointer(&c)))
	return c/freq*1e9 + c%freq*1e9/freq
}
