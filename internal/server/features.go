package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zenpo/fastrouter/internal/router"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

// ---- Claude Code warmup bypass (after 9Router's bypassHandler.js, MIT) ----

const (
	bypassText        = "CLI Command Execution: Clear Terminal"
	titleSkipPattern  = "Please write a 5-10 word title for the following conversation:"
	namingSystemToken = "isNewTopic"
)

// bypass answers Claude Code's housekeeping calls (warmup, token count, title
// extraction) locally, without spending upstream quota. Only for claude-cli.
func (s *Server) bypass(w http.ResponseWriter, p *proxyReq, st *router.State) bool {
	if !strings.Contains(p.r.UserAgent(), "claude-cli") {
		return false
	}
	b := p.body
	if !bytes.Contains(b, []byte("Warmup")) && !bytes.Contains(b, []byte(`"count"`)) && !bytes.Contains(b, []byte(`"{"`)) &&
		!bytes.Contains(b, []byte("5-10 word title")) && !(st.CCFilterNaming && bytes.Contains(b, []byte(namingSystemToken))) {
		return false // cheap byte prefilter: nearly every real request stops here
	}
	ir, err := p.parse()
	if err != nil || len(ir.Messages) == 0 {
		return false
	}
	msgs := ir.Messages
	text := func(m tr.Message) string {
		var parts []string
		for _, pt := range m.Parts {
			if pt.Type == tr.PartText {
				parts = append(parts, pt.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	hit, reply := false, bypassText
	last := msgs[len(msgs)-1]
	switch {
	case last.Role == "assistant" && len(last.Parts) > 0 && last.Parts[0].Type == tr.PartText && last.Parts[0].Text == "{":
		hit = true
	case text(msgs[0]) == "Warmup":
		hit = true
	case len(msgs) == 1 && msgs[0].Role == "user" && text(msgs[0]) == "count":
		hit = true
	}
	if !hit {
		var user []string
		for _, m := range msgs {
			if m.Role == "user" {
				user = append(user, text(m))
			}
		}
		hit = strings.Contains(strings.Join(user, " "), titleSkipPattern)
	}
	if !hit && st.CCFilterNaming && strings.Contains(ir.SystemText(), namingSystemToken) {
		for _, m := range msgs {
			if m.Role == "user" {
				words := strings.Fields(text(m))
				words = words[:min(3, len(words))]
				nb, _ := json.Marshal(map[string]any{"isNewTopic": true, "title": strings.Join(words, " ")})
				hit, reply = true, string(nb)
				break
			}
		}
	}
	if !hit {
		return false
	}
	ct := "application/json"
	if p.stream {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-FastRouter-Bypass", "claude-code")
	_, _ = w.Write(tr.Synthetic(p.cf, p.model, reply, p.stream))
	return true
}

// ---- capacityAdapter vision fallback ----

var imageMarkers = [...][]byte{
	tr.OpenAI:    []byte(`image_url`),
	tr.Anthropic: []byte(`"image"`),
	tr.Gemini:    []byte(`nline`), // inlineData / inline_data
	tr.Responses: []byte(`input_image`),
}

// visionTargets puts the capacity-adapter vision models first when the
// request carries images and none of the resolved models can read them.
func (s *Server) visionTargets(p *proxyReq, st *router.State, targets []router.Target) []router.Target {
	if !bytes.Contains(p.body, imageMarkers[p.cf]) && !(p.cf == tr.Gemini && bytes.Contains(p.body, []byte("ileData"))) {
		return targets
	}
	for _, t := range targets {
		if st.SupportsVision(t) {
			return targets
		}
	}
	if ir, err := p.parse(); err != nil || !ir.HasImages() {
		return targets
	}
	// Only vision models the key may call (M9).
	key := keyFrom(p.r.Context())
	var pool []router.Target
	for _, t := range st.VisionFallback() {
		if key.Allows(t.Label) {
			pool = append(pool, t)
		}
	}
	if len(pool) == 0 {
		return targets
	}
	return append(pool, targets...)
}

// ---- providerThinking (imported per-provider thinking modes) ----

// modeThinking maps a 9Router thinking mode onto a request setting:
// "on" = budget 10000, "off" = disabled, anything else is an effort level.
func modeThinking(mode string) *tr.Thinking {
	switch mode {
	case "on":
		return &tr.Thinking{Mode: "enabled", Budget: 10000}
	case "off":
		return &tr.Thinking{Mode: "disabled"}
	}
	return &tr.Thinking{Effort: mode}
}

// injectThinking adds the provider's thinking mode to a passthrough body the
// client left without one, using the field its format expects.
func injectThinking(b []byte, f tr.Format, mode string, h *reqHead) []byte {
	switch f {
	case tr.OpenAI: // as 9Router does
		switch mode {
		case "on":
			return injectTopLevel(b, "thinking", `{"type":"enabled","budget_tokens":10000}`)
		case "off":
			return injectTopLevel(b, "thinking", `{"type":"disabled"}`)
		}
		return injectTopLevel(b, "reasoning_effort", strconv.Quote(mode))
	case tr.Anthropic:
		if mode == "off" {
			return injectTopLevel(b, "thinking", `{"type":"disabled"}`)
		}
		if len(h.Temperature) > 0 {
			return b // Anthropic rejects thinking with a custom temperature
		}
		budget := modeThinking(mode).BudgetFor()
		if h.MaxTokens > 0 && budget >= h.MaxTokens {
			budget = h.MaxTokens - 1
		}
		if budget < 1024 {
			return b
		}
		return injectTopLevel(b, "thinking", `{"type":"enabled","budget_tokens":`+strconv.Itoa(budget)+`}`)
	case tr.Responses:
		effort := mode
		switch mode {
		case "off":
			return b
		case "on":
			effort = "medium"
		case "max", "xhigh":
			effort = "high"
		}
		return injectTopLevel(b, "reasoning", `{"effort":`+strconv.Quote(effort)+`}`)
	}
	return b
}

// injectTopLevel adds "key":raw as the first field of a JSON object. The
// caller makes sure key is not already present.
func injectTopLevel(b []byte, key, raw string) []byte {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return b
	}
	out := make([]byte, 0, len(b)+len(key)+len(raw)+4)
	out = append(out, b[:i+1]...)
	out = append(out, strconv.Quote(key)...)
	out = append(out, ':')
	out = append(out, raw...)
	if j := skipSpace(b, i+1); j < len(b) && b[j] != '}' {
		out = append(out, ',')
	}
	return append(out, b[i+1:]...)
}

// ---- local API key identity ----

type keyCtx struct{}

func withKey(ctx context.Context, k *router.KeyInfo) context.Context {
	return context.WithValue(ctx, keyCtx{}, k)
}

func keyFrom(ctx context.Context) *router.KeyInfo {
	k, _ := ctx.Value(keyCtx{}).(*router.KeyInfo)
	return k
}
