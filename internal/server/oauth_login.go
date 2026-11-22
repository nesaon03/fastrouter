package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nesaon03/fastrouter/internal/oauth"
	"github.com/nesaon03/fastrouter/internal/store"
)

// callbackURI is FastRouter's own /callback on localhost (what 9Router
// registers too). From another machine the user pastes the address instead.
func callbackURI(r *http.Request) string {
	port := "20128"
	if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "" {
		port = p
	}
	return "http://localhost:" + port + "/callback"
}

func (s *Server) oauthEnv(redirect string) oauth.Env {
	return oauth.Env{Client: s.logins.client, RedirectURI: redirect}
}

func (s *Server) startOAuth(r *http.Request, f *loginFlow, spec *oauth.Spec) error {
	f.spec, f.Kind = spec, spec.Kind
	if spec.Kind == oauth.KindPaste {
		f.hint = "Paste the credential below."
		s.logins.add(f)
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	p, err := spec.Start(ctx, s.oauthEnv(callbackURI(r)))
	if err != nil {
		return err
	}
	f.pend, f.URL, f.userCode, f.hint, f.state = p, p.URL, p.UserCode, p.Hint, p.State
	s.logins.add(f)
	if spec.Kind == oauth.KindDevice {
		go s.pollOAuth(f)
	}
	return nil
}

func (s *Server) pollOAuth(f *loginFlow) {
	p := f.pend
	deadline := time.Now().Add(flowTTL)
	if !p.Expires.IsZero() && p.Expires.Before(deadline) {
		deadline = p.Expires
	}
	for time.Now().Before(deadline) {
		time.Sleep(p.Interval)
		f.mu.Lock()
		st := f.status
		f.mu.Unlock()
		if st != "pending" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		res, err := f.spec.Poll(ctx, s.oauthEnv(""), p)
		cancel()
		if errors.Is(err, oauth.ErrPending) {
			continue
		}
		s.finishOAuth(f, res, err)
		return
	}
	f.set("error", "the sign-in expired; start again", "")
}

func (s *Server) finishOAuth(f *loginFlow, res *oauth.Result, err error) error {
	if err != nil {
		f.set("error", err.Error(), "") // flow errors carry status codes only, never tokens
		return err
	}
	id, err := s.saveLogin(f, func(c *store.Connection) {
		if f.ConnID == "" {
			c.Name = ""
		}
		oauth.Apply(c, res, time.Now())
	})
	if err != nil {
		f.set("error", err.Error(), "")
		return err
	}
	f.set("done", "Signed in", id)
	return nil
}

func (s *Server) exchangeOAuth(ctx context.Context, f *loginFlow, q url.Values) error {
	f.mu.Lock()
	pending := f.status == "pending"
	f.mu.Unlock()
	if !pending {
		return errors.New("this sign-in already finished")
	}
	if e := q.Get("error"); e != "" {
		f.set("error", "sign-in was refused: "+clip(e, 80), "")
		return errors.New("sign-in refused")
	}
	if q.Get("code") == "" {
		return errors.New("no code in the address")
	}
	if st := q.Get("state"); st != "" && st != f.state && !strings.Contains(q.Get("code"), "#") {
		return errors.New("the address does not belong to this sign-in (state mismatch)")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	res, err := f.spec.Exchange(ctx, s.oauthEnv(f.pend.RedirectURI()), f.pend, q)
	return s.finishOAuth(f, res, err)
}

// pasteOAuth finishes a browser flow from a pasted callback address or code,
// or a paste flow from the pasted credential.
func (s *Server) pasteOAuth(ctx context.Context, f *loginFlow, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("nothing pasted")
	}
	switch f.spec.Kind {
	case oauth.KindPaste:
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		res, err := f.spec.Paste(cctx, s.oauthEnv(""), text)
		return s.finishOAuth(f, res, err)
	case oauth.KindBrowser:
		q := url.Values{}
		if u, err := url.Parse(text); err == nil && u.RawQuery != "" {
			q = u.Query()
		} else {
			q.Set("code", text) // a bare code (Claude shows "code#state")
		}
		return s.exchangeOAuth(ctx, f, q)
	}
	return errors.New("this sign-in finishes on its own once you confirm the code")
}

// handleOAuthCallback is the redirect target of browser sign-ins.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var flow *loginFlow
	if st := q.Get("state"); st != "" {
		s.logins.mu.Lock()
		for _, f := range s.logins.flows {
			if f.spec != nil && f.state == st && time.Since(f.created) < flowTTL {
				flow = f
			}
		}
		s.logins.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if flow == nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, callbackPage("Unknown or expired FastRouter sign-in. Start again from the dashboard."))
		return
	}
	if err := s.exchangeOAuth(r.Context(), flow, q); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, callbackPage("Sign-in failed: "+htmlEscape(err.Error())))
		return
	}
	fmt.Fprint(w, callbackPage("Signed in. You can close this tab and go back to the FastRouter dashboard."))
}

func callbackPage(msg string) string {
	return `<!doctype html><meta charset="utf-8"><title>FastRouter sign-in</title>` +
		`<body style="font:15px system-ui;display:grid;place-items:center;height:90vh;color:#222"><p>` + msg +
		`</p><script>try{window.opener&&window.opener.postMessage({type:"fastrouter-oauth"},location.origin)}catch(e){}</script></body>`
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
