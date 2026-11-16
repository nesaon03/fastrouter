package oauth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

// Google sign-ins: Antigravity and Gemini CLI use Google's installed-app
// OAuth (loopback redirect) and Cloud Code Assist's v1internal API, which
// wraps a Gemini generateContent body as {project, model, request}.
// Clients are the public desktop clients 9Router ships (open-sse/providers/shared.js).
const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
	codeAssistLoad    = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	codeAssistOnboard = "https://cloudcode-pa.googleapis.com/v1internal:onboardUser"

	AntigravityBase      = "https://daily-cloudcode-pa.googleapis.com"
	AntigravityUserAgent = "antigravity/hub/2.24.0 darwin/arm64"
	GeminiCLIBase        = "https://cloudcode-pa.googleapis.com/v1internal"
	geminiCLIVersion     = "0.34.0"
	geminiCLIAPIClient   = "google-genai-sdk/1.41.0 gl-node/v22.19.0"
)

type googleClient struct {
	id, secret string
	scopes     []string
	userAgent  string // loadCodeAssist / onboard
}

var (
	antigravityClient = googleClient{
		id:     "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
		secret: "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",
		scopes: []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cclog",
			"https://www.googleapis.com/auth/experimentsandconfigs"},
		userAgent: AntigravityUserAgent,
	}
	geminiCLIClient = googleClient{
		id:     "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com",
		secret: "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",
		scopes: []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile"},
	}
)

func init() {
	register(googleSpec("antigravity", antigravityClient, true))
	register(googleSpec("gemini-cli", geminiCLIClient, false))
}

func googleSpec(id string, gc googleClient, onboard bool) *Spec {
	return &Spec{ID: id, Kind: KindBrowser, Risk: true, RefreshLead: 5 * time.Minute,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			p := &Pending{Kind: KindBrowser, State: randHex(16), redirect: e.RedirectURI, Expires: time.Now().Add(10 * time.Minute)}
			q := url.Values{"client_id": {gc.id}, "response_type": {"code"}, "redirect_uri": {e.RedirectURI},
				"scope": {strings.Join(gc.scopes, " ")}, "state": {p.State}, "access_type": {"offline"}, "prompt": {"consent"}}
			p.URL = googleAuthURL + "?" + q.Encode()
			return p, nil
		},
		Exchange: func(ctx context.Context, e Env, p *Pending, q url.Values) (*Result, error) {
			var t tokenResp
			_, err := postForm(ctx, e.Client, googleTokenURL, url.Values{"grant_type": {"authorization_code"}, "client_id": {gc.id},
				"client_secret": {gc.secret}, "code": {q.Get("code")}, "redirect_uri": {p.redirect}}, nil, &t)
			if err != nil {
				return nil, fmt.Errorf("token exchange failed: %w", err)
			}
			r := &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn,
				Meta: map[string]any{"scope": t.Scope}}
			var ui struct{ Email, Name string }
			_, _ = getJSON(ctx, e.Client, googleUserInfoURL, map[string]string{"Authorization": "Bearer " + t.AccessToken}, &ui)
			r.Email, r.Name = ui.Email, ui.Email
			pid, tier := loadCodeAssist(ctx, e.Client, t.AccessToken, gc.userAgent, !onboard)
			if pid != "" {
				r.Meta["projectId"] = pid
			}
			if onboard && pid != "" {
				go onboardUser(e.Client, t.AccessToken, gc.userAgent, tier)
			}
			return r, nil
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			var t tokenResp
			_, err := postForm(ctx, e.Client, googleTokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {Secret(c, "refreshToken")},
				"client_id": {gc.id}, "client_secret": {gc.secret}}, nil, &t)
			if err != nil {
				return nil, refreshErr(err)
			}
			r := &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}
			if Meta(c, "projectId") == "" { // imported without a project: look it up once
				if pid, _ := loadCodeAssist(ctx, e.Client, t.AccessToken, gc.userAgent, !onboard); pid != "" {
					r.Meta = map[string]any{"projectId": pid}
				}
			}
			return r, nil
		},
	}
}

func clientMetadata() map[string]int {
	p := 0
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		p = 2
	case "darwin/amd64":
		p = 1
	case "linux/arm64":
		p = 4
	case "linux/amd64":
		p = 3
	case "windows/amd64", "windows/arm64", "windows/386":
		p = 5
	}
	return map[string]int{"ideType": 9, "platform": p, "pluginType": 2}
}

