package saver

import (
	"bytes"
	"sync"

	tr "github.com/nesaon03/fastrouter/internal/translate"
)

// sep joins injected prompts to existing system text (9Router's SEP).
const sep = "\n\n"

// rtkBody compresses tool-result text in messages[] or input[] (9Router's
// compressMessages shapes). It returns the new body and the filters that hit.
func rtkBody(b []byte) ([]byte, []string) {
	r := root(b)
	items := field(b, r, "messages")
	if items.kind(b) != '[' {
		items = field(b, r, "input")
	}
	if items.kind(b) != '[' {
		return b, nil
	}
	var eds []edit
	var hits []string
	compress := func(v span) {
		s, ok := str(b, v)
		if !ok {
			return
		}
		if out, name, ok := compressText(s); ok {
			eds = append(eds, edit{v.s, v.e, encodeString(out)})
			hits = append(hits, name)
		}
	}
	textParts := func(arr span, partType string) {
		elems(b, arr, func(_ int, part span) bool {
			if strEq(b, field(b, part, "type"), partType) {
				compress(field(b, part, "text"))
			}
			return true
		})
	}
	elems(b, items, func(_ int, msg span) bool {
		if msg.kind(b) != '{' {
			return true
		}
		if strEq(b, field(b, msg, "type"), "function_call_output") {
			out := field(b, msg, "output")
			if out.kind(b) == '"' {
				compress(out)
			} else {
				textParts(out, "input_text")
			}
			return true
		}
		content := field(b, msg, "content")
		isTool := strEq(b, field(b, msg, "role"), "tool")
		switch {
		case isTool && content.kind(b) == '"':
			compress(content)
		case content.kind(b) != '[':
		case isTool:
			textParts(content, "text")
		default:
			elems(b, content, func(_ int, blk span) bool {
				if !strEq(b, field(b, blk, "type"), "tool_result") || string(raw(b, field(b, blk, "is_error"))) == "true" {
					return true
				}
				c := field(b, blk, "content")
				if c.kind(b) == '"' {
					compress(c)
				} else {
					textParts(c, "text")
				}
				return true
			})
		}
		return true
	})
	return apply(b, eds), hits
}

func raw(b []byte, v span) []byte {
	if !v.ok() {
		return nil
	}
	return b[v.s:v.e]
}

// injectPrompt adds prompt to the system instructions of a body in format f,
// once (9Router's injectSystemPrompt). Bodies it cannot place it in are returned as-is.
func injectPrompt(b []byte, f tr.Format, prompt string) []byte {
	r := root(b)
	if r.kind(b) != '{' || prompt == "" {
		return b
	}
	var eds []edit
	switch f {
	case tr.Anthropic:
		eds = injectClaude(b, r, prompt)
	case tr.Gemini:
		eds = injectGemini(b, r, prompt)
	default:
		if ins := field(b, r, "instructions"); ins.kind(b) == '"' {
			eds = appendString(b, ins, prompt)
		} else if msgs := field(b, r, "messages"); msgs.kind(b) == '[' {
			eds = injectMessages(b, msgs, prompt, false)
		} else if in := field(b, r, "input"); in.kind(b) == '[' {
			eds = injectMessages(b, in, prompt, true)
		}
	}
	return apply(b, eds)
}

// appendString appends prompt to the string at v unless it is already a
// segment. The existing text is not decoded unless it may hold the prompt.
func appendString(b []byte, v span, prompt string) []edit {
	if v.e-v.s == 2 { // ""
		return []edit{{v.s, v.e, escaped(prompt)}}
	}
	if mayContain(b[v.s:v.e], prompt) {
		if cur, _ := str(b, v); hasPromptSegment(cur, prompt) {
			return nil
		}
	}
	tail := escaped(sep + prompt) // `"\n\n...“`; drop its opening quote
	return []edit{{v.e - 1, v.e, tail[1:]}}
}

// mayContain is a fast pre-check: false means raw cannot contain prompt.
func mayContain(raw []byte, prompt string) bool {
	probe := prompt
	if len(probe) > 32 {
		probe = probe[:32]
	}
	for i := 0; i < len(probe); i++ {
		if c := probe[i]; c < 0x20 || c == '"' || c == '\\' || c >= 0x80 {
			return true // probe would be escaped differently; decode to be sure
		}
	}
	return bytes.Contains(raw, []byte(probe))
}

var escCache sync.Map // string -> []byte

