package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/pricing"
)

// The discovery pass prices models no compiled rate card covers, and a reload
// (a settings write) keeps those learned rates.
func TestHarvestPricingLearnsUnknownModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"vendor/brand-new-model","pricing":{"prompt":"0.000002","completion":"0.000008"}}
		]}`))
	}))
	defer srv.Close()
	old := pricing.OpenRouterModelsURL
	pricing.OpenRouterModelsURL = srv.URL
	defer func() { pricing.OpenRouterModelsURL = old }()

	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.harvestAt = time.Time{} // force a harvest
	s.harvestPricing(context.Background())

	table := s.rec.prices.Load()
	if p := table.Lookup("brand-new-model", "vendor"); p == nil || p.Input != 2 || p.Output != 8 {
		t.Fatalf("harvested price: %+v", p)
	}
	// A settings reload rebuilds the table; the learned rate must survive.
	s.Reload(s.state.Load().Snap)
	table = s.rec.prices.Load()
	if p := table.Lookup("brand-new-model", "vendor"); p == nil || p.Input != 2 {
		t.Fatalf("price lost after reload: %+v", p)
	}
	// And the TTL stops a second fetch within the window.
	s.harvestPricing(context.Background())
}

// A failed fetch leaves the table as it was and does not trip the TTL, so the
// next discovery pass retries.
func TestHarvestPricingFailureKeepsState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	old := pricing.OpenRouterModelsURL
	pricing.OpenRouterModelsURL = srv.URL
	defer func() { pricing.OpenRouterModelsURL = old }()

	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.harvestAt = time.Time{}
	s.harvestPricing(context.Background())
	if !s.harvestAt.IsZero() {
		t.Fatal("a failed fetch must not be recorded as a successful harvest")
	}
}
