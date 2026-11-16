package login

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

func fakeIDToken(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func shape(t *testing.T, body string) map[string]any {
	t.Helper()
	r, err := ShapeCodex([]byte(body), "conn")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	m["_lite"] = r.Lite
	return m
}

func TestShapeCodexRegular(t *testing.T) {
	m := shape(t, `{"model":"gpt-5.6-terra-review","input":"hi","temperature":0.2,"max_output_tokens":9,"previous_response_id":"resp_1",
		"tools":[{"type":"function","function":{"name":"sh","description":"run","parameters":{"type":"object","properties":{"x":{"type":"string","pattern":"\\p{L}+"}}}}},
		{"type":"web_search_preview"},{"type":"bogus"}],"tool_choice":{"type":"function","name":"nope"},"reasoning_effort":"max"}`)
	if m["model"] != "gpt-5.6-terra" || m["stream"] != true || m["store"] != false || m["_lite"] != false {
		t.Fatalf("basics: %v", m)
	}
	if !strings.HasPrefix(m["instructions"].(string), "You are Codex") {
		t.Fatal("default instructions missing")
	}
	for _, k := range []string{"temperature", "max_output_tokens", "previous_response_id", "tool_choice", "reasoning_effort"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be dropped", k)
		}
	}
	tools := m["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "sh" || tools[1].(map[string]any)["type"] != "web_search_preview" {
		t.Fatalf("tools: %v", tools)
	}
	if strings.Contains(mustJSON(tools[0]), "pattern") {
		t.Error("\\p{} pattern should be stripped")
	}
	r := m["reasoning"].(map[string]any)
	if r["effort"] != "xhigh" || r["summary"] != "auto" || mustJSON(m["include"]) != `["reasoning.encrypted_content"]` {
		t.Fatalf("reasoning: %v include %v", r, m["include"])
	}
	in := m["input"].([]any)[0].(map[string]any)
	if in["role"] != "user" || m["prompt_cache_key"] == "" {
		t.Fatalf("input/session: %v", m)
	}
}

func TestShapeCodexLiteAndIDs(t *testing.T) {
	m := shape(t, `{"model":"gpt-6.1-terra","instructions":"be brief","input":[{"type":"message","role":"system","content":"sys"},
		{"type":"message","id":"msg_1","role":"user","content":"q"},{"type":"item_reference","id":"rs_9"},"resp_x"],
		"tools":[{"type":"function","name":"f","parameters":{}}]}`)
	if m["_lite"] != true || m["instructions"] != "" || m["tools"] != nil || m["parallel_tool_calls"] != false || m["tool_choice"] != "auto" {
		t.Fatalf("lite shape: %v", m)
	}
	in := m["input"].([]any)
	if len(in) != 4 {
		t.Fatalf("want prefix(2)+2 items, got %d: %v", len(in), in)
	}
	if p := in[0].(map[string]any); p["type"] != "additional_tools" || len(p["tools"].([]any)) != 1 {
		t.Fatalf("tools prefix: %v", p)
	}
	if p := in[1].(map[string]any); !strings.Contains(mustJSON(p), "be brief") {
		t.Fatalf("instruction prefix: %v", p)
	}
	if in[2].(map[string]any)["role"] != "developer" {
		t.Error("system should become developer")
	}
	if _, ok := in[3].(map[string]any)["id"]; ok {
		t.Error("server item id should be stripped")
	}
	if r := m["reasoning"].(map[string]any); r["effort"] != "medium" || r["context"] != "all_turns" {
		t.Fatalf("lite reasoning: %v", r)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestParseCodexIDToken(t *testing.T) {
	a := ParseCodexIDToken(fakeIDToken(map[string]any{"email": "a@b.c", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc", "chatgpt_plan_type": "plus"}}))
	if a.Email != "a@b.c" || a.AccountID != "acc" || a.PlanType != "plus" {
		t.Fatalf("%+v", a)
	}
}

func TestCodexDue(t *testing.T) {
	now := time.Now()
	c := store.Connection{Provider: Codex, Secrets: map[string]string{"refreshToken": "r"}, Meta: map[string]any{
		"expiresAt": now.Add(time.Hour).Format(time.RFC3339), "lastRefreshAt": now.Add(-time.Hour).Format(time.RFC3339)}}
	if CodexDue(c, now) {
		t.Fatal("fresh token is not due")
	}
	c.Meta["expiresAt"] = now.Add(5 * time.Minute).Format(time.RFC3339)
	if !CodexDue(c, now) {
		t.Fatal("expiring token is due")
	}
	c.Meta["expiresAt"] = now.Add(9 * 24 * time.Hour).Format(time.RFC3339)
	c.Meta["lastRefreshAt"] = now.Add(-9 * 24 * time.Hour).Format(time.RFC3339)
	if !CodexDue(c, now) {
		t.Fatal("stale refresh (>8d) is due")
	}
}

type memStore struct {
	mu    sync.Mutex
	conns []store.Connection
	saved int
}

func (m *memStore) Load() (*store.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Connection, len(m.conns))
	for i, c := range m.conns {
		sec, meta := map[string]string{}, map[string]any{}
		for k, v := range c.Secrets {
			sec[k] = v
		}
		for k, v := range c.Meta {
			meta[k] = v
		}
		c.Secrets, c.Meta = sec, meta
		out[i] = c
	}
	return &store.Snapshot{Connections: out}, nil
}

func (m *memStore) SaveConnection(c store.Connection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved++
	for i := range m.conns {
		if m.conns[i].ID == c.ID {
			m.conns[i] = c
		}
	}
	return nil
}

func TestRefresherRotatesAndRespectsImported(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["grant_type"] != "refresh_token" || in["client_id"] != CodexClientID {
			t.Errorf("bad refresh request %v", in)
		}
		if in["refresh_token"] == "dead" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token_reused"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600,
			"id_token": fakeIDToken(map[string]any{"email": "x@y.z", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "A1"}})})
	}))
	defer srv.Close()
	old := time.Now().Add(-time.Minute).Format(time.RFC3339)
	mk := func(id, rt, origin string) store.Connection {
		return store.Connection{ID: id, Provider: Codex, Active: true, Status: "ok", Secrets: map[string]string{"accessToken": "a", "refreshToken": rt},
			Meta: map[string]any{"expiresAt": old, "origin": origin}}
	}
	ms := &memStore{conns: []store.Connection{mk("mine", "r1", OriginFastRouter), mk("imported", "r2", ""), mk("dead", "dead", OriginFastRouter)}}
	var updates []string
	r := &Refresher{Store: ms, Client: srv.Client(), Endpoints: Endpoints{CodexToken: srv.URL}, OnUpdate: func(c store.Connection) { updates = append(updates, c.ID+":"+c.Status) }}
	if n := r.RunOnce(context.Background()); n != 1 || calls != 2 {
		t.Fatalf("refreshed %d with %d calls; imported must be skipped", n, calls)
	}
	snap, _ := ms.Load()
	got := map[string]store.Connection{}
	for _, c := range snap.Connections {
		got[c.ID] = c
	}
	if c := got["mine"]; c.Secrets["accessToken"] != "new-access" || c.Secrets["refreshToken"] != "new-refresh" || CodexAccountID(c) != "A1" || CodexDue(c, time.Now()) {
		t.Fatalf("not rotated: %+v", c.Meta)
	}
	if got["dead"].Status != StatusNeedsReauth || got["imported"].Secrets["accessToken"] != "a" {
		t.Fatal("dead token should need re-auth; imported untouched")
	}
	if strings.Join(updates, ",") != "mine:ok,dead:needs-reauth" {
		t.Fatalf("updates %v", updates)
	}
	if r.RunOnce(context.Background()) != 0 || calls != 2 {
		t.Fatal("nothing should be due on the second pass")
	}
	r.RefreshImported = true
	if r.RunOnce(context.Background()) != 1 {
		t.Fatal("refreshImported should refresh the imported account")
	}
}

func TestRefreshTransientErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	_, err := Endpoints{CodexToken: srv.URL}.CodexRefresh(context.Background(), srv.Client(), "r")
	if err == nil || errors.Is(err, ErrReauth) {
		t.Fatalf("503 should be a retryable error, got %v", err)
	}
}

func TestGLMFlow(t *testing.T) {
	polls := 0
	var poll string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /init", func(w http.ResponseWriter, r *http.Request) {
		poll = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"code":0,"data":{"authorize_url":"https://z.ai/auth?x=1","flow_id":"F1","poll_interval_sec":1}}`))
	})
	mux.HandleFunc("GET /poll/F1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != poll {
			t.Error("poll token must match init")
		}
		polls++
		if polls == 1 {
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"ready","token":"jwt","user":{"email":"g@z.ai","user_id":7},"zai":{"access_token":"zat"}}}`))
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"access_token":"biz"}}`))
	})
	mux.HandleFunc("GET /api/biz/customer/getCustomerInfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer biz" {
			t.Error("business token not used")
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"organizations":[{"organizationId":"o1","organizationName":"default","projects":[{"projectId":"team","projectType":"2"},{"projectId":"p1","projectName":"default"}]}]}}`))
	})
	mux.HandleFunc("GET /api/biz/v1/organization/o1/projects/p1/api_keys", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":[{"name":"other","apiKey":"zz"}]}`))
	})
	mux.HandleFunc("POST /api/biz/v1/organization/o1/projects/p1/api_keys", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"apiKey":"kid"}}`))
	})
	mux.HandleFunc("GET /api/biz/v1/organization/o1/projects/p1/api_keys/copy/kid", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"secretKey":"sec"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ep := Endpoints{GLMInit: srv.URL + "/init", GLMPoll: srv.URL + "/poll", GLMBusinessLogin: srv.URL + "/login", GLMAPI: srv.URL}
	d, err := ep.GLMStart(context.Background(), srv.Client())
	if err != nil || d.URL != "https://z.ai/auth?x=1" {
		t.Fatal(err, d)
	}
	if _, err := ep.GLMCheck(context.Background(), srv.Client(), d); !errors.Is(err, ErrPending) {
		t.Fatalf("want pending, got %v", err)
	}
	res, err := ep.GLMCheck(context.Background(), srv.Client(), d)
	if err != nil {
		t.Fatal(err)
	}
	if res.APIKey != "kid.sec" || res.Email != "g@z.ai" || res.UserID != "7" {
		t.Fatalf("%+v", res)
	}
	var c store.Connection
	ApplyGLM(&c, res)
	if AccessKey(c) != "kid.sec" || c.Status != StatusOK || c.Name != "g@z.ai" {
		t.Fatalf("%+v", c)
	}
}
