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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/oauth"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/saver"
	"github.com/zenpo/fastrouter/internal/secret"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

const maxBody = 64 << 20

var framePool = sync.Pool{New: func() any { return tr.NewFrameReader(nil) }}

// upstreamErr is a failed attempt that can still be reported to the client.
type upstreamErr struct {
	status int
	body   []byte
	header http.Header
	uf     tr.Format // format of body (M3)
}

// reqHead is the part of a client body read on every request. The raw fields
// only record whether the client set them.
type reqHead struct {
	Model           string          `json:"model"`
	Stream          bool            `json:"stream"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     json.RawMessage `json:"temperature"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort"`
	Thinking        json.RawMessage `json:"thinking"`
	Reasoning       json.RawMessage `json:"reasoning"`
	OutputConfig    json.RawMessage `json:"output_config"`
	StreamOptions   json.RawMessage `json:"stream_options"`
}

// proxyReq is one client request. The full parse into the shared form only
// happens when something needs it (translation, bypass, vision checks).
type proxyReq struct {
	r      *http.Request
	cf     tr.Format
	body   []byte
	model  string
	stream bool
	head   reqHead

	ir     *tr.Request
	irErr  error
	parsed bool

	injectedUsage bool // we asked an OpenAI upstream for a usage chunk the client did not request
}

func (p *proxyReq) parse() (*tr.Request, error) {
	if !p.parsed {
		p.parsed = true
		p.ir, p.irErr = tr.ParseRequest(p.cf, p.body, p.model, p.stream)
	}
	return p.ir, p.irErr
}

// clientSetThinking reports whether the client chose a thinking/reasoning setting itself.
func (p *proxyReq) clientSetThinking() bool {
	h := &p.head
	return len(h.ReasoningEffort) > 0 || len(h.Thinking) > 0 || len(h.Reasoning) > 0 || len(h.OutputConfig) > 0 ||
		(p.cf == tr.Gemini && bytes.Contains(p.body, []byte("hinking")))
}

