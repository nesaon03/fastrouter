package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/store"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

func codexConn(id, status, token string) store.Connection {
	return store.Connection{ID: id, Provider: "codex", Name: id, AuthType: "oauth", Priority: 1, Active: true, Status: status,
		Secrets: map[string]string{"accessToken": token, "refreshToken": "r"},
		Meta:    map[string]any{"providerSpecificData": map[string]any{"chatgptAccountId": "ACC-1"}}}
}

func TestCodexTargetShapesRequestAndFoldsStream(t *testing.T) {
	var mu sync.Mutex
	var gotHdr http.Header
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotHdr, gotBody = r.Header.Clone(), nil
		_ = json.Unmarshal(b, &gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	}))
	defer up.Close()
	// Imported before the plugin existed: status "needs-plugin" is usable now.
	s, hs := newTestServer(t, &store.Snapshot{Connections: []store.Connection{codexConn("c1", "needs-plugin", "tok-1")}})
	p, _ := s.state.Load().Provider("cx")
	p.URLs[tr.Responses] = up.URL

	resp, body := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"cx/gpt-5.6-terra","max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	mustContain(t, body, `"content":"OK"`)
	mu.Lock()
	defer mu.Unlock()
	if gotHdr.Get("Authorization") != "Bearer tok-1" || gotHdr.Get("ChatGPT-Account-ID") != "ACC-1" || gotHdr.Get("originator") != "codex_cli_rs" ||
		gotHdr.Get("session_id") == "" || gotHdr.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers: %v", gotHdr)
	}
	if gotBody["stream"] != true || gotBody["store"] != false || gotBody["model"] != "gpt-5.6-terra" || gotBody["instructions"] == nil {
		t.Fatalf("body: %v", gotBody)
	}
	if _, ok := gotBody["max_output_tokens"]; ok {
		t.Fatal("Codex rejects max_output_tokens")
	}
}

func TestGLMTargetUsesAPIKeyStyleAuth(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["glm-key"] = jsonReply(200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	c := store.Connection{ID: "g1", Provider: "glm", AuthType: "oauth", Priority: 1, Active: true, Status: "needs-plugin",
		Secrets: map[string]string{"accessToken": "glm-key"}, Meta: map[string]any{}}
	s, hs := newTestServer(t, &store.Snapshot{Connections: []store.Connection{c}})
	p, _ := s.state.Load().Provider("glm")
	p.URLs[tr.Anthropic] = up.URL + "/v1/messages?beta=true"
	resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"glm/glm-5.3","max_tokens":5,"messages":[{"role":"user","content":"Say OK"}]}`, nil)
	if resp.StatusCode != 200 || sc.last().XAPIKey != "glm-key" {
		t.Fatalf("%d %s key=%q", resp.StatusCode, body, sc.last().XAPIKey)
	}
}

func TestPluginProvidersStayParked(t *testing.T) {
	_, hs := newTestServer(t, &store.Snapshot{Connections: []store.Connection{{ID: "a", Provider: "cursor", Active: true, Status: "needs-plugin",
		Secrets: map[string]string{"accessToken": "x"}, Meta: map[string]any{}}}})
	resp, body := post(t, hs.URL, `{"model":"cu/claude-4-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "needs the login plugin") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestDashboardCodexSignInFlow(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "CODE" || r.Form.Get("code_verifier") == "" {
			t.Errorf("bad exchange %v", r.Form)
		}
		claims, _ := json.Marshal(map[string]any{"email": "new@user.io", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ACC-9"}})
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "fresh-r", "expires_in": 3600,
			"id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".s"})
	}))
	defer tok.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}, Connections: []store.Connection{codexConn("old", "ok", "stale")},
		APIKeys: []store.APIKey{{ID: "k", Name: "k", Key: localKey, Active: true}}}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Plugins.CodexCallbackPort = 1 // never bindable: forces the paste path
	s := New(cfg, snap, st)
	s.logins.ep.CodexToken = tok.URL
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	// Posts without a dashboard session and CSRF token are refused.
	if resp, _ := do(t, "POST", hs.URL+"/api/login/codex/start", `{}`, map[string]string{"Content-Type": "application/json"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	hdr := adminSession(t, hs.URL, "")
	resp, body := do(t, "POST", hs.URL+"/api/login/codex/start", `{"connectionId":"old"}`, hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var f struct{ ID, URL, Kind string }
	_ = json.Unmarshal([]byte(body), &f)
	if !strings.HasPrefix(f.URL, login.CodexAuthorizeURL) || !strings.Contains(f.URL, "code_challenge_method=S256") || f.Kind != "browser" {
		t.Fatalf("auth url %s", f.URL)
	}
	state := strings.SplitN(strings.SplitN(f.URL, "state=", 2)[1], "&", 2)[0]
	if resp, _ := do(t, "POST", hs.URL+"/api/login/flows/"+f.ID+"/callback", `{"url":"http://localhost:1455/auth/callback?code=CODE&state=wrong"}`, hdr); resp.StatusCode != 400 {
		t.Fatal("wrong state must be refused")
	}
	resp, body = do(t, "POST", hs.URL+"/api/login/flows/"+f.ID+"/callback", `{"url":"http://localhost:1455/auth/callback?code=CODE&state=`+state+`"}`, hdr)
	if resp.StatusCode != 200 || !strings.Contains(body, `"status":"done"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	back, _ := st.Load()
	c := back.Connections[0]
	if c.ID != "old" || c.Secrets["accessToken"] != "fresh" || c.Meta["origin"] != login.OriginFastRouter || login.CodexAccountID(c) != "ACC-9" {
		t.Fatalf("not saved: %+v", c.Meta)
	}
	p, _ := s.state.Load().Provider("cx")
	if a := p.Pool.Accounts()[0]; a.Key() != "fresh" || a.AccountID != "ACC-9" {
		t.Fatal("live table not reloaded")
	}
	_, body = do(t, "GET", hs.URL+"/api/logins", "", nil)
	if strings.Contains(body, `"fresh"`) || !strings.Contains(body, `"AutoRefresh":true`) {
		t.Fatalf("logins view leaks or wrong: %s", body)
	}
}
