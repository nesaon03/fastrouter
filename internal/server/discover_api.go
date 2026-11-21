package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/discover"
	"github.com/zenpo/fastrouter/internal/oauth"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/secret"
	"github.com/zenpo/fastrouter/internal/store"
)

// Model discovery: ask each configured provider what it serves, and store the
// answer beside the compiled catalog. Providers release models far more often
// than this binary is rebuilt, and a model id is not validated on the request
// path (resolve is prefix-only), so the only thing a new model breaks is what
// /v1/models and the dashboard picker advertise. This keeps those current
// without a release.
//
// Nothing is invented: only ids the provider itself lists are stored, and the
// catalog's Upstream map still decides which id is sent upstream.

const (
	// discoveryFirstDelay lets sign-in token refreshes settle before the first
	// poll, which matters for providers whose token just rotated.
	discoveryFirstDelay = 45 * time.Second
	// discoveryPause spaces providers out so a poll never bursts.
	discoveryPause = 300 * time.Millisecond
)

// discovering guards against overlapping runs (boot poll + a manual refresh).
var discovering atomic.Bool

// RunModelDiscovery polls every provider's model list until ctx is cancelled.
func (s *Server) RunModelDiscovery(ctx context.Context) {
	if s.st == nil || !s.discoveryEnabled() {
		return
	}
	every := s.discoveryEvery()
	// First pass shortly after boot, jittered so a fleet of instances does not
	// poll in lockstep. The jitter is a couple of minutes, not a fraction of the
	// full interval: the first poll should not wait half an hour.
	first := discoveryFirstDelay + jitter(2*time.Minute)
	select {
	case <-ctx.Done():
		return
	case <-time.After(first):
	}
	for {
		if n, err := s.RefreshDiscovered(ctx, ""); err != nil {
			if ctx.Err() == nil {
				log.Printf("model discovery: %v", err)
			}
		} else if n > 0 {
			log.Printf("model discovery: %d new model(s) listed by providers", n)
		}
		// Ã‚Â±10% so the interval drifts off any provider's rate window.
		wait := every - every/10 + jitter(every/5)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// A cheap pseudo-random spread; discovery timing needs no crypto-quality
	// randomness and this keeps the package free of a rand seed.
	return time.Duration(time.Now().UnixNano() % int64(d))
}

func (s *Server) discoveryEnabled() bool {
	m := s.cfgDiscovery
	return m.Enabled == nil || *m.Enabled
}

func (s *Server) discoveryEvery() time.Duration {
	d := time.Duration(s.cfgDiscovery.Every)
	if d <= 0 {
		d = 6 * time.Hour
	}
	return d
}

// RefreshDiscovered polls providers and saves what they list. An empty only
// means every provider with an account. The return value is the number of model
// ids that were not known before this call.
func (s *Server) RefreshDiscovered(ctx context.Context, only string) (int, error) {
	if s.st == nil {
		return 0, errors.New("no store")
	}
	if !discovering.CompareAndSwap(false, true) {
		return 0, errDiscoveryBusy
	}
	defer discovering.Store(false)
	s.harvestPricing(ctx)

	st := s.state.Load()
	if st == nil {
		return 0, errors.New("no routing table")
	}
	known := map[string]bool{}
	for _, m := range st.Models {
		known[m] = true
	}

	type target struct {
		id    string
		cat   *catalog.Provider
		prov  *router.Provider
		accts []*router.Account
	}
	var targets []target
	for _, p := range st.List {
		if p.Plugin || (only != "" && p.ID != only) {
			continue
		}
		accts := p.Pool.Accounts()
		if len(accts) == 0 {
			continue
		}
		c, ok := catalog.Lookup(p.ID)
		if !ok {
			// A custom endpoint: build the list endpoints from its own URLs.
			c = &catalog.Provider{ID: p.ID, ChatURL: p.URLs[0], MessagesURL: p.URLs[1],
				ResponsesURL: p.URLs[2], GeminiURL: p.URLs[3]}
		}
		targets = append(targets, target{id: p.ID, cat: c, prov: p, accts: accts})
	}
	if only != "" && len(targets) == 0 {
		return 0, errors.New("no active account for that provider")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].id < targets[j].id })

	added := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			return added, ctx.Err()
		}
		res, err := s.listProvider(ctx, t.prov, t.cat, t.accts[0])
		if err != nil {
			if errors.Is(err, discover.ErrNoEndpoint) {
				continue // no list this provider publishes; the catalog it is
			}
			if errors.Is(err, discover.ErrUnauthorized) {
				log.Printf("model discovery: %s rejected the credential; skipping", t.id)
				continue
			}
			// One provider being down must not stop the rest.
			log.Printf("model discovery: %s: %v", t.id, secret.Redact(err.Error()))
			time.Sleep(discoveryPause)
			continue
		}
		rows := make([]store.DiscoveredModel, 0, len(res.ids))
		prefix := t.id
		if p := stateProvider(st, t.id); p != nil {
			prefix = st.PrefixFor(p)
		}
		skipped := 0
		seen := map[string]bool{}
		for _, m := range res.ids {
			// PublicID reverses a served id back to the name callers use, and
			// drops an id that is only an adapter suffix.
			m, drop := discover.PublicID(t.cat.Upstream, m)
			if drop || m == "" || seen[m] {
				continue
			}
			if !discover.LooksLikeChat(m) {
				skipped++ // embeddings, speech, image models
				continue
			}
			// An id the provider itself marks unavailable (Antigravity lists
			// "gemini-4-carbon-disabled" and 404s it on every account) is not a
			// model anyone can call, so it is not advertised.
			if strings.HasSuffix(m, "-disabled") {
				skipped++
				continue
			}
			seen[m] = true
			if !known[prefix+"/"+m] {
				added++
			}
			rows = append(rows, store.DiscoveredModel{Provider: t.id, Model: m,
				Status: store.DiscoveredListed, Source: "api", Caps: res.caps[m]})
		}
		if skipped > 0 {
			log.Printf("model discovery: %s: stored %d chat model(s), skipped %d non-chat id(s)",
				t.id, len(rows), skipped)
		}
		if len(rows) == 0 {
			continue
		}
		if err := s.st.SaveDiscovered(t.id, rows, time.Now()); err != nil {
			log.Printf("model discovery: saving %s: %v", t.id, err)
			continue
		}
		time.Sleep(discoveryPause)
	}
	if err := s.ReloadFromStore(); err != nil {
		return added, err
	}
	return added, nil
}

