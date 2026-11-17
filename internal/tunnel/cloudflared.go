// Package tunnel exposes FastRouter outside this computer the way 9Router
// does: a Cloudflare quick tunnel (cloudflared, no account) and Tailscale
// Serve / Funnel. Nothing is installed or started unless the user asks.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Cloudflared runs one quick tunnel to http://127.0.0.1:Port and restarts it
// if it exits or stops answering while enabled.
type Cloudflared struct {
	Dir  string // FastRouter data dir; downloads go to Dir/bin
	Port int

	mu        sync.Mutex
	enabled   bool
	state     string // off | starting | running | reconnecting | error
	url       string
	errMsg    string
	since     time.Time
	restarts  int
	reachable bool
	lastOK    time.Time
	cmd       *exec.Cmd
	gen       int // bumps on every start/stop so stale goroutines exit
	dl        download
	logTail   string
}

type download struct {
	Active   bool   `json:"active"`
	Progress int    `json:"progress"`
	Error    string `json:"error,omitempty"`
}

// CloudflaredStatus is what the dashboard shows.
type CloudflaredStatus struct {
	Installed bool      `json:"installed"`
	Path      string    `json:"path,omitempty"`
	Source    string    `json:"source,omitempty"` // fastrouter | path | 9router | system
	Enabled   bool      `json:"enabled"`
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"`
	Reachable bool      `json:"reachable"`
	Error     string    `json:"error,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	Restarts  int       `json:"restarts"`
	Download  download  `json:"download"`
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

// Find looks for cloudflared: FastRouter's own copy, PATH, 9Router's copy,
// then the usual install folders.
func (c *Cloudflared) Find() (path, source string) {
	own := filepath.Join(c.Dir, "bin", binName())
	if validBinary(own) {
		return own, "fastrouter"
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, "path"
	}
	var cands [][2]string
	if runtime.GOOS == "windows" {
		cands = append(cands, [2]string{filepath.Join(os.Getenv("APPDATA"), "9router", "bin", binName()), "9router"},
			[2]string{filepath.Join(os.Getenv("ProgramFiles(x86)"), "cloudflared", binName()), "system"},
			[2]string{filepath.Join(os.Getenv("ProgramFiles"), "cloudflared", binName()), "system"})
	} else {
		home, _ := os.UserHomeDir()
		cands = append(cands, [2]string{filepath.Join(home, ".9router", "bin", binName()), "9router"},
			[2]string{"/usr/local/bin/cloudflared", "system"}, [2]string{"/opt/homebrew/bin/cloudflared", "system"}, [2]string{"/usr/bin/cloudflared", "system"})
	}
	for _, x := range cands {
		if validBinary(x[0]) {
			return x[0], x[1]
		}
	}
	return "", ""
}

// validBinary: present, over 1 MB and an executable for this OS.
func validBinary(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 1<<20 {
		return false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		return magic[0] == 'M' && magic[1] == 'Z'
	case "darwin":
		return (magic[0] == 0xcf || magic[0] == 0xce) && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe
	}
	return string(magic) == "\x7fELF"
}

// DownloadURL is Cloudflare's latest release for this platform.
func DownloadURL() (string, error) {
	base := "https://github.com/cloudflare/cloudflared/releases/latest/download/"
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return base + "cloudflared-windows-amd64.exe", nil
	case "windows/386", "windows/arm64":
		return base + "cloudflared-windows-386.exe", nil
	case "linux/amd64":
		return base + "cloudflared-linux-amd64", nil
	case "linux/arm64":
		return base + "cloudflared-linux-arm64", nil
	}
	return "", fmt.Errorf("no cloudflared download for %s/%s; install it yourself (brew install cloudflared)", runtime.GOOS, runtime.GOARCH)
}

// Install downloads cloudflared into Dir/bin in the background. Only called
// when the user presses Install.
func (c *Cloudflared) Install(client *http.Client) error {
	u, err := DownloadURL()
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.dl.Active {
		c.mu.Unlock()
		return errors.New("already downloading")
	}
	c.dl = download{Active: true}
	c.mu.Unlock()
	go func() {
		err := c.fetch(client, u)
		c.mu.Lock()
		c.dl.Active = false
		if err != nil {
			c.dl.Error = err.Error()
		} else {
			c.dl.Progress = 100
		}
		c.mu.Unlock()
	}()
	return nil
}

func (c *Cloudflared) fetch(client *http.Client, u string) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	dir := filepath.Join(c.Dir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, binName()+".tmp")
	defer os.Remove(tmp)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	resp, err := client.Do(req) // GitHub redirects to its CDN; the default client follows
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	total, done := resp.ContentLength, int64(0)
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			done += int64(n)
			if total > 0 {
				c.mu.Lock()
				c.dl.Progress = int(done * 99 / total)
				c.mu.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !validBinary(tmp) {
		return errors.New("the download is not a valid cloudflared binary")
	}
	return os.Rename(tmp, filepath.Join(dir, binName()))
}

// Enable starts the tunnel and keeps it up until Disable.
func (c *Cloudflared) Enable() error {
	bin, _ := c.Find()
	if bin == "" {
		return errors.New("cloudflared is not installed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enabled && c.cmd != nil {
		return nil
	}
	c.enabled, c.restarts, c.errMsg = true, 0, ""
	c.startLocked(bin)
	return nil
}

// Disable stops the tunnel.
func (c *Cloudflared) Disable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = false
	c.gen++
	c.killLocked()
	c.state, c.url, c.errMsg, c.reachable = "off", "", "", false
}

func (c *Cloudflared) killLocked() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.cmd = nil
}

var quickURL = regexp.MustCompile(`https://([a-z0-9-]+)\.trycloudflare\.com`)

func (c *Cloudflared) startLocked(bin string) {
	c.gen++
	gen := c.gen
	if c.url == "" {
		c.state = "starting"
	} else {
		c.state = "reconnecting"
	}
	c.since = time.Now()
	// An empty config keeps a ~/.cloudflared/config.yml from changing the tunnel.
	cfgDir, _ := os.MkdirTemp("", "fastrouter-cloudflared-")
	cfg := filepath.Join(cfgDir, "config.yml")
	_ = os.WriteFile(cfg, []byte("# FastRouter quick tunnel\n"), 0o600)
	cmd := exec.Command(bin, "tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", c.Port), "--config", cfg, "--no-autoupdate", "--retries", "99")
	cmd.Env = append(os.Environ(), "TUNNEL_TRANSPORT_PROTOCOL=http2")
	cmd.Dir = os.TempDir()
	hideWindow(cmd)
	setPdeathsig(cmd)
	stderr, _ := cmd.StderrPipe()
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		os.RemoveAll(cfgDir)
		c.state, c.errMsg = "error", "could not start cloudflared: "+err.Error()
		return
	}
	bindToParent(cmd.Process.Pid)
	c.cmd = cmd
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			c.mu.Lock()
			if gen == c.gen {
				c.logTail = tailLines(c.logTail+line+"\n", 20)
				for _, m := range quickURL.FindAllString(line, -1) {
					if !strings.HasPrefix(m, "https://api.") {
						c.url, c.state = m, "running"
					}
				}
			}
			c.mu.Unlock()
		}
	}()
	go func() {
		err := cmd.Wait()
		os.RemoveAll(cfgDir)
		c.mu.Lock()
		defer c.mu.Unlock()
		if gen != c.gen || !c.enabled {
			return
		}
		c.cmd = nil
		msg := "cloudflared exited"
		if err != nil {
			msg += ": " + err.Error()
		}
		if t := strings.TrimSpace(c.logTail); t != "" {
			msg += " — " + lastLine(t)
		}
		c.errMsg, c.state = msg, "reconnecting"
		c.restarts++
		delay := backoff(c.restarts)
		go func() {
			time.Sleep(delay)
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.enabled && gen == c.gen && c.cmd == nil {
				if b, _ := c.Find(); b != "" {
					c.startLocked(b)
				} else {
					c.state, c.errMsg = "error", "cloudflared is no longer installed"
				}
			}
		}()
	}()
	go c.watch(gen)
}

