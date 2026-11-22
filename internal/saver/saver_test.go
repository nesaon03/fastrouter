package saver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

// vectors.json comes from running 9Router's own code (testdata/gen-vectors.mjs).
type vectors struct {
	RTK []struct {
		Name, In, Out string
		Body, Want    json.RawMessage
	}
	Inject []struct {
		Format, Name string
		Body, Want   json.RawMessage
	}
}

func loadVectors(t testing.TB) vectors {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func sameJSON(t *testing.T, name string, got []byte, want json.RawMessage) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: invalid JSON %v: %s", name, err, got)
	}
	_ = json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got %s\nwant %s", name, got, want)
	}
}

func TestRTKMatches9Router(t *testing.T) {
	for _, v := range loadVectors(t).RTK {
		if v.Body != nil {
			got, _ := rtkBody(v.Body)
			sameJSON(t, v.Name, got, v.Want)
			continue
		}
		got, _, _ := compressText(v.In)
		if got != v.Out {
			t.Errorf("%s: output differs from 9Router\n got %q\nwant %q", v.Name, got, v.Out)
		}
	}
}

var formats = map[string]tr.Format{"openai": tr.OpenAI, "anthropic": tr.Anthropic, "gemini": tr.Gemini, "responses": tr.Responses}

func TestInjectMatches9Router(t *testing.T) {
	for _, v := range loadVectors(t).Inject {
		got := injectPrompt(v.Body, formats[v.Format], "PROMPT TEXT")
		sameJSON(t, v.Name, got, v.Want)
		again := injectPrompt(got, formats[v.Format], "PROMPT TEXT")
		if string(again) != string(got) {
			t.Errorf("%s: not idempotent", v.Name)
		}
	}
}

func TestUntouchedBodyIsSameSlice(t *testing.T) {
	b := []byte(`{"model":"m","messages":[{"role":"tool","content":"short"}]}`)
	got, hits := rtkBody(b)
	if &got[0] != &b[0] || hits != nil {
		t.Fatal("body without savings should be returned as-is")
	}
}

func TestPromptsVerbatim(t *testing.T) {
	if !strings.HasPrefix(CavemanPrompts["ultra"], "Respond ultra-terse. Maximum compression. Telegraphic.") ||
		!strings.HasPrefix(PonytailPrompts["ultra"], "You are a lazy senior developer.") || len(CavemanPrompts) != 6 || len(PonytailPrompts) != 3 {
		t.Fatal("prompt table not ported")
	}
}

func settings(kv map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range kv {
		out[k], _ = json.Marshal(v)
	}
	return out
}

var imported = settings(map[string]any{"rtkEnabled": true, "cavemanEnabled": true, "cavemanLevel": "ultra",
	"ponytailEnabled": true, "ponytailLevel": "ultra", "headroomEnabled": true, "headroomUrl": "http://localhost:8787", "headroomCodeAware": true})

func TestSettingsResolution(t *testing.T) {
	s := New(imported, config.TokenSavers{})
	if !s.RTK || s.CavemanLevel != "ultra" || s.PonytailLevel != "ultra" || s.Headroom == nil || !s.HeadroomCodeAware ||
		s.Headroom.Endpoint != "http://localhost:8787/v1/compress" {
		t.Fatalf("imported settings not applied: %+v", s)
	}
	off, f, lite := "off", false, "lite"
	s = New(imported, config.TokenSavers{Caveman: &off, Ponytail: &lite, Headroom: &f, RTK: &f})
	if s.RTK || s.CavemanLevel != "" || s.PonytailLevel != "lite" || s.Headroom != nil {
		t.Fatalf("overrides not applied: %+v", s)
	}
	s = New(nil, config.TokenSavers{}) // 9Router defaults
	if !s.RTK || s.CavemanLevel != "" || s.PonytailLevel != "" || s.Headroom != nil {
		t.Fatalf("defaults wrong: %+v", s)
	}
}

func TestOptOut(t *testing.T) {
	h := http.Header{}
	h.Set("x-9router-token-saver", "OFF")
	if !OptedOut(h) {
		t.Fatal("9Router header")
	}
	h = http.Header{}
	h.Set("x-fastrouter-token-saver", "off")
	if !OptedOut(h) || OptedOut(http.Header{}) {
		t.Fatal("fastrouter header")
	}
}

