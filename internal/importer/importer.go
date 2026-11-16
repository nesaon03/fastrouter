// Package importer converts a 9Router backup JSON into FastRouter's store.
package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/secret"
	"github.com/zenpo/fastrouter/internal/store"
)

type backup struct {
	Settings            map[string]json.RawMessage `json:"settings"`
	ProviderConnections []map[string]any           `json:"providerConnections"`
	ProviderNodes       []struct {
		ID, Name, Prefix, Type, APIType, BaseURL string
	} `json:"providerNodes"`
	APIKeys []struct {
		ID       string          `json:"id"`
		Key      string          `json:"key"`
		Name     string          `json:"name"`
		IsActive *bool           `json:"isActive"`
		Access   json.RawMessage `json:"access"`
	} `json:"apiKeys"`
	Combos []struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Kind   *string  `json:"kind"`
		Models []string `json:"models"`
	} `json:"combos"`
	CustomModels []struct {
		ProviderAlias string          `json:"providerAlias"`
		ID            string          `json:"id"`
		Type          string          `json:"type"`
		Name          string          `json:"name"`
		Caps          json.RawMessage `json:"caps"`
	} `json:"customModels"`
	// Kept verbatim as settings so nothing from the backup is lost.
	ModelAliases json.RawMessage `json:"modelAliases"`
	MitmAlias    json.RawMessage `json:"mitmAlias"`
	Pricing      json.RawMessage `json:"pricing"`
	ProxyPools   json.RawMessage `json:"proxyPools"`
}

// secretField matches credential-bearing field names (authType/authMethod are not secrets).
var secretField = regexp.MustCompile(`(?i)(apikey|^key$|token|password|secret|cookie|jwt|credential|session)`)

func isSecret(k string) bool {
	return secretField.MatchString(k) && !strings.HasPrefix(strings.ToLower(k), "auth")
}

// Summary holds names and counts only; it never contains secret values.
type Summary struct {
	Connections, ActiveConnections, PluginConnections int
	ConnectionsByProvider                             map[string]int
	Nodes                                             []string // "name (prefix)"
	RebuiltNodes                                      int
	Combos                                            []string
	APIKeys                                           int
	CustomModels, FlaggedModels, PluginModels         int
	SettingsKeys                                      int
	ProviderStrategies, ProviderThinking              int
	DashboardPassword                                 bool
	Warnings                                          []store.Warning
}

