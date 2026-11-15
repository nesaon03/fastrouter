package router

import (
	"crypto/sha256"
	"encoding/json"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/store"
)

// KeyLimit is an optional per-key limit from the config file (off by default).
type KeyLimit = config.KeyLimit

// KeyInfo is a local API key's identity and limits.
type KeyInfo struct {
	ID, Name string
	allow    []string // lower-case patterns; nil = unrestricted
	limiter  *bucket
}

func buildKeys(keys []store.APIKey, limits map[string]KeyLimit) map[[32]byte]*KeyInfo {
	out := map[[32]byte]*KeyInfo{}
	for _, k := range keys {
		if !k.Active || k.Key == "" {
			continue
		}
		ki := &KeyInfo{ID: k.ID, Name: k.Name}
		// 9Router's own per-key access list (access.restricted + allow).
		var access struct {
			Restricted bool     `json:"restricted"`
			Allow      []string `json:"allow"`
		}
		if json.Unmarshal(k.Access, &access) == nil && access.Restricted {
			ki.allow = lowerAll(access.Allow)
			if ki.allow == nil {
				ki.allow = []string{}
			}
		}
		lim, ok := limits[k.Name]
		if !ok {
			lim, ok = limits[k.ID]
		}
		if ok {
			if len(lim.Models) > 0 {
				ki.allow = append(ki.allow, lowerAll(lim.Models)...)
			}
			if lim.RPM > 0 {
				ki.limiter = newBucket(lim.RPM)
			}
		}
		out[sha256.Sum256([]byte(k.Key))] = ki
	}
	return out
}

func lowerAll(in []string) []string {
	var out []string
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

// Allows reports whether the key may call model (a combo name or prefix/model).
func (k *KeyInfo) Allows(model string) bool {
	if k == nil || k.allow == nil {
		return true
	}
	m := strings.ToLower(model)
	for _, a := range k.allow {
		if a == m {
			return true
		}
		if strings.Contains(a, "*") {
			if ok, _ := path.Match(a, m); ok {
				return true
			}
		}
	}
	return false
}

// Take consumes one request from the key's rate limit; ok=false means wait retryAfter.
func (k *KeyInfo) Take() (ok bool, retryAfter time.Duration) {
	if k == nil || k.limiter == nil {
		return true, 0
	}
	return k.limiter.take(time.Now())
}

// bucket is a token bucket refilled at rpm per minute, burst = rpm. Each key
// has its own lock; there is no shared limiter state.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // tokens per second
	last   time.Time
}

func newBucket(rpm int) *bucket {
	return &bucket{tokens: float64(rpm), max: float64(rpm), rate: float64(rpm) / 60, last: time.Now()}
}

func (b *bucket) take(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}
