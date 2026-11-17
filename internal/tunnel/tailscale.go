package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Tailscale drives the tailscale CLI. FastRouter only ever adds or removes
// its own serve/funnel entry; other serve ports are left alone (no "reset").
type Tailscale struct {
	Port int
	Bin  string // override for tests
	run  func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// TailscaleStatus is what the dashboard shows.
type TailscaleStatus struct {
	Installed  bool     `json:"installed"`
	Running    bool     `json:"running"` // daemon reachable
	LoggedIn   bool     `json:"loggedIn"`
	State      string   `json:"backendState,omitempty"`
	AuthURL    string   `json:"authUrl,omitempty"`
	DNSName    string   `json:"dnsName,omitempty"`
	IPs        []string `json:"ips,omitempty"`
	Tailnet    string   `json:"tailnet,omitempty"`
	HTTPS      bool     `json:"https"`  // tailnet has HTTPS certificates enabled
	Funnel     bool     `json:"funnel"` // node may use Funnel
	Mode       string   `json:"mode"`   // off | serve | funnel (FastRouter's own entry)
	URL        string   `json:"url,omitempty"`
	OtherPorts []string `json:"otherPorts,omitempty"` // serve entries that are not FastRouter
	Error      string   `json:"error,omitempty"`
	Version    string   `json:"version,omitempty"`
}

func (t *Tailscale) exec(ctx context.Context, args ...string) ([]byte, error) {
	bin := t.Find()
	if bin == "" {
		return nil, errors.New("tailscale is not installed")
	}
	if t.run != nil {
		return t.run(ctx, bin, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	hideWindow(cmd)
	return cmd.CombinedOutput()
}

// Find returns the tailscale CLI path or "".
func (t *Tailscale) Find() string {
	if t.Bin != "" {
		return t.Bin
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	var cands []string
	switch runtime.GOOS {
	case "windows":
		cands = []string{filepath.Join(os.Getenv("ProgramFiles"), "Tailscale", "tailscale.exe"), `C:\Program Files\Tailscale\tailscale.exe`}
	case "darwin":
		cands = []string{"/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/usr/local/bin/tailscale", "/opt/homebrew/bin/tailscale"}
	default:
		cands = []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale"}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

type tsStatus struct {
	Version      string
	BackendState string
	AuthURL      string
	CertDomains  []string
	Self         *struct {
		DNSName      string
		TailscaleIPs []string
		Online       bool
		Capabilities []string
		CapMap       map[string]any
	}
	CurrentTailnet *struct{ Name string }
}

type serveConfig struct {
	TCP map[string]struct {
		HTTPS        bool   `json:"HTTPS"`
		HTTP         bool   `json:"HTTP"`
		TCPForward   string `json:"TCPForward"`
		TerminateTLS string `json:"TerminateTLS"`
	} `json:"TCP"`
	Web map[string]struct {
		Handlers map[string]struct {
			Proxy string `json:"Proxy"`
			Path  string `json:"Path"`
			Text  string `json:"Text"`
		} `json:"Handlers"`
	} `json:"Web"`
	AllowFunnel map[string]bool `json:"AllowFunnel"`
}

func (t *Tailscale) target() string { return fmt.Sprintf("http://127.0.0.1:%d", t.Port) }

func (t *Tailscale) isOurs(proxy string) bool {
	p := strings.TrimRight(proxy, "/")
	for _, h := range []string{"127.0.0.1", "localhost"} {
		if p == fmt.Sprintf("http://%s:%d", h, t.Port) {
			return true
		}
	}
	return p == strconv.Itoa(t.Port)
}

// Status reads `tailscale status --json` and `serve status --json`.
func (t *Tailscale) Status(ctx context.Context) TailscaleStatus {
	st := TailscaleStatus{Mode: "off"}
	if t.Find() == "" {
		return st
	}
	st.Installed = true
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := t.exec(ctx, "status", "--json")
	var s tsStatus
	if jerr := json.Unmarshal(jsonPart(out), &s); jerr != nil {
		st.Error = firstLine(out, err)
		return st
	}
	st.Running = true
	st.Version = strings.SplitN(s.Version, "-", 2)[0]
	st.State, st.AuthURL = s.BackendState, s.AuthURL
	if s.Self != nil {
		st.LoggedIn = s.BackendState == "Running"
		st.DNSName = strings.TrimSuffix(s.Self.DNSName, ".")
		st.IPs = s.Self.TailscaleIPs
		for _, c := range s.Self.Capabilities {
			if strings.Contains(c, "funnel") {
				st.Funnel = true
			}
		}
		for k := range s.Self.CapMap {
			if strings.Contains(k, "funnel") {
				st.Funnel = true
			}
		}
	}
	if s.CurrentTailnet != nil {
		st.Tailnet = s.CurrentTailnet.Name
	}
	st.HTTPS = len(s.CertDomains) > 0
	if !st.LoggedIn {
		return st
	}
	sc, err := t.serveStatus(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Mode, st.URL, st.OtherPorts = t.classify(sc, st.DNSName)
	return st
}

func (t *Tailscale) serveStatus(ctx context.Context) (serveConfig, error) {
	var sc serveConfig
	out, err := t.exec(ctx, "serve", "status", "--json")
	if b := jsonPart(out); len(b) > 0 {
		if jerr := json.Unmarshal(b, &sc); jerr == nil {
			return sc, nil
		}
	}
	if err != nil {
		return sc, errors.New(firstLine(out, err))
	}
	return sc, nil // empty output: nothing served
}

// classify finds FastRouter's entry (a web handler proxying to our port).
func (t *Tailscale) classify(sc serveConfig, dns string) (mode, url string, other []string) {
	mode = "off"
	ours := map[string]bool{}
	for hp, w := range sc.Web {
		for _, h := range w.Handlers {
			if t.isOurs(h.Proxy) {
				_, port, _ := strings.Cut(hp, ":")
				ours[port] = true
				m := "serve"
				if sc.AllowFunnel[hp] {
					m = "funnel"
				}
				if mode == "off" || m == "funnel" {
					mode = m
					scheme := "https"
					if c, ok := sc.TCP[port]; ok && c.HTTP {
						scheme = "http"
					}
					url = scheme + "://" + dns
					if !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
						url += ":" + port
					}
				}
			}
		}
	}
	for port := range sc.TCP {
		if !ours[port] {
			other = append(other, port)
		}
	}
	sortStrings(other)
	return mode, url, other
}

var enableURL = regexp.MustCompile(`https://login\.tailscale\.com/\S+`)

// NeedsAction is returned when the tailnet admin must allow HTTPS / Funnel
// (or the user must log in) first; URL is where to do it.
type NeedsAction struct {
	What, URL string
}

func (e *NeedsAction) Error() string { return e.What }

// Enable adds FastRouter's entry: "serve" (tailnet only) or "funnel"
// (public). Serve uses HTTPS on 443 when the tailnet has certificates,
// otherwise plain HTTP on FastRouter's port (still WireGuard-encrypted).
func (t *Tailscale) Enable(ctx context.Context, mode string) error {
	st := t.Status(ctx)
	switch {
	case !st.Installed:
		return errors.New("tailscale is not installed")
	case !st.Running:
		return errors.New("the Tailscale service is not running; open the Tailscale app")
	case !st.LoggedIn:
		return &NeedsAction{What: "Tailscale is not logged in", URL: st.AuthURL}
	}
	if st.Mode != "off" {
		if err := t.Disable(ctx); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	var args []string
	switch mode {
	case "funnel":
		if !st.HTTPS {
			return &NeedsAction{What: "Funnel needs HTTPS certificates; turn on HTTPS in the Tailscale admin console (DNS page)", URL: "https://login.tailscale.com/admin/dns"}
		}
		if t.portTaken(ctx, "443") {
			return errors.New("port 443 on this Tailscale node is already served by something else")
		}
		args = []string{"funnel", "--bg", "--yes", "--https=443", t.target()}
	case "serve":
		if st.HTTPS {
			if t.portTaken(ctx, "443") {
				return errors.New("port 443 on this Tailscale node is already served by something else")
			}
			args = []string{"serve", "--bg", "--yes", "--https=443", t.target()}
		} else {
			p := strconv.Itoa(t.Port)
			if t.portTaken(ctx, p) {
				return fmt.Errorf("port %s on this Tailscale node is already served by something else", p)
			}
			args = []string{"serve", "--bg", "--yes", "--http=" + p, t.target()}
		}
	default:
		return errors.New("mode must be serve or funnel")
	}
	out, err := t.exec(ctx, args...)
	if err != nil && strings.Contains(string(out), "flag provided but not defined: -yes") {
		args = removeArg(args, "--yes")
		out, err = t.exec(ctx, args...)
	}
	text := string(out)
	if u := enableURL.FindString(text); u != "" && (err != nil || strings.Contains(strings.ToLower(text), "not enabled")) {
		return &NeedsAction{What: firstLine(out, err), URL: u}
	}
	if err != nil {
		return errors.New(firstLine(out, err))
	}
	return nil
}

func (t *Tailscale) portTaken(ctx context.Context, port string) bool {
	sc, err := t.serveStatus(ctx)
	if err != nil {
		return false
	}
	if _, ok := sc.TCP[port]; !ok {
		return false
	}
	for hp, w := range sc.Web {
		if strings.HasSuffix(hp, ":"+port) {
			for _, h := range w.Handlers {
				if !t.isOurs(h.Proxy) {
					return true
				}
			}
			return false
		}
	}
	return true // a TCP forward we don't own
}

// Disable removes only FastRouter's serve/funnel entries.
func (t *Tailscale) Disable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sc, err := t.serveStatus(ctx)
	if err != nil {
		return err
	}
	for hp, w := range sc.Web {
		mine := false
		for _, h := range w.Handlers {
			mine = mine || t.isOurs(h.Proxy)
		}
		if !mine {
			continue
		}
		_, port, _ := strings.Cut(hp, ":")
		flag := "--https=" + port
		if c, ok := sc.TCP[port]; ok && c.HTTP {
			flag = "--http=" + port
		}
		if sc.AllowFunnel[hp] {
			if out, err := t.exec(ctx, "funnel", flag, "off"); err != nil {
				return errors.New(firstLine(out, err))
			}
			continue
		}
		if out, err := t.exec(ctx, "serve", flag, "off"); err != nil {
			return errors.New(firstLine(out, err))
		}
	}
	return nil
}

// Login starts `tailscale login` and returns the browser URL to finish it.
func (t *Tailscale) Login(ctx context.Context) (string, error) {
	bin := t.Find()
	if bin == "" {
		return "", errors.New("tailscale is not installed")
	}
	if t.run == nil {
		cmd := exec.Command(bin, "login", "--timeout=5m")
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			return "", err
		}
		go cmd.Wait()
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := t.Status(ctx)
		if st.AuthURL != "" {
			return st.AuthURL, nil
		}
		if st.LoggedIn {
			return "", nil
		}
		time.Sleep(time.Second)
	}
	return "", errors.New("Tailscale did not return a login link; open the Tailscale app to log in")
}

// InstallURL is Tailscale's official download page for this OS.
func InstallURL() string {
	switch runtime.GOOS {
	case "windows":
		return "https://pkgs.tailscale.com/stable/tailscale-setup-latest-amd64.msi"
	case "darwin":
		return "https://tailscale.com/download/mac"
	}
	return "https://tailscale.com/download/linux"
}

func jsonPart(b []byte) []byte {
	i := strings.IndexAny(string(b), "{[")
	if i < 0 {
		return nil
	}
	return b[i:]
}

func firstLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" && err != nil {
		return err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func removeArg(a []string, x string) []string {
	out := a[:0:0]
	for _, v := range a {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && less(a[j], a[j-1]); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func less(a, b string) bool {
	x, e1 := strconv.Atoi(a)
	y, e2 := strconv.Atoi(b)
	if e1 == nil && e2 == nil {
		return x < y
	}
	return a < b
}
