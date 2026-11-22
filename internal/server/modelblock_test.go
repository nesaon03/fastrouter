package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

func TestModelUnsupported(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		want   bool
	}{
		{404, `{"error":{"message":"The model ` + "`gpt-x`" + ` does not exist"}}`, true},
		{404, `{"error":{"message":"model_not_found"}}`, true},
		{404, `{"error":{"message":"models/gemini-x is not found for API version v1beta"}}`, true},
		{404, `{"error":{"message":"Deployment not found"}}`, true},
		// Google Code Assist: no "model" word, but it is a model/account gap.
		{404, `{"error":{"message":"Requested entity was not found."}}`, true},
		{400, `{"error":{"message":"unsupported model: foo"}}`, true},
		{400, `{"error":{"message":"invalid model name"}}`, true},
		{422, `{"error":{"message":"no such model"}}`, true},
		{403, `{"error":{"message":"you do not have access to model claude-x"}}`, true},
		// Not model-scoped: keep the old account-cooldown behaviour.
		{404, `{"error":{"message":"not found"}}`, false},
		{404, ``, false},
		{400, `{"error":{"message":"messages: field required"}}`, false},
		{400, `{"error":{"message":"model is required"}}`, false},
		{403, `{"error":{"message":"organization disabled"}}`, false},
		{429, `{"error":{"message":"model rate limited"}}`, false},
		{500, `{"error":{"message":"model crashed"}}`, false},
	}
	for _, c := range cases {
		if got := modelUnsupported(c.status, c.msg); got != c.want {
			t.Errorf("modelUnsupported(%d, %q) = %v, want %v", c.status, c.msg, got, c.want)
		}
	}
}

func TestModelBlockExpiresAndClears(t *testing.T) {
	var b modelBlock
	now := time.Now()
	if b.blocked("a", "m", now) {
		t.Fatal("empty cache reported a block")
	}
	b.block("a", "m", now)
	if !b.blocked("a", "m", now) {
		t.Fatal("blocked pair not reported")
	}
	if b.blocked("a", "other", now) || b.blocked("b", "m", now) {
		t.Fatal("block leaked to another account or model")
	}
	if !b.blocked("a", "m", now.Add(modelBlockTTL-time.Minute)) {
		t.Fatal("block expired too early")
	}
	if b.blocked("a", "m", now.Add(modelBlockTTL+time.Minute)) {
		t.Fatal("block did not expire")
	}
	b.block("a", "m", now)
	b.clear("a", "m")
	if b.blocked("a", "m", now) {
		t.Fatal("clear did not remove the block")
	}
}

func TestModelBlockCap(t *testing.T) {
	var b modelBlock
	now := time.Now()
	for i := 0; i < modelBlockMax+10; i++ {
		b.block("acct"+strconv.Itoa(i), "model"+strconv.Itoa(i), now)
	}
	b.mu.Lock()
	n := len(b.until)
	b.mu.Unlock()
	if n > modelBlockMax {
		t.Fatalf("cache grew past the cap: %d", n)
	}
}

// writeOK is the plain chat answer used by the gap tests.
func writeOK(w http.ResponseWriter, model, key string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"model":`+jsonQuote(model)+`,"key":`+jsonQuote(key)+`,"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A 404 that names the model blocks that account for that model only: the call
// fails over immediately, the next call skips the account, and its other models
// keep working (no account-wide cooldown).
func TestModelGapSkipsOnlyThatPair(t *testing.T) {
	var aHits, bHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if key == "key-a" {
			aHits.Add(1)
			if req.Model == "missing-model" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"The model `+"`missing-model`"+` does not exist"}}`)
				return
			}
			writeOK(w, req.Model, key)
			return
		}
		bHits.Add(1)
		writeOK(w, req.Model, key)
	}))
	defer up.Close()

	snap := &store.Snapshot{
		Nodes: []store.Node{node("fk", "fk", up.URL)},
		Connections: []store.Connection{
			conn("a", "fk", "key-a", 1),
			conn("b", "fk", "key-b", 2),
		},
	}
	_, hs := newTestServer(t, snap)
	body := func(model string) string {
		return `{"model":"fk/` + model + `","messages":[{"role":"user","content":"hi"}]}`
	}

	// First call: account A says the model is missing, B answers.
	resp, out := post(t, hs.URL, body("missing-model"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "key-b") {
		t.Fatalf("first call: %d %s", resp.StatusCode, out)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("hits a=%d b=%d, want 1/1", aHits.Load(), bHits.Load())
	}

	// Second call: A is skipped for this model, so it is not asked again.
	resp, out = post(t, hs.URL, body("missing-model"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "key-b") {
		t.Fatalf("second call: %d %s", resp.StatusCode, out)
	}
	if aHits.Load() != 1 {
		t.Fatalf("account A was asked again for the missing model: %d hits", aHits.Load())
	}

	// Another model still uses A: the gap is not an account cooldown.
	resp, out = post(t, hs.URL, body("other-model"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "key-a") {
		t.Fatalf("other model: %d %s", resp.StatusCode, out)
	}
}

// A 404 that does not name a model keeps the old behaviour: the account cools
// down (so the next call goes straight to the other account) and no per-model
// block is recorded.
func TestGeneric404StillCoolsTheAccount(t *testing.T) {
	var aHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "key-a" {
			aHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"not found"}}`)
			return
		}
		writeOK(w, "any", key)
	}))
	defer up.Close()

	snap := &store.Snapshot{
		Nodes: []store.Node{node("fk", "fk", up.URL)},
		Connections: []store.Connection{
			conn("a", "fk", "key-a", 1),
			conn("b", "fk", "key-b", 2),
		},
	}
	s, hs := newTestServer(t, snap)
	post(t, hs.URL, `{"model":"fk/whatever","messages":[{"role":"user","content":"hi"}]}`)
	post(t, hs.URL, `{"model":"fk/another","messages":[{"role":"user","content":"hi"}]}`)
	if aHits.Load() != 1 {
		t.Fatalf("generic 404 did not cool the account: %d hits", aHits.Load())
	}
	if s.blocks.blocked("a", "whatever", time.Now()) || s.blocks.blocked("a", "another", time.Now()) {
		t.Fatal("generic 404 must not record a model gap")
	}
}

// Antigravity's "Requested entity was not found." is a model gap, not a broken
// account: the account is skipped for that model after the first 404.
func TestModelGapGoogleEntity(t *testing.T) {
	var aHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "key-a" {
			aHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"Requested entity was not found."}}`)
			return
		}
		writeOK(w, "any", key)
	}))
	defer up.Close()
	snap := &store.Snapshot{
		Nodes: []store.Node{node("fk", "fk", up.URL)},
		Connections: []store.Connection{
			conn("a", "fk", "key-a", 1),
			conn("b", "fk", "key-b", 2),
		},
	}
	_, hs := newTestServer(t, snap)
	body := `{"model":"fk/ag-model","messages":[{"role":"user","content":"hi"}]}`
	if resp, out := post(t, hs.URL, body); resp.StatusCode != http.StatusOK || !strings.Contains(out, "key-b") {
		t.Fatalf("first call: %d %s", resp.StatusCode, out)
	}
	if resp, out := post(t, hs.URL, body); resp.StatusCode != http.StatusOK || !strings.Contains(out, "key-b") {
		t.Fatalf("second call: %d %s", resp.StatusCode, out)
	}
	if aHits.Load() != 1 {
		t.Fatalf("Google entity-404 account was retried: %d hits", aHits.Load())
	}
}
