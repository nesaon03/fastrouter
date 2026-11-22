package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/store"
)

type hit struct {
	Host, Path, Auth, XAPIKey, UA string
	Header                        http.Header
	Body                          map[string]any
}

// fakeUpstreams sends every outgoing request of s to one handler (no network).
func fakeUpstreams(t *testing.T, s *Server, fn func(w http.ResponseWriter, r *http.Request)) *[]hit {
	var mu sync.Mutex
	hits := &[]hit{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		*hits = append(*hits, hit{r.Header.Get("X-Orig-Host"), r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("User-Agent"), r.Header.Clone(), body})
		mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	s.client.Transport = rt(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.Header.Set("X-Orig-Host", r.URL.Host)
		r2.URL.Scheme, r2.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	return hits
}

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAntigravityRequestShaping(t *testing.T) {
	c := store.Connection{ID: "ag1", Provider: "antigravity", Active: true, Status: "needs-plugin", Priority: 1,
		Secrets: map[string]string{"accessToken": "ya29.AG"}, Meta: map[string]any{"projectId": "proj-9", "email": "a@x.io"}}
	s, hs := newTestServer(t, &store.Snapshot{Connections: []store.Connection{c}})
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}}`+"\n\n")
	})
	resp, body := post(t, hs.URL, `{"model":"ag/gemini-4-carbon-high","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"Say OK"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"OK"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	h := (*hits)[0]
	if h.Host != "daily-cloudcode-pa.googleapis.com" || h.Path != "/v1internal:streamGenerateContent" || h.Auth != "Bearer ya29.AG" || h.UA != "antigravity/hub/2.24.0 darwin/arm64" {
		t.Fatalf("upstream %+v", h)
	}
	if h.Header.Get("x-goog-api-key") != "" || h.Body["project"] != "proj-9" || h.Body["model"] != "gemini-4-carbon-high" || h.Body["request"] == nil {
		t.Fatalf("body %v", h.Body)
	}
	// Non-streaming client: the SSE answer is folded into JSON.
	resp, body = post(t, hs.URL, `{"model":"ag/gemini-4-carbon-high","max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"content":"OK"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestClaudeCodeAndCopilotShaping(t *testing.T) {
	conns := []store.Connection{
		{ID: "cc", Provider: "claude", Active: true, Status: "ok", Priority: 1, Secrets: map[string]string{"accessToken": "sk-ant-oat01"}, Meta: map[string]any{}},
		{ID: "gh", Provider: "github", Active: true, Status: "ok", Priority: 1, Secrets: map[string]string{"accessToken": "gho", "copilotToken": "tid=1"}, Meta: map[string]any{}},
	}
	s, hs := newTestServer(t, &store.Snapshot{Connections: conns})
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "messages") {
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	})
	if resp, body := post(t, hs.URL, `{"model":"cc/claude-sonnet-5","max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	h := (*hits)[0]
	sys, _ := json.Marshal(h.Body["system"])
	if h.Host != "api.anthropic.com" || h.Auth != "Bearer sk-ant-oat01" || h.XAPIKey != "" || !strings.Contains(string(sys), "You are Claude Code") ||
		!strings.HasPrefix(h.UA, "claude-cli/") {
		t.Fatalf("claude upstream %+v", h)
	}
	if resp, body := post(t, hs.URL, `{"model":"gh/claude-sonnet-5","max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	h = (*hits)[1]
	if h.Host != "api.githubcopilot.com" || h.Path != "/v1/messages" || h.Auth != "Bearer tid=1" || h.Header.Get("Copilot-Integration-Id") != "vscode-chat" {
		t.Fatalf("copilot upstream %+v", h)
	}
	if resp, _ := post(t, hs.URL, `{"model":"gh/gpt-5.4","max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`); resp.StatusCode != 200 || (*hits)[2].Path != "/chat/completions" {
		t.Fatalf("copilot chat %d %+v", resp.StatusCode, (*hits)[2])
	}
	if _, ok := (*hits)[2].Body["max_completion_tokens"]; !ok {
		t.Fatalf("gpt-5 on Copilot needs max_completion_tokens: %v", (*hits)[2].Body)
	}
}

func TestOAuthDeviceAndCallbackFlows(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}, APIKeys: []store.APIKey{{ID: "k", Name: "k", Key: localKey, Active: true}}}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	s := New(testConfig(), snap, st)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); s.Close() })
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/device-auth/codes":
			_, _ = io.WriteString(w, `{"code":"K9","verificationUrl":"https://app.kilo.ai/device?code=K9","expiresIn":600}`)
		case "/api/device-auth/codes/K9":
			_, _ = io.WriteString(w, `{"status":"approved","token":"kilo-tok","userEmail":"kilo@x.io"}`)
		case "/api/profile":
			_, _ = io.WriteString(w, `{"organizations":[]}`)
		case "/v1/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"sk-ant-oat-new","refresh_token":"r","expires_in":28800}`)
		case "/api/oauth/profile":
			_, _ = io.WriteString(w, `{"account":{"email":"claude@x.io"}}`)
		}
	})
	hdr := adminSession(t, hs.URL, "")
	start := func(prov string) map[string]any {
		resp, body := do(t, "POST", hs.URL+"/api/login/"+prov+"/start", `{}`, hdr)
		var v map[string]any
		_ = json.Unmarshal([]byte(body), &v)
		if resp.StatusCode != 200 {
			t.Fatalf("start %s: %d %s", prov, resp.StatusCode, body)
		}
		return v
	}
	f := start("kilocode")
	if f["kind"] != "device" || f["userCode"] != "K9" {
		t.Fatalf("%v", f)
	}
	deadline := time.Now().Add(8 * time.Second)
	for flow := s.logins.get(f["id"].(string)); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if v := flow.view(); v["status"] != "pending" {
			if v["status"] != "done" {
				t.Fatalf("%v", v)
			}
			break
		}
	}
	f = start("claude")
	u, _ := url.Parse(f["url"].(string))
	state := u.Query().Get("state")
	if u.Query().Get("redirect_uri") != "http://localhost:"+u2port(hs.URL)+"/callback" {
		t.Fatalf("redirect %s", u.Query().Get("redirect_uri"))
	}
	resp, err := http.Get(hs.URL + "/callback?code=C1&state=" + state)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("callback %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if resp, _ := http.Get(hs.URL + "/callback?code=C1&state=wrong"); resp.StatusCode != 400 {
		t.Fatal("unknown state must fail")
	}
	snap, _ = st.Load()
	got := map[string]store.Connection{}
	for _, c := range snap.Connections {
		got[c.Provider] = c
	}
	if got["kilocode"].Secrets["accessToken"] != "kilo-tok" || got["claude"].Secrets["accessToken"] != "sk-ant-oat-new" || got["claude"].Meta["origin"] != "fastrouter" {
		t.Fatalf("saved %+v", got)
	}
	if p, ok := s.state.Load().Provider("claude"); !ok || p.Pool.Size() != 1 {
		t.Fatal("claude account not routable after sign-in")
	}
}

