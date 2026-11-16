package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

// mock answers every upstream host from one handler (no real network).
type mock struct {
	mu   sync.Mutex
	reqs []*http.Request
	body map[string][]byte // path -> last body
	fn   func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newMock(t *testing.T, fn func(w http.ResponseWriter, r *http.Request, body []byte)) (*mock, Env) {
	m := &mock{fn: fn, body: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.reqs = append(m.reqs, r)
		m.body[r.URL.Path] = b
		m.mu.Unlock()
		m.fn(w, r, b)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	tr := roundTrip(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.Header.Set("X-Orig-Host", r.URL.Host)
		r2.URL.Scheme, r2.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	return m, Env{Client: &http.Client{Transport: tr}, RedirectURI: "http://localhost:20128/callback"}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestGitHubDeviceFlow(t *testing.T) {
	polls := 0
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch r.URL.Path {
		case "/login/device/code":
			writeJSON(w, 200, map[string]any{"device_code": "DC", "user_code": "ABCD-1234", "verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 1})
		case "/login/oauth/access_token":
			polls++
			if polls == 1 {
				writeJSON(w, 200, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(w, 200, map[string]any{"access_token": "gho_SECRET"})
		case "/user":
			writeJSON(w, 200, map[string]any{"login": "octo", "id": 7})
		case "/copilot_internal/v2/token":
			if r.Header.Get("Authorization") != "token gho_SECRET" {
				w.WriteHeader(401)
				return
			}
			writeJSON(w, 200, map[string]any{"token": "tid=COPILOT", "expires_at": time.Now().Add(30 * time.Minute).Unix()})
		default:
			w.WriteHeader(404)
		}
	})
	s, _ := Get("github")
	p, err := s.Start(context.Background(), env)
	if err != nil || p.UserCode != "ABCD-1234" || p.Interval != time.Second {
		t.Fatalf("start: %+v %v", p, err)
	}
	if _, err := s.Poll(context.Background(), env, p); !errors.Is(err, ErrPending) {
		t.Fatalf("first poll should be pending, got %v", err)
	}
	r, err := s.Poll(context.Background(), env, p)
	if err != nil || r.Secrets["copilotToken"] != "tid=COPILOT" || r.Secrets["accessToken"] != "gho_SECRET" || r.Name != "octo" || r.ExpiresIn < 1700 {
		t.Fatalf("poll: %+v %v", r, err)
	}
	c := store.Connection{Provider: "github"}
	Apply(&c, r, time.Now())
	if AccessKey(c) != "tid=COPILOT" || c.Status != "ok" {
		t.Fatalf("access key / status: %q %q", AccessKey(c), c.Status)
	}
}

func TestClaudeExchangeCodeHashState(t *testing.T) {
	m, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			var in map[string]string
			_ = json.Unmarshal(b, &in)
			if in["code"] != "THECODE" || in["state"] != "ST" || in["code_verifier"] == "" || in["client_id"] == "" {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			writeJSON(w, 200, map[string]any{"access_token": "sk-ant-oat-X", "refresh_token": "R", "expires_in": 28800})
		case "/api/oauth/profile":
			writeJSON(w, 200, map[string]any{"account": map[string]any{"email": "me@x.io"}})
		}
	})
	s, _ := Get("claude")
	p, _ := s.Start(context.Background(), env)
	if !strings.Contains(p.URL, "code_challenge=") || !strings.Contains(p.URL, "redirect_uri=http%3A%2F%2Flocalhost%3A20128%2Fcallback") {
		t.Fatalf("auth url %s", p.URL)
	}
	r, err := s.Exchange(context.Background(), env, p, url.Values{"code": {"THECODE#ST"}})
	if err != nil || r.Email != "me@x.io" || r.Secrets["accessToken"] != "sk-ant-oat-X" {
		t.Fatalf("%+v %v", r, err)
	}
	if m.reqs[0].Header.Get("Content-Type") != "application/json" {
		t.Fatal("claude token exchange must be JSON")
	}
}

func TestGoogleExchangeFindsProject(t *testing.T) {
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch {
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			writeJSON(w, 200, map[string]any{"access_token": "ya29.A", "refresh_token": "1//R", "expires_in": 3599})
		case strings.HasPrefix(r.URL.Path, "/oauth2/v1/userinfo"):
			writeJSON(w, 200, map[string]any{"email": "g@x.io"})
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			writeJSON(w, 200, map[string]any{"cloudaicompanionProject": map[string]any{"id": "proj-42"}, "allowedTiers": []any{map[string]any{"id": "free-tier", "isDefault": true}}})
		case strings.HasSuffix(r.URL.Path, ":onboardUser"):
			writeJSON(w, 200, map[string]any{"done": true})
		}
	})
	s, _ := Get("gemini-cli")
	p, _ := s.Start(context.Background(), env)
	if !strings.Contains(p.URL, "access_type=offline") {
		t.Fatal(p.URL)
	}
	r, err := s.Exchange(context.Background(), env, p, url.Values{"code": {"4/abc"}})
	if err != nil || r.Meta["projectId"] != "proj-42" || r.Email != "g@x.io" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestKiloPoll(t *testing.T) {
	n := 0
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		switch {
		case r.URL.Path == "/api/device-auth/codes" && r.Method == "POST":
			writeJSON(w, 200, map[string]any{"code": "K1", "verificationUrl": "https://app.kilo.ai/device?code=K1", "expiresIn": 600})
		case r.URL.Path == "/api/device-auth/codes/K1":
			if n++; n == 1 {
				w.WriteHeader(202)
				return
			}
			writeJSON(w, 200, map[string]any{"status": "approved", "token": "kilo-T", "userEmail": "k@x.io"})
		case r.URL.Path == "/api/profile":
			writeJSON(w, 200, map[string]any{"organizations": []any{map[string]any{"id": "org-1"}}})
		}
	})
	s, _ := Get("kilocode")
	p, _ := s.Start(context.Background(), env)
	if _, err := s.Poll(context.Background(), env, p); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	r, err := s.Poll(context.Background(), env, p)
	if err != nil || r.PSD["orgId"] != "org-1" || r.Secrets["accessToken"] != "kilo-T" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestClineCodeIsBase64Tokens(t *testing.T) {
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) { w.WriteHeader(500) })
	s, _ := Get("cline")
	p, _ := s.Start(context.Background(), env)
	raw, _ := json.Marshal(map[string]string{"accessToken": "eyJhbGciOi.x.y", "refreshToken": "RT", "email": "c@x.io", "expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339)})
	r, err := s.Exchange(context.Background(), env, p, url.Values{"code": {base64.RawStdEncoding.EncodeToString(raw)}})
	if err != nil || r.Email != "c@x.io" || r.ExpiresIn < 3500 {
		t.Fatalf("%+v %v", r, err)
	}
	if clineToken("eyJabc") != "workos:eyJabc" || clineToken("plain") != "plain" {
		t.Fatal("workos prefix")
	}
}

func TestRefreshErrorsCarryNoSecrets(t *testing.T) {
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "token 1//SECRET-R is revoked"})
	})
	s, _ := Get("antigravity")
	c := store.Connection{Provider: "antigravity", Secrets: map[string]string{"refreshToken": "1//SECRET-R"}}
	_, err := s.Refresh(context.Background(), env, c)
	if !errors.Is(err, ErrReauth) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("got %v", err)
	}
}

