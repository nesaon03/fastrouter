package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/config"
	"github.com/nesaon03/fastrouter/internal/store"
)

// modelFeed is a fake provider whose published list can change between polls.
type modelFeed struct {
	mu   sync.Mutex
	ids  []string
	hits []string
	code int // 0 = 200
}

func (f *modelFeed) set(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = ids
}

func (f *modelFeed) status(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
}

func (f *modelFeed) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

// newFeed starts an OpenAI-shaped model list.
func newFeed(t *testing.T, ids ...string) (*modelFeed, *httptest.Server) {
	t.Helper()
	f := &modelFeed{ids: ids}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits = append(f.hits, r.URL.Path)
		ids, code := append([]string(nil), f.ids...), f.code
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		if r.Header.Get("Authorization") == "" && r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// newDiscoveryServer builds a server whose only provider is a custom endpoint
// pointing at url, so discovery has a real HTTP target and a real account.
func newDiscoveryServer(t *testing.T, url string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	snap := &store.Snapshot{
		Nodes: []store.Node{{ID: "fake", Name: "Fake", Prefix: "fk", BaseURL: url, Type: "openai-compatible"}},
		Connections: []store.Connection{{
			ID: "c1", Provider: "fake", Name: "acct", AuthType: "apiKey",
			Active: true, Status: "ok", Secrets: map[string]string{"apiKey": "k1"},
		}},
	}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, loaded, st)
	// Discovery tests must not fetch the public rate list: pretend a harvest just
	// ran, which the TTL then treats as fresh. Pricing has its own tests.
	s.harvestAt = time.Now()
	return s
}

// A provider's list becomes part of /v1/models with no code change.
func TestRefreshDiscoveredAddsModels(t *testing.T) {
	f, up := newFeed(t, "alpha-model", "beta-model")
	s := newDiscoveryServer(t, up.URL)

	n, err := s.RefreshDiscovered(context.Background(), "fake")
	if err != nil {
		t.Fatalf("RefreshDiscovered: %v", err)
	}
	if n != 2 {
		t.Fatalf("new = %d, want 2", n)
	}
	st := s.state.Load()
	got := map[string]bool{}
	for _, m := range st.Models {
		got[m] = true
	}
	if !got["fk/alpha-model"] || !got["fk/beta-model"] {
		t.Fatalf("discovered models missing from %v", st.Models)
	}
	if !st.IsDiscovered("fake", "alpha-model") {
		t.Fatal("overlay not marked as discovered")
	}
	if len(f.asked()) == 0 {
		t.Fatal("no list request was made")
	}
}

