package server

// Live swarm telemetry for the dashboard: one record per swarm execution, with
// per-round, per-agent states and answer snippets so the Combos page can draw
// the debate - including who each agent read - as it happens. This is
// observability only - nothing here changes routing.

import (
	"bytes"
	"encoding/json"
	"log"
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
	States     [][]string `json:"states"`              // [round][agent]
	Millis     [][]int64  `json:"millis"`              // [round][agent]
	Texts      [][]string `json:"texts"`               // [round][agent] answer snippet, for the debate transcript
	FullTexts  [][]string `json:"fullTexts,omitempty"` // [round][agent] full answer, persisted only
	JudgeText  string     `json:"judgeText,omitempty"` // final answer written by the judge
	JudgeState string     `json:"judgeState"`
	Error      string     `json:"error,omitempty"`
	Owner      string     `json:"owner,omitempty"`   // API key the learning belongs to
	Class      string     `json:"class,omitempty"`   // task class of this debate
	Learned    int        `json:"learned,omitempty"` // debates learned before this run
	Learn      bool       `json:"-"`                 // per-request learning opt-out
}

type swarmTracker struct {
	mu       sync.Mutex
	runs     map[string]*swarmRun
	order    []string // newest last
	onFinish func(*swarmRun)
}

func (t *swarmTracker) start(combo string, agents []string, judge string, rounds int, owner, class string, learned int, learn bool) string {
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
		FullTexts:  make([][]string, rounds),
		JudgeState: swarmWaiting,
		Owner:      owner,
		Class:      class,
		Learned:    learned,
		Learn:      learn,
	}
	for i := range r.States {
		r.States[i] = make([]string, len(agents))
		r.Millis[i] = make([]int64, len(agents))
		r.Texts[i] = make([]string, len(agents))
		r.FullTexts[i] = make([]string, len(agents))
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

// swarmFullTextChars bounds the persisted transcript. Agents are capped at
// 1200 tokens, so 6000 characters covers a normal answer without letting one
// agent bloat the database.
const swarmFullTextChars = 6000

func swarmFullText(s string) string {
	r := []rune(s)
	if len(r) <= swarmFullTextChars {
		return s
	}
	return string(r[:swarmFullTextChars]) + "…"
}

// text stores an agent's answer: a snippet for the live board, the full text
// for the persisted transcript.
func (t *swarmTracker) text(id string, round, agent int, text string) {
	t.update(id, func(r *swarmRun) {
		if round < 1 || round > len(r.Texts) || agent < 0 || agent >= len(r.Texts[round-1]) {
			return
		}
		r.Texts[round-1][agent] = swarmTextSnippet(text)
		r.FullTexts[round-1][agent] = swarmFullText(text)
	})
}

// judgeText stores the final answer the judge (or consensus shortcut) wrote.
func (t *swarmTracker) judgeText(id, text string) {
	t.update(id, func(r *swarmRun) { r.JudgeText = swarmFullText(text) })
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

// copyRun deep-copies a run so the persistence callback never reads a live
// map entry while the request path is still mutating it.
func copyRun(r *swarmRun) *swarmRun {
	cp := *r
	cp.Agents = append([]string{}, r.Agents...)
	cp.States = make([][]string, len(r.States))
	cp.Millis = make([][]int64, len(r.Millis))
	cp.Texts = make([][]string, len(r.Texts))
	cp.FullTexts = make([][]string, len(r.FullTexts))
	for j := range r.States {
		cp.States[j] = append([]string{}, r.States[j]...)
		cp.Millis[j] = append([]int64{}, r.Millis[j]...)
		cp.Texts[j] = append([]string{}, r.Texts[j]...)
		cp.FullTexts[j] = append([]string{}, r.FullTexts[j]...)
	}
	return &cp
}

// finish marks a run as done and persists its full transcript exactly once.
func (t *swarmTracker) finish(id, status string, consensus bool, errMsg string) {
	var done *swarmRun
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
		done = copyRun(r)
	})
	if done != nil && t.onFinish != nil {
		t.onFinish(done)
	}
}

// snapshot returns newest first for the live board. Full texts and the judge
// answer are deliberately dropped: the transcript endpoint serves those on
// demand, so polling stays small.
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
		cp := copyRun(r)
		cp.FullTexts = nil
		cp.JudgeText = ""
		if cp.Status == swarmRunning {
			cp.ElapsedMS = now.Sub(cp.Started).Milliseconds()
		}
		out = append(out, *cp)
	}
	return out
}

