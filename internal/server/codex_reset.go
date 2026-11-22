package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // schedule time zones on Windows

	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/store"
)

// Codex extras on the Quota Tracker, after 9Router's
// open-sse/services/usage/codex.js and shared/services/quotaAutoPing.js:
//   - rate-limit reset credits: read the count and expiry, spend one
//   - auto-ping: start the next 5h window with a tiny request, on reset or on
//     a fixed daily schedule
//
// Antigravity shares the auto-ping scheduler (it has no reset credits); its
// 5h rows come from the Code Assist quota summary.
//
// The URLs are variables so tests point them at a mock; nothing here runs
// unless the user presses a button or turns auto-ping on.
var (
	codexUsageURL         = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL  = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexResetConsumeURL  = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	codexPingModel        = "gpt-5.6-terra" // 9Router's lightweight auto-ping model
	antigravityPingModel  = "gemini-3.8-flash-low"
	codexAutoPingInterval = time.Minute
)

type codexUsage struct {
	Plan         string
	Quotas       []quotaRow
	ResetCredits int
	LimitReached bool
}

// parseCodexUsage reads /wham/usage: the main 5h + weekly windows plus the
// code-review and Spark limits when the plan has them.
func parseCodexUsage(d map[string]any) codexUsage {
	u := codexUsage{Quotas: []quotaRow{}}
	u.Plan, _ = d["plan_type"].(string)
	if u.Plan == "" {
		if sm, ok := d["summary"].(map[string]any); ok {
			u.Plan, _ = sm["plan"].(string)
		}
	}
	if rc, ok := d["rate_limit_reset_credits"].(map[string]any); ok {
		u.ResetCredits = int(clamp(num(rc["available_count"]), 0, 1e6))
	}
	byID, _ := d["rate_limits_by_limit_id"].(map[string]any)
	extra, _ := d["additional_rate_limits"].([]any)
	find := func(direct []string, ids []string, match func(string) bool) map[string]any {
		for _, k := range direct {
			if m, ok := d[k].(map[string]any); ok {
				return m
			}
		}
		for _, k := range ids {
			if m, ok := byID[k].(map[string]any); ok {
				return m
			}
		}
		for _, e := range extra {
			m, _ := e.(map[string]any)
			id := ""
			for _, k := range []string{"limit_name", "metered_feature", "id"} {
				if v, _ := m[k].(string); v != "" {
					id = strings.ToLower(v)
					break
				}
			}
			if id != "" && match(id) {
				return m
			}
		}
		return nil
	}
	main := find([]string{"rate_limit", "rate_limits"}, []string{"codex"}, func(string) bool { return false })
	review := find([]string{"code_review_rate_limit", "review_rate_limit"}, []string{"code_review", "codex_review", "review"}, func(id string) bool { return strings.Contains(id, "review") })
	spark := find([]string{"spark_rate_limit", "gpt_5_3_codex_spark_rate_limit"}, []string{"gpt-5.3-codex-spark", "gpt_5_3_codex_spark", "spark"}, func(id string) bool { return strings.Contains(id, "spark") })
	add := func(snap map[string]any, prefix string) {
		if snap == nil {
			return
		}
		body := snap
		if rl, ok := snap["rate_limit"].(map[string]any); ok {
			body = rl
		}
		if prefix == "" {
			u.LimitReached, _ = body["limit_reached"].(bool)
		}
		pick := func(keys ...string) map[string]any {
			for _, k := range keys {
				if m, ok := body[k].(map[string]any); ok {
					return m
				}
				if m, ok := snap[k].(map[string]any); ok {
					return m
				}
			}
			return nil
		}
		for _, w := range []struct {
			win  map[string]any
			name string
		}{{pick("primary_window", "primary"), "Session (5h)"}, {pick("secondary_window", "secondary"), "Weekly"}} {
			if w.win == nil {
				continue
			}
			used := clamp(num(firstOf(w.win, "used_percent", "percent_used")), 0, 100)
			reset := resetTime(firstOf(w.win, "reset_at", "resets_at", "resetAt"))
			if reset == "" {
				if sec := num(w.win["reset_after_seconds"]); sec > 0 {
					reset = time.Now().Add(time.Duration(sec) * time.Second).UTC().Format(time.RFC3339)
				}
			}
			name := w.name
			if prefix != "" {
				name = prefix + " " + strings.ToLower(w.name)
			}
			u.Quotas = append(u.Quotas, quotaRow{Name: name, Used: used, Limit: 100, Unit: "%", RemainingPct: 100 - used, ResetAt: reset})
		}
	}
	add(main, "")
	add(review, "Code review")
	add(spark, "Spark")
	return u
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// codexConn returns a Codex sign-in account with a usable access token,
// refreshing tokens that are past their expiry first.
func (s *Server) codexConn(ctx context.Context, id string) (store.Connection, error) {
	find := func() (store.Connection, bool) {
		for _, c := range s.state.Load().Snap.Connections {
			if c.ID == id {
				return c, true
			}
		}
		return store.Connection{}, false
	}
	c, ok := find()
	if !ok {
		return c, errNotFound
	}
	if c.Provider != "codex" {
		return c, errors.New("reset credits are only available for Codex accounts")
	}
	if login.Expired(c, time.Now()) && s.st != nil {
		s.RefreshDue(ctx)
		if c2, ok := find(); ok {
			c = c2
		}
	}
	if login.AccessKey(c) == "" {
		return c, errors.New("this Codex account has no access token; sign in again")
	}
	return c, nil
}

type resetCredit struct {
	Status    string `json:"status"`
	GrantedAt string `json:"grantedAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type resetCredits struct {
	AvailableCount int           `json:"availableCount"`
	Credits        []resetCredit `json:"credits"`
	Demo           bool          `json:"demo,omitempty"`
}

func codexHeaders(c store.Connection) map[string]string {
	h := map[string]string{"OpenAI-Beta": "codex-1", "originator": "codex_cli_rs"}
	if id := login.CodexAccountID(c); id != "" {
		h["ChatGPT-Account-ID"] = id
	}
	return h
}

// GetResetCredits reads GET /wham/rate-limit-reset-credits (read-only).
func (s *Server) getResetCredits(ctx context.Context, c store.Connection) (resetCredits, error) {
	var d struct {
		AvailableCount any `json:"available_count"`
		AvailableAlt   any `json:"availableCount"`
		Credits        []map[string]any
		Message        any `json:"message"`
		Error          any `json:"error"`
		Detail         any `json:"detail"`
	}
	out := resetCredits{Credits: []resetCredit{}}
	code, err := s.quotaGET(ctx, codexResetCreditsURL, login.AccessKey(c), codexHeaders(c), &d)
	if err != nil {
		return out, err
	}
	if code >= 300 {
		return out, errors.New(statusErr("Codex reset credits", code))
	}
	out.AvailableCount = int(clamp(num(firstNonNil(d.AvailableCount, d.AvailableAlt)), 0, 1e6))
	for _, cr := range d.Credits {
		st, _ := cr["status"].(string)
		if st == "" {
			st = "unknown"
		}
		out.Credits = append(out.Credits, resetCredit{Status: st, GrantedAt: resetTime(firstOf(cr, "granted_at", "grantedAt")),
			ExpiresAt: resetTime(firstOf(cr, "expires_at", "expiresAt"))})
	}
	sort.SliceStable(out.Credits, func(i, j int) bool {
		a, b := out.Credits[i].ExpiresAt, out.Credits[j].ExpiresAt
		return a != "" && (b == "" || a < b)
	})
	return out, nil
}

// CodexCreditRow is one account in CodexCreditsReport.
type CodexCreditRow struct {
	Account string
	Credits resetCredits
	Error   string
}

// CodexCreditsReport reads every Codex account's reset credits (read-only),
// for the `fastrouter codex-credits` command.
func (s *Server) CodexCreditsReport(ctx context.Context) []CodexCreditRow {
	var out []CodexCreditRow
	for _, c := range s.state.Load().Snap.Connections {
		if c.Provider != "codex" {
			continue
		}
		row := CodexCreditRow{Account: maskEmail(c.Name)}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		// No token refresh here: the running server owns the rotating
		// refresh token, and a second refresher would invalidate its copy.
		if login.AccessKey(c) == "" || login.Expired(c, time.Now()) {
			row.Error = "access token missing or expired; the running FastRouter refreshes it"
		} else if rc, err := s.getResetCredits(cctx, c); err != nil {
			row.Error = err.Error()
		} else {
			row.Credits = rc
		}
		cancel()
		out = append(out, row)
	}
	return out
}

func firstNonNil(v ...any) any {
	for _, x := range v {
		if x != nil {
			return x
		}
	}
	return nil
}

type consumeResult struct {
	OK           bool   `json:"reset"`
	Code         string `json:"code,omitempty"`
	WindowsReset int    `json:"windowsReset"`
	Message      string `json:"message,omitempty"`
	status       int
	noCredit     bool
}

// consumeResetCredit spends one credit (irreversible). The redeem id is made
// here, never by the client, so a replayed request cannot spend twice.
func (s *Server) consumeResetCredit(ctx context.Context, c store.Connection) (consumeResult, error) {
	var r consumeResult
	id := newRedeemID()
	body, _ := json.Marshal(map[string]string{"redeem_request_id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResetConsumeURL, bytes.NewReader(body))
	if err != nil {
		return r, err
	}
	req.Header.Set("Authorization", "Bearer "+login.AccessKey(c))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fastrouter/"+Version)
	resp, err := s.client.Do(req)
	if err != nil {
		return r, fmt.Errorf("failed to consume Codex reset credit: %s", shortNetErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	r.status = resp.StatusCode
	r.Code, _ = d["code"].(string)
	r.WindowsReset = int(num(d["windows_reset"]))
	r.Message, _ = d["message"].(string)
	if r.Message == "" {
		if m, ok := d["detail"].(string); ok {
			r.Message = m
		}
	}
	ok := resp.StatusCode < 300
	r.OK = ok && (r.Code == "reset" || r.WindowsReset > 0)
	r.noCredit = ok && r.Code == "no_credit"
	return r, nil
}

func newRedeemID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func demoMode() bool { return os.Getenv("FASTROUTER_DEMO_QUOTA") == "1" }

func (s *Server) handleResetCredits(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if demoMode() {
		now := time.Now().UTC()
		adminJSON(w, http.StatusOK, resetCredits{AvailableCount: 1, Demo: true, Credits: []resetCredit{
			{Status: "available", GrantedAt: now.Add(-9 * 24 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(21*24*time.Hour + 5*time.Hour).Format(time.RFC3339)},
			{Status: "redeemed", GrantedAt: now.Add(-40 * 24 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)}}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := s.codexConn(ctx, id)
	if err != nil {
		adminErr(w, codeFor(err), err.Error())
		return
	}
	out, err := s.getResetCredits(ctx, c)
	if err != nil {
		adminErr(w, http.StatusBadGateway, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, out)
}

func (s *Server) handleResetConsume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if demoMode() {
		quotas.drop(id)
		adminJSON(w, http.StatusOK, map[string]any{"reset": true, "code": "reset", "windowsReset": 1, "demo": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	c, err := s.codexConn(ctx, id)
	if err != nil {
		adminErr(w, codeFor(err), err.Error())
		return
	}
	res, err := s.consumeResetCredit(ctx, c)
	quotas.drop(id) // the next quota read shows the new state either way
	if err != nil {
		adminErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("codex reset credit used on %s: code=%s windows_reset=%d", maskEmail(c.Name), res.Code, res.WindowsReset)
	switch {
	case res.OK:
		adminJSON(w, http.StatusOK, res)
	case res.noCredit:
		adminJSON(w, http.StatusConflict, map[string]any{"error": "No Codex reset credits available.", "code": "no_credit"})
	default:
		msg := res.Message
		if msg == "" {
			msg = "Codex reset credit consume returned an unexpected response (" + strconv.Itoa(res.status) + ")."
		}
		st := http.StatusBadGateway
		if res.status >= 400 && res.status < 500 {
			st = res.status
		}
		adminJSON(w, st, map[string]any{"error": msg, "code": firstNonEmpty(res.Code, "unknown_response")})
	}
}

func codeFor(err error) int {
	if errors.Is(err, errNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func (c *quotaCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.at, id)
}

/* ---------------------------------------------------------------- auto-ping */

// pingSchedule fires at fixed wall-clock slots: anchor, anchor+window, ...
type pingSchedule struct {
	Enabled     bool    `json:"enabled"`
	Anchor      string  `json:"anchor"`
	Timezone    string  `json:"timezone"`
	WindowHours float64 `json:"windowHours"`
	SlotsPerDay int     `json:"slotsPerDay"`
}

type autoPingCfg struct {
	Connections map[string]bool          `json:"connections"`
	Schedules   map[string]*pingSchedule `json:"schedules"`
}

type autoPingState struct {
	LastPingAt   string `json:"lastPingAt,omitempty"`
	LastResetKey string `json:"lastResetKey,omitempty"`
	LastSlotKey  string `json:"lastSlotKey,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	LastErrorAt  string `json:"lastErrorAt,omitempty"`
}

type autoPingView struct {
	Mode       string        `json:"mode"` // off | reset | schedule
	Schedule   *pingSchedule `json:"schedule,omitempty"`
	LastPingAt string        `json:"lastPingAt,omitempty"`
	LastError  string        `json:"lastError,omitempty"`
	NextSlotAt string        `json:"nextSlotAt,omitempty"`
}

const (
	autoPingKey      = "frCodexAutoPing"
	autoPingStateKey = "frCodexAutoPingState"
)

func (s *Server) autoPingConfig() (autoPingCfg, map[string]autoPingState) {
	sn := s.state.Load().Snap
	var cfg autoPingCfg
	_ = json.Unmarshal(sn.Settings[autoPingKey], &cfg)
	st := map[string]autoPingState{}
	_ = json.Unmarshal(sn.Settings[autoPingStateKey], &st)
	return cfg, st
}

func (s *Server) autoPingFor(c store.Connection) *autoPingView {
	if c.Provider != "codex" && c.Provider != "antigravity" {
		return nil
	}
	cfg, st := s.autoPingConfig()
	v := &autoPingView{Mode: "off", LastPingAt: st[c.ID].LastPingAt, LastError: st[c.ID].LastError}
	if sc := normalizeSchedule(cfg.Schedules[c.ID]); sc != nil {
		v.Mode, v.Schedule = "schedule", sc
		if n, ok := nextSlot(sc, time.Now()); ok {
			v.NextSlotAt = n.at.UTC().Format(time.RFC3339)
		}
	} else if cfg.Connections[c.ID] {
		v.Mode = "reset"
	}
	return v
}

func parseAnchor(a string) (h, m int, ok bool) {
	hh, mm, found := strings.Cut(strings.TrimSpace(a), ":")
	if !found {
		return 0, 0, false
	}
	h, e1 := strconv.Atoi(hh)
	m, e2 := strconv.Atoi(mm)
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(mm) != 2 {
		return 0, 0, false
	}
	return h, m, true
}

func maxSlots(windowHours float64) int {
	n := int(24 / windowHours)
	if n < 1 {
		n = 1
	}
	return n
}

// normalizeSchedule returns a defaulted schedule, or nil when off/invalid.
func normalizeSchedule(p *pingSchedule) *pingSchedule {
	if p == nil || !p.Enabled {
		return nil
	}
	sc := *p
	h, m, ok := parseAnchor(sc.Anchor)
	if !ok {
		h, m = 9, 30
	}
	sc.Anchor = fmt.Sprintf("%02d:%02d", h, m)
	if _, err := time.LoadLocation(sc.Timezone); err != nil || sc.Timezone == "" {
		sc.Timezone = "Local"
	}
	if sc.WindowHours < 0.5 || sc.WindowHours > 24 {
		sc.WindowHours = 5
	}
	if sc.SlotsPerDay < 1 {
		sc.SlotsPerDay = 3
	}
	if mx := maxSlots(sc.WindowHours); sc.SlotsPerDay > mx {
		sc.SlotsPerDay = mx
	}
	return &sc
}

type slot struct {
	key string
	at  time.Time
}

func scheduleSlots(sc *pingSchedule, now time.Time) []slot {
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		loc = time.Local
	}
	h, m, _ := parseAnchor(sc.Anchor)
	var out []slot
	for d := -1; d <= 1; d++ {
		day := now.In(loc).AddDate(0, 0, d)
		first := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
		for i := 0; i < sc.SlotsPerDay; i++ {
			out = append(out, slot{key: first.Format("2006-01-02") + "#" + strconv.Itoa(i),
				at: first.Add(time.Duration(float64(i) * sc.WindowHours * float64(time.Hour)))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

const slotGrace = 30 * time.Minute

// dueSlot is the latest slot that has passed within the grace window.
func dueSlot(sc *pingSchedule, now time.Time) (slot, bool) {
	var best slot
	found := false
	for _, x := range scheduleSlots(sc, now) {
		if !x.at.After(now) && now.Sub(x.at) <= slotGrace {
			best, found = x, true
		}
	}
	return best, found
}

func nextSlot(sc *pingSchedule, now time.Time) (slot, bool) {
	for _, x := range scheduleSlots(sc, now) {
		if x.at.After(now) {
			return x, true
		}
	}
	return slot{}, false
}

func (s *Server) handleAutoPingSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Mode     string        `json:"mode"` // off | reset | schedule
		Schedule *pingSchedule `json:"schedule"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	var conn *store.Connection
	for _, c := range s.state.Load().Snap.Connections {
		if c.ID == id {
			c := c
			conn = &c
		}
	}
	if conn == nil || (conn.Provider != "codex" && conn.Provider != "antigravity") {
		adminErr(w, http.StatusNotFound, "account not found")
		return
	}
	switch in.Mode {
	case "off", "reset":
	case "schedule":
		if in.Schedule == nil {
			adminErr(w, http.StatusBadRequest, "schedule is required")
			return
		}
		if _, _, ok := parseAnchor(in.Schedule.Anchor); !ok {
			adminErr(w, http.StatusBadRequest, "anchor must be HH:MM")
			return
		}
		if in.Schedule.WindowHours < 0.5 || in.Schedule.WindowHours > 24 {
			adminErr(w, http.StatusBadRequest, "window must be 0.5 to 24 hours")
			return
		}
		if mx := maxSlots(in.Schedule.WindowHours); in.Schedule.SlotsPerDay < 1 || in.Schedule.SlotsPerDay > mx {
			adminErr(w, http.StatusBadRequest, fmt.Sprintf("pings per day must be 1 to %d", mx))
			return
		}
		if in.Schedule.Timezone != "" {
			if _, err := time.LoadLocation(in.Schedule.Timezone); err != nil {
				adminErr(w, http.StatusBadRequest, "unknown time zone")
				return
			}
		}
	default:
		adminErr(w, http.StatusBadRequest, "mode must be off, reset or schedule")
		return
	}
	err := s.mutate(func(sn *store.Snapshot) error {
		var cfg autoPingCfg
		_ = json.Unmarshal(sn.Settings[autoPingKey], &cfg)
		if cfg.Connections == nil {
			cfg.Connections = map[string]bool{}
		}
		if cfg.Schedules == nil {
			cfg.Schedules = map[string]*pingSchedule{}
		}
		delete(cfg.Connections, id)
		delete(cfg.Schedules, id)
		switch in.Mode {
		case "reset":
			cfg.Connections[id] = true
		case "schedule":
			sc := *in.Schedule
			sc.Enabled = true
			cfg.Schedules[id] = &sc
		}
		b, _ := json.Marshal(cfg)
		sn.Settings[autoPingKey] = b
		return nil
	})
	if err != nil {
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, s.autoPingFor(*conn))
}

// autoPinger keeps per-account memory between ticks (9Router's resetCache /
// failureCache).
type autoPinger struct {
	mu        sync.Mutex
	running   bool
	lastReset map[string]time.Time
	failedAt  map[string]time.Time
	send      func(ctx context.Context, id string) (bool, string) // tests
}

const (
	pingFailureCooldown = 15 * time.Minute
	pingMinInterval     = 10 * time.Minute
	resetDrift          = 30 * time.Second
)

// RunAutoPing checks opted-in Codex and Antigravity accounts every minute
// until ctx ends.
func (s *Server) RunAutoPing(ctx context.Context) {
	t := time.NewTicker(codexAutoPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.autoPingTick(ctx, time.Now())
		}
	}
}

func (s *Server) autoPingTick(ctx context.Context, now time.Time) {
	ap := &s.pinger
	ap.mu.Lock()
	if ap.running {
		ap.mu.Unlock()
		return
	}
	ap.running = true
	if ap.lastReset == nil {
		ap.lastReset, ap.failedAt = map[string]time.Time{}, map[string]time.Time{}
	}
	ap.mu.Unlock()
	defer func() { ap.mu.Lock(); ap.running = false; ap.mu.Unlock() }()

	cfg, states := s.autoPingConfig()
	for _, c := range s.state.Load().Snap.Connections {
		if !c.Active || (c.Provider != "codex" && c.Provider != "antigravity") {
			continue
		}
		sc := normalizeSchedule(cfg.Schedules[c.ID])
		reactive := cfg.Connections[c.ID] && sc == nil
		if sc == nil && !reactive {
			continue
		}
		s.autoPingOne(ctx, c, sc, reactive, states[c.ID], now)
	}
}

func (s *Server) autoPingOne(ctx context.Context, c store.Connection, sc *pingSchedule, reactive bool, st autoPingState, now time.Time) {
	ap := &s.pinger
	ap.mu.Lock()
	failed := ap.failedAt[c.ID]
	prevReset, hadPrev := ap.lastReset[c.ID]
	ap.mu.Unlock()
	if !failed.IsZero() && now.Sub(failed) < pingFailureCooldown {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	res := s.fetchQuota(cctx, credFor(c))
	cancel()
	if res.Error != "" {
		ap.mu.Lock()
		ap.failedAt[c.ID] = now
		ap.mu.Unlock()
		return
	}
	quotas.put(c.ID, res)
	session, blocked := pingWindow(c.Provider, res.Quotas)
	if blocked || session == nil || session.RemainingPct <= 0 {
		return // nothing to start: the window or its weekly cap is used up
	}
	resetAt, _ := time.Parse(time.RFC3339, session.ResetAt)
	if !resetAt.IsZero() {
		ap.mu.Lock()
		ap.lastReset[c.ID] = resetAt
		ap.mu.Unlock()
	}
	lastPing, _ := time.Parse(time.RFC3339, st.LastPingAt)
	recent := !lastPing.IsZero() && now.Sub(lastPing) < pingMinInterval
	var slotKey, resetKey string
	if sc != nil {
		due, ok := dueSlot(sc, now)
		if !ok || due.key == st.LastSlotKey || recent {
			return
		}
		slotKey = due.key
	} else {
		// Codex: an idle 5h window's reset time slides forward with the
		// clock; a slide means no window is running, so start one.
		if resetAt.IsZero() || !hadPrev || resetAt.Sub(prevReset) < resetDrift || recent {
			return
		}
		resetKey = resetAt.Truncate(time.Minute).UTC().Format(time.RFC3339)
		if resetKey == st.LastResetKey {
			return
		}
	}
	pctx, pcancel := context.WithTimeout(ctx, 2*time.Minute)
	ok, msg := s.pingAccount(pctx, c)
	pcancel()
	if !ok {
		ap.mu.Lock()
		ap.failedAt[c.ID] = now
		ap.mu.Unlock()
		log.Printf("[auto-ping] %s %s: %s", c.Provider, maskEmail(c.Name), msg)
		s.saveAutoPingState(c.ID, func(x *autoPingState) { x.LastError, x.LastErrorAt = msg, now.UTC().Format(time.RFC3339) })
		return
	}
	ap.mu.Lock()
	delete(ap.failedAt, c.ID)
	ap.mu.Unlock()
	quotas.drop(c.ID)
	log.Printf("[auto-ping] %s %s: ping sent (%s)", c.Provider, maskEmail(c.Name), firstNonEmpty(slotKey, "reset "+resetKey))
	s.saveAutoPingState(c.ID, func(x *autoPingState) {
		x.LastPingAt, x.LastError, x.LastErrorAt = now.UTC().Format(time.RFC3339), "", ""
		if slotKey != "" {
			x.LastSlotKey = slotKey
		}
		if resetKey != "" {
			x.LastResetKey = resetKey
		}
	})
}

// pingWindow returns the rolling-window row a ping should start, and whether
// an exhausted limit makes a ping pointless.
func pingWindow(provider string, rows []quotaRow) (*quotaRow, bool) {
	if provider == "antigravity" {
		return antigravityPingWindow(rows)
	}
	var session *quotaRow
	for i := range rows {
		q := &rows[i]
		if q.Name == "Session (5h)" {
			session = q
		} else if q.RemainingPct == 0 && !strings.Contains(strings.ToLower(q.Name), "review") && !strings.Contains(strings.ToLower(q.Name), "spark") {
			return nil, true
		}
	}
	return session, false
}

// antigravityPingWindow picks the Gemini family window, because the ping model
// is a Gemini flash. A used-up Gemini weekly cap blocks the ping; if the plan
// has no plain Gemini window, Argon is the fallback.
func antigravityPingWindow(rows []quotaRow) (*quotaRow, bool) {
	session := map[string]*quotaRow{}
	weekly := map[string]*quotaRow{}
	for i := range rows {
		q := &rows[i]
		switch {
		case strings.HasSuffix(q.Name, " (Weekly)"):
			weekly[strings.TrimSuffix(q.Name, " (Weekly)")] = q
		case strings.HasSuffix(q.Name, " (5h)"):
			session[strings.TrimSuffix(q.Name, " (5h)")] = q
		}
	}
	for _, fam := range []string{"Gemini", "Gemini 4 Argon"} {
		s := session[fam]
		if s == nil {
			continue
		}
		if w := weekly[fam]; w != nil && w.RemainingPct <= 0 {
			return nil, true
		}
		if s.RemainingPct <= 0 {
			return nil, false
		}
		return s, false
	}
	return nil, false
}

// pingAccount sends one tiny request through the normal pipeline to this
// account only (the Providers page's "Test" path).
func (s *Server) pingAccount(ctx context.Context, c store.Connection) (bool, string) {
	if s.pinger.send != nil {
		return s.pinger.send(ctx, c.ID)
	}
	model := codexPingModel
	if c.Provider == "antigravity" {
		model = antigravityPingModel
	}
	out := s.LiveTest(ctx, LiveOptions{Provider: c.Provider, Account: c.ID, Models: map[string]string{c.Provider: model}})
	if len(out) == 1 && out[0].OK {
		return true, ""
	}
	if len(out) == 1 {
		return false, out[0].Class + ": " + out[0].Message
	}
	return false, "ping failed"
}

func (s *Server) saveAutoPingState(id string, f func(*autoPingState)) {
	err := s.mutate(func(sn *store.Snapshot) error {
		m := map[string]autoPingState{}
		_ = json.Unmarshal(sn.Settings[autoPingStateKey], &m)
		x := m[id]
		f(&x)
		m[id] = x
		b, _ := json.Marshal(m)
		sn.Settings[autoPingStateKey] = b
		return nil
	})
	if err != nil {
		log.Printf("[auto-ping] save state: %v", err)
	}
}
