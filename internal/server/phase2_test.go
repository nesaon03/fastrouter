package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/store"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "translate", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seen is what the scripted upstream received.
type seen struct {
	Path, Auth, XAPIKey, GoogKey string
	Body                         map[string]any
}

// scripted is a fake upstream whose answer is chosen by the account key.
type scripted struct {
	mu    sync.Mutex
	reqs  []seen
	hits  atomic.Int32
	reply map[string]func(w http.ResponseWriter, r *http.Request)
}

func newScripted(t *testing.T) (*scripted, *httptest.Server) {
	sc := &scripted{reply: map[string]func(http.ResponseWriter, *http.Request){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		key := firstNonEmpty(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			firstNonEmpty(r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key")))
		sc.mu.Lock()
		sc.reqs = append(sc.reqs, seen{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), XAPIKey: r.Header.Get("x-api-key"),
			GoogKey: r.Header.Get("x-goog-api-key"), Body: body})
		fn := sc.reply[key]
		sc.mu.Unlock()
		if fn == nil {
			http.Error(w, `{"error":{"message":"unknown key"}}`, 500)
			return
		}
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	return sc, srv
}

func (sc *scripted) last() seen {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.reqs[len(sc.reqs)-1]
}

func sse(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func jsonReply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func anthropicNode(id, prefix, base string) store.Node {
	return store.Node{ID: id, Name: id, Prefix: prefix, Type: "anthropic-compatible", BaseURL: base}
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+localKey)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
}

// decodeClient parses a client-side answer back into the shared form.
func decodeClient(t *testing.T, f tr.Format, body string, stream bool) *tr.Response {
	t.Helper()
	if !stream {
		r, err := tr.ParseResponse(f, []byte(body))
		if err != nil {
			t.Fatalf("parse %v response: %v\n%s", f, err, body)
		}
		return r
	}
	fr := tr.NewFrameReader(strings.NewReader(body))
	dec := tr.NewDecoder(f)
	var c tr.Collector
	for {
		fm, err := fr.Next()
		if err != nil {
			break
		}
		evs, err := dec.Decode(fm)
		if err != nil {
			t.Fatalf("decode %v frame: %v", f, err)
		}
		c.Add(evs)
	}
	c.Add(dec.End())
	return c.Response()
}

func checkFixtureAnswer(t *testing.T, r *tr.Response) {
	t.Helper()
	checkAnswer(t, r, true)
}

func checkAnswer(t *testing.T, r *tr.Response, wantUsage bool) {
	t.Helper()
	if !wantUsage {
		r.Usage = tr.Usage{Input: 120, Output: 45}
	}
	var tools, text int
	for _, p := range r.Parts {
		switch p.Type {
		case tr.PartToolCall:
			tools++
		case tr.PartText:
			text++
		}
	}
	if tools != 2 || text == 0 || r.Finish != tr.FinishToolCalls || r.Usage.Input != 120 || r.Usage.Output != 45 {
		t.Fatalf("translated answer lost data: tools=%d text=%d finish=%v usage=%+v", tools, text, r.Finish, r.Usage)
	}
}

const weatherReq = `"messages":[{"role":"user","content":"weather in Paris and Tokyo?"}],"max_tokens":512`

// Every client format against an OpenAI-chat upstream and an Anthropic
// upstream, streamed and not, must come back complete in the client's format.
func TestTranslatedPaths(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["oa-stream"] = sse(fixture(t, "stream.openai.sse"))
	sc.reply["oa-json"] = jsonReply(200, fixture(t, "response.openai.json"))
	sc.reply["an-stream"] = sse(fixture(t, "stream.anthropic.sse"))
	sc.reply["an-json"] = jsonReply(200, fixture(t, "response.anthropic.json"))
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes: []store.Node{node("oas", "oas", up.URL+"/v1"), node("oaj", "oaj", up.URL+"/v1"),
			anthropicNode("ans", "ans", up.URL+"/v1"), anthropicNode("anj", "anj", up.URL+"/v1")},
		Connections: []store.Connection{conn("c1", "oas", "oa-stream", 1), conn("c2", "oaj", "oa-json", 1),
			conn("c3", "ans", "an-stream", 1), conn("c4", "anj", "an-json", 1)},
	})
	clients := []struct {
		f    tr.Format
		path func(model string, stream bool) string
		body func(model string, stream bool) string
	}{
		{tr.OpenAI, func(string, bool) string { return "/v1/chat/completions" },
			func(m string, s bool) string {
				return `{"model":"` + m + `","stream":` + boolStr(s) + `,` + weatherReq + `}`
			}},
		{tr.Anthropic, func(string, bool) string { return "/v1/messages" },
			func(m string, s bool) string {
				return `{"model":"` + m + `","stream":` + boolStr(s) + `,` + weatherReq + `}`
			}},
		{tr.Responses, func(string, bool) string { return "/v1/responses" },
			func(m string, s bool) string {
				return `{"model":"` + m + `","stream":` + boolStr(s) + `,"input":"weather in Paris and Tokyo?","max_output_tokens":512}`
			}},
		{tr.Gemini, func(m string, s bool) string {
			if s {
				return "/v1beta/models/" + m + ":streamGenerateContent?alt=sse"
			}
			return "/v1beta/models/" + m + ":generateContent"
		}, func(string, bool) string {
			return `{"contents":[{"role":"user","parts":[{"text":"weather in Paris and Tokyo?"}]}],"generationConfig":{"maxOutputTokens":512}}`
		}},
	}
	for _, c := range clients {
		for _, up := range []struct {
			prefix string
			uf     tr.Format
		}{{"oas", tr.OpenAI}, {"oaj", tr.OpenAI}, {"ans", tr.Anthropic}, {"anj", tr.Anthropic}} {
			for _, stream := range []bool{false, true} {
				name := c.f.String() + "<-" + up.prefix + map[bool]string{true: "/stream", false: "/json"}[stream]
				t.Run(name, func(t *testing.T) {
					model := up.prefix + "/m"
					resp, body := do(t, "POST", hs.URL+c.path(model, stream), c.body(model, stream), nil)
					if resp.StatusCode != 200 {
						t.Fatalf("status %d: %s", resp.StatusCode, body)
					}
					wantCT := map[bool]string{true: "text/event-stream", false: "application/json"}[stream]
					if !strings.HasPrefix(resp.Header.Get("Content-Type"), wantCT) {
						t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
					}
					if (c.f != up.uf) != (resp.Header.Get("X-FastRouter-Translated") != "") {
						t.Fatalf("translated header %q", resp.Header.Get("X-FastRouter-Translated"))
					}
					// OpenAI clients only get stream usage when they ask for it (stream_options)
					checkAnswer(t, decodeClient(t, c.f, body, stream), !(c.f == tr.OpenAI && up.uf == tr.OpenAI && stream))
					got := sc.last()
					if got.Body["model"] != "m" && up.uf != tr.Gemini {
						t.Fatalf("upstream model %v", got.Body["model"])
					}
					if up.uf == tr.Anthropic && (got.Path != "/v1/messages" || got.XAPIKey == "") {
						t.Fatalf("anthropic upstream call %+v", got)
					}
					if up.uf == tr.OpenAI && (got.Path != "/v1/chat/completions" || got.Auth == "") {
						t.Fatalf("openai upstream call %+v", got)
					}
					if up.uf == tr.OpenAI && stream && c.f == tr.OpenAI && got.Body["stream_options"] == nil {
						t.Fatal("include_usage not requested on passthrough stream")
					}
				})
			}
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestResponsesUpstreamAndGeminiQueryKey(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["rs"] = sse(fixture(t, "stream.responses.sse"))
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{{ID: "rn", Name: "rn", Prefix: "rn", Type: "openai-compatible", APIType: "responses", BaseURL: up.URL + "/v1"}},
		Connections: []store.Connection{conn("c1", "rn", "rs", 1)},
	})
	req, _ := http.NewRequest("POST", hs.URL+"/v1beta/models/rn/m:streamGenerateContent?alt=sse&key="+localKey,
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	checkFixtureAnswer(t, decodeClient(t, tr.Gemini, string(b), true))
	if got := sc.last(); got.Path != "/v1/responses" || got.Body["input"] == nil {
		t.Fatalf("responses upstream call %+v", got)
	}
}

func TestEmptyResponseFailover(t *testing.T) {
	sc, up := newScripted(t)
	empty := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	filtered := strings.Replace(empty, `"stop"`, `"content_filter"`, 1)
	sc.reply["empty"] = sse(empty)
	sc.reply["emptyjson"] = jsonReply(200, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	sc.reply["filtered"] = sse(filtered)
	sc.reply["good"] = sse(fixture(t, "stream.openai.sse"))
	sc.reply["goodjson"] = jsonReply(200, fixture(t, "response.openai.json"))
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes: []store.Node{node("e", "e", up.URL+"/v1"), node("j", "j", up.URL+"/v1"), node("f", "f", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "e", "empty", 1), conn("a2", "e", "good", 2),
			conn("b1", "j", "emptyjson", 1), conn("b2", "j", "goodjson", 2),
			conn("f1", "f", "filtered", 1), conn("f2", "f", "good", 2)},
		Settings: map[string]json.RawMessage{},
	})
	for _, tc := range []struct{ model, wantAcct string }{{"e/m", "a2"}, {"f/m", "f1"}} {
		resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"`+tc.model+`","stream":true,`+weatherReq+`}`, nil)
		if got := resp.Header.Get("X-FastRouter-Account"); got != tc.wantAcct {
			t.Fatalf("%s: answered by %q, want %q\n%s", tc.model, got, tc.wantAcct, body)
		}
	}
	resp, body := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"j/m",`+weatherReq+`}`, nil)
	if resp.Header.Get("X-FastRouter-Account") != "b2" {
		t.Fatalf("non-stream empty answer not failed over: %s", body)
	}
}

func TestClaudeCodeWarmupBypass(t *testing.T) {
	sc, up := newScripted(t)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n", "n", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n", "k", 1)},
	})
	cc := map[string]string{"User-Agent": "claude-cli/2.1.0 (external, cli)"}
	for _, body := range []string{
		`{"model":"n/m","max_tokens":1,"messages":[{"role":"user","content":"Warmup"}]}`,
		`{"model":"n/m","max_tokens":1,"messages":[{"role":"user","content":"count"}]}`,
		`{"model":"n/m","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"Please write a 5-10 word title for the following conversation: hi"}]}`,
	} {
		resp, out := do(t, "POST", hs.URL+"/v1/messages", body, cc)
		if resp.StatusCode != 200 || resp.Header.Get("X-FastRouter-Bypass") == "" {
			t.Fatalf("not bypassed: %d %s", resp.StatusCode, out)
		}
		mustContain(t, out, bypassText)
	}
	// Same body without the claude-cli user agent goes upstream.
	_, _ = do(t, "POST", hs.URL+"/v1/messages", `{"model":"n/m","max_tokens":1,"messages":[{"role":"user","content":"Warmup"}]}`, nil)
	if sc.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1", sc.hits.Load())
	}
}

