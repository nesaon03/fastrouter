package server

// Live token-saver activity. The savers return a tag describing exactly what
// they changed ("RTK:2,HEADROOM,CAVEMAN:ultra,PONYTAIL:ultra"); recording those
// tags is what lets the Token Saver page say "Running" about real traffic
// rather than echoing a switch back at the user.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/saver"
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

// savingsView is the Token Saver page's scoreboard: net tokens saved, prompt
// tokens added, and the average answer size for the break-even hint.
type savingsView struct {
	Window         string  `json:"window"`
	SavedTokens    int64   `json:"savedTokens"`
	AddedTokens    int64   `json:"addedTokens"`
	SavedCost      float64 `json:"savedCost"`
	Requests       int64   `json:"requests"`
	AvgOutput      int64   `json:"avgOutputTokens"`
	PromptCaveman  int64   `json:"promptCaveman"`
	PromptPonytail int64   `json:"promptPonytail"`
	PromptCustom   int64   `json:"promptCustom"`
}

func (s *Server) saverSavingsView() savingsView {
	v := savingsView{Window: "30d"}
	if s.st != nil {
		if t, err := s.st.SaverTotals(time.Now().Add(-30 * 24 * time.Hour)); err == nil {
			v.SavedTokens, v.AddedTokens, v.SavedCost = t.SavedInput, t.AddedInput, t.SavedCost
			v.Requests = t.OK
			if t.OK > 0 {
				v.AvgOutput = t.Output / t.OK
			}
		}
	}
	if sv := s.saver.Load(); sv != nil {
		pc := sv.PromptCost()
		v.PromptCaveman, v.PromptPonytail, v.PromptCustom = pc.Caveman, pc.Ponytail, pc.Custom
	}
	return v
}

// handleSaverPreview runs RTK's detector on pasted text, so the dashboard can
// show what compression would do without sending a request.
func (s *Server) handleSaverPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if len(in.Text) > 500<<10 {
		adminErr(w, http.StatusBadRequest, "text is too large (max 500 KB)")
		return
	}
	out, filter, ok := saver.PreviewRTK(in.Text)
	adminJSON(w, http.StatusOK, map[string]any{
		"compressed": out, "filter": filter, "changed": ok,
		"bytesIn": len(in.Text), "bytesOut": len(out),
	})
}
