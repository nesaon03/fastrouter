package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/secret"
	"github.com/zenpo/fastrouter/internal/store"
)

type apiFormat int

const (
	formatOpenAI apiFormat = iota
	formatAnthropic
)

const maxBody = 64 << 20

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// upstreamErr is a failed attempt that can still be reported to the client.
type upstreamErr struct {
	status int
	body   []byte
	header http.Header
}

func (s *Server) handleProxy(f apiFormat) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeError(w, f, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
		var head struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(body, &head); err != nil || head.Model == "" {
			writeError(w, f, http.StatusBadRequest, "invalid_request_error", "request must be JSON with a \"model\" field")
			return
		}
		st := s.state.Load()
		targets, err := st.Resolve(head.Model)
		if err != nil {
			writeError(w, f, http.StatusNotFound, "model_not_found", err.Error())
			return
		}

		var last *upstreamErr
		var reasons []string
		for _, t := range targets {
			url := t.Provider.ChatURL
			if f == formatAnthropic {
				url = t.Provider.MessagesURL
			}
			switch {
			case t.Provider.Plugin:
				reasons = append(reasons, t.Label+": needs the login plugin (not in core yet)")
				continue
			case url == "":
				reasons = append(reasons, t.Label+": provider does not speak this API format (translation is Phase 2)")
				continue
			case t.Provider.Pool.Size() == 0:
				reasons = append(reasons, t.Label+": no active accounts")
				continue
			}
			upBody := body
			if t.Model != head.Model {
				upBody, _ = replaceTopLevelString(body, "model", t.Model)
			}
			for _, acct := range t.Provider.Pool.Order(time.Now()) {
				u := url
				if acct.ChatURL != "" && f == formatOpenAI {
					u = acct.ChatURL
				}
				done, uerr := s.attempt(w, r, f, acct, u, t.Label, upBody)
				if done {
					return
				}
				if r.Context().Err() != nil {
					return // client went away; stop trying
				}
				if uerr != nil {
					last = uerr
					if !retryable(uerr.status) {
						writeUpstream(w, uerr)
						return
					}
				}
			}
		}
		if last != nil {
			writeUpstream(w, last)
			return
		}
		msg := "no upstream could serve this model"
		if len(reasons) > 0 {
			msg += ": " + fmt.Sprint(reasons)
		}
		writeError(w, f, http.StatusBadGateway, "api_error", msg)
	}
}

// attempt sends one upstream request. done=true means a response (success or
// a non-retryable passthrough) was written to the client. A failure before the
// first byte returns done=false so the caller can try the next account.
func (s *Server) attempt(w http.ResponseWriter, r *http.Request, f apiFormat, acct *router.Account, url, label string, body []byte) (bool, *upstreamErr) {
	start := time.Now()
	ctx, cancel := context.WithCancelCause(r.Context()) // client disconnect cancels upstream
	defer cancel(nil)
	errFirstByte := errors.New("first-byte timeout")
	errIdle := errors.New("idle timeout")
	firstTimer := time.AfterFunc(s.cfgFirstByte, func() { cancel(errFirstByte) })
	defer firstTimer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, &upstreamErr{status: 0}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fastrouter/"+Version)
	if f == formatAnthropic {
		req.Header.Set("x-api-key", acct.Key())
		req.Header.Set("anthropic-version", firstNonEmpty(r.Header.Get("anthropic-version"), "2023-06-01"))
		if b := r.Header.Get("anthropic-beta"); b != "" {
			req.Header.Set("anthropic-beta", b)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+acct.Key())
	}
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}

	rec := func(status int) {
		s.rec.record(store.UsageEvent{At: start, ConnectionID: acct.ID, Model: label, Status: status, Latency: time.Since(start)})
	}

	resp, err := s.client.Do(req)
	if err != nil {
		cause := context.Cause(ctx)
		if r.Context().Err() == nil {
			acct.Fail(s.cfgCooldown, 0)
			log.Printf("upstream %s (account %s) failed before first byte: %s", label, acct.ID, secret.Redact(errString(err, cause)))
		}
		rec(0)
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		rec(resp.StatusCode)
		if retryable(resp.StatusCode) {
			acct.Fail(s.cfgCooldown, retryAfter(resp.Header))
			log.Printf("upstream %s (account %s) returned %d; trying next", label, acct.ID, resp.StatusCode)
		}
		return false, &upstreamErr{status: resp.StatusCode, body: eb, header: resp.Header}
	}

	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp

	// Wait for the first byte of the body before committing to this upstream.
	n, rerr := resp.Body.Read(buf)
	for n == 0 && rerr == nil {
		n, rerr = resp.Body.Read(buf)
	}
	if n == 0 {
		if r.Context().Err() == nil {
			acct.Fail(s.cfgCooldown, 0)
			log.Printf("upstream %s (account %s) sent no body: %s", label, acct.ID, secret.Redact(errString(rerr, context.Cause(ctx))))
		}
		rec(0)
		return false, nil
	}
	firstTimer.Stop()
	acct.Succeed()

	h := w.Header()
	for _, k := range []string{"Content-Type", "X-Request-Id", "Openai-Processing-Ms", "Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("X-FastRouter-Account", acct.ID)
	h.Set("X-FastRouter-Model", label)
	stream := isEventStream(resp.Header.Get("Content-Type"))
	if stream {
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	idle := time.AfterFunc(s.cfgIdle, func() { cancel(errIdle) })
	defer idle.Stop()
	for {
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break // client gone; deferred cancel stops the upstream
			}
			if stream && flusher != nil {
				flusher.Flush()
			}
			idle.Reset(s.cfgIdle)
		}
		if rerr != nil {
			if rerr != io.EOF && r.Context().Err() == nil {
				log.Printf("upstream %s stream ended early: %s", label, secret.Redact(errString(rerr, context.Cause(ctx))))
			}
			break
		}
		n, rerr = resp.Body.Read(buf)
	}
	rec(resp.StatusCode)
	return true, nil
}

func retryable(status int) bool {
	switch {
	case status == 0, status == 401, status == 402, status == 403, status == 404, status == 408, status == 429:
		return true
	case status >= 500:
		return true
	}
	return false
}

func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

func isEventStream(ct string) bool {
	return len(ct) >= 17 && ct[:17] == "text/event-stream"
}

func writeUpstream(w http.ResponseWriter, e *upstreamErr) {
	if ct := e.header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

func writeError(w http.ResponseWriter, f apiFormat, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var v any
	if f == formatAnthropic {
		v = map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}}
	} else {
		v = map[string]any{"error": map[string]string{"type": typ, "message": msg, "code": typ}}
	}
	_ = json.NewEncoder(w).Encode(v)
}

func errString(err, cause error) string {
	if cause != nil && !errors.Is(cause, context.Canceled) {
		return cause.Error()
	}
	if err == nil {
		return "empty response"
	}
	return err.Error()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