func TestApplyOrderAndTag(t *testing.T) {
	var calls atomic.Int32
	hr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in struct {
			Messages []map[string]any `json:"messages"`
			Model    string           `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/v1/compress" || in.Model != "up-model" {
			t.Errorf("bad call %s %s", r.URL.Path, in.Model)
		}
		for _, m := range in.Messages {
			if strings.Contains(m["content"].(string), "PROMPT") || strings.Contains(m["content"].(string), "Telegraphic") {
				t.Error("headroom must run before caveman")
			}
			if m["role"] == "user" {
				m["content"] = "short"
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": in.Messages})
	}))
	defer hr.Close()
	set := settings(map[string]any{"cavemanEnabled": true, "cavemanLevel": "ultra", "ponytailEnabled": true, "ponytailLevel": "ultra",
		"headroomEnabled": true, "headroomUrl": hr.URL})
	s := New(set, config.TokenSavers{})
	grep := loadVectors(t).RTK[5].In
	body, _ := json.Marshal(map[string]any{"model": "up-model", "messages": []any{
		map[string]any{"role": "user", "content": "a long user message"},
		map[string]any{"role": "tool", "tool_call_id": "1", "content": grep}}})
	out, st := s.Apply(context.Background(), body, tr.OpenAI, "up-model")
	if st.Tag != "RTK:1,HEADROOM,CAVEMAN:ultra,PONYTAIL:ultra" {
		t.Fatalf("tag %q", st.Tag)
	}
	var got struct {
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].Role != "system" || !strings.Contains(got.Messages[0].Content, "Telegraphic") ||
		!strings.Contains(got.Messages[0].Content, "\n\nYou are a lazy senior developer") || got.Messages[1].Content != "short" ||
		!strings.HasPrefix(got.Messages[2].Content, "60 matches in 4F:") {
		t.Fatalf("unexpected body %s", out)
	}
}

func TestHeadroomFailOpenAndPause(t *testing.T) {
	var calls atomic.Int32
	hr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer hr.Close()
	h := NewHeadroom(hr.URL, time.Second, false)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 3; i++ {
		if out, ok := h.Compress(context.Background(), body, tr.OpenAI, "m"); ok || string(out) != string(body) {
			t.Fatal("failure must leave the body alone")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls after failure should pause, got %d", calls.Load())
	}
}

func TestHeadroomClaudeProjection(t *testing.T) {
	hr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Messages []map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&in)
		roles := []string{}
		for _, m := range in.Messages {
			roles = append(roles, m["role"].(string))
			if m["role"] == "tool" {
				m["content"] = "compressed"
			}
		}
		if strings.Join(roles, ",") != "system,user,assistant,assistant,tool" {
			t.Errorf("roles %v", roles)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": in.Messages})
	}))
	defer hr.Close()
	h := NewHeadroom(hr.URL, time.Second, false)
	body := []byte(`{"system":"sys","messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"long tool output"}]}]}`)
	out, ok := h.Compress(context.Background(), body, tr.Anthropic, "m")
	if !ok || !strings.Contains(string(out), `"content":"compressed"`) || !strings.Contains(string(out), `"tool_use_id":"t1"`) {
		t.Fatalf("%v %s", ok, out)
	}
}

func TestHeadroomResponsesToolOutputs(t *testing.T) {
	hr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in struct{ Messages []map[string]any }
		_ = json.Unmarshal(b, &in)
		if len(in.Messages) != 1 || in.Messages[0]["tool_call_id"] != "headroom-tool-0" {
			t.Errorf("only plain-text outputs should be sent: %s", b)
		}
		in.Messages[0]["content"] = "tiny"
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": in.Messages})
	}))
	defer hr.Close()
	h := NewHeadroom(hr.URL, time.Second, false)
	body := []byte(`{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"a long plain output"},{"type":"function_call_output","call_id":"d","output":"{\"json\":true}"}]}`)
	out, ok := h.Compress(context.Background(), body, tr.Responses, "m")
	if !ok || !strings.Contains(string(out), `"output":"tiny"`) || !strings.Contains(string(out), `{\"json\":true}`) {
		t.Fatalf("%v %s", ok, out)
	}
}

func benchBody() []byte {
	b, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": []any{
		map[string]any{"role": "system", "content": strings.Repeat("You are a coding agent. ", 200)},
		map[string]any{"role": "user", "content": strings.Repeat("Please fix the bug. ", 100)},
		map[string]any{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{"id": "1", "type": "function",
			"function": map[string]any{"name": "sh", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "1", "content": strings.Repeat("src/a.go:12:  fmt.Println(x)\n", 200)},
		map[string]any{"role": "user", "content": "continue"},
	}})
	return b
}

// BenchmarkApplyImported is the per-request cost with the imported settings
// (RTK + Caveman ultra + Ponytail ultra; Headroom paused/absent).
func BenchmarkApplyImported(b *testing.B) {
	s := New(imported, config.TokenSavers{})
	s.Headroom.pausedUntil.Store(time.Now().Add(time.Hour).UnixNano())
	body := benchBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Apply(context.Background(), body, tr.OpenAI, "m")
	}
}

// BenchmarkApplyNoToolOutput is the common case: prompts only, no tool output to compress.
func BenchmarkApplyNoToolOutput(b *testing.B) {
	s := New(imported, config.TokenSavers{})
	s.Headroom = nil
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Say OK"}]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Apply(context.Background(), body, tr.OpenAI, "m")
	}
}

func TestHeadroomArgs(t *testing.T) {
	s := New(imported, config.TokenSavers{})
	if got := strings.Join(s.HeadroomArgs("8787"), " "); got != "proxy --port 8787 --code-aware" {
		t.Fatalf("imported: %q", got)
	}
	f := false
	s = New(imported, config.TokenSavers{HeadroomCodeAware: &f, HeadroomKompress: &f})
	if got := strings.Join(s.HeadroomArgs("9000"), " "); got != "proxy --port 9000 --disable-kompress" {
		t.Fatalf("overrides: %q", got)
	}
	if cmd, err := New(imported, config.TokenSavers{}).StartHeadroom(context.Background(), ""); cmd != nil || err != nil {
		t.Fatal("autostart must be off by default")
	}
}

func TestCustomPrompt(t *testing.T) {
	set := map[string]json.RawMessage{"rtkEnabled": json.RawMessage(`false`), "customPromptEnabled": json.RawMessage(`true`), "customPrompt": json.RawMessage(`"  Answer in French.  "`)}
	s := New(set, config.TokenSavers{})
	out, st := s.Apply(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), tr.OpenAI, "m")
	if st.Tag != "CUSTOMPROMPT:17c" || !strings.Contains(string(out), "Answer in French.") {
		t.Fatalf("tag %q body %s", st.Tag, out)
	}
	set["customPromptEnabled"] = json.RawMessage(`false`)
	if New(set, config.TokenSavers{}).Enabled() {
		t.Fatal("custom prompt off should leave savers off")
	}
}
