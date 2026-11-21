package store

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSaveDiscoveredUpsert(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.SaveDiscovered("deepseek", []DiscoveredModel{
		{Provider: "deepseek", Model: "m1"},
		{Provider: "deepseek", Model: "m2"},
	}, now); err != nil {
		t.Fatal(err)
	}
	// A second poll that lists m2 and a new m3 keeps m1 (one miss, not dead).
	if err := st.SaveDiscovered("deepseek", []DiscoveredModel{
		{Provider: "deepseek", Model: "m2"},
		{Provider: "deepseek", Model: "m3"},
	}, now); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DiscoveredFor("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DiscoveredModel{}
	for _, r := range rows {
		got[r.Model] = r
	}
	if len(got) != 3 {
		t.Fatalf("rows %v", got)
	}
	if got["m1"].Misses != 1 || got["m1"].Status == DiscoveredDead {
		t.Fatalf("m1 after one miss: %+v", got["m1"])
	}
	if got["m2"].Misses != 0 || got["m2"].Status != DiscoveredListed {
		t.Fatalf("m2 still listed: %+v", got["m2"])
	}
	// Two consecutive misses make a model dead.
	if err := st.SaveDiscovered("deepseek", []DiscoveredModel{
		{Provider: "deepseek", Model: "m2"},
		{Provider: "deepseek", Model: "m3"},
	}, now); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.DiscoveredFor("deepseek")
	for _, r := range rows {
		if r.Model == "m1" && r.Status != DiscoveredDead {
			t.Fatalf("m1 should be dead after two misses: %+v", r)
		}
	}
}

// A model that disappears twice is dropped; if the provider lists it again it
// comes back. Two misses is the buffer against one bad poll, and nothing else
// may leave a model permanently hidden (a per-account 404 does not mean the
// model is gone: plans differ, so one account may not have what another serves).
func TestSaveDiscoveredDropsAndReturns(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_ = st.SaveDiscovered("p", []DiscoveredModel{{Provider: "p", Model: "flappy"}}, now)
	_ = st.SaveDiscovered("p", nil, now) // miss 1
	_ = st.SaveDiscovered("p", nil, now) // miss 2 -> dead
	rows, _ := st.DiscoveredFor("p")
	if len(rows) != 1 || rows[0].Status != DiscoveredDead {
		t.Fatalf("should be dead after two misses: %+v", rows)
	}
	_ = st.SaveDiscovered("p", []DiscoveredModel{{Provider: "p", Model: "flappy"}}, now)
	rows, _ = st.DiscoveredFor("p")
	if rows[0].Status != DiscoveredListed || rows[0].Misses != 0 {
		t.Fatalf("a re-listed model must come back: %+v", rows[0])
	}
}

func TestSetDiscoveredStatusIgnoresUnknown(t *testing.T) {
	st := newTestStore(t)
	if err := st.SetDiscoveredStatus("nobody", "nothing", DiscoveredDead, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DiscoveredFor("nobody")
	if len(rows) != 0 {
		t.Fatalf("invented a row: %+v", rows)
	}
}

// The overlay must survive a config rewrite: it is not part of the import.
func TestDiscoveredSurvivesReplaceAll(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.SaveDiscovered("deepseek", []DiscoveredModel{{Provider: "deepseek", Model: "m1"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAll(&Snapshot{}); err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Discovered) != 1 || snap.Discovered[0].Model != "m1" {
		t.Fatalf("overlay lost in ReplaceAll: %+v", snap.Discovered)
	}
}

func TestLoadReadsDiscovered(t *testing.T) {
	st := newTestStore(t)
	if err := st.SaveDiscovered("p", []DiscoveredModel{{Provider: "p", Model: "m", Caps: `{"vision":true}`}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Discovered) != 1 || snap.Discovered[0].Caps != `{"vision":true}` {
		t.Fatalf("snapshot %+v", snap.Discovered)
	}
}

func TestClearDiscovered(t *testing.T) {
	st := newTestStore(t)
	_ = st.SaveDiscovered("p", []DiscoveredModel{{Provider: "p", Model: "m"}}, time.Now())
	if err := st.ClearDiscovered("p"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.DiscoveredFor("p"); len(rows) != 0 {
		t.Fatalf("not cleared: %+v", rows)
	}
	if err := st.ClearDiscovered(""); err == nil {
		t.Fatal("an empty provider must be refused")
	}
}
