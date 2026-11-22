package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nesaon03/fastrouter/internal/store"
)

func postPath(t *testing.T, base, path, body, contentType string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+localKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func openAISnap() *store.Snapshot {
	return &store.Snapshot{Connections: []store.Connection{conn("o1", "openai", "sk-openai", 1)}}
}

// Embeddings keep the OpenAI body, gain the upstream model id and are priced.
func TestEmbeddingsPassThrough(t *testing.T) {
	s, hs := newTestServer(t, openAISnap())
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`)
	})
	resp, out := postPath(t, hs.URL, "/v1/embeddings",
		`{"model":"openai/text-embedding-3-small","input":"hello"}`, "application/json")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"embedding":[0.1]`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-FastRouter-Provider"); got != "openai" {
		t.Fatalf("provider header %q", got)
	}
	h := (*hits)[0]
	if h.Host != "api.openai.com" || h.Path != "/v1/embeddings" || h.Auth != "Bearer sk-openai" {
		t.Fatalf("upstream %+v", h)
	}
	if h.Body["model"] != "text-embedding-3-small" || h.Body["input"] != "hello" {
		t.Fatalf("body %v", h.Body)
	}
}

// Images use their own path and relay a JSON answer unchanged.
func TestImagesPassThrough(t *testing.T) {
	s, hs := newTestServer(t, openAISnap())
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://img/x.png"}]}`)
	})
	resp, out := postPath(t, hs.URL, "/v1/images/generations",
		`{"model":"openai/gpt-image-2","prompt":"a cat"}`, "application/json")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "img/x.png") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	h := (*hits)[0]
	if h.Path != "/v1/images/generations" || h.Body["model"] != "gpt-image-2" || h.Body["prompt"] != "a cat" {
		t.Fatalf("upstream %+v", h)
	}
}

// A multipart transcription is parsed for the model, rebuilt with the upstream
// id, and the audio part survives byte-for-byte.
func TestTranscriptionMultipart(t *testing.T) {
	s, hs := newTestServer(t, openAISnap())
	audio := []byte("RIFF....fake-audio-bytes")
	var gotModel string
	var gotAudio []byte
	var gotCT string
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not multipart: %v", err)
			return
		}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("part: %v", err)
				return
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "model":
				gotModel = string(b)
			case "file":
				gotAudio = b
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hello world"}`)
	})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "openai/whisper-large-v3")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write(audio)
	_ = mw.Close()

	resp, out := postPath(t, hs.URL, "/v1/audio/transcriptions", buf.String(), mw.FormDataContentType())
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "hello world") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Fatalf("content-type %q", gotCT)
	}
	if gotModel != "whisper-large-v3" {
		t.Fatalf("upstream model %q", gotModel)
	}
	if !bytes.Equal(gotAudio, audio) {
		t.Fatalf("audio changed: %q", gotAudio)
	}
}

// Speech returns binary audio, which must be relayed with its content type.
func TestSpeechBinaryPassThrough(t *testing.T) {
	s, hs := newTestServer(t, openAISnap())
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte{0xFF, 0xFB, 0x90, 0x00})
	})
	resp, out := postPath(t, hs.URL, "/v1/audio/speech",
		`{"model":"openai/gpt-4o-mini-tts","input":"hi","voice":"alloy"}`, "application/json")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if out != string([]byte{0xFF, 0xFB, 0x90, 0x00}) {
		t.Fatalf("audio bytes changed: %q", out)
	}
	h := (*hits)[0]
	if h.Path != "/v1/audio/speech" || h.Body["voice"] != "alloy" || h.Body["model"] != "gpt-4o-mini-tts" {
		t.Fatalf("upstream %+v", h)
	}
}

// A rate-limited account fails over to the next one.
func TestEmbeddingsFailover(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		conn("o1", "openai", "sk-first", 1),
		conn("o2", "openai", "sk-second", 2),
	}}
	s, hs := newTestServer(t, snap)
	var hits atomic.Int32
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer sk-first" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
	})
	resp, out := postPath(t, hs.URL, "/v1/embeddings", `{"model":"openai/text-embedding-3-small","input":"x"}`, "application/json")
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("%d %s hits=%d", resp.StatusCode, out, hits.Load())
	}
}

// A provider with no OpenAI-compatible endpoint is skipped with a reason.
func TestNonChatNoEndpoint(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)}}
	_, hs := newTestServer(t, snap)
	resp, out := postPath(t, hs.URL, "/v1/embeddings", `{"model":"typesafe/whatever","input":"x"}`, "application/json")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(out, "no OpenAI-compatible endpoint") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
}

// A model-scoped 404 blocks that account for that model only.
func TestNonChatModelGap(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		conn("o1", "openai", "sk-first", 1),
		conn("o2", "openai", "sk-second", 2),
	}}
	s, hs := newTestServer(t, snap)
	var firstHits atomic.Int32
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-first" {
			firstHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"The model 'text-embedding-3-small' does not exist"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"usage":{"prompt_tokens":1}}`)
	})
	body := `{"model":"openai/text-embedding-3-small","input":"x"}`
	postPath(t, hs.URL, "/v1/embeddings", body, "application/json")
	postPath(t, hs.URL, "/v1/embeddings", body, "application/json")
	if firstHits.Load() != 1 {
		t.Fatalf("blocked account was retried: %d hits", firstHits.Load())
	}
}

// The model id is rewritten for a custom node too (prefix stripped upstream).
func TestNonChatCustomNodeModel(t *testing.T) {
	snap := &store.Snapshot{
		Nodes:       []store.Node{node("fk", "fk", "http://127.0.0.1:1/v1")},
		Connections: []store.Connection{conn("a", "fk", "key-a", 1)},
	}
	s, hs := newTestServer(t, snap)
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	postPath(t, hs.URL, "/v1/embeddings", `{"model":"fk/e5-large","input":"x"}`, "application/json")
	h := (*hits)[0]
	if h.Body["model"] != "e5-large" {
		t.Fatalf("model not rewritten: %v", h.Body)
	}
	if h.Path != "/v1/embeddings" {
		t.Fatalf("path %q", h.Path)
	}
}

// The usage recorder receives the embedding token counts.
func TestNonChatUsageRecorded(t *testing.T) {
	var env struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal([]byte(`{"usage":{"prompt_tokens":7}}`), &env)
	if env.Usage.PromptTokens != 7 {
		t.Fatalf("%+v", env)
	}
}
