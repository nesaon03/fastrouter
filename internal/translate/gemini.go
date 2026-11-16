package translate

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ---------- request ----------

type gmBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

type gmPart struct {
	Text             string          `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	InlineData       *gmBlob         `json:"inlineData,omitempty"`
	FileData         *gmBlob         `json:"fileData,omitempty"`
	FunctionCall     *gmFuncCall     `json:"functionCall,omitempty"`
	FunctionResponse *gmFuncResponse `json:"functionResponse,omitempty"`
}

// gmPartIn also accepts snake_case keys sent by some clients.
type gmPartIn struct {
	gmPart
	InlineDataSnake       *gmBlob         `json:"inline_data,omitempty"`
	FileDataSnake         *gmBlob         `json:"file_data,omitempty"`
	FunctionCallSnake     *gmFuncCall     `json:"function_call,omitempty"`
	FunctionResponseSnake *gmFuncResponse `json:"function_response,omitempty"`
	ThoughtSignatureSnake string          `json:"thought_signature,omitempty"`
}

func (p *gmPartIn) norm() gmPart {
	g := p.gmPart
	if g.InlineData == nil {
		g.InlineData = p.InlineDataSnake
	}
	if g.FileData == nil {
		g.FileData = p.FileDataSnake
	}
	if g.FunctionCall == nil {
		g.FunctionCall = p.FunctionCallSnake
	}
	if g.FunctionResponse == nil {
		g.FunctionResponse = p.FunctionResponseSnake
	}
	if g.ThoughtSignature == "" {
		g.ThoughtSignature = p.ThoughtSignatureSnake
	}
	return g
}

type gmFuncCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gmFuncResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type gmContent struct {
	Role  string   `json:"role,omitempty"`
	Parts []gmPart `json:"parts"`
}

type gmContentIn struct {
	Role  string     `json:"role"`
	Parts []gmPartIn `json:"parts"`
}

type gmFuncDecl struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type gmTool struct {
	FunctionDeclarations []gmFuncDecl `json:"functionDeclarations,omitempty"`
}

type gmThinkingConfig struct {
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type gmGenConfig struct {
	MaxOutputTokens  int               `json:"maxOutputTokens,omitempty"`
	Temperature      *float64          `json:"temperature,omitempty"`
	TopP             *float64          `json:"topP,omitempty"`
	TopK             *int              `json:"topK,omitempty"`
	StopSequences    []string          `json:"stopSequences,omitempty"`
	ResponseMimeType string            `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage   `json:"responseJsonSchema,omitempty"`
	ThinkingConfig   *gmThinkingConfig `json:"thinkingConfig,omitempty"`
}

type gmToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode,omitempty"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type gmRequestIn struct {
	Contents          []gmContentIn `json:"contents"`
	SystemInstruction *gmContentIn  `json:"systemInstruction"`
	SystemSnake       *gmContentIn  `json:"system_instruction"`
	Tools             []struct {
		FunctionDeclarations []gmFuncDecl `json:"functionDeclarations"`
		FunctionSnake        []gmFuncDecl `json:"function_declarations"`
	} `json:"tools"`
	ToolConfig       *gmToolConfig `json:"toolConfig"`
	GenerationConfig *gmGenConfig  `json:"generationConfig"`
}

// callQueue pairs Gemini function responses with earlier calls by name, in
// order, since older Gemini APIs carry no call ids.
type callQueue map[string][]string