func loadCodeAssist(ctx context.Context, c *http.Client, token, ua string, cliMode bool) (project, tier string) {
	in := map[string]any{"metadata": clientMetadata()}
	if cliMode {
		in["mode"] = 1
	}
	h := map[string]string{"Authorization": "Bearer " + token}
	if ua != "" {
		h["User-Agent"] = ua
		h["x-request-source"] = "local"
	}
	var out struct {
		Project      any `json:"cloudaicompanionProject"`
		AllowedTiers []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"isDefault"`
		} `json:"allowedTiers"`
	}
	if _, err := postJSON(ctx, c, codeAssistLoad, in, h, &out); err != nil {
		return "", ""
	}
	switch v := out.Project.(type) {
	case string:
		project = v
	case map[string]any:
		project = anyStr(v["id"])
	}
	tier = "legacy-tier"
	for _, t := range out.AllowedTiers {
		if t.IsDefault && t.ID != "" {
			tier = strings.TrimSpace(t.ID)
			break
		}
	}
	return project, tier
}

// onboardUser finishes Antigravity onboarding in the background (9Router postExchange).
func onboardUser(c *http.Client, token, ua, tier string) {
	h := map[string]string{"Authorization": "Bearer " + token, "User-Agent": ua, "x-request-source": "local"}
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var out struct {
			Done bool `json:"done"`
		}
		_, err := postJSON(ctx, c, codeAssistOnboard, map[string]any{"tierId": tier, "metadata": clientMetadata()}, h, &out)
		cancel()
		if err != nil || out.Done {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// ---- request shaping ----

var thinkSuffix = regexp.MustCompile(`^(.*)\(([^()]+)\)\s*$`)

// splitThinking turns "model(high)" (9Router's upstream id form) into model + level.
func splitThinking(model string) (string, string) {
	if m := thinkSuffix.FindStringSubmatch(model); m != nil {
		return strings.TrimSpace(m[1]), strings.ToLower(strings.TrimSpace(m[2]))
	}
	return model, ""
}

var levelBudget = map[string]int{"minimal": 512, "low": 1024, "medium": 8192, "high": 24576, "max": 32768, "xhigh": 32768}

var badFnChars = regexp.MustCompile(`[^a-zA-Z0-9_.:\-]`)

func sanitizeFn(name string) string {
	if name == "" {
		return "_unknown"
	}
	s := badFnChars.ReplaceAllString(name, "_")
	if c := s[0]; !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		s = "_" + s
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// agDefaultSignature stands in for a lost thoughtSignature: Gemini 3+ rejects
// a replayed functionCall without one, and most clients do not keep them.
// This is Google's documented validator-skip value (9Router sends a captured
// signature instead, DEFAULT_THINKING_AG_SIGNATURE).
const agDefaultSignature = "skip_thought_signature_validator"

var agRequestBlacklist = []string{"output_config", "thinking", "reasoning_effort", "reasoning", "enable_thinking", "thinking_budget"}

var agPromptRewrites = []*regexp.Regexp{
	regexp.MustCompile(`You are a Claude agent, built on Anthropic's Claude Agent SDK\.`),
	regexp.MustCompile(`(?im)^x-anthropic-billing-header:[^\n]*(?:\r?\n)*`),
}

// shapeCodeAssist wraps a Gemini body for v1internal. antigravity adds the
// IDE envelope (requestId, userAgent, sessionId) and its content fixes.
func shapeCodeAssist(body []byte, model string, a Account, antigravity bool) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if inner, ok := req["request"].(map[string]any); ok && req["project"] != nil { // already wrapped (client sent v1internal)
		req = inner
	}
	model, level := splitThinking(model)
	for _, k := range agRequestBlacklist {
		delete(req, k)
	}
	gc, _ := req["generationConfig"].(map[string]any)
	if gc == nil {
		gc = map[string]any{}
	}
	if level != "" {
		tc := map[string]any{"includeThoughts": true}
		if strings.HasPrefix(model, "gemini-3") || strings.HasPrefix(model, "gemini-4") {
			tc["thinkingLevel"] = level
		} else if b := levelBudget[level]; b > 0 {
			tc["thinkingBudget"] = b
		}
		gc["thinkingConfig"] = tc
	}
	if mt, ok := gc["maxOutputTokens"].(float64); ok && mt > 64000 {
		gc["maxOutputTokens"] = 64000
	}
	if len(gc) > 0 {
		req["generationConfig"] = gc
	}
	if antigravity {
		fixAntigravityContents(req)
		if si, ok := req["systemInstruction"].(map[string]any); ok {
			if parts, ok := si["parts"].([]any); ok {
				for _, p := range parts {
					if pm, ok := p.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok {
							for _, re := range agPromptRewrites {
								t = re.ReplaceAllString(t, "")
							}
							pm["text"] = t
						}
					}
				}
			}
		}
		delete(req, "safetySettings")
		session := a.Extra["sessionId"]
		if session == "" {
			session = numericSession(a.ID)
		}
		req["sessionId"] = session
	}
	env := map[string]any{"model": model, "request": req}
	if pid := a.Extra["projectId"]; pid != "" {
		env["project"] = pid
	}
	if antigravity {
		if env["project"] == nil {
			env["project"] = "useful-flow-" + randHex(3)[:5]
		}
		env["userAgent"] = "antigravity"
		n := 1
		if cs, ok := req["contents"].([]any); ok && len(cs) > 0 {
			n = len(cs)*2 - 1
		}
		conv := seedUUID("antigravity:conversation:" + a.ID)
		traj := seedUUID("antigravity:trajectory:" + a.ID + ":" + model + ":agent")
		env["requestId"] = fmt.Sprintf("agent/%s/%d/%s/%d", conv, time.Now().UnixMilli(), traj, n)
	}
	return json.Marshal(env)
}