func TestRefresherDueRules(t *testing.T) {
	now := time.Now()
	mk := func(prov, origin string, exp time.Duration) store.Connection {
		return store.Connection{ID: prov, Provider: prov, Active: true, Status: "ok",
			Secrets: map[string]string{"accessToken": "a", "refreshToken": "r"},
			Meta:    map[string]any{"origin": origin, "expiresAt": now.Add(exp).UTC().Format(time.RFC3339Nano)}}
	}
	r := &Refresher{}
	cases := []struct {
		c    store.Connection
		want bool
	}{
		{mk("antigravity", "", 2*time.Minute), true},      // Google tokens don't rotate: refresh ahead
		{mk("antigravity", "", time.Hour), false},         // not due yet
		{mk("claude", "", time.Hour), false},              // rotating + imported: wait for expiry
		{mk("claude", "", -time.Minute), true},            // expired
		{mk("claude", "fastrouter", 3*time.Hour), true},   // own session: 4h lead
		{mk("kilocode", "fastrouter", -time.Hour), false}, // no refresh grant
		{mk("cursor", "fastrouter", -time.Hour), false},   // not ours
	}
	for _, tc := range cases {
		if got := r.Due(tc.c, now); got != tc.want {
			t.Errorf("%s origin=%v: due=%v want %v", tc.c.Provider, tc.c.Meta["origin"], got, tc.want)
		}
	}
	gh := store.Connection{Provider: "github", Active: true, Status: "ok", Secrets: map[string]string{"accessToken": "gho"}, Meta: map[string]any{}}
	if !r.Due(gh, now) {
		t.Error("github without a Copilot token must be minted")
	}
}

