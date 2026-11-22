package saver

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool-output compaction and dedupe. Both are lossy by design: the model gets a
// short marker instead of content it has already seen (or content that is too
// old to matter). Error results are never touched.

type toolText struct {
	span    span
	text    string
	isError bool
}

// walkToolTexts visits every tool-result text span in messages[] or input[].
// It mirrors rtkBody's traversal without editing anything.
func walkToolTexts(b []byte, fn func(toolText)) {
	r := root(b)
	items := field(b, r, "messages")
	if items.kind(b) != '[' {
		items = field(b, r, "input")
	}
	if items.kind(b) != '[' {
		return
	}
	visit := func(v span, isErr bool) {
		if s, ok := str(b, v); ok {
			fn(toolText{span: v, text: s, isError: isErr})
		}
	}
	textParts := func(arr span, partType string, isErr bool) {
		elems(b, arr, func(_ int, part span) bool {
			if strEq(b, field(b, part, "type"), partType) {
				visit(field(b, part, "text"), isErr)
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
				visit(out, false)
			} else {
				textParts(out, "input_text", false)
			}
			return true
		}
		content := field(b, msg, "content")
		isTool := strEq(b, field(b, msg, "role"), "tool")
		switch {
		case isTool && content.kind(b) == '"':
			visit(content, false)
		case content.kind(b) != '[':
		case isTool:
			textParts(content, "text", false)
		default:
			elems(b, content, func(_ int, blk span) bool {
				if !strEq(b, field(b, blk, "type"), "tool_result") {
					return true
				}
				isErr := string(raw(b, field(b, blk, "is_error"))) == "true"
				c := field(b, blk, "content")
				if c.kind(b) == '"' {
					visit(c, isErr)
				} else {
					textParts(c, "text", isErr)
				}
				return true
			})
		}
		return true
	})
}

func toolHash(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:])
}

// compactBody trims all but the last keep tool results and/or replaces repeated
// identical results. keep <= 0 with dedupe=false leaves the body unchanged.
func compactBody(b []byte, keep int, dedupe bool) []byte {
	if keep <= 0 && !dedupe {
		return b
	}
	var texts []toolText
	walkToolTexts(b, func(t toolText) { texts = append(texts, t) })
	if len(texts) == 0 {
		return b
	}
	cutoff := len(texts) - keep
	if cutoff < 0 {
		cutoff = 0
	}
	var eds []edit
	seen := map[string]bool{}
	for i, t := range texts {
		if t.isError {
			continue
		}
		if keep > 0 && i < cutoff {
			marker := "[older tool output trimmed: " + strconv.Itoa(utf8.RuneCountInString(t.text)) + " chars]"
			if utf8.RuneCountInString(marker) >= utf8.RuneCountInString(t.text) {
				continue // the marker would be larger than the content
			}
			eds = append(eds, edit{t.span.s, t.span.e, encodeString(marker)})
			continue
		}
		if dedupe {
			h := toolHash(t.text)
			if seen[h] {
				marker := "[duplicate of an earlier tool output: " + strconv.Itoa(utf8.RuneCountInString(t.text)) + " chars]"
				if utf8.RuneCountInString(marker) >= utf8.RuneCountInString(t.text) {
					continue
				}
				eds = append(eds, edit{t.span.s, t.span.e, encodeString(marker)})
				continue
			}
			seen[h] = true
		}
	}
	if len(eds) == 0 {
		return b
	}
	return apply(b, eds)
}

// PreviewRTK exposes the RTK detector for the dashboard's preview box.
func PreviewRTK(text string) (string, string, bool) {
	return compressText(text)
}
