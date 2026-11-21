// Package pricing turns token usage into a USD cost.
//
// The default rate table (pricing_9router.json) and the lookup order are taken
// from 9Router's open-sse/providers/pricing.js, Copyright (c) 2024-2026 decolua
// and contributors, MIT License (see LICENSE-9router in this directory).
package pricing

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Price is a rate card in USD per 1M tokens. Zero cached/reasoning/cache-write
// rates fall back to the input or output rate, as in 9Router.
type Price struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Cached        float64 `json:"cached"`
	Reasoning     float64 `json:"reasoning"`
	CacheCreation float64 `json:"cache_creation"`
}

// Tokens is one request's usage. Input includes cache reads and writes;
// Output includes Reasoning.
type Tokens struct{ Input, Output, CacheRead, CacheWrite, Reasoning int64 }

//go:embed pricing_9router.json
var defaultJSON []byte

type pattern struct {
	re    *regexp.Regexp
	price Price
}

// Table resolves prices. It is safe for concurrent use.
type Table struct {
	mu       sync.RWMutex // guards model/patterns/free against Learn
	gen      atomic.Uint64
	provider map[string]map[string]Price
	model    map[string]Price
	patterns []pattern
	free     []string
	cache    sync.Map // gen+"\x00"+providers+"\x00"+model -> *Price (nil = unknown)
}

// New loads the built-in table with user overrides merged on top. overrides
// uses 9Router's "pricing" shape: {provider: {model: {input, output, ...}}}.
func New(overrides json.RawMessage) (*Table, error) {
	var d struct {
		Model    map[string]Price            `json:"model"`
		Provider map[string]map[string]Price `json:"provider"`
		Patterns []struct {
			Pattern string `json:"pattern"`
			Pricing Price  `json:"pricing"`
		} `json:"patterns"`
		Free []string `json:"free"`
	}
	if err := json.Unmarshal(defaultJSON, &d); err != nil {
		return nil, err
	}
	t := &Table{provider: d.Provider, model: d.Model, free: d.Free}
	if t.provider == nil {
		t.provider = map[string]map[string]Price{}
	}
	if t.model == nil {
		t.model = map[string]Price{}
	}
	if len(overrides) > 0 {
		var user map[string]map[string]Price
		if err := json.Unmarshal(overrides, &user); err != nil {
			return nil, err
		}
		for prov, models := range user {
			if t.provider[prov] == nil {
				t.provider[prov] = map[string]Price{}
			}
			for m, p := range models {
				t.provider[prov][m] = p
			}
		}
	}
	for _, p := range d.Patterns {
		parts := strings.Split(p.Pattern, "*")
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		re, err := regexp.Compile("(?i)^" + strings.Join(parts, ".*") + "$")
		if err != nil {
			return nil, err
		}
		t.patterns = append(t.patterns, pattern{re, p.Pricing})
	}
	return t, nil
}

// Lookup finds the rate card for a model, trying each provider key in order
// (e.g. id then alias): provider override, free namespace, model, pattern.
func (t *Table) Lookup(model string, providers ...string) *Price {
	key := strconv.FormatUint(t.gen.Load(), 10) + "\x00" + strings.Join(providers, ",") + "\x00" + model
	if v, ok := t.cache.Load(key); ok {
		return v.(*Price)
	}
	t.mu.RLock()
	p := t.lookup(model, providers)
	t.mu.RUnlock()
	t.cache.Store(key, p)
	return p
}

// Learn records a rate card harvested from a provider's public price list. It
// only fills a gap: a compiled or user-overridden price is never replaced, so a
// harvested rate cannot silently change what an already-priced model costs.
// The model id and its base name (the part after the last "/") both become
// known, so "anthropic/claude-x" prices "claude-x" on any provider.
// It reports whether anything new was learned.
func (t *Table) Learn(model string, p Price) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	t.mu.Lock()
	changed := false
	put := func(k string) {
		if k == "" {
			return
		}
		if _, ok := t.model[k]; ok {
			return
		}
		t.model[k] = p
		changed = true
	}
	put(model)
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		put(model[i+1:])
	}
	if changed {
		t.gen.Add(1) // new cache generation: stale "unknown" answers stop being served
	}
	t.mu.Unlock()
	return changed
}

func (t *Table) lookup(model string, providers []string) *Price {
	if model == "" {
		return nil
	}
	for _, prov := range providers {
		if p, ok := t.provider[prov][model]; ok {
			return &p
		}
	}
	for _, ns := range t.free {
		if strings.HasPrefix(model, ns) {
			return &Price{}
		}
	}
	base := model
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		base = model[i+1:]
	}
	if p, ok := t.model[base]; ok {
		return &p
	}
	if p, ok := t.model[model]; ok {
		return &p
	}
	for _, pt := range t.patterns {
		if pt.re.MatchString(base) || pt.re.MatchString(model) {
			p := pt.price
			return &p
		}
	}
	return nil
}

// Cost is the USD cost of tok at price p (0 when p is nil). Unlike 9Router,
// reasoning tokens are not charged twice: they are part of Output, so only the
// rest of Output is billed at the output rate.
func (p *Price) Cost(tok Tokens) float64 {
	if p == nil {
		return 0
	}
	or := func(v, def float64) float64 {
		if v > 0 {
			return v
		}
		return def
	}
	nonCached := max(0, tok.Input-tok.CacheRead-tok.CacheWrite)
	reasoning := min(tok.Reasoning, tok.Output)
	c := float64(nonCached)*p.Input +
		float64(tok.CacheRead)*or(p.Cached, p.Input) +
		float64(tok.CacheWrite)*or(p.CacheCreation, p.Input) +
		float64(tok.Output-reasoning)*p.Output +
		float64(reasoning)*or(p.Reasoning, p.Output)
	return c / 1e6
}
