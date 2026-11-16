package clitools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnv(t *testing.T) *Env {
	h := t.TempDir()
	return &Env{Home: h, GOOS: "linux", BackupDir: filepath.Join(h, "backups"), Port: 20129}
}

func put(t *testing.T, p, s string) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

const base = "http://127.0.0.1:20129"

func in(model string) Input {
	return Input{BaseURL: base, APIKey: "sk-test_123", Model: model}
}

func TestEveryConfigToolAppliesAndResets(t *testing.T) {
	for _, tl := range All() {
		if tl.Kind != "config" {
			if tl.apply != nil {
				t.Fatalf("%s: guide with writer", tl.ID)
			}
			continue
		}
		e := testEnv(t)
		if s := StatusOf(e, tl); s.Connected {
			t.Fatalf("%s connected before apply", tl.ID)
		}
		res, err := Apply(e, tl, in("cc/claude-sonnet-5"))
		if err != nil || len(res.Written) == 0 {
			t.Fatalf("%s apply: %v %v", tl.ID, err, res)
		}
		s := StatusOf(e, tl)
		if !s.Connected || !s.Installed {
			t.Fatalf("%s not connected after apply: %+v", tl.ID, s)
		}
		if tl.ID != "omp" && tl.ID != "copilot" && s.Model != "cc/claude-sonnet-5" && tl.ID != "claude" {
			t.Fatalf("%s model %q", tl.ID, s.Model)
		}
		if _, err := Reset(e, tl); err != nil {
			t.Fatalf("%s reset: %v", tl.ID, err)
		}
		if s := StatusOf(e, tl); s.Connected {
			t.Fatalf("%s still connected after reset: %+v", tl.ID, s)
		}
	}
}

func TestApplyBacksUpAndKeepsUserSettings(t *testing.T) {
	e := testEnv(t)
	p := e.p(".claude", "settings.json")
	orig := "{\n  // mine\n  \"theme\": \"dark\",\n  \"env\": {\"FOO\": \"1\"},\n}\n"
	put(t, p, orig)
	res, err := Apply(e, Find("claude"), Input{BaseURL: base + "/v1", APIKey: "k", Model: "m", Roles: map[string]string{"haiku": "h"}})
	if err != nil || len(res.Backups) != 1 {
		t.Fatalf("%v %v", err, res)
	}
	if b, _ := os.ReadFile(res.Backups[0]); string(b) != orig {
		t.Fatal("backup differs from original")
	}
	o, _ := readObj(p)
	env := o.Child("env", false)
	if o.Str("theme") != "dark" || env.Str("FOO") != "1" || env.Str("ANTHROPIC_BASE_URL") != base || env.Str("ANTHROPIC_DEFAULT_HAIKU_MODEL") != "h" {
		t.Fatalf("bad result: %s", marshalJSON(o))
	}
	// Re-applying the same values writes nothing.
	res, _ = Apply(e, Find("claude"), Input{BaseURL: base, APIKey: "k", Model: "m", Roles: map[string]string{"haiku": "h"}})
	if len(res.Written) != 0 {
		t.Fatalf("rewrote unchanged file: %v", res.Written)
	}
}

func TestInvalidJSONIsNotOverwritten(t *testing.T) {
	e := testEnv(t)
	p := e.p(".factory", "settings.json")
	put(t, p, "{broken")
	if _, err := Apply(e, Find("droid"), in("m")); err == nil {
		t.Fatal("expected error")
	}
	if b, _ := os.ReadFile(p); string(b) != "{broken" {
		t.Fatal("file changed")
	}
}

