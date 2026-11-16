package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/store"
)

// Sign-in flows started from the dashboard (Codex and GLM only).

const flowTTL = 10 * time.Minute

type loginFlow struct {
	ID, Provider, ConnID string
	URL, Kind            string // Kind: "browser" (Codex redirect) or "device" (Z.ai)
	created              time.Time

	mu      sync.Mutex
	status  string // pending | done | error
	message string
	result  string // connection id once saved

	verifier, state, redirect string // Codex PKCE
}

func (f *loginFlow) set(status, msg, conn string) {
	f.mu.Lock()
	f.status, f.message, f.result = status, msg, conn
	f.mu.Unlock()
}

func (f *loginFlow) view() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"id": f.ID, "provider": f.Provider, "kind": f.Kind, "url": f.URL, "status": f.status,
		"message": f.message, "connectionId": f.result}
}

type logins struct {
	ep        login.Endpoints
	codexPort int
	client    *http.Client

	mu       sync.Mutex
	flows    map[string]*loginFlow
	listener *http.Server // Codex callback listener, while a Codex flow is pending
}

func newLogins(port int, client *http.Client) *logins {
	if port <= 0 {
		port = login.CodexDefaultPort
	}
	return &logins{ep: login.Default, codexPort: port, client: client, flows: map[string]*loginFlow{}}
}

func (l *logins) get(id string) *loginFlow {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.flows[id]
	if f != nil && time.Since(f.created) > flowTTL+time.Minute {
		delete(l.flows, id)
		return nil
	}
	return f
}

func (l *logins) add(f *loginFlow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, old := range l.flows {
		if time.Since(old.created) > flowTTL+time.Minute {
			delete(l.flows, id)
		}
	}
	l.flows[f.ID] = f
}

// requireDashboardHeader blocks cross-site form posts: a custom header forces
// a CORS preflight that FastRouter never answers.
func requireDashboardHeader(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-FastRouter-Dashboard") != "1" {
			http.Error(w, "missing X-FastRouter-Dashboard header", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// handleLogins lists Codex and GLM accounts and their sign-in state. No secrets.
func (s *Server) handleLogins(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID, Provider, Name, Status, ExpiresAt string
		Active, AutoRefresh, Expired          bool
	}
	out := []row{}
	now := time.Now()
	for _, c := range s.state.Load().Snap.Connections {
		if !login.Handles(c.Provider) {
			continue
		}
		exp, _ := c.Meta["expiresAt"].(string)
		out = append(out, row{ID: c.ID, Provider: c.Provider, Name: maskEmail(c.Name), Status: c.Status, ExpiresAt: exp,
			Active: c.Active, AutoRefresh: s.refresher.Eligible(c) && c.Secrets["refreshToken"] != "", Expired: login.Expired(c, now)})
	}
	writeJSON(w, map[string]any{"accounts": out, "refreshImported": s.refresher.RefreshImported})
}

// handleLoginStart starts a sign-in. Body: {"connectionId": "..."} to sign an
// existing account in again; empty to add a new account.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	prov := r.PathValue("provider")
	if !login.Handles(prov) {
		http.Error(w, "only codex and glm sign-ins are supported", http.StatusNotFound)
		return
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
	if in.ConnectionID != "" {
		found := false
		for _, c := range s.state.Load().Snap.Connections {
			found = found || (c.ID == in.ConnectionID && c.Provider == prov)
		}
		if !found {
			http.Error(w, "unknown "+prov+" account", http.StatusNotFound)
			return
		}
	}
	f := &loginFlow{ID: login.RandomID(12), Provider: prov, ConnID: in.ConnectionID, created: time.Now(), status: "pending"}
	switch prov {
	case login.Codex:
		f.Kind = "browser"
		var challenge string
		f.verifier, challenge = login.PKCE()
		f.state = login.RandomID(16)
		f.redirect = login.CodexRedirectURI(s.logins.codexPort)
		f.URL = login.CodexAuthURL(f.redirect, f.state, challenge)
		s.logins.add(f)
		if err := s.ensureCodexListener(); err != nil {
			f.message = fmt.Sprintf("port %d is busy (%v): after signing in, copy the address your browser lands on and paste it here", s.logins.codexPort, err)
		}
	case login.GLM:
		f.Kind = "device"
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		d, err := s.logins.ep.GLMStart(ctx, s.logins.client)
		cancel()
		if err != nil {
			http.Error(w, "could not start the Z.ai sign-in: "+err.Error(), http.StatusBadGateway)
			return
		}
		f.URL = d.URL
		s.logins.add(f)
		go s.pollGLM(f, d)
	}
	writeJSON(w, f.view())
}

func (s *Server) handleLoginFlow(w http.ResponseWriter, r *http.Request) {
	f := s.logins.get(r.PathValue("id"))
	if f == nil {
		http.Error(w, "unknown or expired sign-in", http.StatusNotFound)
		return
	}
	writeJSON(w, f.view())
}

// handleLoginPaste finishes a Codex sign-in from a pasted callback address,
// for when the local callback port could not be used.
func (s *Server) handleLoginPaste(w http.ResponseWriter, r *http.Request) {
	f := s.logins.get(r.PathValue("id"))
	if f == nil || f.Provider != login.Codex {
		http.Error(w, "unknown or expired Codex sign-in", http.StatusNotFound)
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "expected {\"url\": ...}", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil {
		http.Error(w, "not a URL", http.StatusBadRequest)
		return
	}
	if err := s.finishCodex(r.Context(), f, u.Query()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, f.view())
}

// ensureCodexListener serves http://localhost:PORT/auth/callback (the
// redirect registered for the Codex client) while Codex flows are pending.
func (s *Server) ensureCodexListener() error {
	l := s.logins
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listener != nil {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(l.codexPort)))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mux.HandleFunc("GET "+login.CodexCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var flow *loginFlow
		l.mu.Lock()
		for _, f := range l.flows {
			if f.Provider == login.Codex && f.state != "" && f.state == q.Get("state") {
				flow = f
			}
		}
		l.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if flow == nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<p>Unknown or expired FastRouter sign-in. Start again from the dashboard.</p>")
			return
		}
		if err := s.finishCodex(r.Context(), flow, q); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "<p>Sign-in failed: %s</p>", htmlEscape(err.Error()))
			return
		}
		fmt.Fprint(w, "<p>Signed in. You can close this tab and go back to the FastRouter dashboard.</p>")
	})
	l.listener = hs
	go func() { _ = hs.Serve(ln) }()
	go func() { // stop when no Codex flow is pending
		for {
			time.Sleep(5 * time.Second)
			l.mu.Lock()
			busy := false
			for _, f := range l.flows {
				f.mu.Lock()
				busy = busy || (f.Provider == login.Codex && f.status == "pending" && time.Since(f.created) < flowTTL)
				f.mu.Unlock()
			}
			if !busy {
				l.listener = nil
				l.mu.Unlock()
				_ = hs.Close()
				return
			}
			l.mu.Unlock()
		}
	}()
	return nil
}

