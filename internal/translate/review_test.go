package translate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// L1: a translated OpenAI stream with no upstream finish still ends with a finish chunk.
func TestL1OpenAICloseKeepsFinishChunk(t *testing.T) {
	e := NewEncoder(OpenAI, "m")
	out := string(e.Encode([]Event{{Kind: EvText, Text: "hi"}})) + string(e.Close())
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing finish chunk:\n%s", out)
	}
}

// H1: every encoder ends a broken stream with its own error frame, never a normal finish.
func TestH1FailFrames(t *testing.T) {
	want := map[Format][]string{
		OpenAI:    {`"error"`, "boom"},
		Anthropic: {"event: error", "boom"},
		Responses: {"response.failed", `"status":"failed"`, "boom"},
		Gemini:    {`"error"`, "UNAVAILABLE", "boom"},
	}
	bad := map[Format]string{OpenAI: "[DONE]", Anthropic: "message_stop", Responses: "response.completed", Gemini: "STOP"}
	for f, ws := range want {
		e := NewEncoder(f, "m")
		out := string(e.Encode([]Event{{Kind: EvText, Text: "partial"}, {Kind: EvError, Text: "boom"}})) + string(e.Fail("boom"))
		for _, w := range ws {
			if !strings.Contains(out, w) {
				t.Errorf("%v: missing %q in\n%s", f, w, out)
			}
		}
		if strings.Contains(out, bad[f]) {
			t.Errorf("%v: broken stream looks finished:\n%s", f, out)
		}
	}
}

// pipe decodes upstream SSE in format from, re-encodes it for a client in
// format to, then collects the client stream back into a Response.
func pipe(t *testing.T, from, to Format, sse string) *Response {
	t.Helper()
	dec, enc := NewDecoder(from), NewEncoder(to, "m")
	var out []byte
	fr := NewFrameReader(strings.NewReader(sse))
	for {
		f, err := fr.Next()
		if err != nil {
			break
		}
		evs, _ := dec.Decode(f)
		out = append(out, enc.Encode(evs)...)
	}
	out = append(out, enc.Encode(dec.End())...)
	out = append(out, enc.Close()...)
	cdec, coll := NewDecoder(to), &Collector{}
	cfr := NewFrameReader(strings.NewReader(string(out)))
	for {
		f, err := cfr.Next()
		if err != nil {
			break
		}
		evs, _ := cdec.Decode(f)
		coll.Add(evs)
	}
	coll.Add(cdec.End())
	return coll.Response()
}

func toolArgs(r *Response) map[string]string {
	out := map[string]string{}
	for _, p := range r.Parts {
		if p.Type == PartToolCall {
			out[p.ToolCallID] = string(p.Args)
		}
	}
	return out
}

// H2: interleaved parallel tool-call deltas keep every tool's arguments.
func TestH2InterleavedToolArgs(t *testing.T) {
	sse := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":""}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"g","arguments":""}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"q\":2}"}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	for _, to := range []Format{Anthropic, Responses, Gemini, OpenAI} {
		got := toolArgs(pipe(t, OpenAI, to, sse))
		if len(got) != 2 || !jsonEq(got["a"], `{"p":1}`) || !jsonEq(got["b"], `{"q":2}`) {
			t.Errorf("%v: tool args %v", to, got)
		}
	}
}

// H2: providers that omit index and send one call per chunk are kept apart by id.
func TestH2ToolCallsWithoutIndex(t *testing.T) {
	sse := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"id":"b","type":"function","function":{"name":"g","arguments":"{\"y\":"}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"2}"}}]}}]}

data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

`
	got := toolArgs(pipe(t, OpenAI, Anthropic, sse))
	if len(got) != 2 || !jsonEq(got["a"], `{"x":1}`) || !jsonEq(got["b"], `{"y":2}`) {
		t.Fatalf("tool args %v", got)
	}
}

func jsonEq(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
