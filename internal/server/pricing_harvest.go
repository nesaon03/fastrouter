package server

import (
	"context"
	"log"
	"time"

	"github.com/nesaon03/fastrouter/internal/pricing"
)

const (
	// harvestedPricingTTL bounds how often the public rate list is fetched:
	// once a day is plenty (vendors change list prices rarely) and keeps
	// discovery cheap.
	harvestedPricingTTL = 20 * time.Hour
	// harvestedPricingTimeout caps one fetch so a stalled connection can never
	// hold the discovery pass (or the harvest lock) open.
	harvestedPricingTimeout = 30 * time.Second
)

// harvestPricing fills rate cards for models no compiled price covers, using
// OpenRouter's public catalog. It runs as part of model discovery, so the same
// pass that learns a model id can learn what it costs; a model with no rate
// card then reports as unpriced in the dashboard instead of $0.00.
func (s *Server) harvestPricing(ctx context.Context) {
	if s.harvestFresh() {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, harvestedPricingTimeout)
	defer cancel()
	got, err := pricing.FetchOpenRouter(fctx, s.client)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("pricing: public rate list unavailable: %v", err)
		}
		return
	}

	s.harvestMu.Lock()
	defer s.harvestMu.Unlock()
	if s.harvestFreshLocked() {
		return // another pass finished while this one was fetching
	}
	if s.harvested == nil {
		s.harvested = make(map[string]pricing.Price, len(got))
	}
	learned := 0
	t := s.rec.prices.Load()
	for id, p := range got {
		s.harvested[id] = p
		if t != nil && t.Learn(id, p) {
			learned++
		}
	}
	s.harvestAt = time.Now()
	log.Printf("pricing: %d public rate(s) loaded, %d model(s) priced for the first time", len(got), learned)
}

func (s *Server) harvestFresh() bool {
	s.harvestMu.Lock()
	defer s.harvestMu.Unlock()
	return s.harvestFreshLocked()
}

func (s *Server) harvestFreshLocked() bool {
	return !s.harvestAt.IsZero() && time.Since(s.harvestAt) < harvestedPricingTTL
}