func gmToParts(ps []gmPartIn, names callQueue) []Part {
	out := make([]Part, 0, len(ps))
	for i := range ps {
		p := ps[i].norm()
		switch {
		case p.FunctionCall != nil:
			id := p.FunctionCall.ID
			if id == "" {
				id = "call_" + p.FunctionCall.Name + "_" + strconv.Itoa(len(names[p.FunctionCall.Name]))
			}
			names[p.FunctionCall.Name] = append(names[p.FunctionCall.Name], id)
			out = append(out, Part{Type: PartToolCall, ToolCallID: id, ToolName: p.FunctionCall.Name, Args: compactArgs(p.FunctionCall.Args), Signature: p.ThoughtSignature})
		case p.FunctionResponse != nil:
			id := p.FunctionResponse.ID
			if q := names[p.FunctionResponse.Name]; id == "" && len(q) > 0 {
				id, names[p.FunctionResponse.Name] = q[0], q[1:]
			} else if id == "" {
				id = "call_" + p.FunctionResponse.Name
			}
			out = append(out, Part{Type: PartToolResult, ToolCallID: id, ToolName: p.FunctionResponse.Name, Result: []Part{{Type: PartText, Text: gmResponseText(p.FunctionResponse.Response)}}})
		case p.InlineData != nil:
			out = append(out, Part{Type: PartImage, MediaType: p.InlineData.MimeType, Data: p.InlineData.Data})
		case p.FileData != nil:
			out = append(out, Part{Type: PartImage, MediaType: p.FileData.MimeType, URL: p.FileData.FileURI})
		case p.Thought:
			out = append(out, Part{Type: PartThinking, Text: p.Text, Signature: p.ThoughtSignature})
		case p.Text != "" || p.ThoughtSignature != "":
			if p.Text == "" { // bare signature part
				out = append(out, Part{Type: PartThinking, Signature: p.ThoughtSignature})
				continue
			}
			out = append(out, Part{Type: PartText, Text: p.Text, Signature: p.ThoughtSignature})
		}
	}
	return out
}

func gmResponseText(raw json.RawMessage) string {
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) == nil {
		for _, k := range []string{"output", "result", "content"} {
			if s, ok := v[k]; ok && len(v) == 1 {
				if str := rawString(s); str != "" {
					return str
				}
				return string(s)
			}
		}
	}
	return string(raw)
}

func parseGeminiRequest(body []byte, model string, stream bool) (*Request, error) {
	var in gmRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Request{Model: model, Stream: stream}
	names := callQueue{}
	sys := in.SystemInstruction
	if sys == nil {
		sys = in.SystemSnake
	}
	if sys != nil {
		for _, p := range gmToParts(sys.Parts, names) {
			if p.Type == PartText {
				r.System = append(r.System, p)
			}
		}
	}
	for _, c := range in.Contents {
		role := "user"
		if c.Role == "model" {
			role = "assistant"
		}
		parts := gmToParts(c.Parts, names)
		// a signature on a model text/function part belongs to a preceding thought
		r.Messages = append(r.Messages, Message{Role: role, Parts: parts})
	}
	for _, t := range in.Tools {
		decls := append(t.FunctionDeclarations, t.FunctionSnake...)
		for _, d := range decls {
			schema := d.ParametersJSONSchema
			if len(schema) == 0 {
				schema = d.Parameters
			}
			r.Tools = append(r.Tools, Tool{Name: d.Name, Description: d.Description, Schema: lowerSchemaTypes(schema)})
		}
	}
	if tc := in.ToolConfig; tc != nil {
		switch strings.ToUpper(tc.FunctionCallingConfig.Mode) {
		case "ANY":
			if n := tc.FunctionCallingConfig.AllowedFunctionNames; len(n) == 1 {
				r.ToolChoice = &ToolChoice{Mode: "tool", Name: n[0]}
			} else {
				r.ToolChoice = &ToolChoice{Mode: "required"}
			}
		case "NONE":
			r.ToolChoice = &ToolChoice{Mode: "none"}
		case "AUTO":
			r.ToolChoice = &ToolChoice{Mode: "auto"}
		}
	}
	if g := in.GenerationConfig; g != nil {
		r.MaxTokens, r.Temperature, r.TopP, r.TopK, r.Stop = g.MaxOutputTokens, g.Temperature, g.TopP, g.TopK, g.StopSequences
		if g.ResponseMimeType == "application/json" {
			r.JSONMode, r.JSONSchema = true, g.ResponseSchema
		}
		if tc := g.ThinkingConfig; tc != nil {
			t := &Thinking{Effort: strings.ToLower(tc.ThinkingLevel)}
			if tc.ThinkingBudget != nil {
				switch b := *tc.ThinkingBudget; {
				case b == 0:
					t.Mode = "disabled"
				case b > 0:
					t.Mode, t.Budget = "enabled", b
				default:
					t.Mode = "adaptive"
				}
			} else if tc.IncludeThoughts {
				t.Mode = "enabled"
			}
			r.Thinking = t
		}
	}
	return r, nil
}

