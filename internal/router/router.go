// Package router holds FastRouter's hot state in memory: providers, per-provider
// account pools and combos. Nothing here takes a global lock; each pool has its
// own small mutex for round-robin bookkeeping and accounts use atomics.
package router

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/discover"
	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/oauth"
	"github.com/zenpo/fastrouter/internal/store"
	"github.com/zenpo/fastrouter/internal/translate"
)

// Account is one provider credential with live health state.
type Account struct {
	ID, Name string
	key      string    // bearer / x-api-key value; never logged
	URLs     [4]string // per-account endpoint overrides (region), by format
	// AccountID is the ChatGPT account id for Codex accounts ("" otherwise).
	AccountID string
	// Extra is non-secret per-account data OAuth adapters need (projectId, orgId, ...).
	Extra     map[string]string
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
	ID, Name, Prefix string
	URLs             [4]string         // endpoint per translate.Format ("" = not supported)
	PreferNative     bool              // translate into the native (Gemini) format, not OpenAI-compatible
	Official         bool              // strict OpenAI-style API (no vendor reasoning fields)
	CacheControl     bool              // forward cache_control inside OpenAI bodies
	ThinkingMode     string            // imported providerThinking mode ("" = auto)
	Plugin           bool              // login provider deferred to a plugin
	Login            string            // official-login plugin ("codex", "glm") or ""
	Upstream         map[string]string // catalog model id -> served id
	DecisionURL      string            // typed-judgment endpoint ("" = not a decision engine)
	DecisionModel    string            // default decision model id
	DefaultModel     string            // custom endpoint: model for bare "prefix" / "prefix/default"
	Pool             *Pool
}

// Pick chooses the upstream format for a client speaking cf: the same format
// when the provider accepts it (passthrough), else the provider's best one.
func (p *Provider) Pick(cf translate.Format) (translate.Format, string) {
	if u := p.URLs[cf]; u != "" {
		return cf, u
	}
	order := []translate.Format{translate.OpenAI, translate.Anthropic, translate.Responses, translate.Gemini}
	if p.PreferNative {
		order = []translate.Format{translate.Gemini, translate.Anthropic, translate.OpenAI, translate.Responses}
	}
	for _, f := range order {
		if u := p.URLs[f]; u != "" {
			return f, u
		}
	}
	return cf, ""
}

// Combo is a named model list.
type Combo struct {
	Name       string
	Models     []string
	RoundRobin bool
	// Strategy is fallback, round-robin, smart (a router model picks the
	// order), fusion (every model answers, a judge writes the reply) or swarm
	// (agents answer, debate each other for a few synchronized rounds, and a
	// judge writes the reply).
	Strategy      string
	SmartPolicy   string  // smart: "smart" (switch on high confidence), "strict" or "per-turn"
	Threshold     float64 // smart: confidence needed to switch a session's model
	RouterModel   string  // smart: model that picks; default the first combo model
	RouterEngine  string  // smart: "model" (default) or "decision" (typed choice from a decision engine)
	DecisionModel string  // smart/cascade: decision engine model, e.g. "jev"; "" = auto
	JudgeModel    string  // fusion/swarm: model that writes the answer; default the first combo model
	Rounds        int     // swarm: debate rounds; 0 means the default, capped when applied
	Roles         bool    // swarm: give each agent a distinct reasoning lens in round 1
	next          atomic.Uint64
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
	keys      map[[32]byte]*KeyInfo // active local API keys by sha256
	Models    []string              // ids for /v1/models
	Snap      *store.Snapshot
	List      []*Provider // unique providers, stable order

	visionCaps map[string]bool // "providerID/model" -> declared vision capability
	vision     *Combo          // capacityAdapter vision pool (nil = off)
	// discovered holds the "providerID/model" keys that came from a provider's
	// own model list, so a 404 for one can be recorded without touching the
	// catalog's models.
	discovered map[string]bool
	// CCFilterNaming answers Claude Code's topic-naming requests locally (imported setting).
	CCFilterNaming bool
}

// ErrModelNotFound means the model name did not resolve.
var ErrModelNotFound = errors.New("model not found")

