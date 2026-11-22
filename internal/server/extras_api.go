package server

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nesaon03/fastrouter/internal/saver"
	"github.com/nesaon03/fastrouter/skills"
)

// ---- Headroom (Token Saver page) ----

func (s *Server) handleHeadroomStatus(w http.ResponseWriter, r *http.Request) {
	sv := s.saver.Load()
	adminJSON(w, http.StatusOK, s.hr.Status(r.Context(), sv.HeadroomURL, r.URL.Query().Get("fresh") == "1"))
}

func (s *Server) handleHeadroomInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Extras []string `json:"extras"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if err := s.hr.Install(in.Extras); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": "install"})
}

func (s *Server) handleHeadroomUninstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Extra string `json:"extra"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if err := s.hr.Uninstall(in.Extra); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": "uninstall"})
}

func (s *Server) handleHeadroomStart(w http.ResponseWriter, r *http.Request) {
	sv := s.saver.Load()
	pid, err := s.hr.Start(sv.HeadroomURL, sv.HeadroomCodeAware, sv.HeadroomKompress)
	if err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": pid})
}

func (s *Server) handleHeadroomStop(w http.ResponseWriter, r *http.Request) {
	if err := s.hr.Stop(); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	adminJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleHeadroomRestart applies new code-aware / kompress flags (9Router's /api/headroom/restart).
func (s *Server) handleHeadroomRestart(w http.ResponseWriter, r *http.Request) {
	_ = s.hr.Stop()
	s.handleHeadroomStart(w, r)
}

// ---- Agent Skills ----

var skillName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// skillRawBase is where the SKILL.md files live on GitHub (nesaon03/fastrouter, main).
const skillRawBase = "https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills"

var hostRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^\[[0-9a-fA-F:]+\](:[0-9]{1,5})?$`)

// handleSkill serves skills/<name>/SKILL.md. Public like the dashboard files:
// it holds no secrets. Example URLs point at the address it was fetched from.
func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !skillName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(skills.FS, name+"/SKILL.md")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body := string(b)
	if hostRe.MatchString(r.Host) && r.Host != "localhost:20128" {
		body = strings.ReplaceAll(body, "http://localhost:20128", "http://"+r.Host)
	}
	if hostRe.MatchString(r.Host) {
		// The GitHub repo is private: keep links between skills on this server.
		body = strings.ReplaceAll(body, skillRawBase, "http://"+r.Host+"/skills")
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(body))
}

// skillList is the Agent Skills page data: each skill's front-matter description.
func skillList() []map[string]string {
	out := []map[string]string{}
	entries, _ := fs.ReadDir(skills.FS, ".")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(skills.FS, e.Name()+"/SKILL.md")
		if err != nil {
			continue
		}
		desc := ""
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "description: "); ok {
				desc = v
				break
			}
		}
		out = append(out, map[string]string{"id": e.Name(), "description": desc})
	}
	return out
}

func (s *Server) handleSkillList(w http.ResponseWriter, r *http.Request) {
	adminJSON(w, http.StatusOK, map[string]any{"skills": skillList()})
}

func newHeadroomCtl(dataDir string) *saver.HeadroomCtl {
	if dataDir == "" {
		return &saver.HeadroomCtl{}
	}
	return &saver.HeadroomCtl{Dir: filepath.Join(dataDir, "headroom")}
}
