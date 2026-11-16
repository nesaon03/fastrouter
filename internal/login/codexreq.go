package login

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

//go:embed codex_instructions.txt
var codexDefaultInstructions string

// Responses-lite models (9Router: responsesLite: true); "[1m]" and "-review"
// variants map to the base model upstream.
var codexLite = map[string]bool{"gpt-6.1-sol": true, "gpt-6.1-terra": true, "gpt-6-sol": true, "gpt-6-luna": true}

var codexLiteLevels = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

var (
	serverItemID   = regexp.MustCompile(`^(rs|fc|resp|msg)_`)
	hostedTools    = map[string]bool{"image_generation": true, "web_search": true, "web_search_preview": true, "file_search": true, "computer": true, "computer_use_preview": true, "code_interpreter": true, "mcp": true, "local_shell": true, "tool_search": true}
	codexAllowlist = map[string]bool{"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true, "stream": true, "store": true,
		"reasoning": true, "service_tier": true, "include": true, "prompt_cache_key": true, "client_metadata": true, "text": true, "parallel_tool_calls": true}
	unicodeProp = regexp.MustCompile(`\\[pP]\{`)
)

// CodexUpstreamModel maps FastRouter's model id to the id Codex expects.
func CodexUpstreamModel(m string) string {
	if m == "codex-auto-review" {
		return m
	}
	m = strings.TrimSuffix(m, "[1m]")
	return strings.TrimSuffix(m, "-review")
}

// CodexRequest is a shaped Codex Responses request.
type CodexRequest struct {
	Body      []byte
	Lite      bool
	SessionID string
}

// ShapeCodex rewrites a Responses-format body the way 9Router's codex
// executor does: always streamed, store=false, default instructions, no
// server item ids, flat function tools, Codex reasoning defaults and only
// the fields Codex accepts. fallbackSession seeds the prompt-cache session.
func ShapeCodex(raw []byte, fallbackSession string) (*CodexRequest, error) {
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	model, _ := b["model"].(string)
	upstream := CodexUpstreamModel(model)

	switch in := b["input"].(type) {
	case string:
		b["input"] = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": in}}}}
	case nil:
	}
	input, _ := b["input"].([]any)
	tools, _ := b["tools"].([]any)
	hasWebSearch := false
	for _, t := range tools {
		if m, _ := t.(map[string]any); m != nil && m["type"] == "web_search" {
			hasWebSearch = true
		}
	}
	lite := codexLite[upstream] && !hasWebSearch
	if len(input) == 0 {
		input = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "..."}}}}
	}
	out := input[:0]
	for _, it := range input {
		if s, ok := it.(string); ok && serverItemID.MatchString(s) {
			continue
		}
		if m, ok := it.(map[string]any); ok {
			if m["type"] == "item_reference" {
				continue
			}
			if m["role"] == "system" && (m["type"] == nil || m["type"] == "message") {
				m["role"] = "developer" // keeps it in the cacheable prefix
			}
			if id, _ := m["id"].(string); serverItemID.MatchString(id) && !(lite && m["role"] == "developer" && strings.HasPrefix(id, "msg_")) {
				delete(m, "id")
			}
		}
		out = append(out, it)
	}
	input = out
	b["tools"] = normalizeCodexTools(b, tools)

	session, _ := b["prompt_cache_key"].(string)
	if session == "" {
		session = sessionFrom(input, fallbackSession)
		b["prompt_cache_key"] = session
	}
	b["stream"] = true
	b["store"] = false
	instr, _ := b["instructions"].(string)
	if !lite && strings.TrimSpace(instr) == "" {
		b["instructions"] = codexDefaultInstructions
	}
	b["model"] = upstream

	if lite {
		hasPrefix := false
		for _, it := range input {
			if m, _ := it.(map[string]any); m != nil && m["type"] == "additional_tools" {
				hasPrefix = true
			}
		}
		if !hasPrefix {
			if strings.TrimSpace(instr) == "" {
				instr = codexDefaultInstructions
			}
			t := b["tools"]
			if t == nil {
				t = []any{}
			}
			prefix := []any{map[string]any{"type": "additional_tools", "role": "developer", "tools": t},
				map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": instr}}}}
			input = append(prefix, input...)
		}
		b["instructions"] = ""
		b["tools"] = nil
		if b["tool_choice"] == nil {
			b["tool_choice"] = "auto"
		}
		b["parallel_tool_calls"] = false
	} else {
		delete(b, "parallel_tool_calls")
	}
	b["input"] = input

	// Reasoning: explicit reasoning.effort > reasoning_effort > model suffix > default.
	m, _ := b["model"].(string)
	suffixEffort := ""
	for _, lvl := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if strings.HasSuffix(m, "-"+lvl) {
			suffixEffort = lvl
			b["model"] = strings.TrimSuffix(m, "-"+lvl)
			break
		}
	}
	reasoning, _ := b["reasoning"].(map[string]any)
	if reasoning == nil {
		effort, _ := b["reasoning_effort"].(string)
		if effort == "" {
			effort = suffixEffort
		}
		if effort == "" {
			effort = "low"
			if lite {
				effort = "medium"
			}
		}
		reasoning = map[string]any{"effort": effort}
	}
	effort, _ := reasoning["effort"].(string)
	reasoning["effort"] = normalizeEffort(effort, lite)
	if lite {
		reasoning["context"] = "all_turns"
		delete(reasoning, "summary")
	} else if reasoning["summary"] == nil {
		reasoning["summary"] = "auto"
	}
	b["reasoning"] = reasoning
	if e, _ := reasoning["effort"].(string); e != "" && e != "none" {
		b["include"] = []any{"reasoning.encrypted_content"}
	}
	if st, _ := b["service_tier"].(string); st == "fast" {
		b["service_tier"] = "priority"
	} else if st != "priority" {
		delete(b, "service_tier")
	}
	for k := range b {
		if !codexAllowlist[k] {
			delete(b, k)
		}
	}
	body, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	return &CodexRequest{Body: body, Lite: lite, SessionID: session}, nil
}

