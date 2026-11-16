package oauth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

// Store is what the refresher needs from the database.
type Store interface {
	Load() (*store.Snapshot, error)
	SaveConnection(store.Connection) error
}

// Refresher keeps OAuth access tokens fresh in the background so the request
// path never waits on a refresh.
type Refresher struct {
	Store  Store
	Client *http.Client
	// RefreshImported also refreshes imported accounts whose provider rotates
	// refresh tokens before they expire (otherwise only once expired, so a
	// still-running 9Router holding the same token is not signed out early).
	RefreshImported bool
	OnUpdate        func(store.Connection)
	Now             func() time.Time

	mu      sync.Mutex
	backoff map[string]time.Time
}

// Due reports whether c should be refreshed now.
func (r *Refresher) Due(c store.Connection, now time.Time) bool {
	s, ok := specs[c.Provider]
	if !ok || s.Refresh == nil || !c.Active || c.Status == "needs-reauth" {
		return false
	}
	exp := ExpiresAt(c)
	if AccessKey(c) == "" {
		return Secret(c, "refreshToken") != "" || c.Provider == "github" || c.Provider == "vertex"
	}
	if exp.IsZero() {
		return false
	}
	imported := anyStr(c.Meta["origin"]) != "fastrouter"
	if s.Rotates && imported && !r.RefreshImported {
		return !now.Before(exp)
	}
	return !now.Before(exp.Add(-s.RefreshLead))
}

// RefreshOne refreshes one account and saves it. It returns the updated connection.
func (r *Refresher) RefreshOne(ctx context.Context, c store.Connection) (store.Connection, error) {
	s, ok := specs[c.Provider]
	if !ok || s.Refresh == nil {
		return c, errors.New("this provider has no token refresh")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	res, err := s.Refresh(cctx, Env{Client: r.Client}, c)
	cancel()
	if err != nil {
		if errors.Is(err, ErrReauth) {
			c.Status = "needs-reauth"
			if c.Meta == nil {
				c.Meta = map[string]any{}
			}
			c.Meta["lastError"] = "sign-in expired; sign in again from the dashboard"
			if r.Store != nil {
				_ = r.Store.SaveConnection(c)
			}
			if r.OnUpdate != nil {
				r.OnUpdate(c)
			}
		}
		return c, err
	}
	Apply(&c, res, now())
	if r.Store != nil {
		if err := r.Store.SaveConnection(c); err != nil {
			return c, err
		}
	}
	if r.OnUpdate != nil {
		r.OnUpdate(c)
	}
	return c, nil
}

// RunOnce refreshes every due account and returns how many succeeded.
func (r *Refresher) RunOnce(ctx context.Context) int {
	if r.Store == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.backoff == nil {
		r.backoff = map[string]time.Time{}
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	snap, err := r.Store.Load()
	if err != nil {
		log.Printf("oauth refresh: load failed: %v", err)
		return 0
	}
	n := 0
	for _, c := range snap.Connections {
		if now.Before(r.backoff[c.ID]) || !r.Due(c, now) {
			continue
		}
		if _, err := r.RefreshOne(ctx, c); err != nil {
			r.backoff[c.ID] = now.Add(5 * time.Minute)
			log.Printf("oauth refresh: %s account %s: %v", c.Provider, c.ID, err) // errors never carry tokens
			continue
		}
		delete(r.backoff, c.ID)
		n++
		log.Printf("oauth refresh: %s account %s refreshed", c.Provider, c.ID)
	}
	return n
}

// Run refreshes on start and then every interval until ctx ends.
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
