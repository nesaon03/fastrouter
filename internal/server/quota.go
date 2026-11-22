package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nesaon03/fastrouter/internal/catalog"
	"github.com/nesaon03/fastrouter/internal/login"
	"github.com/nesaon03/fastrouter/internal/oauth"
	"github.com/nesaon03/fastrouter/internal/store"
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
	Plan   string     `json:"plan,omitempty"`
	Quotas []quotaRow `json:"quotas"`
	// ResetCredits is Codex's available rate-limit reset credits (nil for other providers).
	ResetCredits *int   `json:"resetCredits,omitempty"`
	Error        string `json:"error,omitempty"`
	FetchedAt    string `json:"fetchedAt"`
}

type quotaAccount struct {
	ID        string        `json:"id"`
	Provider  string        `json:"provider"`
	Name      string        `json:"name"`
	Active    bool          `json:"active"`
	Supported bool          `json:"supported"`
	Note      string        `json:"note,omitempty"`
	AutoPing  *autoPingView `json:"autoPing,omitempty"` // Codex sign-in accounts only
	quotaResult
}

type quotaCred struct{ provider, key, accountID, project string }

// quotaSupported lists providers with a usage API FastRouter reads.
var quotaSupported = map[string]bool{"codex": true, "glm": true, "glm-cn": true, "deepseek": true, "opencode-go": true, "openrouter": true, "antigravity": true}

// Antigravity quota endpoints (9Router's transport.usage for the provider).
// Variables so tests can point them at a local server.
var (
	agLoadAssistURL   = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	agQuotaSummaryURL = oauth.AntigravityBase + "/v1internal:retrieveUserQuotaSummary"
	agModelsURL       = oauth.AntigravityBase + "/v1internal:fetchAvailableModels"
)

const antigravityClientVersion = "2.24.0"

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
	switch {
	case login.Handles(c.Provider):
		q.key = login.AccessKey(c)
		if c.Provider == login.Codex {
			q.accountID = login.CodexAccountID(c)
		}
	case oauth.Handles(c.Provider):
		q.key = oauth.AccessKey(c)
		q.project = oauth.Meta(c, "projectId")
	default:
		q.key = c.Secrets["apiKey"]
	}
	return q
}

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	sn := s.state.Load().Snap
	refresh := r.URL.Query().Get("refresh") == "1"
	only := r.URL.Query().Get("account")
	if os.Getenv("FASTROUTER_DEMO_QUOTA") == "1" {
		acc := demoQuota(sn)
		for i := range acc {
			for _, c := range sn.Connections {
				if c.ID == acc[i].ID {
					acc[i].AutoPing = s.autoPingFor(c)
				}
			}
		}
		adminJSON(w, http.StatusOK, map[string]any{"demo": true, "accounts": acc})
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
		qa.AutoPing = s.autoPingFor(c)
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

func (s *Server) quotaPOST(ctx context.Context, url, key string, extra map[string]string, body, v any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
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
		code, err := s.quotaGET(ctx, codexUsageURL, c.key, h, &d)
		if fail("Codex", code, err) {
			return
		}
		u := parseCodexUsage(d)
		res.Plan, res.Quotas, res.ResetCredits = u.Plan, u.Quotas, &u.ResetCredits
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
	case "antigravity":
		s.fetchAntigravityQuota(ctx, c, &res)
	default:
		res.Error = "No usage API for this provider."
	}
	if res.Error == "" && len(res.Quotas) == 0 {
		res.Error = "The usage API returned no quota data."
	}
	return res
}

