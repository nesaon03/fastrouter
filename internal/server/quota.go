package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/store"
)

// Quota Tracker (after 9Router's open-sse/services/usage/*). Each fetcher is
// one read-only GET to the provider's own usage/balance API with the account's
// credential. Results are cached per account for a minute.

type quotaRow struct {
	Name         string  `json:"name"`
	Used         float64 `json:"used"`
	Limit        float64 `json:"limit"`        // 0 with Unit "balance" = no limit, just a balance
	Unit         string  `json:"unit"`         // "%", "USD", "CNY", "credits"
	RemainingPct float64 `json:"remainingPct"` // -1 when unknown
	ResetAt      string  `json:"resetAt,omitempty"`
}

type quotaResult struct {
	Plan      string     `json:"plan,omitempty"`
	Quotas    []quotaRow `json:"quotas"`
	Error     string     `json:"error,omitempty"`
	FetchedAt string     `json:"fetchedAt"`
}

type quotaAccount struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	Supported bool   `json:"supported"`
	Note      string `json:"note,omitempty"`
	quotaResult
}

type quotaCred struct{ provider, key, accountID string }

// quotaSupported lists providers with a usage API FastRouter reads.
var quotaSupported = map[string]bool{"codex": true, "glm": true, "glm-cn": true, "deepseek": true, "opencode-go": true, "openrouter": true}

type quotaCache struct {
	mu sync.Mutex
	m  map[string]quotaResult
	at map[string]time.Time
}

func (c *quotaCache) get(id string, ttl time.Duration) (quotaResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.at[id]; ok && time.Since(t) < ttl {
		return c.m[id], true
	}
	return quotaResult{}, false
}

func (c *quotaCache) put(id string, r quotaResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m, c.at = map[string]quotaResult{}, map[string]time.Time{}
	}
	c.m[id], c.at[id] = r, time.Now()
}

var quotas quotaCache

