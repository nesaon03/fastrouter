package translate

import (
	"sync/atomic"
	"time"
)

var idCounter atomic.Uint64

// nowNano is monotonic-ish and unique per call, for generated ids.
func nowNano() int64 { return time.Now().UnixNano()/1000 + int64(idCounter.Add(1)) }

// nowUnix is the current time in seconds (overridable in tests).
var nowUnix = func() int64 { return time.Now().Unix() }
