package translate

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ---------- request ----------

type oaRequest struct {
	Model               string          `json:"model"`
	Messages            []oaMessage     `json:"messages"`
	Tools               []oaTool        `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	TopK                *int            `json:"top_k"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	Reasoning           *struct {
		Effort    string `json:"effort"`
		MaxTokens int    `json:"max_tokens"`
		Enabled   *bool  `json:"enabled"`
	} `json:"reasoning"`
	Thinking       *anThinking `json:"thinking"`
	User           string      `json:"user"`
	ResponseFormat *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

type oaMessage struct {
	Role             string              `json:"role"`
	Content          json.RawMessage     `json:"content"`
	ToolCalls        []oaToolCall        `json:"tool_calls"`
	ToolCallID       string              `json:"tool_call_id"`
	ReasoningContent string              `json:"reasoning_content"`
	Reasoning        string              `json:"reasoning"`
	ReasoningDetails []oaReasoningDetail `json:"reasoning_details"`
}

type oaReasoningDetail struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

type oaContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string          `json:"name,omitempty"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// oaParts reads OpenAI message content (string or parts array).
func oaParts(raw json.RawMessage) []Part {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		if s := rawString(raw); s != "" {
			return []Part{{Type: PartText, Text: s}}
		}
		return nil
	}
	var cps []oaContentPart
	_ = json.Unmarshal(raw, &cps)
	out := make([]Part, 0, len(cps))
	for _, c := range cps {
		switch c.Type {
		case "text", "input_text", "output_text":
			out = append(out, Part{Type: PartText, Text: c.Text, CacheControl: c.CacheControl})
		case "image_url":
			if c.ImageURL != nil {
				p := imagePart(c.ImageURL.URL, c.ImageURL.Detail)
				p.CacheControl = c.CacheControl
				out = append(out, p)
			}
		}
	}
	return out
}

func parseOpenAIRequest(body []byte) (*Request, error) {
	var in oaRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Request{Model: in.Model, Temperature: in.Temperature, TopP: in.TopP, TopK: in.TopK,
		Stream: in.Stream, ParallelToolCalls: in.ParallelToolCalls, User: in.User, MaxTokens: in.MaxTokens}
	if in.MaxCompletionTokens > 0 {
		r.MaxTokens = in.MaxCompletionTokens
	}
	r.Stop = parseStop(in.Stop)
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			r.System = append(r.System, oaParts(m.Content)...)
		case "user":
			r.Messages = append(r.Messages, Message{Role: "user", Parts: oaParts(m.Content)})
		case "assistant":
			var parts []Part
			for _, d := range m.ReasoningDetails {
				switch d.Type {
				case "reasoning.encrypted":
					parts = append(parts, Part{Type: PartRedactedThinking, Data: d.Data})
				default:
					parts = append(parts, Part{Type: PartThinking, Text: d.Text, Signature: d.Signature})
				}
			}
			if len(m.ReasoningDetails) == 0 {
				if t := firstNonEmpty(m.ReasoningContent, m.Reasoning); t != "" {
					parts = append(parts, Part{Type: PartThinking, Text: t})
				}
			}
			parts = append(parts, oaParts(m.Content)...)
			for _, tc := range m.ToolCalls {
				parts = append(parts, Part{Type: PartToolCall, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: compactArgs(tc.Function.Arguments)})
			}
			r.Messages = append(r.Messages, Message{Role: "assistant", Parts: parts})
		case "tool", "function":
			res := Part{Type: PartToolResult, ToolCallID: m.ToolCallID, Result: oaParts(m.Content)}
			// consecutive tool results share one user turn
			if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == "user" && allToolResults(r.Messages[n-1].Parts) {
				r.Messages[n-1].Parts = append(r.Messages[n-1].Parts, res)
			} else {
				r.Messages = append(r.Messages, Message{Role: "user", Parts: []Part{res}})
			}
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		r.Tools = append(r.Tools, Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: t.Function.Parameters, CacheControl: t.CacheControl})
	}
	r.ToolChoice = parseOAToolChoice(in.ToolChoice)
	switch {
	case in.Thinking != nil:
		r.Thinking = &Thinking{Mode: in.Thinking.Type, Budget: in.Thinking.BudgetTokens, Effort: in.ReasoningEffort}
	case in.ReasoningEffort != "":
		r.Thinking = &Thinking{Effort: in.ReasoningEffort}
	case in.Reasoning != nil:
		t := &Thinking{Effort: in.Reasoning.Effort, Budget: in.Reasoning.MaxTokens}
		if in.Reasoning.Enabled != nil && !*in.Reasoning.Enabled {
			t.Mode = "disabled"
		}
		r.Thinking = t
	}
	if rf := in.ResponseFormat; rf != nil && rf.Type != "" && rf.Type != "text" {
		r.JSONMode = true
		if rf.JSONSchema != nil {
			r.JSONSchema, r.SchemaName = rf.JSONSchema.Schema, rf.JSONSchema.Name
		}
	}
	return r, nil
}

func allToolResults(ps []Part) bool {
	for _, p := range ps {
		if p.Type != PartToolResult {
			return false
		}
	}
	return len(ps) > 0
}

func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		return []string{rawString(raw)}
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func parseOAToolChoice(raw json.RawMessage) *ToolChoice {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		return &ToolChoice{Mode: rawString(raw)}
	}
	var v struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &v)
	return &ToolChoice{Mode: "tool", Name: firstNonEmpty(v.Function.Name, v.Name)}
}

// EmitOptions tunes request emission for a target provider.
type EmitOptions struct {
	Model            string // upstream model name
	KeepCacheControl bool   // forward Anthropic cache_control inside OpenAI parts (OpenRouter-style gateways)
	ReasoningFields  bool   // send reasoning_content / thinking extensions (DeepSeek-style providers)
	MaxCompletion    bool   // use max_completion_tokens (api.openai.com)
}

type oaOutMessage struct {
	Role             string              `json:"role"`
	Content          any                 `json:"content"`
	ToolCalls        []oaToolCall        `json:"tool_calls,omitempty"`
	ToolCallID       string              `json:"tool_call_id,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ReasoningDetails []oaReasoningDetail `json:"reasoning_details,omitempty"`
}

