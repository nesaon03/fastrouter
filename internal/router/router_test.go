package router

import (
	"testing"
	"time"
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
