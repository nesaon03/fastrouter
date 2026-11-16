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

// H4: a Gemini signature reaches an Anthropic client tagged, and on the next
// turn it is restored for Gemini but never sent to Anthropic.
func TestH4SignaturesStayWithTheirFamily(t *testing.T) {
	evs := []Event{{Kind: EvThinking, Text: "hmm"}, {Kind: EvSignature, Text: "GEMSIG"}}
	TagEvents(evs, Gemini)
	out := string(NewEncoder(Anthropic, "m").Encode(evs))
	if !strings.Contains(out, `"signature":"frsig:gemini:GEMSIG"`) {
		t.Fatalf("signature not tagged:\n%s", out)
	}
	body := `{"model":"m","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"q"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"frsig:gemini:GEMSIG"},{"type":"text","text":"a"}]},
		{"role":"user","content":"again"}]}`
	req, err := ParseRequest(Anthropic, []byte(body), "m", false)
	if err != nil {
		t.Fatal(err)
	}
	an, _ := EmitRequest(Anthropic, req, EmitOptions{Model: "m"})
	if strings.Contains(string(an), "GEMSIG") {
		t.Fatalf("Gemini signature sent to Anthropic: %s", an)
	}
	gm, _ := EmitRequest(Gemini, req, EmitOptions{Model: "m"})
	if !strings.Contains(string(gm), `"thoughtSignature":"GEMSIG"`) || strings.Contains(string(gm), "frsig") {
		t.Fatalf("signature not restored for Gemini: %s", gm)
	}
	// Native (untagged) Anthropic signatures still pass to Anthropic only.
	req2, _ := ParseRequest(Anthropic, []byte(strings.ReplaceAll(body, "frsig:gemini:GEMSIG", "ANSIG")), "m", false)
	an2, _ := EmitRequest(Anthropic, req2, EmitOptions{Model: "m"})
	gm2, _ := EmitRequest(Gemini, req2, EmitOptions{Model: "m"})
	if !strings.Contains(string(an2), "ANSIG") || strings.Contains(string(gm2), "ANSIG") {
		t.Fatalf("native signature handling wrong:\n%s\n%s", an2, gm2)
	}
}

// M4: tool parameters named like JSON-Schema keywords must survive cleaning.
func TestM4GeminiSchemaKeepsKeywordNamedParams(t *testing.T) {
	in := `{"type":"object","properties":{"default":{"type":"string","default":"x"},"if":{"type":["string","null"]},"name":{"type":"object","properties":{"const":{"type":"string","const":"a"}}},"list":{"type":"array","items":{"type":"string","examples":["e"]}}},"required":["default","name"],"additionalProperties":false}`
	var got map[string]any
	if err := json.Unmarshal(geminiSchema(json.RawMessage(in)), &got); err != nil {
		t.Fatal(err)
	}
	props := got["properties"].(map[string]any)
	for _, k := range []string{"default", "if", "name", "list"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("parameter %q was deleted: %v", k, props)
		}
	}
	if _, ok := props["default"].(map[string]any)["default"]; ok {
		t.Fatal("default keyword inside a parameter schema should still be stripped")
	}
	if props["if"].(map[string]any)["nullable"] != true {
		t.Fatal("nested type list not cleaned")
	}
	inner := props["name"].(map[string]any)["properties"].(map[string]any)["const"].(map[string]any)
	if _, ok := inner["enum"]; !ok {
		t.Fatalf("nested const not converted: %v", inner)
	}
	if _, ok := props["list"].(map[string]any)["items"].(map[string]any)["examples"]; ok {
		t.Fatal("items not cleaned")
	}
	if _, ok := got["additionalProperties"]; ok {
		t.Fatal("top-level additionalProperties not stripped")
	}
	low := string(lowerSchemaTypes(json.RawMessage(`{"type":"OBJECT","properties":{"type":{"type":"STRING","enum":["OBJECT"]}}}`)))
	if low != `{"properties":{"type":{"enum":["OBJECT"],"type":"string"}},"type":"object"}` {
		t.Fatal(low)
	}
}

// L12: one SSE line can't grow without bound.
func TestL12FrameLineCap(t *testing.T) {
	fr := NewFrameReader(strings.NewReader("data: " + strings.Repeat("x", MaxFrameLine+10) + "\n\n"))
	if _, err := fr.Next(); err != ErrFrameTooLarge {
		t.Fatalf("err %v", err)
	}
}
