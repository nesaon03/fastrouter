package server

// Non-chat OpenAI-compatible routes: embeddings, image generation, speech to
// text and text to speech. They share the routing rules of the chat proxy
// (prefix/model resolution, account order, cooldowns, per-account model gaps,
// usage records) but pass the body through instead of translating it: these
// APIs have no cross-vendor IR, and inventing one would silently change what a
// caller gets. A provider therefore needs an OpenAI-compatible base URL, which
// is derived from its chat or responses endpoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/store"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

const (
	nonChatMaxBody = 64 << 20
	nonChatTimeout = 120 * time.Second
)

// nonChatPaths maps a route kind to the path appended to the provider base.
var nonChatPaths = map[string]string{
	"embeddings":     "/embeddings",
	"images":         "/images/generations",
	"transcriptions": "/audio/transcriptions",
	"speech":         "/audio/speech",
}

// handleOpenAIPass serves one non-chat route.
func (s *Server) handleOpenAIPass(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		raw, err := io.ReadAll(io.LimitReader(r.Body, nonChatMaxBody))
		if err != nil {
			writeError(w, tr.OpenAI, http.StatusBadRequest, "invalid_request_error", "could not read request: "+err.Error())
			return
		}
		ct := firstNonEmpty(r.Header.Get("Content-Type"), "application/json")
		model, err := nonChatModel(kind, raw, ct)
		if err != nil {
			writeError(w, tr.OpenAI, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		st := s.state.Load()
		key := keyFrom(r.Context())
		if !key.Allows(model) {
			writeError(w, tr.OpenAI, http.StatusForbidden, "permission_error", "this API key may not use model "+model)
			return
		}
		if ok, wait := key.Take(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
			writeError(w, tr.OpenAI, http.StatusTooManyRequests, "rate_limit_error", "this API key's request rate limit was reached")
			return
		}
		targets, err := st.Resolve(model)
		if err != nil {
			writeError(w, tr.OpenAI, http.StatusNotFound, "model_not_found", err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), nonChatTimeout)
		defer cancel()
		var reasons []string
		var last *upstreamErr
		for _, t := range targets {
			base := openAIBase(t.Provider.URLs[tr.OpenAI])
			if base == "" {
				base = openAIBase(t.Provider.URLs[tr.Responses])
			}
			if base == "" {
				reasons = append(reasons, t.Label+": provider has no OpenAI-compatible endpoint")
				continue
			}
			body, bodyCT, err := nonChatBody(raw, ct, t.Model)
			if err != nil {
				writeError(w, tr.OpenAI, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			url := base + nonChatPaths[kind]
			for _, a := range t.Provider.Pool.Order(time.Now()) {
				if s.blocks.blocked(a.ID, t.Model, time.Now()) {
					reasons = append(reasons, t.Label+": account "+a.Name+" does not serve this model")
					continue
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					writeError(w, tr.OpenAI, http.StatusBadGateway, "api_error", "could not build the upstream request")
					return
				}
				req.Header.Set("Content-Type", bodyCT)
				req.Header.Set("Accept", "application/json")
				req.Header.Set("User-Agent", "fastrouter/"+Version)
				req.Header.Set("Authorization", "Bearer "+a.Key())
				resp, err := s.client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						a.Fail(s.cfgCooldown, 0)
						reasons = append(reasons, t.Label+": "+shortNetErr(err))
					}
					continue
				}
				out, _ := io.ReadAll(io.LimitReader(resp.Body, nonChatMaxBody))
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					a.Succeed()
					s.recordNonChat(r, start, t, a, resp.StatusCode, out)
					h := w.Header()
					h.Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"))
					h.Set("X-FastRouter-Provider", t.Provider.ID)
					h.Set("X-FastRouter-Model", t.Model)
					h.Set("X-FastRouter-Account", a.ID)
					w.WriteHeader(resp.StatusCode)
					_, _ = w.Write(out)
					return
				}
				if resp.StatusCode == http.StatusNotFound && modelUnsupported(resp.StatusCode, upstreamMessage(out)) {
					// A plan gap: skip this account for this model, not the whole
					// account for every model.
					s.blocks.block(a.ID, t.Model, time.Now())
					reasons = append(reasons, t.Label+": account "+a.Name+" does not serve this model")
					continue
				}
				last = &upstreamErr{status: resp.StatusCode, body: out, header: resp.Header, uf: tr.OpenAI}
				if !retryable(resp.StatusCode) {
					s.relayNonChat(w, resp, out)
					return
				}
				a.Fail(s.cfgCooldown, retryAfter(resp.Header))
				reasons = append(reasons, fmt.Sprintf("%s: HTTP %d", t.Label, resp.StatusCode))
			}
		}
		if last != nil {
			s.relayNonChat(w, &http.Response{StatusCode: last.status, Header: last.header}, last.body)
			return
		}
		msg := "no upstream could serve this request"
		if len(reasons) > 0 {
			msg += ": " + strings.Join(reasons, "; ")
		}
		writeError(w, tr.OpenAI, http.StatusBadGateway, "api_error", msg)
	}
}