// One bad poll keeps a model; two in a row drop it from /v1/models.
func TestRefreshDiscoveredTwoMissesDropModel(t *testing.T) {
	f, up := newFeed(t, "one", "two")
	s := newDiscoveryServer(t, up.URL)
	if _, err := s.RefreshDiscovered(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}

	f.set("one")
	if _, err := s.RefreshDiscovered(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	stillListed := false
	for _, m := range s.state.Load().Models {
		if m == "fk/two" {
			stillListed = true
		}
	}
	if !stillListed {
		t.Fatal("one miss should not drop a model yet")
	}

	if _, err := s.RefreshDiscovered(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.state.Load().Models {
		if m == "fk/two" {
			t.Fatalf("model survived two misses: %v", s.state.Load().Models)
		}
	}
}

// A rejected credential is skipped, with no error and no models.
func TestRefreshDiscoveredUnauthorized(t *testing.T) {
	f, up := newFeed(t, "x")
	f.status(http.StatusUnauthorized)
	s := newDiscoveryServer(t, up.URL)
	n, err := s.RefreshDiscovered(context.Background(), "fake")
	if err != nil {
		t.Fatalf("a rejected credential must not fail the run: %v", err)
	}
	if n != 0 {
		t.Fatalf("added %d from a 401", n)
	}
}

// A provider with no model list endpoint is passed over, not retried forever.
func TestRefreshDiscoveredNoEndpoint(t *testing.T) {
	// A plain server that 404s every path: no candidate answers.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()
	s := newDiscoveryServer(t, up.URL)
	if _, err := s.RefreshDiscovered(context.Background(), "fake"); err != nil {
		t.Fatalf("a 404-only provider must not fail the run: %v", err)
	}
	if rows, _ := s.st.DiscoveredFor("fake"); len(rows) != 0 {
		t.Fatalf("stored models from a 404: %+v", rows)
	}
}

// Discovery is opt-out.
func TestRunModelDiscoveryDisabled(t *testing.T) {
	f, up := newFeed(t, "x")
	s := newDiscoveryServer(t, up.URL)
	off := false
	s.cfgDiscovery.Enabled = &off
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.RunModelDiscovery(ctx) // returns immediately
	if len(f.asked()) != 0 {
		t.Fatalf("disabled discovery still polled: %v", f.asked())
	}
}

// A model marked dead comes back as soon as the provider lists it again. This
// is the repair path for the bug where a 404 from one account (its plan simply
// does not include the model) hid a model every other account served.
func TestRefreshDiscoveredRevivesDeadModel(t *testing.T) {
	_, up := newFeed(t, "revived-1")
	s := newDiscoveryServer(t, up.URL)
	if _, err := s.RefreshDiscovered(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if !hasAnyModel(s.state.Load().Models, "fk/revived-1") {
		t.Fatal("setup: model not listed")
	}
	// Two empty polls mark it dead and drop it from the list.
	for range 2 {
		if err := s.st.SaveDiscovered("fake", nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReloadFromStore(); err != nil {
		t.Fatal(err)
	}
	if hasAnyModel(s.state.Load().Models, "fk/revived-1") {
		t.Fatal("setup: dead model should be hidden")
	}
	// The provider lists it again: it must be advertised again.
	if _, err := s.RefreshDiscovered(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if !hasAnyModel(s.state.Load().Models, "fk/revived-1") {
		t.Fatalf("re-listed model stayed hidden: %v", s.state.Load().Models)
	}
}

// A provider whose list does not describe what its API accepts must not be
// polled: Codex answers /backend-api/models with ChatGPT product slugs
// (gpt-6, gpt-6-astra-wm) that its own responses endpoint rejects, so one poll
// would fill the picker with ids that fail on first use.
func TestRefreshDiscoveredSkipsCodex(t *testing.T) {
	_, srv := newFeed(t, "gpt-6-astra-wm")
	s := newDiscoveryServer(t, srv.URL)
	// Point a codex connection at the running table, with a token, so the codex
	// branch is what gets exercised.
	snap := s.state.Load().Snap
	snap.Connections = append(snap.Connections, store.Connection{
		ID: "cx", Provider: "codex", Name: "cx", AuthType: "oauth", Active: true, Status: "ok",
		Secrets: map[string]string{"accessToken": "tok"},
	})
	s.Reload(snap)

	n, err := s.RefreshDiscovered(context.Background(), "codex")
	if err != nil {
		t.Fatalf("codex must be skipped quietly, got %v", err)
	}
	if n != 0 {
		t.Fatalf("codex contributed %d models", n)
	}
	if rows, _ := s.st.DiscoveredFor("codex"); len(rows) != 0 {
		t.Fatalf("codex stored rows: %+v", rows)
	}
}

// The dashboard's combo form creates swarm combos, validates rounds, and reads
// them back so the form can be reopened.
func TestSwarmComboAdmin(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	rec, got := adminCall(t, s, http.MethodPost, "/api/admin/combos", map[string]any{
		"name": "swarmteam", "strategy": "swarm", "models": []string{"fake/a", "fake/b"},
		"judgeModel": "fake/a", "rounds": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	_ = got
	st := s.state.Load()
	c, ok := st.Combo("swarmteam")
	if !ok || c.Strategy != "swarm" || c.Rounds != 3 || c.JudgeModel != "fake/a" {
		t.Fatalf("stored combo: %+v", c)
	}
	// Rounds outside 1..3 are refused rather than clamped silently.
	if rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/combos", map[string]any{
		"name": "toomany", "strategy": "swarm", "models": []string{"fake/a", "fake/b"}, "rounds": 9,
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("rounds=9 should be refused, got %d", rec.Code)
	}
	// The state the form reads back carries rounds.
	_, state := adminCall(t, s, http.MethodGet, "/api/admin/state", nil)
	combos, _ := state["combos"].([]any)
	found := false
	for _, raw := range combos {
		cb, _ := raw.(map[string]any)
		if cb["name"] == "swarmteam" {
			found = true
			if n, _ := cb["rounds"].(float64); int(n) != 3 {
				t.Fatalf("state rounds = %v", cb["rounds"])
			}
		}
	}
	if !found {
		t.Fatal("swarm combo missing from admin state")
	}
}

// The default is a six-hour cadence, not zero (which would poll in a hot loop).
func TestDiscoveryEveryDefault(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	if got := s.discoveryEvery(); got != 6*time.Hour {
		t.Fatalf("every = %v, want 6h", got)
	}
	if !s.discoveryEnabled() {
		t.Fatal("discovery should default to on")
	}
}