// lowerSchemaTypes turns Gemini's upper-case OpenAPI types ("OBJECT") into JSON Schema types.
func lowerSchemaTypes(s json.RawMessage) json.RawMessage {
	if len(s) == 0 || !strings.Contains(string(s), `"type"`) {
		return s
	}
	var v any
	if json.Unmarshal(s, &v) != nil {
		return s
	}
	walkSchema(v, func(m map[string]any) {
		if t, ok := m["type"].(string); ok {
			m["type"] = strings.ToLower(t)
		}
	})
	out, _ := json.Marshal(v)
	return out
}

func walkSchema(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, c := range x {
			walkSchema(c, fn)
		}
	case []any:
		for _, c := range x {
			walkSchema(c, fn)
		}
	}
}

// geminiSchema strips JSON Schema keywords Gemini's OpenAPI subset rejects.
func geminiSchema(s json.RawMessage) json.RawMessage {
	if len(s) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(s, &v) != nil {
		return nil
	}
	walkSchema(v, func(m map[string]any) {
		for _, k := range []string{"$schema", "$id", "$comment", "additionalProperties", "examples", "patternProperties",
			"unevaluatedProperties", "propertyNames", "dependentSchemas", "if", "then", "else", "exclusiveMinimum", "exclusiveMaximum", "default", "strict"} {
			delete(m, k)
		}
		if c, ok := m["const"]; ok {
			delete(m, "const")
			m["enum"] = []any{c}
		}
		// type: ["string","null"] -> type string, nullable
		if ts, ok := m["type"].([]any); ok {
			for _, t := range ts {
				if s, _ := t.(string); s != "null" && s != "" {
					m["type"] = s
				} else if s == "null" {
					m["nullable"] = true
				}
			}
		}
		if props, ok := m["properties"].(map[string]any); ok && len(props) == 0 && m["type"] == "object" {
			delete(m, "properties")
		}
	})
	out, _ := json.Marshal(v)
	if string(out) == `{"type":"object"}` {
		return nil
	}
	return out
}

// dummySignature lets Gemini 3 accept function calls that have no recorded
// thoughtSignature (history from another provider). Documented by Google.
const dummySignature = "skip_thought_signature_validator"

func partsToGm(parts []Part, assistant, keepSig bool, names map[string]string) []gmPart {
	out := make([]gmPart, 0, len(parts))
	sawCall := false
	var pendingSig string
	for _, p := range parts {
		if !keepSig {
			p.Signature = ""
		}
		switch p.Type {
		case PartText:
			if p.Text == "" {
				continue
			}
			out = append(out, gmPart{Text: p.Text, ThoughtSignature: p.Signature})
		case PartImage:
			if p.Data != "" {
				out = append(out, gmPart{InlineData: &gmBlob{MimeType: p.MediaType, Data: p.Data}})
			} else {
				out = append(out, gmPart{FileData: &gmBlob{MimeType: p.MediaType, FileURI: p.URL}})
			}
		case PartThinking:
			if !assistant {
				continue
			}
			if p.Text == "" {
				pendingSig = p.Signature
				continue
			}
			out = append(out, gmPart{Text: p.Text, Thought: true, ThoughtSignature: p.Signature})
		case PartToolCall:
			names[p.ToolCallID] = p.ToolName
			sig := p.Signature
			if sig == "" && pendingSig != "" {
				sig, pendingSig = pendingSig, ""
			}
			if sig == "" && !sawCall {
				sig = dummySignature
			}
			sawCall = true
			out = append(out, gmPart{FunctionCall: &gmFuncCall{Name: p.ToolName, Args: argsObject(p.Args)}, ThoughtSignature: sig})
		case PartToolResult:
			name := firstNonEmpty(names[p.ToolCallID], p.ToolName)
			resp, _ := json.Marshal(map[string]any{"output": resultText(p.Result)})
			if p.IsError {
				resp, _ = json.Marshal(map[string]any{"error": resultText(p.Result)})
			}
			out = append(out, gmPart{FunctionResponse: &gmFuncResponse{Name: name, Response: resp}})
			for _, rp := range p.Result {
				if rp.Type == PartImage && rp.Data != "" {
					out = append(out, gmPart{InlineData: &gmBlob{MimeType: rp.MediaType, Data: rp.Data}})
				}
			}
		}
	}
	return out
}

