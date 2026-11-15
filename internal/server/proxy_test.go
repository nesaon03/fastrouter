package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const localKey = "test-local-key"

// fakeUpstream behaves per API key: "ok" answers, "fail500" errors, "slow" never answers in time.
func fakeUpstream(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch key {
		case "fail500":
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		case "bad400":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
			return
		case "slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 3 {
				fmt.Fprintf(w, "data: {\"model\":%q,\"i\":%d,\"key\":%q}\n\n", req.Model, i, key)
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q,"key":%q,"choices":[{"message":{"role":"assistant","content":"hi"}}]}`, req.Model, key)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func node(id, prefix, base string) store.Node {
	return store.Node{ID: id, Name: id, Prefix: prefix, Type: "openai-compatible", APIType: "chat", BaseURL: base}
}

func conn(id, provider, key string, priority int) store.Connection {
	return store.Connection{ID: id, Provider: provider, Name: id, AuthType: "apikey", Priority: priority, Active: true, Status: "ok",
		Secrets: map[string]string{"apiKey": key}, Meta: map[string]any{}}
}

func testConfig() config.Config {
	c := config.Default()
	c.FirstByteTimeout = config.Duration(300 * time.Millisecond)
	c.IdleTimeout = config.Duration(time.Second)
	return c
}

func newTestServer(t *testing.T, snap *store.Snapshot) (*Server, *httptest.Server) {
	t.Helper()
	if snap.Settings == nil {
		snap.Settings = map[string]json.RawMessage{}
	}
	snap.APIKeys = append(snap.APIKeys, store.APIKey{ID: "k", Name: "k", Key: localKey, Active: true})
	s := New(testConfig(), snap, nil)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); s.Close() })
	return s, hs
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+localKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestPassthroughNonStreaming(t *testing.T) {
	up := fakeUpstream(t, nil)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n1", "ds", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "ok", 1)},
	})
	// "ds" is a built-in prefix, so this custom node is reachable only by its id.
	resp, body := post(t, hs.URL, `{"model":"n1/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi \"model\": x"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"model":"deepseek-v4.1-flash"`) || !strings.Contains(body, `"key":"ok"`) {
		t.Fatalf("upstream did not get rewritten model / key: %s", body)
	}
}

func TestPassthroughStreamingIsByteExact(t *testing.T) {
	up := fakeUpstream(t, nil)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n1", "fast", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "ok", 1)},
	})
	resp, body := post(t, hs.URL, `{"model":"fast/m1","stream":true,"messages":[]}`)
	want := ""
	for i := range 3 {
		want += fmt.Sprintf("data: {\"model\":%q,\"i\":%d,\"key\":%q}\n\n", "m1", i, "ok")
	}
	want += "data: [DONE]\n\n"
	if resp.StatusCode != 200 || body != want {
		t.Fatalf("status %d\n got: %q\nwant: %q", resp.StatusCode, body, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type %q", ct)
	}
}

func TestAccountFallbackBeforeFirstByte(t *testing.T) {
	up := fakeUpstream(t, nil)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes: []store.Node{node("n1", "fast", up.URL+"/v1")},
		Connections: []store.Connection{
			conn("bad", "n1", "fail500", 1),
			conn("slow", "n1", "slow", 2),
			conn("good", "n1", "ok", 3),
		},
	})
	resp, body := post(t, hs.URL, `{"model":"fast/m1","stream":true}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-FastRouter-Account") != "good" {
		t.Fatalf("status %d account %q body %s", resp.StatusCode, resp.Header.Get("X-FastRouter-Account"), body)
	}
	// The failed accounts are now cooling down, so the next request goes straight to "good".
	start := time.Now()
	resp, _ = post(t, hs.URL, `{"model":"fast/m1"}`)
	if resp.Header.Get("X-FastRouter-Account") != "good" || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("second request: account %q took %v", resp.Header.Get("X-FastRouter-Account"), time.Since(start))
	}
}

