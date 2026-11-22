package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nesaon03/fastrouter/internal/store"
)

// comboSnapshot builds a fake provider with two models plus optional decision
// engine and combo options.
func comboSnapshot(t *testing.T, upURL string, withTypesafe bool, combo store.Combo, opts string) *store.Snapshot {
	t.Helper()
	conns := []store.Connection{
		conn("a", "fk", "key-a", 1),
		conn("b", "fk", "key-b", 2),
	}
	if withTypesafe {
		conns = append(conns, conn("t1", "typesafe", "key-jev", 1))
	}
	return &store.Snapshot{
		Nodes:       []store.Node{node("fk", "fk", upURL)},
		Connections: conns,
		Combos:      []store.Combo{combo},
		Settings:    map[string]json.RawMessage{"comboStrategies": json.RawMessage(opts)},
	}
}

// answerModel echoes the requested model id so a test can tell which step ran.
func answerModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":"answer from %s"}}]}`, req.Model, req.Model)
}

// A smart combo with routerEngine=decision picks the model with a typed choice
// instead of a chat completion.
func TestSmartComboDecisionRouterPicksModel(t *testing.T) {
	var mu sync.Mutex
	var pickBody map[string]any
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", true,
		store.Combo{ID: "team", Name: "team", Strategy: "smart", Models: []string{"fk/a", "fk/b"}},
		`{"team":{"routerEngine":"decision","decisionModel":"jev"}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(b, &pickBody)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"answers":{"pick":{"type":"choice","choice":"fk/b","confidence":0.91}}}`)
			return
		}
		answerModel(w, r)
	})
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"write a fast regex"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "answer from b") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	mu.Lock()
	defer mu.Unlock()
	qs, _ := pickBody["questions"].(map[string]any)
	pick, _ := qs["pick"].(map[string]any)
	crit, _ := pick["criteria"].(map[string]any)
	if pick["type"] != "choice" || crit["fk/a"] == nil || crit["fk/b"] == nil {
		t.Fatalf("pick question %v", pickBody)
	}
}

// A decision router that fails keeps the listed order (fail open).
func TestSmartComboDecisionRouterFailsOpen(t *testing.T) {
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", true,
		store.Combo{ID: "team", Name: "team", Strategy: "smart", Models: []string{"fk/a", "fk/b"}},
		`{"team":{"routerEngine":"decision","decisionModel":"jev"}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		answerModel(w, r)
	})
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "answer from a") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
}

// Cascade sends the cheap answer when the decision engine accepts it.
func TestCascadeAcceptsCheapAnswer(t *testing.T) {
	var bCalls atomic.Int32
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", true,
		store.Combo{ID: "lad", Name: "lad", Strategy: "cascade", Models: []string{"fk/a", "fk/b"}},
		`{"lad":{"decisionModel":"jev"}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"answers":{"verdict":{"type":"choice","choice":"accept","confidence":0.95}}}`)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "fk/b" {
			bCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":"answer from %s"}}]}`, req.Model, req.Model)
	})
	resp, out := post(t, hs.URL, `{"model":"lad","messages":[{"role":"user","content":"capital of France?"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "answer from a") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if bCalls.Load() != 0 {
		t.Fatalf("escalated despite accept: %d calls to fk/b", bCalls.Load())
	}
	if got := resp.Header.Get("X-FastRouter-Model"); got != "fk/a" {
		t.Fatalf("model header %q", got)
	}
}

// Cascade escalates to the next model when the verdict says so.
func TestCascadeEscalates(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", true,
		store.Combo{ID: "lad", Name: "lad", Strategy: "cascade", Models: []string{"fk/a", "fk/b"}},
		`{"lad":{"decisionModel":"jev"}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"answers":{"verdict":{"type":"choice","choice":"escalate"}}}`)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "a" || req.Model == "fk/a" {
			aCalls.Add(1)
		} else {
			bCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":"answer from %s"}}]}`, req.Model, req.Model)
	})
	resp, out := post(t, hs.URL, `{"model":"lad","messages":[{"role":"user","content":"prove this theorem"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "answer from b") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("calls a=%d b=%d", aCalls.Load(), bCalls.Load())
	}
}

// Without a decision engine the first answer is sent (same as fallback).
func TestCascadeWithoutDecisionEngine(t *testing.T) {
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", false,
		store.Combo{ID: "lad", Name: "lad", Strategy: "cascade", Models: []string{"fk/a", "fk/b"}},
		`{"lad":{}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		answerModel(w, r)
	})
	resp, out := post(t, hs.URL, `{"model":"lad","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "answer from a") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
}

// The dashboard can create a cascade combo and switch a smart combo to the
// decision router; invalid values are refused.
func TestCascadeComboAdmin(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/combos", map[string]any{
		"name": "lad", "strategy": "cascade", "models": []string{"fk/a", "fk/b"}, "decisionModel": "jev",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	sn := s.state.Load().Snap
	var found bool
	for _, c := range sn.Combos {
		if c.Name == "lad" {
			found = c.Strategy == "cascade"
		}
	}
	if !found {
		t.Fatalf("cascade combo missing: %+v", sn.Combos)
	}
	if got := comboOptions(sn)["lad"]["decisionModel"]; got != "jev" {
		t.Fatalf("decisionModel %v", got)
	}

	rec, _ = adminCall(t, s, http.MethodPost, "/api/admin/combos", map[string]any{
		"name": "pick", "strategy": "smart", "models": []string{"fk/a", "fk/b"}, "routerEngine": "decision", "decisionModel": "jev",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("smart create: %d %s", rec.Code, rec.Body.String())
	}
	if got := comboOptions(s.state.Load().Snap)["pick"]["routerEngine"]; got != "decision" {
		t.Fatalf("routerEngine %v", got)
	}

	if rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/combos", map[string]any{
		"name": "bad", "strategy": "smart", "models": []string{"fk/a"}, "routerEngine": "magic",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid engine: %d, want 400", rec.Code)
	}
}

// A request with tools skips the buffered steps and goes straight to the last
// model, so a tool call can stream from the model that emits it.
func TestCascadeToolsGoStraightToStrongest(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", true,
		store.Combo{ID: "lad", Name: "lad", Strategy: "cascade", Models: []string{"fk/a", "fk/b"}},
		`{"lad":{"decisionModel":"jev"}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			t.Error("decision engine called for a tool request")
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "fk/a" {
			aCalls.Add(1)
		} else {
			bCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":"answer from %s"}}]}`, req.Model, req.Model)
	})
	resp, out := post(t, hs.URL, `{"model":"lad","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "answer from b") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if aCalls.Load() != 0 || bCalls.Load() != 1 {
		t.Fatalf("calls a=%d b=%d", aCalls.Load(), bCalls.Load())
	}
}

// The accepted answer is streamed when the client asked for a stream.
func TestCascadeStreamsAcceptedAnswer(t *testing.T) {
	snap := comboSnapshot(t, "http://127.0.0.1:1/unused", true,
		store.Combo{ID: "lad", Name: "lad", Strategy: "cascade", Models: []string{"fk/a", "fk/b"}},
		`{"lad":{"decisionModel":"jev"}}`)
	s, hs := newTestServer(t, snap)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"answers":{"verdict":{"type":"choice","choice":"accept"}}}`)
			return
		}
		answerModel(w, r)
	})
	resp, out := post(t, hs.URL, `{"model":"lad","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(out, "answer from a") {
		t.Fatalf("stream missing the answer: %s", out)
	}
}
