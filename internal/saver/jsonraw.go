package saver

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// A tiny, allocation-free JSON navigator: it finds values by byte offsets so
// savers can edit a request without decoding and re-encoding all of it.

func ws(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	return i
}

// skipValue returns the index just past the JSON value starting at b[i], or -1.
func skipValue(b []byte, i int) int {
	i = ws(b, i)
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		for j := i + 1; j < len(b); {
			k := bytes.IndexAny(b[j:], `"\`)
			if k < 0 {
				return -1
			}
			j += k
			if b[j] == '"' {
				return j + 1
			}
			j += 2 // skip the escaped byte
		}
		return -1
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				k := skipValue(b, j)
				if k < 0 {
					return -1
				}
				j = k - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	default:
		j := i
		for j < len(b) && b[j] != ',' && b[j] != '}' && b[j] != ']' && b[j] != ' ' && b[j] != '\n' && b[j] != '\r' && b[j] != '\t' {
			j++
		}
		return j
	}
}

// span is a value's [start, end) offsets in the body.
type span struct{ s, e int }

func (v span) ok() bool { return v.e > v.s }

func (v span) kind(b []byte) byte {
	if !v.ok() {
		return 0
	}
	return b[v.s]
}

// fields calls fn for each member of the object at v, stopping if fn returns false.
func fields(b []byte, v span, fn func(key []byte, val span) bool) {
	if v.kind(b) != '{' {
		return
	}
	i := ws(b, v.s+1)
	for i < v.e && b[i] == '"' {
		ke := skipValue(b, i)
		if ke < 0 {
			return
		}
		key := b[i+1 : ke-1]
		c := ws(b, ke)
		if c >= len(b) || b[c] != ':' {
			return
		}
		vs := ws(b, c+1)
		ve := skipValue(b, vs)
		if ve < 0 {
			return
		}
		if !fn(key, span{vs, ve}) {
			return
		}
		i = ws(b, ve)
		if i < len(b) && b[i] == ',' {
			i = ws(b, i+1)
		}
	}
}

// field finds one member of the object at v.
func field(b []byte, v span, key string) span {
	var out span
	fields(b, v, func(k []byte, val span) bool {
		if string(k) == key {
			out = val
			return false
		}
		return true
	})
	return out
}

// elems calls fn for each element of the array at v.
func elems(b []byte, v span, fn func(i int, val span) bool) {
	if v.kind(b) != '[' {
		return
	}
	i := ws(b, v.s+1)
	for n := 0; i < v.e && b[i] != ']'; n++ {
		ve := skipValue(b, i)
		if ve < 0 {
			return
		}
		if !fn(n, span{i, ve}) {
			return
		}
		i = ws(b, ve)
		if i < len(b) && b[i] == ',' {
			i = ws(b, i+1)
		}
	}
}

// str decodes a JSON string value ("" and false if v is not a string).
func str(b []byte, v span) (string, bool) {
	if v.kind(b) != '"' {
		return "", false
	}
	raw := b[v.s+1 : v.e-1]
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw), true
	}
	var s string
	if json.Unmarshal(b[v.s:v.e], &s) != nil {
		return "", false
	}
	return s, true
}

// strEq reports whether v is the JSON string want (no allocation for plain strings).
func strEq(b []byte, v span, want string) bool {
	if v.kind(b) != '"' {
		return false
	}
	raw := b[v.s+1 : v.e-1]
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw) == want
	}
	s, _ := str(b, v)
	return s == want
}

func root(b []byte) span {
	i := ws(b, 0)
	e := skipValue(b, i)
	if e < 0 {
		return span{}
	}
	return span{i, e}
}

// encodeString returns s as a JSON string without HTML escaping.
func encodeString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// edit replaces body[s:e] with repl (s == e inserts).
type edit struct {
	s, e int
	repl []byte
}

// apply performs non-overlapping edits in one copy.
func apply(b []byte, eds []edit) []byte {
	if len(eds) == 0 {
		return b
	}
	sort.SliceStable(eds, func(i, j int) bool { return eds[i].s < eds[j].s })
	n := len(b)
	for _, e := range eds {
		n += len(e.repl) - (e.e - e.s)
	}
	out := make([]byte, 0, n)
	last := 0
	for _, e := range eds {
		if e.s < last {
			continue // overlapping edit: skip, never corrupt the body
		}
		out = append(out, b[last:e.s]...)
		out = append(out, e.repl...)
		last = e.e
	}
	return append(out, b[last:]...)
}

// insertMember builds an edit that adds "key":raw as the first member of the object at v.
func insertMember(b []byte, v span, key, raw string) edit {
	sep := ","
	if j := ws(b, v.s+1); j < len(b) && b[j] == '}' {
		sep = ""
	}
	return edit{v.s + 1, v.s + 1, []byte(string(encodeString(key)) + ":" + raw + sep)}
}

// appendElem builds an edit that appends raw as the last element of the array at v.
func appendElem(b []byte, v span, raw string) edit {
	end := v.e - 1 // the ']'
	empty := ws(b, v.s+1) == end
	if empty {
		return edit{end, end, []byte(raw)}
	}
	return edit{end, end, []byte("," + raw)}
}

// insertElemAt builds an edit that inserts raw before the element at el.
func insertElemAt(el span, raw string) edit {
	return edit{el.s, el.s, []byte(raw + ",")}
}

func hasPromptSegment(haystack, prompt string) bool {
	if haystack == prompt {
		return true
	}
	for _, seg := range strings.Split(haystack, sep) {
		if seg == prompt {
			return true
		}
	}
	return false
}