func backoff(n int) time.Duration {
	d := time.Duration(n*n) * 3 * time.Second
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

// watch probes the public URL and restarts a tunnel that stayed unreachable
// for three minutes (as 9Router's watchdog does).
func (c *Cloudflared) watch(gen int) {
	client := &http.Client{Timeout: 6 * time.Second}
	for {
		time.Sleep(10 * time.Second)
		c.mu.Lock()
		if gen != c.gen || !c.enabled {
			c.mu.Unlock()
			return
		}
		u, since := c.url, c.since
		c.mu.Unlock()
		if u == "" {
			if time.Since(since) > 90*time.Second {
				c.restart(gen, "no tunnel URL after 90 s")
				return
			}
			continue
		}
		ok := probe(client, u)
		c.mu.Lock()
		if gen != c.gen {
			c.mu.Unlock()
			return
		}
		c.reachable = ok
		if ok {
			c.lastOK, c.errMsg = time.Now(), ""
			if c.state == "reconnecting" {
				c.state = "running"
			}
		}
		stale := !ok && time.Since(maxTime(c.lastOK, c.since)) > 3*time.Minute
		c.mu.Unlock()
		if stale {
			c.restart(gen, "the tunnel stopped answering")
			return
		}
	}
}

func (c *Cloudflared) restart(gen int, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen || !c.enabled {
		return
	}
	c.gen++ // the old process's Wait goroutine must not restart it again
	c.killLocked()
	c.errMsg = why + "; restarting"
	c.restarts++
	if b, _ := c.Find(); b != "" {
		c.startLocked(b)
	}
}

func probe(client *http.Client, base string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// Status reports the tunnel without blocking on the network.
func (c *Cloudflared) Status() CloudflaredStatus {
	p, src := c.Find()
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state
	if st == "" {
		st = "off"
	}
	return CloudflaredStatus{Installed: p != "", Path: p, Source: src, Enabled: c.enabled, State: st, URL: c.url, Reachable: c.reachable,
		Error: c.errMsg, Since: c.since, Restarts: c.restarts, Download: c.dl}
}

// URL is the current public URL ("" when off).
func (c *Cloudflared) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return ""
	}
	return c.url
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	x := l[len(l)-1]
	if len(x) > 240 {
		x = x[:240] + "…"
	}
	return x
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
