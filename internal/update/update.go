// Package update reports whether the published npm package is newer than the
// running binary.
//
// The npm registry is the release channel: the maintainer publishes
// `fastrouter@x.y.z`, users run `npm i -g fastrouter@latest`, and the dashboard
// shows the command. This package only reads the public `latest` document; it
// never installs anything.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is the npm registry's latest-version document.
const DefaultURL = "https://registry.npmjs.org/fastrouter/latest"

// Command is what the dashboard shows and copies.
const Command = "npm i -g fastrouter@latest"

// Info is one check result, ready for JSON.
type Info struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	Command         string    `json:"command"`
	CheckedAt       time.Time `json:"checkedAt,omitempty"`
	Error           string    `json:"error,omitempty"`
}

// Checker caches the registry answer so the dashboard cannot hammer it.
type Checker struct {
	URL      string
	Client   *http.Client
	TTL      time.Duration // how long a successful answer is trusted
	ErrorTTL time.Duration // how long a failure is remembered

	mu     sync.Mutex
	at     time.Time
	latest string
	err    string
}

// New returns a checker with production defaults.
func New() *Checker {
	return &Checker{
		URL:      DefaultURL,
		Client:   &http.Client{Timeout: 5 * time.Second},
		TTL:      6 * time.Hour,
		ErrorTTL: 15 * time.Minute,
	}
}

// Check returns the latest known version, fetching at most once per TTL.
func (c *Checker) Check(ctx context.Context, current string) Info {
	c.mu.Lock()
	if !c.at.IsZero() {
		ttl := c.TTL
		if c.err != "" {
			ttl = c.ErrorTTL
		}
		if time.Since(c.at) < ttl {
			info := c.infoLocked(current)
			c.mu.Unlock()
			return info
		}
	}
	c.mu.Unlock()

	latest, err := c.fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = time.Now()
	if err != nil {
		c.err = err.Error()
	} else {
		c.latest, c.err = latest, ""
	}
	return c.infoLocked(current)
}

func (c *Checker) infoLocked(current string) Info {
	info := Info{Current: current, Latest: c.latest, Command: Command, CheckedAt: c.at, Error: c.err}
	if c.latest != "" {
		info.UpdateAvailable = Compare(c.latest, current) > 0
	}
	return info
}

func (c *Checker) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fastrouter/"+currentUserAgent)
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry answered HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("npm registry answer is not JSON: %w", err)
	}
	v := strings.TrimSpace(doc.Version)
	if v == "" {
		return "", fmt.Errorf("npm registry answer has no version")
	}
	return v, nil
}

// currentUserAgent is replaced by the server at startup so the registry sees a
// real version; a plain package test still has a valid value.
var currentUserAgent = "dev"

// SetUserAgent records the running version in registry requests.
func SetUserAgent(v string) {
	if strings.TrimSpace(v) != "" {
		currentUserAgent = v
	}
}

// Compare orders two version strings: -1, 0 or 1. It compares the numeric
// major.minor.patch parts and ignores a leading "v" and any pre-release or
// build suffix, which is enough for the release channel.
func Compare(a, b string) int {
	pa, pb := parse(a), parse(b)
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func parse(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}
