package server

import (
	"log"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

// recorder writes usage events to SQLite in the background, off the request path.
type recorder struct {
	ch   chan store.UsageEvent
	done chan struct{}
}

func newRecorder(st *store.Store) *recorder {
	r := &recorder{ch: make(chan store.UsageEvent, 4096), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		var batch []store.UsageEvent
		flush := func() {
			if len(batch) == 0 || st == nil {
				batch = batch[:0]
				return
			}
			if err := st.WriteUsage(batch); err != nil {
				log.Printf("usage write failed: %v", err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case e, ok := <-r.ch:
				if !ok {
					flush()
					return
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
func (r *recorder) record(e store.UsageEvent) {
	select {
	case r.ch <- e:
	default:
	}
}

func (r *recorder) close() {
	close(r.ch)
	<-r.done
}
