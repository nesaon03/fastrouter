package translate

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ---------- request ----------

type anThinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system"`
	Messages      []anMessage     `json:"messages"`
	Tools         []anTool        `json:"tools"`
	ToolChoice    *anToolChoice   `json:"tool_choice"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Thinking      *anThinking     `json:"thinking"`
	OutputConfig  *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type anMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Source       *anSource       `json:"source,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

func anBlocks(raw json.RawMessage) []anBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		if s := rawString(raw); s != "" {
			return []anBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var bs []anBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func anImage(src *anSource, cc json.RawMessage) (Part, bool) {
	if src == nil {
		return Part{}, false
	}
	switch src.Type {
	case "base64":
		return Part{Type: PartImage, MediaType: src.MediaType, Data: src.Data, CacheControl: cc}, true
	case "url":
		return Part{Type: PartImage, URL: src.URL, MediaType: guessMime(src.URL), CacheControl: cc}, true
	}
	return Part{}, false
}

func anToParts(bs []anBlock) []Part {
	out := make([]Part, 0, len(bs))
	for _, b := range bs {
		switch b.Type {
		case "text":
			out = append(out, Part{Type: PartText, Text: b.Text, CacheControl: b.CacheControl})
		case "image", "document":
			if p, ok := anImage(b.Source, b.CacheControl); ok {
				out = append(out, p)
			}
		case "thinking":
			out = append(out, Part{Type: PartThinking, Text: b.Thinking, Signature: b.Signature})
		case "redacted_thinking":
			out = append(out, Part{Type: PartRedactedThinking, Data: b.Data})
		case "tool_use":
			out = append(out, Part{Type: PartToolCall, ToolCallID: b.ID, ToolName: b.Name, Args: compactArgs(b.Input), CacheControl: b.CacheControl})
		case "tool_result":
			out = append(out, Part{Type: PartToolResult, ToolCallID: b.ToolUseID, Result: anToParts(anBlocks(b.Content)), IsError: b.IsError, CacheControl: b.CacheControl})
		}
	}
	return out
}

// stripBillingHeader drops Claude Code's billing marker line from system text (as 9Router does).
func stripBillingHeader(s string) string {
	if strings.HasPrefix(strings.ToLower(s), "x-anthropic-billing-header:") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[i+1:]
		}
		return ""
	}
	return s
}

func parseAnthropicRequest(body []byte) (*Request, error) {
	var in anRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Request{Model: in.Model, MaxTokens: in.MaxTokens, Temperature: in.Temperature, TopP: in.TopP, TopK: in.TopK,
		Stop: in.StopSequences, Stream: in.Stream}
	for _, b := range anBlocks(in.System) {
		if b.Type == "text" {
			t := stripBillingHeader(b.Text)
			if t == "" {
				if n := len(r.System); n > 0 && len(b.CacheControl) > 0 {
					r.System[n-1].CacheControl = b.CacheControl
				}
				continue
			}
			r.System = append(r.System, Part{Type: PartText, Text: t, CacheControl: b.CacheControl})
		}
	}
	for _, m := range in.Messages {
		r.Messages = append(r.Messages, Message{Role: m.Role, Parts: anToParts(anBlocks(m.Content))})
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "custom" {
			continue // server tools (web_search, bash...) have no portable equivalent
		}
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema, CacheControl: t.CacheControl})
	}
	if tc := in.ToolChoice; tc != nil {
		switch tc.Type {
		case "any":
			r.ToolChoice = &ToolChoice{Mode: "required"}
		case "tool":
			r.ToolChoice = &ToolChoice{Mode: "tool", Name: tc.Name}
		default:
			r.ToolChoice = &ToolChoice{Mode: tc.Type}
		}
		if tc.DisableParallelToolUse {
			f := false
			r.ParallelToolCalls = &f
		}
	}
	if in.Thinking != nil {
		r.Thinking = &Thinking{Mode: in.Thinking.Type, Budget: in.Thinking.BudgetTokens}
	}
	if in.OutputConfig != nil && in.OutputConfig.Effort != "" {
		if r.Thinking == nil {
			r.Thinking = &Thinking{}
		}
		r.Thinking.Effort = in.OutputConfig.Effort
	}
	if in.Metadata != nil {
		r.User = in.Metadata.UserID
	}
	return r, nil
}

