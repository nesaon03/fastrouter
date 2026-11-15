package translate

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var formats = []Format{OpenAI, Anthropic, Gemini, Responses}

func resetIDs() {
	n := 0
	genID = func(prefix string, _ int) string { n++; return fmt.Sprintf("%sgen%d", prefix, n) }
	nowUnix = func() int64 { return 1700000000 }
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run go test -update): %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden\n got: %s\nwant: %s", name, got, want)
	}
}

func pretty(b []byte) []byte {
	var out bytes.Buffer
	if json.Indent(&out, b, "", "  ") != nil {
		return b
	}
	return append(out.Bytes(), '\n')
}

// ---- semantic summaries used for round-trip checks ----

func canonJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func partSummary(p Part) string {
	switch p.Type {
	case PartText:
		return "text:" + p.Text
	case PartImage:
		return "image:" + p.MediaType + ":" + p.Data + p.URL
	case PartToolCall:
		return "call:" + p.ToolName + canonJSON(p.Args)
	case PartToolResult:
		return "result:" + resultText(p.Result)
	}
	return ""
}

// requestSummary flattens a request to comparable lines (thinking and ids excluded,
// consecutive same-role turns merged, since formats differ there by design).
func requestSummary(r *Request) []string {
	out := []string{"system:" + r.SystemText()}
	lastRole := ""
	for _, m := range r.Messages {
		if m.Role != lastRole {
			out = append(out, "role:"+m.Role)
			lastRole = m.Role
		}
		for _, p := range m.Parts {
			if s := partSummary(p); s != "" {
				out = append(out, s)
			}
		}
	}
	for _, t := range r.Tools {
		out = append(out, "tool:"+t.Name+":"+t.Description+":"+canonJSON(string(t.Schema)))
	}
	if r.ToolChoice != nil {
		out = append(out, "choice:"+r.ToolChoice.Mode+r.ToolChoice.Name)
	}
	out = append(out, fmt.Sprintf("max:%d stream:%v", r.MaxTokens, r.Stream))
	return out
}

type respSummary struct {
	Text, Thinking string
	Calls          []string
	Finish         Finish
}

func summarizeResponse(r *Response) respSummary {
	var s respSummary
	for _, p := range r.Parts {
		switch p.Type {
		case PartText:
			s.Text += p.Text
		case PartThinking:
			s.Thinking += p.Text
		case PartToolCall:
			s.Calls = append(s.Calls, p.ToolName+canonJSON(p.Args))
		}
	}
	s.Finish = r.Finish
	return s
}

// collect folds stream events into a Response.
func collect(evs []Event) *Response {
	r := &Response{}
	args := map[int]*strings.Builder{}
	var order []int
	var names = map[int]string{}
	var text, thinking strings.Builder
	for _, e := range evs {
		switch e.Kind {
		case EvText:
			text.WriteString(e.Text)
		case EvThinking:
			thinking.WriteString(e.Text)
		case EvToolStart:
			args[e.Index] = &strings.Builder{}
			names[e.Index] = e.Name
			order = append(order, e.Index)
		case EvToolArgs:
			if b := args[e.Index]; b != nil {
				b.WriteString(e.Text)
			}
		case EvUsage:
			r.Usage = e.Usage
		case EvFinish:
			r.Finish = e.Finish
		}
	}
	if thinking.Len() > 0 {
		r.Parts = append(r.Parts, Part{Type: PartThinking, Text: thinking.String()})
	}
	if text.Len() > 0 {
		r.Parts = append(r.Parts, Part{Type: PartText, Text: text.String()})
	}
	for _, i := range order {
		r.Parts = append(r.Parts, Part{Type: PartToolCall, ToolName: names[i], Args: args[i].String()})
	}
	return r
}

func decodeAll(t *testing.T, f Format, stream []byte) []Event {
	t.Helper()
	fr := NewFrameReader(bytes.NewReader(stream))
	dec := NewDecoder(f)
	var evs []Event
	for {
		frame, err := fr.Next()
		if err != nil {
			break
		}
		e, derr := dec.Decode(frame)
		if derr != nil {
			t.Fatalf("%s decode: %v (frame %q)", f, derr, frame.Data)
		}
		evs = append(evs, e...)
	}
	return append(evs, dec.End()...)
}

