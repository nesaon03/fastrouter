package server

// Live token-saver activity. The savers return a tag describing exactly what
// they changed ("RTK:2,HEADROOM,CAVEMAN:ultra,PONYTAIL:ultra"); recording those
// tags is what lets the Token Saver page say "Running" about real traffic
// rather than echoing a switch back at the user.

import (
	"strings"
	"sync/atomic"
	"time"
)

// saverNames are the four savers, in the order the page shows them.
var saverNames = []string{"rtk", "headroom", "caveman", "ponytail", "customprompt"}

// saverStat is one saver's last run and total run count. Atomics keep the
// request path lock-free.
type saverStat struct {
	last  atomic.Int64 // unix nanos
	count atomic.Int64
}

func (st *saverStat) note(now time.Time) {
	st.last.Store(now.UnixNano())
	st.count.Add(1)
}

// lastRun returns the last time the saver ran; zero time means never.
func (st *saverStat) lastRun() time.Time {
	if n := st.last.Load(); n > 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

type saverRuns struct {
	rtk          saverStat
	headroom     saverStat
	caveman      saverStat
	ponytail     saverStat
	customprompt saverStat
}

// get returns one saver's stat by name.
func (r *saverRuns) get(name string) *saverStat {
	switch name {
	case "rtk":
		return &r.rtk
	case "headroom":
		return &r.headroom
	case "caveman":
		return &r.caveman
	case "ponytail":
		return &r.ponytail
	case "customprompt":
		return &r.customprompt
	}
	return nil
}

// noteSaverTag records the savers that ran for one request.
func (s *Server) noteSaverTag(tag string) {
	if tag == "" {
		return
	}
	now := time.Now()
	for _, part := range strings.Split(tag, ",") {
		name := part
		if i := strings.IndexByte(part, ':'); i > 0 {
			name = part[:i]
		}
		if st := s.saverRuns.get(strings.ToLower(strings.TrimSpace(name))); st != nil {
			st.note(now)
		}
	}
}

// saverRunsView is the JSON the dashboard reads: when each saver last ran, how
// many times, and whether that counts as running now.
func (s *Server) saverRunsView() map[string]any {
	out := make(map[string]any, len(saverNames))
	now := time.Now()
	for _, name := range saverNames {
		st := s.saverRuns.get(name)
		if st == nil {
			continue
		}
		last := st.lastRun()
		entry := map[string]any{"count": st.count.Load()}
		if !last.IsZero() {
			entry["last"] = last.UTC().Format(time.RFC3339Nano)
			// "Running" means it changed a request in the last five minutes: a
			// saver that is on but has seen no traffic reads as On, not Running.
			entry["running"] = now.Sub(last) < 5*time.Minute
			entry["agoSec"] = int(now.Sub(last).Seconds())
		} else {
			entry["running"] = false
		}
		out[name] = entry
	}
	return out
}