type oaOutRequest struct {
	Model               string          `json:"model"`
	Messages            []oaOutMessage  `json:"messages"`
	Tools               []oaTool        `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	TopK                *int            `json:"top_k,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Thinking            *anThinking     `json:"thinking,omitempty"`
	User                string          `json:"user,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

func oaContent(parts []Part, keepCache bool) any {
	simple := true
	for _, p := range parts {
		if p.Type != PartText || (keepCache && len(p.CacheControl) > 0) {
			simple = false
		}
	}
	if simple {
		var b strings.Builder
		for i, p := range parts {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
		return b.String()
	}
	out := make([]oaContentPart, 0, len(parts))
	for _, p := range parts {
		var cp oaContentPart
		switch p.Type {
		case PartText:
			cp = oaContentPart{Type: "text", Text: p.Text}
		case PartImage:
			u := p.URL
			if p.Data != "" {
				u = dataURL(p.MediaType, p.Data)
			}
			cp = oaContentPart{Type: "image_url"}
			cp.ImageURL = &struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			}{u, p.Detail}
		default:
			continue
		}
		if keepCache {
			cp.CacheControl = p.CacheControl
		}
		out = append(out, cp)
	}
	return out
}

func emitOpenAIRequest(r *Request, o EmitOptions) ([]byte, error) {
	out := oaOutRequest{Model: o.Model, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK, Stop: r.Stop,
		Stream: r.Stream, ParallelToolCalls: r.ParallelToolCalls, User: r.User}
	if o.MaxCompletion {
		out.MaxCompletionTokens = r.MaxTokens
	} else {
		out.MaxTokens = r.MaxTokens
	}
	if r.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if len(r.System) > 0 {
		out.Messages = append(out.Messages, oaOutMessage{Role: "system", Content: oaContent(r.System, o.KeepCacheControl)})
	}
	for _, m := range r.Messages {
		if m.Role == "assistant" {
			msg := oaOutMessage{Role: "assistant"}
			var content []Part
			var reasoning strings.Builder
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					content = append(content, p)
				case PartThinking:
					reasoning.WriteString(p.Text)
					if p.Signature != "" && o.ReasoningFields {
						msg.ReasoningDetails = append(msg.ReasoningDetails, oaReasoningDetail{Type: "reasoning.text", Text: p.Text, Signature: p.Signature})
					}
				case PartRedactedThinking:
					if o.ReasoningFields {
						msg.ReasoningDetails = append(msg.ReasoningDetails, oaReasoningDetail{Type: "reasoning.encrypted", Data: p.Data})
					}
				case PartToolCall:
					tc := oaToolCall{ID: p.ToolCallID, Type: "function"}
					tc.Function.Name = p.ToolName
					args, _ := json.Marshal(firstNonEmpty(p.Args, "{}"))
					tc.Function.Arguments = args
					msg.ToolCalls = append(msg.ToolCalls, tc)
				}
			}
			if o.ReasoningFields {
				msg.ReasoningContent = reasoning.String()
			}
			if len(content) > 0 {
				msg.Content = oaContent(content, o.KeepCacheControl)
			} else if len(msg.ToolCalls) == 0 {
				msg.Content = ""
			}
			out.Messages = append(out.Messages, msg)
			continue
		}
		// user turn: tool results first (as tool messages), then the rest
		var rest, toolImages []Part
		for _, p := range m.Parts {
			if p.Type != PartToolResult {
				rest = append(rest, p)
				continue
			}
			text := resultText(p.Result)
			if p.IsError && text == "" {
				text = "error"
			}
			out.Messages = append(out.Messages, oaOutMessage{Role: "tool", ToolCallID: p.ToolCallID, Content: text})
			for _, rp := range p.Result {
				if rp.Type == PartImage {
					toolImages = append(toolImages, rp)
				}
			}
		}
		rest = append(toolImages, rest...)
		if len(rest) > 0 {
			out.Messages = append(out.Messages, oaOutMessage{Role: "user", Content: oaContent(rest, o.KeepCacheControl)})
		}
	}
	fixMissingToolResults(&out)
	for _, t := range r.Tools {
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name, ot.Function.Description = t.Name, t.Description
		ot.Function.Parameters = schemaOrEmpty(t.Schema)
		if o.KeepCacheControl {
			ot.CacheControl = t.CacheControl
		}
		out.Tools = append(out.Tools, ot)
	}
	if tc := r.ToolChoice; tc != nil {
		switch tc.Mode {
		case "tool":
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		case "auto", "none", "required":
			out.ToolChoice = tc.Mode
		}
	}
	if t := r.Thinking; t != nil {
		out.ReasoningEffort = t.EffortFor()
		if out.ReasoningEffort == "none" && !o.ReasoningFields {
			out.ReasoningEffort = ""
		}
		if o.ReasoningFields && (t.Mode == "enabled" || t.Mode == "disabled") {
			out.Thinking = &anThinking{Type: t.Mode}
		}
	}
	if r.JSONMode {
		if len(r.JSONSchema) > 0 {
			out.ResponseFormat, _ = json.Marshal(map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": firstNonEmpty(r.SchemaName, "response"), "schema": r.JSONSchema}})
		} else {
			out.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
		}
	}
	return json.Marshal(out)
}

// fixMissingToolResults adds a placeholder answer for any tool call left
// unanswered, which OpenAI-format upstreams reject (same fix as 9Router).
func fixMissingToolResults(out *oaOutRequest) {
	msgs := out.Messages
	var fixed []oaOutMessage
	for i := 0; i < len(msgs); i++ {
		fixed = append(fixed, msgs[i])
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		j := i + 1
		for ; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			answered[msgs[j].ToolCallID] = true
			fixed = append(fixed, msgs[j])
		}
		for _, tc := range msgs[i].ToolCalls {
			if !answered[tc.ID] {
				fixed = append(fixed, oaOutMessage{Role: "tool", ToolCallID: tc.ID, Content: "[No response received]"})
			}
		}
		i = j - 1
	}
	out.Messages = fixed
}

func schemaOrEmpty(s json.RawMessage) json.RawMessage {
	if len(s) == 0 || string(s) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return s
}

// ---------- response ----------

type oaUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	PromptCacheHitTokens     int `json:"prompt_cache_hit_tokens,omitempty"`     // DeepSeek
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"` // some gateways
}

