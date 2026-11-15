// Package translate converts requests, responses and streams between the
// OpenAI Chat, Anthropic Messages, Gemini generateContent and OpenAI Responses
// wire formats.
//
// Each format has one parser and one emitter that work on a shared in-memory
// form (Go structs, never re-serialized to an intermediate JSON format), so a
// translation is a single decode plus a single encode for any pair. Streams are
// translated event by event: a Decoder turns one upstream SSE frame into
// Events, and an Encoder turns Events into client SSE frames.
//
// Behaviour follows 9Router's translators (MIT, decolua/9router) where the
// formats leave room for choice; the code here is written fresh.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Format is an API wire format.
type Format int

const (
	OpenAI    Format = iota // OpenAI Chat Completions
	Anthropic               // Anthropic Messages
	Gemini                  // Google generateContent
	Responses               // OpenAI Responses
)

func (f Format) String() string {
	return [...]string{"openai", "anthropic", "gemini", "responses"}[f]
}

// PartType is the kind of a content part.
type PartType uint8

const (
	PartText PartType = iota
	PartImage
	PartThinking
	PartRedactedThinking
	PartToolCall
	PartToolResult
)

// Part is one piece of message content.
type Part struct {
	Type         PartType
	Text         string          // text, thinking text
	CacheControl json.RawMessage // Anthropic-style cache marker, kept where the target supports it

	MediaType string // image: mime type
	Data      string // image: base64 data; redacted thinking: opaque data
	URL       string // image: remote URL
	Detail    string // image: OpenAI detail hint

	Signature string // thinking signature / Gemini thoughtSignature

	ToolCallID string // tool call and tool result
	ToolName   string
	Args       string // tool call: JSON-encoded arguments

	Result  []Part // tool result content (text and images)
	IsError bool
}

// Message is one conversation turn. Role is "user" or "assistant"; tool
// results travel as PartToolResult parts in user messages.
type Message struct {
	Role  string
	Parts []Part
}

// Tool is a function the model may call.
type Tool struct {
	Name, Description string
	Schema            json.RawMessage
	CacheControl      json.RawMessage
}

// ToolChoice says whether/which tool must be called. Mode: auto, none, required, tool.
type ToolChoice struct {
	Mode string
	Name string
}

// Thinking is the requested reasoning configuration.
// Mode: "" (unset), enabled, disabled, adaptive. Effort: none|minimal|low|medium|high|xhigh|max.
type Thinking struct {
	Mode   string
	Budget int
	Effort string
}

// Request is a chat request in format-neutral form.
type Request struct {
	From              Format // client format the request was parsed from
	Model             string
	System            []Part
	Messages          []Message
	Tools             []Tool
	ToolChoice        *ToolChoice
	ParallelToolCalls *bool
	MaxTokens         int
	Temperature       *float64
	TopP              *float64
	TopK              *int
	Stop              []string
	Stream            bool
	Thinking          *Thinking
	User              string
	JSONMode          bool            // response must be a JSON object
	JSONSchema        json.RawMessage // optional schema for JSON mode
	SchemaName        string
}

// Finish is a normalized stop reason.
type Finish uint8

const (
	FinishNone Finish = iota
	FinishStop
	FinishLength
	FinishToolCalls
	FinishContentFilter
)

// Usage counts tokens. Input includes cached tokens (CacheRead and CacheWrite
// are subsets of it); Output includes Reasoning.
type Usage struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int
}

// IsZero reports whether no counts are set.
func (u Usage) IsZero() bool { return u == Usage{} }

// Response is a complete (non-streaming) model answer.
type Response struct {
	ID, Model string
	Parts     []Part // text, thinking, redacted thinking, tool calls
	Finish    Finish
	Usage     Usage
}

// HasContent reports whether the response carries anything a client can use.
func (r *Response) HasContent() bool {
	for _, p := range r.Parts {
		if (p.Type == PartText && p.Text != "") || p.Type == PartToolCall || (p.Type == PartThinking && p.Text != "") {
			return true
		}
	}
	return false
}

// EventKind is the type of a stream event.
type EventKind uint8

const (
	EvStart EventKind = iota
	EvText
	EvThinking
	EvSignature // thinking signature for the current thinking block
	EvRedacted  // redacted thinking block (Data)
	EvToolStart // a new tool call (Index, ID, Name)
	EvToolArgs  // more JSON for tool call Index
	EvUsage     // updated usage (cumulative)
	EvFinish
	EvError // upstream reported an error mid-stream
)

// Event is one normalized stream event.
type Event struct {
	Kind   EventKind
	Text   string // text, thinking, args, signature, redacted data, error message
	Index  int    // tool call index within the response
	ID     string // message id (Start) or tool call id
	Name   string // tool name or model (Start)
	Finish Finish
	Usage  Usage
}

