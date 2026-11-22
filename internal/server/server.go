// Package server is FastRouter's HTTP front end.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nesaon03/fastrouter/internal/config"
	"github.com/nesaon03/fastrouter/internal/dashboard"
	"github.com/nesaon03/fastrouter/internal/login"
	"github.com/nesaon03/fastrouter/internal/oauth"
	"github.com/nesaon03/fastrouter/internal/pricing"
	"github.com/nesaon03/fastrouter/internal/router"
	"github.com/nesaon03/fastrouter/internal/saver"
	"github.com/nesaon03/fastrouter/internal/store"
	tr "github.com/nesaon03/fastrouter/internal/translate"
	"github.com/nesaon03/fastrouter/internal/tunnel"
	"github.com/nesaon03/fastrouter/internal/update"
	"golang.org/x/crypto/bcrypt"
)

// Version is the build's version. It defaults to the release the source is on;
// -ldflags "-X .../server.Version=..." overrides it when the toolchain honours
// it (main.commit and main.date come from the VCS stamps instead).
var Version = "0.7.2"

// Server routes client requests to upstreams.
type Server struct {
	state atomic.Pointer[router.State]
	// inflight counts open upstream attempts per provider (usage graph).
	inflight sync.Map // provider id -> *atomic.Int64
	client   *http.Client
	rec      *recorder
	start    time.Time

	cfgFirstByte, cfgIdle, cfgCooldown, cfgEmptyWait time.Duration
	keyLimits                                        map[string]config.KeyLimit
	st                                               *store.Store
	tsCfg                                            config.TokenSavers
	logins                                           *logins
	refresher                                        *login.Refresher
	oauthRefresher                                   *oauth.Refresher
	saver                                            atomic.Pointer[saver.Saver]

	allowedHosts map[string]bool // M5: extra hosts trusted in open (keyless) mode

	sessions   sessionStore // dashboard logins
	loginLimit loginLimiter
	adminMu    sync.Mutex // serialises dashboard writes

	pwOK sync.Map // sha256(user:pass) -> true, so bcrypt runs once per credential

	sticky stickyStore // smart-combo session picks

	hr *saver.HeadroomCtl // Token Saver page: install / start / stop Headroom

	dataDir string // CLI Tools backups live under dataDir/cli-backups
	port    int
	host    string
	cfgPath string // the config file this process loaded (for autostart)

	cfgDiscovery config.ModelDiscovery // runtime model-list poller

	// harvested holds rate cards learned from a public price list (see
	// harvestPricing); they are re-applied whenever Reload builds a new table.
	harvestMu sync.Mutex
	harvested map[string]pricing.Price
	harvestAt time.Time

	// blocks remembers account+model pairs an account said it does not serve,
	// so one plan gap costs one failed request per model, not one per call.
	blocks modelBlock

	// swarms keeps live/recent swarm runs for the Combos page diagram.
	swarms swarmTracker

	// decCache serves repeated decision judgments without a second engine call.
	decMu    sync.Mutex
	decCache map[[32]byte]decisionCacheEntry

	// updates checks the npm release channel (cached, read-only).
	updates *update.Checker

	// saverRuns records when each token saver last actually changed a request,
	// so the Token Saver page can show real activity instead of the toggle.
	saverRuns saverRuns

	bench benchState // Speed Test tab

	cf *tunnel.Cloudflared // Endpoint page: Cloudflare quick tunnel
	ts *tunnel.Tailscale   // Endpoint page: Tailscale Serve / Funnel

	pinger autoPinger // Quota Tracker: Codex auto-ping
}

