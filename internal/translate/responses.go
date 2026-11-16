package translate

import (
	"encoding/json"
	"strings"
)

// ---------- request ----------

type rsRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions"`
	Tools             []rsTool        `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	MaxOutputTokens   int             `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Reasoning         *rsReasoning    `json:"reasoning"`
	User              string          `json:"user"`
	Text              *rsText         `json:"text"`
}

type rsReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type rsText struct {
	Format *struct {
		Type   string          `json:"type"`
		Name   string          `json:"name,omitempty"`
		Schema json.RawMessage `json:"schema,omitempty"`
	} `json:"format,omitempty"`
}

type rsTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type rsSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rsItem struct {
	Type             string          `json:"type,omitempty"`
	ID               string          `json:"id,omitempty"`
	Role             string          `json:"role,omitempty"`
	Status           string          `json:"status,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        string          `json:"arguments,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	Summary          []rsSummary     `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
}

type rsContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
}

func rsParts(raw json.RawMessage) []Part {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		if s := rawString(raw); s != "" {
			return []Part{{Type: PartText, Text: s}}
		}
		return nil
	}
	var cps []rsContentPart
	_ = json.Unmarshal(raw, &cps)
	out := make([]Part, 0, len(cps))
	for _, c := range cps {
		switch c.Type {
		case "input_text", "output_text", "text":
			out = append(out, Part{Type: PartText, Text: c.Text})
		case "refusal":
			out = append(out, Part{Type: PartText, Text: c.Refusal})
		case "input_image":
			if c.ImageURL != "" {
				out = append(out, imagePart(c.ImageURL, c.Detail))
			}
		}
	}
	return out
}

func parseResponsesRequest(body []byte) (*Request, error) {
	var in rsRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Request{Model: in.Model, MaxTokens: in.MaxOutputTokens, Temperature: in.Temperature, TopP: in.TopP,
		Stream: in.Stream, ParallelToolCalls: in.ParallelToolCalls, User: in.User}
	if in.Instructions != "" {
		r.System = append(r.System, Part{Type: PartText, Text: in.Instructions})
	}
	var items []rsItem
	if len(in.Input) > 0 && in.Input[0] == '"' {
		items = []rsItem{{Type: "message", Role: "user", Content: in.Input}}
	} else {
		_ = json.Unmarshal(in.Input, &items)
	}
	add := func(role string, p ...Part) {
		if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == role {
			last := &r.Messages[n-1]
			// a real user message after tool results starts a new turn
			if role == "user" && allToolResults(last.Parts) != allToolResults(p) {
				r.Messages = append(r.Messages, Message{Role: role, Parts: p})
				return
			}
			last.Parts = append(last.Parts, p...)
			return
		}
		r.Messages = append(r.Messages, Message{Role: role, Parts: p})
	}
	for _, it := range items {
		switch it.Type {
		case "", "message":
			switch it.Role {
			case "system", "developer":
				r.System = append(r.System, rsParts(it.Content)...)
			case "assistant":
				add("assistant", rsParts(it.Content)...)
			default:
				r.Messages = append(r.Messages, Message{Role: "user", Parts: rsParts(it.Content)})
			}
		case "function_call":
			add("assistant", Part{Type: PartToolCall, ToolCallID: it.CallID, ToolName: it.Name, Args: firstNonEmpty(it.Arguments, "{}")})
		case "function_call_output":
			out := rsParts(it.Output)
			if len(it.Output) > 0 && it.Output[0] != '"' && it.Output[0] != '[' {
				out = []Part{{Type: PartText, Text: string(it.Output)}}
			}
			add("user", Part{Type: PartToolResult, ToolCallID: it.CallID, Result: out})
		case "reasoning":
			var b strings.Builder
			for _, s := range it.Summary {
				b.WriteString(s.Text)
			}
			add("assistant", Part{Type: PartThinking, Text: b.String(), Signature: it.EncryptedContent})
		}
	}
	for _, t := range in.Tools {
		if t.Type == "function" {
			r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Schema: t.Parameters})
		}
	}
	r.ToolChoice = parseOAToolChoice(in.ToolChoice)
	if in.Reasoning != nil && in.Reasoning.Effort != "" {
		r.Thinking = &Thinking{Effort: in.Reasoning.Effort}
	}
	if in.Text != nil && in.Text.Format != nil && in.Text.Format.Type != "text" {
		r.JSONMode, r.JSONSchema, r.SchemaName = true, in.Text.Format.Schema, in.Text.Format.Name
	}
	return r, nil
}

