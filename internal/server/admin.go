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

	"github.com/nesaon03/fastrouter/internal/catalog"
	"github.com/nesaon03/fastrouter/internal/discover"
	"github.com/nesaon03/fastrouter/internal/login"
	"github.com/nesaon03/fastrouter/internal/oauth"
	"github.com/nesaon03/fastrouter/internal/router"
	"github.com/nesaon03/fastrouter/internal/store"
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

// Fresh installs start with a known dashboard password and a one-time marker.
// An imported 9Router password always wins; clearing the password later is not
// undone by a restart.
const defaultDashboardPassword = "1234"

const (
	passwordInitKey    = "dashboardPasswordInitialized"
	passwordDefaultKey = "dashboardPasswordDefault"
)

func defaultPasswordActive(sn *store.Snapshot) bool {
	raw, ok := sn.Settings[passwordDefaultKey]
	return ok && strings.TrimSpace(string(raw)) == "true"
}

// EnsureDefaultPassword gives a fresh install the 1234 starting password once.
// It returns true when it set that password.
func (s *Server) EnsureDefaultPassword() (bool, error) {
	if s.st == nil {
		return false, nil
	}
	sn := s.state.Load().Snap
	if raw, ok := sn.Settings[passwordInitKey]; ok && strings.TrimSpace(string(raw)) == "true" {
		return false, nil
	}
	if sn.DashboardPassword != "" {
		return false, s.mutate(func(sn *store.Snapshot) error {
			sn.Settings[passwordInitKey] = json.RawMessage("true")
			sn.Settings[passwordDefaultKey] = json.RawMessage("false")
			return nil
		})
	}
	h, err := bcrypt.GenerateFromPassword([]byte(defaultDashboardPassword), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	set := false
	err = s.mutate(func(sn *store.Snapshot) error {
		if sn.DashboardPassword == "" {
			sn.DashboardPassword = string(h)
			sn.Settings[passwordDefaultKey] = json.RawMessage("true")
			set = true
		} else {
			sn.Settings[passwordDefaultKey] = json.RawMessage("false")
		}
		sn.Settings[passwordInitKey] = json.RawMessage("true")
		return nil
	})
	return set, err
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
	sn := s.state.Load().Snap
	pwSet := sn.DashboardPassword != ""
	out := map[string]any{"passwordSet": pwSet, "defaultPassword": defaultPasswordActive(sn), "authenticated": false, "version": Version}
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
	if l := len(in.New); l < 4 || l > 200 {
		adminErr(w, http.StatusBadRequest, "new password must be 4 to 200 characters")
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
	if err := s.mutate(func(sn *store.Snapshot) error {
		sn.DashboardPassword = string(h)
		sn.Settings[passwordInitKey] = json.RawMessage("true")
		sn.Settings[passwordDefaultKey] = json.RawMessage("false")
		return nil
	}); err != nil {
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
// checkPrefix validates a custom endpoint's model prefix: one lower-case word
// (no slash or spaces) that no other provider or endpoint answers to.
func checkPrefix(sn *store.Snapshot, raw, exceptNode string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case p == "":
		return "", bad("prefix is required")
	case strings.Contains(p, "/"):
		return "", bad("prefix can't contain a slash (it becomes prefix/model)")
	case strings.ContainsAny(p, " \t"):
		return "", bad("prefix can't contain spaces")
	case !prefixRe.MatchString(p):
		return "", bad("prefix must be lower-case letters, digits, - or _ (max 32)")
	case prefixTaken(sn, p, exceptNode):
		return "", bad("prefix %q is already used by another provider", p)
	}
	return p, nil
}

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
	Email      string `json:"email,omitempty"` // masked
	Refresh    bool   `json:"autoRefresh,omitempty"`
}

type adminProvider struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Prefix   string         `json:"prefix"`
	Aliases  []string       `json:"aliases"`
	Kind     string         `json:"kind"` // apikey | login | plugin | openai-compatible | anthropic-compatible
	Login    string         `json:"login,omitempty"`
	Flow     string         `json:"flow,omitempty"` // sign-in kind: browser | device | paste
	Risk     bool           `json:"risk,omitempty"` // show the Risk Notice
	Website  string         `json:"website,omitempty"`
	Regions  []string       `json:"regions,omitempty"`
	BaseURL  string         `json:"baseUrl,omitempty"`
	APIType  string         `json:"apiType,omitempty"`
	Model    string         `json:"defaultModel,omitempty"`
	Usable   int            `json:"usable"`
	Accounts []adminAccount `json:"accounts"`
	Color    string         `json:"color,omitempty"`
	KeyURL   string         `json:"keyUrl,omitempty"`
	Extra    bool           `json:"extra,omitempty"`
	Category string         `json:"category,omitempty"` // 9Router section: oauth | free | freeTier | apikey | webCookie | decision
	Priority int            `json:"priority,omitempty"`
	TextIcon string         `json:"textIcon,omitempty"`
	NoAuth   bool           `json:"noAuth,omitempty"`
	Decision bool           `json:"decision,omitempty"`      // also answers POST /v1/decisions
	DecModel string         `json:"decisionModel,omitempty"` // default id for that endpoint
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
	ID         string `json:"id"` // full routable name, prefix/model
	Name       string `json:"name"`
	Custom     bool   `json:"custom,omitempty"`
	Discovered bool   `json:"discovered,omitempty"` // listed by the provider at run time
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
		} else if oauth.Handles(c.Provider) {
			if oauth.AccessKey(c) != "" || oauth.Secret(c, "refreshToken") != "" || oauth.Secret(c, "serviceAccount") != "" {
				a.KeyHint = "signed in"
			}
			a.ExpiresAt, _ = c.Meta["expiresAt"].(string)
			exp := oauth.ExpiresAt(c)
			a.Expired = !exp.IsZero() && !now.Before(exp)
			a.Email = maskEmail(oauth.Meta(c, "email"))
			a.Refresh = oauth.Secret(c, "refreshToken") != "" || c.Provider == "github" || c.Provider == "vertex"
		} else {
			a.KeyHint = maskSecret(c.Secrets["apiKey"])
		}
		if psd, ok := c.Meta["providerSpecificData"].(map[string]any); ok {
			a.Region, _ = psd["region"].(string)
		}
		if e, ok := c.Meta["lastError"].(string); ok && len(e) < 2000 {
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
		if sp, ok := oauth.Get(c.ID); ok {
			p.Flow, p.Risk = sp.Kind, sp.Risk
		} else if c.Login == "codex" {
			p.Flow, p.Risk = "browser", true
		} else if c.Login == "glm" {
			p.Flow = "device"
		}
		p.Website = providerSite[c.ID]
		if d, ok := catalog.Info(c.ID); ok {
			p.Name, p.Color, p.KeyURL = d.Name, d.Color, d.KeyURL
			p.Category, p.Priority, p.TextIcon, p.NoAuth = d.Category, d.Priority, d.TextIcon, d.NoAuth
		}
		p.Decision, p.DecModel = c.DecisionURL != "", c.DecisionModel
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
			Kind: kind, BaseURL: n.BaseURL, APIType: n.APIType, Model: n.DefaultModel, Usable: usable[n.ID], Accounts: sortAccts(acctsBy[n.ID])})
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
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Kind          string   `json:"kind"`
		Strategy      string   `json:"strategy"`
		Models        []string `json:"models"`
		SmartPolicy   string   `json:"smartPolicy,omitempty"`
		RouterModel   string   `json:"routerModel,omitempty"`
		RouterEngine  string   `json:"routerEngine,omitempty"`
		DecisionModel string   `json:"decisionModel,omitempty"`
		JudgeModel    string   `json:"judgeModel,omitempty"`
		Threshold     float64  `json:"confidenceThreshold,omitempty"`
		Rounds        int      `json:"rounds,omitempty"`
		Roles         bool     `json:"roles,omitempty"`
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
			cb.RouterEngine, _ = o["routerEngine"].(string)
			cb.DecisionModel, _ = o["decisionModel"].(string)
			cb.JudgeModel, _ = o["judgeModel"].(string)
			cb.Threshold, _ = o["confidenceThreshold"].(float64)
			if f, ok := o["rounds"].(float64); ok {
				cb.Rounds = int(f)
			}
			cb.Roles, _ = o["roles"].(bool)
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
		Priority int    `json:"priority"`
		TextIcon string `json:"textIcon,omitempty"`
		KeyURL   string `json:"keyUrl,omitempty"`
		Reason   string `json:"reason"`
	}
	others := []other{}
	for id, d := range catalog.DisplayAll() {
		if !known[id] && d.LLM {
			others = append(others, other{id, d.Name, d.Color, d.Category, d.Priority, d.TextIcon, d.KeyURL, unsupportedReason(id, d)})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	adminJSON(w, http.StatusOK, map[string]any{
		"version":         Version,
		"uptimeSec":       int(time.Since(s.start).Seconds()),
		"passwordSet":     sn.DashboardPassword != "",
		"defaultPassword": defaultPasswordActive(sn),
		"providers":       provs,
		"combos":          combos,
		"keys":            keys,
		"models":          models,
		"modelGroups":     groups,
		"others":          others,
		"settings":        s.settingsView(sn),
		"warnings":        maskWarnings(sn.Warnings),
		"savers":          s.saverRunsView(),
		"savings":         s.saverSavingsView(),
		"autostart":       s.autostartState(),
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
	dec := map[string]any{"cacheSeconds": defaultDecisionCacheSeconds, "gates": map[string][]string{}}
	var ds struct {
		CacheSeconds *int                `json:"cacheSeconds"`
		Gates        map[string][]string `json:"gates"`
	}
	get("decisionSettings", &ds)
	if ds.CacheSeconds != nil {
		dec["cacheSeconds"] = *ds.CacheSeconds
	}
	if ds.Gates != nil {
		dec["gates"] = ds.Gates
	}
	compactKeep := 6
	get("compactKeepToolResults", &compactKeep)
	if compactKeep <= 0 {
		compactKeep = 6
	}
	return map[string]any{
		"rtkEnabled":             b("rtkEnabled", true),
		"cavemanEnabled":         b("cavemanEnabled", false),
		"cavemanLevel":           str("cavemanLevel", "full"),
		"ponytailEnabled":        b("ponytailEnabled", false),
		"ponytailLevel":          str("ponytailLevel", "full"),
		"compactEnabled":         b("compactEnabled", false),
		"compactKeepToolResults": compactKeep,
		"dedupeEnabled":          b("dedupeEnabled", false),
		"headroomEnabled":        b("headroomEnabled", false),
		"headroomUrl":            str("headroomUrl", "http://localhost:8787"),
		"headroomCodeAware":      b("headroomCodeAware", false),
		"headroomKompress":       b("headroomKompress", true),
		"customPromptEnabled":    b("customPromptEnabled", false),
		"customPrompt":           str("customPrompt", ""),
		"providerThinking":       thinking,
		"providerStrategies":     strategies,
		"disabledModels":         disabledModels(sn),
		"stickyRoundRobinLimit":  sticky,
		"vision":                 vis,
		"decision":               dec,
		"lockedByConfig":         locked,
	}
}

// ---- custom models ----

// modelIDRe allows what real model ids contain (OpenRouter embeds slashes).
var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,119}$`)

// handleModelCreate adds a custom model, the manual half of discovery: a model
// whose id does not appear in any list (a preview, an alias, a private
// deployment). It shows in the picker at once and routes like any other id.
func (s *Server) handleModelCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider  string `json:"provider"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		Vision    bool   `json:"vision"`
		Reasoning bool   `json:"reasoning"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	err := s.mutate(func(sn *store.Snapshot) error {
		st := s.state.Load()
		// A model can be added before the account exists, so a provider with no
		// connections yet still has to resolve: fall back to the compiled
		// catalog, which is where its id lives.
		id, plugin := "", false
		if p, ok := st.Provider(strings.TrimSpace(in.Provider)); ok {
			id, plugin = p.ID, p.Plugin
		} else if c, ok := catalog.Lookup(strings.TrimSpace(in.Provider)); ok {
			id, plugin = c.ID, c.Plugin
		} else {
			return bad("unknown provider %q", in.Provider)
		}
		if plugin {
			return bad("%s cannot be routed yet", id)
		}
		model := strings.TrimSpace(in.ID)
		if !modelIDRe.MatchString(model) {
			return bad("model id must be 1 to 120 characters of letters, digits and . _ : / + @ -")
		}
		typ := strings.TrimSpace(in.Type)
		if typ == "" {
			typ = "llm"
		}
		if typ != "llm" {
			return bad("only chat/LLM models can be added here")
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = model
		}
		if _, err := cleanName(name, "name"); err != nil {
			return err
		}
		for _, m := range sn.CustomModels {
			if m.ProviderAlias == id && m.ID == model {
				return bad("%s/%s already exists", id, model)
			}
		}
		caps, _ := json.Marshal(map[string]bool{"vision": in.Vision, "reasoning": in.Reasoning})
		sn.CustomModels = append(sn.CustomModels, store.CustomModel{
			ProviderAlias: id, ID: model, Type: typ, Name: name, Caps: caps})
		return nil
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
}

// handleModelDelete removes a custom model by provider id and model id. The id
// travels in the query string because a model id may contain slashes.
func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("id"))
	if provider == "" || model == "" {
		adminErr(w, http.StatusBadRequest, "provider and id are required")
		return
	}
	err := s.mutate(func(sn *store.Snapshot) error {
		kept := sn.CustomModels[:0]
		found := false
		for _, m := range sn.CustomModels {
			if m.ProviderAlias == provider && m.ID == model {
				found = true
				continue
			}
			kept = append(kept, m)
		}
		if !found {
			return errNotFound
		}
		sn.CustomModels = kept
		return nil
	})
	s.writeResult(w, err, map[string]bool{"ok": true})
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
	ModelID *string `json:"modelId"`
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
		p, err := checkPrefix(sn, *in.Prefix, n.ID)
		if err != nil {
			return err
		}
		n.Prefix = p
	}
	if in.ModelID != nil {
		m := strings.TrimSpace(*in.ModelID)
		if len(m) > 200 || strings.ContainsAny(m, "\t\r\n") {
			return bad("model ID must be one line, at most 200 characters")
		}
		n.DefaultModel = m
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
	Name          *string  `json:"name"`
	Strategy      *string  `json:"strategy"`
	Models        []string `json:"models"`
	SmartPolicy   *string  `json:"smartPolicy"`
	RouterModel   *string  `json:"routerModel"`
	RouterEngine  *string  `json:"routerEngine"`
	DecisionModel *string  `json:"decisionModel"`
	JudgeModel    *string  `json:"judgeModel"`
	Threshold     *float64 `json:"confidenceThreshold"`
	Rounds        *int     `json:"rounds"`
	Roles         *bool    `json:"roles"`
}

var comboStrategies = map[string]bool{"fallback": true, "round-robin": true, "smart": true, "fusion": true, "swarm": true, "cascade": true}
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
			return bad("strategy must be fallback, round-robin, smart, fusion, swarm or cascade")
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
	if in.Rounds != nil {
		if *in.Rounds < 1 || *in.Rounds > swarmMaxRounds {
			return bad("swarm rounds must be between 1 and %d", swarmMaxRounds)
		}
		o["rounds"] = *in.Rounds
	}
	if in.Roles != nil {
		if *in.Roles {
			o["roles"] = true
		} else {
			delete(o, "roles")
		}
	}
	for key, v := range map[string]*string{"routerModel": in.RouterModel, "judgeModel": in.JudgeModel, "decisionModel": in.DecisionModel} {
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
	if in.RouterEngine != nil {
		switch *in.RouterEngine {
		case "", "model":
			delete(o, "routerEngine")
		case "decision":
			o["routerEngine"] = "decision"
		default:
			return bad("router engine must be model or decision")
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
	RTKEnabled             *bool                   `json:"rtkEnabled"`
	CavemanEnabled         *bool                   `json:"cavemanEnabled"`
	CavemanLevel           *string                 `json:"cavemanLevel"`
	PonytailEnabled        *bool                   `json:"ponytailEnabled"`
	PonytailLevel          *string                 `json:"ponytailLevel"`
	CompactEnabled         *bool                   `json:"compactEnabled"`
	CompactKeepToolResults *int                    `json:"compactKeepToolResults"`
	DedupeEnabled          *bool                   `json:"dedupeEnabled"`
	HeadroomEnabled        *bool                   `json:"headroomEnabled"`
	HeadroomURL            *string                 `json:"headroomUrl"`
	HeadroomCodeAware      *bool                   `json:"headroomCodeAware"`
	HeadroomKompress       *bool                   `json:"headroomKompress"`
	CustomPromptEnabled    *bool                   `json:"customPromptEnabled"`
	CustomPrompt           *string                 `json:"customPrompt"`
	ProviderThinking       map[string]string       `json:"providerThinking"`
	ProviderStrategies     map[string]strategyView `json:"providerStrategies"`
	DisabledModels         map[string][]string     `json:"disabledModels"`
	StickyRoundRobinLimit  *int                    `json:"stickyRoundRobinLimit"`
	Vision                 *visionView             `json:"vision"`
	Decision               *decisionSettingsIn     `json:"decision"`
}

// decisionSettingsIn is a partial update: a nil field is left as it is.
type decisionSettingsIn struct {
	CacheSeconds *int                `json:"cacheSeconds"`
	Gates        map[string][]string `json:"gates"`
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
			"compactEnabled": in.CompactEnabled, "dedupeEnabled": in.DedupeEnabled,
			"headroomEnabled": in.HeadroomEnabled, "headroomCodeAware": in.HeadroomCodeAware, "headroomKompress": in.HeadroomKompress,
			"customPromptEnabled": in.CustomPromptEnabled} {
			if v != nil {
				set(k, *v)
			}
		}
		if in.CompactKeepToolResults != nil {
			if *in.CompactKeepToolResults < 1 || *in.CompactKeepToolResults > 50 {
				return bad("keep tool results must be 1 to 50")
			}
			set("compactKeepToolResults", *in.CompactKeepToolResults)
		}
		for k, v := range map[string]*string{"cavemanLevel": in.CavemanLevel, "ponytailLevel": in.PonytailLevel} {
			if v != nil {
				if !saverLevels[*v] {
					return bad("level must be lite, full or ultra")
				}
				set(k, *v)
			}
		}
		if in.CustomPrompt != nil {
			if len(*in.CustomPrompt) > 8000 {
				return bad("custom prompt must be at most 8000 characters")
			}
			set("customPrompt", *in.CustomPrompt)
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
		if in.DisabledModels != nil {
			cur := map[string][]string{}
			_ = json.Unmarshal(sn.Settings["disabledModels"], &cur)
			for id, ms := range in.DisabledModels {
				if !providerKnown(sn, id) {
					return bad("unknown provider %q", id)
				}
				if len(ms) > 2000 {
					return bad("too many models")
				}
				if len(ms) == 0 {
					delete(cur, id)
				} else {
					cur[id] = ms
				}
			}
			set("disabledModels", cur)
		}
		if in.Decision != nil {
			cur := map[string]any{}
			_ = json.Unmarshal(sn.Settings["decisionSettings"], &cur)
			if in.Decision.CacheSeconds != nil {
				n := *in.Decision.CacheSeconds
				if n < 0 || n > 86400 {
					return bad("decision cache seconds must be 0 to 86400")
				}
				cur["cacheSeconds"] = n
			}
			if in.Decision.Gates != nil {
				clean := map[string][]string{}
				if len(in.Decision.Gates) > 20 {
					return bad("at most 20 gate verdicts")
				}
				for verdict, phrases := range in.Decision.Gates {
					v := strings.TrimSpace(verdict)
					if v == "" {
						return bad("gate verdict cannot be empty")
					}
					if len(phrases) > 50 {
						return bad("at most 50 phrases per verdict")
					}
					var keep []string
					for _, p := range phrases {
						p = strings.TrimSpace(p)
						if p == "" {
							continue
						}
						if len(p) > 120 {
							return bad("gate phrase must be at most 120 characters")
						}
						keep = append(keep, p)
					}
					if len(keep) > 0 {
						clean[v] = keep
					}
				}
				cur["gates"] = clean
			}
			set("decisionSettings", cur)
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
func disabledModels(sn *store.Snapshot) map[string][]string {
	m := map[string][]string{}
	_ = json.Unmarshal(sn.Settings["disabledModels"], &m)
	return m
}

func modelGroups(st *router.State, sn *store.Snapshot, provs []adminProvider) []modelGroup {
	off := map[string]bool{}
	for p, ms := range disabledModels(sn) {
		for _, m := range ms {
			off[p+"/"+m] = true
		}
	}
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
			if !seen[m.ID] && !off[ap.ID+"/"+m.ID] {
				seen[m.ID] = true
				g.Models = append(g.Models, pickerModel{ID: prefix + "/" + m.ID, Name: m.Name})
			}
		}
		// Models the provider listed itself since this build (see discover_api.go).
		for _, dm := range sn.Discovered {
			if dm.Provider != ap.ID || dm.Status == store.DiscoveredDead || seen[dm.Model] || off[ap.ID+"/"+dm.Model] {
				continue
			}
			if discover.IsWireID(p.Upstream, dm.Model) {
				continue // the served id, not the public one
			}
			seen[dm.Model] = true
			g.Models = append(g.Models, pickerModel{ID: prefix + "/" + dm.Model, Name: dm.Model, Discovered: true})
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

// unsupportedReason says why a 9Router provider cannot be routed yet, and
// what to do instead when there is a workaround.
// providerSite is 9Router's "Sign up / Learn more" link for sign-in providers.
var providerSite = map[string]string{"antigravity": "https://antigravity.google", "claude": "https://claude.ai", "cline": "https://cline.bot",
	"clinepass": "https://cline.bot", "codebuddy-cn": "https://copilot.tencent.com", "codex": "https://chatgpt.com/codex",
	"gemini-cli": "https://github.com/google-gemini/gemini-cli", "github": "https://github.com/features/copilot", "glm": "https://z.ai",
	"grok-cli": "https://x.ai", "kilocode": "https://kilocode.ai", "kimi": "https://www.kimi.com/code", "ollama": "https://ollama.com",
	"vertex": "https://cloud.google.com/vertex-ai", "codebuddy-intl": "https://www.codebuddy.ai", "minimax-code": "https://agent.minimax.cn",
	"minimax-code-global": "https://agent.minimax.io", "iflow": "https://iflow.cn", "cursor": "https://cursor.com", "opencode": "https://opencode.ai"}

func unsupportedReason(id string, d catalog.Display) string {
	switch id {
	case "ollama-local":
		return "Local Ollama: add it as OpenAI Compatible with http://localhost:11434/v1."
	case "azure", "bedrock", "bedrock-xai", "vertex-partner", "cloudflare-ai":
		return "Needs a per-account endpoint. Add it as OpenAI Compatible with your deployment's base URL."
	case "commandcode":
		return "Uses its own wire protocol, not supported yet."
	case "kiro":
		return "Kiro speaks AWS's binary event-stream protocol (CodeWhisperer); not ported yet."
	case "qoder", "qoder-cn":
		return "Qoder uses its own encrypted request protocol; not ported yet."
	case "zed":
		return "Zed signs in with an RSA key exchange and its own completion format; not ported yet."
	case "xai":
		return "Use an xAI API key on the xAI card, or sign in with Grok CLI."
	case "gitlab":
		return "GitLab Duo needs your own GitLab OAuth application (client id); not supported yet."
	case "muse", "xiaomi-mimo":
		return "Sign-in not ported yet. A plain API key works through OpenAI Compatible."
	case "trae", "windsurf", "kimchi", "nous":
		return "Sign-in not ported yet."
	}
	switch d.Category {
	case "webCookie":
		return "Browser-session (cookie) providers are not supported."
	case "oauth", "free":
		return "Sign-in is not supported yet."
	}
	return "Not supported yet."
}

// handleProviderModels lists a provider's models for its detail page.
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st := s.state.Load()
	type model struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Upstream   string `json:"upstream,omitempty"`
		Custom     bool   `json:"custom,omitempty"`
		Discovered bool   `json:"discovered,omitempty"`
		Status     string `json:"status,omitempty"`
		Disabled   bool   `json:"disabled,omitempty"`
	}
	prefix := id
	var up map[string]string
	if p, ok := st.Provider(id); ok {
		prefix, up = st.PrefixFor(p), p.Upstream
	} else if c, ok := catalog.Lookup(id); ok {
		prefix, up = c.PrimaryAlias(), c.Upstream
	} else {
		adminErr(w, http.StatusNotFound, "unknown provider")
		return
	}
	off := map[string]bool{}
	for _, m := range disabledModels(st.Snap)[id] {
		off[m] = true
	}
	out, seen := []model{}, map[string]bool{}
	for _, m := range st.Snap.CustomModels {
		// Match the stored alias directly: a provider with no connections yet is
		// not in the routing table, and its custom models still belong here.
		if m.ProviderAlias != id || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, model{ID: m.ID, Name: firstNonEmpty(m.Name, m.ID), Custom: true, Disabled: off[m.ID]})
	}
	for _, m := range catalog.Models(id) {
		if !seen[m.ID] {
			seen[m.ID] = true
			u := up[m.ID]
			out = append(out, model{ID: m.ID, Name: m.Name, Upstream: u, Disabled: off[m.ID]})
		}
	}
	for _, dm := range s.discoveredFor(id) {
		if dm.Status == store.DiscoveredDead || seen[dm.Model] || discover.IsWireID(up, dm.Model) {
			continue
		}
		seen[dm.Model] = true
		out = append(out, model{ID: dm.Model, Name: dm.Model, Disabled: off[dm.Model], Discovered: true, Status: dm.Status})
	}
	if p, ok := st.Provider(id); ok && id == "cursor" { // the account's live model picker
		for _, a := range p.Pool.Accounts() {
			ms, err := oauth.CursorModels(r.Context(), a.Key(), a.Extra["machineId"], false)
			if err != nil {
				continue
			}
			for _, m := range ms {
				if !seen[m.ID] && !m.Hidden {
					seen[m.ID] = true
					out = append(out, model{ID: m.ID, Name: m.Name, Disabled: off[m.ID]})
				}
			}
			break
		}
	}
	thinking := ""
	var th map[string]struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(st.Snap.Settings["providerThinking"], &th)
	thinking = th[id].Mode
	adminJSON(w, http.StatusOK, map[string]any{"provider": id, "prefix": prefix, "models": out, "thinking": firstNonEmpty(thinking, "auto")})
}
