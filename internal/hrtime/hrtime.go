// Package hrtime is a high-resolution monotonic clock for request timings.
// On Windows Go's own monotonic clock only ticks every 0.5–15.6 ms, which
// would round FastRouter's sub-millisecond overhead to 0.
package hrtime

import "time"

// Duration since a, both from Now.
func Since(a int64) time.Duration { return time.Duration(Now() - a) }
