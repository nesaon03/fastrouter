package translate

import (
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
