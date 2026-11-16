package saver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Headroom install / start / stop for the dashboard, after 9Router's
// src/lib/headroom/{detect,process}.js. Nothing runs until the user clicks:
//
//	Install:  <python> -m venv <dataDir>/headroom/venv
//	          <venv python> -m pip install --upgrade "headroom-ai[proxy]"
//	Extras:   <venv python> -m pip install --upgrade "headroom-ai[proxy,code]"   (or ml)
//	Remove:   <venv python> -m pip uninstall -y tree-sitter tree-sitter-language-pack   (or torch huggingface-hub)
//	Start:    <headroom> proxy --port 8787 [--code-aware] [--disable-kompress]
//
// 9Router pip-installs into the system Python; FastRouter uses its own venv so
// it never touches global packages (and works where pip refuses global installs).
// A headroom already on PATH is used as is.

// HeadroomExtras are the optional compression extras and the packages that show they are installed.
var HeadroomExtras = map[string][]string{
	"code": {"tree-sitter", "tree-sitter-language-pack"},
	"ml":   {"torch", "huggingface-hub"},
}

// HeadroomCtl manages a local Headroom proxy under dir (<dataDir>/headroom).
type HeadroomCtl struct {
	Dir string

	mu       sync.Mutex
	busy     string // "install" | "uninstall" | ""
	lastErr  string
	proc     *os.Process
	cached   *HeadroomStatus
	cachedAt time.Time
}

// HeadroomStatus is what the Token Saver page shows.
type HeadroomStatus struct {
	Installed bool            `json:"installed"`
	Managed   bool            `json:"managed"` // installed in FastRouter's own venv
	Path      string          `json:"path,omitempty"`
	Python    string          `json:"python,omitempty"`
	Version   string          `json:"version,omitempty"`
	Extras    map[string]bool `json:"extras"`
	Running   bool            `json:"running"`
	LocalURL  bool            `json:"localUrl"`
	CanStart  bool            `json:"canStart"`
	PID       int             `json:"pid,omitempty"`
	Busy      string          `json:"busy,omitempty"`
	Error     string          `json:"error,omitempty"`
	Log       string          `json:"log,omitempty"`
	Commands  []string        `json:"commands"` // what Install would run, shown before the click
}

var isWin = runtime.GOOS == "windows"

func (h *HeadroomCtl) venvDir() string { return filepath.Join(h.Dir, "venv") }

func (h *HeadroomCtl) venvBin(name string) string {
	if isWin {
		return filepath.Join(h.venvDir(), "Scripts", name+".exe")
	}
	return filepath.Join(h.venvDir(), "bin", name)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// extraBins are Python script dirs often missing from PATH (same list as 9Router).
func extraBins() []string {
	if isWin {
		la, ad := os.Getenv("LOCALAPPDATA"), os.Getenv("APPDATA")
		var out []string
		for _, v := range []string{"313", "312", "311", "310"} {
			out = append(out, la+`\Programs\Python\Python`+v+`\Scripts`, ad+`\Python\Python`+v+`\Scripts`)
		}
		return out
	}
	home, _ := os.UserHomeDir()
	return []string{"/usr/local/bin", "/opt/homebrew/bin", home + "/.local/bin", "/usr/bin", "/bin"}
}

// HeadroomBinary finds the headroom CLI: FastRouter's venv first, then PATH.
func HeadroomBinary(dir string) (path string, managed bool) {
	h := &HeadroomCtl{Dir: dir}
	if dir != "" && exists(h.venvBin("headroom")) {
		return h.venvBin("headroom"), true
	}
	if p, err := exec.LookPath("headroom"); err == nil {
		return p, false
	}
	name := "headroom"
	if isWin {
		name += ".exe"
	}
	for _, d := range extraBins() {
		if p := filepath.Join(d, name); exists(p) {
			return p, false
		}
	}
	return "", false
}

var pyVer = regexp.MustCompile(`(\d+)\.(\d+)`)

// findPython returns a Python >= 3.10 command (exe plus leading args).
func findPython() []string {
	var cands [][]string
	if isWin {
		for _, v := range []string{"3.13", "3.12", "3.11", "3.10"} {
			cands = append(cands, []string{"py", "-" + v})
		}
		for _, v := range []string{"313", "312", "311", "310"} {
			cands = append(cands, []string{os.Getenv("LOCALAPPDATA") + `\Programs\Python\Python` + v + `\python.exe`})
		}
		cands = append(cands, []string{"python"}, []string{"python3"})
	} else {
		for _, n := range []string{"python3.13", "python3.12", "python3.11", "python3.10", "python3", "python"} {
			cands = append(cands, []string{n})
		}
	}
	for _, c := range cands {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		cmd := exec.CommandContext(ctx, c[0], append(c[1:], "--version")...)
		hideWindow(cmd)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			continue
		}
		m := pyVer.FindStringSubmatch(string(out))
		if m == nil {
			continue
		}
		maj, _ := strconv.Atoi(m[1])
		mn, _ := strconv.Atoi(m[2])
		if maj > 3 || (maj == 3 && mn >= 10) {
			return c
		}
	}
	return nil
}