// Parse reads a 9Router backup and builds a snapshot plus summary.
func Parse(path string) (*store.Snapshot, *Summary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var b backup
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil, fmt.Errorf("not a 9Router backup: %w", err)
	}
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}}
	sum := &Summary{ConnectionsByProvider: map[string]int{}}
	warn := func(kind, subject, msg string) {
		snap.Warnings = append(snap.Warnings, store.Warning{Kind: kind, Subject: subject, Message: msg})
	}

	// Custom endpoints.
	nodeByID := map[string]bool{}
	prefixOwner := map[string]string{}
	for _, n := range b.ProviderNodes {
		snap.Nodes = append(snap.Nodes, store.Node{ID: n.ID, Name: n.Name, Prefix: n.Prefix, Type: n.Type, APIType: n.APIType, BaseURL: n.BaseURL})
		nodeByID[n.ID] = true
	}
	if renames := fixPrefixes(snap.Nodes); len(renames) > 0 {
		warn("prefix", "custom endpoints", "duplicate prefixes fixed: "+strings.Join(renames, "; "))
	}
	for _, n := range snap.Nodes {
		if owner, dup := prefixOwner[n.Prefix]; dup {
			warn("prefix", n.Prefix, fmt.Sprintf("prefix %q is shared: %s/<model> goes to %q (first match, like 9Router); use %s/<model> to reach %q", n.Prefix, n.Prefix, owner, n.ID, n.Name))
		} else {
			prefixOwner[n.Prefix] = n.Name
		}
	}

	// Provider accounts.
	for _, c := range b.ProviderConnections {
		conn := store.Connection{Secrets: map[string]string{}, Meta: map[string]any{}, Priority: 1, Active: true}
		for k, v := range c {
			switch k {
			case "id":
				conn.ID, _ = v.(string)
			case "provider":
				conn.Provider, _ = v.(string)
			case "name":
				conn.Name, _ = v.(string)
			case "authType":
				conn.AuthType, _ = v.(string)
			case "priority":
				if f, ok := v.(float64); ok {
					conn.Priority = int(f)
				}
			case "isActive":
				if bv, ok := v.(bool); ok {
					conn.Active = bv
				}
			case "providerSpecificData":
				psd, _ := v.(map[string]any)
				keep := map[string]any{}
				for pk, pv := range psd {
					if isSecret(pk) {
						if s, ok := pv.(string); ok && s != "" {
							conn.Secrets["providerSpecificData."+pk] = s
						}
					} else {
						keep[pk] = pv
					}
				}
				conn.Meta["providerSpecificData"] = keep
			case "lastError":
				if s, ok := v.(string); ok {
					conn.Meta[k] = secret.Redact(s)
				}
			default:
				if strings.HasPrefix(k, "modelLock_") || v == nil {
					continue // transient 9Router lock state
				}
				if isSecret(k) {
					if s, ok := v.(string); ok && s != "" {
						conn.Secrets[k] = s
					}
					continue
				}
				conn.Meta[k] = v
			}
		}
		conn.Status = "ok"
		if p, ok := catalog.Lookup(conn.Provider); ok {
			if p.Plugin {
				conn.Status = "needs-plugin"
				sum.PluginConnections++
			}
		} else if !nodeByID[conn.Provider] {
			psd, _ := conn.Meta["providerSpecificData"].(map[string]any)
			base, _ := psd["baseUrl"].(string)
			if strings.HasPrefix(conn.Provider, "openai-compatible") && base != "" {
				id := conn.Provider
				if id == "openai-compatible-chat" {
					id += "-" + conn.ID
				}
				prefix, _ := psd["prefix"].(string)
				name, _ := psd["nodeName"].(string)
				apiType, _ := psd["apiType"].(string)
				snap.Nodes = append(snap.Nodes, store.Node{ID: id, Name: name, Prefix: prefix, Type: "openai-compatible", APIType: apiType, BaseURL: base})
				nodeByID[id] = true
				conn.Provider = id
				sum.RebuiltNodes++
				warn("node", name, fmt.Sprintf("endpoint %q had no provider node; rebuilt it from its account (prefix %q)", name, prefix))
			} else {
				conn.Status = "unknown-provider"
				warn("account", conn.Provider, "account's provider is not known to FastRouter core; imported but unused")
			}
		}
		sum.Connections++
		if conn.Active {
			sum.ActiveConnections++
		}
		sum.ConnectionsByProvider[displayProvider(conn.Provider, snap.Nodes)]++
		snap.Connections = append(snap.Connections, conn)
	}
	if sum.PluginConnections > 0 {
		warn("plugin", "oauth", fmt.Sprintf("%d login-based accounts (antigravity, codex, cursor, glm, opencode) are stored but need the deferred plugin; core skips them", sum.PluginConnections))
	}

	// Settings (password kept exactly as-is, encrypted at rest).
	for k, v := range b.Settings {
		if k == "password" {
			_ = json.Unmarshal(v, &snap.DashboardPassword)
			sum.DashboardPassword = snap.DashboardPassword != ""
			continue
		}
		snap.Settings[k] = v
	}
	for k, v := range map[string]json.RawMessage{"modelAliases": b.ModelAliases, "mitmAlias": b.MitmAlias, "pricing": b.Pricing, "proxyPools": b.ProxyPools} {
		if len(v) > 0 {
			snap.Settings["backup."+k] = v
		}
	}
	sum.SettingsKeys = len(snap.Settings)
	sum.ProviderStrategies = countKeys(b.Settings["providerStrategies"])
	sum.ProviderThinking = countKeys(b.Settings["providerThinking"])

	// Combos.
	var comboStrategies map[string]struct {
		FallbackStrategy string `json:"fallbackStrategy"`
	}
	_ = json.Unmarshal(b.Settings["comboStrategies"], &comboStrategies)
	for _, c := range b.Combos {
		combo := store.Combo{ID: c.ID, Name: c.Name, Models: c.Models, Strategy: "fallback"}
		if c.Kind != nil {
			combo.Kind = *c.Kind
		}
		if s := comboStrategies[c.Name].FallbackStrategy; s != "" {
			combo.Strategy = s
		}
		for _, m := range c.Models {
			prefix, _, _ := strings.Cut(m, "/")
			if p, ok := catalog.Lookup(prefix); ok && p.Plugin {
				warn("combo", c.Name, fmt.Sprintf("combo %q: %s needs the deferred plugin; skipped at runtime", c.Name, m))
			}
		}
		if len(c.Models) == 0 {
			warn("combo", c.Name, fmt.Sprintf("combo %q has no models", c.Name))
		}
		snap.Combos = append(snap.Combos, combo)
		sum.Combos = append(sum.Combos, fmt.Sprintf("%s (%s, %d models)", c.Name, combo.Strategy, len(c.Models)))
	}

	// Local API keys.
	for _, k := range b.APIKeys {
		active := k.IsActive == nil || *k.IsActive
		snap.APIKeys = append(snap.APIKeys, store.APIKey{ID: k.ID, Name: k.Name, Key: k.Key, Active: active, Access: k.Access})
	}
	sum.APIKeys = len(snap.APIKeys)

	// Custom models: flag the ones whose provider no longer exists.
	nodePrefixes := map[string]bool{}
	for _, n := range snap.Nodes {
		nodePrefixes[n.Prefix] = true
	}
	flagged := map[string]int{}
	for _, m := range b.CustomModels {
		_, builtin := catalog.Lookup(m.ProviderAlias)
		if !builtin && !nodeByID[m.ProviderAlias] && !nodePrefixes[m.ProviderAlias] {
			flagged[m.ProviderAlias]++
			continue
		}
		if p, ok := catalog.Lookup(m.ProviderAlias); ok && p.Plugin {
			sum.PluginModels++
		}
		snap.CustomModels = append(snap.CustomModels, store.CustomModel{ProviderAlias: m.ProviderAlias, ID: m.ID, Type: m.Type, Name: m.Name, Caps: m.Caps})
	}
	for _, alias := range sortedKeys(flagged) {
		warn("custom-model", alias, fmt.Sprintf("%d custom model(s) point at provider %q, which no longer exists; not imported", flagged[alias], alias))
		sum.FlaggedModels += flagged[alias]
	}
	sum.CustomModels = len(snap.CustomModels)

	for _, n := range snap.Nodes {
		sum.Nodes = append(sum.Nodes, fmt.Sprintf("%s (%s)", n.Name, n.Prefix))
	}
	sum.Warnings = snap.Warnings
	return snap, sum, nil
}