// New builds a server from config and a loaded snapshot. st may be nil (tests).
func New(cfg config.Config, snap *store.Snapshot, st *store.Store) *Server {
	s := &Server{
		client: &http.Client{Transport: NewTransport(cfg),
			// H3: never follow redirects. Go would copy x-api-key / x-goog-api-key
			// (and replay the body) to another host; LLM APIs never redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		rec:          newRecorder(st),
		start:        time.Now(),
		cfgFirstByte: time.Duration(cfg.FirstByteTimeout),
		cfgIdle:      time.Duration(cfg.IdleTimeout),
		cfgCooldown:  time.Duration(cfg.CooldownBase),
		cfgEmptyWait: time.Duration(cfg.EmptyResponseWait),
		keyLimits:    cfg.KeyLimits,
		st:           st,
		tsCfg:        cfg.TokenSavers,
		allowedHosts: map[string]bool{},
		hr:           newHeadroomCtl(cfg.DataDir),
		dataDir:      cfg.DataDir,
		port:         cfg.Port,
		host:         cfg.Host,
		cfgPath:      cfg.Path,
		cfgDiscovery: cfg.ModelDiscovery,
		updates:      update.New(),
	}
	s.cf = &tunnel.Cloudflared{Dir: cfg.DataDir, Port: cfg.Port}
	s.ts = &tunnel.Tailscale{Port: cfg.Port}
	for _, h := range cfg.AllowedHosts {
		s.allowedHosts[strings.ToLower(strings.TrimSpace(h))] = true
	}
	s.logins = newLogins(cfg.Plugins.CodexCallbackPort, s.client)
	s.refresher = &login.Refresher{Client: s.client, Endpoints: login.Default, RefreshImported: cfg.Plugins.RefreshImported,
		OnUpdate: func(store.Connection) {
			if err := s.ReloadFromStore(); err != nil {
				log.Printf("reload after token refresh failed: %v", err)
			}
		}}
	s.oauthRefresher = &oauth.Refresher{Client: s.client, RefreshImported: cfg.Plugins.RefreshImported, OnUpdate: s.refresher.OnUpdate}
	if st != nil {
		s.refresher.Store = st
		s.oauthRefresher.Store = st
		s.swarms.onFinish = s.persistSwarmRun
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
	next := router.Build(snap, s.keyLimits)
	next.CarryOver(s.state.Load())
	s.state.Store(next)
	t, err := pricing.New(snap.Settings["backup.pricing"])
	if err != nil {
		log.Printf("imported pricing overrides ignored: %v", err)
		t, _ = pricing.New(nil)
	}
	// Harvested rates live outside the table (they come from a public list, not
	// from settings), so a reload must put them back or a config write would
	// silently unpriced every discovered model again.
	s.harvestMu.Lock()
	for id, p := range s.harvested {
		t.Learn(id, p)
	}
	s.harvestMu.Unlock()
	s.rec.prices.Store(t)
	s.saver.Store(saver.New(snap.Settings, s.tsCfg))
}

// Saver returns the current token-saver settings.
func (s *Server) Saver() *saver.Saver { return s.saver.Load() }

// Close flushes background writes.
func (s *Server) Close() {
	s.cf.Disable() // stops the process only; the saved setting stays as it was
	s.rec.close()
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", s.auth(tr.OpenAI, s.handleProxy(tr.OpenAI)))
	mux.Handle("POST /v1/messages", s.auth(tr.Anthropic, s.handleProxy(tr.Anthropic)))
	mux.Handle("POST /v1/responses", s.auth(tr.Responses, s.handleProxy(tr.Responses)))
	mux.Handle("POST /v1/messages/count_tokens", s.auth(tr.Anthropic, http.HandlerFunc(s.handleCountTokens)))
	mux.Handle("POST /v1/decisions", s.auth(tr.OpenAI, http.HandlerFunc(s.handleDecisions)))
	mux.Handle("POST /v1/embeddings", s.auth(tr.OpenAI, s.handleOpenAIPass("embeddings")))
	mux.Handle("POST /v1/images/generations", s.auth(tr.OpenAI, s.handleOpenAIPass("images")))
	mux.Handle("POST /v1/audio/transcriptions", s.auth(tr.OpenAI, s.handleOpenAIPass("transcriptions")))
	mux.Handle("POST /v1/audio/speech", s.auth(tr.OpenAI, s.handleOpenAIPass("speech")))
	mux.Handle("POST /v1beta/models/{rest...}", s.auth(tr.Gemini, s.handleProxy(tr.Gemini)))
	mux.Handle("GET /v1beta/models", s.auth(tr.Gemini, http.HandlerFunc(s.handleGeminiModels)))
	mux.Handle("GET /v1/models", s.auth(tr.OpenAI, http.HandlerFunc(s.handleModels)))
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.Handle("GET /api/status", s.dashAuth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /api/usage", s.dashAuth(http.HandlerFunc(s.handleUsage)))
	mux.Handle("GET /api/usage/requests", s.dashAuth(http.HandlerFunc(s.handleUsageRequests)))
	mux.Handle("GET /api/logins", s.dashAuth(http.HandlerFunc(s.handleLogins)))
	mux.Handle("POST /api/login/{provider}/start", s.adminWrite(s.handleLoginStart))
	mux.Handle("GET /api/login/flows/{id}", s.dashAuth(http.HandlerFunc(s.handleLoginFlow)))
	mux.Handle("POST /api/login/flows/{id}/callback", s.adminWrite(s.handleLoginPaste))
	mux.HandleFunc("GET /callback", s.handleOAuthCallback) // browser redirect; matched by the flow's random state

	// Dashboard session and admin API (see admin.go).
	mux.HandleFunc("GET /api/auth/session", s.handleSession)
	mux.HandleFunc("POST /api/auth/login", s.handleLoginPassword)
	mux.Handle("POST /api/auth/logout", s.adminWrite(s.handleLogout))
	mux.Handle("POST /api/auth/password", s.adminWrite(s.handleSetPassword))
	mux.Handle("GET /api/admin/state", s.adminRead(s.handleAdminState))
	mux.Handle("GET /api/admin/swarm", s.adminRead(http.HandlerFunc(s.handleSwarmRuns)))
	mux.Handle("GET /api/admin/swarm/{id}", s.adminRead(http.HandlerFunc(s.handleSwarmRun)))
	mux.Handle("GET /api/update", s.dashAuth(http.HandlerFunc(s.handleUpdate)))
	mux.Handle("POST /api/admin/accounts", s.adminWrite(s.handleAccountCreate))
	mux.Handle("POST /api/admin/accounts/reorder", s.adminWrite(s.handleAccountReorder))
	mux.Handle("PATCH /api/admin/accounts/{id}", s.adminWrite(s.handleAccountUpdate))
	mux.Handle("DELETE /api/admin/accounts/{id}", s.adminWrite(s.handleAccountDelete))
	mux.Handle("POST /api/admin/nodes", s.adminWrite(s.handleNodeCreate))
	mux.Handle("POST /api/admin/nodes/check", s.adminWrite(s.handleNodeCheck))
	mux.Handle("GET /api/admin/nodes/prefix", s.adminRead(s.handlePrefixCheck))
	mux.Handle("PATCH /api/admin/nodes/{id}", s.adminWrite(s.handleNodeUpdate))
	mux.Handle("DELETE /api/admin/nodes/{id}", s.adminWrite(s.handleNodeDelete))
	mux.Handle("POST /api/admin/combos", s.adminWrite(s.handleComboCreate))
	mux.Handle("PATCH /api/admin/combos/{id}", s.adminWrite(s.handleComboUpdate))
	mux.Handle("DELETE /api/admin/combos/{id}", s.adminWrite(s.handleComboDelete))
	mux.Handle("POST /api/admin/keys", s.adminWrite(s.handleKeyCreate))
	mux.Handle("PATCH /api/admin/keys/{id}", s.adminWrite(s.handleKeyUpdate))
	mux.Handle("DELETE /api/admin/keys/{id}", s.adminWrite(s.handleKeyDelete))
	mux.Handle("PATCH /api/admin/settings", s.adminWrite(s.handleSettingsUpdate))
	mux.Handle("POST /api/admin/savers/preview", s.adminWrite(s.handleSaverPreview))
	mux.Handle("GET /api/admin/logs/stream", s.adminRead(s.handleLogStream))
	mux.Handle("DELETE /api/admin/logs", s.adminWrite(s.handleLogClear))
	mux.Handle("POST /api/admin/providers/test", s.adminWrite(s.handleProviderTest))
	mux.Handle("GET /api/admin/quota", s.adminRead(s.handleQuota))
	mux.Handle("GET /api/admin/quota/{id}/codex-reset-credits", s.adminRead(s.handleResetCredits))
	mux.Handle("POST /api/admin/quota/{id}/codex-reset-credits", s.adminWrite(s.handleResetConsume))
	mux.Handle("PUT /api/admin/quota/{id}/autoping", s.adminWrite(s.handleAutoPingSave))
	mux.Handle("GET /api/admin/providers/{id}/models", s.adminRead(s.handleProviderModels))
	mux.Handle("POST /api/admin/providers/{id}/refresh-models", s.adminWrite(s.handleModelRefresh))
	mux.Handle("POST /api/admin/models", s.adminWrite(s.handleModelCreate))
	mux.Handle("DELETE /api/admin/models", s.adminWrite(s.handleModelDelete))
	mux.Handle("GET /api/admin/autostart", s.adminRead(s.handleAutostartGet))
	mux.Handle("POST /api/admin/autostart", s.adminWrite(s.handleAutostartSet))
	mux.Handle("GET /api/admin/headroom", s.adminRead(s.handleHeadroomStatus))
	mux.Handle("POST /api/admin/headroom/install", s.adminWrite(s.handleHeadroomInstall))
	mux.Handle("POST /api/admin/headroom/uninstall", s.adminWrite(s.handleHeadroomUninstall))
	mux.Handle("POST /api/admin/headroom/start", s.adminWrite(s.handleHeadroomStart))
	mux.Handle("POST /api/admin/headroom/stop", s.adminWrite(s.handleHeadroomStop))
	mux.Handle("POST /api/admin/headroom/restart", s.adminWrite(s.handleHeadroomRestart))
	mux.Handle("GET /api/admin/cli-tools", s.adminRead(s.handleCLITools))
	mux.Handle("GET /api/admin/cli-tools/{id}", s.adminRead(s.handleCLITool))
	mux.Handle("POST /api/admin/cli-tools/{id}/apply", s.adminWrite(s.handleCLIApply))
	mux.Handle("POST /api/admin/cli-tools/{id}/reset", s.adminWrite(s.handleCLIReset))
	mux.Handle("GET /api/admin/skills", s.adminRead(s.handleSkillList))
	mux.Handle("GET /api/admin/bench/options", s.adminRead(s.handleBenchOptions))
	mux.Handle("GET /api/admin/bench/history", s.adminRead(s.handleBenchHistory))
	mux.Handle("POST /api/admin/bench", s.adminWrite(s.handleBenchStart))
	mux.Handle("GET /api/admin/bench/{id}", s.adminRead(s.handleBenchGet))
	mux.Handle("POST /api/admin/bench/{id}/stop", s.adminWrite(s.handleBenchStop))
	mux.Handle("DELETE /api/admin/bench/{id}", s.adminWrite(s.handleBenchDelete))
	mux.Handle("GET /api/admin/bench/{id}/export", s.adminRead(s.handleBenchExport))
	mux.Handle("GET /api/admin/endpoint", s.adminRead(s.handleEndpoint))
	mux.Handle("POST /api/admin/tunnel/install", s.adminWrite(s.handleTunnelInstall))
	mux.Handle("POST /api/admin/tunnel/enable", s.adminWrite(s.handleTunnelEnable))
	mux.Handle("POST /api/admin/tunnel/disable", s.adminWrite(s.handleTunnelDisable))
	mux.Handle("POST /api/admin/tailscale/enable", s.adminWrite(s.handleTailscaleEnable))
	mux.Handle("POST /api/admin/tailscale/disable", s.adminWrite(s.handleTailscaleDisable))
	mux.Handle("POST /api/admin/tailscale/login", s.adminWrite(s.handleTailscaleLogin))
	mux.Handle("POST /api/admin/remote-dashboard", s.adminWrite(s.handleRemoteDashboard))
	mux.HandleFunc("GET /skills/{name}/SKILL.md", s.handleSkill)

	// The SPA itself is static and public; all data comes from the APIs above.
	mux.Handle("GET /dashboard", dashboard.Handler())
	mux.Handle("GET /dashboard/{path...}", dashboard.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
	})
	return s.remoteGuard(mux)
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
			if msg := s.openModeRefusal(r); msg != "" {
				writeError(w, f, http.StatusForbidden, "permission_error", msg)
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
		if s.sessionFrom(r) != nil && (hash != "" || s.openDashboardOK(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if hash == "" {
			if !isLoopback(r.RemoteAddr) {
				http.Error(w, "dashboard is local-only until a password is set", http.StatusForbidden)
				return
			}
			if msg := s.openModeRefusal(r); msg != "" {
				http.Error(w, msg, http.StatusForbidden)
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
		http.Error(w, "dashboard password required (any username)", http.StatusUnauthorized)
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

// openModeRefusal guards requests trusted only because they come from
// loopback (M5). A browser on this machine is loopback too, so a web page could
// otherwise spend quota (CSRF) or read answers after DNS rebinding. Returns
// a reason to refuse, or "".
func (s *Server) openModeRefusal(r *http.Request) string {
	if s.proxied(r) {
		return "requests through a tunnel or proxy need an API key"
	}
	if !s.hostAllowed(r.Host) {
		return "Host " + r.Host + " is not allowed without an API key (add it to allowedHosts or create an API key)"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !s.hostAllowed(u.Host) {
			return "cross-origin request refused without an API key"
		}
	}
	return ""
}

func (s *Server) hostAllowed(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return s.allowedHosts[h]
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
