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
	o := Options{Models: []string{"m"}, Stream: true, IDE: true, Prompt: "medium"}
	if err := o.Normalize(); err != nil {
		t.Fatal(err)
	}
	for ep, want := range map[string][]string{
		Chat:      {"/v1/chat/completions", `"role":"system"`, `"type":"function"`, `"include_usage":true`, `"tool_choice":"auto"`},
		Messages:  {"/v1/messages", `"system":[`, `"input_schema"`, `"cache_control"`},
		Responses: {"/v1/responses", `"instructions"`, `"input_text"`, `"parallel_tool_calls":false`},
	} {
		body, path, hdr := Build(o, "m", ep)
		var v map[string]any
		if json.Unmarshal(body, &v) != nil || path != want[0] || v["model"] != "m" || v["stream"] != true {
			t.Fatalf("%s: %s %s", ep, path, body)
		}
		for _, w := range want[1:] {
			if !strings.Contains(string(body), w) {
				t.Errorf("%s body lacks %s", ep, w)
			}
		}
		if strings.Contains(hdr["User-Agent"], "claude-cli") { // would trigger the Claude Code bypass
			t.Errorf("%s: user agent %q", ep, hdr["User-Agent"])
		}
	}
	plain := Options{Models: []string{"m"}}
	_ = plain.Normalize()
	body, _, _ := Build(plain, "m", Chat)
	if strings.Contains(string(body), "tools") || plain.Runs != 3 || plain.Prompt != "tiny" || plain.MaxTokens != 64 {
		t.Fatalf("plain: %+v %s", plain, body)
	}
	if err := (&Options{Models: []string{"m"}, Endpoints: []string{"gemini"}}).Normalize(); err == nil {
		t.Fatal("bad endpoint accepted")
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
