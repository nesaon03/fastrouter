package server

import (
	"github.com/nesaon03/fastrouter/internal/store"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// handleStatus is the dashboard's read-only data: names, counts, states. No secrets.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	snap := st.Snap
	now := time.Now()

	type acct struct {
		Name, Status string
		Priority     int
		Active       bool
		Cooling      bool
	}
	type prov struct {
		ID, Name, Prefix, Kind string
		Plugin                 bool
		Usable                 int
		Accounts               []acct
	}
	cooling := map[string]bool{}
	for _, p := range st.List {
		for _, a := range p.Pool.Accounts() {
			cooling[a.ID] = a.CoolingDown(now)
		}
	}
	provs := []prov{}
	for _, p := range st.List {
		kind := "built-in"
		if strings.HasPrefix(p.ID, "openai-compatible") || strings.HasPrefix(p.ID, "anthropic-compatible") {
			kind = "custom endpoint"
		}
		pv := prov{ID: p.ID, Name: p.Name, Prefix: st.PrefixFor(p), Kind: kind, Plugin: p.Plugin, Usable: p.Pool.Size(), Accounts: []acct{}}
		for _, c := range snap.Connections {
			if c.Provider == p.ID {
				pv.Accounts = append(pv.Accounts, acct{Name: maskEmail(c.Name), Status: c.Status, Priority: c.Priority, Active: c.Active, Cooling: cooling[c.ID]})
			}
		}
		provs = append(provs, pv)
	}
	type combo struct {
		Name, Strategy, Kind string
		Models               []string
	}
	combos := []combo{}
	for _, c := range snap.Combos {
		combos = append(combos, combo{c.Name, c.Strategy, c.Kind, c.Models})
	}
	writeJSON(w, map[string]any{
		"version":      Version,
		"uptimeSec":    int(time.Since(s.start).Seconds()),
		"providers":    provs,
		"combos":       combos,
		"apiKeys":      len(snap.APIKeys),
		"customModels": len(snap.CustomModels),
		"models":       st.Models,
		"warnings":     maskWarnings(snap.Warnings),
	})
}

// maskEmail keeps account names readable without showing full addresses.
func maskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at < 0 {
		// OAuth subjects such as "google-oauth2|user_01AB…" identify a person too.
		if bar := strings.IndexByte(s, '|'); bar > 0 && len(s)-bar > 8 {
			return s[:bar+1] + "…" + s[len(s)-4:]
		}
		return s
	}
	keep := min(2, at)
	return s[:keep] + "***" + s[at:]
}

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// maskWarnings masks any email address quoted in import warnings.
func maskWarnings(ws []store.Warning) []store.Warning {
	out := make([]store.Warning, len(ws))
	for i, w := range ws {
		w.Subject = emailRe.ReplaceAllStringFunc(w.Subject, maskEmail)
		w.Message = emailRe.ReplaceAllStringFunc(w.Message, maskEmail)
		out[i] = w
	}
	return out
}
