// Package oauth implements sign-in, token refresh and request shaping for the
// subscription ("OAuth") providers 9Router supports: Antigravity, Gemini CLI,
// Claude Code, GitHub Copilot, Kimi, Kilo Code, Cline, ClinePass, CodeBuddy,
// MiniMax Code, Grok CLI and Vertex AI (service account). Endpoints, client
// ids and header shapes mirror 9Router's src/lib/oauth and open-sse executors
// (MIT, decolua/9router). Codex and Z.ai stay in internal/login.
//
// Nothing here logs or returns credentials: errors carry status codes and the
// provider's error code only.
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

// Sign-in kinds the dashboard knows how to show.
const (
	KindBrowser = "browser" // open URL; the provider redirects to FastRouter's /callback (or paste that address)
	KindDevice  = "device"  // open URL and confirm the code; FastRouter polls
	KindPaste   = "paste"   // paste a token or key file
)

var (
	// ErrPending means a device sign-in is not confirmed yet.
	ErrPending = errors.New("authorization pending")
	// ErrReauth means the refresh token is dead; the account needs a new sign-in.
	ErrReauth = errors.New("sign-in expired; sign in again")
)

// Env is what a flow needs from the server.
type Env struct {
	Client      *http.Client
	RedirectURI string // http://localhost:<port>/callback
}

// Pending is an in-progress sign-in. Only Kind, URL, UserCode and Hint are shown.
type Pending struct {
	Kind, URL, UserCode, Hint string
	Interval                  time.Duration
	Expires                   time.Time
	State                     string

	verifier string
	redirect string
	data     map[string]string // device code, registered client, ...
}

// Result is a finished sign-in or refresh.
type Result struct {
	Name, Email string
	Secrets     map[string]string // accessToken, refreshToken, apiKey, ...
	ExpiresIn   int               // seconds; 0 = unknown / does not expire
	Meta        map[string]any    // top-level meta (projectId, ...)
	PSD         map[string]any    // providerSpecificData (non-secret)
}

// Spec is one provider's sign-in and refresh.
type Spec struct {
	ID, Kind string
	// Risk shows 9Router's Risk Notice: the provider's terms may not allow
	// third-party clients and accounts can be limited or banned.
	Risk bool
	// Rotates is true when a refresh hands out a new refresh token and burns
	// the old one (imported 9Router accounts are then only refreshed once expired).
	Rotates     bool
	RefreshLead time.Duration
	Start       func(ctx context.Context, e Env) (*Pending, error)
	Exchange    func(ctx context.Context, e Env, p *Pending, q url.Values) (*Result, error) // browser
	Poll        func(ctx context.Context, e Env, p *Pending) (*Result, error)               // device
	Paste       func(ctx context.Context, e Env, text string) (*Result, error)              // paste
	Refresh     func(ctx context.Context, e Env, c store.Connection) (*Result, error)
}

var specs = map[string]*Spec{}

func register(s *Spec) { specs[s.ID] = s }

// Get returns the spec for a provider id.
func Get(id string) (*Spec, bool) { s, ok := specs[id]; return s, ok }

// Handles reports whether provider id signs in through this package.
func Handles(id string) bool { _, ok := specs[id]; return ok }

// IDs lists the providers handled here.
func IDs() []string {
	out := make([]string, 0, len(specs))
	for id := range specs {
		out = append(out, id)
	}
	return out
}

