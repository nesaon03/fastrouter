package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/oauth"
	"github.com/zenpo/fastrouter/internal/router"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

// Provider "Test All" (9Router's /api/providers/test-batch): checks each active
// account with a free, read-only call (the models list, or the usage API for
// sign-in accounts). No tokens are spent and nothing is written upstream.

type probeResult struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Status   int    `json:"status,omitempty"`
	Message  string `json:"message"`
	Ms       int64  `json:"ms"`
}

// probeCategory maps a provider to its section on the Providers page.
func probeCategory(p *router.Provider) string {
	if !strings.Contains(p.ID, "compatible") {
		if d, ok := catalog.Info(p.ID); ok {
			if d.Category == "freeTier" {
				return "free"
			}
			return d.Category
		}
		return "apikey"
	}
	return "custom"
}

// modelsURL derives a provider's models endpoint from one of its API URLs.
func modelsURL(p *router.Provider, a *router.Account) (string, tr.Format) {
	for _, f := range []tr.Format{tr.OpenAI, tr.Anthropic, tr.Responses, tr.Gemini} {
		u := a.URLs[f]
		if u == "" {
			u = p.URLs[f]
		}
		if u == "" {
			continue
		}
		u = strings.SplitN(u, "?", 2)[0]
		for _, suf := range []string{"/chat/completions", "/messages", "/responses"} {
			if strings.HasSuffix(u, suf) {
				return strings.TrimSuffix(u, suf) + "/models", f
			}
		}
		if f == tr.Gemini {
			return strings.TrimSuffix(u, "/"), f
		}
	}
	return "", tr.OpenAI
}

func (s *Server) probeAccount(ctx context.Context, p *router.Provider, a *router.Account) probeResult {
	res := probeResult{Provider: p.ID, Account: a.ID, Name: maskEmail(a.Name)}
	start := time.Now()
	defer func() { res.Ms = time.Since(start).Milliseconds() }()
	if p.DecisionURL != "" {
		return s.decisionProbe(ctx, p, a)
	}
	if p.Login == "codex" {
		q := s.fetchQuota(ctx, quotaCred{provider: "codex", key: a.Key(), accountID: a.AccountID})
		res.OK, res.Message = q.Error == "", firstNonEmpty(q.Error, "Signed in, usage API reachable")
		return res
	}
	if p.ID == "cursor" {
		ms, err := oauth.CursorModels(ctx, a.Key(), a.Extra["machineId"], true)
		if err != nil {
			res.Message = "Cursor rejected the session: " + err.Error()
			return res
		}
		res.OK, res.Message = true, fmt.Sprintf("Signed in, %d models available", len(ms))
		return res
	}
	u, f := modelsURL(p, a)
	if u == "" {
		res.Message = "No endpoint to test"
		return res
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		res.Message = "Bad endpoint URL"
		return res
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fastrouter/"+Version)
	switch f {
	case tr.Anthropic:
		req.Header.Set("x-api-key", a.Key())
		req.Header.Set("anthropic-version", "2023-06-01")
	case tr.Gemini:
		req.Header.Set("x-goog-api-key", a.Key())
	default:
		req.Header.Set("Authorization", "Bearer "+a.Key())
	}
	resp, err := s.client.Do(req)
	if err != nil {
		res.Message = "Network error: " + shortNetErr(err)
		return res
	}
	resp.Body.Close()
	res.Status = resp.StatusCode
	switch {
	case resp.StatusCode < 300:
		res.OK, res.Message = true, "OK"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Message = "Key rejected"
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		// Some APIs (Anthropic-compatible gateways) have no models list: the
		// host answered and did not reject the key, which is all we can tell.
		res.OK, res.Message = true, "Reachable (no models list to verify the key)"
	case resp.StatusCode == 429:
		res.OK, res.Message = true, "Key accepted, rate limited right now"
	default:
		res.Message = http.StatusText(resp.StatusCode)
	}
	return res
}

func shortNetErr(err error) string {
	m := err.Error()
	if i := strings.LastIndex(m, ": "); i >= 0 {
		m = m[i+2:]
	}
	if len(m) > 120 {
		m = m[:120]
	}
	return m
}

func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Category string `json:"category"` // oauth | free | apikey | custom | all
		Provider string `json:"provider"`
		Account  string `json:"account"`
		Mode     string `json:"mode"`  // "" = free read-only check; "chat" = one tiny "Say OK" request
		Model    string `json:"model"` // chat mode: model to use (default: a cheap chat model)
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Mode == "chat" {
		o := LiveOptions{Provider: in.Provider, Account: in.Account}
		if in.Model != "" && in.Provider != "" {
			o.Models = map[string]string{in.Provider: in.Model}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		res := s.LiveTest(ctx, o)
		passed := 0
		for _, x := range res {
			if x.OK {
				passed++
			}
		}
		adminJSON(w, http.StatusOK, map[string]any{"results": res,
			"summary": map[string]int{"total": len(res), "passed": passed, "failed": len(res) - passed}})
		return
	}
	st := s.state.Load()
	type job struct {
		p *router.Provider
		a *router.Account
	}
	var jobs []job
	for _, p := range st.List {
		if p.Plugin || (in.Provider != "" && p.ID != in.Provider) {
			continue
		}
		if in.Provider == "" && in.Category != "all" && probeCategory(p) != in.Category {
			continue
		}
		for _, a := range p.Pool.Accounts() {
			jobs = append(jobs, job{p, a})
		}
	}
	results := make([]probeResult, len(jobs))
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			results[i] = s.probeAccount(c, j.p, j.a)
		}()
	}
	wg.Wait()
	passed := 0
	for _, x := range results {
		if x.OK {
			passed++
		}
	}
	adminJSON(w, http.StatusOK, map[string]any{"results": results,
		"summary": map[string]int{"total": len(results), "passed": passed, "failed": len(results) - passed}})
}