// fetchAntigravityQuota reads Google Code Assist usage for an Antigravity
// account. The IDE merges two APIs: retrieveUserQuotaSummary gives the rolling
// 5h and weekly windows per model family, and fetchAvailableModels gives
// per-model fractions (the fallback when the summary is empty).
func (s *Server) fetchAntigravityQuota(ctx context.Context, c quotaCred, res *quotaResult) {
	hdr := map[string]string{
		"User-Agent":       oauth.AntigravityUserAgent,
		"X-Client-Name":    "antigravity",
		"X-Client-Version": antigravityClientVersion,
	}
	project := c.project
	var assist struct {
		Project     any `json:"cloudaicompanionProject"`
		CurrentTier struct {
			Name string `json:"name"`
		} `json:"currentTier"`
	}
	meta := map[string]any{"ideType": 9, "platform": antigravityPlatform(), "pluginType": 2}
	code, err := s.quotaPOST(ctx, agLoadAssistURL, c.key, map[string]string{"User-Agent": oauth.AntigravityUserAgent, "x-request-source": "local"}, map[string]any{"metadata": meta, "mode": 1}, &assist)
	if err == nil && code < 300 {
		if project == "" {
			project = quotaString(assist.Project)
		}
		res.Plan = assist.CurrentTier.Name
	}
	body := map[string]any{}
	if project != "" {
		body["project"] = project
	}
	var sum agSummary
	code, err = s.quotaPOST(ctx, agQuotaSummaryURL, c.key, hdr, body, &sum)
	switch {
	case code == http.StatusForbidden:
		res.Error = "Antigravity quota API access forbidden. Chat may still work."
		return
	case code == http.StatusUnauthorized:
		res.Error = "Antigravity quota API authentication expired. Chat may still work."
		return
	case err != nil:
		res.Error = "Antigravity: " + err.Error()
		return
	}
	groups := sum.Groups
	if len(groups) == 0 {
		groups = sum.QuotaSummary.Groups
	}
	res.Quotas = antigravitySummaryRows(groups)
	if len(res.Quotas) > 0 {
		return
	}
	// No summary (some accounts return an empty groups list): fall back to the
	// per-model quotaInfo that the model picker uses.
	var models agModels
	if code, err := s.quotaPOST(ctx, agModelsURL, c.key, hdr, body, &models); err == nil && code < 300 {
		res.Quotas = antigravityModelRows(models.Models)
	}
}

func quotaString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		if s, _ := x["id"].(string); s != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// antigravityPlatform is Code Assist's platform enum (9Router's metadata).
func antigravityPlatform() int {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return 2
	case "darwin/amd64":
		return 1
	case "linux/arm64":
		return 4
	case "linux/amd64":
		return 3
	}
	return 5
}

type agBucket struct {
	Window      string   `json:"window"`
	BucketID    string   `json:"bucketId"`
	DisplayName string   `json:"displayName"`
	Remaining   *float64 `json:"remainingFraction"`
	ResetTime   any      `json:"resetTime"`
	Disabled    bool     `json:"disabled"`
}

type agGroup struct {
	DisplayName string     `json:"displayName"`
	Buckets     []agBucket `json:"buckets"`
}

type agSummary struct {
	Groups       []agGroup `json:"groups"`
	QuotaSummary struct {
		Groups []agGroup `json:"groups"`
	} `json:"quotaSummary"`
}

type agModel struct {
	DisplayName string `json:"displayName"`
	IsInternal  bool   `json:"isInternal"`
	QuotaInfo   *struct {
		Remaining *float64 `json:"remainingFraction"`
		ResetTime any      `json:"resetTime"`
	} `json:"quotaInfo"`
}

type agModels struct {
	Models map[string]agModel `json:"models"`
}

func antigravityFamily(s string) string {
	n := strings.ToLower(s)
	switch {
	case strings.Contains(n, "argon"):
		return "argon"
	case strings.Contains(n, "claude"), strings.Contains(n, "gpt"):
		return "claude"
	case strings.Contains(n, "gemini"):
		return "gemini"
	}
	return ""
}

func antigravityWindow(b agBucket) string {
	w := strings.ToLower(b.Window)
	n := strings.ToLower(b.BucketID + " " + b.DisplayName)
	switch {
	case w == "weekly" || strings.Contains(n, "weekly"):
		return "weekly"
	case w == "5h" || w == "daily" || strings.Contains(n, "five hour") || strings.Contains(n, "5h") || strings.Contains(n, "daily"):
		return "session"
	}
	return ""
}

