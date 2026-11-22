package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/nesaon03/fastrouter/internal/store"
	"github.com/nesaon03/fastrouter/internal/tunnel"
)

// Endpoint page: Local / Tunnel (Cloudflare quick tunnel) / Tailscale.

func (s *Server) settingBool(k string) bool {
	var v bool
	_ = json.Unmarshal(s.state.Load().Snap.Settings[k], &v)
	return v
}

func (s *Server) setSettingBool(k string, v bool) error {
	return s.mutate(func(sn *store.Snapshot) error {
		if sn.Settings == nil {
			sn.Settings = map[string]json.RawMessage{}
		}
		b, _ := json.Marshal(v)
		sn.Settings[k] = b
		return nil
	})
}

// exposeReady: like 9Router, exposing FastRouter needs an API key and a
// dashboard password first.
func (s *Server) exposeReady() error {
	st := s.state.Load()
	var miss []string
	if !st.HasAPIKeys() {
		miss = append(miss, "create an API key (API Keys page)")
	}
	if st.Snap.DashboardPassword == "" {
		miss = append(miss, "set a dashboard password (Settings)")
	}
	if len(miss) > 0 {
		return fmt.Errorf("security required before exposing FastRouter: %s", strings.Join(miss, " and "))
	}
	return nil
}

// ResumeTunnel restarts the Cloudflare tunnel at startup if it was left on.
func (s *Server) ResumeTunnel() {
	if !s.settingBool("frTunnelEnabled") {
		return
	}
	if err := s.exposeReady(); err != nil {
		log.Printf("tunnel not resumed: %v", err)
		return
	}
	if err := s.cf.Enable(); err != nil {
		log.Printf("tunnel not resumed: %v", err)
	}
}

func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	active := 0
	for _, k := range st.Snap.APIKeys {
		if k.Active {
			active++
		}
	}
	all := tunnel.ListensOnAll(s.host)
	lan := []string{}
	for _, ip := range tunnel.LANAddrs() {
		lan = append(lan, fmt.Sprintf("http://%s:%d", ip, s.port))
	}
	ts := s.ts.Status(r.Context())
	direct := []string{}
	if all {
		for _, ip := range ts.IPs {
			if p := net.ParseIP(ip); p != nil && p.To4() != nil {
				direct = append(direct, fmt.Sprintf("http://%s:%d", ip, s.port))
			}
		}
	}
	adminJSON(w, http.StatusOK, map[string]any{
		"port":                s.port,
		"host":                s.host,
		"listensAll":          all,
		"local":               fmt.Sprintf("http://127.0.0.1:%d", s.port),
		"lan":                 lan,
		"direct":              direct,
		"tunnel":              s.cf.Status(),
		"tailscale":           ts,
		"tailscaleInstallUrl": tunnel.InstallURL(),
		"security": map[string]any{
			"activeKeys":      active,
			"passwordSet":     st.Snap.DashboardPassword != "",
			"remoteDashboard": s.settingBool("frRemoteDashboard"),
		},
	})
}

func (s *Server) handleTunnelInstall(w http.ResponseWriter, r *http.Request) {
	if err := s.cf.Install(nil); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleTunnelEnable(w http.ResponseWriter, r *http.Request) {
	if err := s.exposeReady(); err != nil {
		adminErr(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.cf.Enable(); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.setSettingBool("frTunnelEnabled", true); err != nil {
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, s.cf.Status())
}

func (s *Server) handleTunnelDisable(w http.ResponseWriter, r *http.Request) {
	s.cf.Disable()
	if err := s.setSettingBool("frTunnelEnabled", false); err != nil {
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, s.cf.Status())
}

func (s *Server) handleTailscaleEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Mode == "" {
		in.Mode = "serve"
	}
	if err := s.exposeReady(); err != nil {
		adminErr(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.ts.Enable(r.Context(), in.Mode); err != nil {
		var na *tunnel.NeedsAction
		if errors.As(err, &na) {
			adminJSON(w, http.StatusConflict, map[string]any{"error": na.What, "actionUrl": na.URL})
			return
		}
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, s.ts.Status(r.Context()))
}

func (s *Server) handleTailscaleDisable(w http.ResponseWriter, r *http.Request) {
	if err := s.ts.Disable(r.Context()); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, s.ts.Status(r.Context()))
}

func (s *Server) handleTailscaleLogin(w http.ResponseWriter, r *http.Request) {
	u, err := s.ts.Login(r.Context())
	if err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, map[string]any{"authUrl": u})
}

func (s *Server) handleRemoteDashboard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Enabled && s.state.Load().Snap.DashboardPassword == "" {
		adminErr(w, http.StatusConflict, "set a dashboard password first")
		return
	}
	if err := s.setSettingBool("frRemoteDashboard", in.Enabled); err != nil {
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, map[string]any{"remoteDashboard": in.Enabled})
}

// proxyHeaders are set by cloudflared, Tailscale Serve/Funnel and ordinary
// reverse proxies. Such requests reach us from 127.0.0.1 but come from
// elsewhere, so they never get loopback trust.
var proxyHeaders = []string{"Cf-Connecting-Ip", "Cf-Ray", "Cf-Warp-Tag-Id", "Tailscale-User-Login", "Tailscale-Funnel-Request",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"}

func (s *Server) proxied(r *http.Request) bool {
	if s.explicitHost(r.Host) {
		return false // the user listed this host in allowedHosts (their own proxy)
	}
	for _, h := range proxyHeaders {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

func (s *Server) explicitHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	return s.allowedHosts[strings.ToLower(strings.Trim(h, "[]"))]
}

func publicPath(p string) bool {
	return p == "/v1" || strings.HasPrefix(p, "/v1/") || p == "/v1beta" || strings.HasPrefix(p, "/v1beta/") || p == "/health" || p == "/api/health"
}

// remoteGuard keeps the dashboard and admin API local when a request came in
// through a tunnel, unless "Dashboard over tunnel" is on (which still needs
// the password). The /v1 API stays reachable but needs an API key.
func (s *Server) remoteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r.URL.Path) || !s.proxied(r) || s.settingBool("frRemoteDashboard") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, fmt.Sprintf("The FastRouter dashboard is only available on its own computer (http://127.0.0.1:%d/dashboard). Through a tunnel only the API (/v1, /v1beta) is served, and it needs an API key.", s.port), http.StatusForbidden)
	})
}