func (u *oaUsage) toUsage() Usage {
	out := Usage{Input: u.PromptTokens, Output: u.CompletionTokens, CacheRead: u.PromptCacheHitTokens, CacheWrite: u.CacheCreationInputTokens}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

func fromUsageOA(u Usage) *oaUsage {
	out := &oaUsage{PromptTokens: u.Input, CompletionTokens: u.Output, TotalTokens: u.Input + u.Output}
	if u.CacheRead > 0 {
		out.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{u.CacheRead}
	}
	if u.Reasoning > 0 {
		out.CompletionTokensDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{u.Reasoning}
	}
	return out
}

type oaChoiceMsg struct {
	Role             string              `json:"role,omitempty"`
	Content          *string             `json:"content"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	Reasoning        string              `json:"reasoning,omitempty"`
	ReasoningDetails []oaReasoningDetail `json:"reasoning_details,omitempty"`
	ToolCalls        []oaToolCall        `json:"tool_calls,omitempty"`
	Refusal          string              `json:"refusal,omitempty"`
}

type oaResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      oaChoiceMsg `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage,omitempty"`
}

func oaFinish(s string) Finish {
	switch s {
	case "":
		return FinishNone
	case "length":
		return FinishLength
	case "tool_calls", "function_call":
		return FinishToolCalls
	case "content_filter":
		return FinishContentFilter
	}
	return FinishStop
}

func toOAFinish(f Finish) string {
	return [...]string{"stop", "stop", "length", "tool_calls", "content_filter"}[f]
}

func parseOpenAIResponse(body []byte) (*Response, error) {
	var in oaResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Response{ID: in.ID, Model: in.Model}
	if in.Usage != nil {
		r.Usage = in.Usage.toUsage()
	}
	if len(in.Choices) == 0 {
		return r, nil
	}
	c := in.Choices[0]
	m := c.Message
	if len(m.ReasoningDetails) > 0 {
		for _, d := range m.ReasoningDetails {
			if d.Type == "reasoning.encrypted" {
				r.Parts = append(r.Parts, Part{Type: PartRedactedThinking, Data: d.Data})
			} else {
				r.Parts = append(r.Parts, Part{Type: PartThinking, Text: d.Text, Signature: d.Signature})
			}
		}
	} else if t := firstNonEmpty(m.ReasoningContent, m.Reasoning); t != "" {
		r.Parts = append(r.Parts, Part{Type: PartThinking, Text: t})
	}
	if m.Content != nil && *m.Content != "" {
		r.Parts = append(r.Parts, Part{Type: PartText, Text: *m.Content})
	} else if m.Refusal != "" {
		r.Parts = append(r.Parts, Part{Type: PartText, Text: m.Refusal})
	}
	for _, tc := range m.ToolCalls {
		r.Parts = append(r.Parts, Part{Type: PartToolCall, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: compactArgs(tc.Function.Arguments)})
	}
	r.Finish = oaFinish(c.FinishReason)
	if len(m.ToolCalls) > 0 && r.Finish == FinishStop {
		r.Finish = FinishToolCalls
	}
	return r, nil
}

func emitOpenAIResponse(r *Response, model string) ([]byte, error) {
	msg := oaChoiceMsg{Role: "assistant"}
	var text strings.Builder
	var reasoning strings.Builder
	for _, p := range r.Parts {
		switch p.Type {
		case PartText:
			text.WriteString(p.Text)
		case PartThinking:
			reasoning.WriteString(p.Text)
			if p.Signature != "" {
				msg.ReasoningDetails = append(msg.ReasoningDetails, oaReasoningDetail{Type: "reasoning.text", Text: p.Text, Signature: p.Signature})
			}
		case PartRedactedThinking:
			msg.ReasoningDetails = append(msg.ReasoningDetails, oaReasoningDetail{Type: "reasoning.encrypted", Data: p.Data})
		case PartToolCall:
			tc := oaToolCall{ID: p.ToolCallID, Type: "function"}
			tc.Function.Name = p.ToolName
			tc.Function.Arguments, _ = json.Marshal(firstNonEmpty(p.Args, "{}"))
			msg.ToolCalls = append(msg.ToolCalls, tc)
		}
	}
	if text.Len() > 0 || len(msg.ToolCalls) == 0 {
		s := text.String()
		msg.Content = &s
	}
	msg.ReasoningContent = reasoning.String()
	out := struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []any    `json:"choices"`
		Usage   *oaUsage `json:"usage"`
	}{
		ID: oaID(r.ID), Object: "chat.completion", Created: nowUnix(), Model: firstNonEmpty(r.Model, model),
		Choices: []any{map[string]any{"index": 0, "message": msg, "finish_reason": toOAFinish(r.Finish)}},
		Usage:   fromUsageOA(r.Usage),
	}
	return json.Marshal(out)
}