// pipPython is the interpreter that owns the headroom install.
func (h *HeadroomCtl) pipPython(bin string, managed bool) []string {
	if managed {
		return []string{h.venvBin("python")}
	}
	if bin != "" {
		for _, n := range []string{"python.exe", "python3", "python"} {
			if p := filepath.Join(filepath.Dir(bin), n); exists(p) {
				return []string{p}
			}
		}
	}
	return findPython()
}

func localURL(raw string) (port string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !(ip.IsLoopback() || ip.IsUnspecified())) {
		return "", false
	}
	port = u.Port()
	if port == "" {
		port = "8787"
	}
	return port, true
}

// Probe reports whether a Headroom proxy answers /health at raw.
func Probe(ctx context.Context, raw string) bool {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(raw, "/")+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 300
}

func (h *HeadroomCtl) installCommands(py []string, extras []string) []string {
	spec := `"headroom-ai[` + strings.Join(append([]string{"proxy"}, extras...), ",") + `]"`
	cmds := []string{}
	if !exists(h.venvBin("python")) {
		p := "python"
		if py != nil {
			p = strings.Join(py, " ")
		}
		cmds = append(cmds, p+" -m venv "+h.venvDir())
	}
	return append(cmds, h.venvBin("python")+" -m pip install --upgrade "+spec)
}

// Status inspects the install and probes the proxy. Cached for 5 seconds.
func (h *HeadroomCtl) Status(ctx context.Context, proxyURL string, fresh bool) HeadroomStatus {
	h.mu.Lock()
	if !fresh && h.cached != nil && time.Since(h.cachedAt) < 5*time.Second && h.busy == "" {
		st := *h.cached
		h.mu.Unlock()
		st.Running = Probe(ctx, proxyURL)
		return st
	}
	busy, lastErr := h.busy, h.lastErr
	h.mu.Unlock()

	st := HeadroomStatus{Extras: map[string]bool{"code": false, "ml": false}, Busy: busy, Error: lastErr}
	bin, managed := HeadroomBinary(h.Dir)
	st.Path, st.Managed, st.Installed = bin, managed, bin != ""
	py := h.pipPython(bin, managed)
	if py != nil {
		st.Python = strings.Join(py, " ")
	}
	if st.Installed && busy == "" && py != nil {
		st.Version, st.Extras = pipList(py)
	}
	_, st.LocalURL = localURL(proxyURL)
	st.CanStart = st.Installed && st.LocalURL
	st.Running = Probe(ctx, proxyURL)
	st.PID = h.pid()
	st.Log = tail(filepath.Join(h.Dir, "install.log"), 15)
	if busy == "" && !st.Installed {
		st.Commands = h.installCommands(findPythonCached(py, managed), nil)
	} else {
		st.Commands = []string{}
	}
	h.mu.Lock()
	h.cached, h.cachedAt = &st, time.Now()
	h.mu.Unlock()
	return st
}

func findPythonCached(py []string, managed bool) []string {
	if managed {
		return nil
	}
	return py
}

func pipList(py []string) (string, map[string]bool) {
	extras := map[string]bool{"code": false, "ml": false}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py[0], append(py[1:], "-m", "pip", "list", "--format=json", "--disable-pip-version-check")...)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", extras
	}
	var pkgs []struct{ Name, Version string }
	if json.Unmarshal(out, &pkgs) != nil {
		return "", extras
	}
	have := map[string]string{}
	for _, p := range pkgs {
		have[strings.ToLower(p.Name)] = p.Version
	}
	for e, markers := range HeadroomExtras {
		for _, m := range markers {
			if _, ok := have[m]; ok {
				extras[e] = true
			}
		}
	}
	return have["headroom-ai"], extras
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ErrBusy means an install or uninstall is already running.
var ErrBusy = errors.New("an install is already running")

func (h *HeadroomCtl) begin(what string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Dir == "" {
		return errors.New("no data directory configured")
	}
	if h.busy != "" {
		return ErrBusy
	}
	h.busy, h.lastErr, h.cached = what, "", nil
	return nil
}

func (h *HeadroomCtl) end(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.busy, h.cached = "", nil
	if err != nil {
		h.lastErr = err.Error()
	}
}

// run executes one command, appending its output to the install log.
func (h *HeadroomCtl) run(log *os.File, argv []string) error {
	fmt.Fprintf(log, "$ %s\n", strings.Join(argv, " "))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	hideWindow(cmd)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), "PIP_DISABLE_PIP_VERSION_CHECK=1", "PYTHONUTF8=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %v (see %s)", filepath.Base(argv[0]), err, filepath.Join(h.Dir, "install.log"))
	}
	return nil
}

func validExtras(extras []string) ([]string, error) {
	var out []string
	for _, e := range extras {
		if _, ok := HeadroomExtras[e]; !ok {
			return nil, fmt.Errorf("unknown extra %q", e)
		}
		out = append(out, e)
	}
	return out, nil
}