var errDiscoveryBusy = errors.New("model discovery already running")

// listed is one provider's answer: model ids, plus any capability flags the
// provider volunteered (only Code Assist does).
type listed struct {
	ids  []string
	caps map[string]string // model id -> caps JSON
}

// capsJSON renders the capability flags a discovered model carries.
func capsJSON(vision, reasoning bool) string {
	if !vision && !reasoning {
		return ""
	}
	b, _ := json.Marshal(map[string]bool{"vision": vision, "reasoning": reasoning})
	return string(b)
}

// listProvider asks one provider what it serves, in whichever way that provider
// accepts. Three kinds exist:
//
//	plain API key        GET <base>/models with the key
//	sign-in (OAuth)      the vendor's own list, carrying the same headers the
//	                     chat request uses, because the token alone is rejected
//	private protocol     Cursor and Code Assist answer only through their own
//	                     API call; those are asked directly
func (s *Server) listProvider(ctx context.Context, p *router.Provider, cat *catalog.Provider, a *router.Account) (listed, error) {
	acct := oauth.Account{ID: a.ID, Extra: a.Extra}
	// Private protocols first: their headers are not a bearer token.
	switch p.ID {
	case "cursor":
		ms, err := oauth.CursorModels(ctx, a.Key(), a.Extra["machineId"], true)
		if err != nil {
			return listed{}, err
		}
		ids := make([]string, 0, len(ms))
		for _, m := range ms {
			if !m.Hidden && m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
		return listed{ids: ids}, nil
	case "antigravity", "gemini-cli":
		ms, err := oauth.CodeAssistModels(ctx, s.client, p.ID, a.Key(), acct)
		if err != nil {
			return listed{}, err
		}
		out := listed{ids: make([]string, 0, len(ms)), caps: map[string]string{}}
		for _, m := range ms {
			out.ids = append(out.ids, m.ID)
			if c := capsJSON(m.Vision, m.Think); c != "" {
				out.caps[m.ID] = c
			}
		}
		return out, nil
	case "codex":
		// ChatGPT's backend lists product slugs (gpt-6, gpt-6-astra-wm,
		// chatgpt_alpha_model_â€¦) that the Codex responses endpoint rejects with
		// "not supported when using Codex". Advertising them would be worse than
		// advertising nothing: every one of them fails on the first request, so
		// the catalog is Codex's only honest source.
		return listed{}, discover.ErrNoEndpoint
	}

	// A sign-in provider with a normal HTTP API (Claude, Kimi, Grok CLI,
	// Copilot, the Cline family): the list endpoint exists, but it only answers
	// when the request looks like the vendor's own client.
	if p.Login == "oauth" {
		urls, err := discover.Endpoint(cat)
		if err != nil {
			return listed{}, err
		}
		res, err := discover.ListWithHeaders(ctx, s.client, urls, adapterHeaders(p.ID, a))
		if err != nil {
			return listed{}, err
		}
		return listed{ids: res.Models}, nil
	}
	res, err := discover.List(ctx, s.client, cat, a.Key())
	if err != nil {
		return listed{}, err
	}
	return listed{ids: res.Models}, nil
}

// adapterHeaders replays the vendor headers the chat adapter sets, so an
// OAuth-authenticated list request is not rejected for looking like a stranger.
func adapterHeaders(providerID string, a *router.Account) map[string]string {
	h := http.Header{}
	if ad := oauth.AdapterFor(providerID); ad != nil && ad.Headers != nil {
		ad.Headers(h, a.Key(), false, oauth.Account{ID: a.ID, Extra: a.Extra})
	}
	out := make(map[string]string, len(h))
	for k := range h {
		out[k] = h.Get(k)
	}
	if len(out) == 0 {
		out["Authorization"] = "Bearer " + a.Key()
	}
	return out
}

// stateProvider finds the routing-table provider for an id (nil-safe).
func stateProvider(st *router.State, id string) *router.Provider {
	if p, ok := st.Provider(id); ok {
		return p
	}
	return nil
}

// handleModelRefresh polls one provider on demand (dashboard "Refresh models").
func (s *Server) handleModelRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.RefreshDiscovered(r.Context(), id)
	if err != nil && !errors.Is(err, errDiscoveryBusy) {
		adminErr(w, http.StatusBadGateway, err.Error())
		return
	}
	rows := []store.DiscoveredModel{}
	if s.st != nil {
		if got, lerr := s.st.DiscoveredFor(id); lerr == nil {
			rows = got
		}
	}
	adminJSON(w, http.StatusOK, map[string]any{"provider": id, "new": n, "models": rows})
}

// discoveredFor is the picker's view: live overlays for one provider.
func (s *Server) discoveredFor(provider string) []store.DiscoveredModel {
	if s.st == nil {
		return nil
	}
	rows, err := s.st.DiscoveredFor(provider)
	if err != nil {
		return nil
	}
	return rows
}
