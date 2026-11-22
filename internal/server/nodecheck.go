package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nesaon03/fastrouter/internal/secret"
)

// "Check API" for a custom endpoint form (9Router's /api/provider-nodes/validate):
// lists the endpoint's models with the given key, falling back to one tiny
// "Say OK" request. Nothing is saved; the key never appears in the answer.

type nodeCheckIn struct {
	Type    string `json:"type"`    // openai-compatible | anthropic-compatible
	APIType string `json:"apiType"` // chat | responses
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`  // "" = use the saved key of nodeId's first active account
	NodeID  string `json:"nodeId"`  // editing an endpoint
	ModelID string `json:"modelId"` // model for the request fallback
	Prefix  string `json:"prefix"`
}

type nodeCheckOut struct {
	OK         bool     `json:"ok"`
	Method     string   `json:"method"` // models | request
	Status     int      `json:"status,omitempty"`
	Ms         int64    `json:"ms"`
	Class      string   `json:"class,omitempty"`
	Message    string   `json:"message"`
	Models     int      `json:"models,omitempty"`
	FirstModel string   `json:"firstModel,omitempty"`
	Sample     []string `json:"sample,omitempty"`
	ModelFound *bool    `json:"modelFound,omitempty"` // the Model ID is in the list
	UsedModel  string   `json:"usedModel,omitempty"`
	KeySource  string   `json:"keySource,omitempty"` // typed | saved
	PrefixOK   *bool    `json:"prefixOk,omitempty"`
	PrefixMsg  string   `json:"prefixMessage,omitempty"`
}

func (s *Server) handlePrefixCheck(w http.ResponseWriter, r *http.Request) {
	p, err := checkPrefix(s.state.Load().Snap, r.URL.Query().Get("prefix"), r.URL.Query().Get("except"))
	if err != nil {
		adminJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	adminJSON(w, http.StatusOK, map[string]any{"ok": true, "prefix": p})
}

func (s *Server) handleNodeCheck(w http.ResponseWriter, r *http.Request) {
	var in nodeCheckIn
	if !decodeStrict(w, r, &in) {
		return
	}
	var out nodeCheckOut
	sn := s.state.Load().Snap
	if in.Prefix != "" {
		_, err := checkPrefix(sn, in.Prefix, in.NodeID)
		ok := err == nil
		out.PrefixOK = &ok
		if err != nil {
			out.PrefixMsg = err.Error()
		}
	}
	base, err := cleanURL(in.BaseURL, "base URL")
	if err != nil {
		out.Class, out.Message = "bad_request", err.Error()
		adminJSON(w, http.StatusOK, out)
		return
	}
	key := strings.TrimSpace(in.APIKey)
	out.KeySource = "typed"
	if key == "" && in.NodeID != "" {
		for _, c := range sn.Connections {
			if c.Provider == in.NodeID && c.Active && c.Secrets["apiKey"] != "" {
				key, out.KeySource = c.Secrets["apiKey"], "saved"
				break
			}
		}
	}
	if key == "" {
		out.Class, out.Message, out.KeySource = "auth", "Enter an API key to check (it is only used for this check)", ""
		adminJSON(w, http.StatusOK, out)
		return
	}
	anth := strings.HasPrefix(in.Type, "anthropic")
	base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, "/chat/completions"), "/messages"), "/responses")
	auth := func(h http.Header) {
		if anth {
			h.Set("x-api-key", key)
			h.Set("anthropic-version", "2023-06-01")
		} else {
			h.Set("Authorization", "Bearer "+key)
		}
		h.Set("User-Agent", "fastrouter/"+Version)
	}
	clean := func(m string) string { return clip(strings.TrimSpace(scrubKey(secret.Redact(m), key)), 300) }
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	start := time.Now()
	model := strings.TrimSpace(in.ModelID)

	// 1. models list
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	auth(req.Header)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		out.Ms, out.Class, out.Method = time.Since(start).Milliseconds(), "network", "models"
		out.Message = "Could not reach the endpoint: " + shortNetErr(err)
		adminJSON(w, http.StatusOK, out)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	out.Method, out.Status = "models", resp.StatusCode
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &list) == nil && len(list.Data) > 0 {
		out.Ms, out.OK, out.Models, out.FirstModel = time.Since(start).Milliseconds(), true, len(list.Data), list.Data[0].ID
		for i, m := range list.Data {
			if i < 8 {
				out.Sample = append(out.Sample, m.ID)
			}
		}
		out.Message = strconv.Itoa(len(list.Data)) + " models available"
		if model != "" {
			found := false
			for _, m := range list.Data {
				found = found || m.ID == model
			}
			out.ModelFound = &found
		}
		adminJSON(w, http.StatusOK, out)
		return
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		out.Ms = time.Since(start).Milliseconds()
		msg := upstreamMessage(body)
		out.Class = errClass(resp.StatusCode, msg)
		out.Message = checkMessage(out.Class, resp.StatusCode) + tail(clean(msg))
		adminJSON(w, http.StatusOK, out)
		return
	}

	// 2. no usable models list: one tiny request
	if model == "" {
		out.Ms, out.Class = time.Since(start).Milliseconds(), "model"
		out.Message = "No models list here (HTTP " + strconv.Itoa(resp.StatusCode) + "). Enter a Model ID to check with a tiny request instead."
		adminJSON(w, http.StatusOK, out)
		return
	}
	var u string
	var payload map[string]any
	switch {
	case anth:
		u, payload = base+"/messages", map[string]any{"model": model, "max_tokens": 5, "messages": []any{map[string]any{"role": "user", "content": "Say OK"}}}
	case in.APIType == "responses":
		u, payload = base+"/responses", map[string]any{"model": model, "max_output_tokens": 16, "input": "Say OK"}
	default:
		u, payload = base+"/chat/completions", map[string]any{"model": model, "max_tokens": 5, "messages": []any{map[string]any{"role": "user", "content": "Say OK"}}}
	}
	b, _ := json.Marshal(payload)
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	auth(req.Header)
	req.Header.Set("Content-Type", "application/json")
	start = time.Now()
	resp, err = s.client.Do(req)
	out.Method, out.UsedModel = "request", model
	if err != nil {
		out.Ms, out.Class, out.Message = time.Since(start).Milliseconds(), "network", "Could not reach the endpoint: "+shortNetErr(err)
		adminJSON(w, http.StatusOK, out)
		return
	}
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	out.Ms, out.Status = time.Since(start).Milliseconds(), resp.StatusCode
	if resp.StatusCode < 300 {
		out.OK, out.Message = true, "Answered a tiny request with "+model
	} else {
		msg := upstreamMessage(body)
		out.Class = errClass(resp.StatusCode, msg)
		out.Message = checkMessage(out.Class, resp.StatusCode) + tail(clean(msg))
	}
	adminJSON(w, http.StatusOK, out)
}

func tail(m string) string {
	if m == "" {
		return ""
	}
	return ": " + m
}

func checkMessage(class string, status int) string {
	if class == "auth" {
		return "API key rejected (HTTP " + strconv.Itoa(status) + ")"
	}
	return classMessage(class, status)
}
