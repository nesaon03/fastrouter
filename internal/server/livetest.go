package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/secret"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

// Live provider test: one tiny chat request ("Say OK", max 5 tokens) per
// account through the normal routing path, pinned to that account. Results
// carry the provider, account label, status, latency and an error class only:
// never keys, tokens or response bodies.

// LiveResult is one account's live-test outcome.
type LiveResult struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	OK       bool   `json:"ok"`
	Status   int    `json:"status,omitempty"`
	Class    string `json:"class,omitempty"` // auth | banned | quota | rate_limit | model | bad_request | overloaded | upstream | network | timeout | empty
	Message  string `json:"message"`
	Ms       int64  `json:"ms"`
}

// LiveOptions selects what to test.
type LiveOptions struct {
	Provider string            // "" = all
	Account  string            // "" = all
	Models   map[string]string // provider id -> model to use (default: first chat model)
}

// testModel picks a chat model for provider p: one that recently worked for
// this provider, else one from a combo, else a cheap catalog default.
func testModel(st *router.State, p *router.Provider, override string, working map[string]string) string {
	if override != "" {
		return override
	}
	skip := func(id string) bool {
		l := strings.ToLower(id)
		for _, w := range []string{"embed", "image", "veo", "tts", "whisper", "rerank", "moderation", "audio", "imagen"} {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	pref := map[string]string{"antigravity": "gemini-3.8-flash-low", "gemini-cli": "gemini-3-flash-preview", "github": "gpt-5-mini",
		"claude": "claude-haiku-4-5-20251001", "deepseek": "deepseek-v4.1-flash", "openai": "gpt-5-mini", "gemini": "gemini-3.8-flash",
		"grok-cli": "grok-4.5", "vertex": "gemini-3-flash-preview", "kimi": "kimi-for-coding", "minimax-code": "MiniMax-M2.7",
		"minimax-code-global": "MiniMax-M2.7", "openrouter": "openrouter/free", "cursor": "default"}
	if m := pref[p.ID]; m != "" {
		return m
	}
	if m := working[p.ID]; m != "" {
		for k, v := range p.Upstream {
			if v == m {
				m = k
			}
		}
		known := len(catalog.Models(p.ID)) == 0
		for _, cm := range catalog.Models(p.ID) {
			known = known || cm.ID == m
		}
		if known { // a model this provider still lists and that worked recently
			return m
		}
	}
	for _, c := range st.Snap.Combos {
		for _, m := range c.Models {
			if pre, name, ok := strings.Cut(m, "/"); ok && (pre == st.PrefixFor(p) || pre == p.ID) {
				return name
			}
		}
	}
	for _, m := range catalog.Models(p.ID) {
		if !skip(m.ID) {
			return m.ID
		}
	}
	prefix := st.PrefixFor(p) + "/"
	for _, m := range st.Models {
		if strings.HasPrefix(m, prefix) && !skip(m) {
			return strings.TrimPrefix(m, prefix)
		}
	}
	return ""
}

// captureWriter records a proxied answer in memory.
type captureWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.h }
func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if c.buf.Len() < 64<<10 {
		c.buf.Write(b)
	}
	return len(b), nil
}
func (c *captureWriter) Flush() {}

