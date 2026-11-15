package server

import (
	"net/http"
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
		"warnings":     snap.Warnings,
	})
}

// maskEmail keeps account names readable without showing full addresses.
func maskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return s
	}
	keep := min(2, at)
	return s[:keep] + "***" + s[at:]
}
