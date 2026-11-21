package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/router"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

// Smart and fusion combos, after 9Router's combo service (MIT, decolua/9router):
//   - smart: a router model reads the latest user turn and picks which combo
//     model should answer; the rest stay as fallbacks. The pick sticks to the
//     session (prompt cache stays warm) unless the policy says otherwise.
//   - fusion: every combo model answers in parallel (no tools, no streaming),
//     then a judge model writes one final answer from the panel's replies.

const (
	smartTimeout     = 8 * time.Second
	smartStateChars  = 2000
	stickyTTL        = 2 * time.Hour
	stickyMax        = 5000
	fusionHardLimit  = 90 * time.Second
	fusionGraceAfter = 8 * time.Second
	cascadeHardLimit = 120 * time.Second
)

type stickyEntry struct {
	model string
	used  time.Time
}

type stickyStore struct {
	mu sync.Mutex
	m  map[string]stickyEntry
}

func (ss *stickyStore) get(key string, models []string) string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	e, ok := ss.m[key]
	if !ok || time.Since(e.used) > stickyTTL || !contains(models, e.model) {
		delete(ss.m, key)
		return ""
	}
	e.used = time.Now()
	ss.m[key] = e
	return e.model
}

func (ss *stickyStore) set(key, model string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.m == nil {
		ss.m = map[string]stickyEntry{}
	}
	if len(ss.m) >= stickyMax {
		for k, e := range ss.m {
			if time.Since(e.used) > stickyTTL || len(ss.m) >= stickyMax {
				delete(ss.m, k)
			}
		}
	}
	ss.m[key] = stickyEntry{model, time.Now()}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// specialCombo serves smart, fusion and swarm combos.
func (s *Server) specialCombo(w http.ResponseWriter, p *proxyReq, st *router.State, c *router.Combo) {
	switch c.Strategy {
	case "fusion":
		s.fusion(w, p, st, c)
		return
	case "swarm":
		s.swarm(w, p, st, c)
		return
	case "cascade":
		s.cascade(w, p, st, c)
		return
	}
	order := s.smartOrder(p, c)
	targets, err := st.ResolveModels(c.Name, order)
	if err != nil {
		writeError(w, p.cf, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	s.route(w, p, s.visionTargets(p, st, targets))
}

// sessionKey identifies a conversation: the client's session header, else a
// hash of the API key and the first user turn (stable across a chat).
func sessionKey(p *proxyReq) string {
	for _, h := range []string{"session_id", "x-session-id", "x-claude-code-session-id"} {
		if v := p.r.Header.Get(h); v != "" {
			return v
		}
	}
	ir, err := p.parse()
	if err != nil {
		return ""
	}
	for _, m := range ir.Messages {
		if m.Role == "user" {
			if t := partsText(m.Parts); t != "" {
				keyID := ""
				if k := keyFrom(p.r.Context()); k != nil {
					keyID = k.ID
				}
				sum := sha256.Sum256([]byte(keyID + "\x00" + t))
				return hex.EncodeToString(sum[:12])
			}
		}
	}
	return ""
}

func partsText(ps []tr.Part) string {
	var b strings.Builder
	for _, p := range ps {
		if p.Type == tr.PartText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// lastUserText is the latest user turn's text, capped.
func lastUserText(ir *tr.Request) string {
	for i := len(ir.Messages) - 1; i >= 0; i-- {
		if ir.Messages[i].Role == "user" {
			if t := strings.TrimSpace(partsText(ir.Messages[i].Parts)); t != "" {
				if len(t) > smartStateChars {
					t = t[:smartStateChars]
				}
				return t
			}
		}
	}
	return ""
}

type smartPick struct {
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// smartOrder returns the combo's models with the chosen one first.
func (s *Server) smartOrder(p *proxyReq, c *router.Combo) []string {
	models := c.Models
	if len(models) <= 1 {
		return models
	}
	sid := ""
	if c.SmartPolicy != "per-turn" {
		if sid = sessionKey(p); sid != "" {
			sid = c.Name + ":" + sid
		}
	}
	sticky := ""
	if sid != "" {
		sticky = s.sticky.get(sid, models)
	}
	first := func(m string) []string {
		out := []string{m}
		for _, x := range models {
			if x != m {
				out = append(out, x)
			}
		}
		return out
	}
	if sticky != "" && c.SmartPolicy == "strict" {
		return first(sticky)
	}
	pick, err := s.smartChoice(p, c)
	conf := 0.0
	if pick != nil && pick.Confidence != nil {
		conf = *pick.Confidence
	}
	switch {
	case err != nil:
		log.Printf("COMBO %s: smart router failed (%v); keeping %s", c.Name, err, firstNonEmpty(sticky, "the listed order"))
	case sticky != "":
		if pick.Choice != sticky && conf >= c.Threshold {
			log.Printf("COMBO %s: smart router switched %s -> %s (confidence %.2f)", c.Name, sticky, pick.Choice, conf)
			s.sticky.set(sid, pick.Choice)
			return orderByPick(models, pick)
		}
		log.Printf("COMBO %s: smart router kept %s for this session", c.Name, sticky)
	case pick.Confidence != nil && conf < c.Threshold:
		log.Printf("COMBO %s: smart router unsure (%.2f < %.2f); using the listed order", c.Name, conf, c.Threshold)
	default:
		log.Printf("COMBO %s: smart router picked %s (confidence %.2f)", c.Name, pick.Choice, conf)
		if sid != "" {
			s.sticky.set(sid, pick.Choice)
		}
		return orderByPick(models, pick)
	}
	if sticky != "" {
		return first(sticky)
	}
	return models
}

func orderByPick(models []string, pick *smartPick) []string {
	others := []string{}
	for _, m := range models {
		if m != pick.Choice {
			others = append(others, m)
		}
	}
	sort.SliceStable(others, func(i, j int) bool { return pick.Probabilities[others[i]] > pick.Probabilities[others[j]] })
	return append([]string{pick.Choice}, others...)
}

// smartChoice asks the router model which combo model fits the request.
func (s *Server) smartChoice(p *proxyReq, c *router.Combo) (*smartPick, error) {
	ir, err := p.parse()
	if err != nil {
		return nil, err
	}
	state := lastUserText(ir)
	if state == "" {
		return nil, fmt.Errorf("no user text to classify")
	}
	if c.RouterEngine == "decision" {
		return s.smartChoiceDecision(p, c, state)
	}
	routerModel := firstNonEmpty(c.RouterModel, c.Models[0])
	var list strings.Builder
	for _, m := range c.Models {
		list.WriteString("- " + m + "\n")
	}
	zero := 0.0
	q := &tr.Request{
		System: []tr.Part{{Type: tr.PartText, Text: "You route requests to the most appropriate model. Answer with one JSON object and nothing else."}},
		Messages: []tr.Message{{Role: "user", Parts: []tr.Part{{Type: tr.PartText, Text: "Which of these configured models is the most appropriate for this request?\n\nModels:\n" +
			list.String() + "\nRequest:\n" + state + "\n\nReply with JSON only: {\"choice\": \"<model from the list>\", \"confidence\": <0 to 1>, \"probabilities\": {\"<model>\": <0 to 1>}}"}}}},
		MaxTokens: 300, Temperature: &zero,
	}
	ctx, cancel := context.WithTimeout(p.r.Context(), smartTimeout)
	defer cancel()
	resp, err := s.internalCall(ctx, p.r, q, routerModel)
	if err != nil {
		return nil, err
	}
	text := partsText(resp.Parts)
	i, j := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}')
	if i < 0 || j < i {
		return nil, fmt.Errorf("router model %s did not answer with JSON", routerModel)
	}
	var pick smartPick
	if err := json.Unmarshal([]byte(text[i:j+1]), &pick); err != nil {
		return nil, fmt.Errorf("router model %s: %v", routerModel, err)
	}
	pick.Choice = strings.TrimSpace(pick.Choice)
	if !contains(c.Models, pick.Choice) {
		return nil, fmt.Errorf("router model %s chose %q, which is not in the combo", routerModel, pick.Choice)
	}
	return &pick, nil
}

// smartChoiceDecision asks a decision engine which combo model fits. A typed
// choice costs no prose tokens and returns in one short round trip, which is
// what makes per-turn routing affordable.
func (s *Server) smartChoiceDecision(p *proxyReq, c *router.Combo, state string) (*smartPick, error) {
	criteria := make(map[string]any, len(c.Models))
	for _, m := range c.Models {
		criteria[m] = m
	}
	questions, err := json.Marshal(map[string]any{"pick": map[string]any{
		"type":         "choice",
		"instructions": "Pick the configured model that is most appropriate for this request.",
		"criteria":     criteria,
	}})
	if err != nil {
		return nil, err
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(p.r.Context(), smartTimeout)
	defer cancel()
	res, err := s.decisionAnswer(ctx, decisionReq{Model: c.DecisionModel, State: stateJSON, Questions: questions})
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, fmt.Errorf("decision engine: HTTP %d %s", res.Status, upstreamMessage(res.Body))
	}
	if res.Account != nil {
		s.recordDecision(p.r, time.Now(), res, res.Account)
	}
	return parseDecisionChoice(res.Body, "pick")
}

// parseDecisionChoice reads one named choice from either engine's response shape.
func parseDecisionChoice(body []byte, name string) (*smartPick, error) {
	var doc struct {
		Answers json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Answers) == 0 {
		return nil, fmt.Errorf("decision engine answered without answers")
	}
	type answer struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Confidence    *float64           `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
	}
	var got answer
	var asMap map[string]answer
	if err := json.Unmarshal(doc.Answers, &asMap); err == nil {
		got = asMap[name]
	} else {
		var list []struct {
			Name string `json:"name"`
			answer
		}
		if err := json.Unmarshal(doc.Answers, &list); err != nil {
			return nil, fmt.Errorf("decision engine answered in an unknown shape")
		}
		for _, it := range list {
			if it.Name == name {
				got = it.answer
			}
		}
	}
	got.Choice = strings.TrimSpace(got.Choice)
	if got.Choice == "" {
		return nil, fmt.Errorf("decision engine did not choose a model")
	}
	return &smartPick{Choice: got.Choice, Confidence: got.Confidence, Probabilities: got.Probabilities}, nil
}

// bufWriter collects an internal response.
type bufWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufWriter) Header() http.Header { return b.h }
func (b *bufWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}
func (b *bufWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufWriter) Flush() {}

// internalCall sends ir to model through normal routing (accounts, fallback,
// usage records) as a non-streaming OpenAI request and parses the answer.
func (s *Server) internalCall(ctx context.Context, parent *http.Request, ir *tr.Request, model string) (*tr.Response, error) {
	r := *ir
	r.Model, r.Stream = model, false
	body, err := tr.EmitRequest(tr.OpenAI, &r, tr.EmitOptions{Model: model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.RemoteAddr = parent.RemoteAddr
	p := &proxyReq{r: req, cf: tr.OpenAI, body: body, model: model}
	if err := scanHead(body, &p.head); err != nil {
		return nil, err
	}
	targets, err := s.state.Load().Resolve(model)
	if err != nil {
		return nil, err
	}
	bw := &bufWriter{h: http.Header{}}
	s.route(bw, p, targets)
	if bw.status != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d %s", model, bw.status, upstreamMessage(bw.buf.Bytes()))
	}
	resp, err := tr.ParseResponse(tr.OpenAI, bw.buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s: %v", model, err)
	}
	return resp, nil
}

// flattenTools turns tool calls and results into prose so panel models keep
// the context without being able to call tools.
func flattenTools(ms []tr.Message) []tr.Message {
	out := make([]tr.Message, 0, len(ms))
	for _, m := range ms {
		nm := tr.Message{Role: m.Role}
		for _, p := range m.Parts {
			switch p.Type {
			case tr.PartToolCall:
				nm.Parts = append(nm.Parts, tr.Part{Type: tr.PartText, Text: fmt.Sprintf("[called tool %s with %s]", p.ToolName, p.Args)})
			case tr.PartToolResult:
				nm.Parts = append(nm.Parts, tr.Part{Type: tr.PartText, Text: "[tool result: " + partsText(p.Result) + "]"})
			case tr.PartThinking, tr.PartRedactedThinking:
			default:
				nm.Parts = append(nm.Parts, p)
			}
		}
		if len(nm.Parts) > 0 {
			out = append(out, nm)
		}
	}
	return out
}

func judgePrompt(answers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the JUDGE in a model-fusion panel. %d expert models independently answered the user's most recent request. Their responses are below, anonymized by source.\n\n", len(answers))
	b.WriteString("Do NOT mention that multiple models were used, and do NOT refer to the sources. Produce ONE authoritative final answer addressed directly to the user.\n\n")
	b.WriteString("First, internally analyze the panel along these dimensions: consensus (points most sources agree on — treat as higher-confidence), contradictions (where they disagree — resolve with your own judgment), partial coverage, unique insights only one source surfaced, and blind spots every source missed. Then write the best possible final answer grounded in that analysis — more complete and correct than any single response, with no filler.\n\n")
	b.WriteString("=== PANEL RESPONSES ===\n")
	for i, a := range answers {
		fmt.Fprintf(&b, "[Source %d]\n%s\n\n", i+1, a)
	}
	b.WriteString("=== END PANEL RESPONSES ===\n\nNow write the final answer to the user's original request.")
	return b.String()
}

// fusion fans the request out to every combo model, then has the judge answer.
func (s *Server) fusion(w http.ResponseWriter, p *proxyReq, st *router.State, c *router.Combo) {
	panel := c.Models
	judge := firstNonEmpty(c.JudgeModel, panel[0])
	if len(panel) == 1 {
		s.routeModel(w, p, st, panel[0])
		return
	}
	ir, err := p.parse()
	if err != nil {
		writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	q := *ir
	q.Tools, q.ToolChoice, q.ParallelToolCalls = nil, nil, nil
	q.Messages = flattenTools(ir.Messages)
	log.Printf("FUSION %s: panel of %d, judge %s", c.Name, len(panel), judge)

	ctx, cancel := context.WithTimeout(p.r.Context(), fusionHardLimit)
	defer cancel()
	type result struct {
		i    int
		text string
		err  error
	}
	ch := make(chan result, len(panel))
	for i, m := range panel {
		go func(i int, m string) {
			resp, err := s.internalCall(ctx, p.r, &q, m)
			text := ""
			if err == nil {
				if text = strings.TrimSpace(partsText(resp.Parts)); text == "" {
					err = fmt.Errorf("%s: empty answer", m)
				}
			}
			ch <- result{i, text, err}
		}(i, m)
	}
	answers := make([]string, len(panel))
	quorum := min(2, len(panel))
	ok, got := 0, 0
	var grace <-chan time.Time
collect:
	for got < len(panel) {
		select {
		case r := <-ch:
			got++
			if r.err != nil {
				log.Printf("FUSION %s: %v", c.Name, r.err)
				continue
			}
			answers[r.i] = r.text
			if ok++; ok == quorum && grace == nil && got < len(panel) {
				grace = time.After(fusionGraceAfter)
			}
		case <-grace:
			log.Printf("FUSION %s: %d of %d answered; not waiting for the rest", c.Name, ok, len(panel))
			break collect
		case <-ctx.Done():
			break collect
		}
	}
	var texts, from []string
	for i, a := range answers {
		if a != "" {
			texts, from = append(texts, a), append(from, panel[i])
		}
	}
	switch len(texts) {
	case 0:
		writeError(w, p.cf, http.StatusServiceUnavailable, "api_error", "all fusion panel models failed")
		return
	case 1:
		log.Printf("FUSION %s: only %s answered; replying with it directly", c.Name, from[0])
		s.routeModel(w, p, st, from[0])
		return
	}
	j := *ir
	j.Messages = append(append([]tr.Message{}, ir.Messages...), tr.Message{Role: "user", Parts: []tr.Part{{Type: tr.PartText, Text: judgePrompt(texts)}}})
	body, err := tr.EmitRequest(p.cf, &j, tr.EmitOptions{Model: judge})
	if err != nil {
		writeError(w, p.cf, http.StatusInternalServerError, "api_error", "could not build the judge request: "+err.Error())
		return
	}
	p2 := &proxyReq{r: p.r, cf: p.cf, body: body, model: judge, stream: p.stream}
	if err := scanHead(body, &p2.head); err != nil {
		writeError(w, p.cf, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	p2.model, p2.stream = judge, p.stream
	log.Printf("FUSION %s: judging %d answers with %s", c.Name, len(texts), judge)
	s.routeModel(w, p2, st, judge)
}

func (s *Server) routeModel(w http.ResponseWriter, p *proxyReq, st *router.State, model string) {
	targets, err := st.Resolve(model)
	if err != nil {
		writeError(w, p.cf, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	s.route(w, p, targets)
}

// cascade answers with the cheapest model, asks a decision engine whether the
// draft is good enough, and escalates to the next (stronger) model only on
// "escalate". The cheap answer is buffered so the verdict is read before the
// client sees anything; a streaming client still receives a real SSE stream.
func (s *Server) cascade(w http.ResponseWriter, p *proxyReq, st *router.State, c *router.Combo) {
	ladder := c.Models
	if len(ladder) == 0 {
		writeError(w, p.cf, http.StatusNotFound, "model_not_found", "combo "+c.Name+" has no models")
		return
	}
	ir, err := p.parse()
	if err != nil {
		writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// A tool call has to stream from the model that emits it, so a request with
	// tools goes straight to the strongest step.
	if len(ladder) == 1 || len(ir.Tools) > 0 {
		s.routeModel(w, p, st, ladder[len(ladder)-1])
		return
	}
	q := *ir
	q.Stream = false
	q.Tools, q.ToolChoice, q.ParallelToolCalls = nil, nil, nil
	ctx, cancel := context.WithTimeout(p.r.Context(), cascadeHardLimit)
	defer cancel()
	user := lastUserText(ir)
	for i, m := range ladder {
		if i == len(ladder)-1 {
			log.Printf("CASCADE %s: escalating to %s", c.Name, m)
			s.routeModel(w, p, st, m)
			return
		}
		resp, err := s.internalCall(ctx, p.r, &q, m)
		if err != nil {
			log.Printf("CASCADE %s: %s failed (%v); escalating", c.Name, m, err)
			continue
		}
		text := strings.TrimSpace(partsText(resp.Parts))
		if text == "" {
			log.Printf("CASCADE %s: %s answered empty; escalating", c.Name, m)
			continue
		}
		verdict, err := s.cascadeVerdict(ctx, p, c, user, text)
		switch {
		case err != nil:
			// No verdict is not a reason to spend a stronger model: send the
			// draft, which is exactly what a fallback combo would have done.
			log.Printf("CASCADE %s: no verdict for %s (%v); sending its answer", c.Name, m, err)
			writeBufferedAnswer(w, p, m, resp)
			return
		case verdict == "escalate":
			log.Printf("CASCADE %s: %s escalated by the decision engine", c.Name, m)
		default:
			log.Printf("CASCADE %s: %s accepted by the decision engine", c.Name, m)
			writeBufferedAnswer(w, p, m, resp)
			return
		}
	}
}

// cascadeVerdict asks the decision engine whether a draft may be sent.
func (s *Server) cascadeVerdict(ctx context.Context, p *proxyReq, c *router.Combo, user, draft string) (string, error) {
	state, _ := json.Marshal("Request:\n" + user + "\n\nDraft answer:\n" + clipAnswer(draft))
	questions, _ := json.Marshal(map[string]any{"verdict": map[string]any{
		"type":         "choice",
		"instructions": "Is this draft good enough to send to the user?",
		"criteria": map[string]any{
			"accept":   "correct, complete and safe to send",
			"escalate": "wrong, incomplete, off-topic or unsafe to send",
		},
	}})
	start := time.Now()
	res, err := s.decisionAnswer(ctx, decisionReq{Model: c.DecisionModel, State: state, Questions: questions})
	if err != nil {
		return "", err
	}
	if res.Account != nil {
		s.recordDecision(p.r, start, res, res.Account)
	}
	if res.Status < 200 || res.Status >= 300 {
		return "", fmt.Errorf("HTTP %d", res.Status)
	}
	pick, err := parseDecisionChoice(res.Body, "verdict")
	if err != nil {
		return "", err
	}
	return strings.ToLower(pick.Choice), nil
}

// writeBufferedAnswer relays an answer that was collected before the client
// saw anything, in the client's format and streaming mode.
func writeBufferedAnswer(w http.ResponseWriter, p *proxyReq, model string, resp *tr.Response) {
	text := partsText(resp.Parts)
	if p.stream {
		enc := tr.NewEncoder(p.cf, model)
		out := enc.Encode([]tr.Event{
			{Kind: tr.EvStart, Name: model},
			{Kind: tr.EvUsage, Usage: resp.Usage},
			{Kind: tr.EvText, Text: text},
			{Kind: tr.EvUsage, Usage: resp.Usage},
			{Kind: tr.EvFinish, Finish: tr.FinishStop},
		})
		out = append(out, enc.Close()...)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-FastRouter-Model", model)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
	out, _ := tr.EmitResponse(p.cf, &tr.Response{Model: model, Parts: []tr.Part{{Type: tr.PartText, Text: text}},
		Finish: tr.FinishStop, Usage: resp.Usage}, model)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-FastRouter-Model", model)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