// Run parses path and replaces the store's configuration with it.
func Run(st *store.Store, path string) (*Summary, error) {
	snap, sum, err := Parse(path)
	if err != nil {
		return nil, err
	}
	return sum, st.ReplaceAll(snap)
}

func displayProvider(id string, nodes []store.Node) string {
	for _, n := range nodes {
		if n.ID == id {
			return "custom:" + n.Name
		}
	}
	return id
}

func countKeys(raw json.RawMessage) int {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return len(m)
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Print writes the summary (names and counts only).
func (s *Summary) Print(w interface{ Write([]byte) (int, error) }) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f+"\n", a...) }
	p("Imported 9Router backup")
	p("  Accounts:        %d (%d active, %d need the deferred login plugin)", s.Connections, s.ActiveConnections, s.PluginConnections)
	for _, k := range sortedKeys(s.ConnectionsByProvider) {
		p("    %-28s %d", k, s.ConnectionsByProvider[k])
	}
	p("  Custom endpoints: %d (%d rebuilt from account data)", len(s.Nodes), s.RebuiltNodes)
	for _, n := range s.Nodes {
		p("    %s", n)
	}
	p("  Combos:          %d", len(s.Combos))
	for _, c := range s.Combos {
		p("    %s", c)
	}
	p("  Local API keys:  %d (encrypted)", s.APIKeys)
	p("  Custom models:   %d imported (%d of them under plugin providers), %d flagged (provider gone)", s.CustomModels, s.PluginModels, s.FlaggedModels)
	p("  Settings:        %d keys (%d provider strategies, %d thinking overrides)", s.SettingsKeys, s.ProviderStrategies, s.ProviderThinking)
	p("  Dashboard password: %v (kept as-is, encrypted)", s.DashboardPassword)
	p("  Warnings:        %d", len(s.Warnings))
	for _, wn := range s.Warnings {
		p("    [%s] %s", wn.Kind, wn.Message)
	}
}