// antigravitySummaryRows turns quota groups into the same family windows the
// Antigravity IDE shows. A disabled weekly bucket means "no weekly limit".
func antigravitySummaryRows(groups []agGroup) []quotaRow {
	found := map[string]quotaRow{}
	for _, g := range groups {
		for _, b := range g.Buckets {
			fam := antigravityFamily(g.DisplayName + " " + b.BucketID + " " + b.DisplayName)
			win := antigravityWindow(b)
			if fam == "" || win == "" || (b.Disabled && win == "weekly") {
				continue
			}
			id := fam + "|" + win
			if _, ok := found[id]; ok {
				continue
			}
			rem := 0.0
			if b.Remaining != nil {
				rem = clamp(*b.Remaining, 0, 1)
			}
			if b.Disabled {
				rem = 0
			}
			label, suffix := "Gemini", "(5h)"
			switch fam {
			case "argon":
				label = "Gemini 4 Argon"
			case "claude":
				label = "Claude & GPT"
			}
			if win == "weekly" {
				suffix = "(Weekly)"
			}
			used := clamp(100*(1-rem), 0, 100)
			found[id] = quotaRow{Name: label + " " + suffix, Used: used, Limit: 100, Unit: "%", RemainingPct: 100 * rem, ResetAt: resetTime(b.ResetTime)}
		}
	}
	out := make([]quotaRow, 0, len(found))
	for _, k := range []string{"argon|session", "argon|weekly", "gemini|session", "gemini|weekly", "claude|session", "claude|weekly"} {
		if r, ok := found[k]; ok {
			out = append(out, r)
		}
	}
	return out
}

// antigravityModelRows aggregates per-model fractions into one row per family,
// using the lowest fraction (the model most likely to be exhausted).
func antigravityModelRows(models map[string]agModel) []quotaRow {
	type low struct {
		rem   float64
		reset string
	}
	best := map[string]low{}
	for id, m := range models {
		if m.IsInternal || m.QuotaInfo == nil || m.QuotaInfo.Remaining == nil {
			continue
		}
		name := strings.ToLower(id + " " + m.DisplayName)
		if strings.Contains(name, "image") {
			continue
		}
		fam := antigravityFamily(name)
		if fam == "" {
			continue
		}
		rem := clamp(*m.QuotaInfo.Remaining, 0, 1)
		if cur, ok := best[fam]; !ok || rem < cur.rem {
			best[fam] = low{rem: rem, reset: resetTime(m.QuotaInfo.ResetTime)}
		}
	}
	out := []quotaRow{}
	for _, f := range []struct{ key, name string }{{"argon", "Gemini 4 Argon"}, {"gemini", "Gemini"}, {"claude", "Claude & GPT"}} {
		if b, ok := best[f.key]; ok {
			used := clamp(100*(1-b.rem), 0, 100)
			out = append(out, quotaRow{Name: f.name + " models", Used: used, Limit: 100, Unit: "%", RemainingPct: 100 * b.rem, ResetAt: b.reset})
		}
	}
	return out
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
		"codex": {{Name: "Session (5h)", Used: 62, Limit: 100, Unit: "%", RemainingPct: 38, ResetAt: at(4*time.Hour + 59*time.Minute)}, {Name: "Weekly", Used: 31, Limit: 100, Unit: "%", RemainingPct: 69, ResetAt: at(97 * time.Hour)},
			{Name: "Code review weekly", Used: 4, Limit: 100, Unit: "%", RemainingPct: 96, ResetAt: at(97 * time.Hour)}},
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
		if c.Provider == "codex" {
			n := 1
			qa.ResetCredits = &n
		}
		if qa.Quotas == nil {
			qa.Quotas = []quotaRow{}
			qa.Note = "This provider has no usage API that FastRouter reads."
		}
		out = append(out, qa)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Supported && !out[j].Supported })
	return out
}