type memStore struct {
	snap  store.Snapshot
	saved []store.Connection
}

func (m *memStore) Load() (*store.Snapshot, error)          { return &m.snap, nil }
func (m *memStore) SaveConnection(c store.Connection) error { m.saved = append(m.saved, c); return nil }

func TestRefresherMarksReauth(t *testing.T) {
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		writeJSON(w, 401, map[string]any{"error": "invalid_grant"})
	})
	ms := &memStore{snap: store.Snapshot{Connections: []store.Connection{{ID: "k", Provider: "kimi", Active: true, Status: "ok",
		Secrets: map[string]string{"refreshToken": "r"}, Meta: map[string]any{"origin": "fastrouter"}}}}}
	r := &Refresher{Store: ms, Client: env.Client}
	if n := r.RunOnce(context.Background()); n != 0 || len(ms.saved) != 1 || ms.saved[0].Status != "needs-reauth" {
		t.Fatalf("n=%d saved=%+v", n, ms.saved)
	}
	if r.RunOnce(context.Background()); len(ms.saved) != 1 {
		t.Fatal("backoff should skip the account")
	}
}

func TestShapeCodeAssistAntigravity(t *testing.T) {
	in := `{"contents":[{"role":"model","parts":[{"text":"hi"}]},{"role":"user","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":"b"}]}],
	"systemInstruction":{"parts":[{"text":"You are a Claude agent, built on Anthropic's Claude Agent SDK. Be brief."}]},
	"generationConfig":{"maxOutputTokens":100000},"tools":[{"functionDeclarations":[{"name":"my tool!"}]}],"safetySettings":[]}`
	out, err := shapeCodeAssist([]byte(in), "gemini-4-carbon-high(high)", Account{ID: "acc", Extra: map[string]string{"projectId": "p-1"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	_ = json.Unmarshal(out, &env)
	req := env["request"].(map[string]any)
	gc := req["generationConfig"].(map[string]any)
	tc := gc["thinkingConfig"].(map[string]any)
	if env["model"] != "gemini-4-carbon-high" || env["project"] != "p-1" || env["userAgent"] != "antigravity" || tc["thinkingLevel"] != "high" || gc["maxOutputTokens"].(float64) != 64000 {
		t.Fatalf("envelope: %s", out)
	}
	if !strings.HasPrefix(env["requestId"].(string), "agent/") || req["sessionId"] == nil || req["safetySettings"] != nil {
		t.Fatalf("ids: %s", out)
	}
	cs := req["contents"].([]any)
	if len(cs) != 3 || cs[0].(map[string]any)["role"] != "user" || len(cs[2].(map[string]any)["parts"].([]any)) != 2 {
		t.Fatalf("contents not fixed: %s", out)
	}
	if strings.Contains(string(out), "Claude Agent SDK") || !strings.Contains(string(out), `"my_tool_"`) || !strings.Contains(string(out), "VALIDATED") {
		t.Fatalf("rewrites: %s", out)
	}
	// Gemini CLI: plain envelope, budget for 2.5
	out, _ = shapeCodeAssist([]byte(`{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`), "gemini-2.5-pro(low)", Account{Extra: map[string]string{"projectId": "p"}}, false)
	if !strings.Contains(string(out), `"thinkingBudget":1024`) || strings.Contains(string(out), "requestId") {
		t.Fatalf("cli: %s", out)
	}
}

func TestUnwrapResponse(t *testing.T) {
	sse := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]}}]}}\n\ndata: {\"response\":{\"usageMetadata\":{}}}\n\n"
	b, _ := io.ReadAll(UnwrapResponse(io.NopCloser(strings.NewReader(sse))))
	if strings.Contains(string(b), `"response"`) || !strings.Contains(string(b), "data: {\"candidates\"") {
		t.Fatalf("sse: %q", b)
	}
	b, _ = io.ReadAll(UnwrapResponse(io.NopCloser(strings.NewReader(`{"response":{"candidates":[]},"traceId":"t"}`))))
	if string(b) != `{"candidates":[]}` {
		t.Fatalf("json: %q", b)
	}
}

