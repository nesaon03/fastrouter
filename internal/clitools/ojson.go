package clitools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

// Obj is a JSON object that keeps its key order, so rewriting a tool's
// settings file only changes the keys FastRouter manages.
type Obj struct {
	keys []string
	vals map[string]any
}

func newObj() *Obj { return &Obj{vals: map[string]any{}} }

func (o *Obj) Get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}
func (o *Obj) Has(k string) bool {
	if o == nil {
		return false
	}
	_, ok := o.vals[k]
	return ok
}
func (o *Obj) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}
func (o *Obj) Keys() []string { return append([]string(nil), o.keys...) }

func (o *Obj) Set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *Obj) Del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Child returns the object at k, creating (or replacing a non-object) when create is set.
func (o *Obj) Child(k string, create bool) *Obj {
	if o == nil {
		return nil
	}
	if c, ok := o.vals[k].(*Obj); ok {
		return c
	}
	if !create {
		return nil
	}
	c := newObj()
	o.Set(k, c)
	return c
}

// Str returns the string at k ("" when missing or not a string).
func (o *Obj) Str(k string) string {
	if o == nil {
		return ""
	}
	s, _ := o.vals[k].(string)
	return s
}

// path walks dotted keys without creating anything.
func (o *Obj) path(keys ...string) *Obj {
	cur := o
	for _, k := range keys {
		if cur == nil {
			return nil
		}
		cur = cur.Child(k, false)
	}
	return cur
}

var trailingComma = regexp.MustCompile(`,(\s*[}\]])`)

// stripJSONC removes // and /* */ comments outside strings (VS Code files allow them).
func stripJSONC(b []byte) []byte {
	var out bytes.Buffer
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(b) && b[i+1] == '/' {
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if c == '/' && i+1 < len(b) && b[i+1] == '*' {
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(c)
	}
	return trailingComma.ReplaceAll(out.Bytes(), []byte("$1"))
}

// parseJSON reads a (possibly commented) JSON document into Obj/[]any values.
func parseJSON(b []byte) (any, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(b)) == 0 {
		return newObj(), nil
	}
	d := json.NewDecoder(bytes.NewReader(stripJSONC(b)))
	d.UseNumber()
	v, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON document")
	}
	return v, nil
}

func decodeValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObj()
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(d)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			_, err := d.Token()
			return o, err
		case '[':
			arr := []any{}
			for d.More() {
				v, err := decodeValue(d)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := d.Token()
			return arr, err
		}
		return nil, errors.New("unexpected delimiter")
	default:
		return t, nil
	}
}

// marshalJSON writes v with two-space indentation (what the tools use).
func marshalJSON(v any) []byte {
	var b strings.Builder
	writeValue(&b, v, "")
	b.WriteByte('\n')
	return []byte(b.String())
}

func writeValue(b *strings.Builder, v any, ind string) {
	switch v := v.(type) {
	case *Obj:
		if v.Len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range v.keys {
			b.WriteString(ind + "  ")
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteString(": ")
			writeValue(b, v.vals[k], ind+"  ")
			if i < len(v.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "}")
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, x := range v {
			b.WriteString(ind + "  ")
			writeValue(b, x, ind+"  ")
			if i < len(v)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "]")
	case []string:
		arr := make([]any, len(v))
		for i, s := range v {
			arr[i] = s
		}
		writeValue(b, arr, ind)
	default:
		enc, _ := json.Marshal(v)
		b.Write(enc)
	}
}

// obj builds an ordered object from alternating key, value pairs.
func obj(kv ...any) *Obj {
	o := newObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}
