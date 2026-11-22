package server

// The dashboard's "start when I sign in" switch. It goes through the same
// internal/autostart plan as `fastrouter service install`, so both paths create
// the identical Startup shortcut / systemd unit / LaunchAgent.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nesaon03/fastrouter/internal/autostart"
)

// autostartState is what the Settings page renders.
func (s *Server) autostartState() map[string]any {
	path, installed := autostart.Installed(runtime.GOOS)
	out := map[string]any{
		"supported": autostart.Supported(runtime.GOOS),
		"installed": installed,
		"path":      path,
	}
	if !out["supported"].(bool) {
		out["reason"] = "This system has no per-user login autostart here."
		return out
	}
	exe, err := s.selfPath()
	if err != nil {
		out["reason"] = err.Error()
		return out
	}
	args := s.serveArgs(exe)
	if p, err := autostart.Build(runtime.GOOS, "install", exe, args); err == nil {
		if len(p) > 1 {
			out["note"] = p[len(p)-1].Note
		}
	}
	return out
}

// selfPath is the running binary, with symlinks resolved the way the CLI does.
func (s *Server) selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return exe, nil
}

// serveArgs are the flags the login entry should run: the same config file this
// process loaded, and the port it is serving when it is not the default.
func (s *Server) serveArgs(exe string) []string {
	args := []string{"serve"}
	if s.cfgPath != "" {
		args = append(args, "--config", s.cfgPath)
	}
	if s.port != 0 && s.port != 20128 {
		args = append(args, "--port", strconv.Itoa(s.port))
	}
	if s.host != "" && s.host != "127.0.0.1" {
		args = append(args, "--host", s.host)
	}
	_ = exe
	return args
}

// handleAutostartGet reports the current login-start state.
func (s *Server) handleAutostartGet(w http.ResponseWriter, r *http.Request) {
	adminJSON(w, http.StatusOK, s.autostartState())
}

// handleAutostartSet installs or removes the login entry.
func (s *Server) handleAutostartSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enable bool `json:"enable"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if !autostart.Supported(runtime.GOOS) {
		adminErr(w, http.StatusBadRequest, "autostart is not supported on this system")
		return
	}
	exe, err := s.selfPath()
	if err != nil {
		adminErr(w, http.StatusInternalServerError, "could not locate the running binary")
		return
	}
	action := "install"
	if !in.Enable {
		action = "uninstall"
	}
	plan, err := autostart.Build(runtime.GOOS, action, exe, s.serveArgs(exe))
	if err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A plan can spawn a helper (systemctl, launchctl, PowerShell); bound it so a
	// wedged helper cannot hold the admin lock.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var log strings.Builder
	if err := plan.Apply(io.MultiWriter(&log, io.Discard)); err != nil {
		if ctx.Err() != nil {
			adminErr(w, http.StatusGatewayTimeout, "the system helper timed out")
			return
		}
		adminErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, ok := autostart.Installed(runtime.GOOS); action == "install" && !ok {
		adminErr(w, http.StatusInternalServerError, "the login entry was not created")
		return
	}
	out := s.autostartState()
	out["ok"] = true
	out["detail"] = strings.TrimSpace(log.String())
	adminJSON(w, http.StatusOK, out)
}
