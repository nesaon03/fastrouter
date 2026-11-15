package pricing

import (
	"encoding/json"
	"math"
	"testing"
)

func TestLookupAndCost(t *testing.T) {
	tb, err := New(json.RawMessage(`{"ds":{"deepseek-v4.1-flash":{"input":1,"output":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	// user override wins
	if p := tb.Lookup("deepseek-v4.1-flash", "deepseek", "ds"); p == nil || p.Input != 1 {
		t.Fatalf("override: %+v", p)
	}
	// canonical model with vendor prefix stripped
	if p := tb.Lookup("anthropic/claude-opus-4-6", "x"); p == nil || p.Input != 5 {
		t.Fatalf("model: %+v", p)
	}
	// pattern
	if p := tb.Lookup("deepseek-v9-something", "deepseek"); p == nil || p.Input != 0.14 {
		t.Fatalf("pattern: %+v", p)
	}
	// free namespace
	if p := tb.Lookup("cline-free/deepseek-v4-flash"); p == nil || p.Cost(Tokens{Input: 1e6, Output: 1e6}) != 0 {
		t.Fatalf("free: %+v", p)
	}
	if tb.Lookup("totally-unknown-model") != nil {
		t.Fatal("unknown should be nil")
	}
	p := &Price{Input: 2, Output: 10, Cached: 0.5, Reasoning: 20, CacheCreation: 3}
	got := p.Cost(Tokens{Input: 1000, CacheRead: 600, CacheWrite: 100, Output: 500, Reasoning: 200})
	want := (300*2 + 600*0.5 + 100*3 + 300*10 + 200*20) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost %v want %v", got, want)
	}
}
