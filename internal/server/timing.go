package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/zenpo/fastrouter/internal/hrtime"
)

// Request timing for the Speed Test tab and `fastrouter bench`. A client that
// sends "X-FastRouter-Timing: 1" gets one HTTP trailer of the same name after
// the body, so streamed answers are measured too:
//
//	pre=0.42;post=0.03;hold=0.00;upttfb=812.5;attempts=1
//
// pre     request received -> first upstream request sent (FastRouter's own work)
// post    upstream answer ready -> first byte written to the client
// hold    upstream first byte -> first real content (an empty-answer guard, not CPU)
// upttfb  upstream request sent -> upstream response headers
// All in milliseconds. Nothing in it identifies an account or carries a secret.
const timingHeader = "X-FastRouter-Timing"

// Marks are hrtime nanoseconds (0 = not reached): Go's own clock is too
// coarse on Windows for sub-millisecond overhead.
type timing struct {
	recv, firstSend, lastSend, upHdr, upFirst, upReady, clientFirst int64
	attempts                                                        int
}

func newTiming(w http.ResponseWriter, r *http.Request, recv int64) *timing {
	if r.Header.Get(timingHeader) != "1" {
		return nil
	}
	w.Header().Set("Trailer", timingHeader) // also forces chunked encoding, so the trailer survives small bodies
	return &timing{recv: recv}
}

func (t *timing) send() {
	if t == nil {
		return
	}
	t.attempts++
	t.lastSend = hrtime.Now()
	if t.firstSend == 0 {
		t.firstSend = t.lastSend
	}
	t.upHdr, t.upFirst, t.upReady, t.clientFirst = 0, 0, 0, 0
}

func (t *timing) mark(f func(*timing) *int64) {
	if t == nil {
		return
	}
	if p := f(t); *p == 0 {
		*p = hrtime.Now()
	}
}

func (t *timing) headers()     { t.mark(func(t *timing) *int64 { return &t.upHdr }) }
func (t *timing) firstByte()   { t.mark(func(t *timing) *int64 { return &t.upFirst }) }
func (t *timing) ready()       { t.mark(func(t *timing) *int64 { return &t.upReady }) }
func (t *timing) clientWrite() { t.mark(func(t *timing) *int64 { return &t.clientFirst }) }

func msBetween(a, b int64) float64 {
	if a == 0 || b == 0 || b < a {
		return 0
	}
	return float64((b-a)/1000) / 1000
}

func (t *timing) String() string {
	if t == nil || t.firstSend == 0 {
		return ""
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	parts := []string{
		"pre=" + f(msBetween(t.recv, t.firstSend)),
		"post=" + f(msBetween(t.upReady, t.clientFirst)),
		"hold=" + f(msBetween(t.upFirst, t.upReady)),
		"upttfb=" + f(msBetween(t.lastSend, t.upHdr)),
		"attempts=" + strconv.Itoa(t.attempts),
	}
	if t.attempts > 1 {
		parts = append(parts, "failover="+f(msBetween(t.firstSend, t.lastSend)))
	}
	return strings.Join(parts, ";")
}

// finish sets the trailer once the handler is done writing.
func (t *timing) finish(w http.ResponseWriter) {
	if v := t.String(); v != "" {
		w.Header().Set(timingHeader, v)
	}
}