// IsContent reports whether the event carries user-visible output.
func (e *Event) IsContent() bool {
	switch e.Kind {
	case EvText, EvThinking:
		return e.Text != ""
	case EvToolStart, EvRedacted:
		return true
	}
	return false
}

// thinking budgets used when mapping effort levels to token budgets.
var effortBudget = map[string]int{"minimal": 1024, "low": 4096, "medium": 10000, "high": 24576, "xhigh": 32000, "max": 32000}

// BudgetFor returns a token budget for t (0 = disabled, -1 = provider default).
func (t *Thinking) BudgetFor() int {
	switch {
	case t == nil:
		return -1
	case t.Mode == "disabled" || t.Effort == "none":
		return 0
	case t.Budget > 0:
		return t.Budget
	case t.Effort != "":
		if b, ok := effortBudget[t.Effort]; ok {
			return b
		}
	}
	if t.Mode == "enabled" {
		return 10000
	}
	return -1
}

// EffortFor returns an effort level for t ("" = none requested).
func (t *Thinking) EffortFor() string {
	switch {
	case t == nil:
		return ""
	case t.Effort != "":
		return t.Effort
	case t.Mode == "disabled":
		return "none"
	case t.Budget > 0 && t.Budget < 2048:
		return "minimal"
	case t.Budget > 0 && t.Budget < 8192:
		return "low"
	case t.Budget > 0 && t.Budget < 20000:
		return "medium"
	case t.Budget > 0:
		return "high"
	case t.Mode == "enabled" || t.Mode == "adaptive":
		return "medium"
	}
	return ""
}

// Enabled reports whether reasoning output was asked for.
func (t *Thinking) Enabled() bool {
	return t != nil && t.BudgetFor() != 0 && (t.Mode != "" || t.Effort != "" || t.Budget > 0)
}

// keepSignatures reports whether thinking signatures in r can be sent to an
// upstream speaking `to`. Signatures are provider-specific, so they only go
// back to the format they came from; OpenAI-format clients may carry either
// kind (inside reasoning_details), so theirs are passed on.
func (r *Request) keepSignatures(to Format) bool { return r.From == to || r.From == OpenAI }

// HasImages reports whether any message carries an image.
func (r *Request) HasImages() bool {
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if p.Type == PartImage {
				return true
			}
			for _, rp := range p.Result {
				if rp.Type == PartImage {
					return true
				}
			}
		}
	}
	return false
}

// SystemText joins the system parts.
func (r *Request) SystemText() string {
	var b strings.Builder
	for i, p := range r.System {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// resultText flattens tool result content to text.
func resultText(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == PartText {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// parseDataURL splits "data:image/png;base64,AAAA".
func parseDataURL(u string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(u, "data:")
	if !found {
		return "", "", false
	}
	meta, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mediaType, _, _ = strings.Cut(meta, ";")
	return mediaType, data, true
}

func dataURL(mediaType, data string) string { return "data:" + mediaType + ";base64," + data }

// imagePart builds an image part from an OpenAI-style URL (data: or http).
func imagePart(url, detail string) Part {
	if mt, data, ok := parseDataURL(url); ok {
		return Part{Type: PartImage, MediaType: mt, Data: data, Detail: detail}
	}
	return Part{Type: PartImage, URL: url, Detail: detail, MediaType: guessMime(url)}
}

func guessMime(url string) string {
	u := strings.ToLower(url)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	switch {
	case strings.HasSuffix(u, ".png"):
		return "image/png"
	case strings.HasSuffix(u, ".gif"):
		return "image/gif"
	case strings.HasSuffix(u, ".webp"):
		return "image/webp"
	case strings.HasSuffix(u, ".pdf"):
		return "application/pdf"
	}
	return "image/jpeg"
}

// rawString returns the string value of a JSON string, or "" for anything else.
func rawString(r json.RawMessage) string {
	var s string
	if len(r) > 0 && r[0] == '"' {
		_ = json.Unmarshal(r, &s)
	}
	return s
}

// compactArgs normalizes tool arguments to a JSON object string.
func compactArgs(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	if raw[0] == '"' { // some providers double-encode
		return rawString(raw)
	}
	return string(raw)
}

// argsObject returns tool arguments as a JSON object (invalid JSON becomes {}).
func argsObject(args string) json.RawMessage {
	if args == "" || !json.Valid([]byte(args)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

// genID makes a short unique-enough id with a prefix (replaced in golden tests).
var genID = func(prefix string, n int) string { return fmt.Sprintf("%s%x%04x", prefix, nowNano(), n&0xffff) }
