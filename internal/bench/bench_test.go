package bench

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDist(t *testing.T) {
	d := NewDist([]float64{5, 1, 4, 2, 3, 10, 6, 7, 8, 9})
	if d.Min != 1 || d.Max != 10 || d.Avg != 5.5 || d.P50 != 5 || d.P95 != 10 {
		t.Fatalf("%+v", d)
	}
	if (NewDist(nil) != Dist{}) {
		t.Fatal("empty")
	}
}

func TestParseEventFormats(t *testing.T) {
	cases := []struct {
		data      string
		text      string
		in, out   int
		wantError bool
	}{
		{`{"choices":[{"delta":{"content":"hi"}}]}`, "hi", 0, 0, false},
		{`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`, "", 9, 4, false},
		{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"yo"}}`, "yo", 0, 0, false},
		{`{"type":"message_start","message":{"usage":{"input_tokens":30,"output_tokens":1}}}`, "", 30, 1, false},
		{`{"type":"message_delta","usage":{"output_tokens":12}}`, "", 0, 12, false},
		{`{"type":"response.output_text.delta","delta":"ok"}`, "ok", 0, 0, false},
		{`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7}}}`, "", 5, 7, false},
		{`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, "", 0, 0, true},
	}
	for _, c := range cases {
		text, in, out, e := parseEvent([]byte(c.data))
		if text != c.text || in != c.in || out != c.out || (e != "") != c.wantError {
			t.Errorf("%s: got %q %d %d %q", c.data, text, in, out, e)
		}
	}
}

func TestBuildShapes(t *testing.T) {
	o := Options{Models: []string{"m/x"}, Stream: true, Prompt: "medium"}
	ids := []string{}
	for _, p := range Profiles {
		ids = append(ids, p.ID)
	}
	o.Clients = ids
	if err := o.Normalize(); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"cursor":     {"/v1/chat/completions", `"role":"system"`, `"type":"function"`, `"include_usage":true`, `"tool_choice":"auto"`, `additional_data`},
		"claude":     {"/v1/messages", `"system":[`, `"input_schema"`, `"cache_control"`, `"max_tokens":32000`, `"name":"Bash"`},
		"codex":      {"/v1/responses", `"instructions"`, `"input_text"`, `"parallel_tool_calls":false`, `"effort":"medium"`, `"name":"apply_patch"`},
		"cline":      {"/v1/chat/completions", `TOOL USE`, `\u003cexecute_command\u003e`},
		"gemini-cli": {"/v1beta/models/m/x:streamGenerateContent?alt=sse", `"systemInstruction"`, `"functionDeclarations"`, `"run_shell_command"`},
		"api-chat":   {"/v1/chat/completions", `"reasoning_effort":"low"`, `"max_tokens":2048`},
		"api-gemini": {"/v1beta/models/m/x:streamGenerateContent?alt=sse", `"thinkingBudget":4096`},
	}
	for _, id := range ids {
		body, path, hdr := Build(o, "m/x", id)
		var v map[string]any
		if json.Unmarshal(body, &v) != nil {
			t.Fatalf("%s: %s", id, body)
		}
		p := ProfileByID(id)
		if p.Endpoint != Gemini && (v["model"] != "m/x" || v["stream"] != true) {
			t.Fatalf("%s: %s", id, body)
		}
		if p.Kind != "api" && (!strings.Contains(string(body), "Do not call any tools") || p.Tools == 0) {
			t.Errorf("%s: not an agent request", id)
		}
		if p.Kind == "api" && strings.Contains(string(body), "tools") {
			t.Errorf("%s: plain request has tools", id)
		}
		for i, w := range want[id] {
			if i == 0 && path != w {
				t.Errorf("%s: path %s", id, path)
			} else if i > 0 && !strings.Contains(string(body), w) {
				t.Errorf("%s body lacks %s", id, w)
			}
		}
		if strings.Contains(hdr["User-Agent"], "claude-cli") || !strings.Contains(hdr["User-Agent"], "fastrouter-bench") { // claude-cli would trigger the Claude Code bypass
			t.Errorf("%s: user agent %q", id, hdr["User-Agent"])
		}
	}
	plain := Options{Models: []string{"m"}}
	_ = plain.Normalize()
	if plain.Runs != 3 || plain.Prompt != "tiny" || plain.MaxTokens != 1024 || plain.Effort != "auto" || len(plain.Clients) != 1 || plain.Clients[0] != "api-chat" {
		t.Fatalf("plain: %+v", plain)
	}
	none := Options{Models: []string{"m"}, Endpoints: []string{"responses"}, Effort: "none"}
	_ = none.Normalize()
	if body, _, _ := Build(none, "m", none.Clients[0]); strings.Contains(string(body), "reasoning") {
		t.Fatalf("effort none: %s", body)
	}
	legacy := Options{Models: []string{"m"}, IDE: true}
	_ = legacy.Normalize()
	if strings.Join(legacy.Clients, ",") != "cursor,claude,codex" || legacy.Total() != 9 {
		t.Fatalf("legacy: %+v", legacy)
	}
	if err := (&Options{Models: []string{"m"}, Clients: []string{"vim"}}).Normalize(); err == nil {
		t.Fatal("bad client accepted")
	}
}

