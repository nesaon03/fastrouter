package server

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/nesaon03/fastrouter/internal/clitools"
)

// CLI Tools page: detect coding tools and point their config at FastRouter.
// Status reads are read-only; files change only on Apply / Reset (backed up first).

type cliKey struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Masked string `json:"masked"`
}

type cliToolOut struct {
	*clitools.Tool
	Status clitools.Status `json:"status"`
}

func (s *Server) cliEnv() *clitools.Env {
	return clitools.OSEnv(filepath.Join(s.dataDir, "cli-backups"), s.port)
}

func (s *Server) cliEndpoints() []string {
	return []string{fmt.Sprintf("http://127.0.0.1:%d", s.port), fmt.Sprintf("http://localhost:%d", s.port)}
}

func (s *Server) handleCLITools(w http.ResponseWriter, r *http.Request) {
	e := s.cliEnv()
	out := []cliToolOut{}
	for _, t := range clitools.All() {
		out = append(out, cliToolOut{t, clitools.StatusOf(e, t)})
	}
	var keys []cliKey
	for _, k := range s.state.Load().Snap.APIKeys {
		if k.Active {
			keys = append(keys, cliKey{k.ID, k.Name, maskSecret(k.Key)})
		}
	}
	adminJSON(w, 200, map[string]any{"tools": out, "endpoints": s.cliEndpoints(), "keys": keys, "backupDir": e.BackupDir})
}

func (s *Server) handleCLITool(w http.ResponseWriter, r *http.Request) {
	t := clitools.Find(r.PathValue("id"))
	if t == nil {
		adminErr(w, 404, "unknown tool")
		return
	}
	adminJSON(w, 200, cliToolOut{t, clitools.StatusOf(s.cliEnv(), t)})
}

type cliApplyIn struct {
	Endpoint string            `json:"endpoint"`
	KeyID    string            `json:"keyId"`
	Model    string            `json:"model"`
	Roles    map[string]string `json:"roles"`
	Audio    *clitools.Audio   `json:"audio"`
}

func (s *Server) handleCLIApply(w http.ResponseWriter, r *http.Request) {
	t := clitools.Find(r.PathValue("id"))
	if t == nil {
		adminErr(w, 404, "unknown tool")
		return
	}
	var in cliApplyIn
	if !decodeStrict(w, r, &in) {
		return
	}
	ok := false
	for _, ep := range s.cliEndpoints() {
		ok = ok || in.Endpoint == ep
	}
	if !ok {
		adminErr(w, 400, "endpoint must be this FastRouter's local URL")
		return
	}
	// The key itself never travels through the browser: resolve it here.
	key, active := "", 0
	for _, k := range s.state.Load().Snap.APIKeys {
		if k.Active {
			active++
			if k.ID == in.KeyID {
				key = k.Key
			}
		}
	}
	if key == "" && (in.KeyID != "" || active > 0) {
		adminErr(w, 400, "choose one of your FastRouter API keys")
		return
	}
	roles := map[string]string{}
	for k, v := range in.Roles {
		if v != "" {
			roles[k] = v
		}
	}
	s.adminMu.Lock()
	res, err := clitools.Apply(s.cliEnv(), t, clitools.Input{BaseURL: in.Endpoint, APIKey: key, Model: in.Model, Roles: roles, Audio: in.Audio})
	s.adminMu.Unlock()
	s.cliResult(w, t, res, err)
}

func (s *Server) handleCLIReset(w http.ResponseWriter, r *http.Request) {
	t := clitools.Find(r.PathValue("id"))
	if t == nil {
		adminErr(w, 404, "unknown tool")
		return
	}
	s.adminMu.Lock()
	res, err := clitools.Reset(s.cliEnv(), t)
	s.adminMu.Unlock()
	s.cliResult(w, t, res, err)
}

func (s *Server) cliResult(w http.ResponseWriter, t *clitools.Tool, res clitools.Result, err error) {
	if err != nil {
		adminJSON(w, 400, map[string]any{"error": err.Error(), "written": res.Written, "backups": res.Backups})
		return
	}
	adminJSON(w, 200, map[string]any{"ok": true, "written": res.Written, "backups": res.Backups, "status": clitools.StatusOf(s.cliEnv(), t)})
}
