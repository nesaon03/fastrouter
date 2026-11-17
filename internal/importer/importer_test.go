package importer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

const fixture = "../../testdata/9router-backup.redacted.json"

func TestImportRedactedBackup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sum, err := Run(st, fixture)
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name      string
		got, want int
	}{
		{"connections", sum.Connections, 23},
		{"active connections", sum.ActiveConnections, 20},
		{"plugin connections", sum.PluginConnections, 0}, // cursor is handled by internal/oauth now (antigravity too; codex and glm by internal/login)
		{"nodes (7 + 1 rebuilt)", len(sum.Nodes), 8},
		{"rebuilt nodes", sum.RebuiltNodes, 1},
		{"combos", len(sum.Combos), 4},
		{"api keys", sum.APIKeys, 1},
		{"custom models imported", sum.CustomModels, 25},
		{"custom models flagged", sum.FlaggedModels, 10},
		{"provider strategies", sum.ProviderStrategies, 11},
		{"provider thinking", sum.ProviderThinking, 3},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if !sum.DashboardPassword {
		t.Error("dashboard password not imported")
	}

	// Round-trip through SQLite.
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.DashboardPassword != "REDACTED-password-1" {
		t.Errorf("dashboard password changed on import")
	}
	byID := map[string]store.Connection{}
	for _, c := range snap.Connections {
		byID[c.ID] = c
	}
	// isActive and priority are preserved.
	if c := byID["71476afd-75ee-4c10-bb73-9942f6865aa7"]; c.Active || c.Priority != 1 || c.Provider != "opencode-go" {
		t.Errorf("opencode-go test account: active=%v priority=%d provider=%s", c.Active, c.Priority, c.Provider)
	}
	prios := map[int]bool{}
	for _, c := range snap.Connections {
		if c.Provider == "antigravity" {
			prios[c.Priority] = true
			if c.Status != "ok" { // signed in through internal/oauth now
				t.Errorf("antigravity account status = %q, want ok", c.Status)
			}
		}
		want := map[string]string{"cursor": "ok", "codex": "ok", "glm": "ok"}[c.Provider]
		if want != "" && c.Status != want {
			t.Errorf("%s status = %q, want %q", c.Provider, c.Status, want)
		}
		if c.Provider == "codex" && (c.Secrets["accessToken"] == "" || c.Secrets["refreshToken"] == "") {
			t.Error("codex tokens not kept")
		}
		if c.Provider == "deepseek" && c.Secrets["apiKey"] == "" {
			t.Error("deepseek apiKey not decrypted on load")
		}
		for k := range c.Meta {
			if isSecret(k) {
				t.Errorf("secret field %q leaked into plaintext meta", k)
			}
		}
	}
	if len(prios) != 3 {
		t.Errorf("antigravity priorities = %v, want 1,2,3", prios)
	}
	for _, k := range []string{"comboStrategies", "providerStrategies", "providerThinking", "capacityAdapter"} {
		if len(snap.Settings[k]) == 0 {
			t.Errorf("setting %s not imported", k)
		}
	}
	strat := map[string]string{}
	for _, c := range snap.Combos {
		strat[c.Name] = c.Strategy
	}
	if strat["my-combo-team"] != "round-robin" || strat["research"] != "fallback" {
		t.Errorf("combo strategies = %v", strat)
	}

	// No secret value may appear in the database file in plaintext.
	db, _ := os.ReadFile(filepath.Join(dir, "fastrouter.db"))
	wal, _ := os.ReadFile(filepath.Join(dir, "fastrouter.db-wal"))
	raw := append(db, wal...)
	for _, needle := range []string{"REDACTED-apiKey", "REDACTED-accessToken", "REDACTED-refreshToken", "REDACTED-key", "REDACTED-password", "REDACTED-zaiBusinessToken"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Errorf("found %q in plaintext in the database", needle)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "secret.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("secret.key mode = %v, err %v", fi.Mode().Perm(), err)
	}
}

func TestFixtureHasNoRealSecrets(t *testing.T) {
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	snap, _, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Connections {
		for k, v := range c.Secrets {
			if !bytes.HasPrefix([]byte(v), []byte("REDACTED-")) {
				t.Errorf("fixture secret %s on %s is not redacted", k, c.Provider)
			}
		}
	}
	for _, k := range snap.APIKeys {
		if !bytes.HasPrefix([]byte(k.Key), []byte("REDACTED-")) {
			t.Error("fixture api key not redacted")
		}
	}
	if bytes.Contains(raw, []byte("@gmail.com")) {
		t.Error("fixture contains an email address")
	}
}