type rsRequestOut struct {
	Model             string       `json:"model"`
	Input             []rsItem     `json:"input"`
	Instructions      string       `json:"instructions,omitempty"`
	Tools             []rsTool     `json:"tools,omitempty"`
	ToolChoice        any          `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool        `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int          `json:"max_output_tokens,omitempty"`
	Temperature       *float64     `json:"temperature,omitempty"`
	TopP              *float64     `json:"top_p,omitempty"`
	Stream            bool         `json:"stream,omitempty"`
	Reasoning         *rsReasoning `json:"reasoning,omitempty"`
	Text              any          `json:"text,omitempty"`
	User              string       `json:"user,omitempty"`
	Store             bool         `json:"store"`
}

func rsContent(parts []Part, assistant bool) json.RawMessage {
	var cps []rsContentPart
	for _, p := range parts {
		switch p.Type {
		case PartText:
			typ := "input_text"
			if assistant {
				typ = "output_text"
			}
			cps = append(cps, rsContentPart{Type: typ, Text: p.Text})
		case PartImage:
			u := p.URL
			if p.Data != "" {
				u = dataURL(p.MediaType, p.Data)
			}
			cps = append(cps, rsContentPart{Type: "input_image", ImageURL: u, Detail: firstNonEmpty(p.Detail, "auto")})
		}
	}
	b, _ := json.Marshal(cps)
	return b
}

func emitResponsesRequest(r *Request, o EmitOptions) ([]byte, error) {
	out := rsRequestOut{Model: o.Model, Instructions: r.SystemText(), MaxOutputTokens: r.MaxTokens, Temperature: r.Temperature,
		TopP: r.TopP, Stream: r.Stream, ParallelToolCalls: r.ParallelToolCalls, User: r.User, Input: []rsItem{}}
	for _, m := range r.Messages {
		var pending []Part
		flush := func() {
			if len(pending) > 0 {
				out.Input = append(out.Input, rsItem{Type: "message", Role: m.Role, Content: rsContent(pending, m.Role == "assistant")})
				pending = nil
			}
		}
		for _, p := range m.Parts {
			switch p.Type {
			case PartText, PartImage:
				pending = append(pending, p)
			case PartToolCall:
				flush()
				out.Input = append(out.Input, rsItem{Type: "function_call", CallID: p.ToolCallID, Name: p.ToolName, Arguments: firstNonEmpty(p.Args, "{}")})
			case PartToolResult:
				flush()
				o, _ := json.Marshal(resultText(p.Result))
				out.Input = append(out.Input, rsItem{Type: "function_call_output", CallID: p.ToolCallID, Output: o})
			}
			// Reasoning items need ids the upstream issued, so foreign thinking is not replayed.
		}
		flush()
	}
	for _, t := range r.Tools {
		f := false
		out.Tools = append(out.Tools, rsTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schemaOrEmpty(t.Schema), Strict: &f})
	}
	if tc := r.ToolChoice; tc != nil {
		switch tc.Mode {
		case "tool":
			out.ToolChoice = map[string]string{"type": "function", "name": tc.Name}
		case "auto", "none", "required":
			out.ToolChoice = tc.Mode
		}
	}
	if t := r.Thinking; t != nil {
		if e := t.EffortFor(); e != "" {
			if e == "max" || e == "xhigh" {
				e = "high"
			}
			out.Reasoning = &rsReasoning{Effort: e}
			if e != "none" {
				out.Reasoning.Summary = "auto"
			}
		}
	}
	if r.JSONMode {
		if len(r.JSONSchema) > 0 {
			out.Text = map[string]any{"format": map[string]any{"type": "json_schema", "name": firstNonEmpty(r.SchemaName, "response"), "schema": r.JSONSchema}}
		} else {
			out.Text = map[string]any{"format": map[string]string{"type": "json_object"}}
		}
	}
	return json.Marshal(out)
}