type anOutRequest struct {
	Model         string        `json:"model"`
	System        []anBlock     `json:"system,omitempty"`
	Messages      []anOutMsg    `json:"messages"`
	Tools         []anTool      `json:"tools,omitempty"`
	ToolChoice    *anToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int           `json:"max_tokens"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	TopK          *int          `json:"top_k,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
	Thinking      *anThinking   `json:"thinking,omitempty"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata,omitempty"`
}

type anOutMsg struct {
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

func partsToAn(parts []Part) []anBlock {
	out := make([]anBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case PartText:
			if p.Text == "" {
				continue // Anthropic rejects empty text blocks
			}
			out = append(out, anBlock{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
		case PartImage:
			src := &anSource{Type: "base64", MediaType: p.MediaType, Data: p.Data}
			if p.Data == "" {
				src = &anSource{Type: "url", URL: p.URL}
			}
			typ := "image"
			if p.MediaType == "application/pdf" {
				typ = "document"
			}
			out = append(out, anBlock{Type: typ, Source: src, CacheControl: p.CacheControl})
		case PartThinking:
			out = append(out, anBlock{Type: "thinking", Thinking: p.Text, Signature: p.Signature})
		case PartRedactedThinking:
			out = append(out, anBlock{Type: "redacted_thinking", Data: p.Data})
		case PartToolCall:
			out = append(out, anBlock{Type: "tool_use", ID: p.ToolCallID, Name: p.ToolName, Input: argsObject(p.Args), CacheControl: p.CacheControl})
		case PartToolResult:
			content, _ := json.Marshal(partsToAn(p.Result))
			if len(p.Result) == 1 && p.Result[0].Type == PartText {
				content, _ = json.Marshal(p.Result[0].Text)
			} else if len(p.Result) == 0 {
				content = json.RawMessage(`""`)
			}
			out = append(out, anBlock{Type: "tool_result", ToolUseID: p.ToolCallID, Content: content, IsError: p.IsError, CacheControl: p.CacheControl})
		}
	}
	return out
}

func emitAnthropicRequest(r *Request, o EmitOptions) ([]byte, error) {
	out := anOutRequest{Model: o.Model, MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK,
		StopSequences: r.Stop, Stream: r.Stream}
	for _, p := range r.System {
		if p.Text != "" {
			out.System = append(out.System, anBlock{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
		}
	}
	keepSig := r.keepSignatures(Anthropic)
	for _, m := range r.Messages {
		blocks := partsToAn(m.Parts)
		if !keepSig {
			for i := range blocks {
				blocks[i].Signature = ""
			}
		}
		if m.Role != "assistant" {
			// tool results must lead the user turn
			var results, rest []anBlock
			for _, b := range blocks {
				if b.Type == "tool_result" {
					results = append(results, b)
				} else {
					rest = append(rest, b)
				}
			}
			blocks = append(results, rest...)
		} else if !thinkingAllowed(r) {
			blocks = dropUnsignedThinking(blocks, false)
		} else {
			blocks = dropUnsignedThinking(blocks, true)
		}
		if len(blocks) == 0 {
			continue
		}
		// merge consecutive same-role turns
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == m.Role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			continue
		}
		out.Messages = append(out.Messages, anOutMsg{Role: m.Role, Content: blocks})
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, anTool{Name: t.Name, Description: t.Description, InputSchema: schemaOrEmpty(t.Schema), CacheControl: t.CacheControl})
	}
	if tc := r.ToolChoice; tc != nil && len(out.Tools) > 0 {
		switch tc.Mode {
		case "required":
			out.ToolChoice = &anToolChoice{Type: "any"}
		case "tool":
			out.ToolChoice = &anToolChoice{Type: "tool", Name: tc.Name}
		case "none":
			out.ToolChoice = &anToolChoice{Type: "none"}
		default:
			out.ToolChoice = &anToolChoice{Type: "auto"}
		}
	}
	if r.ParallelToolCalls != nil && !*r.ParallelToolCalls && len(out.Tools) > 0 {
		if out.ToolChoice == nil {
			out.ToolChoice = &anToolChoice{Type: "auto"}
		}
		out.ToolChoice.DisableParallelToolUse = true
	}
	if t := r.Thinking; t != nil && !lastToolTurnSigned(out.Messages) {
		t = nil // Anthropic rejects thinking when the open tool turn has no signed thinking block
		out.Messages = stripThinking(out.Messages)
	} else if t != nil {
		switch b := t.BudgetFor(); {
		case b == 0:
			out.Thinking = &anThinking{Type: "disabled"}
		case t.Mode == "adaptive":
			out.Thinking = &anThinking{Type: "adaptive"}
		case b > 0:
			b = max(b, 1024)
			out.Thinking = &anThinking{Type: "enabled", BudgetTokens: b}
			if out.MaxTokens <= b {
				out.MaxTokens = b + 8192
			}
			out.Temperature, out.TopK = nil, nil // not allowed with thinking
		}
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 8192
	}
	if r.User != "" {
		out.Metadata = &struct {
			UserID string `json:"user_id"`
		}{r.User}
	}
	return json.Marshal(out)
}

// lastToolTurnSigned reports whether the last assistant turn, if it used
// tools, starts with a thinking block (required when thinking is on).
func lastToolTurnSigned(msgs []anOutMsg) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		hasTool := false
		for _, b := range msgs[i].Content {
			if b.Type == "tool_use" {
				hasTool = true
			}
		}
		if !hasTool {
			return true
		}
		first := msgs[i].Content[0].Type
		return first == "thinking" || first == "redacted_thinking"
	}
	return true
}