// usageFields returns the usage counts both formats can represent.
func usageFields(u Usage, a, b Format) Usage {
	for _, f := range []Format{a, b} {
		if f != Anthropic {
			u.CacheWrite = 0
		}
		if f == Anthropic {
			u.Reasoning = 0
		}
	}
	return u
}

var wantUsage = Usage{Input: 120, Output: 45, CacheRead: 100, Reasoning: 20}

// ---- tests ----

func TestRequestTranslationGolden(t *testing.T) {
	for _, src := range formats {
		body := read(t, "request."+src.String()+".json")
		req, err := ParseRequest(src, body, "gemini-3.8-flash", true)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		want := requestSummary(req)
		for _, dst := range formats {
			if dst == src {
				continue
			}
			resetIDs()
			out, err := EmitRequest(dst, req, EmitOptions{Model: "upstream-model", ReasoningFields: true, KeepCacheControl: true})
			if err != nil {
				t.Fatal(err)
			}
			golden(t, fmt.Sprintf("req_%s_to_%s.json", src, dst), pretty(out))

			back, err := ParseRequest(dst, out, "upstream-model", true)
			if err != nil {
				t.Fatalf("%s->%s reparse: %v", src, dst, err)
			}
			got := requestSummary(back)
			want := want
			if dst == Gemini || src == Gemini || dst == Anthropic {
				got, want = trimSchema(got), trimSchema(want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s->%s round trip\n got: %q\nwant: %q", src, dst, got, want)
			}
		}
	}
}

// trimSchema drops tool schemas (Gemini rewrites them to its OpenAPI subset)
// and max_tokens (Anthropic raises it above the thinking budget by design).
func trimSchema(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, "tool:") {
			f := strings.SplitN(l, ":", 4)
			l = strings.Join(f[:3], ":")
		}
		if strings.HasPrefix(l, "max:") {
			l = l[strings.Index(l, " ")+1:]
		}
		out = append(out, l)
	}
	return out
}

