package server

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/pricing"
	"github.com/zenpo/fastrouter/internal/store"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

// usageItem is a finished attempt plus what is needed to price it later.
type usageItem struct {
	ev            store.UsageEvent
	model, prefix string // upstream model and provider alias, for pricing
}

// recorder prices and writes usage events in the background, off the request path.
type recorder struct {
	ch     chan usageItem
	done   chan struct{}
	prices atomic.Pointer[pricing.Table]
}

func newRecorder(st *store.Store) *recorder {
	r := &recorder{ch: make(chan usageItem, 4096), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		var batch []store.UsageEvent
		flush := func() {
			if len(batch) > 0 && st != nil {
				if err := st.WriteUsage(batch); err != nil {
					log.Printf("usage write failed: %v", err)
				}
			}
			batch = batch[:0]
		}
		for {
			select {
			case it, ok := <-r.ch:
				if !ok {
					flush()
					return
				}
				e := it.ev
				if t := r.prices.Load(); t != nil && (e.Input > 0 || e.Output > 0) {
					e.Cost = t.Lookup(it.model, e.Provider, it.prefix).Cost(pricing.Tokens{
						Input: e.Input, Output: e.Output, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Reasoning: e.Reasoning})
				}
				batch = append(batch, e)
				if len(batch) >= 256 {
					flush()
				}
			case <-tick.C:
				flush()
			}
		}
	}()
	return r
}

// record never blocks the request path; if the queue is full the event is dropped.
func (r *recorder) record(it usageItem) {
	select {
	case r.ch <- it:
	default:
	}
}

func (r *recorder) close() {
	close(r.ch)
	<-r.done
}

// record queues one attempt's outcome and token usage.
func (s *Server) record(a *attempt, start time.Time, status int, u tr.Usage) {
	keyID := ""
	if k := keyFrom(a.p.r.Context()); k != nil {
		keyID = k.ID
	}
	s.rec.record(usageItem{
		ev: store.UsageEvent{At: start, ConnectionID: a.acct.ID, Provider: a.t.Provider.ID, KeyID: keyID, Model: a.t.Model,
			Status: status, Latency: time.Since(start), Input: int64(u.Input), Output: int64(u.Output),
			CacheRead: int64(u.CacheRead), CacheWrite: int64(u.CacheWrite), Reasoning: int64(u.Reasoning)},
		model: a.t.Model, prefix: a.t.Provider.Prefix,
	})
}
