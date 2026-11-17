package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

func TestParseCodexUsageExtras(t *testing.T) {
	var d map[string]any
	_ = json.Unmarshal([]byte(`{"plan_type":"plus","rate_limit":{"limit_reached":false,
	  "primary_window":{"used_percent":40,"reset_at":1900000000},"secondary_window":{"used_percent":10,"reset_after_seconds":3600}},
	  "code_review_rate_limit":{"primary_window":{"used_percent":5,"reset_at":1900000000}},
	  "additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"secondary_window":{"used_percent":100}}}],
	  "rate_limit_reset_credits":{"available_count":2}}`), &d)
	u := parseCodexUsage(d)
	var names []string
	for _, q := range u.Quotas {
		names = append(names, q.Name)
	}
	if got := strings.Join(names, ","); got != "Session (5h),Weekly,Code review session (5h),Spark weekly" {
		t.Fatalf("rows: %s", got)
	}
	if u.ResetCredits != 2 || u.Plan != "plus" || u.Quotas[1].ResetAt == "" || u.Quotas[3].RemainingPct != 0 {
		t.Fatalf("%+v", u)
	}
}

// codexMock is a stand-in for chatgpt.com's /wham endpoints. No request in
// these tests reaches the real service.
type codexMock struct {
	mu        sync.Mutex
	consumed  []map[string]string
	hdr       http.Header
	consumeTo string // response for consume
}

func (m *codexMock) server(t *testing.T) *httptest.Server {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.hdr = r.Header.Clone()
		switch {
		case r.Method == "GET" && r.URL.Path == "/credits":
			io.WriteString(w, `{"available_count":1,"credits":[{"status":"redeemed","granted_at":"2026-09-01T00:00:00Z","expires_at":"2026-10-01T00:00:00Z"},
			  {"status":"available","granted_at":1792000000,"expires_at":"2026-12-09T08:00:00Z"}]}`)
		case r.Method == "POST" && r.URL.Path == "/consume":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			m.consumed = append(m.consumed, b)
			io.WriteString(w, m.consumeTo)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hs.Close)
	old := [2]string{codexResetCreditsURL, codexResetConsumeURL}
	codexResetCreditsURL, codexResetConsumeURL = hs.URL+"/credits", hs.URL+"/consume"
	t.Cleanup(func() { codexResetCreditsURL, codexResetConsumeURL = old[0], old[1] })
	return hs
}