func TestResponseTranslationGolden(t *testing.T) {
	for _, src := range formats {
		resp, err := ParseResponse(src, read(t, "response."+src.String()+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Usage != wantUsage && !(src == Anthropic && resp.Usage == Usage{Input: 120, Output: 45, CacheRead: 100}) {
			t.Errorf("%s usage = %+v", src, resp.Usage)
		}
		want := summarizeResponse(resp)
		if want.Finish != FinishToolCalls || len(want.Calls) != 2 || want.Text != "Checking both." || want.Thinking == "" {
			t.Fatalf("%s fixture parsed wrong: %+v", src, want)
		}
		for _, dst := range formats {
			if dst == src {
				continue
			}
			resetIDs()
			out, err := EmitResponse(dst, resp, "m")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, fmt.Sprintf("resp_%s_to_%s.json", src, dst), pretty(out))
			back, err := ParseResponse(dst, out)
			if err != nil {
				t.Fatal(err)
			}
			if got := summarizeResponse(back); !reflect.DeepEqual(got, want) {
				t.Errorf("%s->%s\n got: %+v\nwant: %+v", src, dst, got, want)
			}
			if g, w := usageFields(back.Usage, src, dst), usageFields(resp.Usage, src, dst); g != w {
				t.Errorf("%s->%s usage %+v want %+v", src, dst, g, w)
			}
		}
	}
}

func TestStreamTranslationGolden(t *testing.T) {
	for _, src := range formats {
		stream := read(t, "stream."+src.String()+".sse")
		evs := decodeAll(t, src, stream)
		want := collect(evs)
		ws := summarizeResponse(want)
		if ws.Text != "Checking both." || ws.Thinking != "Need weather for two cities." || len(ws.Calls) != 2 || ws.Finish != FinishToolCalls {
			t.Fatalf("%s stream decoded wrong: %+v", src, ws)
		}
		if g, w := usageFields(want.Usage, src, src), usageFields(wantUsage, src, src); g != w {
			t.Errorf("%s stream usage %+v want %+v", src, g, w)
		}
		for _, dst := range formats {
			if dst == src {
				continue
			}
			resetIDs()
			enc := NewEncoder(dst, "m")
			var out []byte
			for _, e := range evs { // one event at a time, as the proxy does
				out = append(out, enc.Encode([]Event{e})...)
			}
			out = append(out, enc.Close()...)
			golden(t, fmt.Sprintf("stream_%s_to_%s.sse", src, dst), out)

			back := collect(decodeAll(t, dst, out))
			if got := summarizeResponse(back); !reflect.DeepEqual(got, ws) {
				t.Errorf("%s->%s stream\n got: %+v\nwant: %+v", src, dst, got, ws)
			}
			if g, w := usageFields(back.Usage, src, dst), usageFields(want.Usage, src, dst); g != w {
				t.Errorf("%s->%s stream usage %+v want %+v", src, dst, g, w)
			}
		}
	}
}

// Every pair through a third format and back keeps the conversation intact.
func TestRequestRoundTripViaEveryFormat(t *testing.T) {
	req, _ := ParseRequest(Anthropic, read(t, "request.anthropic.json"), "", true)
	want := trimSchema(requestSummary(req))
	for _, mid := range formats {
		for _, end := range formats {
			b1, _ := EmitRequest(mid, req, EmitOptions{Model: "m", ReasoningFields: true})
			r1, err := ParseRequest(mid, b1, "m", true)
			if err != nil {
				t.Fatal(err)
			}
			b2, _ := EmitRequest(end, r1, EmitOptions{Model: "m", ReasoningFields: true})
			r2, err := ParseRequest(end, b2, "m", true)
			if err != nil {
				t.Fatal(err)
			}
			if got := trimSchema(requestSummary(r2)); !reflect.DeepEqual(got, want) {
				t.Errorf("anthropic->%s->%s\n got: %q\nwant: %q", mid, end, got, want)
			}
		}
	}
}

func TestThinkingAndSignatures(t *testing.T) {
	// Anthropic signed thinking survives a trip through Anthropic emit.
	req, _ := ParseRequest(Anthropic, read(t, "request.anthropic.json"), "", true)
	out, _ := EmitRequest(Anthropic, req, EmitOptions{Model: "m"})
	if !bytes.Contains(out, []byte(`"signature":"sig-abc"`)) || !bytes.Contains(out, []byte(`"budget_tokens":2048`)) {
		t.Errorf("anthropic emit lost thinking/signature: %s", out)
	}
	if bytes.Contains(out, []byte(`"temperature"`)) {
		t.Error("temperature must be dropped when thinking is on")
	}
	// Unsigned thinking from OpenAI history is dropped for Anthropic (it would be rejected).
	oreq, _ := ParseRequest(OpenAI, read(t, "request.openai.json"), "", true)
	out, _ = EmitRequest(Anthropic, oreq, EmitOptions{Model: "m"})
	if bytes.Contains(out, []byte(`Need two lookups`)) {
		t.Error("unsigned thinking should not be sent to Anthropic")
	}
	if bytes.Contains(out, []byte(`"thinking":{`)) {
		t.Errorf("thinking must be off when the open tool turn has no signed thinking: %s", out)
	}
	simple, _ := ParseRequest(OpenAI, []byte(`{"model":"m","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`), "", false)
	out, _ = EmitRequest(Anthropic, simple, EmitOptions{Model: "m"})
	if !bytes.Contains(out, []byte(`"budget_tokens":4096`)) {
		t.Errorf("reasoning_effort low should map to a 4096 budget: %s", out)
	}
	// Gemini gets a thought signature on the first replayed call.
	out, _ = EmitRequest(Gemini, oreq, EmitOptions{Model: "m"})
	if !bytes.Contains(out, []byte(dummySignature)) {
		t.Error("gemini emit should add a placeholder thoughtSignature for foreign tool calls")
	}
	greq, _ := ParseRequest(Gemini, read(t, "request.gemini.json"), "m", true)
	out, _ = EmitRequest(Gemini, greq, EmitOptions{Model: "m"})
	if !bytes.Contains(out, []byte(`"thoughtSignature":"gsig-1"`)) {
		t.Errorf("gemini signature lost: %s", out)
	}
	// Anthropic thinking budget maps to OpenAI effort and back.
	if e := (&Thinking{Mode: "enabled", Budget: 2048}).EffortFor(); e != "low" {
		t.Errorf("budget 2048 -> %q", e)
	}
	// Signatures flow through streams: Anthropic -> OpenAI reasoning_details -> Anthropic.
	evs := decodeAll(t, Anthropic, read(t, "stream.anthropic.sse"))
	enc := NewEncoder(OpenAI, "m")
	oa := append(enc.Encode(evs), enc.Close()...)
	back := decodeAll(t, OpenAI, oa)
	found := false
	for _, e := range back {
		if e.Kind == EvSignature && e.Text == "sig-xyz" {
			found = true
		}
	}
	if !found {
		t.Error("signature lost through OpenAI stream encoding")
	}
}

func TestCacheControl(t *testing.T) {
	req, _ := ParseRequest(Anthropic, read(t, "request.anthropic.json"), "", true)
	out, _ := EmitRequest(OpenAI, req, EmitOptions{Model: "m", KeepCacheControl: true})
	if bytes.Count(out, []byte(`"cache_control"`)) != 2 {
		t.Errorf("cache_control should be kept on system and last user part: %s", out)
	}
	out, _ = EmitRequest(OpenAI, req, EmitOptions{Model: "m"})
	if bytes.Contains(out, []byte(`cache_control`)) {
		t.Error("cache_control must be dropped unless the target supports it")
	}
	out, _ = EmitRequest(Anthropic, req, EmitOptions{Model: "m"})
	if bytes.Count(out, []byte(`"cache_control"`)) != 2 {
		t.Errorf("anthropic passthrough of cache_control: %s", out)
	}
}

func TestFinishReasons(t *testing.T) {
	cases := []struct {
		f          Finish
		oa, an, gm string
	}{
		{FinishStop, "stop", "end_turn", "STOP"},
		{FinishLength, "length", "max_tokens", "MAX_TOKENS"},
		{FinishToolCalls, "tool_calls", "tool_use", "STOP"},
		{FinishContentFilter, "content_filter", "refusal", "SAFETY"},
	}
	for _, c := range cases {
		if toOAFinish(c.f) != c.oa || oaFinish(c.oa) != c.f {
			t.Errorf("openai %v", c.f)
		}
		if toAnFinish(c.f) != c.an || anFinish(c.an) != c.f {
			t.Errorf("anthropic %v", c.f)
		}
		if toGmFinish(c.f) != c.gm {
			t.Errorf("gemini %v", c.f)
		}
	}
	if rsFinish("incomplete", "max_output_tokens", false) != FinishLength {
		t.Error("responses incomplete")
	}
}

func TestSyntheticAndEstimate(t *testing.T) {
	for _, f := range formats {
		for _, stream := range []bool{false, true} {
			b := Synthetic(f, "m", "hello", stream)
			var r *Response
			if stream {
				r = collect(decodeAll(t, f, b))
			} else {
				var err error
				if r, err = ParseResponse(f, b); err != nil {
					t.Fatal(err)
				}
			}
			if s := summarizeResponse(r); s.Text != "hello" {
				t.Errorf("%s stream=%v synthetic text %q", f, stream, s.Text)
			}
		}
	}
	req, _ := ParseRequest(Anthropic, read(t, "request.anthropic.json"), "", true)
	if n := EstimateTokens(req); n < 1600 || n > 2000 {
		t.Errorf("estimate %d", n)
	}
}

func TestFrameReader(t *testing.T) {
	in := "event: a\ndata: {\"x\":1}\n\n: ping\n\ndata: line1\ndata: line2\r\n\r\ndata: tail"
	fr := NewFrameReader(strings.NewReader(in))
	var got []string
	for {
		f, err := fr.Next()
		if err != nil {
			break
		}
		got = append(got, f.Event+"|"+string(f.Data))
	}
	want := []string{`a|{"x":1}`, "|", "|line1\nline2", "|tail"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("frames %q", got)
	}
}