func normalizeEffort(v string, lite bool) string {
	if lite {
		switch {
		case v == "none" || v == "minimal":
			return "low"
		case v == "ultra":
			return "max"
		case codexLiteLevels[v]:
			return v
		}
		return "medium"
	}
	if v == "max" || v == "ultra" {
		return "xhigh"
	}
	return v
}

// normalizeCodexTools flattens Chat-style function tools, keeps hosted and
// custom tools, strips \p{..} patterns Codex rejects, and drops a tool_choice
// that names an unknown function.
func normalizeCodexTools(b map[string]any, tools []any) any {
	if tools == nil {
		return b["tools"]
	}
	valid := map[string]bool{}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m, _ := t.(map[string]any)
		if m == nil {
			continue
		}
		typ, _ := m["type"].(string)
		switch {
		case typ == "namespace":
			if sub, _ := m["tools"].([]any); sub != nil {
				for _, st := range sub {
					if sm, _ := st.(map[string]any); sm != nil {
						if n, _ := sm["name"].(string); n != "" {
							valid[n] = true
						}
						if p, ok := sm["parameters"].(map[string]any); ok {
							stripPatterns(p)
						}
					}
				}
			}
			out = append(out, m)
			continue
		case typ == "custom":
			out = append(out, m)
			continue
		case typ != "function":
			if typ != "" && m["function"] == nil && m["name"] == nil && hostedTools[typ] {
				out = append(out, m)
			}
			continue
		}
		fn, _ := m["function"].(map[string]any)
		name, _ := m["name"].(string)
		if name == "" && fn != nil {
			name, _ = fn["name"].(string)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(name) > 128 {
			name = name[:128]
		}
		desc, _ := m["description"].(string)
		if desc == "" && fn != nil {
			desc, _ = fn["description"].(string)
		}
		params, _ := m["parameters"].(map[string]any)
		if params == nil && fn != nil {
			params, _ = fn["parameters"].(map[string]any)
		}
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		stripPatterns(params)
		nt := map[string]any{"type": "function", "name": name, "parameters": params}
		if desc != "" {
			nt["description"] = desc
		}
		if s, ok := m["strict"].(bool); ok {
			nt["strict"] = s
		} else if fn != nil {
			if s, ok := fn["strict"].(bool); ok {
				nt["strict"] = s
			}
		}
		valid[name] = true
		out = append(out, nt)
	}
	if tc, _ := b["tool_choice"].(map[string]any); tc != nil && tc["type"] == "function" {
		if n, _ := tc["name"].(string); !valid[strings.TrimSpace(n)] {
			delete(b, "tool_choice")
		}
	}
	return out
}

func stripPatterns(v any) {
	switch x := v.(type) {
	case map[string]any:
		if p, ok := x["pattern"].(string); ok && unicodeProp.MatchString(p) {
			delete(x, "pattern")
		}
		for _, c := range x {
			stripPatterns(c)
		}
	case []any:
		for _, c := range x {
			stripPatterns(c)
		}
	}
}

// sessionFrom derives a stable prompt-cache session id from the first user
// message, so one conversation keeps hitting the same cache.
func sessionFrom(input []any, fallback string) string {
	for _, it := range input {
		m, _ := it.(map[string]any)
		if m == nil || m["role"] != "user" {
			continue
		}
		raw, _ := json.Marshal(m["content"])
		sum := sha256.Sum256(append([]byte(fallback+"\x00"), raw...))
		return hex.EncodeToString(sum[:16])
	}
	return fallback
}

// CodexHeaders sets the headers 9Router's codex executor sends.
func CodexHeaders(h http.Header, accessToken, accountID string, r *CodexRequest) {
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Accept", "text/event-stream")
	h.Set("originator", "codex_cli_rs")
	h.Set("User-Agent", "codex_cli_rs/"+CodexCLIVersion)
	h.Set("version", CodexCLIVersion)
	h.Set("session_id", r.SessionID)
	if accountID != "" {
		h.Set("ChatGPT-Account-ID", accountID)
	}
	if r.Lite {
		h.Set("x-openai-internal-codex-responses-lite", "true")
	}
}
