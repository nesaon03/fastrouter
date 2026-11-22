package login

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/nesaon03/fastrouter/internal/store"
)

// Provider ids handled by this package.
const (
	Codex = "codex"
	GLM   = "glm"
)

// Status values for login accounts.
const (
	StatusOK          = "ok"
	StatusNeedsReauth = "needs-reauth"
	// OriginFastRouter marks accounts signed in through FastRouter itself.
	// Accounts imported from 9Router share their refresh token with the running
	// 9Router; see Refresher.RefreshImported.
	OriginFastRouter = "fastrouter"
)

// Handles reports whether provider is an official-login provider.
func Handles(provider string) bool { return provider == Codex || provider == GLM }

func metaTime(c store.Connection, k string) time.Time {
	s, _ := c.Meta[k].(string)
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// AccessKey is the credential sent upstream: the Codex access token, or the
// GLM coding-plan key (an API-key GLM account uses apiKey).
func AccessKey(c store.Connection) string {
	return firstNonEmpty(c.Secrets["accessToken"], c.Secrets["apiKey"])
}

// CodexAccountID is the ChatGPT account id for the ChatGPT-Account-ID header.
func CodexAccountID(c store.Connection) string {
	psd, _ := c.Meta["providerSpecificData"].(map[string]any)
	for _, k := range []string{"workspaceId", "chatgptAccountId", "accountId"} {
		if s, _ := psd[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// Expired reports whether a Codex access token is past its expiry.
func Expired(c store.Connection, now time.Time) bool {
	exp := metaTime(c, "expiresAt")
	return c.Provider == Codex && !exp.IsZero() && !now.Before(exp)
}

// CodexDue reports whether a Codex account should refresh now: the token
// expires within CodexRefreshLead, or the last refresh is older than
// CodexMaxRefreshAge (9Router shouldRefreshCredentials).
func CodexDue(c store.Connection, now time.Time) bool {
	if c.Provider != Codex || c.Secrets["refreshToken"] == "" {
		return false
	}
	if exp := metaTime(c, "expiresAt"); !exp.IsZero() && exp.Sub(now) < CodexRefreshLead {
		return true
	}
	last := metaTime(c, "lastRefreshAt")
	return last.IsZero() || now.Sub(last) >= CodexMaxRefreshAge
}

// ApplyCodexTokens stores tokens and identity on c.
func ApplyCodexTokens(c *store.Connection, t *Tokens, now time.Time) {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	c.Secrets["accessToken"] = t.AccessToken
	if t.RefreshToken != "" {
		c.Secrets["refreshToken"] = t.RefreshToken
	}
	if t.IDToken != "" {
		c.Secrets["idToken"] = t.IDToken
	}
	if t.ExpiresIn > 0 {
		c.Meta["expiresIn"] = t.ExpiresIn
		c.Meta["expiresAt"] = now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	c.Meta["lastRefreshAt"] = now.UTC().Format(time.RFC3339Nano)
	if acct := ParseCodexIDToken(c.Secrets["idToken"]); acct.AccountID != "" || acct.Email != "" {
		psd, _ := c.Meta["providerSpecificData"].(map[string]any)
		if psd == nil {
			psd = map[string]any{}
		}
		if acct.AccountID != "" {
			psd["chatgptAccountId"] = acct.AccountID
		}
		if acct.PlanType != "" {
			psd["chatgptPlanType"] = acct.PlanType
		}
		c.Meta["providerSpecificData"] = psd
		if acct.Email != "" {
			c.Meta["email"] = acct.Email
			if c.Name == "" {
				c.Name = acct.Email
			}
		}
	}
	c.Status = StatusOK
	delete(c.Meta, "lastError")
}

// ApplyGLM stores a finished Z.ai sign-in on c.
func ApplyGLM(c *store.Connection, r *GLMResult) {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	c.Secrets["accessToken"] = r.APIKey
	if r.JWT != "" {
		c.Secrets["providerSpecificData.zcodeJwtToken"] = r.JWT
	}
	if r.BusinessToken != "" {
		c.Secrets["providerSpecificData.zaiBusinessToken"] = r.BusinessToken
	}
	psd, _ := c.Meta["providerSpecificData"].(map[string]any)
	if psd == nil {
		psd = map[string]any{}
	}
	psd["authMethod"] = "cli_poll"
	if r.UserID != "" {
		psd["userId"] = r.UserID
	}
	c.Meta["providerSpecificData"] = psd
	if r.Email != "" {
		c.Meta["email"] = r.Email
		if c.Name == "" {
			c.Name = r.Email
		}
	}
	if c.Name == "" {
		c.Name = firstNonEmpty(r.Name, "Z.ai account")
	}
	c.Status = StatusOK
	delete(c.Meta, "lastError")
}

// Store is what the refresher needs from the database.
type Store interface {
	Load() (*store.Snapshot, error)
	SaveConnection(store.Connection) error
}

// Refresher refreshes Codex access tokens in the background from their
// stored refresh tokens. GLM's coding-plan key does not expire on a timer
// and Z.ai has no refresh grant, so GLM accounts are only re-signed-in.
type Refresher struct {
	Store     Store
	Client    *http.Client
	Endpoints Endpoints
	// RefreshImported also refreshes accounts imported from 9Router. Off by
	// default: OpenAI rotates refresh tokens, so refreshing a token the running
	// 9Router also holds signs that 9Router out.
	RefreshImported bool
	// OnUpdate is called with every changed connection (new key or status).
	OnUpdate func(store.Connection)
	Now      func() time.Time

	mu sync.Mutex // one refresh pass at a time
}

// Eligible reports whether the refresher may refresh c.
func (r *Refresher) Eligible(c store.Connection) bool {
	if c.Provider != Codex || !c.Active || c.Status == StatusNeedsReauth {
		return false
	}
	origin, _ := c.Meta["origin"].(string)
	return origin == OriginFastRouter || r.RefreshImported
}

// RunOnce refreshes every due account and returns how many were refreshed.
func (r *Refresher) RunOnce(ctx context.Context) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	snap, err := r.Store.Load()
	if err != nil {
		log.Printf("token refresh: load failed: %v", err)
		return 0
	}
	n := 0
	for _, c := range snap.Connections {
		if !r.Eligible(c) || !CodexDue(c, now()) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		t, err := r.Endpoints.CodexRefresh(cctx, r.Client, c.Secrets["refreshToken"])
		cancel()
		switch {
		case errors.Is(err, ErrReauth):
			c.Status = StatusNeedsReauth
			c.Meta["lastError"] = "refresh token no longer valid; sign in again from the dashboard"
			log.Printf("token refresh: codex account %s needs a new sign-in", c.ID)
		case err != nil:
			log.Printf("token refresh: codex account %s: %v (will retry)", c.ID, err) // error text never contains tokens
			continue
		default:
			ApplyCodexTokens(&c, t, now())
			n++
			log.Printf("token refresh: codex account %s refreshed", c.ID)
		}
		if err := r.Store.SaveConnection(c); err != nil {
			log.Printf("token refresh: save failed for %s: %v", c.ID, err)
			continue
		}
		if r.OnUpdate != nil {
			r.OnUpdate(c)
		}
	}
	return n
}

// Run refreshes on start and then every interval until ctx ends.
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
