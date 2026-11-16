// Package clitools detects coding CLIs and points their config files at
// FastRouter, following 9Router's per-tool config writers (MIT, decolua/9router).
// Reads are read-only; writes happen only on Apply/Reset and back up every file first.
package clitools

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Env is where the tools live: the user's home and the OS-specific folders.
type Env struct {
	Home, AppData, LocalAppData, XDGConfig, GOOS string
	PathDirs                                     []string
	BackupDir                                    string // FastRouter's data dir/cli-backups
	Port                                         int    // FastRouter's port, to tell "ours" from other routers

	tool, stamp string
	backups     []string
	written     []string
}

// OSEnv reads the environment of the running process.
func OSEnv(backupDir string, port int) *Env {
	home, _ := os.UserHomeDir()
	e := &Env{Home: home, GOOS: runtime.GOOS, BackupDir: backupDir, Port: port,
		AppData: os.Getenv("APPDATA"), LocalAppData: os.Getenv("LOCALAPPDATA"), XDGConfig: os.Getenv("XDG_CONFIG_HOME")}
	if e.AppData == "" {
		e.AppData = filepath.Join(home, "AppData", "Roaming")
	}
	if e.LocalAppData == "" {
		e.LocalAppData = filepath.Join(home, "AppData", "Local")
	}
	e.PathDirs = filepath.SplitList(os.Getenv("PATH"))
	if e.GOOS == "windows" {
		e.PathDirs = append(e.PathDirs, filepath.Join(e.AppData, "npm")) // npm -g shims, like 9Router
	}
	e.PathDirs = append(e.PathDirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".cargo", "bin"))
	return e
}

func (e *Env) p(parts ...string) string { return filepath.Join(append([]string{e.Home}, parts...)...) }

func (e *Env) xdg(parts ...string) string {
	base := e.XDGConfig
	if base == "" {
		base = e.p(".config")
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

// vscodeUser is VS Code's per-user settings folder.
func (e *Env) vscodeUser() string {
	switch e.GOOS {
	case "windows":
		return filepath.Join(e.AppData, "Code", "User")
	case "darwin":
		return e.p("Library", "Application Support", "Code", "User")
	}
	return e.p(".config", "Code", "User")
}

// onPath reports whether any of the binaries is installed.
func (e *Env) onPath(bins ...string) bool {
	exts := []string{""}
	if e.GOOS == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ".ps1", ""}
	}
	for _, b := range bins {
		for _, d := range e.PathDirs {
			if d == "" {
				continue
			}
			for _, x := range exts {
				if st, err := os.Stat(filepath.Join(d, b+x)); err == nil && !st.IsDir() {
					return true
				}
			}
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readText(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// readObj reads a JSON object file; a missing file is an empty object.
func readObj(p string) (*Obj, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return newObj(), nil
	}
	if err != nil {
		return nil, err
	}
	v, err := parseJSON(b)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix or move it first", filepath.Base(p), err)
	}
	o, ok := v.(*Obj)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON object", filepath.Base(p))
	}
	return o, nil
}

// peekObj reads a JSON object for status checks (nil on any problem).
func peekObj(p string) *Obj {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	v, err := parseJSON(b)
	if err != nil {
		return nil
	}
	o, _ := v.(*Obj)
	return o
}

// write saves a file after copying the current one into the backup folder.
func (e *Env) write(p string, data []byte) error {
	if old, err := os.ReadFile(p); err == nil {
		if string(old) == string(data) {
			return nil
		}
		if e.BackupDir == "" {
			return errors.New("no backup folder configured")
		}
		rel := strings.NewReplacer(":", "", string(filepath.Separator), "__", "/", "__").Replace(p)
		dst := filepath.Join(e.BackupDir, e.tool, e.stamp, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, old, 0o600); err != nil {
			return fmt.Errorf("backup failed, nothing written: %w", err)
		}
		e.backups = append(e.backups, dst)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := p + ".fastrouter-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	e.written = append(e.written, p)
	return nil
}

func (e *Env) writeObj(p string, o *Obj) error { return e.write(p, marshalJSON(o)) }

// ours reports whether a base URL points at this FastRouter.
func (e *Env) ours(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	h := u.Hostname()
	local := h == "localhost" || h == "127.0.0.1" || h == "0.0.0.0" || h == "::1"
	return local && u.Port() == fmt.Sprint(e.Port)
}

// other names another local router a config points at (9Router's default port).
func other(raw string) string {
	if strings.Contains(raw, ":20128") || strings.Contains(strings.ToLower(raw), "9router") {
		return "9Router"
	}
	return ""
}

var (
	modelRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+\-\[\]]{1,200}$`)
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,400}$`)
)

func (e *Env) begin(tool string) {
	e.tool, e.stamp = tool, time.Now().Format("20060102-150405")
	e.backups, e.written = nil, nil
}
