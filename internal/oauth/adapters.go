package oauth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Account is the per-account view request shaping gets (no secrets besides
// the access key passed to Headers).
type Account struct {
	ID    string
	Extra map[string]string // projectId, deviceId, orgId, email, userId, location
}

// Wire formats an adapter can pick (mirrors translate.Format names).
const (
	FmtOpenAI    = "openai"
	FmtAnthropic = "anthropic"
	FmtResponses = "responses"
	FmtGemini    = "gemini"
)

// Adapter shapes requests for one OAuth provider on top of the normal
// translation: the target format/URL, the body envelope and the headers.
type Adapter struct {
	// Route picks the upstream format and URL for a model ("" = provider default).
	Route func(model string, stream bool, a Account) (format, url string)
	// Body rewrites the already-translated body for this account.
	Body func(body []byte, model string, stream bool, a Account) ([]byte, error)
	// Headers sets auth and client headers (replacing the default auth header).
	Headers func(h http.Header, key string, stream bool, a Account)
	// Response wraps the upstream body (e.g. unwrap Code Assist envelopes).
	Response func(io.ReadCloser) io.ReadCloser
	// ForceStream: the upstream only streams; the proxy folds it for JSON clients.
	ForceStream bool
	// Do replaces the HTTP call for providers with a custom transport (Cursor's
	// HTTP/2 Connect stream). body is the translated request; the response it
	// returns is read like an upstream answer.
	Do func(req *http.Request, body []byte, a Account) (*http.Response, error)
}

var adapters = map[string]*Adapter{}

// AdapterFor returns the adapter for provider id, or nil.
func AdapterFor(id string) *Adapter { return adapters[id] }

func bearer(h http.Header, key string) {
	h.Del("x-api-key")
	h.Del("x-goog-api-key")
	h.Set("Authorization", "Bearer "+key)
}

func setAll(h http.Header, kv map[string]string) {
	for k, v := range kv {
		h.Set(k, v)
	}
}

// patchJSON decodes a JSON object, applies fn and re-encodes it.
func patchJSON(body []byte, fn func(m map[string]any)) ([]byte, error) {
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	fn(m)
	return json.Marshal(m)
}

func forceStreamBody(format string) func([]byte, string, bool, Account) ([]byte, error) {
	return func(body []byte, _ string, _ bool, _ Account) ([]byte, error) {
		return patchJSON(body, func(m map[string]any) {
			m["stream"] = true
			if format == FmtOpenAI {
				m["stream_options"] = map[string]any{"include_usage": true}
			}
		})
	}
}

const claudeCLIVersion = "2.1.322"

// ClaudeSystemPrompt must lead the system prompt for Claude subscription tokens.
const ClaudeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

var claudeHeaders = map[string]string{
	"Anthropic-Version":                         "2023-06-01",
	"Anthropic-Beta":                            "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
	"User-Agent":                                "claude-cli/" + claudeCLIVersion + " (external, sdk-cli)",
	"X-App":                                     "cli",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Runtime-Version":               "v24.14.0",
	"X-Stainless-Package-Version":               "0.80.0",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-Timeout":                       "600",
}

// ShapeClaude puts the Claude Code system prompt first (OAuth tokens are
// rejected without it) and drops the client's own copy if present.
func ShapeClaude(body []byte) ([]byte, error) {
	return patchJSON(body, func(m map[string]any) {
		lead := map[string]any{"type": "text", "text": ClaudeSystemPrompt}
		var blocks []any
		switch s := m["system"].(type) {
		case string:
			if s != "" && !strings.HasPrefix(s, ClaudeSystemPrompt) {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
		case []any:
			for _, b := range s {
				if bm, ok := b.(map[string]any); ok && strings.HasPrefix(anyStr(bm["text"]), ClaudeSystemPrompt) {
					continue
				}
				blocks = append(blocks, b)
			}
		}
		m["system"] = append([]any{lead}, blocks...)
	})
}

func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func kimiHeaders(deviceID string) map[string]string {
	model := map[string]string{"darwin": "macOS", "windows": "Windows", "linux": "Linux"}[runtime.GOOS]
	if model == "" {
		model = runtime.GOOS
	}
	if deviceID == "" {
		deviceID = "kimi-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	return map[string]string{"X-Msh-Platform": "fastrouter", "X-Msh-Version": "1.0.0", "X-Msh-Device-Name": hostName(),
		"X-Msh-Device-Model": model + " " + runtime.GOARCH, "X-Msh-Device-Id": deviceID}
}

// clineToken adds the "workos:" prefix Cline expects on WorkOS JWTs.
func clineToken(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(strings.ToLower(t), "workos:") || !strings.HasPrefix(t, "eyJ") {
		return t
	}
	return "workos:" + t
}

func clineHeaders(h http.Header, key string) {
	setAll(h, map[string]string{"HTTP-Referer": "https://cline.bot", "X-Title": "Cline", "User-Agent": "FastRouter/1.0",
		"X-PLATFORM": runtime.GOOS, "X-PLATFORM-VERSION": runtime.Version(), "X-CLIENT-TYPE": "fastrouter",
		"X-CLIENT-VERSION": "1.0.0", "X-CORE-VERSION": "1.0.0", "X-IS-MULTIROOT": "false"})
	bearer(h, clineToken(key))
}

// UnwrapCline strips Cline's {"success":true,"data":{...}} JSON envelope.
// Streams (anything not starting with '{') pass through untouched.
func UnwrapCline(r io.ReadCloser) io.ReadCloser {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(1); len(b) == 0 || b[0] != '{' {
		return readCloser{br, r}
	}
	b, err := io.ReadAll(io.LimitReader(br, 32<<20))
	r.Close()
	if err == nil && bytes.Contains(b[:min(len(b), 64)], []byte(`"success"`)) {
		var env struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(b, &env) == nil && env.Success && len(env.Data) > 0 && env.Data[0] == '{' {
			b = env.Data
		}
	}
	return io.NopCloser(bytes.NewReader(b))
}

type readCloser struct {
	io.Reader
	io.Closer
}

func init() {
	adapters["antigravity"] = &Adapter{
		Route: func(model string, stream bool, _ Account) (string, string) {
			m, _ := splitThinking(model)
			if stream && !strings.Contains(m, "image") {
				return FmtGemini, AntigravityBase + "/v1internal:streamGenerateContent?alt=sse"
			}
			return FmtGemini, AntigravityBase + "/v1internal:generateContent"
		},
		Body: func(b []byte, model string, _ bool, a Account) ([]byte, error) {
			return shapeCodeAssist(b, model, a, true)
		},
		Headers: func(h http.Header, key string, _ bool, _ Account) {
			bearer(h, key)
			h.Set("User-Agent", AntigravityUserAgent)
		},
		Response: UnwrapResponse,
	}
	adapters["gemini-cli"] = &Adapter{
		Route: func(model string, stream bool, _ Account) (string, string) {
			if stream {
				return FmtGemini, GeminiCLIBase + ":streamGenerateContent?alt=sse"
			}
			return FmtGemini, GeminiCLIBase + ":generateContent"
		},
		Body: func(b []byte, model string, _ bool, a Account) ([]byte, error) {
			return shapeCodeAssist(b, model, a, false)
		},
		Headers: func(h http.Header, key string, _ bool, _ Account) {
			bearer(h, key)
			h.Set("User-Agent", "GeminiCLI/"+geminiCLIVersion+" ("+runtime.GOOS+"; "+runtime.GOARCH+"; terminal)")
			h.Set("X-Goog-Api-Client", geminiCLIAPIClient)
		},
		Response: UnwrapResponse,
	}
	adapters["claude"] = &Adapter{
		Body: func(b []byte, _ string, _ bool, _ Account) ([]byte, error) { return ShapeClaude(b) },
		Headers: func(h http.Header, key string, _ bool, _ Account) {
			bearer(h, key)
			setAll(h, claudeHeaders)
		},
	}
	adapters["github"] = &Adapter{
		Route: func(model string, _ bool, _ Account) (string, string) {
			m := strings.ToLower(model)
			switch {
			case strings.Contains(m, "claude"):
				return FmtAnthropic, "https://api.githubcopilot.com/v1/messages"
			case strings.Contains(m, "codex"):
				return FmtResponses, "https://api.githubcopilot.com/responses"
			}
			return FmtOpenAI, "https://api.githubcopilot.com/chat/completions"
		},
		Body: func(b []byte, model string, _ bool, _ Account) ([]byte, error) {
			if !strings.Contains(model, "gpt-5") && !strings.HasPrefix(model, "o") {
				return b, nil
			}
			return patchJSON(b, func(m map[string]any) {
				if v, ok := m["max_tokens"]; ok && m["messages"] != nil {
					m["max_completion_tokens"] = v
					delete(m, "max_tokens")
				}
			})
		},
		Headers: func(h http.Header, key string, _ bool, _ Account) {
			bearer(h, key)
			setAll(h, map[string]string{"copilot-integration-id": "vscode-chat", "editor-version": "vscode/1.110.0",
				"editor-plugin-version": "copilot-chat/0.38.0", "user-agent": "GitHubCopilotChat/0.38.0",
				"openai-intent": "conversation-panel", "x-github-api-version": "2025-04-01", "x-request-id": uuid4(),
				"x-vscode-user-agent-library-version": "electron-fetch", "X-Initiator": "user", "anthropic-version": "2023-06-01"})
		},
	}
	adapters["kimi"] = &Adapter{
		Headers: func(h http.Header, key string, _ bool, a Account) {
			h.Del("Authorization")
			h.Set("x-api-key", key)
			setAll(h, kimiHeaders(a.Extra["deviceId"]))
			h.Set("Anthropic-Beta", "claude-code-20250219,interleaved-thinking-2025-05-14")
		},
	}
	adapters["iflow"] = &Adapter{Headers: func(h http.Header, key string, _ bool, _ Account) { bearer(h, key); h.Set("User-Agent", "iFlow-Cli") }}
	adapters["kilocode"] = &Adapter{
		Headers: func(h http.Header, key string, _ bool, a Account) {
			bearer(h, key)
			if org := a.Extra["orgId"]; org != "" {
				h.Set("X-Kilocode-OrganizationID", org)
			}
		},
	}
	for _, id := range []string{"cline", "clinepass"} {
		adapters[id] = &Adapter{
			Headers:  func(h http.Header, key string, _ bool, _ Account) { clineHeaders(h, key) },
			Response: UnwrapCline,
		}
	}
	codebuddy := func(ua, ide string) *Adapter {
		return &Adapter{ForceStream: true, Body: forceStreamBody(FmtOpenAI),
			Headers: func(h http.Header, key string, _ bool, _ Account) {
				bearer(h, key)
				id := uuid4()
				setAll(h, map[string]string{"User-Agent": ua, "X-Product": "SaaS", "X-IDE-Type": ide, "X-IDE-Name": ide,
					"x-requested-with": "XMLHttpRequest", "x-codebuddy-request": "1", "X-Request-ID": id,
					"X-Conversation-ID": uuid4(), "X-Conversation-Request-ID": id, "X-Conversation-Message-ID": id})
			}}
	}
	adapters["codebuddy-cn"] = codebuddy("CLI/2.108.1 CodeBuddy/2.108.1", "CLI")
	adapters["codebuddy-intl"] = codebuddy("IDE/2.108.1 CodeBuddy/2.108.1", "IDE")
	minimax := &Adapter{
		Headers: func(h http.Header, key string, _ bool, a Account) {
			bearer(h, key)
			_, off := time.Now().Zone()
			setAll(h, map[string]string{"User-Agent": "MiniMaxAgent", "X-Mavis-Agent-Id": "main", "x-api-key": "sk-xxx",
				"X-Mavis-Timezone-Offset": strconv.Itoa(off), "X-Mavis-Session-Id": seedUUID("mavis:" + a.ID)})
		},
	}
	adapters["minimax-code"] = minimax
	adapters["minimax-code-global"] = minimax
	adapters["grok-cli"] = &Adapter{ForceStream: true,
		Body: func(b []byte, model string, _ bool, _ Account) ([]byte, error) { return shapeGrokCLI(b, model) },
		Headers: func(h http.Header, key string, _ bool, a Account) {
			bearer(h, key)
			sess := seedUUID("grok-cli:" + a.ID)
			setAll(h, map[string]string{"User-Agent": "grok-shell/1.0.44 (linux; x86_64)", "x-grok-client-identifier": "grok-shell",
				"x-grok-client-version": "1.0.44", "x-xai-token-auth": "xai-grok-cli", "x-grok-session-id": sess, "x-grok-conv-id": sess,
				"x-grok-req-id": uuid4(), "x-grok-turn-idx": "1"})
			if e := a.Extra["email"]; e != "" {
				h.Set("x-email", e)
			}
			if u := a.Extra["userId"]; u != "" {
				h.Set("x-userid", u)
			}
		},
	}
	adapters["vertex"] = &Adapter{
		Route: func(model string, stream bool, a Account) (string, string) {
			loc := firstNonEmpty(a.Extra["location"], "global")
			host := "aiplatform.googleapis.com"
			if loc != "global" {
				host = loc + "-aiplatform.googleapis.com"
			}
			action := ":generateContent"
			if stream {
				action = ":streamGenerateContent?alt=sse"
			}
			return FmtGemini, "https://" + host + "/v1/projects/" + a.Extra["projectId"] + "/locations/" + loc + "/publishers/google/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + action
		},
		Headers: func(h http.Header, key string, _ bool, _ Account) { bearer(h, key) },
	}
}

var grokAllowed = map[string]bool{"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "reasoning": true, "include": true, "stream": true, "store": true, "temperature": true,
	"top_p": true, "max_output_tokens": true, "text": true, "metadata": true}

// shapeGrokCLI mirrors 9Router's GrokCliExecutor.transformRequest essentials.
func shapeGrokCLI(body []byte, model string) ([]byte, error) {
	return patchJSON(body, func(m map[string]any) {
		m["stream"], m["store"] = true, false
		base := model
		effort := ""
		for _, e := range []string{"-high", "-medium", "-low"} {
			if strings.HasSuffix(base, e) {
				base, effort = strings.TrimSuffix(base, e), e[1:]
			}
		}
		m["model"] = base
		r, _ := m["reasoning"].(map[string]any)
		if r == nil {
			r = map[string]any{}
		}
		if r["summary"] == nil {
			r["summary"] = "concise"
		}
		if effort != "" {
			r["effort"] = effort
		}
		m["reasoning"] = r
		inc, _ := m["include"].([]any)
		has := false
		for _, v := range inc {
			has = has || v == "reasoning.encrypted_content"
		}
		if !has {
			m["include"] = append(inc, "reasoning.encrypted_content")
		}
		for k := range m {
			if !grokAllowed[k] {
				delete(m, k)
			}
		}
	})
}