func TestValidation(t *testing.T) {
	e := testEnv(t)
	for _, bad := range []Input{
		{BaseURL: base, Model: "m\"; rm"},
		{BaseURL: base, Model: "m", APIKey: "a b"},
		{BaseURL: "file:///x", Model: "m"},
		{BaseURL: base, Model: "m", Roles: map[string]string{"nope": "x"}},
		{BaseURL: base},
	} {
		if _, err := Apply(e, Find("hermes"), bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := Apply(e, Find("cursor"), in("m")); err == nil {
		t.Fatal("guide applied")
	}
}

func TestHermesRolesAudioAndKeepOthers(t *testing.T) {
	e := testEnv(t)
	p := e.p(".hermes", "config.yaml")
	put(t, p, "model:\n  default: \"anthropic/claude\"\n  provider: \"openrouter\"\nterminal:\n  backend: local\nauxiliary:\n  vision:\n    provider: \"openrouter\"\n    model: \"x\"\n")
	put(t, e.p(".hermes", ".env"), "OTHER=1\nOPENAI_API_KEY=old\n")
	tl := Find("hermes")
	_, err := Apply(e, tl, Input{BaseURL: base, APIKey: "sk1", Model: "cx/gpt-5.5", Roles: map[string]string{"compression": "gc/gemini-3-flash", "delegation": "cc/claude-haiku"}, Audio: &Audio{Enabled: true, Model: "openai/whisper-1"}})
	if err != nil {
		t.Fatal(err)
	}
	y := readText(p)
	for _, want := range []string{"terminal:\n  backend: local", "  vision:\n    provider: \"openrouter\"", "compression:", "delegation:", "stt:", `base_url: "` + base + `/v1"`} {
		if !strings.Contains(y, want) {
			t.Fatalf("missing %q in\n%s", want, y)
		}
	}
	env := readText(e.p(".hermes", ".env"))
	if !strings.Contains(env, "OTHER=1") || !strings.Contains(env, "OPENAI_API_KEY=sk1") || strings.Contains(env, "old") {
		t.Fatalf("env: %q", env)
	}
	s := StatusOf(e, tl)
	if !s.Connected || s.Model != "cx/gpt-5.5" || s.Roles["compression"] != "gc/gemini-3-flash" || s.Roles["vision"] != "" || s.Audio != "openai/whisper-1" {
		t.Fatalf("%+v", s)
	}
	// Clearing a role goes back to inherit.
	Apply(e, tl, Input{BaseURL: base, APIKey: "sk1", Model: "cx/gpt-5.5", Audio: &Audio{}})
	s = StatusOf(e, tl)
	if len(s.Roles) != 0 || s.Audio != "" {
		t.Fatalf("roles not cleared: %+v", s)
	}
	Reset(e, tl)
	y = readText(p)
	if strings.Contains(y, "20129") || !strings.Contains(y, "vision:") {
		t.Fatalf("reset:\n%s", y)
	}
}

func TestCodexAndGrokKeepUserConfig(t *testing.T) {
	e := testEnv(t)
	cp := e.p(".codex", "config.toml")
	put(t, cp, "model = \"gpt-5\"\napproval_policy = \"never\"\n\n[mcp_servers.x]\ncommand = \"y\"\n")
	if _, err := Apply(e, Find("codex"), in("cx/gpt-5.5")); err != nil {
		t.Fatal(err)
	}
	c := readText(cp)
	if tomlGet(c, "", "approval_policy") != "never" || tomlGet(c, "mcp_servers.x", "command") != "y" || tomlGet(c, "", "model_provider") != "fastrouter" {
		t.Fatalf("codex:\n%s", c)
	}
	gp := e.p(".grok", "config.toml")
	put(t, gp, "[models]\ndefault = \"grok-4\"\n")
	Apply(e, Find("grok-build"), in("m"))
	if tomlGet(readText(gp), "models", "default") != "fastrouter" {
		t.Fatal(readText(gp))
	}
	Reset(e, Find("grok-build"))
	if g := readText(gp); tomlGet(g, "models", "default") != "grok-4" || strings.Contains(g, "fastrouter") {
		t.Fatalf("grok reset:\n%s", g)
	}
}

func TestOtherRouterDetected(t *testing.T) {
	e := testEnv(t)
	put(t, e.p(".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"http://localhost:20128"}}`)
	if s := StatusOf(e, Find("claude")); s.Connected || s.Other != "9Router" {
		t.Fatalf("%+v", s)
	}
}