func TestShapeClaudeAndGrok(t *testing.T) {
	out, _ := ShapeClaude([]byte(`{"system":"Be brief.","messages":[]}`))
	if !strings.HasPrefix(string(out), `{"messages":[],"system":[{"text":"You are Claude Code`) || !strings.Contains(string(out), "Be brief.") {
		t.Fatalf("%s", out)
	}
	out, _ = shapeGrokCLI([]byte(`{"model":"grok-4.5-high","input":[],"user":"u","max_output_tokens":5}`), "grok-4.5-high")
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	r := m["reasoning"].(map[string]any)
	if m["model"] != "grok-4.5" || r["effort"] != "high" || m["stream"] != true || m["store"] != false || m["user"] != nil {
		t.Fatalf("%s", out)
	}
}

func TestUnwrapCline(t *testing.T) {
	b, _ := io.ReadAll(UnwrapCline(io.NopCloser(strings.NewReader(`{"success":true,"data":{"id":"x","choices":[]}}`))))
	if string(b) != `{"id":"x","choices":[]}` {
		t.Fatalf("%s", b)
	}
	b, _ = io.ReadAll(UnwrapCline(io.NopCloser(strings.NewReader("data: {}\n\n"))))
	if string(b) != "data: {}\n\n" {
		t.Fatalf("%q", b)
	}
}

func TestVertexServiceAccount(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	sa, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "vp", "client_email": "sa@vp.iam.gserviceaccount.com", "private_key": pemKey})
	_, env := newMock(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		f, _ := url.ParseQuery(string(b))
		parts := strings.Split(f.Get("assertion"), ".")
		if f.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(parts) != 3 {
			w.WriteHeader(400)
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "ya29.V", "expires_in": 3600})
	})
	s, _ := Get("vertex")
	r, err := s.Paste(context.Background(), env, string(sa))
	if err != nil || r.Secrets["accessToken"] != "ya29.V" || r.PSD["projectId"] != "vp" || r.Secrets["serviceAccount"] == "" {
		t.Fatalf("%+v %v", r, err)
	}
	fmt, u := adapters["vertex"].Route("gemini-2.5-flash", true, Account{Extra: map[string]string{"projectId": "vp", "location": "us-central1"}})
	if fmt != FmtGemini || u != "https://us-central1-aiplatform.googleapis.com/v1/projects/vp/locations/us-central1/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Fatal(u)
	}
	if _, err := s.Paste(context.Background(), env, "not json"); err == nil {
		t.Fatal("bad paste accepted")
	}
}

func TestAdaptersHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-api-key", "k")
	adapters["claude"].Headers(h, "sk-ant-oat", true, Account{})
	if h.Get("Authorization") != "Bearer sk-ant-oat" || h.Get("x-api-key") != "" || !strings.Contains(h.Get("Anthropic-Beta"), "oauth-2025-04-20") {
		t.Fatalf("claude %v", h)
	}
	h = http.Header{}
	adapters["kimi"].Headers(h, "kimi-tok", true, Account{Extra: map[string]string{"deviceId": "d1"}})
	if h.Get("x-api-key") != "kimi-tok" || h.Get("X-Msh-Device-Id") != "d1" {
		t.Fatalf("kimi %v", h)
	}
	f, u := adapters["github"].Route("claude-sonnet-5", false, Account{})
	if f != FmtAnthropic || !strings.HasSuffix(u, "/v1/messages") {
		t.Fatal(f, u)
	}
	if f, _ := adapters["github"].Route("gpt-5.3-codex", false, Account{}); f != FmtResponses {
		t.Fatal(f)
	}
	b, _ := adapters["codebuddy-cn"].Body([]byte(`{"model":"m","stream":false}`), "m", false, Account{})
	if !bytes.Contains(b, []byte(`"stream":true`)) {
		t.Fatalf("%s", b)
	}
}
