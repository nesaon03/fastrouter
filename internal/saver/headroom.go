package saver

// Headroom (https://github.com/chopratejas/headroom) context compression,
// following 9Router's open-sse/rtk/headroom.js. Fail-open: any error leaves
// the body unchanged, and a failure pauses calls for a short while.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	tr "github.com/zenpo/fastrouter/internal/translate"
)

const headroomMaxDepth = 8

// Headroom is a client for a Headroom proxy's /v1/compress endpoint.
type Headroom struct {
	Endpoint      string
	Timeout       time.Duration
	CompressUser  bool
	Pause         time.Duration // skip calls this long after a failure
	Client        *http.Client
	pausedUntil   atomic.Int64
	LastSkipCause atomic.Value // string, for the status page
}

// NewHeadroom builds a client for base URL u (e.g. http://localhost:8787).
func NewHeadroom(u string, timeout time.Duration, compressUser bool) *Headroom {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Headroom{Endpoint: compressEndpoint(u), Timeout: timeout, CompressUser: compressUser,
		Pause: 30 * time.Second, Client: &http.Client{}}
}

func compressEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return strings.TrimRight(raw, "/") + "/v1/compress"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/compress"
	u.Fragment = ""
	return u.String()
}

// Available reports whether calls are currently allowed (not paused after a failure).
func (h *Headroom) Available() bool {
	return h != nil && time.Now().UnixNano() >= h.pausedUntil.Load()
}

func (h *Headroom) fail(cause string) {
	h.pausedUntil.Store(time.Now().Add(h.Pause).UnixNano())
	h.LastSkipCause.Store(cause)
}

type hrMsg = map[string]any

func (h *Headroom) call(ctx context.Context, msgs []hrMsg, model string) ([]hrMsg, error) {
	payload := map[string]any{"messages": msgs, "model": model}
	if h.CompressUser {
		payload["config"] = map[string]any{"compress_user_messages": true}
	}
	body, err := marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("proxy returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Messages []hrMsg `json:"messages"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out.Messages == nil {
		return nil, errors.New("proxy response missing messages[]")
	}
	return out.Messages, nil
}

