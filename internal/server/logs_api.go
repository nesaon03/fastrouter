package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nesaon03/fastrouter/internal/logring"
)

// handleLogStream sends the console log over SSE: one "init" event with the
// buffered lines, then "line" and "clear" events as they happen.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lines, ch, stop := logring.Default.Subscribe()
	defer stop()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	send("init", lines)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			if ev.Clear {
				send("clear", map[string]bool{"ok": true})
			} else {
				send("line", ev.Line)
			}
		}
	}
}

func (s *Server) handleLogClear(w http.ResponseWriter, r *http.Request) {
	logring.Default.Clear()
	writeJSON(w, map[string]bool{"ok": true})
}
