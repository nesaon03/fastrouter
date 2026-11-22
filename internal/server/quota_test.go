package server

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

func f64(v float64) *float64 { return &v }

func TestAntigravitySummaryRows(t *testing.T) {
	groups := []agGroup{
		{DisplayName: "Gemini 4 Argon", Buckets: []agBucket{
			{Window: "5h", BucketID: "gemini_argon_session", Remaining: f64(0.8), ResetTime: "2026-11-22T12:00:00Z"},
			{Window: "weekly", BucketID: "gemini_argon_weekly", Remaining: f64(0.6)},
		}},
		{DisplayName: "Gemini", Buckets: []agBucket{
			{Window: "daily", BucketID: "gemini_session", Remaining: f64(0.25)},
			{Window: "weekly", BucketID: "gemini_weekly", Disabled: true},
		}},
		{DisplayName: "Claude & GPT", Buckets: []agBucket{
			{Window: "5h", BucketID: "claude_gpt_session", Disabled: true},
			{Window: "weekly", BucketID: "claude_gpt_weekly", Remaining: f64(0.9)},
		}},
	}
	rows := antigravitySummaryRows(groups)
	want := []struct {
		name string
		rem  float64
	}{
		{"Gemini 4 Argon (5h)", 80},
		{"Gemini 4 Argon (Weekly)", 60},
		{"Gemini (5h)", 25},
		{"Claude & GPT (5h)", 0},
		{"Claude & GPT (Weekly)", 90},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows (%v), want %d", len(rows), rows, len(want))
	}
	for i, w := range want {
		if rows[i].Name != w.name || rows[i].RemainingPct != w.rem || math.Abs(rows[i].Used-(100-w.rem)) > 1e-9 {
			t.Errorf("row %d = %s rem=%v used=%v, want %s rem=%v", i, rows[i].Name, rows[i].RemainingPct, rows[i].Used, w.name, w.rem)
		}
	}
	if rows[0].ResetAt != "2026-11-22T12:00:00Z" {
		t.Errorf("reset = %q", rows[0].ResetAt)
	}
}

func TestAntigravityQuotaFetch(t *testing.T) {
	modelsCalled := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q", got)
		}
		switch r.URL.Path {
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"proj-1","currentTier":{"name":"Pro"}}`)
		case "/summary":
			if got := r.Header.Get("X-Client-Name"); got != "antigravity" {
				t.Errorf("X-Client-Name = %q", got)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["project"] != "proj-1" {
				t.Errorf("project = %v", body["project"])
			}
			_, _ = io.WriteString(w, `{"groups":[{"displayName":"Gemini","buckets":[{"window":"5h","bucketId":"gemini_session","remainingFraction":0.5,"resetTime":"2026-11-22T12:00:00Z"}]}]}`)
		case "/models":
			modelsCalled = true
		}
	}))
	defer up.Close()
	setAntigravityURLs(t, up.URL+"/load", up.URL+"/summary", up.URL+"/models")

	s, _ := newTestServer(t, &store.Snapshot{})
	res := s.fetchQuota(context.Background(), quotaCred{provider: "antigravity", key: "tok"})
	if res.Error != "" {
		t.Fatalf("error = %s", res.Error)
	}
	if res.Plan != "Pro" {
		t.Errorf("plan = %q", res.Plan)
	}
	if len(res.Quotas) != 1 || res.Quotas[0].Name != "Gemini (5h)" || res.Quotas[0].RemainingPct != 50 {
		t.Fatalf("quotas = %+v", res.Quotas)
	}
	if modelsCalled {
		t.Error("models fallback ran although the summary had rows")
	}
}

func TestAntigravityQuotaModelFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/load":
			_, _ = io.WriteString(w, `{}`)
		case "/summary":
			_, _ = io.WriteString(w, `{"groups":[]}`)
		case "/models":
			_, _ = io.WriteString(w, `{"models":{
				"gemini-3.8-flash-high":{"quotaInfo":{"remainingFraction":0.9}},
				"gemini-3.8-flash-low":{"quotaInfo":{"remainingFraction":0.4,"resetTime":"2026-11-22T13:00:00Z"}},
				"claude-opus-5-5":{"quotaInfo":{"remainingFraction":0.7}},
				"internal-model":{"isInternal":true,"quotaInfo":{"remainingFraction":0}},
				"gemini-3.1-flash-image":{"quotaInfo":{"remainingFraction":0.1}}}}`)
		}
	}))
	defer up.Close()
	setAntigravityURLs(t, up.URL+"/load", up.URL+"/summary", up.URL+"/models")

	s, _ := newTestServer(t, &store.Snapshot{})
	res := s.fetchQuota(context.Background(), quotaCred{provider: "antigravity", key: "tok"})
	if res.Error != "" {
		t.Fatalf("error = %s", res.Error)
	}
	if len(res.Quotas) != 2 {
		t.Fatalf("quotas = %+v", res.Quotas)
	}
	if res.Quotas[0].Name != "Gemini models" || res.Quotas[0].RemainingPct != 40 || res.Quotas[0].ResetAt != "2026-11-22T13:00:00Z" {
		t.Errorf("gemini row = %+v", res.Quotas[0])
	}
	if res.Quotas[1].Name != "Claude & GPT models" || res.Quotas[1].RemainingPct != 70 {
		t.Errorf("claude row = %+v", res.Quotas[1])
	}
}

func TestAntigravityQuotaForbidden(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer up.Close()
	setAntigravityURLs(t, up.URL+"/load", up.URL+"/summary", up.URL+"/models")

	s, _ := newTestServer(t, &store.Snapshot{})
	res := s.fetchQuota(context.Background(), quotaCred{provider: "antigravity", key: "tok"})
	if !strings.Contains(res.Error, "forbidden") {
		t.Fatalf("error = %q", res.Error)
	}
	if len(res.Quotas) != 0 {
		t.Errorf("quotas = %+v", res.Quotas)
	}
}

func TestCredForAntigravity(t *testing.T) {
	c := store.Connection{Provider: "antigravity", Secrets: map[string]string{"accessToken": "tok"}, Meta: map[string]any{"projectId": "p1"}}
	q := credFor(c)
	if q.key != "tok" || q.project != "p1" {
		t.Fatalf("cred = %+v", q)
	}
	if !quotaSupported["antigravity"] {
		t.Fatal("antigravity is not quota-supported")
	}
}

// setAntigravityURLs points the three endpoints at a test server and restores
// them when the test ends.
func setAntigravityURLs(t *testing.T, load, summary, models string) {
	t.Helper()
	oldL, oldS, oldM := agLoadAssistURL, agQuotaSummaryURL, agModelsURL
	agLoadAssistURL, agQuotaSummaryURL, agModelsURL = load, summary, models
	t.Cleanup(func() { agLoadAssistURL, agQuotaSummaryURL, agModelsURL = oldL, oldS, oldM })
}
