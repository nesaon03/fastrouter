package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/login"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/store"
)

// Dashboard admin API. Every write needs a session cookie (set by logging in
// with the dashboard password) plus the session's CSRF token in X-CSRF-Token,
// a JSON body and a same-origin Origin. Secrets are write-only: the browser
// only ever sees masked hints. Writes are persisted and swapped in live with
// Reload, the same path as SIGHUP, so the /v1 hot path is untouched.

const sessionCookie = "fr_session"
const sessionTTL = 24 * time.Hour

type session struct {
	csrf string
	exp  time.Time
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]*session // sha256(token) -> session
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tokenKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (ss *sessionStore) create() (tok string, se *session) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.m == nil {
		ss.m = map[string]*session{}
	}
	now := time.Now()
	for k, v := range ss.m {
		if now.After(v.exp) {
			delete(ss.m, k)
		}
	}
	tok = newToken()
	se = &session{csrf: newToken(), exp: now.Add(sessionTTL)}
	ss.m[tokenKey(tok)] = se
	return tok, se
}

func (ss *sessionStore) get(tok string) *session {
	if tok == "" {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	se := ss.m[tokenKey(tok)]
	if se == nil || time.Now().After(se.exp) {
		return nil
	}
	return se
}

func (ss *sessionStore) drop(tok string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.m, tokenKey(tok))
}

func (ss *sessionStore) clear() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.m = map[string]*session{}
}

// loginLimiter slows password guessing: 5 failures per IP lock it out for a minute (L7).
type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*struct {
		n     int
		until time.Time
	}
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[ip]
	return e != nil && time.Now().Before(e.until)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*struct {
			n     int
			until time.Time
		}{}
	}
	e := l.m[ip]
	if e == nil {
		e = &struct {
			n     int
			until time.Time
		}{}
		l.m[ip] = e
	}
	e.n++
	if e.n >= 5 {
		e.n, e.until = 0, time.Now().Add(time.Minute)
	}
}

func (l *loginLimiter) ok(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) sessionFrom(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return s.sessions.get(c.Value)
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request) *session {
	tok, se := s.sessions.create()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(sessionTTL / time.Second)})
	return se
}

// sameOrigin reports whether an Origin header names the host the request was sent to.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// openDashboardOK is the keyless rule: no password set means loopback only, with a local Host/Origin.
func (s *Server) openDashboardOK(r *http.Request) bool {
	return isLoopback(r.RemoteAddr) && s.openModeRefusal(r) == ""
}

func adminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func adminErr(w http.ResponseWriter, status int, msg string) {
	adminJSON(w, status, map[string]string{"error": msg})
}

// adminWrite guards every state-changing dashboard call.
func (s *Server) adminWrite(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		se := s.sessionFrom(r)
		if se == nil {
			adminErr(w, http.StatusUnauthorized, "log in first")
			return
		}
		if s.state.Load().Snap.DashboardPassword == "" && !s.openDashboardOK(r) {
			adminErr(w, http.StatusForbidden, "dashboard is local-only until a password is set")
			return
		}
		got := r.Header.Get("X-CSRF-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(se.csrf)) != 1 {
			adminErr(w, http.StatusForbidden, "missing or wrong CSRF token")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			adminErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if r.Method != http.MethodDelete && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			adminErr(w, http.StatusUnsupportedMediaType, "send JSON")
			return
		}
		h(w, r)
	})
}

// adminRead allows a session cookie, or the older Basic-auth / keyless rules.
func (s *Server) adminRead(h http.HandlerFunc) http.Handler {
	return s.dashAuth(h)
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		adminErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// ---- auth endpoints ----

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	pwSet := s.state.Load().Snap.DashboardPassword != ""
	out := map[string]any{"passwordSet": pwSet, "authenticated": false, "version": Version}
	if se := s.sessionFrom(r); se != nil && (pwSet || s.openDashboardOK(r)) {
		out["authenticated"], out["csrf"] = true, se.csrf
	} else if !pwSet {
		if s.openDashboardOK(r) {
			se := s.setSession(w, r)
			out["authenticated"], out["csrf"] = true, se.csrf
		} else {
			out["error"] = "dashboard is local-only until a password is set"
		}
	}
	adminJSON(w, http.StatusOK, out)
}

