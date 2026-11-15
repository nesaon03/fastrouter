// Package router holds FastRouter's hot state in memory: providers, per-provider
// account pools and combos. Nothing here takes a global lock; each pool has its
// own small mutex for round-robin bookkeeping and accounts use atomics.
package router

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/store"
)

// Account is one provider credential with live health state.
type Account struct {
	ID, Name  string
	key       string // bearer / x-api-key value; never logged
	ChatURL   string // per-account override (region), else provider's
	coolUntil atomic.Int64
	fails     atomic.Int32
}

// Key returns the credential for building an upstream request.
func (a *Account) Key() string { return a.key }

// CoolingDown reports whether the account is in cooldown.
func (a *Account) CoolingDown(now time.Time) bool { return a.coolUntil.Load() > now.UnixNano() }

// Fail puts the account in cooldown: base * 2^(fails-1), capped at 5 minutes,
// or retryAfter if the upstream said so.
func (a *Account) Fail(base, retryAfter time.Duration) {
	n := a.fails.Add(1)
	d := retryAfter
	if d <= 0 {
		d = base << min(n-1, 4)
	}
	d = min(d, 5*time.Minute)
	a.coolUntil.Store(time.Now().Add(d).UnixNano())
}

// Succeed clears failure state.
func (a *Account) Succeed() {
	if a.fails.Load() != 0 {
		a.fails.Store(0)
		a.coolUntil.Store(0)
	}
}

// Pool is the set of active accounts for one provider.
type Pool struct {
	accounts   []*Account // priority order
	roundRobin bool
	sticky     int

	mu         sync.Mutex
	cur, count int
}

// Order returns accounts in the order to try them for one request.
// Round-robin keeps using the same account `sticky` times before rotating.
// Accounts in cooldown go last, so they are only used as a last resort.
func (p *Pool) Order(now time.Time) []*Account {
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	start := 0
	if p.roundRobin && n > 1 {
		p.mu.Lock()
		if p.count >= p.sticky {
			p.cur = (p.cur + 1) % n
			p.count = 0
		}
		p.count++
		start = p.cur
		p.mu.Unlock()
	}
	out := make([]*Account, 0, n)
	var cooling []*Account
	for i := range n {
		a := p.accounts[(start+i)%n]
		if a.CoolingDown(now) {
			cooling = append(cooling, a)
		} else {
			out = append(out, a)
		}
	}
	return append(out, cooling...)
}

// Accounts lists the pool in priority order without touching rotation state.
func (p *Pool) Accounts() []*Account { return p.accounts }

// Size is the number of usable accounts.
func (p *Pool) Size() int { return len(p.accounts) }

// Provider is a resolved upstream (built-in or custom endpoint).
type Provider struct {
	ID, Name, Prefix     string
	ChatURL, MessagesURL string
	Plugin               bool // login provider deferred to a plugin
	Pool                 *Pool
}

// Combo is a named model list.
type Combo struct {
	Name       string
	Models     []string
	RoundRobin bool
	next       atomic.Uint64
}

// Target is one (provider, upstream model) candidate.
type Target struct {
	Provider *Provider
	Model    string // model name sent upstream
	Label    string // prefix/model as requested
}

// State is an immutable-after-build routing table.
type State struct {
	providers map[string]*Provider // by prefix, alias and id
	combos    map[string]*Combo
	keyHashes [][32]byte // active local API keys (sha256)
	Models    []string   // ids for /v1/models
	Snap      *store.Snapshot
	List      []*Provider // unique providers, stable order
}

// ErrModelNotFound means the model name did not resolve.
var ErrModelNotFound = errors.New("model not found")

