// Package logring keeps the last log lines in memory for the dashboard's
// Console Log page. Lines are redacted before they are stored.
package logring

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nesaon03/fastrouter/internal/secret"
)

// Line is one log line.
type Line struct {
	ID    uint64 `json:"id"`
	At    string `json:"at"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Event goes to subscribers: a new line, or a clear.
type Event struct {
	Clear bool
	Line  Line
}

// Ring is an io.Writer that keeps the last N lines.
type Ring struct {
	mu      sync.Mutex
	size    int
	lines   []Line
	next    uint64
	partial []byte
	subs    map[chan Event]struct{}
}

// New returns a ring holding up to size lines.
func New(size int) *Ring {
	return &Ring{size: size, subs: map[chan Event]struct{}{}}
}

// Default receives the std logger's output (wired in main).
var Default = New(1000)

var stdPrefix = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d(\.\d+)? `)

var emailRe = regexp.MustCompile(`([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@([A-Za-z0-9.-]+\.[A-Za-z]{2,})`)

func level(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "panic") || strings.Contains(l, "error") || strings.Contains(l, "failed") || strings.Contains(l, "fatal"):
		return "error"
	case strings.Contains(l, "warn") || strings.Contains(l, "cooldown") || strings.Contains(l, "retry") || strings.Contains(l, "skipped"):
		return "warn"
	case strings.HasPrefix(s, "COMBO") || strings.HasPrefix(s, "FUSION") || strings.Contains(s, " COMBO") || strings.Contains(s, " FUSION"):
		return "debug"
	}
	return "info"
}

// Write splits p into lines; it never fails.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := append(r.partial, p...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		r.add(string(buf[:i]))
		buf = buf[i+1:]
	}
	if len(buf) > 8192 {
		r.add(string(buf))
		buf = nil
	}
	r.partial = append([]byte(nil), buf...)
	return len(p), nil
}

func (r *Ring) add(text string) {
	text = stdPrefix.ReplaceAllString(strings.TrimRight(text, "\r"), "") // the page shows its own time
	if text == "" {
		return
	}
	text = emailRe.ReplaceAllString(secret.Redact(text), "$1***@$2")
	r.next++
	l := Line{ID: r.next, At: time.Now().UTC().Format(time.RFC3339Nano), Level: level(text), Text: text}
	if len(r.lines) >= r.size {
		copy(r.lines, r.lines[1:])
		r.lines = r.lines[:len(r.lines)-1]
	}
	r.lines = append(r.lines, l)
	for ch := range r.subs {
		select {
		case ch <- Event{Line: l}:
		default: // slow reader: drop rather than block logging
		}
	}
}

// Snapshot returns the stored lines, oldest first.
func (r *Ring) Snapshot() []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Line(nil), r.lines...)
}

// Clear drops every stored line and tells subscribers.
func (r *Ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = r.lines[:0]
	for ch := range r.subs {
		select {
		case ch <- Event{Clear: true}:
		default:
		}
	}
}

// Subscribe returns the current lines and a channel of later events.
func (r *Ring) Subscribe() ([]Line, chan Event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Event, 256)
	r.subs[ch] = struct{}{}
	return append([]Line(nil), r.lines...), ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}
