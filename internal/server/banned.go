package server

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/oauth"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/store"
)

// An account is only called banned when the provider says, in plain words, that
// the whole account was disabled for a terms-of-service violation. Anything
// else (per-model or per-project 403s, quota, region) keeps its real error.
func tosBanned(status int, msg string) bool {
	if status != 403 {
		return false
	}
	l := strings.ToLower(msg)
	return strings.Contains(l, "violation of terms of service") && strings.Contains(l, "disabled")
}

// Probe models for confirming an account-level block on every Code Assist host.
var banProbeModel = map[string]string{"antigravity": "gemini-3.8-flash-low(low)", "gemini-cli": "gemini-3-flash-preview"}

// confirmBanned repeats the request the way 9Router could (every host it knows
// for the provider). It is banned only if every host gives the same ToS block.
// Providers without alternative hosts have nothing more to try.
func (s *Server) confirmBanned(ctx context.Context, provider string, a *router.Account) bool {
	if !oauth.HasCodeAssistHosts(provider) {
		return true
	}
	res := oauth.ProbeCodeAssist(ctx, s.client, provider, a.Key(), oauth.Account{ID: a.ID, Extra: a.Extra}, banProbeModel[provider])
	if len(res) == 0 {
		return false
	}
	for _, r := range res {
		if !tosBanned(r.Status, r.Message) {
			return false
		}
	}
	return true
}

var banChecks sync.Map // account id -> struct{} while a background check runs

// noteBan is called from the proxy on a ToS-looking 403: it confirms off the
// hot path and, when confirmed, marks the account banned.
func (s *Server) noteBan(provider string, a *router.Account, msg string) {
	if s.st == nil {
		return
	}
	if _, busy := banChecks.LoadOrStore(a.ID, struct{}{}); busy {
		return
	}
	msg = scrubKey(msg, a.Key())
	go func() {
		defer banChecks.Delete(a.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if s.confirmBanned(ctx, provider, a) {
			s.setAccountStatus(a.ID, "banned", msg)
		}
	}()
}

// setAccountStatus saves a connection's status ("banned" or "ok") and reloads routing.
func (s *Server) setAccountStatus(id, status, msg string) {
	if s.st == nil {
		return
	}
	snap, err := s.st.Load()
	if err != nil {
		return
	}
	for _, c := range snap.Connections {
		if c.ID != id || c.Status == status {
			continue
		}
		if c.Meta == nil {
			c.Meta = map[string]any{}
		}
		c.Status = status
		if status == "banned" {
			c.Meta["lastError"] = msg
			c.Meta["bannedAt"] = time.Now().UTC().Format(time.RFC3339)
		} else {
			delete(c.Meta, "lastError")
			delete(c.Meta, "bannedAt")
		}
		if err := s.st.SaveConnection(c); err != nil {
			log.Printf("account %s: saving status %s failed: %v", id, status, err)
			return
		}
		log.Printf("account %s marked %s", id, status)
		_ = s.ReloadFromStore()
		return
	}
}

// withBannedRoutable returns a routing table where the banned accounts that
// match the filter are usable again, so a manual retest can reach them.
func withBannedRoutable(st *router.State, provider, account string, limits map[string]config.KeyLimit) *router.State {
	if st.Snap == nil {
		return st
	}
	var hit bool
	cp := *st.Snap
	cp.Connections = make([]store.Connection, len(st.Snap.Connections))
	for i, c := range st.Snap.Connections {
		if c.Status == "banned" && (provider == "" || c.Provider == provider) && (account == "" || c.ID == account) {
			c.Status, hit = "ok", true
		}
		cp.Connections[i] = c
	}
	if !hit {
		return st
	}
	return router.Build(&cp, limits)
}

// scrubKey removes the account's own credential from a provider message.
func scrubKey(msg, key string) string {
	if len(key) >= 6 {
		msg = strings.ReplaceAll(msg, key, "[redacted]")
	}
	return msg
}