// outOfFunds spots "no balance / out of credits" errors some gateways send
// as 401/403 (modelhub answers in Russian: "Недостаточно баланса").
func outOfFunds(l string) bool {
	for _, w := range []string{"balance", "credit", "insufficient funds", "top up", "payment", "баланс", "余额"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

func errClass(status int, msg string) string {
	l := strings.ToLower(msg)
	switch {
	case tosBanned(status, msg):
		return "banned"
	case status == 503 && (strings.Contains(l, "demand") || strings.Contains(l, "overloaded")):
		return "overloaded"
	case status == 401 || status == 403:
		if strings.Contains(l, "quota") || strings.Contains(l, "billing") || strings.Contains(l, "subscription") || outOfFunds(l) {
			return "quota"
		}
		return "auth"
	case status == 402:
		return "quota"
	case status == 429:
		if strings.Contains(l, "quota") || strings.Contains(l, "exhausted") || strings.Contains(l, "limit reached") {
			return "quota"
		}
		return "rate_limit"
	case status == 404:
		return "model"
	case status == 400 || status == 422:
		if strings.Contains(l, "model") {
			return "model"
		}
		return "bad_request"
	case status >= 500 && strings.Contains(l, "timeout"):
		return "timeout"
	case status >= 500:
		return "upstream"
	}
	return "network"
}

// liveTest runs the pinned "Say OK" request for one account.
func (s *Server) liveTest(ctx context.Context, st *router.State, p *router.Provider, a *router.Account, model string) (res LiveResult) {
	res = LiveResult{Provider: p.ID, Account: a.ID, Name: maskEmail(a.Name), Model: model}
	start := time.Now()
	defer func() { res.Ms = time.Since(start).Milliseconds() }()
	if model == "" {
		res.Class, res.Message = "model", "no chat model known for this provider"
		return res
	}
	label := st.PrefixFor(p) + "/" + model
	targets, err := st.Resolve(label)
	if err != nil || len(targets) == 0 {
		res.Class, res.Message = "model", "model does not resolve"
		return res
	}
	t := targets[0]
	t.Provider = p.Only(a)
	body := []byte(`{"model":` + strconv.Quote(label) + `,"max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://fastrouter.local/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	pr := &proxyReq{r: r, cf: tr.OpenAI, body: body, model: label}
	_ = scanHead(body, &pr.head)
	cw := &captureWriter{h: http.Header{}}
	_, busy := banChecks.LoadOrStore(a.ID, struct{}{}) // the proxy's background ban check is done inline below
	s.route(cw, pr, []router.Target{t})
	if !busy {
		defer banChecks.Delete(a.ID)
	}
	res.Status = cw.status
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(cw.buf.Bytes(), &out)
	switch {
	case ctx.Err() != nil:
		res.Class, res.Message = "timeout", "no answer in time"
	case cw.status == 200 && len(out.Choices) > 0:
		res.OK, res.Message = true, "OK"
		if connStatus(s.state.Load(), a.ID) == "banned" { // a manual retest passed: usable again
			s.setAccountStatus(a.ID, "ok", "")
		}
		if strings.TrimSpace(out.Choices[0].Message.Content+out.Choices[0].Message.ReasoningContent) == "" {
			res.Message = "OK (answered, empty text: " + out.Choices[0].FinishReason + ")"
		}
	case cw.status == 200:
		res.Class, res.Message = "empty", "answer had no choices"
	case cw.status == http.StatusBadGateway && strings.Contains(out.Error.Message, "could not reach the provider"):
		res.Class, res.Message = "network", classMessage("network", 0)+": "+scrubKey(secret.Redact(out.Error.Message), a.Key())
	default:
		res.Class = errClass(cw.status, out.Error.Message)
		real := scrubKey(secret.Redact(strings.TrimSpace(out.Error.Message)), a.Key())
		if res.Class == "banned" {
			if s.confirmBanned(ctx, p.ID, a) {
				s.setAccountStatus(a.ID, "banned", real)
			} else { // another host 9Router would use still answers: not an account ban
				res.Class = "auth"
			}
		}
		res.Message = classMessage(res.Class, cw.status)
		if real != "" {
			res.Message += ": " + real
		}
	}
	return res
}

func classMessage(class string, status int) string {
	m := map[string]string{"auth": "credential rejected (sign in again / check key)", "banned": "account disabled by the provider (terms of service)",
		"overloaded": "provider overloaded, try later", "quota": "quota or plan exhausted",
		"rate_limit": "rate limited", "model": "model not available for this account", "bad_request": "request rejected",
		"upstream": "provider error", "network": "could not reach the provider", "timeout": "timed out"}[class]
	if status > 0 {
		return m + " (HTTP " + strconv.Itoa(status) + ")"
	}
	return m
}

// LiveTest tests every matching active account, 4 at a time.
func (s *Server) LiveTest(ctx context.Context, o LiveOptions) []LiveResult {
	st := withBannedRoutable(s.state.Load(), o.Provider, o.Account, s.keyLimits) // a manual test may clear a ban
	type job struct {
		p     *router.Provider
		a     *router.Account
		model string
	}
	var jobs []job
	var working map[string]string
	if s.st != nil {
		working, _ = s.st.WorkingModels()
	}
	for _, p := range st.List {
		if p.Plugin || (o.Provider != "" && p.ID != o.Provider) {
			continue
		}
		for _, a := range p.Pool.Accounts() {
			if o.Account == "" || a.ID == o.Account {
				m := testModel(st, p, o.Models[p.ID], working)
				if m == "" { // e.g. a custom endpoint with no catalog: the account's default, else its models list
					m = firstNonEmpty(connMeta(st, a.ID, "defaultModel"), s.discoverModel(ctx, p, a))
				}
				jobs = append(jobs, job{p, a, m})
			}
		}
	}
	out := make([]LiveResult, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			out[i] = s.liveTest(c, st, j.p, j.a, j.model)
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// RefreshDue refreshes OAuth tokens that are due (same rules as the background refresher).
func (s *Server) RefreshDue(ctx context.Context) int {
	if s.st == nil {
		return 0
	}
	n := s.oauthRefresher.RunOnce(ctx)
	return n + s.refresher.RunOnce(ctx)
}

func connMeta(st *router.State, id, k string) string {
	if st.Snap != nil {
		for _, c := range st.Snap.Connections {
			if c.ID == id {
				v, _ := c.Meta[k].(string)
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// discoverModel asks the provider's models list for a chat model to test with.
func (s *Server) discoverModel(ctx context.Context, p *router.Provider, a *router.Account) string {
	u, f := modelsURL(p, a)
	if u == "" || f != tr.OpenAI {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+a.Key())
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out) != nil {
		return ""
	}
	for _, m := range out.Data {
		l := strings.ToLower(m.ID)
		if m.ID != "" && !strings.Contains(l, "embed") && !strings.Contains(l, "rerank") && !strings.Contains(l, "whisper") && !strings.Contains(l, "tts") {
			return m.ID
		}
	}
	return ""
}

func connStatus(st *router.State, id string) string {
	if st.Snap != nil {
		for _, c := range st.Snap.Connections {
			if c.ID == id {
				return c.Status
			}
		}
	}
	return ""
}
