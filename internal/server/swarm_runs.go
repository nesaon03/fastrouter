package server

// Live swarm telemetry for the dashboard: one record per swarm execution, with
// per-round, per-agent states and answer snippets so the Combos page can draw
// the debate - including who each agent read - as it happens. This is
// observability only - nothing here changes routing.

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Swarm run/agent states used by the diagram.
const (
	swarmWaiting  = "waiting"  // round has not started for this agent
	swarmThinking = "thinking" // request in flight
	swarmOK       = "ok"       // answered
	swarmEmpty    = "empty"    // answered with nothing
	swarmError    = "error"    // request failed
	swarmTimeout  = "timeout"  // round ended before this agent answered
	swarmSkipped  = "skipped"  // agent was already out of the debate
	swarmDone     = "done"
	swarmRunning  = "running"
)

const (
	swarmRunsKept = 12
	swarmRunTTL   = 30 * time.Minute
)

// swarmRun is one execution of a swarm combo.
type swarmRun struct {
	ID         string     `json:"id"`
	Combo      string     `json:"combo"`
	Agents     []string   `json:"agents"`
	Judge      string     `json:"judge"`
	Rounds     int        `json:"rounds"`
	Round      int        `json:"round"` // 1-based; the round in progress or last one
	Status     string     `json:"status"`
	Consensus  bool       `json:"consensus,omitempty"`
	Started    time.Time  `json:"started"`
	Updated    time.Time  `json:"updated"`
	ElapsedMS  int64      `json:"elapsedMs"`
	States     [][]string `json:"states"` // [round][agent]
	Millis     [][]int64  `json:"millis"` // [round][agent]
	Texts      [][]string `json:"texts"`  // [round][agent] answer snippet, for the debate transcript
	JudgeState string     `json:"judgeState"`
	Error      string     `json:"error,omitempty"`
}

type swarmTracker struct {
	mu    sync.Mutex
	runs  map[string]*swarmRun
	order []string // newest last
}

