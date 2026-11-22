package server

// Decision engines: typed, low-latency judgments (choice / score / predicate)
// instead of a chat turn. TypeSafe Jev and OpenAI Decisions speak different
// request shapes for the same idea, so this endpoint accepts either shape,
// rewrites it per target, and fails over between engines and accounts.
//
// The engines are not chat models: they are never listed in /v1/models and a
// chat request cannot reach them. This is the only route that uses them.
//
// Two savings sit in front of the engines:
//   - a local gate answers choice questions whose state matches a configured
//     phrase (settings decisionSettings.gates) without any network call;
//   - a short-lived cache returns the same answer for the same canonical
//     request, so polling tools do not pay for a repeated judgment.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nesaon03/fastrouter/internal/catalog"
	"github.com/nesaon03/fastrouter/internal/router"
	"github.com/nesaon03/fastrouter/internal/secret"
	"github.com/nesaon03/fastrouter/internal/store"
	tr "github.com/nesaon03/fastrouter/internal/translate"
)

const (
	decisionMaxBody = 4 << 20
	decisionTimeout = 60 * time.Second
	// decisionCacheMax bounds the in-memory answer cache.
	decisionCacheMax = 512
	// defaultDecisionCacheSeconds keeps a repeated judgment free for 15 minutes.
	defaultDecisionCacheSeconds = 900
)

// decisionOrder tries the cheap single-purpose engine before a general one.
var decisionOrder = []string{"typesafe", "openai"}

// Client-side decision failures (mapped to 400 vs 502 by the handler).
var (
	errNoDecisionProvider = errors.New("no decision provider configured: add TypeSafe Jev or OpenAI under Providers, then retry")
	errNoDecisionAnswered = errors.New("no decision provider answered")
)

type decisionReq struct {
	Model     string          `json:"model"`
	State     json.RawMessage `json:"state"` // Jev: shared evidence
	Input     json.RawMessage `json:"input"` // Decisions: same idea, other name
	Questions json.RawMessage `json:"questions"`
	// Extra carries every other top-level field (assets for image decisions,
	// effort, images, ...) so a provider extension is forwarded, not dropped.
	Extra map[string]any `json:"-"`
	// DangerImages is the TypeSafe-Danger-Zone-Images header value, forwarded
	// because image and Box questions are gated on it.
	DangerImages string `json:"-"`
}

type decisionTarget struct {
	id, prefix string
	url, model string
	pool       *router.Pool // nil when no account is configured
}

// decisionSettings is settings.decisionSettings.
type decisionSettings struct {
	CacheSeconds int                 `json:"cacheSeconds"` // 0 = off
	Gates        map[string][]string `json:"gates"`        // verdict -> phrases
}

func (s *Server) decisionSettings() decisionSettings {
	ds := decisionSettings{CacheSeconds: defaultDecisionCacheSeconds}
	if st := s.state.Load(); st != nil {
		_ = json.Unmarshal(st.Snap.Settings["decisionSettings"], &ds)
	}
	ds.CacheSeconds = max(0, min(ds.CacheSeconds, 86400))
	return ds
}

// decisionResult is one answer plus what the caller needs to relay and price it.
type decisionResult struct {
	Status      int
	Body        []byte
	ContentType string
	Provider    string
	Model       string
	Account     *router.Account // nil for a gate or cache hit
	Cached      bool
	Gated       bool
}

