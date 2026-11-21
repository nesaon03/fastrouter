// Package discover reads a provider's own model list at run time, so a model
// released after this build still shows up and routes.
//
// It only asks for what a provider already publishes: the same /models endpoint
// the vendor's own examples use. Nothing is scraped, and no request is sent
// without an account the user already configured.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/catalog"
)

// ErrNoEndpoint means this provider publishes no model list this package can
// read (Codex, Cursor, Antigravity and other sign-in protocols), so the compiled
// catalog stays the only source for it.
var ErrNoEndpoint = errors.New("provider has no model list endpoint")

// ErrUnauthorized means the credential was rejected: the caller should stop
// polling this provider until the account changes.
var ErrUnauthorized = errors.New("credential rejected by the model list")

// maxList caps a response body. A model list is JSON, not a stream.
const maxList = 4 << 20

// Result is one provider's discovery result.
type Result struct {
	Provider string
	Models   []string
	URL      string // the endpoint that answered (for the dashboard)
	Status   int
}

// Endpoint guesses the model-list URLs to try for p, most likely first. A
// provider's chat endpoint is the only reliable base, so the list URL is
// derived from it rather than hard-coded per provider.
func Endpoint(p *catalog.Provider) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimRight(u, "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	// Gemini's GeminiURL already points at .../v1beta/models.
	if p.GeminiURL != "" {
		if strings.HasSuffix(strings.TrimRight(p.GeminiURL, "/"), "/models") {
			add(p.GeminiURL)
		} else {
			add(p.GeminiURL + "/models")
		}
	}
	for _, raw := range []string{p.ChatURL, p.ResponsesURL, p.MessagesURL} {
		if raw == "" {
			continue
		}
		base := stripQuery(raw)
		for _, suffix := range []string{"/chat/completions", "/completions", "/messages", "/responses"} {
			if strings.HasSuffix(base, suffix) {
				base = strings.TrimSuffix(base, suffix)
				break
			}
		}
		// The base usually keeps its /v1 (openai.com/v1), so /models is right
		// there; a base without it (api.deepseek.com) needs the prefix added.
		if strings.HasSuffix(base, "/v1") || strings.HasSuffix(base, "/v1beta") || strings.HasSuffix(base, "/v4") {
			add(base + "/models")
		} else {
			add(base + "/models")
			add(base + "/v1/models")
		}
	}
	// Anthropic's list endpoint lives under /v1 whatever the messages URL said.
	if p.MessagesURL != "" {
		if u := anthropicBase(p.MessagesURL); u != "" {
			add(u + "/models")
			add(u + "/v1/models")
		}
	}
	if len(out) == 0 {
		return nil, ErrNoEndpoint
	}
	return out, nil
}

func stripQuery(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

func anthropicBase(u string) string {
	u = stripQuery(u)
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			return u[:i+3] + rest[:j]
		}
	}
	return ""
}

// isAnthropic reports whether a list URL belongs to an Anthropic-shape API,
// which wants x-api-key and a version header instead of a bearer token.
func isAnthropic(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "anthropic.com") || strings.Contains(l, "api.z.ai") ||
		strings.Contains(l, "/anthropic") || strings.Contains(l, "kimi.com")
}

// isGemini reports whether a list URL is Google's native list shape.
func isGemini(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "generativelanguage.googleapis.com") || strings.Contains(l, "/v1beta/models")
}

// List asks p for its model ids using key. It tries each candidate endpoint
// until one answers with a parseable list, so a wrong guess costs one 404.
func List(ctx context.Context, c *http.Client, p *catalog.Provider, key string) (Result, error) {
	if key == "" {
		return Result{}, fmt.Errorf("%s: no credential", p.ID)
	}
	urls, err := Endpoint(p)
	if err != nil {
		return Result{}, err
	}
	var lastErr error
	for _, u := range urls {
		ids, status, err := get(ctx, c, u, key)
		switch {
		case errors.Is(err, ErrUnauthorized):
			return Result{Provider: p.ID, URL: u, Status: status}, err
		case err != nil:
			lastErr = err
			continue
		}
		if len(ids) == 0 {
			lastErr = fmt.Errorf("%s: empty model list", u)
			continue
		}
		return Result{Provider: p.ID, Models: ids, URL: u, Status: status}, nil
	}
	if lastErr == nil {
		lastErr = ErrNoEndpoint
	}
	return Result{Provider: p.ID}, lastErr
}

// ListWithHeaders asks for a list with headers chosen by the caller, for
// providers whose credentials are not a plain key: a sign-in provider's OAuth
// token needs the same vendor headers its chat request uses, or the list is
// rejected. It tries each URL in order and returns the first usable answer.
func ListWithHeaders(ctx context.Context, c *http.Client, urls []string, headers map[string]string) (Result, error) {
	if len(urls) == 0 {
		return Result{}, ErrNoEndpoint
	}
	var lastErr error
	for _, u := range urls {
		ids, status, err := getWith(ctx, c, u, headers)
		switch {
		case errors.Is(err, ErrUnauthorized):
			return Result{URL: u, Status: status}, err
		case err != nil:
			lastErr = err
			continue
		}
		if len(ids) == 0 {
			lastErr = fmt.Errorf("%s: empty model list", u)
			continue
		}
		return Result{Models: ids, URL: u, Status: status}, nil
	}
	if lastErr == nil {
		lastErr = ErrNoEndpoint
	}
	return Result{}, lastErr
}

