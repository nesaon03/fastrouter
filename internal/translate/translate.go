package translate

import (
	"fmt"
	"unicode/utf8"
)

// ParseRequest reads a client request. model and stream are used for Gemini,
// whose model and streaming mode live in the URL.
func ParseRequest(f Format, body []byte, model string, stream bool) (*Request, error) {
	var r *Request
	var err error
	switch f {
	case OpenAI:
		r, err = parseOpenAIRequest(body)
	case Anthropic:
		r, err = parseAnthropicRequest(body)
	case Gemini:
		r, err = parseGeminiRequest(body, model, stream)
	case Responses:
		r, err = parseResponsesRequest(body)
	default:
		return nil, fmt.Errorf("unknown format %d", f)
	}
	if r != nil {
		r.From = f
	}
	return r, err
}

// EmitRequest encodes r for an upstream speaking format f.
func EmitRequest(f Format, r *Request, o EmitOptions) ([]byte, error) {
	if o.Model == "" {
		o.Model = r.Model
	}
	switch f {
	case OpenAI:
		return emitOpenAIRequest(r, o)
	case Anthropic:
		return emitAnthropicRequest(r, o)
	case Gemini:
		return emitGeminiRequest(r, o)
	case Responses:
		return emitResponsesRequest(r, o)
	}
	return nil, fmt.Errorf("unknown format %d", f)
}

// ParseResponse reads a complete upstream response.
func ParseResponse(f Format, body []byte) (*Response, error) {
	switch f {
	case OpenAI:
		return parseOpenAIResponse(body)
	case Anthropic:
		return parseAnthropicResponse(body)
	case Gemini:
		return parseGeminiResponse(body)
	case Responses:
		return parseResponsesResponse(body)
	}
	return nil, fmt.Errorf("unknown format %d", f)
}

// EmitResponse encodes a complete response for a client speaking format f.
func EmitResponse(f Format, r *Response, model string) ([]byte, error) {
	switch f {
	case OpenAI:
		return emitOpenAIResponse(r, model)
	case Anthropic:
		return emitAnthropicResponse(r, model)
	case Gemini:
		return emitGeminiResponse(r, model)
	case Responses:
		return emitResponsesResponse(r, model)
	}
	return nil, fmt.Errorf("unknown format %d", f)
}

// Decoder turns upstream SSE frames into events.
type Decoder interface {
	Decode(Frame) ([]Event, error)
	End() []Event // events implied by end of stream (e.g. a finish never sent)
}

// Encoder turns events into client SSE bytes.
type Encoder interface {
	Encode([]Event) []byte
	Close() []byte // terminating frames
	// Fail ends a stream that broke (upstream error or cut-off) with the
	// format's error frame instead of a normal finish, so clients can tell a
	// truncated answer from a complete one.
	Fail(msg string) []byte
}

// NewDecoder returns a stream decoder for upstream format f.
func NewDecoder(f Format) Decoder {
	switch f {
	case Anthropic:
		return &anthropicDecoder{}
	case Gemini:
		return &geminiDecoder{}
	case Responses:
		return &responsesDecoder{}
	}
	return &openaiDecoder{}
}

// NewEncoder returns a stream encoder for client format f.
func NewEncoder(f Format, model string) Encoder {
	switch f {
	case Anthropic:
		return newAnthropicEncoder(model)
	case Gemini:
		return &geminiEncoder{model: model}
	case Responses:
		return newResponsesEncoder(model)
	}
	return &openaiEncoder{model: model}
}

// EstimateTokens roughly counts prompt tokens (about 4 characters per token,
// plus a flat cost per image), for /v1/messages/count_tokens.
func EstimateTokens(r *Request) int {
	chars := 0
	images := 0
	count := func(ps []Part) {
		for _, p := range ps {
			switch p.Type {
			case PartImage:
				images++
			case PartToolCall:
				chars += len(p.ToolName) + len(p.Args)
			default:
				chars += utf8.RuneCountInString(p.Text)
			}
			for _, rp := range p.Result {
				if rp.Type == PartImage {
					images++
				}
				chars += utf8.RuneCountInString(rp.Text)
			}
		}
	}
	count(r.System)
	for _, m := range r.Messages {
		count(m.Parts)
		chars += 4 // role overhead
	}
	for _, t := range r.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	return chars/4 + images*1600 + 1
}

// Synthetic builds a complete response with fixed text (used for bypassed requests).
func Synthetic(f Format, model, text string, stream bool) []byte {
	if !stream {
		b, _ := EmitResponse(f, &Response{Model: model, Parts: []Part{{Type: PartText, Text: text}}, Finish: FinishStop, Usage: Usage{Input: 1, Output: 1}}, model)
		return b
	}
	enc := NewEncoder(f, model)
	out := enc.Encode([]Event{{Kind: EvStart, Name: model}, {Kind: EvUsage, Usage: Usage{Input: 1}}, {Kind: EvText, Text: text},
		{Kind: EvUsage, Usage: Usage{Input: 1, Output: 1}}, {Kind: EvFinish, Finish: FinishStop}})
	return append(out, enc.Close()...)
}