func stripThinking(msgs []anOutMsg) []anOutMsg {
	for i := range msgs {
		msgs[i].Content = dropUnsignedThinking(msgs[i].Content, false)
	}
	return msgs
}

func thinkingAllowed(r *Request) bool { return r.Thinking != nil && r.Thinking.BudgetFor() != 0 }

// dropUnsignedThinking removes thinking blocks Anthropic would reject: all of
// them when thinking is off, and unsigned ones (from other providers) when on.
func dropUnsignedThinking(bs []anBlock, keepSigned bool) []anBlock {
	out := bs[:0:0]
	for _, b := range bs {
		if b.Type == "thinking" && (!keepSigned || b.Signature == "") {
			continue
		}
		if b.Type == "redacted_thinking" && !keepSigned {
			continue
		}
		out = append(out, b)
	}
	return out
}

// ---------- response ----------

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

func (u *anUsage) toUsage() Usage {
	return Usage{Input: u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens, Output: u.OutputTokens,
		CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
}

func fromUsageAn(u Usage) anUsage {
	return anUsage{InputTokens: max(0, u.Input-u.CacheRead-u.CacheWrite), OutputTokens: u.Output,
		CacheReadInputTokens: u.CacheRead, CacheCreationInputTokens: u.CacheWrite}
}

type anResponse struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Role       string    `json:"role"`
	Model      string    `json:"model"`
	Content    []anBlock `json:"content"`
	StopReason *string   `json:"stop_reason"`
	StopSeq    *string   `json:"stop_sequence"`
	Usage      anUsage   `json:"usage"`
}

func anFinish(s string) Finish {
	switch s {
	case "":
		return FinishNone
	case "max_tokens", "model_context_window_exceeded":
		return FinishLength
	case "tool_use":
		return FinishToolCalls
	case "refusal":
		return FinishContentFilter
	}
	return FinishStop
}

func toAnFinish(f Finish) string {
	return [...]string{"end_turn", "end_turn", "max_tokens", "tool_use", "refusal"}[f]
}

func parseAnthropicResponse(body []byte) (*Response, error) {
	var in anResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Response{ID: in.ID, Model: in.Model, Parts: anToParts(in.Content), Usage: in.Usage.toUsage()}
	if in.StopReason != nil {
		r.Finish = anFinish(*in.StopReason)
	}
	return r, nil
}

func emitAnthropicResponse(r *Response, model string) ([]byte, error) {
	stop := toAnFinish(r.Finish)
	content := partsToAn(r.Parts)
	if content == nil {
		content = []anBlock{}
	}
	return json.Marshal(anResponse{ID: anID(r.ID), Type: "message", Role: "assistant", Model: firstNonEmpty(r.Model, model),
		Content: content, StopReason: &stop, Usage: fromUsageAn(r.Usage)})
}

func anID(id string) string {
	if id == "" {
		return genID("msg_", 0)
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}

// ---------- stream ----------

type anEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string  `json:"id"`
		Model string  `json:"model"`
		Usage anUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *anBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
		StopDetails *struct {
			Explanation string `json:"explanation"`
		} `json:"stop_details"`
	} `json:"delta"`
	Usage *anUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicDecoder struct {
	usage    Usage
	tools    map[int]int // block index -> tool index
	nextTool int
	finish   Finish
	finished bool
}