// Build creates a State from a snapshot. limits are optional per-key limits
// from the config file, keyed by key name or id.
func Build(snap *store.Snapshot, limits map[string]KeyLimit) *State {
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
		p := &Provider{ID: c.ID, Name: c.ID, Prefix: c.PrimaryAlias(), Plugin: c.Plugin, Login: c.Login, Upstream: c.Upstream, Pool: &Pool{},
			PreferNative: c.PreferNative, Official: c.Official, CacheControl: c.CacheControl,
			DecisionURL: c.DecisionURL, DecisionModel: c.DecisionModel}
		p.URLs[translate.OpenAI], p.URLs[translate.Anthropic] = c.ChatURL, c.MessagesURL
		p.URLs[translate.Responses], p.URLs[translate.Gemini] = c.ResponsesURL, c.GeminiURL
		add(p, append([]string{c.ID}, c.Aliases...)...)
		return p
	}
	isExtra := func(id string) bool { c, ok := catalog.Lookup(id); return ok && c.Extra }
	for _, c := range snap.Connections {
		if !isExtra(c.Provider) {
			builtin(c.Provider)
		}
	}
	for _, n := range snap.Nodes {
		if catalog.Reserved(n.Prefix) {
			continue
		}
		add(nodeProvider(n, n.Prefix), append([]string{n.ID, n.Prefix}, n.Aliases...)...)
	}
	// Generated extras come after custom endpoints, so an endpoint keeps its prefix.
	for _, c := range snap.Connections {
		if isExtra(c.Provider) {
			builtin(c.Provider)
		}
	}
	// Nodes whose prefix collides with a built-in are still reachable by id.
	for _, n := range snap.Nodes {
		if _, ok := byID[n.ID]; !ok {
			add(nodeProvider(n, n.ID), n.ID)
		}
	}
	var thinking map[string]struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(snap.Settings["providerThinking"], &thinking)
	for id, t := range thinking {
		if p := byID[id]; p != nil && t.Mode != "auto" {
			p.ThinkingMode = t.Mode
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
		if p == nil || !c.Active || p.Plugin {
			continue
		}
		key := c.Secrets["apiKey"]
		var accountID string
		if p.Login != "" {
			// Imported before FastRouter supported the sign-in: usable now.
			if c.Status != "ok" && c.Status != "needs-plugin" && c.Status != "unknown-provider" {
				continue
			}
			key, accountID = login.AccessKey(c), login.CodexAccountID(c)
			if p.Login == "oauth" {
				key = oauth.AccessKey(c)
			}
		} else if c.Status != "ok" {
			continue
		}
		if key == "" {
			continue
		}
		a := &Account{ID: c.ID, Name: c.Name, key: key, AccountID: accountID}
		if p.Login == "oauth" {
			a.Extra = oauth.Extra(c)
		}
		if psd, ok := c.Meta["providerSpecificData"].(map[string]any); ok {
			if region, _ := psd["region"].(string); region != "" {
				if cat, ok := catalog.Lookup(p.ID); ok && cat.Regions[region] != "" {
					a.URLs[translate.OpenAI] = cat.Regions[region] + "/chat/completions"
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

	var comboOpts map[string]struct {
		SmartPolicy         string  `json:"smartPolicy"`
		ConfidenceThreshold float64 `json:"confidenceThreshold"`
		RouterModel         string  `json:"routerModel"`
		RouterEngine        string  `json:"routerEngine"`
		DecisionModel       string  `json:"decisionModel"`
		JudgeModel          string  `json:"judgeModel"`
		Rounds              int     `json:"rounds"`
		Roles               bool    `json:"roles"`
	}
	_ = json.Unmarshal(snap.Settings["comboStrategies"], &comboOpts)
	for _, c := range snap.Combos {
		if c.Kind != "" || len(c.Models) == 0 {
			continue // e.g. webSearch combos are not chat models
		}
		o := comboOpts[c.Name]
		cb := &Combo{Name: c.Name, Models: c.Models, RoundRobin: c.Strategy == "round-robin", Strategy: c.Strategy,
			SmartPolicy: o.SmartPolicy, Threshold: o.ConfidenceThreshold, RouterModel: o.RouterModel, RouterEngine: o.RouterEngine,
			DecisionModel: o.DecisionModel, JudgeModel: o.JudgeModel, Rounds: o.Rounds, Roles: o.Roles}
		if cb.SmartPolicy == "" {
			cb.SmartPolicy = "smart"
		}
		if cb.Threshold > 1 {
			cb.Threshold /= 100 // stored as a percentage
		}
		if cb.Threshold <= 0 {
			cb.Threshold = 0.8
		}
		s.combos[c.Name] = cb
		s.Models = append(s.Models, c.Name)
	}
	s.visionCaps = map[string]bool{}
	for _, m := range snap.CustomModels {
		p := s.providers[m.ProviderAlias]
		if p == nil {
			continue
		}
		var caps struct {
			Vision *bool `json:"vision"`
		}
		if json.Unmarshal(m.Caps, &caps) == nil && caps.Vision != nil {
			s.visionCaps[p.ID+"/"+m.ID] = *caps.Vision
		}
		if (m.Type == "" || m.Type == "llm") && !p.Plugin {
			s.Models = append(s.Models, s.PrefixFor(p)+"/"+m.ID)
		}
	}
	s.mergeDiscovered(snap)
	var adapter struct {
		Vision *struct {
			Enabled    *bool    `json:"enabled"`
			RoundRobin bool     `json:"roundRobin"`
			Models     []string `json:"models"`
		} `json:"vision"`
	}
	_ = json.Unmarshal(snap.Settings["capacityAdapter"], &adapter)
	if v := adapter.Vision; v != nil && (v.Enabled == nil || *v.Enabled) && len(v.Models) > 0 {
		s.vision = &Combo{Name: "capacity-adapter:vision", Models: v.Models, RoundRobin: v.RoundRobin}
	}
	_ = json.Unmarshal(snap.Settings["ccFilterNaming"], &s.CCFilterNaming)
	s.keys = buildKeys(snap.APIKeys, limits)
	return s
}

// mergeDiscovered adds the models a provider's own /models list reported, so a
// model released after this build is listed without a new binary. Routing never
// needed the catalog (resolve is prefix-only), so this only affects /v1/models,
// the dashboard picker and vision detection.
//
// The catalog stays authoritative: a discovered id that is only a wire id (a
// value of the provider's Upstream map) is skipped, and an id the catalog or a
// custom model already lists is not duplicated.
func (s *State) mergeDiscovered(snap *store.Snapshot) {
	if snap == nil || len(snap.Discovered) == 0 {
		return
	}
	seen := make(map[string]bool, len(s.Models))
	for _, m := range s.Models {
		seen[m] = true
	}
	s.discovered = make(map[string]bool, len(snap.Discovered))
	for _, dm := range snap.Discovered {
		if dm.Model == "" || dm.Status == store.DiscoveredDead {
			continue
		}
		p := s.providers[dm.Provider]
		if p == nil || p.Plugin {
			continue
		}
		if discover.IsWireID(p.Upstream, dm.Model) {
			continue
		}
		s.discovered[p.ID+"/"+dm.Model] = true
		name := s.PrefixFor(p) + "/" + dm.Model
		if seen[name] {
			continue
		}
		seen[name] = true
		s.Models = append(s.Models, name)
		if visionCap(dm.Caps) {
			s.visionCaps[p.ID+"/"+dm.Model] = true
		}
	}
}

// visionCap reads {"vision":true} from a discovered model's caps JSON.
func visionCap(caps string) bool {
	if caps == "" {
		return false
	}
	var c struct {
		Vision *bool `json:"vision"`
	}
	if json.Unmarshal([]byte(caps), &c) != nil {
		return false
	}
	return c.Vision != nil && *c.Vision
}

func nodeProvider(n store.Node, prefix string) *Provider {
	base := strings.TrimRight(n.BaseURL, "/")
	p := &Provider{ID: n.ID, Name: n.Name, Prefix: prefix, Pool: &Pool{}, CacheControl: true, DefaultModel: n.DefaultModel}
	switch {
	case strings.HasPrefix(n.Type, "anthropic"):
		p.URLs[translate.Anthropic] = base + "/messages"
	case n.APIType == "responses":
		p.URLs[translate.Responses] = base + "/responses"
	default:
		p.URLs[translate.OpenAI] = base + "/chat/completions"
	}
	return p
}

// SupportsVision reports whether a target model can read images: declared
// custom-model caps first, then name-based detection.
func (s *State) SupportsVision(t Target) bool {
	if v, ok := s.visionCaps[t.Provider.ID+"/"+t.Model]; ok {
		return v
	}
	return catalog.LooksLikeVision(t.Model)
}

// VisionFallback returns the capacity-adapter vision pool as targets, or nil.
func (s *State) VisionFallback() []Target {
	if s.vision == nil {
		return nil
	}
	models := s.vision.Models
	if s.vision.RoundRobin && len(models) > 1 {
		start := int(s.vision.next.Add(1)-1) % len(models)
		models = append(append([]string{}, models[start:]...), models[:start]...)
	}
	var out []Target
	for _, m := range models {
		if ts, err := s.resolve(m, 1); err == nil {
			out = append(out, ts...)
		}
	}
	return out
}

// HasAPIKeys reports whether any local API key is configured.
func (s *State) HasAPIKeys() bool { return len(s.keys) > 0 }

// CheckAPIKey returns the key's info, or nil if the key is unknown. The map is
// keyed by a SHA-256 of the key, so lookups do not leak timing on the key itself.
func (s *State) CheckAPIKey(key string) *KeyInfo {
	return s.keys[sha256.Sum256([]byte(key))]
}

// Resolve turns a requested model (combo name or prefix/model) into candidates.
func (s *State) Resolve(model string) ([]Target, error) {
	return s.resolve(model, 0)
}

// Combo returns the combo with this name.
func (s *State) Combo(name string) (*Combo, bool) {
	c, ok := s.combos[name]
	return c, ok
}

// IsDiscovered reports whether providerID/model came from a provider's own
// model list rather than the compiled catalog.
func (s *State) IsDiscovered(providerID, model string) bool {
	return s.discovered[providerID+"/"+model]
}

// ResolveModels resolves an ordered model list (as a fallback chain) into
// targets, skipping names that do not resolve.
func (s *State) ResolveModels(name string, models []string) ([]Target, error) {
	return s.resolveList(name, models, 0)
}

func (s *State) resolveList(name string, models []string, depth int) ([]Target, error) {
	var out []Target
	for _, m := range models {
		ts, err := s.resolve(m, depth+1)
		if err == nil {
			out = append(out, ts...)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: combo %q has no usable models", ErrModelNotFound, name)
	}
	return out, nil
}

func (s *State) resolve(model string, depth int) ([]Target, error) {
	if c, ok := s.combos[model]; ok && depth < 3 {
		models := c.Models
		if c.RoundRobin && len(models) > 1 {
			start := int(c.next.Add(1)-1) % len(models)
			models = append(append([]string{}, models[start:]...), models[:start]...)
		}
		return s.resolveList(model, models, depth)
	}
	prefix, name, ok := strings.Cut(model, "/")
	if p := s.providers[prefix]; p != nil && p.DefaultModel != "" && (name == "" || name == "default") {
		// a custom endpoint's default model: "prefix", "prefix/" or "prefix/default"
		return []Target{{Provider: p, Model: p.DefaultModel, Label: prefix + "/" + p.DefaultModel}}, nil
	}
	if !ok || name == "" {
		return nil, fmt.Errorf("%w: %q (use prefix/model or a combo name)", ErrModelNotFound, model)
	}
	p := s.providers[prefix]
	if p == nil {
		return nil, fmt.Errorf("%w: unknown provider prefix %q", ErrModelNotFound, prefix)
	}
	if up, ok := p.Upstream[name]; ok {
		name = up
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

// Provider looks up a provider by id, alias or prefix.
func (s *State) Provider(key string) (*Provider, bool) {
	p, ok := s.providers[key]
	return p, ok
}

// CarryOver copies live state from the table being replaced: account
// cooldowns, round-robin positions, combo rotation and key rate limits, so a
// reload (SIGHUP, a sign-in or a token refresh) does not reset them.
func (s *State) CarryOver(prev *State) {
	if prev == nil {
		return
	}
	accts := map[string]*Account{}
	pools := map[string]*Pool{}
	for _, p := range prev.List {
		pools[p.ID] = p.Pool
		for _, a := range p.Pool.accounts {
			accts[a.ID] = a
		}
	}
	for _, p := range s.List {
		if old := pools[p.ID]; old != nil {
			old.mu.Lock()
			p.Pool.cur, p.Pool.count = old.cur, old.count
			old.mu.Unlock()
			if n := len(p.Pool.accounts); n > 0 {
				p.Pool.cur %= n
			}
		}
		for _, a := range p.Pool.accounts {
			if old := accts[a.ID]; old != nil {
				a.coolUntil.Store(old.coolUntil.Load())
				a.fails.Store(old.fails.Load())
			}
		}
	}
	for name, c := range s.combos {
		if old := prev.combos[name]; old != nil {
			c.next.Store(old.next.Load())
		}
	}
	oldKeys := map[string]*KeyInfo{}
	for _, k := range prev.keys {
		oldKeys[k.ID] = k
	}
	for _, k := range s.keys {
		if old := oldKeys[k.ID]; old != nil && old.limiter != nil && k.limiter != nil && old.limiter.max == k.limiter.max {
			k.limiter = old.limiter
		}
	}
}

// Only returns a copy of p whose pool holds just account a (for per-account tests).
func (p *Provider) Only(a *Account) *Provider {
	cp := *p
	cp.Pool = &Pool{accounts: []*Account{a}, sticky: 1}
	return &cp
}