// Swarm transcript persistence. The live board polls small snapshots; finished
// debates are stored in SQLite with full answers and the judge's final reply.
func (s *Server) persistSwarmRun(r *swarmRun) {
	if s.st == nil {
		return
	}
	doc, err := json.Marshal(r)
	if err != nil {
		log.Printf("swarm transcript encode failed: %v", err)
		return
	}
	if err := s.st.SaveSwarmRun(r.ID, r.Combo, r.Started, r.Status, r.Consensus, doc); err != nil {
		log.Printf("swarm transcript save failed: %v", err)
	}
}

const swarmJudgeCaptureBytes = 64 << 10

// swarmStatusWriter records the status and a bounded copy of the judge reply so
// the transcript can show the final answer, for both streamed and buffered
// responses.
type swarmStatusWriter struct {
	http.ResponseWriter
	status int
	body   []byte
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
	if n := swarmJudgeCaptureBytes - len(w.body); n > 0 {
		if n > len(p) {
			n = len(p)
		}
		w.body = append(w.body, p[:n]...)
	}
	return w.ResponseWriter.Write(p)
}

func (w *swarmStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *swarmStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// judgeText extracts the assistant text from the captured client-format body.
// It understands OpenAI, Anthropic and Gemini shapes, JSON or SSE.
func (w *swarmStatusWriter) judgeText() string {
	var b strings.Builder
	body := strings.TrimSpace(string(w.body))
	if strings.HasPrefix(body, "data:") || bytes.Contains(w.body, []byte("\ndata:")) {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if d == "" || d == "[DONE]" {
				continue
			}
			var v any
			if json.Unmarshal([]byte(d), &v) != nil {
				continue
			}
			b.WriteString(swarmExtractText(v))
		}
	} else {
		var v any
		if json.Unmarshal(w.body, &v) == nil {
			b.WriteString(swarmExtractText(v))
		}
	}
	return strings.TrimSpace(b.String())
}

func swarmExtractText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if ch, ok := m["choices"].([]any); ok && len(ch) > 0 {
		if c, ok := ch[0].(map[string]any); ok {
			for _, key := range []string{"delta", "message"} {
				if d, ok := c[key].(map[string]any); ok {
					if t, ok := d["content"].(string); ok {
						return t
					}
					if parts, ok := d["content"].([]any); ok {
						return swarmPartsText(parts)
					}
				}
			}
			if t, ok := c["text"].(string); ok {
				return t
			}
		}
	}
	if d, ok := m["delta"].(map[string]any); ok {
		if t, ok := d["text"].(string); ok {
			return t
		}
	}
	if t, ok := m["text"].(string); ok {
		return t
	}
	if c, ok := m["content"].(string); ok {
		return c
	}
	if c, ok := m["content"].([]any); ok {
		return swarmPartsText(c)
	}
	if cands, ok := m["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			if cont, ok := c["content"].(map[string]any); ok {
				if parts, ok := cont["parts"].([]any); ok {
					return swarmPartsText(parts)
				}
			}
		}
	}
	return ""
}

func swarmPartsText(parts []any) string {
	var b strings.Builder
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// handleSwarmRuns returns live/recent runs plus persisted debate metadata.
func (s *Server) handleSwarmRuns(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"runs": s.swarms.snapshot()}
	if s.st != nil {
		rows, err := s.st.SwarmRuns(20)
		if err != nil {
			log.Printf("swarm history failed: %v", err)
		} else {
			out["history"] = rows
		}
	}
	adminJSON(w, http.StatusOK, out)
}

// handleSwarmMemoryClear drops all learned lessons, per user's request.
func (s *Server) handleSwarmMemoryClear(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		adminErr(w, http.StatusServiceUnavailable, "no database")
		return
	}
	if err := s.st.ClearSwarmMemory(""); err != nil {
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSwarmRun returns one persisted debate's full transcript.
func (s *Server) handleSwarmRun(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		adminErr(w, http.StatusServiceUnavailable, "no database")
		return
	}
	doc, err := s.st.SwarmRun(r.PathValue("id"))
	if err != nil {
		adminErr(w, http.StatusNotFound, "debate not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc)
}