func (s *Server) finishCodex(ctx context.Context, f *loginFlow, q url.Values) error {
	f.mu.Lock()
	pending := f.status == "pending"
	f.mu.Unlock()
	if !pending {
		return errors.New("this sign-in already finished")
	}
	if e := q.Get("error"); e != "" {
		f.set("error", "ChatGPT sign-in was refused: "+e, "")
		return errors.New("sign-in refused: " + e)
	}
	if q.Get("state") != f.state || q.Get("code") == "" {
		return errors.New("the address does not belong to this sign-in (state or code missing)")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t, err := s.logins.ep.CodexExchange(ctx, s.logins.client, q.Get("code"), f.verifier, f.redirect)
	if err != nil {
		f.set("error", "token exchange failed: "+err.Error(), "")
		return err
	}
	id, err := s.saveLogin(f, func(c *store.Connection) { login.ApplyCodexTokens(c, t, time.Now()) })
	if err != nil {
		f.set("error", err.Error(), "")
		return err
	}
	f.set("done", "Signed in", id)
	return nil
}

func (s *Server) pollGLM(f *loginFlow, d *login.GLMDevice) {
	deadline := time.Now().Add(flowTTL)
	if d.ExpiresAt.Before(deadline) {
		deadline = d.ExpiresAt
	}
	for time.Now().Before(deadline) {
		time.Sleep(d.Interval)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, err := s.logins.ep.GLMCheck(ctx, s.logins.client, d)
		cancel()
		if errors.Is(err, login.ErrPending) {
			continue
		}
		if err != nil {
			f.set("error", err.Error(), "")
			return
		}
		id, err := s.saveLogin(f, func(c *store.Connection) { login.ApplyGLM(c, res) })
		if err != nil {
			f.set("error", err.Error(), "")
			return
		}
		f.set("done", "Signed in", id)
		return
	}
	f.set("error", "the Z.ai sign-in expired; start again", "")
}

// saveLogin stores the signed-in account (updating f.ConnID, or adding a new
// one) and reloads the routing table.
func (s *Server) saveLogin(f *loginFlow, apply func(*store.Connection)) (string, error) {
	snap, err := s.st.Load()
	if err != nil {
		return "", err
	}
	c := store.Connection{ID: f.ConnID, Provider: f.Provider, AuthType: "oauth", Priority: 1, Active: true}
	if f.ConnID != "" {
		found := false
		for _, old := range snap.Connections {
			if old.ID == f.ConnID {
				c, found = old, true
			}
		}
		if !found {
			return "", errors.New("the account was removed during sign-in")
		}
	} else {
		c.ID = login.RandomID(16)
		for _, old := range snap.Connections {
			if old.Provider == f.Provider && old.Priority >= c.Priority {
				c.Priority = old.Priority + 1
			}
		}
	}
	apply(&c)
	c.Active = true
	c.Meta["origin"] = login.OriginFastRouter // its own session: safe to refresh
	if err := s.st.SaveConnection(c); err != nil {
		return "", err
	}
	if err := s.ReloadFromStore(); err != nil {
		return "", err
	}
	log.Printf("sign-in: %s account %s saved", f.Provider, c.ID)
	return c.ID, nil
}

// ReloadFromStore reloads the configuration from the database.
func (s *Server) ReloadFromStore() error {
	if s.st == nil {
		return errors.New("no database")
	}
	snap, err := s.st.Load()
	if err != nil {
		return err
	}
	s.Reload(snap)
	return nil
}

// RunTokenRefresh refreshes Codex tokens in the background until ctx ends.
func (s *Server) RunTokenRefresh(ctx context.Context, every time.Duration) {
	if s.st == nil {
		return
	}
	s.refresher.Run(ctx, every)
}

func htmlEscape(s string) string {
	r := []rune{}
	for _, c := range s {
		switch c {
		case '<':
			r = append(r, []rune("&lt;")...)
		case '>':
			r = append(r, []rune("&gt;")...)
		case '&':
			r = append(r, []rune("&amp;")...)
		default:
			r = append(r, c)
		}
	}
	return string(r)
}