type gmRequestOut struct {
	Contents          []gmContent   `json:"contents"`
	SystemInstruction *gmContent    `json:"systemInstruction,omitempty"`
	Tools             []gmTool      `json:"tools,omitempty"`
	ToolConfig        *gmToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *gmGenConfig  `json:"generationConfig,omitempty"`
}

func emitGeminiRequest(r *Request, _ EmitOptions) ([]byte, error) {
	var out gmRequestOut
	if len(r.System) > 0 {
		out.SystemInstruction = &gmContent{Parts: []gmPart{{Text: r.SystemText()}}}
	}
	names := map[string]string{}
	for _, m := range r.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		parts := partsToGm(m.Parts, m.Role == "assistant", r.keepSignatures(Gemini), names)
		if len(parts) == 0 {
			continue
		}
		if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			continue
		}
		out.Contents = append(out.Contents, gmContent{Role: role, Parts: parts})
	}
	if len(r.Tools) > 0 {
		decls := make([]gmFuncDecl, 0, len(r.Tools))
		for _, t := range r.Tools {
			decls = append(decls, gmFuncDecl{Name: t.Name, Description: t.Description, Parameters: geminiSchema(t.Schema)})
		}
		out.Tools = []gmTool{{FunctionDeclarations: decls}}
	}
	if tc := r.ToolChoice; tc != nil && len(r.Tools) > 0 {
		var c gmToolConfig
		switch tc.Mode {
		case "required":
			c.FunctionCallingConfig.Mode = "ANY"
		case "tool":
			c.FunctionCallingConfig.Mode = "ANY"
			c.FunctionCallingConfig.AllowedFunctionNames = []string{tc.Name}
		case "none":
			c.FunctionCallingConfig.Mode = "NONE"
		default:
			c.FunctionCallingConfig.Mode = "AUTO"
		}
		out.ToolConfig = &c
	}
	g := &gmGenConfig{MaxOutputTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, TopK: r.TopK, StopSequences: r.Stop}
	if r.JSONMode {
		g.ResponseMimeType = "application/json"
		g.ResponseSchema = r.JSONSchema
	}
	if t := r.Thinking; t != nil {
		b := t.BudgetFor()
		tc := &gmThinkingConfig{IncludeThoughts: b != 0}
		if b >= 0 {
			tc.ThinkingBudget = &b
		}
		g.ThinkingConfig = tc
	}
	if g.MaxOutputTokens != 0 || g.Temperature != nil || g.TopP != nil || g.TopK != nil || len(g.StopSequences) > 0 || g.ResponseMimeType != "" || g.ThinkingConfig != nil {
		out.GenerationConfig = g
	}
	return json.Marshal(out)
}

// ---------- response ----------

type gmUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
}

func (u *gmUsage) toUsage() Usage {
	return Usage{Input: u.PromptTokenCount, Output: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheRead: u.CachedContentTokenCount, Reasoning: u.ThoughtsTokenCount}
}

func fromUsageGm(u Usage) *gmUsage {
	return &gmUsage{PromptTokenCount: u.Input, CandidatesTokenCount: u.Output - u.Reasoning, TotalTokenCount: u.Input + u.Output,
		CachedContentTokenCount: u.CacheRead, ThoughtsTokenCount: u.Reasoning}
}

type gmCandidate struct {
	Content      gmContentIn `json:"content"`
	FinishReason string      `json:"finishReason"`
	Index        int         `json:"index"`
}