// Install installs (or upgrades) headroom-ai[proxy,extras...] in FastRouter's
// venv, creating the venv first. It runs in the background; poll Status.
func (h *HeadroomCtl) Install(extras []string) error {
	extras, err := validExtras(extras)
	if err != nil {
		return err
	}
	bin, managed := HeadroomBinary(h.Dir)
	var py []string
	if managed || bin == "" {
		py = findPython()
		if py == nil && !exists(h.venvBin("python")) {
			return errors.New("Python 3.10 or newer was not found. Install it from https://www.python.org/downloads/ (tick \"Add to PATH\"), then click Install again")
		}
	} else {
		// headroom already on PATH (installed by the user): add extras with its own Python.
		py = h.pipPython(bin, false)
		if py == nil {
			return errors.New("could not find the Python that owns the installed headroom")
		}
	}
	if err := h.begin("install"); err != nil {
		return err
	}
	go func() {
		var err error
		defer func() { h.end(err) }()
		if err = os.MkdirAll(h.Dir, 0o700); err != nil {
			return
		}
		log, e := os.Create(filepath.Join(h.Dir, "install.log"))
		if e != nil {
			err = e
			return
		}
		defer log.Close()
		spec := "headroom-ai[" + strings.Join(append([]string{"proxy"}, extras...), ",") + "]"
		target := py
		if managed || bin == "" {
			if !exists(h.venvBin("python")) {
				if err = h.run(log, append(append([]string{}, py...), "-m", "venv", h.venvDir())); err != nil {
					return
				}
			}
			target = []string{h.venvBin("python")}
		}
		err = h.run(log, append(append([]string{}, target...), "-m", "pip", "install", "--upgrade", spec))
		if err == nil {
			fmt.Fprintln(log, "Done.")
		}
	}()
	return nil
}

// Uninstall removes the marker packages of one extra (never headroom-ai itself).
func (h *HeadroomCtl) Uninstall(extra string) error {
	markers, ok := HeadroomExtras[extra]
	if !ok {
		return fmt.Errorf("unknown extra %q", extra)
	}
	bin, managed := HeadroomBinary(h.Dir)
	if bin == "" {
		return errors.New("headroom is not installed")
	}
	py := h.pipPython(bin, managed)
	if py == nil {
		return errors.New("could not find the Python that owns headroom")
	}
	if err := h.begin("uninstall"); err != nil {
		return err
	}
	go func() {
		var err error
		defer func() { h.end(err) }()
		log, e := os.Create(filepath.Join(h.Dir, "install.log"))
		if e != nil {
			err = e
			return
		}
		defer log.Close()
		err = h.run(log, append(append(append([]string{}, py...), "-m", "pip", "uninstall", "-y"), markers...))
	}()
	return nil
}

func (h *HeadroomCtl) pidFile() string { return filepath.Join(h.Dir, "proxy.pid") }

func (h *HeadroomCtl) pid() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.proc != nil {
		return h.proc.Pid
	}
	b, _ := os.ReadFile(h.pidFile())
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// Start runs `headroom proxy` for a localhost URL (9Router's flags).
func (h *HeadroomCtl) Start(proxyURL string, codeAware, kompress bool) (int, error) {
	port, ok := localURL(proxyURL)
	if !ok {
		return 0, errors.New("FastRouter only starts Headroom for a localhost URL; start it yourself at the configured URL")
	}
	if Probe(context.Background(), proxyURL) {
		return 0, errors.New("a Headroom proxy is already running at " + proxyURL)
	}
	bin, _ := HeadroomBinary(h.Dir)
	if bin == "" {
		return 0, errors.New("headroom is not installed: click Install first")
	}
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(h.Dir, "proxy.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(bin, (&Saver{HeadroomCodeAware: codeAware, HeadroomKompress: kompress}).HeadroomArgs(port)...)
	hideWindow(cmd)
	detach(cmd)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Like 9Router: fail if it exits during startup, otherwise wait for /health.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return 0, fmt.Errorf("headroom proxy exited during startup (%v); see %s", err, filepath.Join(h.Dir, "proxy.log"))
		case <-time.After(500 * time.Millisecond):
		}
		if Probe(context.Background(), proxyURL) {
			break
		}
	}
	h.mu.Lock()
	h.proc, h.cached = cmd.Process, nil
	h.mu.Unlock()
	_ = os.WriteFile(h.pidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	return cmd.Process.Pid, nil
}

// Stop kills the proxy FastRouter started (this run, or a previous one via its pid file).
func (h *HeadroomCtl) Stop() error {
	h.mu.Lock()
	p := h.proc
	h.proc, h.cached = nil, nil
	h.mu.Unlock()
	if p == nil {
		n := 0
		if b, err := os.ReadFile(h.pidFile()); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(bytes.TrimSpace(b))))
		}
		if n <= 0 {
			return errors.New("FastRouter did not start this Headroom proxy; stop it where it was started")
		}
		var err error
		if p, err = os.FindProcess(n); err != nil {
			return err
		}
	}
	_ = os.Remove(h.pidFile())
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("could not stop headroom: %v", err)
	}
	return nil
}