// ---------- response ----------

type rsUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

func (u *rsUsage) toUsage() Usage {
	return Usage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.InputTokensDetails.CachedTokens, Reasoning: u.OutputTokensDetails.ReasoningTokens}
}

func fromUsageRs(u Usage) *rsUsage {
	out := &rsUsage{InputTokens: u.Input, OutputTokens: u.Output, TotalTokens: u.Input + u.Output}
	out.InputTokensDetails.CachedTokens = u.CacheRead
	out.OutputTokensDetails.ReasoningTokens = u.Reasoning
	return out
}

type rsResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	CreatedAt         int64    `json:"created_at"`
	Status            string   `json:"status"`
	Model             string   `json:"model"`
	Output            []rsItem `json:"output"`
	Usage             *rsUsage `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func rsFinish(status string, incomplete string, sawTool bool) Finish {
	switch {
	case status == "incomplete" && incomplete == "content_filter":
		return FinishContentFilter
	case status == "incomplete":
		return FinishLength
	case sawTool:
		return FinishToolCalls
	}
	return FinishStop
}

func parseResponsesResponse(body []byte) (*Response, error) {
	var in rsResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	return rsToResponse(&in), nil
}

func rsToResponse(in *rsResponse) *Response {
	r := &Response{ID: in.ID, Model: in.Model}
	if in.Usage != nil {
		r.Usage = in.Usage.toUsage()
	}
	sawTool := false
	for _, it := range in.Output {
		switch it.Type {
		case "reasoning":
			var b strings.Builder
			for _, s := range it.Summary {
				b.WriteString(s.Text)
			}
			r.Parts = append(r.Parts, Part{Type: PartThinking, Text: b.String(), Signature: it.EncryptedContent})
		case "message":
			r.Parts = append(r.Parts, rsParts(it.Content)...)
		case "function_call":
			sawTool = true
			r.Parts = append(r.Parts, Part{Type: PartToolCall, ToolCallID: it.CallID, ToolName: it.Name, Args: firstNonEmpty(it.Arguments, "{}")})
		}
	}
	reason := ""
	if in.IncompleteDetails != nil {
		reason = in.IncompleteDetails.Reason
	}
	r.Finish = rsFinish(in.Status, reason, sawTool)
	return r
}

func rsID(id string) string {
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	if id == "" {
		return genID("resp_", 0)
	}
	return "resp_" + strings.TrimPrefix(strings.TrimPrefix(id, "chatcmpl-"), "msg_")
}

func responseObject(id, model string, created int64, status string, items []rsItem, u *Usage, finish Finish) map[string]any {
	if items == nil {
		items = []rsItem{}
	}
	m := map[string]any{"id": id, "object": "response", "created_at": created, "status": status, "model": model,
		"output": items, "parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{}, "incomplete_details": nil, "error": nil}
	if finish == FinishLength || finish == FinishContentFilter {
		m["status"] = "incomplete"
		reason := "max_output_tokens"
		if finish == FinishContentFilter {
			reason = "content_filter"
		}
		m["incomplete_details"] = map[string]string{"reason": reason}
	}
	if u != nil {
		m["usage"] = fromUsageRs(*u)
	} else {
		m["usage"] = nil
	}
	return m
}

func emitResponsesResponse(r *Response, model string) ([]byte, error) {
	var items []rsItem
	var text []Part
	flushText := func() {
		if len(text) > 0 {
			items = append(items, rsItem{Type: "message", ID: genID("msg_", len(items)), Role: "assistant", Status: "completed", Content: rsOutputText(text)})
			text = nil
		}
	}
	for _, p := range r.Parts {
		switch p.Type {
		case PartThinking:
			flushText()
			items = append(items, rsItem{Type: "reasoning", ID: genID("rs_", len(items)), Summary: []rsSummary{{Type: "summary_text", Text: p.Text}}, EncryptedContent: p.Signature})
		case PartText:
			text = append(text, p)
		case PartToolCall:
			flushText()
			items = append(items, rsItem{Type: "function_call", ID: genID("fc_", len(items)), CallID: p.ToolCallID, Name: p.ToolName, Arguments: firstNonEmpty(p.Args, "{}"), Status: "completed"})
		}
	}
	flushText()
	u := r.Usage
	return json.Marshal(responseObject(rsID(r.ID), firstNonEmpty(r.Model, model), nowUnix(), "completed", items, &u, r.Finish))
}

func rsOutputText(parts []Part) json.RawMessage {
	type ot struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}
	var out []ot
	for _, p := range parts {
		out = append(out, ot{"output_text", p.Text, []any{}})
	}
	b, _ := json.Marshal(out)
	return b
}

// ---------- stream ----------

type rsEvent struct {
	Type        string      `json:"type"`
	OutputIndex int         `json:"output_index"`
	Delta       string      `json:"delta"`
	Item        *rsItem     `json:"item"`
	Response    *rsResponse `json:"response"`
	Message     string      `json:"message"`
}

type responsesDecoder struct {
	tools    map[int]int // output index -> tool index
	nextTool int
	sawText  map[int]bool
}

func (d *responsesDecoder) Decode(f Frame) ([]Event, error) {
	if len(f.Data) == 0 || string(f.Data) == "[DONE]" {
		return nil, nil
	}
	var e rsEvent
	if err := json.Unmarshal(f.Data, &e); err != nil {
		return nil, err
	}
	switch e.Type {
	case "response.created":
		if e.Response != nil {
			return []Event{{Kind: EvStart, ID: e.Response.ID, Name: e.Response.Model}}, nil
		}
	case "response.output_item.added":
		if e.Item != nil && e.Item.Type == "function_call" {
			if d.tools == nil {
				d.tools = map[int]int{}
			}
			idx := d.nextTool
			d.nextTool++
			d.tools[e.OutputIndex] = idx
			return []Event{{Kind: EvToolStart, Index: idx, ID: e.Item.CallID, Name: e.Item.Name}}, nil
		}
	case "response.function_call_arguments.delta":
		if idx, ok := d.tools[e.OutputIndex]; ok && e.Delta != "" {
			return []Event{{Kind: EvToolArgs, Index: idx, Text: e.Delta}}, nil
		}
	case "response.output_text.delta", "response.refusal.delta":
		return []Event{{Kind: EvText, Text: e.Delta}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []Event{{Kind: EvThinking, Text: e.Delta}}, nil
	case "response.output_item.done":
		if e.Item != nil && e.Item.Type == "reasoning" && e.Item.EncryptedContent != "" {
			return []Event{{Kind: EvSignature, Text: e.Item.EncryptedContent}}, nil
		}
	case "response.completed", "response.incomplete":
		if e.Response == nil {
			return []Event{{Kind: EvFinish, Finish: FinishStop}}, nil
		}
		r := rsToResponse(e.Response)
		if r.Finish == FinishStop && d.nextTool > 0 {
			r.Finish = FinishToolCalls
		}
		return []Event{{Kind: EvUsage, Usage: r.Usage}, {Kind: EvFinish, Finish: r.Finish}}, nil
	case "response.failed", "error":
		msg := e.Message
		if e.Response != nil && e.Response.Error != nil {
			msg = e.Response.Error.Message
		}
		return []Event{{Kind: EvError, Text: firstNonEmpty(msg, "upstream error")}}, nil
	}
	return nil, nil
}

func (d *responsesDecoder) End() []Event { return nil }

type responsesEncoder struct {
	out     sseBuf
	seq     int
	id      string
	model   string
	created int64
	started bool
	items   []rsItem
	cur     int    // index of the open item, -1 if none
	curType string // reasoning | message | function_call
	text    strings.Builder
	toolIdx map[int]int // tool index -> item index
	// H2: tools that start while another function_call item is streaming are
	// queued and sent as complete items when the tool run ends.
	queued   []*rsQueued
	queuedBy map[int]*rsQueued
	usage    *Usage
	finish   Finish
}

func newResponsesEncoder(model string) *responsesEncoder {
	return &responsesEncoder{model: model, cur: -1}
}

func (e *responsesEncoder) ev(typ string, fields map[string]any) {
	fields["type"] = typ
	fields["sequence_number"] = e.seq
	e.seq++
	e.out.event(typ, fields)
}

func (e *responsesEncoder) start() {
	if e.started {
		return
	}
	e.started = true
	e.id = rsID(e.id)
	e.created = nowUnix()
	resp := responseObject(e.id, e.model, e.created, "in_progress", nil, nil, FinishNone)
	e.ev("response.created", map[string]any{"response": resp})
	e.ev("response.in_progress", map[string]any{"response": resp})
}

func (e *responsesEncoder) closeItem() {
	if e.cur < 0 {
		return
	}
	it := &e.items[e.cur]
	idx := e.cur
	switch e.curType {
	case "reasoning":
		t := e.text.String()
		e.ev("response.reasoning_summary_text.done", map[string]any{"item_id": it.ID, "output_index": idx, "summary_index": 0, "text": t})
		e.ev("response.reasoning_summary_part.done", map[string]any{"item_id": it.ID, "output_index": idx, "summary_index": 0, "part": rsSummary{"summary_text", t}})
		it.Summary = []rsSummary{{"summary_text", t}}
	case "message":
		t := e.text.String()
		part := map[string]any{"type": "output_text", "text": t, "annotations": []any{}}
		e.ev("response.output_text.done", map[string]any{"item_id": it.ID, "output_index": idx, "content_index": 0, "text": t})
		e.ev("response.content_part.done", map[string]any{"item_id": it.ID, "output_index": idx, "content_index": 0, "part": part})
		it.Content = rsOutputText([]Part{{Text: t}})
	case "function_call":
		e.ev("response.function_call_arguments.done", map[string]any{"item_id": it.ID, "output_index": idx, "arguments": it.Arguments})
	}
	it.Status = "completed"
	if it.Type == "reasoning" {
		it.Status = ""
	}
	e.ev("response.output_item.done", map[string]any{"output_index": idx, "item": *it})
	e.cur, e.curType = -1, ""
	e.text.Reset()
}

type rsQueued struct {
	index        int
	callID, name string
	args         strings.Builder
}

// flushTools closes the streaming function_call and sends queued tools whole.
func (e *responsesEncoder) flushTools() {
	if len(e.queued) == 0 {
		return
	}
	q := e.queued
	e.queued, e.queuedBy = nil, nil
	for _, t := range q {
		idx := e.openItem(rsItem{Type: "function_call", ID: genID("fc_", len(e.items)), CallID: t.callID, Name: t.name})
		e.toolIdx[t.index] = idx
		if t.args.Len() > 0 {
			e.items[idx].Arguments = t.args.String()
			e.ev("response.function_call_arguments.delta", map[string]any{"item_id": e.items[idx].ID, "output_index": idx, "delta": e.items[idx].Arguments})
		}
	}
	e.closeItem()
}

func (e *responsesEncoder) openItem(it rsItem) int {
	e.closeItem()
	e.items = append(e.items, it)
	e.cur, e.curType = len(e.items)-1, it.Type
	added := it
	if it.Type == "message" {
		added.Content = json.RawMessage(`[]`)
	}
	if it.Type == "reasoning" {
		added.Summary = []rsSummary{}
	}
	e.ev("response.output_item.added", map[string]any{"output_index": e.cur, "item": rsItemAdded(added)})
	return e.cur
}

// rsItemAdded keeps empty arrays visible in output_item.added.
func rsItemAdded(it rsItem) map[string]any {
	m := map[string]any{"type": it.Type, "id": it.ID}
	switch it.Type {
	case "message":
		m["role"], m["status"], m["content"] = "assistant", "in_progress", []any{}
	case "reasoning":
		m["summary"] = []any{}
	case "function_call":
		m["call_id"], m["name"], m["arguments"], m["status"] = it.CallID, it.Name, "", "in_progress"
	}
	return m
}

func (e *responsesEncoder) Encode(evs []Event) []byte {
	for _, ev := range evs {
		switch ev.Kind {
		case EvStart:
			if ev.ID != "" {
				e.id = ev.ID
			}
			if ev.Name != "" {
				e.model = ev.Name
			}
			continue
		case EvUsage:
			u := ev.Usage
			e.usage = &u
			continue
		}
		e.start()
		if ev.Kind != EvToolStart && ev.Kind != EvToolArgs && ev.Kind != EvFinish && ev.Kind != EvError {
			e.flushTools()
		}
		switch ev.Kind {
		case EvThinking:
			if e.curType != "reasoning" {
				idx := e.openItem(rsItem{Type: "reasoning", ID: genID("rs_", len(e.items))})
				e.ev("response.reasoning_summary_part.added", map[string]any{"item_id": e.items[idx].ID, "output_index": idx, "summary_index": 0, "part": rsSummary{"summary_text", ""}})
			}
			e.text.WriteString(ev.Text)
			e.ev("response.reasoning_summary_text.delta", map[string]any{"item_id": e.items[e.cur].ID, "output_index": e.cur, "summary_index": 0, "delta": ev.Text})
		case EvSignature:
			if ev.Name == "tool" {
				continue
			}
			if e.curType == "reasoning" {
				e.items[e.cur].EncryptedContent += ev.Text
			} else if n := len(e.items); n > 0 && e.items[n-1].Type == "reasoning" {
				e.items[n-1].EncryptedContent += ev.Text
			}
		case EvText:
			if e.curType != "message" {
				idx := e.openItem(rsItem{Type: "message", ID: genID("msg_", len(e.items)), Role: "assistant"})
				e.ev("response.content_part.added", map[string]any{"item_id": e.items[idx].ID, "output_index": idx, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			}
			e.text.WriteString(ev.Text)
			e.ev("response.output_text.delta", map[string]any{"item_id": e.items[e.cur].ID, "output_index": e.cur, "content_index": 0, "delta": ev.Text})
		case EvToolStart:
			if e.curType == "function_call" || len(e.queued) > 0 {
				t := &rsQueued{index: ev.Index, callID: ev.ID, name: ev.Name}
				if e.queuedBy == nil {
					e.queuedBy = map[int]*rsQueued{}
				}
				e.queued, e.queuedBy[ev.Index] = append(e.queued, t), t
				continue
			}
			idx := e.openItem(rsItem{Type: "function_call", ID: genID("fc_", len(e.items)), CallID: ev.ID, Name: ev.Name})
			if e.toolIdx == nil {
				e.toolIdx = map[int]int{}
			}
			e.toolIdx[ev.Index] = idx
		case EvToolArgs:
			if t := e.queuedBy[ev.Index]; t != nil {
				t.args.WriteString(ev.Text)
				continue
			}
			idx, ok := e.toolIdx[ev.Index]
			if !ok {
				continue
			}
			e.items[idx].Arguments += ev.Text
			if idx == e.cur {
				e.ev("response.function_call_arguments.delta", map[string]any{"item_id": e.items[idx].ID, "output_index": idx, "delta": ev.Text})
			}
		case EvFinish:
			e.finish = ev.Finish
		case EvError:
			// reported by Fail, which the proxy calls instead of Close
		}
	}
	return e.out.take()
}

func (e *responsesEncoder) Fail(msg string) []byte {
	e.start()
	e.flushTools()
	e.closeItem()
	resp := responseObject(e.id, e.model, e.created, "completed", e.items, e.usage, e.finish)
	resp["status"] = "failed"
	resp["error"] = map[string]any{"code": "server_error", "message": msg}
	e.ev("response.failed", map[string]any{"response": resp})
	return e.out.take()
}

func (e *responsesEncoder) Close() []byte {
	e.start()
	e.flushTools()
	e.closeItem()
	resp := responseObject(e.id, e.model, e.created, "completed", e.items, e.usage, e.finish)
	typ := "response.completed"
	if resp["status"] == "incomplete" {
		typ = "response.incomplete"
	}
	e.ev(typ, map[string]any{"response": resp})
	return e.out.take()
}
