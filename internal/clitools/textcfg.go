package clitools

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Minimal, line-based TOML editing: FastRouter only touches its own keys and
// tables and leaves everything else in the file byte for byte.

func tomlStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func isHeader(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "[")
}

func headerName(line string) string {
	t := strings.TrimSpace(line)
	if i := strings.Index(t, "#"); i > 0 {
		t = strings.TrimSpace(t[:i])
	}
	if strings.HasPrefix(t, "[[") {
		return ""
	}
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		return strings.TrimSpace(t[1 : len(t)-1])
	}
	return ""
}

func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func join(ls []string) string {
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, "\n") + "\n"
}

// sectionRange returns [start, end) of the table's lines including its header; -1 when absent.
// name "" means the top-level region before the first table.
func sectionRange(ls []string, name string) (int, int) {
	start := -1
	if name == "" {
		start = 0
	} else {
		for i, l := range ls {
			if headerName(l) == name {
				start = i
				break
			}
		}
		if start < 0 {
			return -1, -1
		}
	}
	end := len(ls)
	for i := start; i < len(ls); i++ {
		if (name == "" || i > start) && isHeader(ls[i]) {
			end = i
			break
		}
	}
	return start, end
}

var tomlKeyRe = regexp.MustCompile(`^\s*([A-Za-z0-9_\-."]+)\s*=\s*(.*)$`)

func keyOf(line string) (string, string, bool) {
	m := tomlKeyRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return strings.Trim(m[1], `"`), strings.TrimSpace(m[2]), true
}

// tomlGet returns a key's value in a table ("" = top level), unquoting basic strings.
func tomlGet(text, section, key string) string {
	ls := lines(text)
	s, e := sectionRange(ls, section)
	if s < 0 {
		return ""
	}
	if section != "" {
		s++
	}
	for _, l := range ls[s:e] {
		if k, v, ok := keyOf(l); ok && k == key {
			if strings.HasPrefix(v, `"`) {
				var out string
				end := strings.LastIndex(v, `"`)
				if end > 0 && json.Unmarshal([]byte(v[:end+1]), &out) == nil {
					return out
				}
			}
			if i := strings.Index(v, "#"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			return strings.Trim(v, `'`)
		}
	}
	return ""
}

func tomlHasSection(text, section string) bool {
	s, _ := sectionRange(lines(text), section)
	return s >= 0
}

// tomlSet sets key = literal in a table, creating the table at the end when missing.
func tomlSet(text, section, key, literal string) string {
	ls := lines(text)
	s, e := sectionRange(ls, section)
	line := key + " = " + literal
	if s < 0 {
		out := ls
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		return join(append(out, "["+section+"]", line))
	}
	from := s
	if section != "" {
		from++
	}
	for i := from; i < e; i++ {
		if k, _, ok := keyOf(ls[i]); ok && k == key {
			ls[i] = line
			return join(ls)
		}
	}
	// insert after the last key line of the table (before trailing blanks/comments)
	at := from
	for i := from; i < e; i++ {
		if _, _, ok := keyOf(ls[i]); ok {
			at = i + 1
		}
	}
	if section == "" && at == 0 {
		at = 0
	}
	out := append([]string{}, ls[:at]...)
	out = append(out, line)
	out = append(out, ls[at:]...)
	return join(out)
}

// tomlDel removes a key from a table; an emptied table is removed too.
func tomlDel(text, section, key string) string {
	ls := lines(text)
	s, e := sectionRange(ls, section)
	if s < 0 {
		return text
	}
	from := s
	if section != "" {
		from++
	}
	for i := from; i < e; i++ {
		if k, _, ok := keyOf(ls[i]); ok && k == key {
			ls = append(ls[:i], ls[i+1:]...)
			e--
			break
		}
	}
	if section != "" {
		empty := true
		for i := from; i < e; i++ {
			if strings.TrimSpace(ls[i]) != "" {
				empty = false
			}
		}
		if empty {
			return tomlDropRange(ls, s, e)
		}
	}
	return join(ls)
}

// tomlReplaceSection writes a whole table (body = key lines) in place or at the end.
func tomlReplaceSection(text, section string, body []string) string {
	ls := lines(text)
	s, e := sectionRange(ls, section)
	block := append([]string{"[" + section + "]"}, body...)
	if s < 0 {
		out := ls
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		return join(append(out, block...))
	}
	// keep blank lines that separated it from the next table
	tail := e
	for tail > s+1 && strings.TrimSpace(ls[tail-1]) == "" {
		tail--
	}
	out := append([]string{}, ls[:s]...)
	out = append(out, block...)
	out = append(out, ls[tail:]...)
	return join(out)
}

// tomlRemoveSection drops a table and its sub-tables ([name.*]).
func tomlRemoveSection(text, section string) string {
	ls := lines(text)
	for {
		s, e := -1, -1
		for i, l := range ls {
			if n := headerName(l); n == section || strings.HasPrefix(n, section+".") {
				s, e = sectionRange(ls, n)
				_ = i
				break
			}
		}
		if s < 0 {
			return join(ls)
		}
		ls = lines(tomlDropRange(ls, s, e))
	}
}

func tomlDropRange(ls []string, s, e int) string {
	out := append([]string{}, ls[:s]...)
	out = append(out, ls[e:]...)
	// collapse runs of blank lines left behind
	var clean []string
	for i, l := range out {
		if strings.TrimSpace(l) == "" && (i == 0 || strings.TrimSpace(out[i-1]) == "") {
			continue
		}
		clean = append(clean, l)
	}
	for len(clean) > 0 && strings.TrimSpace(clean[len(clean)-1]) == "" {
		clean = clean[:len(clean)-1]
	}
	return join(clean)
}
