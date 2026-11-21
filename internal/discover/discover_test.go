package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/catalog"
)

// The list URL is derived from the provider's own endpoints, because those are
// the only thing the catalog actually knows. Every core provider must produce a
// plausible candidate.
func TestEndpointDerivation(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"openai", "https://api.openai.com/v1/models"},
		{"deepseek", "https://api.deepseek.com/models"},
		{"openrouter", "https://openrouter.ai/api/v1/models"},
		{"anthropic", "https://api.anthropic.com/v1/models"},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta/models"},
		{"nvidia", "https://integrate.api.nvidia.com/v1/models"},
		{"xai", "https://api.x.ai/v1/models"},
	}
	for _, c := range cases {
		p, ok := catalog.Lookup(c.id)
		if !ok {
			t.Fatalf("%s: not in the catalog", c.id)
		}
		urls, err := Endpoint(p)
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if len(urls) == 0 {
			t.Fatalf("%s: no candidates", c.id)
		}
		found := false
		for _, u := range urls {
			if u == c.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: candidates %v do not include %s", c.id, urls, c.want)
		}
	}
}

// A provider whose only endpoint is an internal protocol has no list at all.
func TestEndpointNoList(t *testing.T) {
	if _, err := Endpoint(&catalog.Provider{ID: "cursor"}); err == nil {
		t.Fatal("a provider with no HTTP endpoint must report ErrNoEndpoint")
	}
}

// The DeepSeek trap: its list returns the served id, and the catalog's Upstream
// map is what maps the public id onto it.
func TestIsWireID(t *testing.T) {
	p, _ := catalog.Lookup("deepseek")
	if !IsWireID(p.Upstream, "deepseek-flash") {
		t.Fatal("deepseek-flash is the served id and must be treated as a wire id")
	}
	if IsWireID(p.Upstream, "deepseek-v4.1-flash") {
		t.Fatal("a public catalog id is not a wire id")
	}
}

func TestDecodeOpenAIShape(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"b-model"},{"id":"a-model"}]}`)
	got, err := decode(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a-model", "b-model"}) {
		t.Fatalf("got %v", got)
	}
}

// Gemini's list nests a models/ prefix and mixes in non-chat entries.
func TestDecodeGeminiShape(t *testing.T) {
	body := []byte(`{"models":[
	  {"name":"models/gemini-9-pro","supportedGenerationMethods":["generateContent"]},
	  {"name":"models/text-embedding-9","supportedGenerationMethods":["embedContent"]}
	]}`)
	got, err := decode(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"gemini-9-pro"}) {
		t.Fatalf("got %v", got)
	}
}

func TestDecodeGarbage(t *testing.T) {
	if _, err := decode([]byte(`<html>nope</html>`), false); err == nil {
		t.Fatal("expected an error")
	}
}

// List walks the candidate URLs, so a wrong first guess costs one 404.
func TestListFallsBack(t *testing.T) {
	var hits []string
	mux := http.NewServeMux()
	// The derived base is /v1, so /models answers; keep a 404 route to prove the
	// fallback list is exercised when a provider wants the other prefix.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "m1"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &catalog.Provider{ID: "test", ChatURL: srv.URL + "/v1/chat/completions"}
	res, err := List(context.Background(), srv.Client(), p, "k")
	if err != nil {
		t.Fatalf("List: %v (%v)", err, hits)
	}
	if len(res.Models) != 1 || res.Models[0] != "m1" {
		t.Fatalf("models %v", res.Models)
	}
}

func TestListUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := &catalog.Provider{ID: "test", ChatURL: srv.URL + "/v1/chat/completions"}
	_, err := List(context.Background(), srv.Client(), p, "bad")
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

// Providers list the served side; the catalog maps a public name onto it.
// Reversing is what makes an Antigravity or DeepSeek discovery usable at all.
func TestPublicID(t *testing.T) {
	p, _ := catalog.Lookup("deepseek")
	// A served id maps back to the public catalog id.
	if got, drop := PublicID(p.Upstream, "deepseek-flash"); drop || got != "deepseek-v4.1-flash" {
		t.Fatalf("wire id mapped to %q (drop %v)", got, drop)
	}
	// A public id is left alone.
	if got, drop := PublicID(p.Upstream, "deepseek-v4.1-flash"); drop || got != "deepseek-v4.1-flash" {
		t.Fatalf("public id mapped to %q", got)
	}
	// A genuinely new id passes through untouched.
	if got, drop := PublicID(p.Upstream, "deepseek-v9-ultra"); drop || got != "deepseek-v9-ultra" {
		t.Fatalf("new id mapped to %q (drop %v)", got, drop)
	}
	// An adapter thinking suffix on its own is not a name a caller can use.
	if _, drop := PublicID(p.Upstream, "gemini-4-carbon(high)"); !drop {
		t.Fatal("a bare wire suffix should be dropped")
	}
	// Antigravity's own list is entirely wire ids; each must map back.
	ag, _ := catalog.Lookup("antigravity")
	if got, drop := PublicID(ag.Upstream, "gemini-3.8-flash-low(low)"); drop || got != "gemini-3.8-flash-low" {
		t.Fatalf("antigravity wire id mapped to %q (drop %v)", got, drop)
	}
}

func TestLooksLikeChat(t *testing.T) {
	chat := []string{"gpt-5.5", "claude-opus-5-5", "deepseek-v9", "grok-4.7", "qwen4-max", "o3"}
	for _, id := range chat {
		if !LooksLikeChat(id) {
			t.Fatalf("%s should be kept", id)
		}
	}
	notChat := []string{"text-embedding-3-large", "tts-1-hd", "whisper-1", "gpt-4o-transcribe",
		"omni-moderation-latest", "gpt-realtime-mini", "dall-e-3", "sora-2", "grok-imagine-image"}
	for _, id := range notChat {
		if LooksLikeChat(id) {
			t.Fatalf("%s should be filtered out", id)
		}
	}
}

// A list endpoint that answers 200 but is not a model list must not be trusted.
func TestListRejectsNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()
	p := &catalog.Provider{ID: "test", ChatURL: srv.URL + "/v1/chat/completions"}
	if _, err := List(context.Background(), srv.Client(), p, "k"); err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
}
