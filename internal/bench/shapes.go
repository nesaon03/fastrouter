package bench

import (
	"encoding/json"
	"strings"
)

// Prompts. "tiny" asks for ~20 tokens so tokens/sec still means something;
// "medium" sends ~700 tokens of code and asks for ~150 words.
const tinyPrompt = "Count from 1 to 20 in words, separated by commas. No other text."

var mediumCode = strings.TrimSpace(`
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Host           string        ` + "`json:\"host\"`" + `
	Port           int           ` + "`json:\"port\"`" + `
	DataDir        string        ` + "`json:\"dataDir\"`" + `
	ConnectTimeout time.Duration ` + "`json:\"connectTimeout\"`" + `
	IdleTimeout    time.Duration ` + "`json:\"idleTimeout\"`" + `
	AllowedHosts   []string      ` + "`json:\"allowedHosts\"`" + `
}

func Load(path string) (Config, error) {
	c := Config{Host: "127.0.0.1", Port: 20128, ConnectTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return c, err
		}
		b, _ = json.MarshalIndent(c, "", "  ")
		return c, os.WriteFile(path, b, 0o600)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Dir(path)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return c, errors.New("port out of range")
	}
	for i, h := range c.AllowedHosts {
		c.AllowedHosts[i] = strings.ToLower(strings.TrimSpace(h))
	}
	return c, nil
}`)

func userText(o Options) string {
	if o.Prompt == "medium" {
		return "Here is a Go file:\n\n```go\n" + mediumCode + "\n```\n\nIn about 150 words, explain what Load does, including its defaults and error cases."
	}
	return tinyPrompt
}

func str(d string) map[string]any     { return map[string]any{"type": "string", "description": d} }
func num(d string) map[string]any     { return map[string]any{"type": "integer", "description": d} }
func boolean(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }
func strs(d string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": d}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// task follows the prompt for simulated clients so the model answers instead
// of reaching for a tool (the benchmark does not run tools).
const task = "\n\nAnswer directly from the context above. Do not call any tools."

// Effort the request asks for: the user's choice, else the client's own
// default; plain API requests ask for low so reasoning models still write
// visible text within the token budget.
func effortFor(o Options, p *Profile) string {
	switch o.Effort {
	case "", "auto":
		if p.Kind == "api" && p.Endpoint != Messages {
			return "low"
		}
		return p.effort
	case "none":
		return ""
	}
	return o.Effort
}

var geminiBudget = map[string]int{"minimal": 1024, "low": 4096, "medium": 10000, "high": 24576}

// Build returns the body, path and headers of one request from client.
func Build(o Options, model, client string) ([]byte, string, map[string]string) {
	p := ProfileByID(client)
	if p == nil {
		p = apiProfile(Chat)
	}
	hdr := map[string]string{"User-Agent": p.UserAgent}
	for k, v := range p.headers {
		hdr[k] = v
	}
	text := userText(o)
	sim := p.Kind != "api"
	if sim {
		text += task
	}
	maxTok := p.maxTokens
	if !sim {
		maxTok = o.MaxTokens
	}
	system := p.system
	if p.xmlTools {
		system += "\n\n" + xmlToolDocs(p.tools)
	}
	effort := effortFor(o, p)
	b := map[string]any{"model": model, "stream": o.Stream}
	for k, v := range p.extra {
		b[k] = v
	}
	var path string
	switch p.Endpoint {
	case Messages:
		path = "/v1/messages"
		b["max_tokens"] = max(maxTok, 1024)
		var content []any
		if p.preamble != "" {
			content = append(content, map[string]any{"type": "text", "text": p.preamble})
		}
		content = append(content, map[string]any{"type": "text", "text": text})
		b["messages"] = []any{map[string]any{"role": "user", "content": content}}
		if system != "" {
			b["system"] = []any{map[string]any{"type": "text", "text": system, "cache_control": map[string]any{"type": "ephemeral"}}}
			b["metadata"] = map[string]any{"user_id": "fastrouter-bench"}
		}
		if len(p.tools) > 0 && !p.xmlTools {
			var tools []any
			for _, n := range p.tools {
				d, s := toolSchema(n)
				tools = append(tools, map[string]any{"name": n, "description": d, "input_schema": s})
			}
			b["tools"] = tools
		}
		if effort != "" {
			b["output_config"] = map[string]any{"effort": effort}
		}
	case Responses:
		path = "/v1/responses"
		if maxTok > 0 {
			b["max_output_tokens"] = maxTok
		}
		if !sim {
			b["store"] = false
		}
		var input []any
		if p.preamble != "" {
			input = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": p.preamble}}})
		}
		b["input"] = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}})
		if system != "" {
			b["instructions"] = system
		}
		if len(p.tools) > 0 && !p.xmlTools {
			var tools []any
			for _, n := range p.tools {
				d, s := toolSchema(n)
				tools = append(tools, map[string]any{"type": "function", "name": n, "description": d, "parameters": s, "strict": false})
			}
			b["tools"] = tools
		}
		if effort != "" {
			r := map[string]any{"effort": effort}
			if sim {
				r["summary"] = "auto"
			}
			b["reasoning"] = r
		}
	case Gemini:
		path = "/v1beta/models/" + model + ":generateContent"
		if o.Stream {
			path = "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
		}
		delete(b, "model")
		delete(b, "stream")
		parts := []any{}
		if p.preamble != "" {
			parts = append(parts, map[string]any{"text": p.preamble})
		}
		b["contents"] = []any{map[string]any{"role": "user", "parts": append(parts, map[string]any{"text": text})}}
		if system != "" {
			b["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": system}}}
		}
		if len(p.tools) > 0 {
			var fns []any
			for _, n := range p.tools {
				d, s := toolSchema(n)
				fns = append(fns, map[string]any{"name": n, "description": d, "parameters": s})
			}
			b["tools"] = []any{map[string]any{"functionDeclarations": fns}}
		}
		gc := map[string]any{}
		if g, ok := b["generationConfig"].(map[string]any); ok {
			for k, v := range g {
				gc[k] = v
			}
		}
		if maxTok > 0 {
			gc["maxOutputTokens"] = maxTok
		}
		if bud, ok := geminiBudget[effort]; ok {
			gc["thinkingConfig"] = map[string]any{"includeThoughts": sim, "thinkingBudget": bud}
		}
		if len(gc) > 0 {
			b["generationConfig"] = gc
		}
	default:
		path = "/v1/chat/completions"
		if maxTok > 0 {
			b["max_tokens"] = maxTok
		}
		if o.Stream {
			b["stream_options"] = map[string]any{"include_usage": true}
		}
		var msgs []any
		if system != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": system})
		}
		user := text
		if p.preamble != "" {
			user = p.preamble + "\n\n" + text
		}
		b["messages"] = append(msgs, map[string]any{"role": "user", "content": user})
		if len(p.tools) > 0 && !p.xmlTools {
			var tools []any
			for _, n := range p.tools {
				d, s := toolSchema(n)
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": n, "description": d, "parameters": s}})
			}
			b["tools"] = tools
		} else {
			delete(b, "tool_choice")
			delete(b, "parallel_tool_calls")
		}
		if effort != "" {
			b["reasoning_effort"] = effort
		}
	}
	return mustJSON(b), path, hdr
}
