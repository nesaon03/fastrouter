package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/store"
)

// cutOff writes body as SSE and then drops the connection without ending the stream.
func cutOff(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}
}

func firstAccount(t *testing.T, s *Server, prefix string) *router.Account {
	t.Helper()
	p, ok := s.state.Load().Provider(prefix)
	if !ok || p.Pool.Size() == 0 {
		t.Fatalf("no provider %s", prefix)
	}
	return p.Pool.Accounts()[0]
}

const oaPartial = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"

// H1: a cut-off translated stream ends with an error event, not end_turn/message_stop.
func TestH1CutOffTranslatedStreamIsAnError(t *testing.T) {
	up := httptest.NewServer(cutOff(oaPartial))
	defer up.Close()
	s, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k", 1)}})
	_, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"zz/m","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	mustContain(t, body, "partial", "event: error", "stream ended early")
	if strings.Contains(body, "message_stop") || strings.Contains(body, "end_turn") {
		t.Fatalf("cut-off stream reported as finished:\n%s", body)
	}
	if !firstAccount(t, s, "zz").CoolingDown(time.Now()) {
		t.Fatal("a broken stream must not mark the account healthy")
	}
}

// H1: an upstream error event after content reaches an OpenAI client as an error, with no [DONE].
func TestH1MidStreamErrorEventReachesClient(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = sse("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{anthropicNode("n1", "an", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k", 1)}})
	_, body := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"an/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	mustContain(t, body, "hello", `"error"`, "Overloaded")
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("errored stream ended with [DONE]:\n%s", body)
	}
}

// H1: passthrough streams that break also get an error frame.
func TestH1CutOffPassthroughStream(t *testing.T) {
	up := httptest.NewServer(cutOff(oaPartial))
	defer up.Close()
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k", 1)}})
	_, body := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"zz/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	mustContain(t, body, "partial", `"code":"upstream_error"`)
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("cut-off stream ended with [DONE]:\n%s", body)
	}
}

// H1: a JSON client gets a 502 instead of a half answer, after failing over.
func TestH1CutOffStreamForJSONClientFailsOver(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["bad"] = cutOff(oaPartial)
	sc.reply["good"] = jsonReply(200, `{"choices":[{"index":0,"message":{"role":"assistant","content":"full answer"},"finish_reason":"stop"}]}`)
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "bad", 1), conn("a2", "n1", "good", 2)}})
	resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"zz/m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "full answer") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	_, hs2 := newTestServer(t, &store.Snapshot{Nodes: []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "bad", 1)}})
	resp, body = do(t, "POST", hs2.URL+"/v1/messages", `{"model":"zz/m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "ended early") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// M1: a client hanging up before the first frame does not cool the account down.
func TestM1ClientCancelDoesNotCoolDownAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	s, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k", 1)}})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", hs.URL+"/v1/chat/completions", strings.NewReader(`{"model":"zz/m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+localKey)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	time.Sleep(100 * time.Millisecond)
	if firstAccount(t, s, "zz").CoolingDown(time.Now()) {
		t.Fatal("client cancel put a healthy account in cooldown")
	}
}

// H3: upstream keys are never sent to another host on redirect.
func TestH3RedirectsAreNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" || r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{anthropicNode("n1", "an", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "secret-key", 1)}})
	resp, body := do(t, "POST", hs.URL+"/v1/messages", `{"model":"an/m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if leaked.Load() {
		t.Fatal("API key was forwarded to the redirect target")
	}
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "redirect") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// M3: the final failover error comes back in the client's format.
func TestM3FinalErrorUsesClientShape(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = jsonReply(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{anthropicNode("n1", "an", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k", 1)}})
	resp, body := do(t, "POST", hs.URL+"/v1/chat/completions", `{"model":"an/m","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 529 || strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"message":"Overloaded"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// H4: a passthrough body carrying a foreign (tagged) signature is re-encoded
// so Anthropic never receives it.
func TestH4PassthroughDropsForeignSignature(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k"] = jsonReply(200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	_, hs := newTestServer(t, &store.Snapshot{Nodes: []store.Node{anthropicNode("n1", "an", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k", 1)}})
	body := `{"model":"an/m","max_tokens":10,"messages":[{"role":"user","content":"q"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"frsig:gemini:GEMSIG"},{"type":"text","text":"a"}]},
		{"role":"user","content":"again"}]}`
	resp, out := do(t, "POST", hs.URL+"/v1/messages", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	sent, _ := json.Marshal(sc.last().Body)
	if strings.Contains(string(sent), "GEMSIG") || strings.Contains(string(sent), "frsig") || !strings.Contains(string(sent), `"model":"m"`) {
		t.Fatalf("foreign signature forwarded: %s", sent)
	}
}
