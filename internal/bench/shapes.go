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

// System prompt used by Simulate IDE (the size and tone of a real agent prompt).
var ideSystem = strings.TrimSpace(`
You are a coding agent running inside the user's IDE. You help the user with software engineering tasks: reading code, explaining it, editing files and running commands.

<tool_calling>
You have tools to read files, search the codebase, edit files and run terminal commands. Only call a tool when it is needed to answer. Never refer to tool names when talking to the user. If you can answer from the context already provided, answer directly without calling tools.
</tool_calling>

<making_code_changes>
When making changes, keep them minimal and focused on the request. Preserve existing style and formatting. Never output large unchanged blocks of code. Make sure generated code can run immediately: include imports and dependencies.
</making_code_changes>

<communication>
Be concise. Use markdown for code. Use backticks for file, function and class names. Do not apologise. Do not invent facts about code you have not seen.
</communication>

<environment>
OS: windows 10.0.19045. Shell: powershell. Workspace: C:\Users\dev\project (git repository, branch main). Today's date: 2026-11-17.
</environment>`)

const ideTask = "\n\n<user_query>\nUsing only the context above, answer directly without calling any tools.\n</user_query>"

type tool struct {
	name, desc string
	props      map[string]any
	required   []string
}

var ideTools = []tool{
	{"read_file", "Read a file from the workspace. Returns the file contents with line numbers.",
		map[string]any{"path": str("Path relative to the workspace root"), "offset": num("First line (1-based)"), "limit": num("Number of lines")}, []string{"path"}},
	{"edit_file", "Replace an exact string in a file with new text.",
		map[string]any{"path": str("File to edit"), "old_string": str("Exact text to replace"), "new_string": str("Replacement text")}, []string{"path", "old_string", "new_string"}},
	{"run_terminal_cmd", "Run a shell command in the workspace and return its output.",
		map[string]any{"command": str("The command to run"), "timeout_ms": num("Timeout in milliseconds")}, []string{"command"}},
	{"grep_search", "Search file contents with a regular expression.",
		map[string]any{"pattern": str("Regular expression"), "path": str("Directory or file to search"), "glob": str("Optional file glob")}, []string{"pattern"}},
	{"codebase_search", "Semantic search over the codebase for code relevant to a question.",
		map[string]any{"query": str("What to look for"), "directories": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, []string{"query"}},
}

func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func num(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }

func schema(t tool) map[string]any {
	return map[string]any{"type": "object", "properties": t.props, "required": t.required}
}

// Build returns the body, path and extra headers for one request.
func Build(o Options, model, ep string) ([]byte, string, map[string]string) {
	text := userText(o)
	hdr := map[string]string{"User-Agent": "fastrouter-bench/1"}
	var b map[string]any
	switch ep {
	case Messages: // Claude Code shape
		hdr["anthropic-version"] = "2023-06-01"
		b = map[string]any{"model": model, "max_tokens": o.MaxTokens, "stream": o.Stream,
			"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}}
		if o.IDE {
			hdr["User-Agent"] = "fastrouter-bench/1 (simulate claude-code)"
			var tools []any
			for _, t := range ideTools {
				tools = append(tools, map[string]any{"name": t.name, "description": t.desc, "input_schema": schema(t)})
			}
			b["system"] = []any{map[string]any{"type": "text", "text": ideSystem, "cache_control": map[string]any{"type": "ephemeral"}}}
			b["tools"] = tools
			b["metadata"] = map[string]any{"user_id": "fastrouter-bench"}
			b["messages"] = []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "<system-reminder>Workspace is a Go project.</system-reminder>"},
				map[string]any{"type": "text", "text": text + ideTask}}}}
		}
		return mustJSON(b), "/v1/messages", hdr
	case Responses: // Codex shape
		b = map[string]any{"model": model, "stream": o.Stream, "max_output_tokens": max(o.MaxTokens, 16), "store": false,
			"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}}
		if o.IDE {
			hdr["User-Agent"] = "fastrouter-bench/1 (simulate codex)"
			var tools []any
			for _, t := range ideTools {
				tools = append(tools, map[string]any{"type": "function", "name": t.name, "description": t.desc, "parameters": schema(t), "strict": false})
			}
			b["instructions"] = ideSystem
			b["tools"] = tools
			b["tool_choice"] = "auto"
			b["parallel_tool_calls"] = false
			b["input"] = []any{
				map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>\n  <cwd>C:\\Users\\dev\\project</cwd>\n  <shell>powershell</shell>\n</environment_context>"}}},
				map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text + ideTask}}}}
		}
		return mustJSON(b), "/v1/responses", hdr
	default: // Chat Completions: Cursor / Cline / Continue shape
		b = map[string]any{"model": model, "max_tokens": o.MaxTokens, "stream": o.Stream,
			"messages": []any{map[string]any{"role": "user", "content": text}}}
		if o.Stream {
			b["stream_options"] = map[string]any{"include_usage": true}
		}
		if o.IDE {
			hdr["User-Agent"] = "fastrouter-bench/1 (simulate cursor)"
			var tools []any
			for _, t := range ideTools {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.name, "description": t.desc, "parameters": schema(t)}})
			}
			b["tools"] = tools
			b["tool_choice"] = "auto"
			b["messages"] = []any{map[string]any{"role": "system", "content": ideSystem},
				map[string]any{"role": "user", "content": "<additional_data>\nOpen file: internal/config/config.go\n</additional_data>" + "\n\n" + text + ideTask}}
		}
		return mustJSON(b), "/v1/chat/completions", hdr
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