// Build creates a State from a snapshot.
func Build(snap *store.Snapshot) *State {
	s := &State{providers: map[string]*Provider{}, combos: map[string]*Combo{}, Snap: snap}

	var strategies map[string]struct {
		FallbackStrategy      string `json:"fallbackStrategy"`
		StickyRoundRobinLimit int    `json:"stickyRoundRobinLimit"`
	}
	_ = json.Unmarshal(snap.Settings["providerStrategies"], &strategies)
	globalSticky := 3
	_ = json.Unmarshal(snap.Settings["stickyRoundRobinLimit"], &globalSticky)

	byID := map[string]*Provider{}
	add := func(p *Provider, keys ...string) {
		byID[p.ID] = p
		s.List = append(s.List, p)
		for _, k := range keys {
			if _, taken := s.providers[k]; !taken && k != "" {
				s.providers[k] = p
			}
		}
	}

	// Built-ins referenced by accounts are created on demand below; custom nodes
	// first are NOT allowed to shadow built-in prefixes (same rule as 9Router).
	builtin := func(id string) *Provider {
		if p, ok := byID[id]; ok {
			return p
		}
		c, ok := catalog.Lookup(id)
		if !ok {
			return nil
		}
		p := &Provider{ID: c.ID, Name: c.ID, Prefix: c.PrimaryAlias(), ChatURL: c.ChatURL, MessagesURL: c.MessagesURL, Plugin: c.Plugin, Pool: &Pool{}}
		add(p, append([]string{c.ID}, c.Aliases...)...)
		return p
	}
	for _, c := range snap.Connections {
		builtin(c.Provider)
	}
	for _, n := range snap.Nodes {
		if _, isBuiltin := catalog.Lookup(n.Prefix); isBuiltin {
			continue
		}
		base := strings.TrimRight(n.BaseURL, "/")
		p := &Provider{ID: n.ID, Name: n.Name, Prefix: n.Prefix, Pool: &Pool{}}
		if strings.HasPrefix(n.Type, "anthropic") {
			p.MessagesURL = base + "/messages"
		} else {
			p.ChatURL = base + "/chat/completions"
		}
		add(p, n.ID, n.Prefix)
	}
	// Nodes whose prefix collides with a built-in are still reachable by id.
	for _, n := range snap.Nodes {
		if _, ok := byID[n.ID]; !ok {
			base := strings.TrimRight(n.BaseURL, "/")
			add(&Provider{ID: n.ID, Name: n.Name, Prefix: n.ID, ChatURL: base + "/chat/completions", Pool: &Pool{}}, n.ID)
		}
	}

	// Accounts into per-provider pools (active, core-supported only).
	type ranked struct {
		a        *Account
		priority int
		ord      int
	}
	pending := map[*Provider][]ranked{}
	for i, c := range snap.Connections {
		p := byID[c.Provider]
		if p == nil || !c.Active || c.Status != "ok" || p.Plugin {
			continue
		}
		key := c.Secrets["apiKey"]
		if key == "" {
			continue
		}
		a := &Account{ID: c.ID, Name: c.Name, key: key}
		if psd, ok := c.Meta["providerSpecificData"].(map[string]any); ok {
			if region, _ := psd["region"].(string); region != "" {
				if cat, ok := catalog.Lookup(p.ID); ok && cat.Regions[region] != "" {
					a.ChatURL = cat.Regions[region] + "/chat/completions"
				}
			}
		}
		pending[p] = append(pending[p], ranked{a, c.Priority, i})
	}
	for p, rs := range pending {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].priority < rs[j].priority })
		for _, r := range rs {
			p.Pool.accounts = append(p.Pool.accounts, r.a)
		}
		st := strategies[p.ID]
		p.Pool.roundRobin = st.FallbackStrategy == "round-robin"
		p.Pool.sticky = st.StickyRoundRobinLimit
		if p.Pool.sticky <= 0 {
			p.Pool.sticky = globalSticky
		}
	}

	for _, c := range snap.Combos {
		if c.Kind != "" || len(c.Models) == 0 {
			continue // e.g. webSearch combos are not chat models
		}
		s.combos[c.Name] = &Combo{Name: c.Name, Models: c.Models, RoundRobin: c.Strategy == "round-robin"}
		s.Models = append(s.Models, c.Name)
	}
	for _, m := range snap.CustomModels {
		if m.Type != "" && m.Type != "llm" {
			continue
		}
		if p := s.providers[m.ProviderAlias]; p != nil && !p.Plugin {
			s.Models = append(s.Models, s.PrefixFor(p)+"/"+m.ID)
		}
	}
	for _, k := range snap.APIKeys {
		if k.Active && k.Key != "" {
			s.keyHashes = append(s.keyHashes, sha256.Sum256([]byte(k.Key)))
		}
	}
	return s
}

// HasAPIKeys reports whether any local API key is configured.
func (s *State) HasAPIKeys() bool { return len(s.keyHashes) > 0 }

// CheckAPIKey compares key against configured keys in constant time.
func (s *State) CheckAPIKey(key string) bool {
	h := sha256.Sum256([]byte(key))
	ok := 0
	for _, k := range s.keyHashes {
		ok |= subtle.ConstantTimeCompare(h[:], k[:])
	}
	return ok == 1
}

// Resolve turns a requested model (combo name or prefix/model) into candidates.
func (s *State) Resolve(model string) ([]Target, error) {
	return s.resolve(model, 0)
}

func (s *State) resolve(model string, depth int) ([]Target, error) {
	if c, ok := s.combos[model]; ok && depth < 3 {
		models := c.Models
		if c.RoundRobin && len(models) > 1 {
			start := int(c.next.Add(1)-1) % len(models)
			models = append(append([]string{}, models[start:]...), models[:start]...)
		}
		var out []Target
		for _, m := range models {
			ts, err := s.resolve(m, depth+1)
			if err == nil {
				out = append(out, ts...)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: combo %q has no usable models", ErrModelNotFound, model)
		}
		return out, nil
	}
	prefix, name, ok := strings.Cut(model, "/")
	if !ok || name == "" {
		return nil, fmt.Errorf("%w: %q (use prefix/model or a combo name)", ErrModelNotFound, model)
	}
	p := s.providers[prefix]
	if p == nil {
		return nil, fmt.Errorf("%w: unknown provider prefix %q", ErrModelNotFound, prefix)
	}
	return []Target{{Provider: p, Model: name, Label: model}}, nil
}

// PrefixFor is the model prefix that reaches p: its own prefix when that is
// unambiguous, otherwise its id.
func (s *State) PrefixFor(p *Provider) string {
	if s.providers[p.Prefix] == p {
		return p.Prefix
	}
	return p.ID
}