func (t *swarmTracker) start(combo string, agents []string, judge string, rounds int) string {
	if rounds < 1 {
		rounds = 1
	}
	now := time.Now()
	r := &swarmRun{
		ID:         combo + "-" + now.Format("150405.000000000"),
		Combo:      combo,
		Agents:     append([]string{}, agents...),
		Judge:      judge,
		Rounds:     rounds,
		Status:     swarmRunning,
		Started:    now,
		Updated:    now,
		States:     make([][]string, rounds),
		Millis:     make([][]int64, rounds),
		Texts:      make([][]string, rounds),
		JudgeState: swarmWaiting,
	}
	for i := range r.States {
		r.States[i] = make([]string, len(agents))
		r.Millis[i] = make([]int64, len(agents))
		r.Texts[i] = make([]string, len(agents))
		for j := range r.States[i] {
			r.States[i][j] = swarmWaiting
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runs == nil {
		t.runs = map[string]*swarmRun{}
	}
	t.runs[r.ID] = r
	t.order = append(t.order, r.ID)
	t.pruneLocked(now)
	return r.ID
}

func (t *swarmTracker) pruneLocked(now time.Time) {
	keep := t.order[:0]
	for _, id := range t.order {
		r := t.runs[id]
		if r == nil {
			continue
		}
		if now.Sub(r.Started) > swarmRunTTL {
			delete(t.runs, id)
			continue
		}
		keep = append(keep, id)
	}
	t.order = keep
	for len(t.order) > swarmRunsKept {
		delete(t.runs, t.order[0])
		t.order = t.order[1:]
	}
}

func (t *swarmTracker) update(id string, fn func(*swarmRun)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[id]
	if r == nil {
		return
	}
	fn(r)
	r.Updated = time.Now()
	if r.Status != swarmRunning {
		r.ElapsedMS = r.Updated.Sub(r.Started).Milliseconds()
	}
}

func (t *swarmTracker) round(id string, round int) {
	t.update(id, func(r *swarmRun) {
		r.Round = round
		for i := range r.States[round-1] {
			r.States[round-1][i] = swarmWaiting
		}
	})
}

func (t *swarmTracker) agent(id string, round, agent int, state string, ms int64) {
	t.update(id, func(r *swarmRun) {
		if round < 1 || round > len(r.States) || agent < 0 || agent >= len(r.States[round-1]) {
			return
		}
		r.States[round-1][agent] = state
		if ms > 0 {
			r.Millis[round-1][agent] = ms
		}
	})
}

// swarmTextChars is how much of each answer the transcript keeps. The polled
// API ships up to four agents x three rounds, so this stays a snippet.
const swarmTextChars = 420

// swarmTextSnippet flattens whitespace and trims an answer for the dashboard.
func swarmTextSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= swarmTextChars {
		return s
	}
	return string(r[:swarmTextChars]) + "…"
}

// text stores the answer snippet an agent produced in a round, so the board can
// show what was said and the transcript can show who read whom.
func (t *swarmTracker) text(id string, round, agent int, text string) {
	t.update(id, func(r *swarmRun) {
		if round < 1 || round > len(r.Texts) || agent < 0 || agent >= len(r.Texts[round-1]) {
			return
		}
		r.Texts[round-1][agent] = swarmTextSnippet(text)
	})
}

func (t *swarmTracker) judge(id, state string) {
	t.update(id, func(r *swarmRun) { r.JudgeState = state })
}

// settleRound marks agents that never answered the round as timed out.
func (t *swarmTracker) settleRound(id string, round int) {
	t.update(id, func(r *swarmRun) {
		if round < 1 || round > len(r.States) {
			return
		}
		for i := range r.States[round-1] {
			if r.States[round-1][i] == swarmThinking || r.States[round-1][i] == swarmWaiting {
				r.States[round-1][i] = swarmTimeout
			}
		}
	})
}

func (t *swarmTracker) finish(id, status string, consensus bool, errMsg string) {
	t.update(id, func(r *swarmRun) {
		if r.Status != swarmRunning {
			return
		}
		r.Status = status
		r.Consensus = consensus
		r.Error = errMsg
		for round := range r.States {
			for i := range r.States[round] {
				st := r.States[round][i]
				if st != swarmThinking && st != swarmWaiting {
					continue
				}
				if round+1 > r.Round {
					r.States[round][i] = swarmSkipped // round never started
				} else {
					r.States[round][i] = swarmTimeout
				}
			}
		}
	})
}

// snapshot returns newest first, deep-copied so the JSON encoder never reads a
// map entry that a live run is mutating.
func (t *swarmTracker) snapshot() []swarmRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	out := make([]swarmRun, 0, len(t.order))
	for i := len(t.order) - 1; i >= 0; i-- {
		r := t.runs[t.order[i]]
		if r == nil || now.Sub(r.Started) > swarmRunTTL {
			continue
		}
		cp := *r
		cp.Agents = append([]string{}, r.Agents...)
		cp.States = make([][]string, len(r.States))
		cp.Millis = make([][]int64, len(r.Millis))
		cp.Texts = make([][]string, len(r.Texts))
		for j := range r.States {
			cp.States[j] = append([]string{}, r.States[j]...)
			cp.Millis[j] = append([]int64{}, r.Millis[j]...)
			cp.Texts[j] = append([]string{}, r.Texts[j]...)
		}
		if cp.Status == swarmRunning {
			cp.ElapsedMS = now.Sub(cp.Started).Milliseconds()
		}
		out = append(out, cp)
	}
	return out
}

// swarmStatusWriter records the status of a streamed judge reply so the run can
// show whether the judge actually answered.
type swarmStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *swarmStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *swarmStatusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *swarmStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *swarmStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// handleSwarmRuns returns recent swarm executions for the Combos page diagram.
func (s *Server) handleSwarmRuns(w http.ResponseWriter, r *http.Request) {
	adminJSON(w, http.StatusOK, map[string]any{"runs": s.swarms.snapshot()})
}