// Apply stores a sign-in or refresh result on c.
func Apply(c *store.Connection, r *Result, now time.Time) {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	for k, v := range r.Secrets {
		if v != "" {
			c.Secrets[k] = v
		}
	}
	for k, v := range r.Meta {
		c.Meta[k] = v
	}
	if len(r.PSD) > 0 {
		psd, _ := c.Meta["providerSpecificData"].(map[string]any)
		if psd == nil {
			psd = map[string]any{}
		}
		for k, v := range r.PSD {
			psd[k] = v
		}
		c.Meta["providerSpecificData"] = psd
	}
	if r.ExpiresIn > 0 {
		c.Meta["expiresIn"] = r.ExpiresIn
		c.Meta["expiresAt"] = now.Add(time.Duration(r.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	c.Meta["lastRefreshAt"] = now.UTC().Format(time.RFC3339Nano)
	if r.Email != "" {
		c.Meta["email"] = r.Email
	}
	if c.Name == "" {
		c.Name = firstNonEmpty(r.Name, r.Email)
	}
	if c.AuthType == "" {
		c.AuthType = "oauth"
	}
	c.Status = "ok"
	delete(c.Meta, "lastError")
}

// Secret reads a credential stored at the top level or under providerSpecificData.
func Secret(c store.Connection, k string) string {
	return firstNonEmpty(c.Secrets[k], c.Secrets["providerSpecificData."+k])
}

// Meta reads a non-secret string at the top level or under providerSpecificData.
func Meta(c store.Connection, k string) string {
	if s := anyStr(c.Meta[k]); s != "" {
		return s
	}
	psd, _ := c.Meta["providerSpecificData"].(map[string]any)
	return anyStr(psd[k])
}

// ExpiresAt is the access token expiry ("" or unparsable = unknown).
func ExpiresAt(c store.Connection) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, anyStr(c.Meta["expiresAt"]))
	return t
}

// AccessKey is the credential sent upstream for an account of provider c.Provider.
func AccessKey(c store.Connection) string {
	switch c.Provider {
	case "github":
		return Secret(c, "copilotToken")
	case "vertex":
		return Secret(c, "accessToken") // minted from the service account by the refresher
	case "iflow":
		return firstNonEmpty(Secret(c, "apiKey"), Secret(c, "accessToken"))
	}
	return firstNonEmpty(Secret(c, "accessToken"), Secret(c, "apiKey"))
}

// Extra is the non-secret per-account data request shaping needs.
func Extra(c store.Connection) map[string]string {
	m := map[string]string{}
	for _, k := range []string{"projectId", "deviceId", "orgId", "email", "userId", "location"} {
		if v := Meta(c, k); v != "" {
			m[k] = v
		}
	}
	if m["projectId"] == "" && c.Provider == "vertex" {
		m["projectId"] = Meta(c, "project_id")
	}
	return m
}

// ---- small HTTP helpers ----

// httpError carries a status and the provider's error code, never the body.
type httpError struct {
	Status int
	Code   string
}

func (h *httpError) Error() string {
	if h.Code != "" {
		return fmt.Sprintf("HTTP %d (%s)", h.Status, h.Code)
	}
	return fmt.Sprintf("HTTP %d", h.Status)
}

// errCode pulls a short, non-secret error code out of an error body.
func errCode(b []byte) string {
	var v struct {
		Error any    `json:"error"`
		Code  any    `json:"code"`
		Msg   string `json:"error_description"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	switch e := v.Error.(type) {
	case string:
		return clip(e, 60)
	case map[string]any:
		if s := anyStr(e["status"]); s != "" {
			return clip(s, 60)
		}
		return clip(anyStr(e["code"]), 60)
	}
	return clip(anyStr(v.Code), 60)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func do(ctx context.Context, c *http.Client, method, u string, body io.Reader, hdr map[string]string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, fmt.Errorf("network error: %w", stripURL(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		if jerr := json.Unmarshal(b, out); jerr != nil && resp.StatusCode < 300 {
			return resp.StatusCode, errors.New("unexpected (non-JSON) answer")
		}
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, &httpError{resp.StatusCode, errCode(b)}
	}
	return resp.StatusCode, nil
}

// stripURL drops the request URL (which may carry a code or token) from a client error.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func postForm(ctx context.Context, c *http.Client, u string, form url.Values, hdr map[string]string, out any) (int, error) {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range hdr {
		h[k] = v
	}
	return do(ctx, c, http.MethodPost, u, strings.NewReader(form.Encode()), h, out)
}

func postJSON(ctx context.Context, c *http.Client, u string, in any, hdr map[string]string, out any) (int, error) {
	b, _ := json.Marshal(in)
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		h[k] = v
	}
	return do(ctx, c, http.MethodPost, u, bytes.NewReader(b), h, out)
}

func getJSON(ctx context.Context, c *http.Client, u string, hdr map[string]string, out any) (int, error) {
	return do(ctx, c, http.MethodGet, u, nil, hdr, out)
}

// tokenResp is the standard OAuth token answer.
type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

// refreshErr maps a failed refresh to ErrReauth when the grant is dead.
func refreshErr(err error) error {
	var he *httpError
	if errors.As(err, &he) && (he.Status == 400 || he.Status == 401) {
		c := strings.ToLower(he.Code)
		if c == "invalid_grant" || strings.Contains(c, "expired") || strings.Contains(c, "revoked") || strings.Contains(c, "invalid_token") || he.Status == 401 {
			return ErrReauth
		}
	}
	return err
}

// pkce returns a verifier and its S256 challenge.
func pkce(n int) (verifier, challenge string) {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// jwtClaim reads one string claim from a JWT payload (display only, unverified).
func jwtClaim(tok, k string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return anyStr(m[k])
}

func anyStr(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprint(x)
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// deviceDefaults fills a device Pending's timers.
func deviceDefaults(p *Pending, interval, expires int) *Pending {
	if interval <= 0 {
		interval = 5
	}
	if expires <= 0 {
		expires = 600
	}
	p.Kind = KindDevice
	p.Interval = time.Duration(interval) * time.Second
	p.Expires = time.Now().Add(time.Duration(expires) * time.Second)
	return p
}

// RedirectURI is the redirect the sign-in was started with.
func (p *Pending) RedirectURI() string { return p.redirect }
