package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
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

type timing struct {
	recv, firstSend, lastSend, upHdr, upFirst, upReady, clientFirst time.Time
	attempts                                                        int
}

func newTiming(w http.ResponseWriter, r *http.Request, recv time.Time) *timing {
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
	t.lastSend = time.Now()
	if t.firstSend.IsZero() {
		t.firstSend = t.lastSend
	}
	t.upHdr, t.upFirst, t.upReady, t.clientFirst = time.Time{}, time.Time{}, time.Time{}, time.Time{}
}

func (t *timing) mark(f func(*timing) *time.Time) {
	if t == nil {
		return
	}
	if p := f(t); p.IsZero() {
		*p = time.Now()
	}
}

func (t *timing) headers()     { t.mark(func(t *timing) *time.Time { return &t.upHdr }) }
func (t *timing) firstByte()   { t.mark(func(t *timing) *time.Time { return &t.upFirst }) }
func (t *timing) ready()       { t.mark(func(t *timing) *time.Time { return &t.upReady }) }
func (t *timing) clientWrite() { t.mark(func(t *timing) *time.Time { return &t.clientFirst }) }

func msBetween(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return 0
	}
	return float64(b.Sub(a).Microseconds()) / 1000
}

func (t *timing) String() string {
	if t == nil || t.firstSend.IsZero() {
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