func (s *Server) relayNonChat(w http.ResponseWriter, resp *http.Response, body []byte) {
	h := w.Header()
	ct := "application/json"
	if resp != nil && resp.Header != nil {
		ct = firstNonEmpty(resp.Header.Get("Content-Type"), ct)
	}
	h.Set("Content-Type", ct)
	status := http.StatusBadGateway
	if resp != nil && resp.StatusCode != 0 {
		status = resp.StatusCode
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// nonChatModel reads the model id from a JSON body or a multipart form.
func nonChatModel(_ string, raw []byte, contentType string) (string, error) {
	mt, params, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("could not read the multipart body: %w", err)
			}
			if part.FormName() == "model" {
				b, _ := io.ReadAll(io.LimitReader(part, 1024))
				return strings.TrimSpace(string(b)), nil
			}
		}
		return "", fmt.Errorf("model field is required")
	}
	var head struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", fmt.Errorf("request is not JSON: %w", err)
	}
	if strings.TrimSpace(head.Model) == "" {
		return "", fmt.Errorf("model is required")
	}
	return strings.TrimSpace(head.Model), nil
}

// nonChatBody rewrites the model id to the one this target serves, keeping the
// rest of the request byte-for-byte where the format allows it.
func nonChatBody(raw []byte, contentType, model string) ([]byte, string, error) {
	mt, params, _ := mime.ParseMediaType(contentType)
	if !strings.HasPrefix(mt, "multipart/") {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, "", fmt.Errorf("request is not JSON: %w", err)
		}
		m["model"] = model
		b, err := json.Marshal(m)
		if err != nil {
			return nil, "", err
		}
		return b, "application/json", nil
	}
	// Multipart: copy every part, replacing the model field. The boundary is
	// regenerated, so the caller's Content-Type is not reused.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	sawModel := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("could not read the multipart body: %w", err)
		}
		name := part.FormName()
		if name == "model" {
			if err := mw.WriteField("model", model); err != nil {
				return nil, "", err
			}
			sawModel = true
			continue
		}
		dst, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.Copy(dst, part); err != nil {
			return nil, "", err
		}
	}
	if !sawModel {
		_ = mw.WriteField("model", model)
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// openAIBase strips a known endpoint suffix, leaving the base URL these routes
// append their path to. Providers with only a native (Anthropic/Gemini)
// endpoint have no such base and are skipped.
func openAIBase(u string) string {
	u = strings.TrimRight(strings.SplitN(u, "?", 2)[0], "/")
	for _, suf := range []string{"/chat/completions", "/responses", "/embeddings", "/images/generations",
		"/audio/transcriptions", "/audio/speech"} {
		if strings.HasSuffix(u, suf) {
			return strings.TrimSuffix(u, suf)
		}
	}
	return ""
}

// recordNonChat stores one non-chat call, with token counts when the response
// reports them (embeddings and newer image APIs do; audio usually does not).
func (s *Server) recordNonChat(r *http.Request, start time.Time, t router.Target, a *router.Account, status int, body []byte) {
	in, out := 0, 0
	var env struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &env) == nil {
		in = firstNonZero(env.Usage.PromptTokens, env.Usage.InputTokens)
		out = firstNonZero(env.Usage.CompletionTokens, env.Usage.OutputTokens)
		if in == 0 && out == 0 && env.Usage.TotalTokens > 0 {
			in = env.Usage.TotalTokens
		}
	}
	keyID := ""
	if k := keyFrom(r.Context()); k != nil {
		keyID = k.ID
	}
	s.rec.record(usageItem{
		ev: store.UsageEvent{At: start, ConnectionID: a.ID, Provider: t.Provider.ID, KeyID: keyID, Model: t.Model,
			Status: status, Latency: time.Since(start), Input: int64(in), Output: int64(out)},
		model: t.Model, prefix: t.Provider.Prefix,
	})
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