func TestComboFallbackAndRoundRobin(t *testing.T) {
	up := fakeUpstream(t, nil)
	snap := &store.Snapshot{
		Nodes: []store.Node{node("nbad", "bad", up.URL+"/v1"), node("ngood", "good", up.URL+"/v1")},
		Connections: []store.Connection{
			conn("b1", "nbad", "fail500", 1),
			conn("g1", "ngood", "ok", 1),
		},
		Combos: []store.Combo{
			{Name: "team", Strategy: "fallback", Models: []string{"bad/m1", "ag/plugin-model", "good/m2"}},
			{Name: "rr", Strategy: "round-robin", Models: []string{"good/a", "good/b"}},
		},
	}
	_, hs := newTestServer(t, snap)
	resp, body := post(t, hs.URL, `{"model":"team"}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-FastRouter-Model") != "good/m2" || !strings.Contains(body, `"model":"m2"`) {
		t.Fatalf("status %d model %q body %s", resp.StatusCode, resp.Header.Get("X-FastRouter-Model"), body)
	}
	var got []string
	for range 4 {
		resp, _ := post(t, hs.URL, `{"model":"rr"}`)
		got = append(got, resp.Header.Get("X-FastRouter-Model"))
	}
	if strings.Join(got, ",") != "good/a,good/b,good/a,good/b" {
		t.Fatalf("round-robin order %v", got)
	}
}

func TestNonRetryableErrorIsPassedThrough(t *testing.T) {
	var hits atomic.Int32
	up := fakeUpstream(t, &hits)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n1", "fast", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n1", "bad400", 1), conn("b", "n1", "ok", 2)},
	})
	resp, body := post(t, hs.URL, `{"model":"fast/m1"}`)
	if resp.StatusCode != 400 || hits.Load() != 1 || !strings.Contains(body, "bad request") {
		t.Fatalf("status %d hits %d body %s", resp.StatusCode, hits.Load(), body)
	}
}

func TestAllAccountsFailReturnsLastError(t *testing.T) {
	up := fakeUpstream(t, nil)
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n1", "fast", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n1", "fail500", 1)},
	})
	resp, _ := post(t, hs.URL, `{"model":"fast/m1"}`)
	if resp.StatusCode != 500 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, body := post(t, hs.URL, `{"model":"nope/m1"}`)
	if resp.StatusCode != 404 || !strings.Contains(body, "unknown provider prefix") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			if _, err := io.WriteString(w, "data: {}\n\n"); err != nil {
				break
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer up.Close()
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:       []store.Node{node("n1", "fast", up.URL+"/v1")},
		Connections: []store.Connection{conn("a", "n1", "ok", 1)},
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", hs.URL+"/v1/chat/completions", strings.NewReader(`{"model":"fast/m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+localKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data:") {
		t.Fatalf("first line %q", line)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not cancelled after the client disconnected")
	}
}

func TestAuthAndModels(t *testing.T) {
	_, hs := newTestServer(t, &store.Snapshot{
		Nodes:        []store.Node{node("n1", "fast", "http://127.0.0.1:1/v1")},
		Combos:       []store.Combo{{Name: "team", Strategy: "fallback", Models: []string{"fast/m"}}},
		CustomModels: []store.CustomModel{{ProviderAlias: "n1", ID: "m", Type: "llm"}},
	})
	resp, err := http.Post(hs.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fast/m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("no key: status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", hs.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", localKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"team"`) || !strings.Contains(string(b), `"fast/m"`) {
		t.Fatalf("models: %d %s", resp.StatusCode, b)
	}
}

func TestDashboardPassword(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	_, hs := newTestServer(t, &store.Snapshot{DashboardPassword: string(hash)})
	for _, tc := range []struct {
		pass string
		want int
	}{{"", 401}, {"wrong", 401}, {"hunter2", 200}} {
		req, _ := http.NewRequest("GET", hs.URL+"/api/status", nil)
		if tc.pass != "" {
			req.SetBasicAuth("zenpo", tc.pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("password %q: status %d, want %d", tc.pass, resp.StatusCode, tc.want)
		}
	}
}

func TestReplaceTopLevelString(t *testing.T) {
	in := `{"messages":[{"model":"inner","content":"\"model\":\"x\""}], "model" : "ds/a", "stream":true}`
	out, ok := replaceTopLevelString([]byte(in), "model", "a")
	want := `{"messages":[{"model":"inner","content":"\"model\":\"x\""}], "model" : "a", "stream":true}`
	if !ok || string(out) != want {
		t.Fatalf("got %s", out)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
}
