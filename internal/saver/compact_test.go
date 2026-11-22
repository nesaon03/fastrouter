package saver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tr "github.com/nesaon03/fastrouter/internal/translate"
)

func TestCompactKeepsLastToolResults(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"do"},
		{"role":"tool","content":"` + strings.Repeat("old output 1 ", 8) + `"},
		{"role":"tool","content":"` + strings.Repeat("old output 2 ", 8) + `"},
		{"role":"tool","content":"` + strings.Repeat("new output 3 ", 8) + `"},
		{"role":"assistant","content":"done"}]}`)
	out := compactBody(body, 1, false)
	var d struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(d.Messages) != 6 {
		t.Fatalf("messages = %d", len(d.Messages))
	}
	if !strings.Contains(d.Messages[2].Content, "trimmed") || !strings.Contains(d.Messages[3].Content, "trimmed") {
		t.Fatalf("old tool output kept: %+v", d.Messages[2:4])
	}
	if d.Messages[4].Content != strings.Repeat("new output 3 ", 8) {
		t.Fatalf("recent tool output changed: %q", d.Messages[4].Content)
	}
	if d.Messages[0].Content != "sys" || d.Messages[5].Content != "done" {
		t.Fatalf("non-tool messages changed: %+v", d.Messages)
	}
}

func TestCompactAnthropicErrorUntouched(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"tool_result","content":"error text","is_error":true}]},
		{"role":"user","content":[{"type":"tool_result","content":"` + strings.Repeat("old text ", 8) + `"}]},
		{"role":"user","content":[{"type":"tool_result","content":"` + strings.Repeat("new text ", 8) + `"}]}]}`)
	out := compactBody(body, 1, false)
	if !strings.Contains(string(out), "error text") {
		t.Fatalf("error result was trimmed:\n%s", out)
	}
	if !strings.Contains(string(out), "older tool output trimmed") {
		t.Fatalf("old result not trimmed:\n%s", out)
	}
	if !strings.Contains(string(out), "new text") {
		t.Fatalf("recent result changed:\n%s", out)
	}
}

func TestCompactDedupe(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"tool","content":"` + strings.Repeat("same output ", 8) + `"},
		{"role":"tool","content":"` + strings.Repeat("other output ", 8) + `"},
		{"role":"tool","content":"` + strings.Repeat("same output ", 8) + `"}]}`)
	out := compactBody(body, 0, true)
	if strings.Count(string(out), strings.Repeat("same output ", 8)) != 1 {
		t.Fatalf("duplicate kept:\n%s", out)
	}
	if !strings.Contains(string(out), "duplicate of an earlier tool output") {
		t.Fatalf("no duplicate marker:\n%s", out)
	}
	if !strings.Contains(string(out), "other output") {
		t.Fatalf("distinct output changed:\n%s", out)
	}
}

func TestCompactResponsesInput(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call_output","output":"` + strings.Repeat("old ", 20) + `"},
		{"type":"function_call_output","output":"` + strings.Repeat("new ", 20) + `"}]}`)
	out := compactBody(body, 1, false)
	if !strings.Contains(string(out), "trimmed") || !strings.Contains(string(out), `"`+strings.Repeat("new ", 20)+`"`) {
		t.Fatalf("responses compaction:\n%s", out)
	}
}

func TestApplyStatsAndTag(t *testing.T) {
	body := []byte(`{"messages":[{"role":"tool","content":"` + strings.Repeat("old ", 40) + `"},{"role":"tool","content":"new"}]}`)
	s := &Saver{CompactKeep: 1, caveman: "be terse", CavemanLevel: "full"}
	out, st := s.Apply(context.Background(), body, tr.OpenAI, "m")
	if st.CompactIn == 0 || st.CompactOut >= st.CompactIn {
		t.Fatalf("compact stats = %+v", st)
	}
	if st.PromptAdded == 0 {
		t.Fatalf("prompt bytes not counted: %+v", st)
	}
	if !strings.Contains(st.Tag, "COMPACT:") || !strings.Contains(st.Tag, "CAVEMAN:full") {
		t.Fatalf("tag = %q", st.Tag)
	}
	if st.NetBytes() >= st.SavedBytes() {
		t.Fatalf("net should subtract the prompt: saved=%d net=%d", st.SavedBytes(), st.NetBytes())
	}
	if !strings.Contains(string(out), "be terse") {
		t.Fatalf("caveman prompt missing:\n%s", out)
	}
}

func TestPreviewRTK(t *testing.T) {
	in := loadVectors(t).RTK[2].In // git status
	out, filter, ok := PreviewRTK(in)
	if !ok || filter == "" || len(out) >= len(in) {
		t.Fatalf("preview: ok=%v filter=%q out=%q", ok, filter, out)
	}
}