func (d *anthropicDecoder) Decode(f Frame) ([]Event, error) {
	if len(f.Data) == 0 {
		return nil, nil
	}
	var e anEvent
	if err := json.Unmarshal(f.Data, &e); err != nil {
		return nil, err
	}
	switch e.Type {
	case "message_start":
		if e.Message == nil {
			return nil, nil
		}
		d.usage = e.Message.Usage.toUsage()
		return []Event{{Kind: EvStart, ID: e.Message.ID, Name: e.Message.Model}, {Kind: EvUsage, Usage: d.usage}}, nil
	case "content_block_start":
		b := e.ContentBlock
		if b == nil {
			return nil, nil
		}
		switch b.Type {
		case "text":
			if b.Text != "" {
				return []Event{{Kind: EvText, Text: b.Text}}, nil
			}
		case "thinking":
			if b.Thinking != "" {
				return []Event{{Kind: EvThinking, Text: b.Thinking}}, nil
			}
		case "redacted_thinking":
			return []Event{{Kind: EvRedacted, Text: b.Data}}, nil
		case "tool_use":
			if d.tools == nil {
				d.tools = map[int]int{}
			}
			idx := d.nextTool
			d.nextTool++
			d.tools[e.Index] = idx
			evs := []Event{{Kind: EvToolStart, Index: idx, ID: b.ID, Name: b.Name}}
			if len(b.Input) > 2 && string(b.Input) != "{}" {
				evs = append(evs, Event{Kind: EvToolArgs, Index: idx, Text: string(b.Input)})
			}
			return evs, nil
		}
	case "content_block_delta":
		dl := e.Delta
		if dl == nil {
			return nil, nil
		}
		switch dl.Type {
		case "text_delta":
			return []Event{{Kind: EvText, Text: dl.Text}}, nil
		case "thinking_delta":
			return []Event{{Kind: EvThinking, Text: dl.Thinking}}, nil
		case "signature_delta":
			return []Event{{Kind: EvSignature, Text: dl.Signature}}, nil
		case "input_json_delta":
			if idx, ok := d.tools[e.Index]; ok && dl.PartialJSON != "" {
				return []Event{{Kind: EvToolArgs, Index: idx, Text: dl.PartialJSON}}, nil
			}
		}
	case "message_delta":
		var evs []Event
		if e.Usage != nil {
			u := *e.Usage
			if u.InputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
				d.usage.Output = u.OutputTokens
			} else {
				d.usage = u.toUsage()
			}
			evs = append(evs, Event{Kind: EvUsage, Usage: d.usage})
		}
		if e.Delta != nil && e.Delta.StopReason != "" {
			if e.Delta.StopReason == "refusal" && e.Delta.StopDetails != nil && e.Delta.StopDetails.Explanation != "" {
				evs = append(evs, Event{Kind: EvText, Text: e.Delta.StopDetails.Explanation})
			}
			d.finish = anFinish(e.Delta.StopReason)
			d.finished = true
			evs = append(evs, Event{Kind: EvFinish, Finish: d.finish})
		}
		return evs, nil
	case "error":
		msg := "upstream error"
		if e.Error != nil {
			msg = e.Error.Message
		}
		return []Event{{Kind: EvError, Text: msg}}, nil
	}
	return nil, nil
}

func (d *anthropicDecoder) End() []Event {
	if !d.finished && d.tools != nil {
		d.finished = true
		return []Event{{Kind: EvFinish, Finish: FinishToolCalls}}
	}
	return nil
}

type anthropicEncoder struct {
	out          sseBuf
	id           string
	model        string
	started      bool
	block        int    // index of the open block, -1 if none
	open         string // type of the open block
	openTool     int
	next         int
	usage        Usage
	finish       Finish
	finished     bool
	pendingStart bool
	// H2: while one tool_use block streams, later tools are buffered and sent
	// as whole blocks when the tool run ends, so interleaved argument deltas
	// are never dropped.
	queued   []*queuedTool
	queuedBy map[int]*queuedTool
}

type queuedTool struct {
	id, name string
	args     strings.Builder
}

func newAnthropicEncoder(model string) *anthropicEncoder {
	return &anthropicEncoder{model: model, block: -1}
}

func (e *anthropicEncoder) startMsg() {
	if e.started {
		return
	}
	e.started = true
	u := fromUsageAn(e.usage)
	u.OutputTokens = 0
	e.out.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": anID(e.id), "type": "message", "role": "assistant", "model": e.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": u}})
}

