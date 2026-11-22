package server

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nesaon03/fastrouter/internal/pricing"
	"github.com/nesaon03/fastrouter/internal/store"
	tr "github.com/nesaon03/fastrouter/internal/translate"
)

// usageItem is a finished attempt plus what is needed to price it later.
type usageItem struct {
	ev            store.UsageEvent
	model, prefix string // upstream model and provider alias, for pricing
	savedBytes    int    // gross input bytes removed by the savers
	addedBytes    int    // prompt bytes injected by the savers
	tag           string
}

// recorder prices and writes usage events in the background, off the request path.
type recorder struct {
	mu     sync.RWMutex // record holds it shared; close takes it exclusively
	closed bool
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
				if t := r.prices.Load(); t != nil {
					if e.Input > 0 || e.Output > 0 {
						e.Cost = t.Lookup(it.model, e.Provider, it.prefix).Cost(pricing.Tokens{
							Input: e.Input, Output: e.Output, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Reasoning: e.Reasoning})
					}
					if saved := estimateTokens(it.savedBytes); saved > 0 {
						e.SavedInput = saved
						e.SavedCost = t.Lookup(it.model, e.Provider, it.prefix).Cost(pricing.Tokens{Input: saved})
					}
					e.AddedInput = estimateTokens(it.addedBytes)
				}
				e.SaverTag = it.tag
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

// record never blocks the request path; if the queue is full or the
// recorder is closed the event is dropped (never a send on a closed channel).
func (r *recorder) record(it usageItem) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.ch <- it:
	default:
	}
}

func (r *recorder) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.ch)
	r.mu.Unlock()
	<-r.done
}

// estimateTokens approximates tokens from bytes (4 bytes per token, rounded up).
func estimateTokens(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64((n + 3) / 4)
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
		savedBytes: a.saverStats.SavedBytes(), addedBytes: a.saverStats.PromptAdded, tag: a.saverStats.Tag,
	})
}
