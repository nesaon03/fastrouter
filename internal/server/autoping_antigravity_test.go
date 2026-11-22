package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/store"
)

// antigravityPingServer is a password-protected server with one Antigravity
// account and a mocked Code Assist quota summary.
func antigravityPingServer(t *testing.T, session, weekly float64) (*Server, *httptest.Server, map[string]string) {
	t.Helper()
	reset := time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)
	summary := fmt.Sprintf(`{"groups":[{"displayName":"Gemini","buckets":[
		{"window":"5h","bucketId":"gemini_session","remainingFraction":%g,"resetTime":%q},
		{"window":"weekly","bucketId":"gemini_weekly","remainingFraction":%g}]}]}`, session, reset, weekly)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"p","currentTier":{"name":"Pro"}}`)
		case "/summary":
			_, _ = io.WriteString(w, summary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	setAntigravityURLs(t, up.URL+"/load", up.URL+"/summary", up.URL+"/models")

	s, hs := passwordServer(t, "pw")
	hdr := adminSession(t, hs.URL, "pw")
	if err := s.mutate(func(sn *store.Snapshot) error {
		sn.Connections = append(sn.Connections, store.Connection{
			ID: "ag1", Provider: "antigravity", Name: "ag1", AuthType: "oauth",
			Active: true, Status: "ok",
			Secrets: map[string]string{"accessToken": "tok"},
			Meta:    map[string]any{"projectId": "p"},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, hs, hdr
}

func enableAntigravityPing(t *testing.T, hs *httptest.Server, hdr map[string]string, now time.Time) {
	t.Helper()
	anchor := now.Add(-5 * time.Minute).Format("15:04")
	body := fmt.Sprintf(`{"mode":"schedule","schedule":{"enabled":true,"anchor":%q,"timezone":"UTC","windowHours":5,"slotsPerDay":1}}`, anchor)
	resp, out := do(t, "PUT", hs.URL+"/api/admin/quota/ag1/autoping", body, hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("save schedule: %d %s", resp.StatusCode, out)
	}
}

func TestAntigravityAutoPingSchedule(t *testing.T) {
	now := time.Now().UTC()
	s, hs, hdr := antigravityPingServer(t, 0.5, 0.9)
	enableAntigravityPing(t, hs, hdr, now)

	var pings []string
	s.pinger.send = func(_ context.Context, id string) (bool, string) {
		pings = append(pings, id)
		return true, ""
	}
	s.autoPingTick(context.Background(), now)
	if len(pings) != 1 || pings[0] != "ag1" {
		t.Fatalf("pings after due slot: %v", pings)
	}
	s.autoPingTick(context.Background(), now.Add(time.Minute))
	if len(pings) != 1 {
		t.Fatalf("pinged the same slot twice: %v", pings)
	}

	var ag store.Connection
	for _, c := range s.state.Load().Snap.Connections {
		if c.ID == "ag1" {
			ag = c
		}
	}
	if v := s.autoPingFor(ag); v == nil || v.Mode != "schedule" || v.LastPingAt == "" {
		t.Fatalf("view: %+v", v)
	}
}

func TestAntigravityAutoPingWeeklyBlocked(t *testing.T) {
	now := time.Now().UTC()
	s, hs, hdr := antigravityPingServer(t, 0.5, 0)
	enableAntigravityPing(t, hs, hdr, now)

	var pings []string
	s.pinger.send = func(_ context.Context, id string) (bool, string) {
		pings = append(pings, id)
		return true, ""
	}
	s.autoPingTick(context.Background(), now)
	if len(pings) != 0 {
		t.Fatalf("pinged with an exhausted weekly cap: %v", pings)
	}
}

func TestAntigravityPingWindow(t *testing.T) {
	rows := []quotaRow{
		{Name: "Gemini 4 Argon (5h)", RemainingPct: 40},
		{Name: "Gemini 4 Argon (Weekly)", RemainingPct: 50},
		{Name: "Gemini (5h)", RemainingPct: 70},
		{Name: "Gemini (Weekly)", RemainingPct: 80},
		{Name: "Claude & GPT (5h)", RemainingPct: 90},
	}
	s, blocked := pingWindow("antigravity", rows)
	if blocked || s == nil || s.Name != "Gemini (5h)" {
		t.Fatalf("session = %+v blocked=%v", s, blocked)
	}
	rows[3].RemainingPct = 0
	if s, blocked := pingWindow("antigravity", rows); s != nil || !blocked {
		t.Fatalf("weekly zero: session=%+v blocked=%v", s, blocked)
	}
}