// Compress returns b with its context compressed by the proxy, or b and false.
func (h *Headroom) Compress(ctx context.Context, b []byte, f tr.Format, model string) ([]byte, bool) {
	if !h.Available() {
		return b, false
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil {
		return b, false
	}
	var p *projection
	switch {
	case f == tr.Gemini:
		p = toolOutputs(geminiResponses(body))
	case f == tr.Responses && unsafeResponsesInput(body):
		p = toolOutputs(functionOutputs(body))
	case f == tr.Anthropic:
		p = claudeProjection(body)
	case f == tr.Responses:
		p = responsesProjection(body)
	default:
		msgs, ok := body["messages"].([]any)
		if !ok {
			return b, false
		}
		out, err := h.call(ctx, toMsgs(msgs), model)
		if err != nil {
			h.fail(err.Error())
			return b, false
		}
		body["messages"] = out
		nb, err := marshal(body)
		return nb, err == nil
	}
	if p == nil || len(p.msgs) == 0 {
		return b, false
	}
	out, err := h.call(ctx, p.msgs, model)
	if err != nil {
		h.fail(err.Error())
		return b, false
	}
	if !p.apply(out) {
		return b, false
	}
	nb, err := marshal(body)
	return nb, err == nil
}

func toMsgs(a []any) []hrMsg {
	out := make([]hrMsg, 0, len(a))
	for _, m := range a {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// target is a settable string inside the decoded body.
type target struct {
	set  func(string)
	text string
}

// projection is a list of OpenAI-shaped messages for the proxy plus where
// each message's text goes back (nil target: context only).
type projection struct {
	msgs     []hrMsg
	targets  []*target
	toolOnly bool // tool-output mode: replace only when shorter
}

func (p *projection) add(role, text string, t *target, extra map[string]any) {
	m := hrMsg{"role": role, "content": text}
	for k, v := range extra {
		m[k] = v
	}
	p.msgs = append(p.msgs, m)
	p.targets = append(p.targets, t)
}

func (p *projection) apply(out []hrMsg) bool {
	if len(out) != len(p.msgs) {
		return false
	}
	texts := make([]string, len(out))
	for i, m := range out {
		exp := p.msgs[i]
		if m["role"] != exp["role"] || fmt.Sprint(m["tool_call_id"]) != fmt.Sprint(exp["tool_call_id"]) {
			return false
		}
		t, ok := messageText(m)
		if !ok || (p.toolOnly && strings.TrimSpace(t) == "") {
			return false
		}
		texts[i] = t
	}
	for i, t := range p.targets {
		if t == nil {
			continue
		}
		if p.toolOnly && len(texts[i]) >= len(t.text) {
			continue
		}
		t.set(texts[i])
	}
	return true
}

func messageText(m hrMsg) (string, bool) {
	switch c := m["content"].(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, p := range c {
			switch v := p.(type) {
			case string:
				parts = append(parts, v)
			case map[string]any:
				if s, ok := v["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), true
		}
	}
	return "", false
}

// slot is a (container, key) pair whose value may be visited.
type slot struct {
	get func() any
	set func(any)
}

func mapSlot(m map[string]any, k string) slot {
	return slot{func() any { return m[k] }, func(v any) { m[k] = v }}
}

func arrSlot(a []any, i int) slot {
	return slot{func() any { return a[i] }, func(v any) { a[i] = v }}
}

func unsafeResponsesInput(body map[string]any) bool {
	in, _ := body["input"].([]any)
	for _, it := range in {
		if m, ok := it.(map[string]any); ok {
			if t, ok := m["type"].(string); ok && t != "message" {
				return true
			}
		}
	}
	return false
}

func functionOutputs(body map[string]any) []slot {
	var out []slot
	in, _ := body["input"].([]any)
	for _, it := range in {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call_output" {
			out = append(out, mapSlot(m, "output"))
		}
	}
	return out
}

func geminiResponses(body map[string]any) []slot {
	src := body
	if r, ok := body["request"].(map[string]any); ok {
		src = r
	}
	var out []slot
	contents, _ := src["contents"].([]any)
	for _, c := range contents {
		cm, _ := c.(map[string]any)
		parts, _ := cm["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if fr, ok := pm["functionResponse"].(map[string]any); ok {
				out = append(out, mapSlot(fr, "response"))
			}
		}
	}
	return out
}

// toolOutputs projects plain (non-JSON) text leaves under the given slots.
func toolOutputs(slots []slot) *projection {
	p := &projection{toolOnly: true}
	var visit func(s slot, depth int)
	visit = func(s slot, depth int) {
		if depth > headroomMaxDepth {
			return
		}
		switch v := s.get().(type) {
		case string:
			if strings.TrimSpace(v) == "" || json.Valid([]byte(v)) {
				return
			}
			p.add("tool", v, &target{set: func(t string) { s.set(t) }, text: v},
				map[string]any{"tool_call_id": fmt.Sprintf("headroom-tool-%d", len(p.msgs))})
		case []any:
			for i := range v {
				visit(arrSlot(v, i), depth+1)
			}
		case map[string]any:
			if t, ok := v["type"]; ok && t != "text" && t != "input_text" {
				return
			}
			for _, k := range []string{"result", "output", "content", "text"} {
				if _, ok := v[k]; ok {
					visit(mapSlot(v, k), depth+1)
				}
			}
		}
	}
	for _, s := range slots {
		visit(s, 0)
	}
	return p
}

// textLeaves adds o[k] (a string, or text blocks of blockType) to p.
func (p *projection) textLeaves(role string, o map[string]any, k, blockType string, extra map[string]any) {
	switch v := o[k].(type) {
	case string:
		p.add(role, v, &target{set: func(t string) { o[k] = t }, text: v}, extra)
	case []any:
		for _, blk := range v {
			bm, ok := blk.(map[string]any)
			if !ok || bm["type"] != blockType {
				continue
			}
			if s, ok := bm["text"].(string); ok {
				p.add(role, s, &target{set: func(t string) { bm["text"] = t }, text: s}, extra)
			}
		}
	}
}

func (p *projection) hasTargets() bool {
	for _, t := range p.targets {
		if t != nil {
			return true
		}
	}
	return false
}

func claudeProjection(body map[string]any) *projection {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	p := &projection{}
	p.textLeaves("system", body, "system", "text", nil)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		blocks, _ := mm["content"].([]any)
		var calls []any
		for _, b := range blocks {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_use" {
				args, _ := json.Marshal(orEmpty(bm["input"]))
				calls = append(calls, map[string]any{"id": bm["id"], "type": "function",
					"function": map[string]any{"name": bm["name"], "arguments": string(args)}})
			}
		}
		if role == "assistant" && len(calls) > 0 {
			p.add("assistant", "", nil, map[string]any{"tool_calls": calls})
		}
		p.textLeaves(role, mm, "content", "text", nil)
		for _, b := range blocks {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_result" {
				p.textLeaves("tool", bm, "content", "text", map[string]any{"tool_call_id": bm["tool_use_id"]})
			}
		}
	}
	if !p.hasTargets() {
		return nil
	}
	return p
}

// responsesProjection handles a Responses input made only of message items.
// 9Router round-trips these through its translators; projecting the text
// leaves keeps every other field as it was.
func responsesProjection(body map[string]any) *projection {
	in, ok := body["input"].([]any)
	if !ok {
		return nil
	}
	p := &projection{}
	for _, it := range in {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "developer" {
			role = "system"
		}
		blockType := "input_text"
		if role == "assistant" {
			blockType = "output_text"
		}
		p.textLeaves(role, m, "content", blockType, nil)
	}
	if !p.hasTargets() {
		return nil
	}
	return p
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