func credFor(c store.Connection) quotaCred {
	q := quotaCred{provider: c.Provider}
	if login.Handles(c.Provider) {
		q.key = login.AccessKey(c)
		if c.Provider == login.Codex {
			q.accountID = login.CodexAccountID(c)
		}
	} else {
		q.key = c.Secrets["apiKey"]
	}
	return q
}

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	sn := s.state.Load().Snap
	refresh := r.URL.Query().Get("refresh") == "1"
	only := r.URL.Query().Get("account")
	if os.Getenv("FASTROUTER_DEMO_QUOTA") == "1" {
		adminJSON(w, http.StatusOK, map[string]any{"demo": true, "accounts": demoQuota(sn)})
		return
	}
	out := make([]quotaAccount, 0, len(sn.Connections))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	var mu sync.Mutex
	for _, c := range sn.Connections {
		if only != "" && c.ID != only {
			continue
		}
		qa := quotaAccount{ID: c.ID, Provider: c.Provider, Name: maskEmail(c.Name), Active: c.Active, Supported: quotaSupported[c.Provider]}
		qa.Quotas = []quotaRow{}
		if !qa.Supported {
			if p, ok := catalog.Lookup(c.Provider); ok && p.Plugin {
				qa.Note = "Quota needs this provider's sign-in plugin, which is not enabled."
			} else {
				qa.Note = "This provider has no usage API that FastRouter reads."
			}
			out = append(out, qa)
			continue
		}
		idx := len(out)
		out = append(out, qa)
		cred := credFor(c)
		id := c.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, ok := quotas.get(id, time.Minute)
			if !ok || refresh {
				sem <- struct{}{}
				ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
				res = s.fetchQuota(ctx, cred)
				cancel()
				<-sem
				quotas.put(id, res)
			}
			mu.Lock()
			out[idx].quotaResult = res
			mu.Unlock()
		}()
	}
	wg.Wait()
	adminJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) quotaGET(ctx context.Context, url, key string, extra map[string]string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fastrouter/"+Version)
	for k, x := range extra {
		req.Header.Set(k, x)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("network error: %s", shortNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func statusErr(name string, code int) string {
	switch code {
	case 401, 403:
		return name + " rejected the credential (" + strconv.Itoa(code) + ")."
	}
	return fmt.Sprintf("%s usage API error (%d).", name, code)
}

// fetchQuota reads one account's quota. It never returns credentials or raw bodies.
func (s *Server) fetchQuota(ctx context.Context, c quotaCred) (res quotaResult) {
	res.Quotas = []quotaRow{}
	res.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	if c.key == "" {
		res.Error = "No credential on this account."
		return res
	}
	fail := func(name string, code int, err error) bool {
		if err != nil {
			res.Error = name + ": " + err.Error()
			return true
		}
		if code >= 300 {
			res.Error = statusErr(name, code)
			return true
		}
		return false
	}
	switch c.provider {
	case "codex":
		var d map[string]any
		h := map[string]string{}
		if c.accountID != "" {
			h["ChatGPT-Account-ID"] = c.accountID
		}
		code, err := s.quotaGET(ctx, "https://chatgpt.com/backend-api/wham/usage", c.key, h, &d)
		if fail("Codex", code, err) {
			return
		}
		res.Plan, _ = d["plan_type"].(string)
		rl, _ := d["rate_limit"].(map[string]any)
		for _, w := range []struct{ k, name string }{{"primary_window", "Session (5h)"}, {"secondary_window", "Weekly"}} {
			if win, ok := rl[w.k].(map[string]any); ok {
				used := clamp(num(win["used_percent"]), 0, 100)
				reset := resetTime(win["reset_at"])
				if reset == "" {
					if sec := num(win["reset_after_seconds"]); sec > 0 {
						reset = time.Now().Add(time.Duration(sec) * time.Second).UTC().Format(time.RFC3339)
					}
				}
				res.Quotas = append(res.Quotas, quotaRow{Name: w.name, Used: used, Limit: 100, Unit: "%", RemainingPct: 100 - used, ResetAt: reset})
			}
		}
	case "glm", "glm-cn":
		u := "https://api.z.ai/api/monitor/usage/quota/limit"
		if c.provider == "glm-cn" {
			u = "https://open.bigmodel.cn/api/monitor/usage/quota/limit"
		}
		var d struct {
			Data struct {
				Level  string `json:"level"`
				Limits []struct {
					Type          string  `json:"type"`
					Unit          int     `json:"unit"`
					Number        int     `json:"number"`
					Percentage    float64 `json:"percentage"`
					NextResetTime float64 `json:"nextResetTime"`
				} `json:"limits"`
			} `json:"data"`
		}
		code, err := s.quotaGET(ctx, u, c.key, nil, &d)
		if fail("GLM", code, err) {
			return
		}
		if l := d.Data.Level; l != "" {
			res.Plan = strings.ToUpper(l[:1]) + strings.ToLower(l[1:])
		}
		for _, l := range d.Data.Limits {
			if l.Type != "TOKENS_LIMIT" && l.Type != "CREDIT_LIMIT" {
				continue
			}
			name := "Tokens"
			switch {
			case l.Unit == 3:
				name = fmt.Sprintf("Session (%dh)", l.Number)
			case l.Unit == 6:
				name = "Weekly (7d)"
			case l.Type != "TOKENS_LIMIT":
				name = fmt.Sprintf("Limit (%d)", l.Number)
			}
			used := clamp(l.Percentage, 0, 100)
			res.Quotas = append(res.Quotas, quotaRow{Name: name, Used: used, Limit: 100, Unit: "%", RemainingPct: 100 - used, ResetAt: resetTime(l.NextResetTime)})
		}
	case "deepseek":
		var d struct {
			BalanceInfos []struct {
				Currency       string `json:"currency"`
				TotalBalance   string `json:"total_balance"`
				GrantedBalance string `json:"granted_balance"`
			} `json:"balance_infos"`
		}
		code, err := s.quotaGET(ctx, "https://api.deepseek.com/user/balance", c.key, nil, &d)
		if fail("DeepSeek", code, err) {
			return
		}
		res.Plan = "Pay as you go"
		for _, b := range d.BalanceInfos {
			v, _ := strconv.ParseFloat(b.TotalBalance, 64)
			res.Quotas = append(res.Quotas, quotaRow{Name: "Balance (" + strings.ToUpper(b.Currency) + ")", Used: v, Unit: strings.ToUpper(b.Currency), RemainingPct: -1})
		}
	case "opencode-go":
		var d struct {
			Usage map[string]struct {
				Percent  any `json:"percent"`
				ResetsAt any `json:"resetsAt"`
			} `json:"usage"`
		}
		code, err := s.quotaGET(ctx, "https://opencode.ai/inference/go/v1/usage", c.key, nil, &d)
		if fail("OpenCode Go", code, err) {
			return
		}
		res.Plan = "OpenCode Go"
		for _, k := range []struct{ k, name string }{{"rolling", "Rolling"}, {"weekly", "Weekly"}, {"monthly", "Monthly"}} {
			if q, ok := d.Usage[k.k]; ok && q.Percent != nil {
				used := clamp(num(q.Percent), 0, 100)
				res.Quotas = append(res.Quotas, quotaRow{Name: k.name, Used: used, Limit: 100, Unit: "%", RemainingPct: 100 - used, ResetAt: resetTime(q.ResetsAt)})
			}
		}
	case "openrouter":
		var d struct {
			Data struct {
				Label          string   `json:"label"`
				Usage          float64  `json:"usage"`
				Limit          *float64 `json:"limit"`
				LimitRemaining *float64 `json:"limit_remaining"`
				IsFreeTier     bool     `json:"is_free_tier"`
			} `json:"data"`
		}
		code, err := s.quotaGET(ctx, "https://openrouter.ai/api/v1/key", c.key, nil, &d)
		if fail("OpenRouter", code, err) {
			return
		}
		res.Plan = "Credits"
		if d.Data.IsFreeTier {
			res.Plan = "Free tier"
		}
		row := quotaRow{Name: "Credits", Used: d.Data.Usage, Unit: "USD", RemainingPct: -1}
		if d.Data.Limit != nil && *d.Data.Limit > 0 {
			row.Limit = *d.Data.Limit
			row.RemainingPct = clamp(100*(*d.Data.Limit-d.Data.Usage) / *d.Data.Limit, 0, 100)
		}
		res.Quotas = append(res.Quotas, row)
	default:
		res.Error = "No usage API for this provider."
	}
	if res.Error == "" && len(res.Quotas) == 0 {
		res.Error = "The usage API returned no quota data."
	}
	return res
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Max(lo, math.Min(hi, v))
}

// resetTime accepts unix seconds, unix milliseconds or an ISO date.
func resetTime(v any) string {
	switch x := v.(type) {
	case float64:
		if x <= 0 {
			return ""
		}
		if x < 1e12 {
			x *= 1000
		}
		return time.UnixMilli(int64(x)).UTC().Format(time.RFC3339)
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return resetTime(f)
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// demoQuota is sample data for screenshots (FASTROUTER_DEMO_QUOTA=1). The UI
// labels it as demo data.
func demoQuota(sn *store.Snapshot) []quotaAccount {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	rows := map[string][]quotaRow{
		"codex":       {{Name: "Session (5h)", Used: 62, Limit: 100, Unit: "%", RemainingPct: 38, ResetAt: at(4*time.Hour + 59*time.Minute)}, {Name: "Weekly", Used: 31, Limit: 100, Unit: "%", RemainingPct: 69, ResetAt: at(97 * time.Hour)}},
		"glm":         {{Name: "Session (5h)", Used: 88, Limit: 100, Unit: "%", RemainingPct: 12, ResetAt: at(2*time.Hour + 14*time.Minute)}, {Name: "Weekly (7d)", Used: 40, Limit: 100, Unit: "%", RemainingPct: 60, ResetAt: at(130 * time.Hour)}},
		"deepseek":    {{Name: "Balance (USD)", Used: 18.42, Unit: "USD", RemainingPct: -1}},
		"opencode-go": {{Name: "Rolling", Used: 100, Limit: 100, Unit: "%", RemainingPct: 0, ResetAt: at(37 * time.Minute)}, {Name: "Weekly", Used: 55, Limit: 100, Unit: "%", RemainingPct: 45, ResetAt: at(52 * time.Hour)}, {Name: "Monthly", Used: 21, Limit: 100, Unit: "%", RemainingPct: 79, ResetAt: at(410 * time.Hour)}},
		"openrouter":  {{Name: "Credits", Used: 7.5, Limit: 10, Unit: "USD", RemainingPct: 25}},
	}
	plans := map[string]string{"codex": "plus", "glm": "Pro", "deepseek": "Pay as you go", "opencode-go": "OpenCode Go", "openrouter": "Credits"}
	out := []quotaAccount{}
	for _, c := range sn.Connections {
		qa := quotaAccount{ID: c.ID, Provider: c.Provider, Name: maskEmail(c.Name), Active: c.Active, Supported: quotaSupported[c.Provider]}
		qa.Quotas, qa.Plan, qa.FetchedAt = rows[c.Provider], plans[c.Provider], now.Format(time.RFC3339)
		if qa.Quotas == nil {
			qa.Quotas = []quotaRow{}
			qa.Note = "This provider has no usage API that FastRouter reads."
		}
		out = append(out, qa)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Supported && !out[j].Supported })
	return out
}
