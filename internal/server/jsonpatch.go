package server

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// replaceTopLevelString returns body with the top-level string field `key`
// set to val, scanning the bytes once instead of decoding the whole request.
// ok is false if the field is missing or not a string.
func replaceTopLevelString(body []byte, key, val string) (out []byte, ok bool) {
	span, ok := replaceTopLevelStringSpan(body, key)
	if !ok {
		return body, false
	}
	q := strconv.Quote(val)
	out = make([]byte, 0, len(body)-(span[1]-span[0])+len(q))
	out = append(out, body[:span[0]]...)
	out = append(out, q...)
	return append(out, body[span[1]:]...), true
}

// replaceTopLevelStringSpan finds the [start,end) span of the top-level
// string value of key.
func replaceTopLevelStringSpan(body []byte, key string) ([2]int, bool) {
	depth := 0
	want := []byte(strconv.Quote(key))
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			end := skipString(body, i)
			if end < 0 {
				return [2]int{}, false
			}
			if depth == 1 && bytes.Equal(body[i:end], want) {
				j := skipSpace(body, end)
				if j < len(body) && body[j] == ':' {
					v := skipSpace(body, j+1)
					if v < len(body) && body[v] == '"' {
						vend := skipString(body, v)
						if vend < 0 {
							return [2]int{}, false
						}
						return [2]int{v, vend}, true
					}
					return [2]int{}, false
				}
			}
			i = end - 1
		}
	}
	return [2]int{}, false
}

// skipString returns the index just past the JSON string starting at body[i].
func skipString(body []byte, i int) int {
	for j := i + 1; j < len(body); j++ {
		switch body[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

func skipSpace(body []byte, i int) int {
	for i < len(body) && (body[i] == ' ' || body[i] == '\n' || body[i] == '\r' || body[i] == '\t') {
		i++
	}
	return i
}

// renameTopLevelKey renames the top-level field oldKey to newKey (if present).
func renameTopLevelKey(body []byte, oldKey, newKey string) []byte {
	depth := 0
	want := []byte(strconv.Quote(oldKey))
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			end := skipString(body, i)
			if end < 0 {
				return body
			}
			if depth == 1 && bytes.Equal(body[i:end], want) {
				if j := skipSpace(body, end); j < len(body) && body[j] == ':' {
					q := strconv.Quote(newKey)
					out := make([]byte, 0, len(body)+len(q)-len(want))
					out = append(append(append(out, body[:i]...), q...), body[end:]...)
					return out
				}
			}
			i = end - 1
		}
	}
	return body
}

// openAIReasoningModel reports OpenAI reasoning models (gpt-5 family, o1/o3/o4),
// which reject any temperature or top_p other than the default 1. Clients
// such as Cline, Roo and Gemini CLI send temperature 0.
func openAIReasoningModel(model string) bool {
	m := strings.ToLower(model)
	if strings.HasPrefix(m, "gpt-5") {
		return !strings.Contains(m, "-chat")
	}
	return len(m) > 1 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9'
}

// dropSampling removes top-level temperature and top_p unless they are 1.
func dropSampling(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"temperature"`)) && !bytes.Contains(body, []byte(`"top_p"`)) {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	changed := false
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := m[k]; ok && string(v) != "1" {
			delete(m, k)
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func keepDefault(v *float64) *float64 {
	if v != nil && *v == 1 {
		return v
	}
	return nil
}