// handleDecisions serves POST /v1/decisions.
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	in, err := readDecisionRequest(r)
	if err != nil {
		writeError(w, tr.OpenAI, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	res, err := s.decisionAnswer(r.Context(), in)
	if err != nil {
		if errors.Is(err, errNoDecisionProvider) {
			writeError(w, tr.OpenAI, http.StatusBadRequest, "invalid_request_error", err.Error())
		} else {
			writeError(w, tr.OpenAI, http.StatusBadGateway, "api_error", err.Error())
		}
		return
	}
	if res.Account != nil {
		s.recordDecision(r, start, res, res.Account)
	}
	h := w.Header()
	h.Set("Content-Type", firstNonEmpty(res.ContentType, "application/json"))
	h.Set("X-FastRouter-Provider", res.Provider)
	h.Set("X-FastRouter-Model", res.Model)
	if res.Account != nil {
		h.Set("X-FastRouter-Account", res.Account.ID)
	}
	if res.Cached {
		h.Set("X-FastRouter-Cache", "hit")
	}
	if res.Gated {
		h.Set("X-FastRouter-Gate", "hit")
	}
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

func readDecisionRequest(r *http.Request) (decisionReq, error) {
	var in decisionReq
	raw, err := io.ReadAll(io.LimitReader(r.Body, decisionMaxBody))
	if err != nil {
		return in, fmt.Errorf("could not read request: %w", err)
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("request is not JSON: %w", err)
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err == nil {
		delete(all, "model")
		delete(all, "state")
		delete(all, "input")
		delete(all, "questions")
		in.Extra = all
	}
	in.DangerImages = r.Header.Get("TypeSafe-Danger-Zone-Images")
	if _, _, _, err := decodeDecisionQuestions(in.Questions); err != nil {
		return in, err
	}
	return in, nil
}

// decisionAnswer answers one canonical request: cache, local gate, then the
// engines. An error means the request itself cannot be served (no provider, bad
// shape); an upstream failure comes back as a result with its status.
func (s *Server) decisionAnswer(ctx context.Context, in decisionReq) (*decisionResult, error) {
	targets := decisionTargets(s.state.Load(), in.Model)
	usable := false
	for _, t := range targets {
		if t.pool != nil && t.pool.Size() > 0 {
			usable = true
			break
		}
	}
	if !usable {
		return nil, errNoDecisionProvider
	}
	ds := s.decisionSettings()

	key, keyOK := decisionCacheKey(in)
	if ds.CacheSeconds > 0 && keyOK {
		if e, ok := s.decisionCacheGet(key, time.Duration(ds.CacheSeconds)*time.Second); ok {
			return &decisionResult{Status: e.status, Body: e.body, ContentType: e.contentType,
				Provider: e.provider, Model: e.model, Cached: true}, nil
		}
	}

	gated, remaining := applyDecisionGates(ds.Gates, in)
	var res *decisionResult
	var err error
	if len(gated) > 0 && len(bytes.TrimSpace(remaining)) == 0 {
		// Every question was settled locally: no engine call at all.
		t := targets[0]
		model := decisionModel(t, in.Model)
		res = &decisionResult{Status: http.StatusOK, ContentType: "application/json",
			Body: syntheticDecisionAnswers(t, gated, model), Provider: t.id, Model: model, Gated: true}
	} else {
		inner := in
		if len(gated) > 0 {
			inner.Questions = remaining
		}
		res, err = s.runDecision(ctx, targets, inner)
		if err != nil {
			return nil, err
		}
		if len(gated) > 0 {
			res.Body = mergeGatedAnswers(res.Body, gated)
			res.Gated = true
		}
	}
	if ds.CacheSeconds > 0 && keyOK && res.Status >= 200 && res.Status < 300 {
		s.decisionCachePut(key, decisionCacheEntry{status: res.Status, body: res.Body,
			contentType: res.ContentType, provider: res.Provider, model: res.Model})
	}
	return res, nil
}

// runDecision sends the request to the first engine/account that answers.
func (s *Server) runDecision(ctx context.Context, targets []decisionTarget, in decisionReq) (*decisionResult, error) {
	ctx, cancel := context.WithTimeout(ctx, decisionTimeout)
	defer cancel()
	var reasons []string
	var lastStatus int
	var lastBody []byte
	for _, t := range targets {
		if t.pool == nil || t.pool.Size() == 0 {
			reasons = append(reasons, t.id+": no active accounts")
			continue
		}
		body, err := decisionBody(t, in)
		if err != nil {
			return nil, err
		}
		model := decisionModel(t, in.Model)
		for _, a := range t.pool.Order(time.Now()) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("could not build the decision request")
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "fastrouter/"+Version)
			req.Header.Set("Authorization", "Bearer "+a.Key())
			if in.DangerImages != "" {
				req.Header.Set("TypeSafe-Danger-Zone-Images", in.DangerImages)
			}
			resp, err := s.client.Do(req)
			if err != nil {
				if ctx.Err() == nil {
					a.Fail(s.cfgCooldown, 0)
					reasons = append(reasons, t.id+": "+shortNetErr(err))
				}
				continue
			}
			out, _ := io.ReadAll(io.LimitReader(resp.Body, decisionMaxBody))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				a.Succeed()
				return &decisionResult{Status: resp.StatusCode, Body: out,
					ContentType: firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"),
					Provider:    t.id, Model: model, Account: a}, nil
			}
			lastStatus, lastBody = resp.StatusCode, out
			logDecisionFailure(t, resp.StatusCode, out)
			if !retryable(resp.StatusCode) {
				// The request itself is wrong (shape, model id, payload). Another
				// engine will reject it the same way, so relay it as-is.
				return &decisionResult{Status: resp.StatusCode, Body: out,
					ContentType: firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"),
					Provider:    t.id, Model: model}, nil
			}
			a.Fail(s.cfgCooldown, retryAfter(resp.Header))
			reasons = append(reasons, fmt.Sprintf("%s: HTTP %d", t.id, resp.StatusCode))
		}
	}
	if lastStatus != 0 && len(lastBody) > 0 {
		return &decisionResult{Status: lastStatus, Body: lastBody, ContentType: "application/json"}, nil
	}
	msg := "no decision provider answered"
	if len(reasons) > 0 {
		msg += ": " + strings.Join(reasons, "; ")
	}
	return nil, fmt.Errorf("%w: %s", errNoDecisionAnswered, msg)
}