// escaped returns s as a JSON string, cached (the saver prompts repeat).
func escaped(s string) []byte {
	if v, ok := escCache.Load(s); ok {
		return v.([]byte)
	}
	e := encodeString(s)
	if len(s) > 200 { // only cache the long, repeated prompt texts
		escCache.Store(s, e)
	}
	return e
}

func textBlock(typ, prompt string) string {
	return `{"type":"` + typ + `","text":` + string(escaped(prompt)) + `}`
}

func hasBlockText(b []byte, arr span, prompt string) bool {
	found := false
	elems(b, arr, func(_ int, el span) bool {
		if strEq(b, field(b, el, "text"), prompt) {
			found = true
			return false
		}
		return true
	})
	return found
}

func injectClaude(b []byte, r span, prompt string) []edit {
	sys := field(b, r, "system")
	switch sys.kind(b) {
	case '"':
		return appendString(b, sys, prompt)
	case '[':
		if hasBlockText(b, sys, prompt) {
			return nil
		}
		var lastCache span
		elems(b, sys, func(_ int, el span) bool {
			if c := field(b, el, "cache_control"); c.ok() && string(raw(b, c)) != "null" {
				lastCache = el
			}
			return true
		})
		blk := textBlock("text", prompt)
		if lastCache.ok() {
			return []edit{insertElemAt(lastCache, blk)}
		}
		return []edit{appendElem(b, sys, blk)}
	case 0:
		return []edit{insertMember(b, r, "system", string(escaped(prompt)))}
	}
	return []edit{{sys.s, sys.e, encodeString(prompt)}} // null or other: replace
}

func injectGemini(b []byte, r span, prompt string) []edit {
	target := r
	if req := field(b, r, "request"); req.kind(b) == '{' {
		target = req
	}
	key := "systemInstruction"
	if field(b, target, "system_instruction").ok() {
		key = "system_instruction"
	}
	part := `{"text":` + string(escaped(prompt)) + `}`
	sys := field(b, target, key)
	if parts := field(b, sys, "parts"); parts.kind(b) == '[' {
		if hasBlockText(b, parts, prompt) {
			return nil
		}
		return []edit{appendElem(b, parts, part)}
	}
	val := `{"parts":[` + part + `]}`
	if sys.ok() {
		return []edit{{sys.s, sys.e, []byte(val)}}
	}
	return []edit{insertMember(b, target, key, val)}
}

// injectMessages handles Chat messages[] and (responses=true) Responses input[].
func injectMessages(b []byte, arr span, prompt string, responses bool) []edit {
	partType := "text"
	if responses {
		partType = "input_text"
	}
	isSys := func(m span) bool {
		if responses && !strEq(b, field(b, m, "type"), "message") {
			return false
		}
		role := field(b, m, "role")
		return strEq(b, role, "system") || strEq(b, role, "developer")
	}
	var first, firstEl span
	present := false
	elems(b, arr, func(i int, m span) bool {
		if i == 0 {
			firstEl = m
		}
		if m.kind(b) != '{' || !isSys(m) {
			return true
		}
		if !first.ok() {
			first = m
		}
		c := field(b, m, "content")
		if s, ok := str(b, c); ok && hasPromptSegment(s, prompt) {
			present = true
		}
		elems(b, c, func(_ int, p span) bool {
			if s, ok := str(b, field(b, p, "text")); ok && hasPromptSegment(s, prompt) {
				present = true
			}
			return !present
		})
		return !present
	})
	if present {
		return nil
	}
	if first.ok() {
		c := field(b, first, "content")
		switch c.kind(b) {
		case '"':
			return appendString(b, c, prompt)
		case '[':
			return []edit{appendElem(b, c, textBlock(partType, prompt))}
		case 0:
			return []edit{insertMember(b, first, "content", contentValue(responses, prompt))}
		}
		return []edit{{c.s, c.e, []byte(contentValue(responses, prompt))}}
	}
	var msg string
	if responses {
		msg = `{"type":"message","role":"system","content":[` + textBlock("input_text", prompt) + `]}`
	} else {
		msg = `{"role":"system","content":` + string(escaped(prompt)) + `}`
	}
	if firstEl.ok() {
		return []edit{insertElemAt(firstEl, msg)}
	}
	return []edit{appendElem(b, arr, msg)}
}

func contentValue(responses bool, prompt string) string {
	if responses {
		return `[` + textBlock("input_text", prompt) + `]`
	}
	return string(escaped(prompt))
}
