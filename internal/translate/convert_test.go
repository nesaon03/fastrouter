package translate

import (
	"os"
	"reflect"
	"testing"
)

// A stream collected into a response must equal the non-stream response, and
// replaying that response as events must collect back to the same thing.
func TestCollectorAndResponseEvents(t *testing.T) {
	for _, f := range []Format{OpenAI, Anthropic, Gemini, Responses} {
		raw, err := os.ReadFile("testdata/stream." + f.String() + ".sse")
		if err != nil {
			t.Fatal(err)
		}
		var c Collector
		c.Add(decodeAll(t, f, raw))
		got := c.Response()
		if !got.HasContent() || got.Usage.Input != 120 || got.Finish != FinishToolCalls {
			t.Fatalf("%v: collected %+v", f, got)
		}
		var c2 Collector
		c2.Add(ResponseEvents(got))
		if !reflect.DeepEqual(summarizeResponse(c2.Response()), summarizeResponse(got)) {
			t.Fatalf("%v: replay mismatch\n%v\n%v", f, summarizeResponse(c2.Response()), summarizeResponse(got))
		}
	}
}