func TestVisionFallbackAndProviderThinking(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = jsonReply(200, fixture(t, "response.openai.json"))
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("txt", "txt", up.URL+"/v1"), node("vis", "vis", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "txt", "k", 1), conn("b", "vis", "k", 1)},
		Settings: map[string]json.RawMessage{
			"capacityAdapter":  json.RawMessage(`{"vision":{"enabled":true,"roundRobin":false,"models":["vis/mimo-v2.5-pro"]}}`),
			"providerThinking": json.RawMessage(`{"txt":{"mode":"max"},"vis":{"mode":"on"}}`),
		},
	})
	img := `{"model":"txt/deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	resp, _ := do(t, "POST", hs.URL+"/v1/chat/completions", img, nil)
	if resp.Header.Get("X-FastRouter-Model") != "vis/mimo-v2.5-pro" {
		t.Fatalf("image request served by %q, want the vision pool", resp.Header.Get("X-FastRouter-Model"))
	}
	if th, _ := sc.last().Body["thinking"].(map[string]any); th["budget_tokens"] != float64(10000) {
		t.Fatalf("mode on: thinking %v", sc.last().Body["thinking"])
	}
	// Text-only requests stay on the asked model and get its thinking mode.
	resp, _ = do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"txt/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.Header.Get("X-FastRouter-Model") != "txt/deepseek-v4-flash" || sc.last().Body["reasoning_effort"] != "max" {
		t.Fatalf("thinking mode not injected: %v", sc.last().Body)
	}
	// A client's own setting wins.
	_, _ = do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"txt/m","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, nil)
	if sc.last().Body["reasoning_effort"] != "low" {
		t.Fatalf("client reasoning_effort overridden: %v", sc.last().Body)
	}
	// A vision-capable model does not trigger the fallback.
	resp, _ = do(t, "POST", hs.URL+"/v1/chat/completions", strings.Replace(img, "txt/deepseek-v4-flash", "txt/qwen3-vl-plus", 1), nil)
	if resp.Header.Get("X-FastRouter-Model") != "txt/qwen3-vl-plus" {
		t.Fatalf("vision model rerouted to %q", resp.Header.Get("X-FastRouter-Model"))
	}
}

func TestKeyLimits(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = jsonReply(200, fixture(t, "response.openai.json"))
	snap := &store.Snapshot{
		Nodes:       []store.Node{node("n", "n", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n", "k", 1)},
		Settings:    map[string]json.RawMessage{},
		APIKeys: []store.APIKey{
			{ID: "r", Name: "restricted", Key: "restricted-key", Active: true, Access: json.RawMessage(`{"restricted":true,"allow":["N/Allowed"]}`)},
			{ID: "l", Name: "limited", Key: "limited-key", Active: true},
		},
	}
	cfg := testConfig()
	cfg.KeyLimits = map[string]config.KeyLimit{"limited": {RPM: 2, Models: []string{"n/*"}}}
	s := New(cfg, snap, nil)
	hs := httptest.NewServer(s.Handler())
	defer func() { hs.Close(); s.Close() }()
	call := func(key, model string) int {
		resp, _ := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"Authorization": "Bearer " + key})
		return resp.StatusCode
	}
	if c := call("restricted-key", "n/allowed"); c != 200 {
		t.Fatalf("allowed model: %d", c)
	}
	if c := call("restricted-key", "n/other"); c != 403 {
		t.Fatalf("disallowed model: %d", c)
	}
	if c := call("limited-key", "x/m"); c != 403 {
		t.Fatalf("model outside glob: %d", c)
	}
	if a, b, c := call("limited-key", "n/m"), call("limited-key", "n/m"), call("limited-key", "n/m"); a != 200 || b != 200 || c != 429 {
		t.Fatalf("rpm 2: %d %d %d", a, b, c)
	}
}

func TestUsageTrackingAndCountTokens(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = sse(fixture(t, "stream.openai.sse"))
	sc.reply["ak"] = sse(fixture(t, "stream.anthropic.sse"))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap := &store.Snapshot{
		Nodes:       []store.Node{node("n", "n", up.URL+"/v1"), anthropicNode("an", "an", up.URL+"/v1")},
		Connections: []store.Connection{conn("acct-1", "n", "k", 1), conn("acct-2", "an", "ak", 1)},
		Settings:    map[string]json.RawMessage{},
		APIKeys:     []store.APIKey{{ID: "key-1", Name: "k", Key: localKey, Active: true}},
	}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	s := New(testConfig(), snap, st)
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"n/deepseek-v4-flash","stream":true,`+weatherReq+`}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// passthrough stream: usage still captured from the frames that carry it
	if resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"an/claude-sonnet-4-6","stream":true,`+weatherReq+`}`, nil); resp.StatusCode != 200 ||
		resp.Header.Get("X-FastRouter-Translated") != "" {
		t.Fatalf("passthrough: %d %s", resp.StatusCode, body)
	}
	_, ct := do(t, "POST", hs.URL+"/v1/messages/count_tokens", `{"model":"n/m","messages":[{"role":"user","content":"hello there, how are you?"}]}`, nil)
	mustContain(t, ct, `"input_tokens":`)
	s.Close() // flush the background writer
	time.Sleep(10 * time.Millisecond)
	rows, err := st.UsageReport(time.Time{}, "account")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	for _, r := range rows {
		if r.Input != 120 || r.Output != 45 || r.Cost <= 0 {
			t.Fatalf("usage row %+v", r)
		}
	}
	for _, r := range rows {
		// the OpenAI fixture reports 20 reasoning tokens; Anthropic streams do not report them
		if r.CacheRead != 100 || (r.Key == "acct-1" && r.Reasoning != 20) {
			t.Fatalf("usage row %+v", r)
		}
	}
	keys, _ := st.UsageReport(time.Time{}, "key")
	if len(keys) != 1 || keys[0].Key != "key-1" || keys[0].Requests != 2 {
		t.Fatalf("by key %+v", keys)
	}
	_, ub := do(t, "GET", hs.URL+"/api/usage?window=all", "", nil)
	mustContain(t, ub, `"accounts"`, `"models"`, `"n/deepseek-v4-flash"`, `"input_tokens":120`)
}

func TestTranslatedUpstreamErrorUsesClientShape(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = jsonReply(400, `{"error":{"message":"context too long","type":"invalid_request_error"}}`)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n", "n", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n", "k", 1)},
	})
	resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"n/m",`+weatherReq+`}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mustContain(t, body, `"type":"error"`, "context too long")
	_, body = do(t, "POST", hs.URL+"/v1beta/models/n/m:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`, nil)
	mustContain(t, body, `"status":"INVALID_ARGUMENT"`, "context too long")
}

func TestInjectedUsageChunkIsHidden(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = sse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n", "n", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n", "k", 1)},
	})
	_, body := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"n/m","stream":true,"messages":[]}`, nil)
	if strings.Contains(body, "usage") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("client saw the injected usage chunk:\n%s", body)
	}
	_, body = do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"n/m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, nil)
	mustContain(t, body, `"usage"`)
}

func TestInjectTopLevel(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`: `{"k":true,"a":1}`, `{}`: `{"k":true}`, ` { } `: ` {"k":true } `, `[]`: `[]`,
	} {
		if got := string(injectTopLevel([]byte(in), "k", "true")); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