// flushTools closes the live tool block and sends the queued tools whole.
func (e *anthropicEncoder) flushTools() {
	if len(e.queued) == 0 {
		return
	}
	e.closeBlock()
	for _, q := range e.queued {
		e.openBlock("tool_use", map[string]any{"type": "tool_use", "id": q.id, "name": q.name, "input": map[string]any{}})
		if q.args.Len() > 0 {
			e.delta("input_json_delta", "partial_json", q.args.String())
		}
		e.closeBlock()
	}
	e.queued, e.queuedBy = nil, nil
}

func (e *anthropicEncoder) closeBlock() {
	if e.block < 0 {
		return
	}
	e.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.block})
	e.block, e.open = -1, ""
}

func (e *anthropicEncoder) openBlock(typ string, block map[string]any) {
	e.closeBlock()
	e.block, e.open = e.next, typ
	e.next++
	e.out.event("content_block_start", map[string]any{"type": "content_block_start", "index": e.block, "content_block": block})
}

// delta writes a content_block_delta by hand (the hot path of every stream);
// keys are in the same sorted order encoding/json uses for maps.
func (e *anthropicEncoder) delta(typ, key, val string) {
	b := append(e.out.b, "event: content_block_delta\ndata: {\"delta\":{\""...)
	b = append(b, key...)
	b = append(b, "\":"...)
	b = appendJSONString(b, val)
	b = append(b, ",\"type\":\""...)
	b = append(b, typ...)
	b = append(b, "\"},\"index\":"...)
	b = strconv.AppendInt(b, int64(e.block), 10)
	e.out.b = append(b, ",\"type\":\"content_block_delta\"}\n\n"...)
}

func (e *anthropicEncoder) Encode(evs []Event) []byte {
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
			e.usage = ev.Usage
			continue
		}
		e.startMsg()
		if ev.Kind != EvToolStart && ev.Kind != EvToolArgs && ev.Kind != EvFinish && ev.Kind != EvError {
			e.flushTools()
		}
		switch ev.Kind {
		case EvText:
			if e.open != "text" {
				e.openBlock("text", map[string]any{"type": "text", "text": ""})
			}
			e.delta("text_delta", "text", ev.Text)
		case EvThinking:
			if e.open != "thinking" {
				e.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
			e.delta("thinking_delta", "thinking", ev.Text)
		case EvSignature:
			if ev.Name == "tool" {
				continue // Gemini function-call signature; no Anthropic equivalent
			}
			if e.open != "thinking" {
				e.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
			e.delta("signature_delta", "signature", ev.Text)
		case EvRedacted:
			e.openBlock("redacted_thinking", map[string]any{"type": "redacted_thinking", "data": ev.Text})
			e.closeBlock()
		case EvToolStart:
			if e.open == "tool_use" || len(e.queued) > 0 {
				q := &queuedTool{id: ev.ID, name: ev.Name}
				if e.queuedBy == nil {
					e.queuedBy = map[int]*queuedTool{}
				}
				e.queued, e.queuedBy[ev.Index] = append(e.queued, q), q
				continue
			}
			e.openBlock("tool_use", map[string]any{"type": "tool_use", "id": ev.ID, "name": ev.Name, "input": map[string]any{}})
			e.openTool = ev.Index
		case EvToolArgs:
			if q := e.queuedBy[ev.Index]; q != nil {
				q.args.WriteString(ev.Text)
			} else if e.open == "tool_use" && e.openTool == ev.Index {
				e.delta("input_json_delta", "partial_json", ev.Text)
			}
		case EvFinish:
			e.finish = ev.Finish
		case EvError:
			// reported by Fail, which the proxy calls instead of Close
		}
	}
	return e.out.take()
}

func (e *anthropicEncoder) Fail(msg string) []byte {
	e.flushTools()
	e.closeBlock()
	e.out.event("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}})
	return e.out.take()
}

func (e *anthropicEncoder) Close() []byte {
	e.startMsg()
	e.flushTools()
	e.closeBlock()
	f := e.finish
	if f == FinishNone {
		f = FinishStop
	}
	e.out.event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": toAnFinish(f), "stop_sequence": nil}, "usage": fromUsageAn(e.usage)})
	e.out.event("message_stop", map[string]any{"type": "message_stop"})
	return e.out.take()
}
