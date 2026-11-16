// Package login implements the official-login providers: Codex (ChatGPT
// sign-in, OAuth PKCE) and GLM (Z.ai sign-in, ZCode CLI polling flow).
// Endpoints, client ids and request shaping mirror 9Router's codex and glm
// providers (MIT, see LICENSE-9router). No browser sessions, fingerprinting
// or traffic interception: only the providers' own OAuth flows and APIs.
package login

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
)

// Codex OAuth and API settings (9Router open-sse/providers/registry/codex.js).
const (
	CodexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	CodexTokenURL     = "https://auth.openai.com/oauth/token"
	CodexScope        = "openid profile email offline_access"
	CodexCallbackPath = "/auth/callback"
	CodexDefaultPort  = 1455
	CodexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	CodexCLIVersion   = "0.167.1"

	// CodexRefreshLead refreshes an access token this long before it expires.
	// 9Router uses 10 minutes: OpenAI rotates refresh tokens, so refreshing
	// early on every call would churn (and reusing a rotated token logs the
	// whole session out).
	CodexRefreshLead = 10 * time.Minute
	// CodexMaxRefreshAge refreshes proactively when the last refresh is older (8 days).
	CodexMaxRefreshAge = 8 * 24 * time.Hour
)

// Tokens is the result of a sign-in or refresh. Never log it.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// CodexAccount is the non-secret identity in a Codex id token.
type CodexAccount struct {
	Email, AccountID, PlanType string
}

// PKCE returns a random verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomID returns n random bytes as hex.
func RandomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CodexRedirectURI is the redirect registered for the Codex client.
func CodexRedirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d%s", port, CodexCallbackPath)
}

// CodexAuthURL builds the ChatGPT sign-in URL.
func CodexAuthURL(redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", CodexClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", CodexScope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "codex_cli_rs")
	q.Set("state", state)
	return CodexAuthorizeURL + "?" + q.Encode()
}

// Endpoints can be pointed at test servers.
type Endpoints struct {
	CodexToken                                 string
	GLMInit, GLMPoll, GLMBusinessLogin, GLMAPI string
}

// Default endpoints.
var Default = Endpoints{
	CodexToken:       CodexTokenURL,
	GLMInit:          "https://zcode.z.ai/api/v1/oauth/cli/init",
	GLMPoll:          "https://zcode.z.ai/api/v1/oauth/cli/poll",
	GLMBusinessLogin: "https://api.z.ai/api/auth/z/login",
	GLMAPI:           "https://api.z.ai",
}

// CodexExchange trades an authorization code for tokens.
func (e Endpoints) CodexExchange(ctx context.Context, c *http.Client, code, verifier, redirectURI string) (*Tokens, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {CodexClientID}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.CodexToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return doTokens(c, req)
}

// ErrReauth means the refresh token is dead and the user must sign in again.
var ErrReauth = errors.New("refresh token expired or already used; sign in again")

// CodexRefresh exchanges a refresh token (9Router refreshCodexToken). A dead
// refresh token returns ErrReauth; other failures are worth retrying later.
func (e Endpoints) CodexRefresh(ctx context.Context, c *http.Client, refreshToken string) (*Tokens, error) {
	body, _ := json.Marshal(map[string]string{"client_id": CodexClientID, "grant_type": "refresh_token", "refresh_token": refreshToken})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.CodexToken, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	t, err := doTokens(c, req)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

type httpError struct {
	status int
	body   string
}

func (h *httpError) Error() string { return fmt.Sprintf("HTTP %d", h.status) }

func doTokens(c *http.Client, req *http.Request) (*Tokens, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if permanentOAuthError(resp.StatusCode, string(b)) {
			return nil, ErrReauth
		}
		return nil, &httpError{resp.StatusCode, string(b)}
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil || t.AccessToken == "" {
		return nil, errors.New("token response had no access_token")
	}
	return &t, nil
}

// permanentOAuthError mirrors 9Router classifyOAuthRefreshError.
func permanentOAuthError(status int, body string) bool {
	if status != 400 && status != 401 {
		return false
	}
	b := strings.ToLower(body)
	for _, m := range []string{"refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated", "invalid_grant", "token_expired"} {
		if strings.Contains(b, m) {
			return true
		}
	}
	return false
}

// ParseCodexIDToken reads the account claims from an id token (no signature
// check: the token came straight from OpenAI over TLS and is only used for
// display and the account header).
func ParseCodexIDToken(idToken string) CodexAccount {
	var claims struct {
		Email     string `json:"email"`
		AccountID string `json:"account_id"`
		PlanType  string `json:"plan_type"`
		Auth      struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return CodexAccount{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil || json.Unmarshal(raw, &claims) != nil {
		return CodexAccount{}
	}
	a := CodexAccount{Email: claims.Email, AccountID: claims.Auth.AccountID, PlanType: claims.Auth.PlanType}
	if a.AccountID == "" {
		a.AccountID = claims.AccountID
	}
	if a.PlanType == "" {
		a.PlanType = claims.PlanType
	}
	return a
}