// ---- cache ----

type decisionCacheEntry struct {
	status      int
	body        []byte
	contentType string
	provider    string
	model       string
	at          time.Time
}

// decisionCacheKey canonicalises the whole request (field order and whitespace
// do not matter) so the same judgment hits regardless of who asks.
func decisionCacheKey(in decisionReq) ([32]byte, bool) {
	var zero [32]byte
	state := in.State
	if len(bytes.TrimSpace(state)) == 0 {
		state = in.Input
	}
	var sv, qv any
	if len(bytes.TrimSpace(state)) > 0 {
		if err := json.Unmarshal(state, &sv); err != nil {
			return zero, false
		}
	}
	if err := json.Unmarshal(in.Questions, &qv); err != nil {
		return zero, false
	}
	canon, err := json.Marshal([]any{strings.ToLower(strings.TrimSpace(in.Model)), sv, qv, in.Extra})
	if err != nil {
		return zero, false
	}
	return sha256.Sum256(canon), true
}

func (s *Server) decisionCacheGet(key [32]byte, ttl time.Duration) (decisionCacheEntry, bool) {
	s.decMu.Lock()
	defer s.decMu.Unlock()
	e, ok := s.decCache[key]
	if !ok {
		return decisionCacheEntry{}, false
	}
	if ttl <= 0 || time.Since(e.at) > ttl {
		delete(s.decCache, key)
		return decisionCacheEntry{}, false
	}
	return e, true
}

func (s *Server) decisionCachePut(key [32]byte, e decisionCacheEntry) {
	s.decMu.Lock()
	defer s.decMu.Unlock()
	if s.decCache == nil {
		s.decCache = map[[32]byte]decisionCacheEntry{}
	}
	if len(s.decCache) >= decisionCacheMax {
		now := time.Now()
		for k, v := range s.decCache {
			if now.Sub(v.at) > time.Hour || len(s.decCache) >= decisionCacheMax {
				delete(s.decCache, k)
			}
		}
		if len(s.decCache) >= decisionCacheMax {
			return // still full of live entries: do not grow past the cap
		}
	}
	e.at = time.Now()
	s.decCache[key] = e
}

// ---- local gate ----