// getWith is get with caller-supplied headers.
func getWith(ctx context.Context, c *http.Client, url string, headers map[string]string) ([]string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity") // a JSON list is never worth gzipping

	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, resp.StatusCode, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, resp.StatusCode, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxList))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	ids, err := decode(body, isGemini(url))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s: %w", url, err)
	}
	return ids, resp.StatusCode, nil
}

// get performs one list request and decodes the ids. Auth is tried one shape at
// a time rather than all at once: some gateways route on which credential header
// they see, and a stray one changes the answer (NVIDIA returns an empty list if
// an Anthropic version header is present).
func get(ctx context.Context, c *http.Client, url, key string) ([]string, int, error) {
	profiles := []map[string]string{
		{"Authorization": "Bearer " + key},
		{"x-api-key": key},
		{"x-goog-api-key": key},
	}
	var lastErr error
	for i, p := range profiles {
		ids, status, err := getWith(ctx, c, url, p)
		if err == nil && len(ids) > 0 {
			return ids, status, nil
		}
		if err != nil {
			if errors.Is(err, ErrUnauthorized) && i < len(profiles)-1 {
				lastErr = err
				continue // this credential header is not the one it wants
			}
			return nil, status, err
		}
		lastErr = fmt.Errorf("%s: empty model list", url)
	}
	if lastErr == nil {
		lastErr = ErrNoEndpoint
	}
	return nil, 0, lastErr
}

// decode reads both published shapes:
//
//	OpenAI / Anthropic  {"data":[{"id":"â€¦"}]}
//	Gemini              {"models":[{"name":"models/â€¦"}]}
func decode(body []byte, gemini bool) ([]string, error) {
	var openai struct {
		Data []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &openai); err == nil && len(openai.Data) > 0 {
		out := make([]string, 0, len(openai.Data))
		for _, m := range openai.Data {
			if m.ID != "" {
				out = append(out, m.ID)
			}
		}
		if len(out) > 0 {
			sort.Strings(out)
			return out, nil
		}
	}
	var gem struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &gem); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(gem.Models))
	for _, m := range gem.Models {
		name := strings.TrimPrefix(m.Name, "models/")
		if name == "" {
			continue
		}
		// Embedding and AQA entries are not chat models; listing them would put
		// unroutable names in the picker.
		if len(m.SupportedGenerationMethods) > 0 && !contains(m.SupportedGenerationMethods, "generateContent") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 && !gemini {
		return nil, errors.New("no model list in the response")
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// IsWireID reports whether model is a value in an Upstream map, i.e. the id the
// provider actually serves rather than the public catalog id. Those must never
// be advertised: the catalog already maps the public id onto them, and listing
// both would give two names for one model, one of them unpriced.
func IsWireID(upstream map[string]string, model string) bool {
	for _, v := range upstream {
		if v == model {
			return true
		}
	}
	return false
}

// PublicID turns an id a provider reports into the id callers should use.
//
// Some providers list their *served* ids, and the catalog maps a friendlier
// public id onto them (DeepSeek: deepseek-flash ← deepseek-v4.1-flash;
// Antigravity: gemini-3.8-flash-low(low) ← gemini-3.8-flash-low). Reverse that
// mapping instead of dropping the entry, so the public name is what gets stored
// and a model that is genuinely new still comes through.
//
// drop is true for an id that can only be a wire id: it carries a thinking
// suffix the adapter appends, so there is no name a caller could use.
func PublicID(upstream map[string]string, model string) (id string, drop bool) {
	if len(upstream) == 0 {
		return model, false
	}
	if _, isPublic := upstream[model]; isPublic {
		return model, false // already the public name
	}
	for pub, wire := range upstream {
		if wire == model {
			return pub, false
		}
	}
	if hasWireSuffix(model) {
		return "", true
	}
	return model, false
}

// hasWireSuffix reports an adapter-appended suffix such as "(high)" or "(low)".
func hasWireSuffix(model string) bool {
	i := strings.LastIndex(model, "(")
	if i <= 0 || !strings.HasSuffix(model, ")") {
		return false
	}
	switch model[i+1 : len(model)-1] {
	case "high", "medium", "low", "none", "max", "minimal":
		return true
	}
	return false
}

// nonChat lists id fragments that name a non-chat model. A provider's list mixes
// embeddings, speech and image models in with chat models, and only chat models
// belong in a completion router's model list. The test is conservative: when in
// doubt an id is kept, because a wrongly hidden model is worse than a wrongly
// shown one.
var nonChat = []string{
	"embed", "tts", "whisper", "transcrib", "moderation", "realtime",
	"dall-e", "sora", "rerank", "imagen", "stable-diffusion", "flux",
	"text-to-speech", "speech-to-text", "image-gen", "-image", "image-",
	"imagine", "vision-gen",
}

// LooksLikeChat reports whether a discovered id is plausibly a chat/completion
// model. Same spirit as catalog.LooksLikeVision: a name heuristic, because no
// provider publishes capabilities in the model list.
func LooksLikeChat(id string) bool {
	l := strings.ToLower(id)
	for _, s := range nonChat {
		if strings.Contains(l, s) {
			return false
		}
	}
	return true
}
