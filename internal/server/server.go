// Package server is FastRouter's HTTP front end.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/dashboard"
	"github.com/zenpo/fastrouter/internal/pricing"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/saver"
	"github.com/zenpo/fastrouter/internal/store"
	tr "github.com/zenpo/fastrouter/internal/translate"
	"golang.org/x/crypto/bcrypt"
)

// Version is set at build time with -ldflags "-X .../server.Version=..."
var Version = "0.2.0-phase2"

// Server routes client requests to upstreams.
type Server struct {
	state  atomic.Pointer[router.State]
	client *http.Client
	rec    *recorder
	start  time.Time

	cfgFirstByte, cfgIdle, cfgCooldown, cfgEmptyWait time.Duration
	keyLimits                                        map[string]config.KeyLimit
	st                                               *store.Store
	tsCfg                                            config.TokenSavers
	saver                                            atomic.Pointer[saver.Saver]

	pwOK sync.Map // sha256(user:pass) -> true, so bcrypt runs once per credential
}

// New builds a server from config and a loaded snapshot. st may be nil (tests).
func New(cfg config.Config, snap *store.Snapshot, st *store.Store) *Server {
	s := &Server{
		client:       &http.Client{Transport: NewTransport(cfg)},
		rec:          newRecorder(st),
		start:        time.Now(),
		cfgFirstByte: time.Duration(cfg.FirstByteTimeout),
		cfgIdle:      time.Duration(cfg.IdleTimeout),
		cfgCooldown:  time.Duration(cfg.CooldownBase),
		cfgEmptyWait: time.Duration(cfg.EmptyResponseWait),
		keyLimits:    cfg.KeyLimits,
		st:           st,
		tsCfg:        cfg.TokenSavers,
	}
	s.Reload(snap)
	return s
}

// NewTransport is an http.Transport tuned for many long-lived streaming calls.
func NewTransport(cfg config.Config) *http.Transport {
	d := &net.Dialer{Timeout: time.Duration(cfg.ConnectTimeout), KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPer,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   time.Duration(cfg.ConnectTimeout),
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true, // pass bytes through untouched; SSE is not worth gzipping locally
		WriteBufferSize:       64 << 10,
		ReadBufferSize:        64 << 10,
	}
}

// Reload swaps in a new routing table atomically (in-flight requests keep the old one).
func (s *Server) Reload(snap *store.Snapshot) {
	s.state.Store(router.Build(snap, s.keyLimits))
	t, err := pricing.New(snap.Settings["backup.pricing"])
	if err != nil {
		log.Printf("imported pricing overrides ignored: %v", err)
		t, _ = pricing.New(nil)
	}
	s.rec.prices.Store(t)
	s.saver.Store(saver.New(snap.Settings, s.tsCfg))
}

// Saver returns the current token-saver settings.
func (s *Server) Saver() *saver.Saver { return s.saver.Load() }

// Close flushes background writes.
func (s *Server) Close() { s.rec.close() }

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", s.auth(tr.OpenAI, s.handleProxy(tr.OpenAI)))
	mux.Handle("POST /v1/messages", s.auth(tr.Anthropic, s.handleProxy(tr.Anthropic)))
	mux.Handle("POST /v1/responses", s.auth(tr.Responses, s.handleProxy(tr.Responses)))
	mux.Handle("POST /v1/messages/count_tokens", s.auth(tr.Anthropic, http.HandlerFunc(s.handleCountTokens)))
	mux.Handle("POST /v1beta/models/{rest...}", s.auth(tr.Gemini, s.handleProxy(tr.Gemini)))
	mux.Handle("GET /v1beta/models", s.auth(tr.Gemini, http.HandlerFunc(s.handleGeminiModels)))
	mux.Handle("GET /v1/models", s.auth(tr.OpenAI, http.HandlerFunc(s.handleModels)))
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.Handle("GET /api/status", s.dashAuth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /api/usage", s.dashAuth(http.HandlerFunc(s.handleUsage)))
	mux.Handle("GET /dashboard", s.dashAuth(dashboard.Handler()))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	return mux
}

// auth checks the local API key. With no keys configured, only loopback clients are allowed.
func (s *Server) auth(f tr.Format, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.state.Load()
		if !st.HasAPIKeys() {
			if !isLoopback(r.RemoteAddr) {
				writeError(w, f, http.StatusUnauthorized, "authentication_error", "no API keys configured; only local clients allowed")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("x-api-key")
		if key == "" {
			key = r.Header.Get("x-goog-api-key")
		}
		if a := r.Header.Get("Authorization"); key == "" && len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
			key = a[7:]
		}
		if key == "" && f == tr.Gemini {
			key = r.URL.Query().Get("key") // Gemini SDKs may pass ?key=
		}
		ki := (*router.KeyInfo)(nil)
		if key != "" {
			ki = st.CheckAPIKey(key)
		}
		if ki == nil {
			writeError(w, f, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(withKey(r.Context(), ki)))
	})
}

// dashAuth protects the dashboard with the imported 9Router password (bcrypt) via Basic auth.
func (s *Server) dashAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := s.state.Load().Snap.DashboardPassword
		if hash == "" {
			if !isLoopback(r.RemoteAddr) {
				http.Error(w, "dashboard is local-only until a password is set", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		if ok && s.checkPassword(hash, pass) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="FastRouter", charset="UTF-8"`)
		http.Error(w, "password required (same as your 9Router dashboard; any username)", http.StatusUnauthorized)
	})
}

func (s *Server) checkPassword(hash, pass string) bool {
	k := sha256.Sum256([]byte(hash + "\x00" + pass))
	if _, ok := s.pwOK.Load(k); ok {
		return true
	}
	var ok bool
	if strings.HasPrefix(hash, "$2") {
		ok = bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
	} else {
		ok = subtle.ConstantTimeCompare([]byte(hash), []byte(pass)) == 1
	}
	if ok {
		s.pwOK.Store(k, true)
	}
	return ok
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	accounts := 0
	for _, p := range st.List {
		accounts += p.Pool.Size()
	}
	writeJSON(w, map[string]any{"status": "ok", "version": Version, "uptimeSec": int(time.Since(s.start).Seconds()), "usableAccounts": accounts})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	data := make([]map[string]any, 0, len(st.Models))
	for _, m := range st.Models {
		data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "fastrouter"})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
