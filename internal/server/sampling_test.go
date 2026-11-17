package server

import "testing"

func TestDropSamplingForOpenAIReasoning(t *testing.T) {
	for m, want := range map[string]bool{"gpt-5-mini": true, "gpt-5.1": true, "o3": true, "o4-mini": true, "gpt-5-chat-latest": false, "gpt-4.1": false, "omni": false} {
		if openAIReasoningModel(m) != want {
			t.Errorf("%s: want %v", m, want)
		}
	}
	got := string(dropSampling([]byte(`{"model":"m","temperature":0,"top_p":1,"messages":[{"role":"user","content":"temperature"}]}`)))
	if got != `{"messages":[{"role":"user","content":"temperature"}],"model":"m","top_p":1}` {
		t.Fatal(got)
	}
	if b := `{"model":"m","messages":[]}`; string(dropSampling([]byte(b))) != b {
		t.Fatal("changed a body without sampling fields")
	}
	one := 1.0
	if keepDefault(&one) == nil || keepDefault(new(float64)) != nil || keepDefault(nil) != nil {
		t.Fatal("keepDefault")
	}
}
