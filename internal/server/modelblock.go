package server

import (
	"strings"
	"sync"
	"time"
)

// A 404/400 "model not found" from one account usually means that plan does
// not include the model, not that the credential is broken. Cooling the whole
// account down (the generic retryable path) would then take its other models
// out of rotation for 30s and keep paying one failed request per call for the
// missing one. This cache remembers the pair instead: the account is skipped
// for that model only, for a while, and any other model still uses it.
//
// The block is evidence-based and self-healing: it expires, and a successful
// response for the same pair clears it early (plans get upgraded).
const (
	modelBlockTTL = 6 * time.Hour
	modelBlockMax = 8192 // hard cap so a broken client cannot grow it forever
)

type modelBlock struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func modelBlockKey(account, model string) string { return account + "\x00" + model }

func (b *modelBlock) blocked(account, model string, now time.Time) bool {
	if account == "" || model == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.until[modelBlockKey(account, model)]
	if !ok {
		return false
	}
	if now.After(u) {
		delete(b.until, modelBlockKey(account, model))
		return false
	}
	return true
}

func (b *modelBlock) block(account, model string, now time.Time) {
	if account == "" || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until == nil {
		b.until = map[string]time.Time{}
	}
	if len(b.until) >= modelBlockMax {
		b.prune(now)
		if len(b.until) >= modelBlockMax {
			return // full of live entries: do not grow past the cap
		}
	}
	b.until[modelBlockKey(account, model)] = now.Add(modelBlockTTL)
}

func (b *modelBlock) clear(account, model string) {
	if account == "" || model == "" {
		return
	}
	b.mu.Lock()
	delete(b.until, modelBlockKey(account, model))
	b.mu.Unlock()
}

func (b *modelBlock) prune(now time.Time) {
	for k, u := range b.until {
		if now.After(u) {
			delete(b.until, k)
		}
	}
}

// modelUnsupported reports whether an upstream error means "this account does
// not serve this model" rather than "the request or the account is bad".
// It only fires on wording that names a model (or an Azure deployment / a
// Gemini api version), so a wrong base URL or a bad payload keeps the old
// account-cooldown behaviour.
func modelUnsupported(status int, msg string) bool {
	m := strings.ToLower(msg)
	if !strings.Contains(m, "model") && !strings.Contains(m, "deployment") && !strings.Contains(m, "api version") {
		return false
	}
	switch status {
	case 404:
		// A model-scoped 404: the endpoint exists, this id does not.
		return true
	case 400, 422:
		for _, p := range []string{"not found", "does not exist", "unknown", "invalid", "unsupported", "not supported", "no such"} {
			if strings.Contains(m, p) {
				return true
			}
		}
	case 403:
		for _, p := range []string{"does not have access", "do not have access", "no access", "not available", "not supported", "unsupported"} {
			if strings.Contains(m, p) {
				return true
			}
		}
	}
	return false
}
