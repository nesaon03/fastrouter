package router

import (
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

func TestStickyRoundRobin(t *testing.T) {
	a, b, c := &Account{ID: "a"}, &Account{ID: "b"}, &Account{ID: "c"}
	p := &Pool{accounts: []*Account{a, b, c}, roundRobin: true, sticky: 2}
	var got string
	for range 6 {
		got += p.Order(time.Now())[0].ID
	}
	if got != "aabbcc" {
		t.Fatalf("order %s, want aabbcc", got)
	}
}

func TestCoolingAccountsGoLast(t *testing.T) {
	a, b := &Account{ID: "a"}, &Account{ID: "b"}
	p := &Pool{accounts: []*Account{a, b}}
	a.Fail(time.Minute, 0)
	if o := p.Order(time.Now()); o[0] != b || o[1] != a {
		t.Fatalf("cooling account not moved last")
	}
	a.Succeed()
	if o := p.Order(time.Now()); o[0] != a {
		t.Fatalf("recovered account not first")
	}
}

// L4: a reload keeps cooldowns and round-robin position.
func TestReloadCarriesOverHealth(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		{ID: "a", Provider: "deepseek", Active: true, Status: "ok", Priority: 1, Secrets: map[string]string{"apiKey": "k1"}},
		{ID: "b", Provider: "deepseek", Active: true, Status: "ok", Priority: 2, Secrets: map[string]string{"apiKey": "k2"}},
	}}
	old := Build(snap, nil)
	p, _ := old.Provider("ds")
	p.Pool.accounts[0].Fail(time.Minute, 0)
	next := Build(snap, nil)
	next.CarryOver(old)
	np, _ := next.Provider("ds")
	if !np.Pool.accounts[0].CoolingDown(time.Now()) || np.Pool.accounts[0].fails.Load() != 1 {
		t.Fatal("cooldown lost on reload")
	}
	if np.Pool.accounts[1].CoolingDown(time.Now()) {
		t.Fatal("healthy account should stay healthy")
	}
}