func (s *Server) handleProxy(cf tr.Format) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r, maxBody)
		if errors.Is(err, errTooLarge) {
			writeError(w, cf, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is over the 64 MB limit")
			return
		}
		if err != nil {
			writeError(w, cf, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
		p := &proxyReq{r: r, cf: cf, body: body}
		if err := scanHead(body, &p.head); err != nil {
			writeError(w, cf, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		p.model, p.stream = p.head.Model, p.head.Stream
		if cf == tr.Gemini {
			var ok bool
			if p.model, p.stream, ok = geminiAction(r.PathValue("rest")); !ok {
				writeError(w, cf, http.StatusNotFound, "not_found", "expected /v1beta/models/{model}:generateContent or :streamGenerateContent")
				return
			}
		}
		if p.model == "" {
			writeError(w, cf, http.StatusBadRequest, "invalid_request_error", "request must name a model")
			return
		}
		key := keyFrom(r.Context())
		if !key.Allows(p.model) {
			writeError(w, cf, http.StatusForbidden, "permission_error", "this API key may not use model "+p.model)
			return
		}
		if ok, wait := key.Take(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
			writeError(w, cf, http.StatusTooManyRequests, "rate_limit_error", "this API key's request rate limit was reached")
			return
		}
		st := s.state.Load()
		if s.bypass(w, p, st) {
			return
		}
		if c, ok := st.Combo(p.model); ok && (c.Strategy == "smart" || c.Strategy == "fusion") {
			s.specialCombo(w, p, st, c)
			return
		}
		targets, err := st.Resolve(p.model)
		if err != nil {
			writeError(w, cf, http.StatusNotFound, "model_not_found", err.Error())
			return
		}
		targets = s.visionTargets(p, st, targets)
		s.route(w, p, targets)
	}
}

// route tries each target and account in order until one answers.
func (s *Server) route(w http.ResponseWriter, p *proxyReq, targets []router.Target) {
	var last *upstreamErr
	var reasons []string
	for ti, t := range targets {
		uf, url := t.Provider.Pick(p.cf)
		var ad *oauth.Adapter
		if t.Provider.Login == "oauth" {
			if ad = oauth.AdapterFor(t.Provider.ID); ad != nil && ad.Route != nil {
				var f string
				f, url = ad.Route(t.Model, p.stream, oauth.Account{})
				uf = oauthFormat(f)
			}
		}
		switch {
		case t.Provider.Plugin:
			reasons = append(reasons, t.Label+": needs the login plugin (not in core yet)")
			continue
		case url == "":
			reasons = append(reasons, t.Label+": provider has no usable endpoint")
			continue
		case t.Provider.Pool.Size() == 0:
			reasons = append(reasons, t.Label+": no active accounts")
			continue
		}
		upBody, err := s.upstreamBody(p, t, uf)
		if err != nil {
			writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", "could not read request: "+err.Error())
			return
		}
		if sv := s.saver.Load(); sv.Enabled() && !saver.OptedOut(p.r.Header) {
			var tag string
			if upBody, tag = sv.Apply(p.r.Context(), upBody, uf, t.Model); tag != "" {
				w.Header().Set(saver.TagHeader, tag)
			} else {
				w.Header().Del(saver.TagHeader)
			}
		}
		var codex *login.CodexRequest
		if t.Provider.Login == login.Codex && uf == tr.Responses {
			if codex, err = login.ShapeCodex(upBody, firstNonEmpty(p.r.Header.Get("session_id"), p.r.Header.Get("x-session-id"))); err != nil {
				writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", "could not build the Codex request: "+err.Error())
				return
			}
			upBody = codex.Body
		}
		gemModel := ""
		if uf == tr.Gemini && (ad == nil || ad.Route == nil) {
			var ok bool
			if gemModel, ok = geminiModelPath(t.Model); !ok {
				writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", "invalid model name for a Gemini upstream")
				return
			}
		}
		order := t.Provider.Pool.Order(time.Now())
		for i, acct := range order {
			u := url
			if acct.URLs[uf] != "" {
				u = acct.URLs[uf]
			}
			body := upBody
			if ad != nil {
				oa := oauth.Account{ID: acct.ID, Extra: acct.Extra}
				if ad.Route != nil {
					_, u = ad.Route(t.Model, p.stream, oa)
				}
				if ad.Body != nil {
					if body, err = ad.Body(upBody, t.Model, p.stream, oa); err != nil {
						writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", "could not build the "+t.Provider.ID+" request: "+err.Error())
						return
					}
				}
			}
			if gemModel != "" {
				if p.stream {
					u += "/" + gemModel + ":streamGenerateContent?alt=sse"
				} else {
					u += "/" + gemModel + ":generateContent"
				}
			}
			a := attempt{p: p, t: t, acct: acct, uf: uf, url: u, body: body, codex: codex, ad: ad,
				canRetry: i < len(order)-1 || anyUsable(targets[ti+1:])}
			live := s.inflightFor(t.Provider.ID)
			live.Add(1)
			done, uerr := s.attempt(w, &a)
			live.Add(-1)
			if done || p.r.Context().Err() != nil {
				return
			}
			if uerr != nil {
				last = uerr
				if !retryable(uerr.status) {
					s.writeUpstream(w, p.cf, uf, uerr)
					return
				}
			}
		}
	}
	if last != nil {
		s.writeUpstream(w, p.cf, last.uf, last) // M3: translate when the last upstream spoke another format
		return
	}
	msg := "no upstream could serve this model"
	if len(reasons) > 0 {
		msg += ": " + strings.Join(reasons, "; ")
	}
	writeError(w, p.cf, http.StatusBadGateway, "api_error", msg)
}

// oauthFormat maps an adapter's wire-format name to the translator format.
func oauthFormat(f string) tr.Format {
	switch f {
	case oauth.FmtAnthropic:
		return tr.Anthropic
	case oauth.FmtResponses:
		return tr.Responses
	case oauth.FmtGemini:
		return tr.Gemini
	}
	return tr.OpenAI
}

func anyUsable(ts []router.Target) bool {
	for _, t := range ts {
		if !t.Provider.Plugin && t.Provider.Pool.Size() > 0 {
			if _, u := t.Provider.Pick(tr.OpenAI); u != "" {
				return true
			}
		}
	}
	return false
}

// upstreamBody builds the body for target t in upstream format uf: the client
// bytes with the model swapped when the formats match, else a translation.
func (s *Server) upstreamBody(p *proxyReq, t router.Target, uf tr.Format) ([]byte, error) {
	mode := t.Provider.ThinkingMode
	// H4: signatures tagged by an earlier translated turn must be restored or
	// dropped, so such a body is re-encoded instead of passed through.
	if uf == p.cf && !tr.HasTaggedSignature(p.body) {
		b := p.body
		newModel, addKey, addRaw := "", "", ""
		if p.cf != tr.Gemini && t.Model != p.model {
			newModel = t.Model
		}
		if p.cf == tr.OpenAI && p.stream && len(p.head.StreamOptions) == 0 {
			addKey, addRaw = "stream_options", `{"include_usage":true}` // so usage can be tracked
			p.injectedUsage = true
		}
		if newModel != "" || addKey != "" {
			b = patchBody(b, newModel, addKey, addRaw) // one copy for both (M6)
		}
		if mode != "" && !p.clientSetThinking() {
			b = injectThinking(b, p.cf, mode, &p.head)
		}
		return b, nil
	}
	ir, err := p.parse()
	if err != nil {
		return nil, err
	}
	req := *ir
	req.Stream = p.stream
	if mode != "" && req.Thinking == nil {
		req.Thinking = modeThinking(mode)
	}
	prov := t.Provider
	return tr.EmitRequest(uf, &req, tr.EmitOptions{
		Model:            t.Model,
		KeepCacheControl: prov.CacheControl,
		ReasoningFields:  !prov.Official,
		MaxCompletion:    prov.ID == "openai",
	})
}

// attempt is one upstream call.
type attempt struct {
	p        *proxyReq
	t        router.Target
	acct     *router.Account
	uf       tr.Format
	url      string
	body     []byte
	codex    *login.CodexRequest // shaped Codex request (Codex targets only)
	ad       *oauth.Adapter      // OAuth provider adapter (nil otherwise)
	canRetry bool                // another account or target is left to fail over to
}

var (
	errFirstByte = errors.New("first-byte timeout")
	errIdle      = errors.New("idle timeout")
)

// attempt sends one upstream request. done=true means a response was written
// to the client. A failure before anything was written returns done=false so
// the caller can try the next account.
func (s *Server) attempt(w http.ResponseWriter, a *attempt) (bool, *upstreamErr) {
	p, acct, label := a.p, a.acct, a.t.Label
	start := time.Now()
	ctx, cancel := context.WithCancelCause(p.r.Context()) // client disconnect cancels upstream
	defer cancel(nil)
	firstTimer := time.AfterFunc(s.cfgFirstByte, func() { cancel(errFirstByte) })
	defer firstTimer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(a.body))
	if err != nil {
		return false, &upstreamErr{status: 0}
	}
	setUpstreamHeaders(req.Header, p, a.uf, acct.Key())
	if a.codex != nil {
		login.CodexHeaders(req.Header, acct.Key(), acct.AccountID, a.codex)
	}
	if a.ad != nil && a.ad.Headers != nil {
		a.ad.Headers(req.Header, acct.Key(), p.stream, oauth.Account{ID: acct.ID, Extra: acct.Extra})
	}

	var usage tr.Usage
	rec := func(status int) { s.record(a, start, status, usage) }

	resp, err := s.client.Do(req)
	if err != nil {
		if p.r.Context().Err() == nil {
			acct.Fail(s.cfgCooldown, 0)
			log.Printf("upstream %s (account %s) failed before first byte: %s", label, acct.ID, secret.Redact(errString(err, context.Cause(ctx))))
		}
		rec(0)
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 { // H3: a redirect is an upstream fault
		rec(resp.StatusCode)
		acct.Fail(s.cfgCooldown, 0)
		log.Printf("upstream %s (account %s) answered with a redirect (%d); not followed", label, acct.ID, resp.StatusCode)
		return false, &upstreamErr{status: http.StatusBadGateway, body: []byte(fmt.Sprintf("upstream redirected (HTTP %d); redirects are not followed", resp.StatusCode))}
	}
	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		rec(resp.StatusCode)
		if retryable(resp.StatusCode) {
			acct.Fail(s.cfgCooldown, retryAfter(resp.Header))
			log.Printf("upstream %s (account %s) returned %d; trying next", label, acct.ID, resp.StatusCode)
		}
		return false, &upstreamErr{status: resp.StatusCode, body: eb, header: resp.Header, uf: a.uf}
	}
	firstTimer.Stop()
	if a.ad != nil && a.ad.Response != nil {
		resp.Body = a.ad.Response(resp.Body)
	}
	idle := time.AfterFunc(s.cfgIdle, func() { cancel(errIdle) })
	defer idle.Stop()

	if !isEventStream(resp.Header.Get("Content-Type")) {
		return s.finishJSON(w, a, resp, idle, &usage, rec, ctx)
	}
	return s.finishStream(w, a, resp, idle, &usage, rec, ctx, start)
}

// finishJSON handles a complete (non-SSE) upstream answer.
func (s *Server) finishJSON(w http.ResponseWriter, a *attempt, resp *http.Response, idle *time.Timer, usage *tr.Usage,
	rec func(int), ctx context.Context) (bool, *upstreamErr) {
	p, acct := a.p, a.acct
	raw, err := io.ReadAll(&idleReader{r: io.LimitReader(resp.Body, maxBody), t: idle, d: s.cfgIdle})
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		if p.r.Context().Err() == nil {
			acct.Fail(s.cfgCooldown, 0)
			log.Printf("upstream %s (account %s) sent no usable body: %s", a.t.Label, acct.ID, secret.Redact(errString(err, context.Cause(ctx))))
		}
		rec(0)
		return false, nil
	}
	parsed, perr := tr.ParseResponse(a.uf, raw)
	if perr == nil {
		if a.uf != p.cf {
			tr.TagResponse(parsed, a.uf) // H4
		}
		*usage = parsed.Usage
		if !parsed.HasContent() && parsed.Finish != tr.FinishContentFilter && a.canRetry {
			log.Printf("upstream %s (account %s) returned an empty response; trying next", a.t.Label, acct.ID)
			rec(resp.StatusCode)
			return false, nil
		}
	}
	acct.Succeed()
	var out []byte
	ct := "application/json"
	switch {
	case perr != nil && a.uf != p.cf:
		rec(resp.StatusCode)
		return false, &upstreamErr{status: http.StatusBadGateway, body: []byte("upstream answer could not be translated: " + perr.Error())}
	case p.stream && perr == nil: // client wanted a stream; replay the answer as one
		enc := tr.NewEncoder(p.cf, a.t.Label)
		out = append(enc.Encode(tr.ResponseEvents(parsed)), enc.Close()...)
		ct = "text/event-stream"
	case a.uf == p.cf:
		out, ct = raw, firstNonEmpty(resp.Header.Get("Content-Type"), ct)
	default:
		if out, err = tr.EmitResponse(p.cf, parsed, a.t.Label); err != nil {
			rec(resp.StatusCode)
			return false, &upstreamErr{status: http.StatusBadGateway, body: []byte(err.Error())}
		}
	}
	setResponseHeaders(w.Header(), resp, a, ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	rec(resp.StatusCode)
	return true, nil
}

// finishStream relays an SSE answer: frame by frame, translated when the
// formats differ. Output is held back until the first real content (or the
// EmptyResponseWait deadline) so an empty answer can still fail over.
func (s *Server) finishStream(w http.ResponseWriter, a *attempt, resp *http.Response, idle *time.Timer, usage *tr.Usage,
	rec func(int), ctx context.Context, start time.Time) (bool, *upstreamErr) {
	p, acct := a.p, a.acct
	passthrough := a.uf == p.cf && p.stream
	dec := tr.NewDecoder(a.uf)
	var enc tr.Encoder
	var coll *tr.Collector
	switch {
	case !p.stream:
		coll = &tr.Collector{} // client wanted JSON; fold the stream
	case !passthrough:
		enc = tr.NewEncoder(p.cf, a.t.Label)
	}
	fr := framePool.Get().(*tr.FrameReader)
	fr.Reset(resp.Body)
	defer func() { fr.Reset(nil); framePool.Put(fr) }()
	flusher, _ := w.(http.Flusher)
	holdUntil := start.Add(s.cfgEmptyWait)

	committed, sawContent, frames := false, false, 0
	finish := tr.FinishNone
	var pending []byte
	var upErr string
	commit := func() {
		committed = true
		ct := "text/event-stream"
		if passthrough {
			ct = firstNonEmpty(resp.Header.Get("Content-Type"), ct)
		}
		h := w.Header()
		setResponseHeaders(h, resp, a, ct)
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
	}
	track := func(evs []tr.Event) {
		for i := range evs {
			switch e := &evs[i]; e.Kind {
			case tr.EvUsage:
				*usage = e.Usage
			case tr.EvFinish:
				finish = e.Finish
			case tr.EvError:
				upErr = e.Text
			default:
				if e.IsContent() {
					sawContent = true
				}
			}
		}
	}

	var rerr error
	clientGone := false
	for {
		f, err := fr.Next()
		if err != nil {
			rerr = err
			break
		}
		frames++
		idle.Reset(s.cfgIdle)
		var evs []tr.Event
		if !passthrough || !committed || bytes.Contains(f.Data, []byte("sage")) {
			evs, _ = dec.Decode(f) // undecodable frames are passed through or skipped
			if a.uf != p.cf {
				tr.TagEvents(evs, a.uf) // H4: mark which family issued each signature
			}
			track(evs)
		}
		if upErr != "" && !committed && !sawContent {
			acct.Fail(s.cfgCooldown, 0)
			log.Printf("upstream %s (account %s) sent an error before any content: %s", a.t.Label, acct.ID, secret.Redact(upErr))
			rec(http.StatusBadGateway)
			return false, &upstreamErr{status: http.StatusBadGateway, body: []byte(upErr)}
		}
		var out []byte
		switch {
		case coll != nil:
			coll.Add(evs)
			continue
		case passthrough:
			if p.injectedUsage && bytes.Contains(f.Data, []byte(`"choices":[]`)) {
				continue // usage-only chunk the client never asked for
			}
			out = f.Raw
		default:
			out = enc.Encode(evs)
		}
		if !committed {
			pending = append(pending, out...)
			if sawContent || time.Now().After(holdUntil) {
				commit()
				if _, werr := w.Write(pending); werr != nil {
					clientGone = true
					break
				}
				pending = nil
				if flusher != nil {
					flusher.Flush()
				}
			}
			continue
		}
		if len(out) > 0 {
			if _, werr := w.Write(out); werr != nil {
				clientGone = true
				break // deferred cancel stops the upstream
			}
			if flusher != nil && fr.Buffered() == 0 {
				flusher.Flush()
			}
		}
	}
	if clientGone {
		rec(resp.StatusCode)
		return true, nil
	}
	tail := dec.End()
	if a.uf != p.cf {
		tr.TagEvents(tail, a.uf)
	}
	track(tail)
	clientCanceled := p.r.Context().Err() != nil
	// H1: a cut-off stream or a mid-stream upstream error is not a clean finish.
	broken := ""
	if !clientCanceled {
		if upErr != "" {
			broken = "upstream error: " + secret.Redact(upErr)
		} else if rerr != io.EOF {
			broken = "upstream stream ended early: " + secret.Redact(errString(rerr, context.Cause(ctx)))
		}
	}

	if !committed {
		if !sawContent && finish != tr.FinishContentFilter && a.canRetry && !clientCanceled {
			if frames == 0 || rerr != io.EOF {
				acct.Fail(s.cfgCooldown, 0)
			}
			log.Printf("upstream %s (account %s) returned an empty stream; trying next", a.t.Label, acct.ID)
			rec(resp.StatusCode)
			return false, nil
		}
		if frames == 0 && rerr != io.EOF && !clientCanceled { // M1: a client hang-up is not the account's fault
			acct.Fail(s.cfgCooldown, 0)
			rec(0)
			return false, nil
		}
		if coll != nil && broken != "" { // JSON client: nothing written yet, so fail over or report it
			acct.Fail(s.cfgCooldown, 0)
			log.Printf("upstream %s (account %s): %s", a.t.Label, acct.ID, broken)
			rec(http.StatusBadGateway)
			return false, &upstreamErr{status: http.StatusBadGateway, body: []byte(broken)}
		}
	}
	if clientCanceled {
		rec(resp.StatusCode)
		return true, nil
	}
	if broken != "" {
		acct.Fail(s.cfgCooldown, 0)
		log.Printf("upstream %s (account %s): %s", a.t.Label, acct.ID, broken)
		switch {
		case enc != nil:
			pending = append(pending, enc.Encode(tail)...)
			pending = append(pending, enc.Fail(broken)...)
		case passthrough && upErr == "": // an upstream error frame was already relayed as-is
			pending = append(pending, tr.NewEncoder(p.cf, a.t.Label).Fail(broken)...)
		}
		if !committed {
			commit()
		}
		_, _ = w.Write(pending)
		if flusher != nil {
			flusher.Flush()
		}
		rec(http.StatusBadGateway)
		return true, nil
	}
	acct.Succeed()
	if coll != nil {
		coll.Add(tail)
		out, err := tr.EmitResponse(p.cf, coll.Response(), a.t.Label)
		if err != nil {
			rec(resp.StatusCode)
			return false, &upstreamErr{status: http.StatusBadGateway, body: []byte(err.Error())}
		}
		setResponseHeaders(w.Header(), resp, a, "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
		rec(resp.StatusCode)
		return true, nil
	}
	if enc != nil {
		pending = append(pending, enc.Encode(tail)...)
		pending = append(pending, enc.Close()...)
	}
	if !committed {
		commit()
	}
	if len(pending) > 0 {
		_, _ = w.Write(pending)
	}
	if flusher != nil {
		flusher.Flush()
	}
	rec(resp.StatusCode)
	return true, nil
}

// idleReader resets the idle timer on every successful read.
type idleReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (ir *idleReader) Read(b []byte) (int, error) {
	n, err := ir.r.Read(b)
	if n > 0 {
		ir.t.Reset(ir.d)
	}
	return n, err
}

func setUpstreamHeaders(h http.Header, p *proxyReq, uf tr.Format, key string) {
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "fastrouter/"+Version)
	if p.stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	switch uf {
	case tr.Anthropic:
		h.Set("x-api-key", key)
		ver := "2023-06-01"
		if p.cf == tr.Anthropic {
			ver = firstNonEmpty(p.r.Header.Get("anthropic-version"), ver)
			if b := p.r.Header.Get("anthropic-beta"); b != "" {
				h.Set("anthropic-beta", b)
			}
		}
		h.Set("anthropic-version", ver)
	case tr.Gemini:
		h.Set("x-goog-api-key", key)
	default:
		h.Set("Authorization", "Bearer "+key)
	}
}

func setResponseHeaders(h http.Header, resp *http.Response, a *attempt, ct string) {
	h.Set("Content-Type", ct)
	for _, k := range []string{"X-Request-Id", "Openai-Processing-Ms", "Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("X-FastRouter-Account", a.acct.ID)
	h.Set("X-FastRouter-Model", a.t.Label)
	if a.uf != a.p.cf {
		h.Set("X-FastRouter-Translated", a.uf.String()+"->"+a.p.cf.String())
	}
}

// geminiAction splits "model:generateContent" from the /v1beta path.
func geminiAction(rest string) (model string, stream, ok bool) {
	i := strings.LastIndexByte(rest, ':')
	if i <= 0 {
		return "", false, false
	}
	model = strings.TrimPrefix(rest[:i], "models/")
	switch rest[i+1:] {
	case "generateContent":
		return model, false, true
	case "streamGenerateContent":
		return model, true, true
	}
	return "", false, false
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

// writeUpstream reports an upstream failure: verbatim when the client speaks
// the upstream's format, else as the client format's error shape.
func (s *Server) writeUpstream(w http.ResponseWriter, cf, uf tr.Format, e *upstreamErr) {
	status := e.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	if uf == cf && e.header != nil && json.Valid(e.body) {
		if ct := e.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(status)
		_, _ = w.Write(e.body)
		return
	}
	writeError(w, cf, status, errType(status), upstreamMessage(e.body))
}

// upstreamMessage pulls a readable message out of an upstream error body.
func upstreamMessage(b []byte) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(b, &v) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var str string
		if json.Unmarshal(v.Error, &str) == nil && str != "" {
			return str
		}
		if v.Message != "" {
			return v.Message
		}
	}
	msg := strings.TrimSpace(string(b))
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return firstNonEmpty(msg, "upstream error")
}

func errType(status int) string {
	switch status {
	case 400, 404, 413, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

var geminiStatus = map[int]string{400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND",
	429: "RESOURCE_EXHAUSTED", 500: "INTERNAL", 502: "UNAVAILABLE", 503: "UNAVAILABLE", 504: "DEADLINE_EXCEEDED"}

func writeError(w http.ResponseWriter, f tr.Format, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var v any
	switch f {
	case tr.Anthropic:
		v = map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}}
	case tr.Gemini:
		v = map[string]any{"error": map[string]any{"code": status, "message": msg, "status": firstNonEmpty(geminiStatus[status], "UNKNOWN")}}
	default:
		v = map[string]any{"error": map[string]string{"type": typ, "message": msg, "code": typ}}
	}
	_ = json.NewEncoder(w).Encode(v)
}

func errString(err, cause error) string {
	if cause != nil && !errors.Is(cause, context.Canceled) {
		return cause.Error()
	}
	if err == nil || err == io.EOF {
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

var geminiModelRe = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// geminiModelPath validates a client-supplied model id before it goes into the
// Gemini URL path (M10): no "/", "?", "#" or ".." can reach the path or query.
func geminiModelPath(m string) (string, bool) {
	m = strings.TrimPrefix(m, "models/")
	if !geminiModelRe.MatchString(m) || strings.Contains(m, "..") {
		return "", false
	}
	return url.PathEscape(m), true
}