func oaID(id string) string {
	if id == "" {
		return genID("chatcmpl-", 0)
	}
	if strings.HasPrefix(id, "chatcmpl") {
		return id
	}
	return "chatcmpl-" + id
}

// ---------- stream ----------

type oaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          *string             `json:"content"`
			ReasoningContent *string             `json:"reasoning_content"`
			Reasoning        *string             `json:"reasoning"`
			ReasoningDetails []oaReasoningDetail `json:"reasoning_details"`
			ToolCalls        []oaToolCall        `json:"tool_calls"`
			Refusal          *string             `json:"refusal"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type openaiDecoder struct {
	started  bool
	tools    map[int]int // upstream tool index -> our index
	nextTool int
	sawTools bool
	finish   Finish
	finished bool
}

func (d *openaiDecoder) Decode(f Frame) ([]Event, error) {
	data := f.Data
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil, nil
	}
	var c oaChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	var evs []Event
	if c.Error != nil {
		return []Event{{Kind: EvError, Text: c.Error.Message}}, nil
	}
	if !d.started {
		d.started = true
		evs = append(evs, Event{Kind: EvStart, ID: c.ID, Name: c.Model})
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		dl := ch.Delta
		for _, rd := range dl.ReasoningDetails {
			if rd.Text != "" {
				evs = append(evs, Event{Kind: EvThinking, Text: rd.Text})
			}
			if rd.Signature != "" {
				evs = append(evs, Event{Kind: EvSignature, Text: rd.Signature})
			}
			if rd.Type == "reasoning.encrypted" && rd.Data != "" {
				evs = append(evs, Event{Kind: EvRedacted, Text: rd.Data})
			}
		}
		if len(dl.ReasoningDetails) == 0 {
			if dl.ReasoningContent != nil && *dl.ReasoningContent != "" {
				evs = append(evs, Event{Kind: EvThinking, Text: *dl.ReasoningContent})
			} else if dl.Reasoning != nil && *dl.Reasoning != "" {
				evs = append(evs, Event{Kind: EvThinking, Text: *dl.Reasoning})
			}
		}
		if dl.Content != nil && *dl.Content != "" {
			evs = append(evs, Event{Kind: EvText, Text: *dl.Content})
		}
		if dl.Refusal != nil && *dl.Refusal != "" {
			evs = append(evs, Event{Kind: EvText, Text: *dl.Refusal})
		}
		for i, tc := range dl.ToolCalls {
			up := i
			if tc.Index != nil {
				up = *tc.Index
			}
			if d.tools == nil {
				d.tools = map[int]int{}
			}
			idx, seen := d.tools[up]
			if !seen {
				idx = d.nextTool
				d.nextTool++
				d.tools[up] = idx
				d.sawTools = true
				evs = append(evs, Event{Kind: EvToolStart, Index: idx, ID: firstNonEmpty(tc.ID, genID("call_", idx)), Name: tc.Function.Name})
			}
			if a := argDelta(tc.Function.Arguments); a != "" {
				evs = append(evs, Event{Kind: EvToolArgs, Index: idx, Text: a})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			d.finish = oaFinish(*ch.FinishReason)
			if d.sawTools && d.finish == FinishStop {
				d.finish = FinishToolCalls
			}
		}
	}
	if c.Usage != nil {
		evs = append(evs, Event{Kind: EvUsage, Usage: c.Usage.toUsage()})
	}
	// Emit finish once usage is known or on [DONE]/end (see Finish()).
	if d.finish != FinishNone && !d.finished && c.Usage != nil {
		d.finished = true
		evs = append(evs, Event{Kind: EvFinish, Finish: d.finish})
	}
	return evs, nil
}

// End flushes a finish that arrived without a usage chunk.
func (d *openaiDecoder) End() []Event {
	if d.finish != FinishNone && !d.finished {
		d.finished = true
		return []Event{{Kind: EvFinish, Finish: d.finish}}
	}
	return nil
}

func argDelta(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		return rawString(raw)
	}
	return string(raw) // an object sent whole
}

type openaiEncoder struct {
	out      sseBuf
	id       string
	model    string
	created  int64
	started  bool
	usage    Usage
	hasUsage bool
	finished bool
}

type oaOutChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []oaOutChoice `json:"choices"`
	Usage   *oaUsage      `json:"usage,omitempty"`
}

type oaOutChoice struct {
	Index        int     `json:"index"`
	Delta        any     `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type oaDelta struct {
	Role             string              `json:"role,omitempty"`
	Content          *string             `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ReasoningDetails []oaReasoningDetail `json:"reasoning_details,omitempty"`
	ToolCalls        []oaToolCall        `json:"tool_calls,omitempty"`
}

func (e *openaiEncoder) chunk(d oaDelta, finish *string, u *oaUsage) {
	e.out.data(oaOutChunk{ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []oaOutChoice{{Index: 0, Delta: d, FinishReason: finish}}, Usage: u})
}

// textChunk writes a one-field delta chunk by hand (the hot path); the output
// matches what chunk() would produce.
func (e *openaiEncoder) textChunk(field, text string) {
	b := append(e.out.b, "data: {\"id\":"...)
	b = appendJSONString(b, e.id)
	b = append(b, ",\"object\":\"chat.completion.chunk\",\"created\":"...)
	b = strconv.AppendInt(b, e.created, 10)
	b = append(b, ",\"model\":"...)
	b = appendJSONString(b, e.model)
	b = append(b, ",\"choices\":[{\"index\":0,\"delta\":{\""...)
	b = append(b, field...)
	b = append(b, "\":"...)
	b = appendJSONString(b, text)
	e.out.b = append(b, "},\"finish_reason\":null}]}\n\n"...)
}

func (e *openaiEncoder) start(id, model string) {
	if e.started {
		return
	}
	e.started = true
	e.id = oaID(id)
	if model != "" {
		e.model = model
	}
	e.created = nowUnix()
	empty := ""
	e.chunk(oaDelta{Role: "assistant", Content: &empty}, nil, nil)
}

func (e *openaiEncoder) Encode(evs []Event) []byte {
	for _, ev := range evs {
		if ev.Kind == EvStart {
			e.start(ev.ID, ev.Name)
			continue
		}
		e.start("", "")
		switch ev.Kind {
		case EvText:
			e.textChunk("content", ev.Text)
		case EvThinking:
			e.textChunk("reasoning_content", ev.Text)
		case EvSignature:
			if ev.Name == "tool" {
				continue
			}
			e.chunk(oaDelta{ReasoningDetails: []oaReasoningDetail{{Type: "reasoning.text", Signature: ev.Text}}}, nil, nil)
		case EvRedacted:
			e.chunk(oaDelta{ReasoningDetails: []oaReasoningDetail{{Type: "reasoning.encrypted", Data: ev.Text}}}, nil, nil)
		case EvToolStart:
			idx := ev.Index
			tc := oaToolCall{Index: &idx, ID: ev.ID, Type: "function"}
			tc.Function.Name = ev.Name
			tc.Function.Arguments = json.RawMessage(`""`)
			e.chunk(oaDelta{ToolCalls: []oaToolCall{tc}}, nil, nil)
		case EvToolArgs:
			idx := ev.Index
			tc := oaToolCall{Index: &idx}
			tc.Function.Arguments, _ = json.Marshal(ev.Text)
			e.chunk(oaDelta{ToolCalls: []oaToolCall{tc}}, nil, nil)
		case EvUsage:
			e.usage, e.hasUsage = ev.Usage, true
		case EvFinish:
			if e.finished {
				continue
			}
			e.finished = true
			fr := toOAFinish(ev.Finish)
			var u *oaUsage
			if e.hasUsage {
				u = fromUsageOA(e.usage)
			}
			e.chunk(oaDelta{}, &fr, u)
		}
	}
	return e.out.take()
}

func (e *openaiEncoder) Close() []byte {
	if !e.finished && e.started {
		e.Encode([]Event{{Kind: EvFinish, Finish: FinishStop}})
	}
	e.out.raw("data: [DONE]\n\n")
	return e.out.take()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