func u2port(u string) string { return u[strings.LastIndex(u, ":")+1:] }

func TestLiveTestClassifies(t *testing.T) {
	conns := []store.Connection{
		{ID: "good", Provider: "kilocode", Name: "good@x.io", Active: true, Status: "ok", Priority: 1, Secrets: map[string]string{"accessToken": "good"}, Meta: map[string]any{}},
		{ID: "bad", Provider: "kimi", Active: true, Status: "ok", Priority: 1, Secrets: map[string]string{"accessToken": "SECRET-bad"}, Meta: map[string]any{}},
	}
	s, _ := newTestServer(t, &store.Snapshot{Connections: conns})
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer good" {
			_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			return
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid token SECRET-bad"}}`)
	})
	res := s.LiveTest(t.Context(), LiveOptions{})
	if len(res) != 2 {
		t.Fatalf("%+v", res)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("leak: %s", b)
	}
	byP := map[string]LiveResult{}
	for _, r := range res {
		byP[r.Provider] = r
	}
	if !byP["kilocode"].OK || byP["kimi"].OK || byP["kimi"].Class != "auth" || byP["kimi"].Status != 401 {
		t.Fatalf("%+v", res)
	}
}

func TestRenameKeyAndSniffSSE(t *testing.T) {
	b := renameTopLevelKey([]byte(`{"model":"m","messages":[{"max_tokens":1}],"max_tokens":5}`), "max_tokens", "max_completion_tokens")
	if string(b) != `{"model":"m","messages":[{"max_tokens":1}],"max_completion_tokens":5}` {
		t.Fatalf("%s", b)
	}
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader("event: response.created\ndata: {}\n\n"))}
	if !sniffSSE(resp) {
		t.Fatal("SSE not detected")
	}
	if all, _ := io.ReadAll(resp.Body); !strings.HasPrefix(string(all), "event:") {
		t.Fatal("peeked bytes lost")
	}
	if errClass(403, "This service has been disabled in this account for violation of Terms of Service.") != "banned" || errClass(429, "RESOURCE_EXHAUSTED quota") != "quota" {
		t.Fatal("classes")
	}
}