func resetServer(t *testing.T) (*Server, *httptest.Server, map[string]string) {
	s, hs := passwordServer(t, "pw")
	if err := s.mutate(func(sn *store.Snapshot) error {
		sn.Connections = append(sn.Connections, codexConn("cx1", "ok", "tok-SECRET"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, hs, adminSession(t, hs.URL, "pw")
}

func TestCodexResetCreditsReadAndConsume(t *testing.T) {
	m := &codexMock{consumeTo: `{"code":"reset","windows_reset":2}`}
	m.server(t)
	_, hs, hdr := resetServer(t)

	resp, body := do(t, "GET", hs.URL+"/api/admin/quota/cx1/codex-reset-credits", "", hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("read: %d %s", resp.StatusCode, body)
	}
	var rc resetCredits
	_ = json.Unmarshal([]byte(body), &rc)
	if rc.AvailableCount != 1 || len(rc.Credits) != 2 || rc.Credits[0].Status != "redeemed" || rc.Credits[1].ExpiresAt != "2026-12-09T08:00:00Z" || rc.Credits[1].GrantedAt == "" {
		t.Fatalf("credits: %s", body)
	}
	if m.hdr.Get("Authorization") != "Bearer tok-SECRET" || m.hdr.Get("ChatGPT-Account-ID") != "ACC-1" || m.hdr.Get("OpenAI-Beta") != "codex-1" {
		t.Fatalf("read headers: %v", m.hdr)
	}
	if strings.Contains(body, "tok-SECRET") {
		t.Fatal("token leaked")
	}

	// A client cannot choose the redeem id; the server makes a fresh UUID.
	resp, body = do(t, "POST", hs.URL+"/api/admin/quota/cx1/codex-reset-credits", `{}`, hdr)
	if resp.StatusCode != 200 || !strings.Contains(body, `"reset":true`) || !strings.Contains(body, `"windowsReset":2`) {
		t.Fatalf("consume: %d %s", resp.StatusCode, body)
	}
	resp, _ = do(t, "POST", hs.URL+"/api/admin/quota/cx1/codex-reset-credits", `{}`, hdr)
	if resp.StatusCode != 200 || len(m.consumed) != 2 {
		t.Fatalf("second consume: %d", resp.StatusCode)
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuid.MatchString(m.consumed[0]["redeem_request_id"]) || m.consumed[0]["redeem_request_id"] == m.consumed[1]["redeem_request_id"] {
		t.Fatalf("redeem ids: %v", m.consumed)
	}

	m.consumeTo = `{"code":"no_credit"}`
	resp, body = do(t, "POST", hs.URL+"/api/admin/quota/cx1/codex-reset-credits", `{}`, hdr)
	if resp.StatusCode != 409 || !strings.Contains(body, "No Codex reset credits") {
		t.Fatalf("no credit: %d %s", resp.StatusCode, body)
	}
	// Needs a dashboard session with CSRF, like every write.
	resp, _ = do(t, "POST", hs.URL+"/api/admin/quota/cx1/codex-reset-credits", `{}`, map[string]string{"Cookie": hdr["Cookie"], "Content-Type": "application/json"})
	if resp.StatusCode != 403 {
		t.Fatalf("consume without CSRF: %d", resp.StatusCode)
	}
	resp, _ = do(t, "POST", hs.URL+"/api/admin/quota/d1/codex-reset-credits", `{}`, hdr)
	if resp.StatusCode != 400 {
		t.Fatalf("non-codex account: %d", resp.StatusCode)
	}
	if len(m.consumed) != 3 {
		t.Fatalf("consume calls: %d", len(m.consumed))
	}
}

func TestPingScheduleSlots(t *testing.T) {
	sc := normalizeSchedule(&pingSchedule{Enabled: true, Anchor: "09:30", Timezone: "Asia/Singapore", WindowHours: 5, SlotsPerDay: 9})
	if sc.SlotsPerDay != 4 {
		t.Fatalf("slots capped to 24/5: %d", sc.SlotsPerDay)
	}
	sc.SlotsPerDay = 3
	loc, _ := time.LoadLocation("Asia/Singapore")
	now := time.Date(2026, 11, 17, 14, 40, 0, 0, loc)
	d, ok := dueSlot(sc, now)
	if !ok || d.at.Format("15:04") != "14:30" || d.key != "2026-11-17#1" {
		t.Fatalf("due: %v %v", d, ok)
	}
	if _, ok := dueSlot(sc, now.Add(40*time.Minute)); ok {
		t.Fatal("a slot past the 30 min grace must not fire")
	}
	n, _ := nextSlot(sc, now)
	if n.at.Format("15:04") != "19:30" {
		t.Fatalf("next: %v", n.at)
	}
	n, _ = nextSlot(sc, time.Date(2026, 11, 17, 20, 0, 0, 0, loc))
	if n.at.Format("01-02 15:04") != "11-18 09:30" {
		t.Fatalf("next day: %v", n.at)
	}
}

func TestCodexAutoPingOnResetSlide(t *testing.T) {
	var resetAt time.Time
	var mu sync.Mutex
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"plan_type": "plus", "rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 0, "reset_at": resetAt.Unix()},
			"secondary_window": map[string]any{"used_percent": 20, "reset_at": resetAt.Add(72 * time.Hour).Unix()}}})
	}))
	defer usage.Close()
	old := codexUsageURL
	codexUsageURL = usage.URL
	defer func() { codexUsageURL = old }()

	s, hs, hdr := resetServer(t)
	var pings []string
	s.pinger.send = func(_ context.Context, id string) (bool, string) { pings = append(pings, id); return true, "" }
	resp, body := do(t, "PUT", hs.URL+"/api/admin/quota/cx1/autoping", `{"mode":"reset"}`, hdr)
	if resp.StatusCode != 200 || !strings.Contains(body, `"mode":"reset"`) {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	now := time.Now()
	resetAt = now.Add(5 * time.Hour)
	s.autoPingTick(context.Background(), now) // first look: remember the reset time
	mu.Lock()
	resetAt = resetAt.Add(time.Minute) // idle window: the reset slides with the clock
	mu.Unlock()
	s.autoPingTick(context.Background(), now.Add(time.Minute))
	if len(pings) != 1 || pings[0] != "cx1" {
		t.Fatalf("pings after slide: %v", pings)
	}
	mu.Lock()
	resetAt = resetAt.Add(time.Minute)
	mu.Unlock()
	s.autoPingTick(context.Background(), now.Add(2*time.Minute))
	if len(pings) != 1 {
		t.Fatalf("pinged again within 10 min: %v", pings)
	}
	resp, _ = do(t, "PUT", hs.URL+"/api/admin/quota/cx1/autoping", `{"mode":"off"}`, hdr)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if v := s.autoPingFor(s.state.Load().Snap.Connections[1]); v.Mode != "off" || v.LastPingAt == "" {
		t.Fatalf("view: %+v", v)
	}
}