// applyDecisionGates answers the choice questions whose text matches a
// configured phrase. It returns the answers it settled and the questions that
// still need an engine (nil when nothing is left).
func applyDecisionGates(gates map[string][]string, in decisionReq) (map[string]string, json.RawMessage) {
	if len(gates) == 0 {
		return nil, in.Questions
	}
	state := in.State
	if len(bytes.TrimSpace(state)) == 0 {
		state = in.Input
	}
	stateText := strings.TrimSpace(string(state))
	var stateVal any
	if json.Unmarshal(state, &stateVal) == nil {
		if s, ok := stateVal.(string); ok {
			stateText = s
		} else {
			b, _ := json.Marshal(stateVal)
			stateText = string(b)
		}
	}
	qsMap, qsList, isMap, err := decodeDecisionQuestions(in.Questions)
	if err != nil {
		return nil, in.Questions
	}
	settled := map[string]string{}

	if isMap {
		rest := map[string]any{}
		names := sortedKeys(qsMap)
		for _, name := range names {
			q, _ := qsMap[name].(map[string]any)
			if v := gateQuestion(gates, stateText, name, q); v != "" {
				settled[name] = v
				continue
			}
			rest[name] = qsMap[name]
		}
		if len(settled) == 0 {
			return nil, in.Questions
		}
		if len(rest) == 0 {
			return settled, nil
		}
		b, _ := json.Marshal(rest)
		return settled, b
	}

	var rest []any
	for _, item := range qsList {
		q, _ := item.(map[string]any)
		name, _ := q["name"].(string)
		if name != "" {
			if v := gateQuestion(gates, stateText, name, q); v != "" {
				settled[name] = v
				continue
			}
		}
		rest = append(rest, item)
	}
	if len(settled) == 0 {
		return nil, in.Questions
	}
	if len(rest) == 0 {
		return settled, nil
	}
	b, _ := json.Marshal(rest)
	return settled, b
}

// gateQuestion returns a verdict when the question is a choice, offers that
// verdict, and the state or instructions contain one of its phrases.
func gateQuestion(gates map[string][]string, stateText, name string, q map[string]any) string {
	if typ, _ := q["type"].(string); typ != "choice" {
		return ""
	}
	criteria := choiceValues(q)
	if len(criteria) == 0 {
		return ""
	}
	instructions, _ := q["instructions"].(string)
	hay := strings.ToLower(stateText + " " + name + " " + instructions)
	for _, verdict := range gateOrder(gates) {
		if !criteria[verdict] {
			continue
		}
		for _, phrase := range gates[verdict] {
			if p := strings.ToLower(strings.TrimSpace(phrase)); p != "" && strings.Contains(hay, p) {
				return verdict
			}
		}
	}
	return ""
}

// choiceValues reads the offered choice keys from either shape.
func choiceValues(q map[string]any) map[string]bool {
	out := map[string]bool{}
	if crit, ok := q["criteria"].(map[string]any); ok {
		for k := range crit {
			out[k] = true
		}
	}
	if choices, ok := q["choices"].([]any); ok {
		for _, ch := range choices {
			cm, _ := ch.(map[string]any)
			if v, _ := cm["value"].(string); v != "" {
				out[v] = true
			}
		}
	}
	return out
}

