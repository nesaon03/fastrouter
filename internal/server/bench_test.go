package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

// A typical small chat request and response.
const benchReq = `{"model":"bench/m1","messages":[{"role":"user","content":"Write a haiku about routers."}],"max_tokens":64}`

func benchUpstream(b *testing.B) *httptest.Server {
	resp := []byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Packets find their way"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6}}`)
	sse := []byte(strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"tok \"}}]}\n\n", 50) + "data: [DONE]\n\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(sse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func benchClient() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256, DisableCompression: true}}
}

func doBench(b *testing.B, url, body string, auth, parallel bool) {
	c := benchClient()
	b.ReportAllocs()
	b.ResetTimer()
	run := func(next func() bool) {
		for next() {
			req, _ := http.NewRequest("POST", url, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if auth {
				req.Header.Set("Authorization", "Bearer "+localKey)
			}
			resp, err := c.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				b.Fatalf("status %d", resp.StatusCode)
			}
		}
	}
	if !parallel {
		i := 0
		run(func() bool { i++; return i <= b.N })
		return
	}
	b.RunParallel(func(pb *testing.PB) { run(pb.Next) })
}

func benchRouter(b *testing.B, up string) string {
	snap := &store.Snapshot{
		Settings:    map[string]json.RawMessage{},
		Nodes:       []store.Node{{ID: "nb", Name: "bench", Prefix: "bench", Type: "openai-compatible", BaseURL: up + "/v1"}},
		Connections: []store.Connection{{ID: "a", Provider: "nb", Active: true, Status: "ok", Secrets: map[string]string{"apiKey": "k"}}},
		APIKeys:     []store.APIKey{{ID: "k", Key: localKey, Active: true}},
	}
	s := New(testConfig(), snap, nil)
	hs := httptest.NewServer(s.Handler())
	b.Cleanup(func() { hs.Close(); s.Close() })
	return hs.URL + "/v1/chat/completions"
}

func benchMessagesURL(chatURL string) string {
	return strings.TrimSuffix(chatURL, "/v1/chat/completions") + "/v1/messages"
}

var benchStreamReq = strings.Replace(benchReq, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)

// Serial benchmarks: ns/op is per-request latency. Router minus Direct = added latency.
func BenchmarkDirect(b *testing.B) {
	doBench(b, benchUpstream(b).URL+"/v1/chat/completions", benchReq, false, false)
}

func BenchmarkViaRouter(b *testing.B) {
	doBench(b, benchRouter(b, benchUpstream(b).URL), benchReq, true, false)
	reportHeap(b)
}

func BenchmarkDirectStream(b *testing.B) {
	doBench(b, benchUpstream(b).URL+"/v1/chat/completions", benchStreamReq, false, false)
}

func BenchmarkViaRouterStream(b *testing.B) {
	doBench(b, benchRouter(b, benchUpstream(b).URL), benchStreamReq, true, false)
	reportHeap(b)
}

// Translated: an Anthropic-format client against an OpenAI-format upstream.
func BenchmarkViaRouterTranslated(b *testing.B) {
	doBench(b, benchMessagesURL(benchRouter(b, benchUpstream(b).URL)), benchReq, true, false)
	reportHeap(b)
}

func BenchmarkViaRouterStreamTranslated(b *testing.B) {
	doBench(b, benchMessagesURL(benchRouter(b, benchUpstream(b).URL)), benchStreamReq, true, false)
	reportHeap(b)
}

func BenchmarkViaRouterStreamTranslatedParallel(b *testing.B) {
	doBench(b, benchMessagesURL(benchRouter(b, benchUpstream(b).URL)), benchStreamReq, true, true)
	reportHeap(b)
}

// Parallel benchmarks: ns/op is amortized cost under concurrent load (throughput).
func BenchmarkDirectParallel(b *testing.B) {
	doBench(b, benchUpstream(b).URL+"/v1/chat/completions", benchReq, false, true)
}

func BenchmarkViaRouterParallel(b *testing.B) {
	doBench(b, benchRouter(b, benchUpstream(b).URL), benchReq, true, true)
	reportHeap(b)
}

func reportHeap(b *testing.B) {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	b.ReportMetric(float64(m.HeapInuse)/(1<<20), "heap-MB")
}