func (s *Server) handleLoginPassword(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
		adminErr(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		adminErr(w, http.StatusUnsupportedMediaType, "send JSON")
		return
	}
	ip := clientIP(r)
	if s.loginLimit.blocked(ip) {
		adminErr(w, http.StatusTooManyRequests, "too many wrong passwords; wait a minute")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	hash := s.state.Load().Snap.DashboardPassword
	if hash == "" {
		if !s.openDashboardOK(r) {
			adminErr(w, http.StatusForbidden, "dashboard is local-only until a password is set")
			return
		}
	} else if len(in.Password) > 200 || !s.checkPassword(hash, in.Password) {
		s.loginLimit.fail(ip)
		adminErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.loginLimit.ok(ip)
	se := s.setSession(w, r)
	adminJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": se.csrf})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	adminJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if l := len(in.New); l < 8 || l > 200 {
		adminErr(w, http.StatusBadRequest, "new password must be 8 to 200 characters")
		return
	}
	ip := clientIP(r)
	if s.loginLimit.blocked(ip) {
		adminErr(w, http.StatusTooManyRequests, "too many wrong passwords; wait a minute")
		return
	}
	if hash := s.state.Load().Snap.DashboardPassword; hash != "" && !s.checkPassword(hash, in.Current) {
		s.loginLimit.fail(ip)
		adminErr(w, http.StatusUnauthorized, "current password is wrong")
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	if err != nil {
		adminErr(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	if err := s.mutate(func(sn *store.Snapshot) error { sn.DashboardPassword = string(h); return nil }); err != nil {
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sessions.clear()
	se := s.setSession(w, r)
	adminJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": se.csrf})
}

// ---- mutation plumbing ----

var errNotFound = errors.New("not found")

type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func bad(format string, a ...any) error { return badRequest{fmt.Sprintf(format, a...)} }

// mutate applies fn to a copy of the configuration, saves it and swaps it in.
func (s *Server) mutate(fn func(*store.Snapshot) error) error {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	var next *store.Snapshot
	if s.st != nil {
		var err error
		if next, err = s.st.Load(); err != nil { // authoritative: includes sign-ins and refreshes
			return err
		}
	} else {
		b, err := json.Marshal(s.state.Load().Snap)
		if err != nil {
			return err
		}
		next = &store.Snapshot{}
		if err := json.Unmarshal(b, next); err != nil {
			return err
		}
	}
	if next.Settings == nil {
		next.Settings = map[string]json.RawMessage{}
	}
	if err := fn(next); err != nil {
		return err
	}
	if s.st != nil {
		if err := s.st.ReplaceAll(next); err != nil {
			return err
		}
	}
	s.Reload(next)
	return nil
}

func (s *Server) writeResult(w http.ResponseWriter, err error, ok any) {
	var br badRequest
	switch {
	case err == nil:
		adminJSON(w, http.StatusOK, ok)
	case errors.Is(err, errNotFound):
		adminErr(w, http.StatusNotFound, "not found")
	case errors.As(err, &br):
		adminErr(w, http.StatusBadRequest, br.msg)
	default:
		adminErr(w, http.StatusInternalServerError, "save failed")
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ---- validation ----

var (
	prefixRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	comboNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	localKeyRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{24,200}$`)
	thinkModes  = map[string]bool{"auto": true, "on": true, "off": true, "low": true, "medium": true, "high": true, "max": true}
	saverLevels = map[string]bool{"lite": true, "full": true, "ultra": true}
)

func cleanName(s string, field string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 100 {
		return "", bad("%s must be 1 to 100 characters", field)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", bad("%s has control characters", field)
		}
	}
	return s, nil
}

func cleanSecret(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 8192 {
		return "", bad("API key must be 1 to 8192 characters")
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", bad("API key has spaces or control characters")
		}
	}
	return s, nil
}

func cleanURL(raw string, field string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(raw) > 500 {
		return "", bad("%s must be an http(s) URL", field)
	}
	if u.User != nil {
		return "", bad("%s must not contain a user name or password", field)
	}
	return strings.TrimRight(raw, "/"), nil
}

func cleanModels(in []string, max int) ([]string, error) {
	if len(in) > max {
		return nil, bad("at most %d models", max)
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || len(m) > 200 || strings.ContainsAny(m, " \t\r\n") {
			return nil, bad("invalid model %q", m)
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out, nil
}

// prefixTaken reports whether a model prefix already reaches another provider.
func prefixTaken(sn *store.Snapshot, prefix, exceptNode string) bool {
	if catalog.Reserved(prefix) {
		return true
	}
	if c, ok := catalog.Lookup(prefix); ok {
		for _, conn := range sn.Connections {
			if conn.Provider == c.ID {
				return true // an extra provider already in use
			}
		}
	}
	for _, n := range sn.Nodes {
		if n.ID == exceptNode {
			continue
		}
		if n.Prefix == prefix || n.ID == prefix {
			return true
		}
		for _, a := range n.Aliases {
			if a == prefix {
				return true
			}
		}
	}
	return false
}

func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) < 12 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}

// ---- read model ----

type adminAccount struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	AuthType   string `json:"authType"`
	Status     string `json:"status"`
	Priority   int    `json:"priority"`
	Active     bool   `json:"active"`
	Cooling    bool   `json:"cooling"`
	KeyHint    string `json:"keyHint"`
	Region     string `json:"region,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	Expired    bool   `json:"expired,omitempty"`
	Uses       int64  `json:"uses"`
	Failures   int64  `json:"failures"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
	LastStatus int    `json:"lastStatus,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

type adminProvider struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Prefix   string         `json:"prefix"`
	Aliases  []string       `json:"aliases"`
	Kind     string         `json:"kind"` // apikey | login | plugin | openai-compatible | anthropic-compatible
	Login    string         `json:"login,omitempty"`
	Regions  []string       `json:"regions,omitempty"`
	BaseURL  string         `json:"baseUrl,omitempty"`
	APIType  string         `json:"apiType,omitempty"`
	Usable   int            `json:"usable"`
	Accounts []adminAccount `json:"accounts"`
	Color    string         `json:"color,omitempty"`
	KeyURL   string         `json:"keyUrl,omitempty"`
	Extra    bool           `json:"extra,omitempty"`
}

// modelGroup is one provider's section in the dashboard's model picker.
type modelGroup struct {
	Provider string        `json:"provider"`
	Name     string        `json:"name"`
	Prefix   string        `json:"prefix"`
	Plugin   bool          `json:"plugin,omitempty"`
	Models   []pickerModel `json:"models"`
}

type pickerModel struct {
	ID     string `json:"id"` // full routable name, prefix/model
	Name   string `json:"name"`
	Custom bool   `json:"custom,omitempty"`
}

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	sn := st.Snap
	now := time.Now()
	cooling := map[string]bool{}
	usable := map[string]int{}
	for _, p := range st.List {
		usable[p.ID] = p.Pool.Size()
		for _, a := range p.Pool.Accounts() {
			cooling[a.ID] = a.CoolingDown(now)
		}
	}
	var stats map[string]store.ConnStats
	if s.st != nil {
		stats, _ = s.st.ConnectionStats()
	}
	acctsBy := map[string][]adminAccount{}
	for _, c := range sn.Connections {
		a := adminAccount{ID: c.ID, Provider: c.Provider, Name: maskEmail(c.Name), AuthType: c.AuthType, Status: c.Status,
			Priority: c.Priority, Active: c.Active, Cooling: cooling[c.ID]}
		if login.Handles(c.Provider) {
			if login.AccessKey(c) != "" {
				a.KeyHint = "signed in"
			}
			a.ExpiresAt, _ = c.Meta["expiresAt"].(string)
			a.Expired = login.Expired(c, now)
		} else {
			a.KeyHint = maskSecret(c.Secrets["apiKey"])
		}
		if psd, ok := c.Meta["providerSpecificData"].(map[string]any); ok {
			a.Region, _ = psd["region"].(string)
		}
		if e, ok := c.Meta["lastError"].(string); ok && len(e) < 300 {
			a.LastError = e // already redacted at import
		}
		if cs, ok := stats[c.ID]; ok {
			a.Uses, a.Failures, a.LastUsedAt, a.LastStatus = cs.Uses, cs.Failures, cs.LastUsedAt, cs.LastStatus
		}
		acctsBy[c.Provider] = append(acctsBy[c.Provider], a)
	}
	sortAccts := func(as []adminAccount) []adminAccount {
		sort.SliceStable(as, func(i, j int) bool { return as[i].Priority < as[j].Priority })
		if as == nil {
			return []adminAccount{}
		}
		return as
	}
	provs := []adminProvider{}
	for _, c := range catalog.All() {
		kind := "apikey"
		switch {
		case c.Plugin:
			kind = "plugin"
		case c.Login != "":
			kind = "login"
		}
		p := adminProvider{ID: c.ID, Name: c.ID, Prefix: c.PrimaryAlias(), Aliases: append([]string{c.ID}, c.Aliases...), Kind: kind,
			Login: c.Login, Usable: usable[c.ID], Accounts: sortAccts(acctsBy[c.ID]), Extra: c.Extra}
		if d, ok := catalog.Info(c.ID); ok {
			p.Name, p.Color, p.KeyURL = d.Name, d.Color, d.KeyURL
		}
		for r := range c.Regions {
			p.Regions = append(p.Regions, r)
		}
		sort.Strings(p.Regions)
		provs = append(provs, p)
	}
	for _, n := range sn.Nodes {
		kind := "openai-compatible"
		if strings.HasPrefix(n.ID, "anthropic-compatible") || n.Type == "anthropic-compatible" {
			kind = "anthropic-compatible"
		}
		provs = append(provs, adminProvider{ID: n.ID, Name: n.Name, Prefix: n.Prefix, Aliases: append([]string{n.Prefix}, n.Aliases...),
			Kind: kind, BaseURL: n.BaseURL, APIType: n.APIType, Usable: usable[n.ID], Accounts: sortAccts(acctsBy[n.ID])})
	}

	type key struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Active     bool     `json:"active"`
		Hint       string   `json:"hint"`
		Restricted bool     `json:"restricted"`
		Allow      []string `json:"allow"`
	}
	keys := []key{}
	for _, k := range sn.APIKeys {
		var acc struct {
			Restricted bool     `json:"restricted"`
			Allow      []string `json:"allow"`
		}
		_ = json.Unmarshal(k.Access, &acc)
		if acc.Allow == nil {
			acc.Allow = []string{}
		}
		keys = append(keys, key{k.ID, k.Name, k.Active, maskSecret(k.Key), acc.Restricted, acc.Allow})
	}
	type combo struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Kind        string   `json:"kind"`
		Strategy    string   `json:"strategy"`
		Models      []string `json:"models"`
		SmartPolicy string   `json:"smartPolicy,omitempty"`
		RouterModel string   `json:"routerModel,omitempty"`
		JudgeModel  string   `json:"judgeModel,omitempty"`
		Threshold   float64  `json:"confidenceThreshold,omitempty"`
	}
	combos := []combo{}
	opts := comboOptions(sn)
	for _, c := range sn.Combos {
		m := c.Models
		if m == nil {
			m = []string{}
		}
		cb := combo{ID: c.ID, Name: c.Name, Kind: c.Kind, Strategy: c.Strategy, Models: m}
		if o := opts[c.Name]; o != nil {
			cb.SmartPolicy, _ = o["smartPolicy"].(string)
			cb.RouterModel, _ = o["routerModel"].(string)
			cb.JudgeModel, _ = o["judgeModel"].(string)
			cb.Threshold, _ = o["confidenceThreshold"].(float64)
		}
		combos = append(combos, cb)
	}
	models := append([]string{}, st.Models...)
	for _, m := range sn.CustomModels {
		models = append(models, m.ProviderAlias+"/"+m.ID)
	}
	groups := modelGroups(st, sn, provs)
	known := map[string]bool{}
	for _, c := range catalog.All() {
		known[c.ID] = true
	}
	type other struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Color    string `json:"color"`
		Category string `json:"category"`
	}
	others := []other{}
	for id, d := range catalog.DisplayAll() {
		if !known[id] && (d.Category == "oauth" || d.Category == "webCookie" || d.Category == "free" || d.Category == "freeTier") {
			others = append(others, other{id, d.Name, d.Color, d.Category})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	adminJSON(w, http.StatusOK, map[string]any{
		"version":     Version,
		"uptimeSec":   int(time.Since(s.start).Seconds()),
		"passwordSet": sn.DashboardPassword != "",
		"providers":   provs,
		"combos":      combos,
		"keys":        keys,
		"models":      models,
		"modelGroups": groups,
		"others":      others,
		"settings":    s.settingsView(sn),
		"warnings":    maskWarnings(sn.Warnings),
	})
}

type strategyView struct {
	FallbackStrategy      string `json:"fallbackStrategy"`
	StickyRoundRobinLimit int    `json:"stickyRoundRobinLimit,omitempty"`
}

type visionView struct {
	Enabled    bool     `json:"enabled"`
	RoundRobin bool     `json:"roundRobin"`
	Models     []string `json:"models"`
}

func (s *Server) settingsView(sn *store.Snapshot) map[string]any {
	get := func(k string, v any) { _ = json.Unmarshal(sn.Settings[k], v) }
	b := func(k string, def bool) bool { v := def; get(k, &v); return v }
	str := func(k, def string) string {
		v := ""
		get(k, &v)
		if v == "" {
			return def
		}
		return v
	}
	thinking := map[string]string{}
	var th map[string]struct {
		Mode string `json:"mode"`
	}
	get("providerThinking", &th)
	for k, v := range th {
		thinking[k] = v.Mode
	}
	strategies := map[string]strategyView{}
	get("providerStrategies", &strategies)
	sticky := 3
	get("stickyRoundRobinLimit", &sticky)
	var ca struct {
		Vision *struct {
			Enabled    *bool    `json:"enabled"`
			RoundRobin bool     `json:"roundRobin"`
			Models     []string `json:"models"`
		} `json:"vision"`
	}
	get("capacityAdapter", &ca)
	vis := visionView{Models: []string{}}
	if ca.Vision != nil {
		vis.Enabled = ca.Vision.Enabled == nil || *ca.Vision.Enabled
		vis.RoundRobin = ca.Vision.RoundRobin
		if ca.Vision.Models != nil {
			vis.Models = ca.Vision.Models
		}
	}
	// Fields fixed by the config file win over the dashboard; the UI shows them locked.
	locked := []string{}
	o := s.tsCfg
	for name, set := range map[string]bool{"rtkEnabled": o.RTK != nil, "caveman": o.Caveman != nil, "ponytail": o.Ponytail != nil,
		"headroomEnabled": o.Headroom != nil, "headroomUrl": o.HeadroomURL != nil, "headroomCodeAware": o.HeadroomCodeAware != nil} {
		if set {
			locked = append(locked, name)
		}
	}
	sort.Strings(locked)
	return map[string]any{
		"rtkEnabled":            b("rtkEnabled", true),
		"cavemanEnabled":        b("cavemanEnabled", false),
		"cavemanLevel":          str("cavemanLevel", "full"),
		"ponytailEnabled":       b("ponytailEnabled", false),
		"ponytailLevel":         str("ponytailLevel", "full"),
		"headroomEnabled":       b("headroomEnabled", false),
		"headroomUrl":           str("headroomUrl", "http://localhost:8787"),
		"headroomCodeAware":     b("headroomCodeAware", false),
		"providerThinking":      thinking,
		"providerStrategies":    strategies,
		"stickyRoundRobinLimit": sticky,
		"vision":                vis,
		"lockedByConfig":        locked,
	}
}

// ---- accounts ----

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		APIKey   string `json:"apiKey"`
		Priority int    `json:"priority"`
		Active   *bool  `json:"active"`
		Region   string `json:"region"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	id := newID()
	err := s.mutate(func(sn *store.Snapshot) error {
		name, err := cleanName(in.Name, "name")
		if err != nil {
			return err
		}
		key, err := cleanSecret(in.APIKey)
		if err != nil {
			return err
		}
		meta := map[string]any{}
		if c, ok := catalog.Lookup(in.Provider); ok && c.ID == in.Provider {
			if c.Plugin {
				return bad("%s needs a login plugin; it can't be added with an API key", c.ID)
			}
			if c.Login != "" {
				return bad("%s accounts are added with the sign-in button", c.ID)
			}
			if c.Extra && !extraReachable(sn, c) {
				return bad("the prefix %q is used by one of your custom endpoints; rename that endpoint's prefix first", c.PrimaryAlias())
			}
			if in.Region != "" {
				if c.Regions[in.Region] == "" {
					return bad("unknown region %q", in.Region)
				}
				meta["providerSpecificData"] = map[string]any{"region": in.Region}
			}
		} else if !hasNode(sn, in.Provider) {
			return bad("unknown provider %q", in.Provider)
		}
		prio := in.Priority
		if prio <= 0 {
			prio = 1
			for _, c := range sn.Connections {
				if c.Provider == in.Provider && c.Priority >= prio {
					prio = c.Priority + 1
				}
			}
		}
		active := in.Active == nil || *in.Active
		sn.Connections = append(sn.Connections, store.Connection{ID: id, Provider: in.Provider, Name: name, AuthType: "apikey",
			Priority: prio, Active: active, Status: "ok", Secrets: map[string]string{"apiKey": key}, Meta: meta})
		return nil
	})
	s.writeResult(w, err, map[string]string{"id": id})
}

func hasNode(sn *store.Snapshot, id string) bool {
	for _, n := range sn.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     *string `json:"name"`
		APIKey   *string `json:"apiKey"`
		Priority *int    `json:"priority"`
		Active   *bool   `json:"active"`
		Region   *string `json:"region"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i := range sn.Connections {
			c := &sn.Connections[i]
			if c.ID != id {
				continue
			}
			if in.Name != nil {
				n, err := cleanName(*in.Name, "name")
				if err != nil {
					return err
				}
				c.Name = n
			}
			if in.APIKey != nil && *in.APIKey != "" { // empty = keep the stored key
				if login.Handles(c.Provider) {
					return bad("sign-in accounts have no API key; use Sign in again")
				}
				k, err := cleanSecret(*in.APIKey)
				if err != nil {
					return err
				}
				if c.Secrets == nil {
					c.Secrets = map[string]string{}
				}
				c.Secrets["apiKey"] = k
				if c.Status != "needs-plugin" {
					c.Status = "ok"
				}
			}
			if in.Priority != nil {
				if *in.Priority < 1 || *in.Priority > 1000 {
					return bad("priority must be 1 to 1000")
				}
				c.Priority = *in.Priority
			}
			if in.Active != nil {
				c.Active = *in.Active
			}
			if in.Region != nil {
				cat, ok := catalog.Lookup(c.Provider)
				if *in.Region != "" && (!ok || cat.Regions[*in.Region] == "") {
					return bad("unknown region %q", *in.Region)
				}
				if c.Meta == nil {
					c.Meta = map[string]any{}
				}
				psd, _ := c.Meta["providerSpecificData"].(map[string]any)
				if psd == nil {
					psd = map[string]any{}
				}
				psd["region"] = *in.Region
				c.Meta["providerSpecificData"] = psd
			}
			return nil
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i, c := range sn.Connections {
			if c.ID == id {
				sn.Connections = append(sn.Connections[:i], sn.Connections[i+1:]...)
				return nil
			}
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// handleAccountReorder sets priorities 1..n in the given order (drag to reorder).
func (s *Server) handleAccountReorder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string   `json:"provider"`
		IDs      []string `json:"ids"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	err := s.mutate(func(sn *store.Snapshot) error {
		pos := map[string]int{}
		for i, id := range in.IDs {
			pos[id] = i + 1
		}
		n := 0
		for i := range sn.Connections {
			c := &sn.Connections[i]
			if c.Provider != in.Provider {
				continue
			}
			p, ok := pos[c.ID]
			if !ok {
				return bad("the order must list every account of %s", in.Provider)
			}
			c.Priority = p
			n++
		}
		if n != len(in.IDs) {
			return bad("unknown account in the order")
		}
		return nil
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// ---- custom endpoints (nodes) ----

type nodeIn struct {
	Name    *string `json:"name"`
	Prefix  *string `json:"prefix"`
	Type    *string `json:"type"`
	APIType *string `json:"apiType"`
	BaseURL *string `json:"baseUrl"`
}

func applyNode(sn *store.Snapshot, n *store.Node, in nodeIn) error {
	if in.Name != nil {
		v, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		n.Name = v
	}
	if in.Prefix != nil {
		p := strings.ToLower(strings.TrimSpace(*in.Prefix))
		if !prefixRe.MatchString(p) {
			return bad("prefix must be lower-case letters, digits, - or _ (max 32)")
		}
		if prefixTaken(sn, p, n.ID) {
			return bad("prefix %q is already used", p)
		}
		n.Prefix = p
	}
	if in.Type != nil {
		if *in.Type != "openai-compatible" && *in.Type != "anthropic-compatible" {
			return bad("type must be openai-compatible or anthropic-compatible")
		}
		n.Type = *in.Type
	}
	if in.APIType != nil {
		if *in.APIType != "chat" && *in.APIType != "responses" && *in.APIType != "" {
			return bad("apiType must be chat or responses")
		}
		n.APIType = *in.APIType
	}
	if in.BaseURL != nil {
		u, err := cleanURL(*in.BaseURL, "base URL")
		if err != nil {
			return err
		}
		n.BaseURL = u
	}
	return nil
}

func (s *Server) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	var in nodeIn
	if !decodeStrict(w, r, &in) {
		return
	}
	var id string
	err := s.mutate(func(sn *store.Snapshot) error {
		if in.Name == nil || in.Prefix == nil || in.BaseURL == nil {
			return bad("name, prefix and base URL are required")
		}
		typ := "openai-compatible"
		if in.Type != nil {
			typ = *in.Type
		}
		id = typ + "-" + newID()
		n := store.Node{ID: id, Type: typ, APIType: "chat"}
		if typ == "anthropic-compatible" {
			n.APIType = ""
		}
		if err := applyNode(sn, &n, in); err != nil {
			return err
		}
		sn.Nodes = append(sn.Nodes, n)
		return nil
	})
	s.writeResult(w, err, map[string]string{"id": id})
}

func (s *Server) handleNodeUpdate(w http.ResponseWriter, r *http.Request) {
	var in nodeIn
	if !decodeStrict(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i := range sn.Nodes {
			if sn.Nodes[i].ID == id {
				in.Type = nil // the id encodes the type
				return applyNode(sn, &sn.Nodes[i], in)
			}
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// handleNodeDelete removes an endpoint and its accounts.
func (s *Server) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		idx := -1
		for i, n := range sn.Nodes {
			if n.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return errNotFound
		}
		sn.Nodes = append(sn.Nodes[:idx], sn.Nodes[idx+1:]...)
		kept := sn.Connections[:0]
		for _, c := range sn.Connections {
			if c.Provider != id {
				kept = append(kept, c)
			}
		}
		sn.Connections = kept
		return nil
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// ---- combos ----

type comboIn struct {
	Name        *string  `json:"name"`
	Strategy    *string  `json:"strategy"`
	Models      []string `json:"models"`
	SmartPolicy *string  `json:"smartPolicy"`
	RouterModel *string  `json:"routerModel"`
	JudgeModel  *string  `json:"judgeModel"`
	Threshold   *float64 `json:"confidenceThreshold"`
}

var comboStrategies = map[string]bool{"fallback": true, "round-robin": true, "smart": true, "fusion": true}
var smartPolicies = map[string]bool{"smart": true, "strict": true, "per-turn": true}

// comboOptions reads settings.comboStrategies (9Router's per-combo options, by name).
func comboOptions(sn *store.Snapshot) map[string]map[string]any {
	m := map[string]map[string]any{}
	_ = json.Unmarshal(sn.Settings["comboStrategies"], &m)
	return m
}

func setComboOptions(sn *store.Snapshot, m map[string]map[string]any) {
	b, _ := json.Marshal(m)
	sn.Settings["comboStrategies"] = b
}

func cleanOptModel(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	m, err := cleanModels([]string{v}, 1)
	if err != nil {
		return "", err
	}
	return m[0], nil
}

func applyCombo(sn *store.Snapshot, c *store.Combo, in comboIn) error {
	opts := comboOptions(sn)
	oldName := c.Name
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if !comboNameRe.MatchString(n) {
			return bad("combo name must be letters, digits, ., - or _ (max 64)")
		}
		for _, o := range sn.Combos {
			if o.ID != c.ID && o.Name == n {
				return bad("a combo named %q exists", n)
			}
		}
		c.Name = n
	}
	if in.Strategy != nil {
		if !comboStrategies[*in.Strategy] {
			return bad("strategy must be fallback, round-robin, smart or fusion")
		}
		c.Strategy = *in.Strategy
	}
	if in.Models != nil {
		m, err := cleanModels(in.Models, 50)
		if err != nil {
			return err
		}
		c.Models = m
	}
	o := opts[oldName]
	if o == nil {
		o = map[string]any{}
	}
	delete(opts, oldName)
	if in.SmartPolicy != nil {
		if !smartPolicies[*in.SmartPolicy] {
			return bad("smart policy must be smart, strict or per-turn")
		}
		o["smartPolicy"] = *in.SmartPolicy
	}
	if in.Threshold != nil {
		if *in.Threshold < 0.1 || *in.Threshold > 1 {
			return bad("confidence threshold must be between 0.1 and 1")
		}
		o["confidenceThreshold"] = *in.Threshold
	}
	for key, v := range map[string]*string{"routerModel": in.RouterModel, "judgeModel": in.JudgeModel} {
		if v == nil {
			continue
		}
		m, err := cleanOptModel(*v)
		if err != nil {
			return err
		}
		if m == "" {
			delete(o, key)
		} else {
			o[key] = m
		}
	}
	if c.Strategy == "fallback" || c.Strategy == "" {
		delete(o, "fallbackStrategy")
	} else {
		o["fallbackStrategy"] = c.Strategy
	}
	if len(o) > 0 {
		opts[c.Name] = o
	}
	setComboOptions(sn, opts)
	return nil
}

func (s *Server) handleComboCreate(w http.ResponseWriter, r *http.Request) {
	var in comboIn
	if !decodeStrict(w, r, &in) {
		return
	}
	id := newID()
	err := s.mutate(func(sn *store.Snapshot) error {
		if in.Name == nil {
			return bad("name is required")
		}
		c := store.Combo{ID: id, Strategy: "fallback", Models: []string{}}
		if err := applyCombo(sn, &c, in); err != nil {
			return err
		}
		sn.Combos = append(sn.Combos, c)
		return nil
	})
	s.writeResult(w, err, map[string]string{"id": id})
}

func (s *Server) handleComboUpdate(w http.ResponseWriter, r *http.Request) {
	var in comboIn
	if !decodeStrict(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i := range sn.Combos {
			if sn.Combos[i].ID == id {
				return applyCombo(sn, &sn.Combos[i], in)
			}
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

func (s *Server) handleComboDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i, c := range sn.Combos {
			if c.ID == id {
				sn.Combos = append(sn.Combos[:i], sn.Combos[i+1:]...)
				opts := comboOptions(sn)
				if _, ok := opts[c.Name]; ok {
					delete(opts, c.Name)
					setComboOptions(sn, opts)
				}
				return nil
			}
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// ---- local API keys ----

type keyIn struct {
	Name       *string  `json:"name"`
	Key        *string  `json:"key"` // generated in the browser; never sent back
	Active     *bool    `json:"active"`
	Restricted *bool    `json:"restricted"`
	Allow      []string `json:"allow"`
}

func applyKey(k *store.APIKey, in keyIn) error {
	if in.Name != nil {
		n, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		k.Name = n
	}
	if in.Active != nil {
		k.Active = *in.Active
	}
	if in.Restricted != nil || in.Allow != nil {
		var acc struct {
			Restricted bool     `json:"restricted"`
			Allow      []string `json:"allow"`
		}
		_ = json.Unmarshal(k.Access, &acc)
		if in.Restricted != nil {
			acc.Restricted = *in.Restricted
		}
		if in.Allow != nil {
			m, err := cleanModels(in.Allow, 100)
			if err != nil {
				return err
			}
			acc.Allow = m
		}
		if acc.Allow == nil {
			acc.Allow = []string{}
		}
		b, _ := json.Marshal(acc)
		k.Access = b
	}
	return nil
}

func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var in keyIn
	if !decodeStrict(w, r, &in) {
		return
	}
	id := newID()
	err := s.mutate(func(sn *store.Snapshot) error {
		if in.Name == nil || in.Key == nil {
			return bad("name and key are required")
		}
		if !localKeyRe.MatchString(*in.Key) {
			return bad("key must be 24 to 200 letters, digits, - or _")
		}
		for _, k := range sn.APIKeys {
			if subtle.ConstantTimeCompare([]byte(k.Key), []byte(*in.Key)) == 1 {
				return bad("that key already exists")
			}
		}
		k := store.APIKey{ID: id, Key: *in.Key, Active: true, Access: json.RawMessage(`{"restricted":false,"allow":[]}`)}
		in.Key = nil
		if err := applyKey(&k, in); err != nil {
			return err
		}
		sn.APIKeys = append(sn.APIKeys, k)
		return nil
	})
	s.writeResult(w, err, map[string]string{"id": id})
}

func (s *Server) handleKeyUpdate(w http.ResponseWriter, r *http.Request) {
	var in keyIn
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Key != nil {
		adminErr(w, http.StatusBadRequest, "a key can't be changed; create a new one")
		return
	}
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i := range sn.APIKeys {
			if sn.APIKeys[i].ID == id {
				return applyKey(&sn.APIKeys[i], in)
			}
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.mutate(func(sn *store.Snapshot) error {
		for i, k := range sn.APIKeys {
			if k.ID == id {
				sn.APIKeys = append(sn.APIKeys[:i], sn.APIKeys[i+1:]...)
				return nil
			}
		}
		return errNotFound
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// ---- settings ----

type settingsIn struct {
	RTKEnabled            *bool                   `json:"rtkEnabled"`
	CavemanEnabled        *bool                   `json:"cavemanEnabled"`
	CavemanLevel          *string                 `json:"cavemanLevel"`
	PonytailEnabled       *bool                   `json:"ponytailEnabled"`
	PonytailLevel         *string                 `json:"ponytailLevel"`
	HeadroomEnabled       *bool                   `json:"headroomEnabled"`
	HeadroomURL           *string                 `json:"headroomUrl"`
	HeadroomCodeAware     *bool                   `json:"headroomCodeAware"`
	ProviderThinking      map[string]string       `json:"providerThinking"`
	ProviderStrategies    map[string]strategyView `json:"providerStrategies"`
	StickyRoundRobinLimit *int                    `json:"stickyRoundRobinLimit"`
	Vision                *visionView             `json:"vision"`
}

func providerKnown(sn *store.Snapshot, id string) bool {
	if c, ok := catalog.Lookup(id); ok && c.ID == id {
		return true
	}
	return hasNode(sn, id)
}

func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var in settingsIn
	if !decodeStrict(w, r, &in) {
		return
	}
	err := s.mutate(func(sn *store.Snapshot) error {
		set := func(k string, v any) {
			b, _ := json.Marshal(v)
			sn.Settings[k] = b
		}
		for k, v := range map[string]*bool{"rtkEnabled": in.RTKEnabled, "cavemanEnabled": in.CavemanEnabled, "ponytailEnabled": in.PonytailEnabled,
			"headroomEnabled": in.HeadroomEnabled, "headroomCodeAware": in.HeadroomCodeAware} {
			if v != nil {
				set(k, *v)
			}
		}
		for k, v := range map[string]*string{"cavemanLevel": in.CavemanLevel, "ponytailLevel": in.PonytailLevel} {
			if v != nil {
				if !saverLevels[*v] {
					return bad("level must be lite, full or ultra")
				}
				set(k, *v)
			}
		}
		if in.HeadroomURL != nil {
			u, err := cleanURL(*in.HeadroomURL, "Headroom URL")
			if err != nil {
				return err
			}
			set("headroomUrl", u)
		}
		if in.ProviderThinking != nil {
			cur := map[string]map[string]any{}
			_ = json.Unmarshal(sn.Settings["providerThinking"], &cur)
			for id, mode := range in.ProviderThinking {
				if !thinkModes[mode] {
					return bad("thinking mode must be auto, on, off, low, medium, high or max")
				}
				if !providerKnown(sn, id) {
					return bad("unknown provider %q", id)
				}
				if mode == "auto" {
					delete(cur, id)
				} else {
					cur[id] = map[string]any{"mode": mode}
				}
			}
			set("providerThinking", cur)
		}
		if in.ProviderStrategies != nil {
			cur := map[string]map[string]any{}
			_ = json.Unmarshal(sn.Settings["providerStrategies"], &cur)
			for id, st := range in.ProviderStrategies {
				if !providerKnown(sn, id) {
					return bad("unknown provider %q", id)
				}
				if st.FallbackStrategy != "fallback" && st.FallbackStrategy != "round-robin" {
					return bad("strategy must be fallback or round-robin")
				}
				if st.StickyRoundRobinLimit < 0 || st.StickyRoundRobinLimit > 1000 {
					return bad("sticky limit must be 0 to 1000")
				}
				e := cur[id]
				if e == nil {
					e = map[string]any{}
				}
				e["fallbackStrategy"] = st.FallbackStrategy
				if st.StickyRoundRobinLimit > 0 {
					e["stickyRoundRobinLimit"] = st.StickyRoundRobinLimit
				} else {
					delete(e, "stickyRoundRobinLimit")
				}
				cur[id] = e
			}
			set("providerStrategies", cur)
		}
		if in.StickyRoundRobinLimit != nil {
			if *in.StickyRoundRobinLimit < 1 || *in.StickyRoundRobinLimit > 1000 {
				return bad("sticky limit must be 1 to 1000")
			}
			set("stickyRoundRobinLimit", *in.StickyRoundRobinLimit)
		}
		if in.Vision != nil {
			m, err := cleanModels(in.Vision.Models, 50)
			if err != nil {
				return err
			}
			ca := map[string]any{}
			_ = json.Unmarshal(sn.Settings["capacityAdapter"], &ca)
			ca["vision"] = map[string]any{"enabled": in.Vision.Enabled, "roundRobin": in.Vision.RoundRobin, "models": m}
			set("capacityAdapter", ca)
		}
		return nil
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// modelGroups lists routable models per provider that has accounts: 9Router's
// catalog ids plus custom models, each as the full prefix/model name.
func modelGroups(st *router.State, sn *store.Snapshot, provs []adminProvider) []modelGroup {
	custom := map[string][]store.CustomModel{}
	for _, m := range sn.CustomModels {
		if p, ok := st.Provider(m.ProviderAlias); ok && (m.Type == "" || m.Type == "llm") {
			custom[p.ID] = append(custom[p.ID], m)
		}
	}
	out := []modelGroup{}
	for _, ap := range provs {
		p, ok := st.Provider(ap.ID)
		if !ok || len(ap.Accounts) == 0 {
			continue
		}
		prefix := st.PrefixFor(p)
		g := modelGroup{Provider: ap.ID, Name: ap.Name, Prefix: prefix, Plugin: p.Plugin, Models: []pickerModel{}}
		seen := map[string]bool{}
		for _, m := range custom[p.ID] {
			if !seen[m.ID] {
				seen[m.ID] = true
				g.Models = append(g.Models, pickerModel{ID: prefix + "/" + m.ID, Name: firstNonEmpty(m.Name, m.ID), Custom: true})
			}
		}
		for _, m := range catalog.Models(ap.ID) {
			if !seen[m.ID] {
				seen[m.ID] = true
				g.Models = append(g.Models, pickerModel{ID: prefix + "/" + m.ID, Name: m.Name})
			}
		}
		out = append(out, g)
	}
	return out
}

// extraReachable reports whether a generated provider still has a model
// prefix that no custom endpoint uses.
func extraReachable(sn *store.Snapshot, c *catalog.Provider) bool {
	taken := map[string]bool{}
	for _, n := range sn.Nodes {
		taken[n.ID], taken[n.Prefix] = true, true
		for _, a := range n.Aliases {
			taken[a] = true
		}
	}
	for _, k := range append([]string{c.ID}, c.Aliases...) {
		if !taken[k] {
			return true
		}
	}
	return false
}