// gateOrder is deterministic and checks the safest verdicts first.
func gateOrder(gates map[string][]string) []string {
	out := make([]string, 0, len(gates))
	for v := range gates {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		rank := func(v string) int {
			switch strings.ToLower(v) {
			case "reject", "deny", "no":
				return 0
			case "review", "escalate", "unclear":
				return 1
			case "accept", "allow", "yes":
				return 2
			}
			return 3
		}
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// syntheticDecisionAnswers builds an engine-shaped response from gate answers.
func syntheticDecisionAnswers(t decisionTarget, gated map[string]string, model string) []byte {
	names := make([]string, 0, len(gated))
	for n := range gated {
		names = append(names, n)
	}
	sort.Strings(names)
	if jevShaped(t.id) {
		answers := map[string]any{}
		for _, n := range names {
			answers[n] = map[string]any{"type": "choice", "choice": gated[n], "confidence": nil, "source": "local-gate"}
		}
		b, _ := json.Marshal(map[string]any{"model": model, "answers": answers, "source": "local-gate"})
		return b
	}
	var answers []any
	for _, n := range names {
		answers = append(answers, map[string]any{"type": "choice", "name": n, "choice": gated[n], "confidence": nil, "source": "local-gate"})
	}
	b, _ := json.Marshal(map[string]any{"model": model, "answers": answers, "source": "local-gate"})
	return b
}

// mergeGatedAnswers adds the locally settled answers to the engine's response.
// If the body is not the expected object, the engine answer is returned as-is.
func mergeGatedAnswers(body []byte, gated map[string]string) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	names := make([]string, 0, len(gated))
	for n := range gated {
		names = append(names, n)
	}
	sort.Strings(names)
	switch answers := doc["answers"].(type) {
	case map[string]any:
		for _, n := range names {
			answers[n] = map[string]any{"type": "choice", "choice": gated[n], "confidence": nil, "source": "local-gate"}
		}
	case []any:
		for _, n := range names {
			answers = append(answers, map[string]any{"type": "choice", "name": n, "choice": gated[n], "confidence": nil, "source": "local-gate"})
		}
		doc["answers"] = answers
	default:
		return body
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// ---- routing ----

// decisionTargets resolves the requested model to the engines to try, in order.
// A provider/model prefix or an exact engine model id pins the engine; anything
// else walks the known engines (cheap first), then any other decision provider.
func decisionTargets(st *router.State, model string) []decisionTarget {
	model = strings.TrimSpace(model)
	if st == nil {
		return nil
	}
	add := func(c catalog.Provider) (decisionTarget, bool) {
		if c.DecisionURL == "" {
			return decisionTarget{}, false
		}
		t := decisionTarget{id: c.ID, prefix: c.PrimaryAlias(), url: c.DecisionURL, model: c.DecisionModel}
		if p, ok := st.Provider(c.ID); ok {
			t.pool = p.Pool
		}
		return t, true
	}

	// "provider/model" pins both the engine and the id.
	if pre, name, ok := strings.Cut(model, "/"); ok && name != "" {
		if c, ok := catalog.Lookup(pre); ok && c.DecisionURL != "" {
			t, _ := add(*c)
			t.model = name
			return []decisionTarget{t}
		}
	}
	// An exact engine model id pins the engine too.
	var pinned string
	for _, c := range catalog.All() {
		if c.DecisionURL != "" && model != "" && strings.EqualFold(c.DecisionModel, model) {
			pinned = c.ID
		}
	}

	var out []decisionTarget
	seen := map[string]bool{}
	push := func(id string) {
		c, ok := catalog.Lookup(id)
		if !ok || seen[c.ID] {
			return
		}
		if t, ok := add(*c); ok {
			seen[c.ID] = true
			out = append(out, t)
		}
	}
	if pinned != "" {
		push(pinned)
	}
	for _, id := range decisionOrder {
		push(id)
	}
	for _, c := range catalog.All() {
		push(c.ID)
	}
	return out
}

// jevShaped reports which request shape a provider expects.
func jevShaped(id string) bool { return id == "typesafe" }

// decisionModel picks the id sent to one target: a pinned "provider/model"
// keeps its id, another engine's known id is replaced by this engine's default,
// and anything else is assumed to be this engine's own model id.
func decisionModel(t decisionTarget, user string) string {
	user = strings.TrimSpace(user)
	if user == "" || user == "auto" || user == "decisions" {
		return t.model
	}
	if pre, name, ok := strings.Cut(user, "/"); ok && name != "" {
		if c, ok := catalog.Lookup(pre); ok && c.ID == t.id {
			return name
		}
	}
	if strings.EqualFold(user, t.model) {
		return user
	}
	for _, c := range catalog.All() {
		if c.DecisionURL != "" && strings.EqualFold(c.DecisionModel, user) {
			return t.model // belongs to another engine
		}
	}
	if _, _, ok := strings.Cut(user, "/"); !ok {
		return user
	}
	return t.model
}

// decisionBody rewrites the canonical request into the target's shape.
func decisionBody(t decisionTarget, in decisionReq) ([]byte, error) {
	qsMap, qsList, isMap, err := decodeDecisionQuestions(in.Questions)
	if err != nil {
		return nil, err
	}
	state := in.State
	if len(bytes.TrimSpace(state)) == 0 {
		state = in.Input
	}
	var stateVal any
	if len(bytes.TrimSpace(state)) > 0 {
		if err := json.Unmarshal(state, &stateVal); err != nil {
			return nil, fmt.Errorf("state/input is not valid JSON: %w", err)
		}
	}
	body := make(map[string]any, len(in.Extra)+3)
	for k, v := range in.Extra {
		if strings.HasPrefix(k, "_") {
			continue // internal markers, never sent upstream
		}
		body[k] = v
	}
	body["model"] = decisionModel(t, in.Model)
	if jevShaped(t.id) {
		delete(body, "input") // the client may have used the Decisions name
		if !isMap {
			qsMap = jevQuestionsFromList(qsList)
		}
		body["questions"] = qsMap
		if stateVal != nil {
			body["state"] = stateVal
		}
	} else {
		delete(body, "state")
		if isMap {
			qsList = decisionsQuestionsFromMap(qsMap)
		}
		body["questions"] = qsList
		if stateVal != nil {
			body["input"] = stateVal
		}
	}
	return json.Marshal(body)
}

// ---- shapes ----

// decodeDecisionQuestions accepts Jev's name-keyed object and Decisions' list.
func decodeDecisionQuestions(raw json.RawMessage) (map[string]any, []any, bool, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 {
		return nil, nil, false, fmt.Errorf("questions is required")
	}
	switch trim[0] {
	case '{':
		var m map[string]any
		if err := json.Unmarshal(trim, &m); err != nil {
			return nil, nil, false, fmt.Errorf("questions is not an object: %w", err)
		}
		if len(m) == 0 {
			return nil, nil, false, fmt.Errorf("questions is empty")
		}
		for name, q := range m {
			if _, ok := q.(map[string]any); !ok {
				return nil, nil, false, fmt.Errorf("question %q is not an object", name)
			}
		}
		return m, nil, true, nil
	case '[':
		var l []any
		if err := json.Unmarshal(trim, &l); err != nil {
			return nil, nil, false, fmt.Errorf("questions is not an array: %w", err)
		}
		if len(l) == 0 {
			return nil, nil, false, fmt.Errorf("questions is empty")
		}
		for i, q := range l {
			if _, ok := q.(map[string]any); !ok {
				return nil, nil, false, fmt.Errorf("question %d is not an object", i+1)
			}
		}
		return nil, l, false, nil
	}
	return nil, nil, false, fmt.Errorf("questions must be an object or an array")
}

// jevQuestionsFromList converts Decisions' [{name,type,...}] into Jev's map.
func jevQuestionsFromList(list []any) map[string]any {
	out := make(map[string]any, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			name = fmt.Sprintf("q%d", i+1)
		}
		q := map[string]any{}
		switch typ, _ := m["type"].(string); typ {
		case "choice":
			crit := map[string]any{}
			if choices, ok := m["choices"].([]any); ok {
				for _, ch := range choices {
					cm, _ := ch.(map[string]any)
					v, _ := cm["value"].(string)
					d, _ := cm["description"].(string)
					if v != "" {
						crit[v] = d
					}
				}
			}
			q["type"] = "choice"
			q["criteria"] = crit
		case "score":
			var crit []any
			if levels, ok := m["levels"].([]any); ok {
				for _, lv := range levels {
					if lm, ok := lv.(map[string]any); ok {
						crit = append(crit, firstNonEmpty(str(lm["label"]), str(lm["description"])))
					} else {
						crit = append(crit, lv)
					}
				}
			}
			q["type"] = "score"
			q["criteria"] = crit
		case "predicate":
			q["type"] = "noul"
		default:
			q["type"] = typ
		}
		if ins, ok := m["instructions"].(string); ok {
			q["instructions"] = ins
		}
		out[name] = q
	}
	return out
}

// decisionsQuestionsFromMap converts Jev's name-keyed map into Decisions' list.
func decisionsQuestionsFromMap(m map[string]any) []any {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, name := range names {
		qm, ok := m[name].(map[string]any)
		if !ok {
			continue
		}
		q := map[string]any{"name": name}
		switch typ, _ := qm["type"].(string); typ {
		case "choice":
			q["type"] = "choice"
			var choices []any
			if crit, ok := qm["criteria"].(map[string]any); ok {
				for _, k := range sortedKeys(crit) {
					choices = append(choices, map[string]any{"value": k, "description": str(crit[k])})
				}
			}
			q["choices"] = choices
		case "score":
			q["type"] = "score"
			var levels []any
			switch c := qm["criteria"].(type) {
			case []any:
				for _, l := range c {
					levels = append(levels, map[string]any{"label": str(l), "description": str(l)})
				}
			case map[string]any:
				for _, k := range sortedKeys(c) {
					levels = append(levels, map[string]any{"label": k, "description": str(c[k])})
				}
			}
			q["levels"] = levels
		case "noul":
			q["type"] = "predicate"
		default:
			q["type"] = typ
		}
		if ins, ok := qm["instructions"].(string); ok {
			q["instructions"] = ins
		}
		out = append(out, q)
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- usage + probe ----

// recordDecision prices and stores one decision call like a chat attempt.
func (s *Server) recordDecision(r *http.Request, start time.Time, res *decisionResult, a *router.Account) {
	var env struct {
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(res.Body, &env)
	keyID := ""
	if k := keyFrom(r.Context()); k != nil {
		keyID = k.ID
	}
	prefix := ""
	if p, ok := s.state.Load().Provider(res.Provider); ok {
		prefix = p.Prefix
	}
	s.rec.record(usageItem{
		ev: store.UsageEvent{At: start, ConnectionID: a.ID, Provider: res.Provider, KeyID: keyID, Model: res.Model,
			Status: res.Status, Latency: time.Since(start), Input: int64(env.Usage.Input), Output: int64(env.Usage.Output)},
		model: res.Model, prefix: prefix,
	})
}

// decisionProbe checks a decision endpoint without spending a question: the
// method is wrong for a POST-only API, so a 404/405 means reachable, and
// 401/403 still identifies a rejected key.
func (s *Server) decisionProbe(ctx context.Context, p *router.Provider, a *router.Account) probeResult {
	res := probeResult{Provider: p.ID, Account: a.ID, Name: maskEmail(a.Name)}
	start := time.Now()
	defer func() { res.Ms = time.Since(start).Milliseconds() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.DecisionURL, nil)
	if err != nil {
		res.Message = "Bad decision endpoint URL"
		return res
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fastrouter/"+Version)
	req.Header.Set("Authorization", "Bearer "+a.Key())
	resp, err := s.client.Do(req)
	if err != nil {
		res.Message = "Network error: " + shortNetErr(err)
		return res
	}
	resp.Body.Close()
	res.Status = resp.StatusCode
	switch {
	case resp.StatusCode < 300:
		res.OK, res.Message = true, "Reachable"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Message = "Key rejected"
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		res.OK, res.Message = true, "Reachable (decision endpoint, key not verified)"
	case resp.StatusCode == 429:
		res.OK, res.Message = true, "Key accepted, rate limited right now"
	default:
		res.Message = http.StatusText(resp.StatusCode)
	}
	return res
}

// logDecisionFailure keeps a provider-shaped error in the log without leaking a
// key or a full body.
func logDecisionFailure(t decisionTarget, status int, body []byte) {
	log.Printf("decision %s: HTTP %d %s", t.id, status, secret.Redact(upstreamMessage(body)))
}
