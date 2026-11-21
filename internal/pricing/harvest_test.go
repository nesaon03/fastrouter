package pricing

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A harvested rate fills a gap but never replaces a compiled or user price, and
// it makes a previously cached "unknown" answer stale.
func TestLearnFillsGapsOnly(t *testing.T) {
	tb, err := New(json.RawMessage(`{"ds":{"known-model":{"input":1,"output":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p := tb.Lookup("vendor/unknown-model"); p != nil {
		t.Fatalf("expected unknown before Learn, got %+v", p)
	}
	learned := Price{Input: 3, Output: 15, Cached: 0.3}
	if !tb.Learn("vendor/unknown-model", learned) {
		t.Fatal("Learn reported no change")
	}
	if p := tb.Lookup("vendor/unknown-model"); p == nil || p.Input != 3 || p.Output != 15 {
		t.Fatalf("learned id: %+v", p)
	}
	// The base name is learned too, so the same model on another provider prices.
	if p := tb.Lookup("unknown-model", "someotherprovider"); p == nil || p.Input != 3 {
		t.Fatalf("learned base: %+v", p)
	}
	// A compiled model rate is never replaced either.
	if p := tb.Lookup("claude-opus-4-6", "x"); p != nil && p.Input > 0 {
		tb.Learn("claude-opus-4-6", Price{Input: 9999, Output: 9999})
		if p2 := tb.Lookup("claude-opus-4-6", "x"); p2 == nil || p2.Input != p.Input {
			t.Fatalf("compiled rate clobbered: %+v -> %+v", p, p2)
		}
	}
	// A provider override also wins over a harvested base-name rate: learning
	// "known-model" cannot change what provider "ds" charges for it.
	tb.Learn("known-model", Price{Input: 999, Output: 999})
	if p := tb.Lookup("known-model", "ds"); p == nil || p.Input != 1 {
		t.Fatalf("override clobbered: %+v", p)
	}
	// Learning the same rate twice is a no-op.
	if tb.Learn("vendor/unknown-model", learned) {
		t.Fatal("second Learn reported a change")
	}
}

// OpenRouter publishes per-token rates as strings; they must become per-1M
// rates, and unusable rows must be skipped.
func TestFetchOpenRouterConvertsRates(t *testing.T) {
	doc := `{"data":[
		{"id":"anthropic/claude-opus-4-6","pricing":{"prompt":"0.000005","completion":"0.000025","input_cache_read":"0.0000005","input_cache_write":"0.00000625","internal_reasoning":"0.000025"}},
		{"id":"some/free-model","pricing":{"prompt":"0","completion":"0"}},
		{"id":"","pricing":{"prompt":"0.001","completion":"0.002"}},
		{"id":"bad/rate","pricing":{"prompt":"free","completion":"lots"}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()
	old := OpenRouterModelsURL
	OpenRouterModelsURL = srv.URL
	defer func() { OpenRouterModelsURL = old }()

	got, err := FetchOpenRouter(context.Background(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got["anthropic/claude-opus-4-6"]
	if !ok {
		t.Fatalf("model missing: %v", got)
	}
	want := Price{Input: 5, Output: 25, Cached: 0.5, CacheCreation: 6.25, Reasoning: 25}
	if math.Abs(p.Input-want.Input) > 1e-9 || math.Abs(p.Output-want.Output) > 1e-9 ||
		math.Abs(p.Cached-want.Cached) > 1e-9 || math.Abs(p.CacheCreation-want.CacheCreation) > 1e-9 ||
		math.Abs(p.Reasoning-want.Reasoning) > 1e-9 {
		t.Fatalf("rates %+v, want %+v", p, want)
	}
	if _, ok := got["some/free-model"]; !ok {
		t.Fatal("a genuinely zero-rate row should be learned as free, not skipped")
	}
	if p := got["some/free-model"]; p.Input != 0 || p.Output != 0 {
		t.Fatalf("free model priced %+v", p)
	}
	if _, ok := got["bad/rate"]; ok {
		t.Fatal("unparsable rates must be skipped")
	}
}

// A non-200 answer is an error, not an empty table: the caller keeps whatever it
// learned before instead of forgetting every rate.
func TestFetchOpenRouterRejectsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	old := OpenRouterModelsURL
	OpenRouterModelsURL = srv.URL
	defer func() { OpenRouterModelsURL = old }()
	if _, err := FetchOpenRouter(context.Background(), srv.Client()); err == nil {
		t.Fatal("expected an error on http 502")
	}
}
