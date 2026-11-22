package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/store"
)

func oaText(text string) func(http.ResponseWriter, *http.Request) {
	b, _ := json.Marshal(text)
	return jsonReply(200, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":`+string(b)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
}

// comboSnap builds three OpenAI-compatible endpoints (a, b, r) on one fake
// upstream; the account key tells the fake which endpoint was called.
func comboSnap(up, strategy string, opts string) *store.Snapshot {
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}}
	for _, p := range []string{"a", "b", "r"} {
		snap.Nodes = append(snap.Nodes, store.Node{ID: "n" + p, Name: p, Prefix: p, Type: "openai-compatible", BaseURL: up + "/v1"})
		snap.Connections = append(snap.Connections, store.Connection{ID: "c" + p, Provider: "n" + p, Name: p, AuthType: "apikey", Priority: 1,
			Active: true, Status: "ok", Secrets: map[string]string{"apiKey": "key-" + p}, Meta: map[string]any{}})
	}
	snap.Combos = []store.Combo{{ID: "c1", Name: "team", Strategy: strategy, Models: []string{"a/m1", "b/m2"}}}
	if opts != "" {
		snap.Settings["comboStrategies"] = json.RawMessage(`{"team":` + opts + `}`)
	}
	return snap
}

func TestSmartComboPicksAndSticks(t *testing.T) {
	sc, up := newScripted(t)
	var routed atomic.Int32
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		routed.Add(1)
		oaText(`Sure: {"choice": "b/m2", "confidence": 0.93, "probabilities": {"b/m2": 0.93, "a/m1": 0.07}}`)(w, r)
	}
	sc.reply["key-a"] = oaText("from a")
	sc.reply["key-b"] = oaText("from b")
	_, hs := newTestServer(t, comboSnap(up.URL, "smart", `{"routerModel":"r/router"}`))
	body := `{"model":"team","messages":[{"role":"user","content":"write a haiku"}]}`
	resp, out := post(t, hs.URL, body)
	if resp.StatusCode != 200 || !strings.Contains(out, "from b") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	// Same conversation: the pick sticks; the router is asked again (smart
	// policy) but a low-confidence answer does not move the session.
	sc.reply["key-r"] = oaText(`{"choice": "a/m1", "confidence": 0.4}`)
	if _, out := post(t, hs.URL, body); !strings.Contains(out, "from b") {
		t.Fatalf("sticky pick lost: %s", out)
	}
	if routed.Load() != 1 {
		t.Fatalf("router calls: %d", routed.Load())
	}
	// A broken router fails open to the listed order.
	sc.reply["key-r"] = jsonReply(500, `{"error":{"message":"down"}}`)
	if _, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"another chat"}]}`); !strings.Contains(out, "from a") {
		t.Fatalf("fail-open: %s", out)
	}
}

func TestSmartComboStrictSkipsRouter(t *testing.T) {
	sc, up := newScripted(t)
	var calls atomic.Int32
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		oaText(`{"choice":"b/m2","confidence":0.9}`)(w, r)
	}
	sc.reply["key-b"] = oaText("from b")
	_, hs := newTestServer(t, comboSnap(up.URL, "smart", `{"routerModel":"r/router","smartPolicy":"strict"}`))
	body := `{"model":"team","messages":[{"role":"user","content":"hi"}]}`
	post(t, hs.URL, body)
	post(t, hs.URL, body)
	if calls.Load() != 1 {
		t.Fatalf("strict policy should ask the router once, asked %d times", calls.Load())
	}
}

func TestFusionComboJudges(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("panel answer A")
	sc.reply["key-b"] = oaText("panel answer B")
	var judgeSaw string
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		judgeSaw = fmt.Sprint(sc.last().Body["messages"])
		oaText("final fused answer")(w, r)
	}
	_, hs := newTestServer(t, comboSnap(up.URL, "fusion", `{"judgeModel":"r/judge"}`))
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object"}}}]}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "final fused answer") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if !strings.Contains(judgeSaw, "panel answer A") || !strings.Contains(judgeSaw, "panel answer B") || !strings.Contains(judgeSaw, "JUDGE") {
		t.Fatalf("judge prompt: %s", judgeSaw)
	}
	for _, r := range sc.reqs {
		if r.Body["model"] != "judge" && r.Body["tools"] != nil {
			t.Fatal("panel calls must not carry tools")
		}
	}
}

func TestFusionComboDegrades(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = jsonReply(500, `{"error":{"message":"a down"}}`)
	sc.reply["key-b"] = oaText("only b")
	sc.reply["key-r"] = oaText("judge should not run")
	_, hs := newTestServer(t, comboSnap(up.URL, "fusion", `{"judgeModel":"r/judge"}`))
	start := time.Now()
	_, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	if !strings.Contains(out, "only b") || time.Since(start) > 5*time.Second {
		t.Fatalf("one answer should be returned directly: %s", out)
	}
	sc.reply["key-b"] = jsonReply(500, `{"error":{"message":"b down"}}`)
	if resp, _ := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != 503 {
		t.Fatalf("all failed: %d", resp.StatusCode)
	}
}
