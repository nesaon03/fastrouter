package server

import (
	"bytes"
	"strconv"
)

// replaceTopLevelString returns body with the top-level string field `key`
// set to val, scanning the bytes once instead of decoding the whole request.
// ok is false if the field is missing or not a string.
func replaceTopLevelString(body []byte, key, val string) (out []byte, ok bool) {
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
				return body, false
			}
			if depth == 1 && bytes.Equal(body[i:end], want) {
				j := skipSpace(body, end)
				if j < len(body) && body[j] == ':' {
					v := skipSpace(body, j+1)
					if v < len(body) && body[v] == '"' {
						vend := skipString(body, v)
						if vend < 0 {
							return body, false
						}
						q := strconv.Quote(val)
						out = make([]byte, 0, len(body)-(vend-v)+len(q))
						out = append(out, body[:v]...)
						out = append(out, q...)
						return append(out, body[vend:]...), true
					}
					return body, false
				}
			}
			i = end - 1
		}
	}
	return body, false
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
