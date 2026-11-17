//go:build !windows

package hrtime

import "time"

var base = time.Now()

// Now returns monotonic nanoseconds.
func Now() int64 { return int64(time.Since(base)) }