func fixAntigravityContents(req map[string]any) {
	cs, _ := req["contents"].([]any)
	var out []any
	for _, c := range cs {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := cm["parts"].([]any)
		var keep []any
		signed := false
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok || len(pm) == 0 {
				continue
			}
			if fc, ok := pm["functionCall"].(map[string]any); ok {
				fc["name"] = sanitizeFn(anyStr(fc["name"]))
				if _, has := pm["thoughtSignature"]; !has && !signed {
					pm["thoughtSignature"] = agDefaultSignature
				}
				signed = true
			} else {
				if th, _ := pm["thought"].(bool); th {
					continue
				}
				if _, has := pm["thoughtSignature"]; has && pm["text"] == nil {
					continue
				}
			}
			if fr, ok := pm["functionResponse"].(map[string]any); ok {
				fr["name"] = sanitizeFn(anyStr(fr["name"]))
				cm["role"] = "user"
			}
			keep = append(keep, pm)
		}
		if len(keep) == 0 {
			continue
		}
		cm["parts"] = keep
		if n := len(out); n > 0 {
			if last := out[n-1].(map[string]any); last["role"] == cm["role"] {
				last["parts"] = append(last["parts"].([]any), keep...)
				continue
			}
		}
		out = append(out, cm)
	}
	if len(out) > 0 && out[0].(map[string]any)["role"] != "user" {
		out = append([]any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "..."}}}}, out...)
	}
	if out != nil {
		req["contents"] = out
	}
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		var decls []any
		seen := map[string]bool{}
		for _, g := range tools {
			gm, _ := g.(map[string]any)
			fds, _ := gm["functionDeclarations"].([]any)
			for _, fd := range fds {
				fm, ok := fd.(map[string]any)
				if !ok {
					continue
				}
				n := sanitizeFn(anyStr(fm["name"]))
				if seen[n] {
					continue
				}
				seen[n] = true
				fm["name"] = n
				if fm["parameters"] == nil {
					fm["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string", "description": "Brief explanation"}}, "required": []any{"reason"}}
				}
				decls = append(decls, fm)
			}
		}
		if len(decls) > 0 {
			req["tools"] = []any{map[string]any{"functionDeclarations": decls}}
			req["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "VALIDATED"}}
		} else {
			delete(req, "tools")
			delete(req, "toolConfig")
		}
	}
}

func seedUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func numericSession(seed string) string {
	sum := sha256.Sum256([]byte("antigravity:session:" + seed))
	var n uint64
	for _, c := range sum[:8] {
		n = n<<8 | uint64(c)
	}
	return fmt.Sprintf("-%d", n>>1)
}

// UnwrapResponse turns Code Assist's {"response": {...}} envelope (per SSE
// event or for the whole JSON body) back into a plain Gemini response.
func UnwrapResponse(r io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer r.Close()
		br := bufio.NewReaderSize(r, 64<<10)
		var whole bytes.Buffer
		sse := false
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				trim := bytes.TrimLeft(line, " \t")
				switch {
				case bytes.HasPrefix(trim, []byte("data:")):
					sse = true
					payload := bytes.TrimSpace(trim[5:])
					if inner := unwrapJSON(payload); inner != nil {
						line = append(append([]byte("data: "), inner...), '\n')
					}
					if _, werr := pw.Write(line); werr != nil {
						return
					}
				case sse:
					if _, werr := pw.Write(line); werr != nil {
						return
					}
				default:
					whole.Write(line)
				}
			}
			if err != nil {
				if !sse {
					b := whole.Bytes()
					if inner := unwrapJSON(bytes.TrimSpace(b)); inner != nil {
						b = inner
					}
					_, _ = pw.Write(b)
				}
				if err == io.EOF {
					err = nil
				}
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return readCloser{pr, closeBoth{pr, r}}
}

type closeBoth struct{ a, b io.Closer }

func (c closeBoth) Close() error { c.a.Close(); return c.b.Close() }

func unwrapJSON(b []byte) []byte {
	if len(b) == 0 || b[0] != '{' || !bytes.Contains(b, []byte(`"response"`)) {
		return nil
	}
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(b, &env) != nil || len(env.Response) == 0 || env.Response[0] != '{' {
		return nil
	}
	return env.Response
}
