package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/bench"
	"github.com/zenpo/fastrouter/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// speedUpstream answers OpenAI chat with text and usage, streaming or not,
// after a short delay so timings are measurable.
func speedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		time.Sleep(20 * time.Millisecond)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, tok := range []string{"one", ", two", ", three"} {
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", tok)
				w.(http.Flusher).Flush()
				time.Sleep(10 * time.Millisecond)
			}
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"one, two, three"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTimingTrailer(t *testing.T) {
	up := speedUpstream(t)
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{node("n1", "gw", up.URL)}, Connections: []store.Connection{conn("c1", "n1", "ok", 1)}})
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"gw/m","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
		req, _ := http.NewRequest("POST", hs.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+localKey)
		req.Header.Set("X-FastRouter-Timing", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		v := resp.Trailer.Get("X-FastRouter-Timing")
		if resp.StatusCode != 200 || !strings.Contains(v, "pre=") || !strings.Contains(v, "upttfb=") || !strings.Contains(v, "attempts=1") {
			t.Fatalf("stream=%v: %d trailer %q body %s", stream, resp.StatusCode, v, b)
		}
		var r bench.Result
		readTimingForTest(&r, v)
		if r.UpTTFBMs < 15 || r.PreMs <= 0 || r.PreMs > 50 {
			t.Fatalf("implausible timing %q", v)
		}
	}
	// Without the opt-in header nothing changes.
	req, _ := http.NewRequest("POST", hs.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gw/m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+localKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Trailer.Get("X-FastRouter-Timing") != "" || resp.Header.Get("Trailer") != "" {
		t.Fatal("timing sent without opt-in")
	}
}

func readTimingForTest(r *bench.Result, v string) {
	h := http.Header{}
	h.Set("X-FastRouter-Timing", v)
	*r = bench.ParseTimingHeader(h)
}

func TestSpeedTestTab(t *testing.T) {
	up := speedUpstream(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	const secretKey = "sk-fr-SPEEDTEST-0123456789abcdef"
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}, DashboardPassword: string(hash),
		Nodes:       []store.Node{node("n1", "gw", up.URL)},
		Connections: []store.Connection{conn("c1", "n1", "ok", 1)},
		APIKeys:     []store.APIKey{{ID: "k", Name: "bench key", Key: secretKey, Active: true}}}
	snap.Connections[0].Name = "someone@example.com"
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	s := New(testConfig(), snap, st)
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	s.bench.base = hs.URL
	hdr := adminSession(t, hs.URL, "pw")
	call := func(method, path, body string, want int) string {
		t.Helper()
		r, b := do(t, method, hs.URL+path, body, hdr)
		if r.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, r.StatusCode, b)
		}
		if strings.Contains(b, secretKey) || strings.Contains(b, "someone@example.com") {
			t.Fatalf("%s leaks the key or an email: %s", path, b)
		}
		return b
	}
	opts := call("GET", "/api/admin/bench/options", "", 200)
	if !strings.Contains(opts, `bench key`) || !strings.Contains(opts, `"id":"gemini-cli"`) || !strings.Contains(opts, `"providers":[{`) {
		t.Fatalf("options: %s", opts)
	}
	call("POST", "/api/admin/bench", `{"models":[]}`, 400)
	call("POST", "/api/admin/bench", `{"models":["gw/m"],"apiKey":"x"}`, 400) // the key is never client-supplied
	var start struct{ ID string }
	json.Unmarshal([]byte(call("POST", "/api/admin/bench", `{"models":["gw/m"],"clients":["cursor","claude","codex","gemini-cli"],"endpoints":["chat"],"stream":true,"runs":2}`, 200)), &start)
	var view struct {
		Status  string
		Total   int
		Done    int
		Results []bench.Result
		Summary []bench.Stat
	}
	deadline := time.Now().Add(15 * time.Second)
	for view.Status != "done" {
		if time.Now().After(deadline) {
			t.Fatalf("not done: %+v", view)
		}
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal([]byte(call("GET", "/api/admin/bench/"+start.ID, "", 200)), &view)
	}
	if view.Total != 10 || len(view.Results) != 10 || len(view.Summary) != 5 || view.Summary[3].Client != "gemini-cli" {
		t.Fatalf("view: %+v", view)
	}
	for _, r := range view.Results {
		if !r.OK || !r.Timed || r.TTFTMs <= 0 || r.TotalMs < r.TTFTMs || r.OutTokens == 0 || r.TokPerSec <= 0 || !strings.HasSuffix(r.Account, " · so***@example.com") {
			t.Fatalf("result: %+v", r)
		}
		if r.OverheadMs > r.TotalMs || r.UpTTFBMs <= 0 {
			t.Fatalf("overhead: %+v", r)
		}
	}
	// A failing model reports its class.
	json.Unmarshal([]byte(call("POST", "/api/admin/bench", `{"models":["nope/x"],"runs":1}`, 200)), &start)
	for view.Status = ""; view.Status != "done"; {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal([]byte(call("GET", "/api/admin/bench/"+start.ID, "", 200)), &view)
	}
	if r := view.Results[0]; r.OK || r.Status != 404 || r.Class != "model" {
		t.Fatalf("failure: %+v", r)
	}
	hist := call("GET", "/api/admin/bench/history", "", 200)
	if strings.Count(hist, `"source":"dashboard"`) != 2 {
		t.Fatalf("history: %s", hist)
	}
	csv := call("GET", "/api/admin/bench/all/export?format=csv", "", 200)
	if lines := strings.Count(csv, "\n"); lines != 12 || !strings.HasPrefix(csv, "run_id,") {
		t.Fatalf("csv (%d lines): %s", lines, csv)
	}
	js := call("GET", "/api/admin/bench/"+start.ID+"/export?format=json", "", 200)
	if !strings.Contains(js, `"results"`) {
		t.Fatalf("json export: %s", js)
	}
	call("DELETE", "/api/admin/bench/"+start.ID, "", 200)
	call("GET", "/api/admin/bench/"+start.ID, "", 404)
	call("DELETE", "/api/admin/bench/all", "", 200)
	if h := call("GET", "/api/admin/bench/history", "", 200); strings.Contains(h, `"id"`) {
		t.Fatalf("not cleared: %s", h)
	}
}