type gmResponse struct {
	Candidates    []gmCandidate `json:"candidates"`
	UsageMetadata *gmUsage      `json:"usageMetadata"`
	ModelVersion  string        `json:"modelVersion"`
	ResponseID    string        `json:"responseId"`
	Error         *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func gmFinish(s string) Finish {
	switch strings.ToUpper(s) {
	case "":
		return FinishNone
	case "MAX_TOKENS":
		return FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return FinishContentFilter
	}
	return FinishStop
}

func toGmFinish(f Finish) string {
	return [...]string{"STOP", "STOP", "MAX_TOKENS", "STOP", "SAFETY"}[f]
}

func parseGeminiResponse(body []byte) (*Response, error) {
	var in gmResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	r := &Response{ID: in.ResponseID, Model: in.ModelVersion}
	if in.UsageMetadata != nil {
		r.Usage = in.UsageMetadata.toUsage()
	}
	if len(in.Candidates) == 0 {
		return r, nil
	}
	c := in.Candidates[0]
	r.Parts = gmToParts(c.Content.Parts, callQueue{})
	r.Finish = gmFinish(c.FinishReason)
	for _, p := range r.Parts {
		if p.Type == PartToolCall && r.Finish == FinishStop {
			r.Finish = FinishToolCalls
		}
	}
	return r, nil
}

func emitGeminiResponse(r *Response, model string) ([]byte, error) {
	parts := partsToGmResponse(r.Parts)
	return json.Marshal(map[string]any{
		"candidates":    []any{map[string]any{"content": gmContent{Role: "model", Parts: parts}, "finishReason": toGmFinish(r.Finish), "index": 0}},
		"usageMetadata": fromUsageGm(r.Usage),
		"modelVersion":  firstNonEmpty(r.Model, model),
		"responseId":    r.ID,
	})
}

func partsToGmResponse(ps []Part) []gmPart {
	out := []gmPart{}
	for _, p := range ps {
		switch p.Type {
		case PartText:
			out = append(out, gmPart{Text: p.Text, ThoughtSignature: p.Signature})
		case PartThinking:
			out = append(out, gmPart{Text: p.Text, Thought: true, ThoughtSignature: p.Signature})
		case PartToolCall:
			out = append(out, gmPart{FunctionCall: &gmFuncCall{ID: p.ToolCallID, Name: p.ToolName, Args: argsObject(p.Args)}, ThoughtSignature: p.Signature})
		}
	}
	return out
}

// ---------- stream ----------

type geminiDecoder struct {
	started  bool
	nextTool int
	finish   Finish
	usage    *Usage
	sawTool  bool
}

func (d *geminiDecoder) Decode(f Frame) ([]Event, error) {
	data := f.Data
	if len(data) == 0 {
		return nil, nil
	}
	// tolerate the JSON-array stream form (without alt=sse)
	data = []byte(strings.TrimLeft(strings.TrimRight(string(data), ",] \n"), "[, \n"))
	var c gmResponse
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Error != nil {
		return []Event{{Kind: EvError, Text: c.Error.Message}}, nil
	}
	var evs []Event
	if !d.started {
		d.started = true
		evs = append(evs, Event{Kind: EvStart, ID: c.ResponseID, Name: c.ModelVersion})
	}
	if len(c.Candidates) > 0 {
		cand := c.Candidates[0]
		for i := range cand.Content.Parts {
			p := cand.Content.Parts[i].norm()
			switch {
			case p.FunctionCall != nil:
				idx := d.nextTool
				d.nextTool++
				d.sawTool = true
				id := firstNonEmpty(p.FunctionCall.ID, genID("call_", idx))
				evs = append(evs, Event{Kind: EvToolStart, Index: idx, ID: id, Name: p.FunctionCall.Name},
					Event{Kind: EvToolArgs, Index: idx, Text: compactArgs(p.FunctionCall.Args)})
				if p.ThoughtSignature != "" {
					evs = append(evs, Event{Kind: EvSignature, Text: p.ThoughtSignature, Index: idx, Name: "tool"})
				}
			case p.Thought:
				if p.Text != "" {
					evs = append(evs, Event{Kind: EvThinking, Text: p.Text})
				}
				if p.ThoughtSignature != "" {
					evs = append(evs, Event{Kind: EvSignature, Text: p.ThoughtSignature})
				}
			default:
				if p.ThoughtSignature != "" {
					evs = append(evs, Event{Kind: EvSignature, Text: p.ThoughtSignature})
				}
				if p.Text != "" {
					evs = append(evs, Event{Kind: EvText, Text: p.Text})
				}
			}
		}
		if cand.FinishReason != "" {
			d.finish = gmFinish(cand.FinishReason)
			if d.sawTool && d.finish == FinishStop {
				d.finish = FinishToolCalls
			}
		}
	}
	if c.UsageMetadata != nil {
		u := c.UsageMetadata.toUsage()
		d.usage = &u
		evs = append(evs, Event{Kind: EvUsage, Usage: u})
	}
	if d.finish != FinishNone {
		evs = append(evs, Event{Kind: EvFinish, Finish: d.finish})
		d.finish = FinishNone
	}
	return evs, nil
}

func (d *geminiDecoder) End() []Event { return nil }

type geminiEncoder struct {
	out     sseBuf
	model   string
	id      string
	usage   Usage
	finish  Finish
	pending map[int]*gmFuncCall // tool calls collecting args
	sigs    map[int]string
	order   []int
}

func (e *geminiEncoder) chunk(parts []gmPart, finish string, usage *gmUsage) {
	cand := map[string]any{"content": gmContent{Role: "model", Parts: parts}, "index": 0}
	if finish != "" {
		cand["finishReason"] = finish
	}
	m := map[string]any{"candidates": []any{cand}, "modelVersion": e.model}
	if e.id != "" {
		m["responseId"] = e.id
	}
	if usage != nil {
		m["usageMetadata"] = usage
	}
	e.out.data(m)
}

func (e *geminiEncoder) flushTools() {
	if len(e.order) == 0 {
		return
	}
	parts := make([]gmPart, 0, len(e.order))
	for _, i := range e.order {
		fc := e.pending[i]
		fc.Args = argsObject(string(fc.Args))
		parts = append(parts, gmPart{FunctionCall: fc, ThoughtSignature: e.sigs[i]})
	}
	e.pending, e.sigs, e.order = nil, nil, nil
	e.chunk(parts, "", nil)
}

func (e *geminiEncoder) Encode(evs []Event) []byte {
	for _, ev := range evs {
		switch ev.Kind {
		case EvStart:
			e.id = ev.ID
			if ev.Name != "" {
				e.model = ev.Name
			}
		case EvText:
			e.flushTools()
			e.chunk([]gmPart{{Text: ev.Text}}, "", nil)
		case EvThinking:
			e.flushTools()
			e.chunk([]gmPart{{Text: ev.Text, Thought: true}}, "", nil)
		case EvSignature:
			if ev.Name == "tool" {
				if e.sigs == nil {
					e.sigs = map[int]string{}
				}
				e.sigs[ev.Index] = ev.Text
				continue
			}
			e.chunk([]gmPart{{Thought: true, ThoughtSignature: ev.Text}}, "", nil)
		case EvToolStart:
			if e.pending == nil {
				e.pending = map[int]*gmFuncCall{}
			}
			e.pending[ev.Index] = &gmFuncCall{ID: ev.ID, Name: ev.Name}
			e.order = append(e.order, ev.Index)
		case EvToolArgs:
			if fc := e.pending[ev.Index]; fc != nil {
				fc.Args = append(fc.Args, ev.Text...)
			}
		case EvUsage:
			e.usage = ev.Usage
		case EvFinish:
			e.finish = ev.Finish
		}
	}
	return e.out.take()
}

func (e *geminiEncoder) Fail(msg string) []byte {
	e.flushTools()
	e.out.data(map[string]any{"error": map[string]any{"code": 502, "message": msg, "status": "UNAVAILABLE"}})
	return e.out.take()
}

func (e *geminiEncoder) Close() []byte {
	e.flushTools()
	f := e.finish
	if f == FinishNone {
		f = FinishStop
	}
	e.chunk([]gmPart{}, toGmFinish(f), fromUsageGm(e.usage))
	return e.out.take()
}