func TestGeminiParseAndLabels(t *testing.T) {
	text, in, out, _ := parseEvent([]byte(`{"candidates":[{"content":{"parts":[{"text":"one"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"thoughtsTokenCount":2}}`))
	if text != "one" || in != 7 || out != 5 {
		t.Fatalf("%q %d %d", text, in, out)
	}
	for ms, want := range map[float64]string{120: "Really fast", 900: "Fast", 2000: "OK", 4000: "Slow", 9000: "Very slow"} {
		if SpeedLabel(ms) != want {
			t.Errorf("%v: %s", ms, SpeedLabel(ms))
		}
	}
	if OverheadLabel(0.4) != "negligible" || OverheadLabel(30) != "noticeable" {
		t.Fatal("overhead labels")
	}
	st := Summarize([]Result{{Model: "m", Client: "api-chat", OK: true, NoText: true, TTFTMs: 900, TotalMs: 900}})
	if st[0].TTFTRuns != 0 || st[0].NoText != 1 || st[0].Client != "api-chat" {
		t.Fatalf("%+v", st[0])
	}
}

func TestSummarizeAndTiming(t *testing.T) {
	h := http.Header{}
	h.Set("X-FastRouter-Timing", "pre=0.40;post=0.10;hold=3.00;upttfb=250.5;attempts=2;failover=120.00")
	r := ParseTimingHeader(h)
	if !r.Timed || r.OverheadMs != 0.5 || r.Attempts != 2 || r.FailoverMs != 120 || r.UpTTFBMs != 250.5 {
		t.Fatalf("%+v", r)
	}
	rs := []Result{
		{Model: "a", Endpoint: Chat, OK: true, TTFTMs: 100, TotalMs: 200, Timed: true, OverheadMs: 1, TokPerSec: 50, Account: "x"},
		{Model: "a", Endpoint: Chat, OK: true, TTFTMs: 300, TotalMs: 400, Timed: true, OverheadMs: 3, TokPerSec: 30, Account: "x"},
		{Model: "a", Endpoint: Chat, OK: false, Class: "quota"},
		{Model: "b", Endpoint: Messages, OK: false, Class: "auth"},
	}
	s := Summarize(rs)
	if len(s) != 2 || s[0].OK != 2 || s[0].Runs != 3 || s[0].TTFT.Avg != 200 || s[0].Overhead.Max != 3 || s[0].TokPS != 40 ||
		s[0].Accounts != "x ×2" || s[0].Errors != "quota" || s[1].OK != 0 || s[1].Errors != "auth" {
		t.Fatalf("%+v", s)
	}
}
