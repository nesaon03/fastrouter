package clitools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Role is an optional extra model slot a tool supports.
type Role struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Note is a hint shown on a tool page.
type Note struct {
	Type string `json:"type"` // info | warning | error
	Text string `json:"text"`
}

// Step is one guide step. Value may hold {{baseUrl}}, {{apiKey}}, {{model}}.
type Step struct {
	Title string `json:"title"`
	Desc  string `json:"desc,omitempty"`
	Value string `json:"value,omitempty"`
	Type  string `json:"type,omitempty"` // "" | apiKey | model
}

// Tool describes one coding tool and how to point it at FastRouter.
type Tool struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Logo        string `json:"logo"`
	Description string `json:"description"`
	Kind        string `json:"kind"` // config | guide
	Docs        string `json:"docs,omitempty"`
	Install     string `json:"install,omitempty"`
	Notes       []Note `json:"notes,omitempty"`
	Steps       []Step `json:"steps,omitempty"`
	Code        string `json:"code,omitempty"`
	Roles       []Role `json:"roles,omitempty"`
	RolesTitle  string `json:"rolesTitle,omitempty"`
	RolesOpen   bool   `json:"rolesOpen,omitempty"` // show the roles expanded (e.g. Claude Code's model slots)
	Audio       bool   `json:"audio,omitempty"`     // Audio STT option (Hermes)
	NeedsModel  bool   `json:"needsModel"`

	bins   []string
	status func(e *Env) Status
	apply  func(e *Env, in Input) error
	reset  func(e *Env) error
}

// Status is what a tool's config says right now (never key values).
type Status struct {
	Installed  bool              `json:"installed"`
	Connected  bool              `json:"connected"`
	Other      string            `json:"other,omitempty"` // points at another router (9Router)
	ConfigPath string            `json:"configPath,omitempty"`
	BaseURL    string            `json:"baseUrl,omitempty"`
	Model      string            `json:"model,omitempty"`
	Roles      map[string]string `json:"roles,omitempty"`
	Audio      string            `json:"audio,omitempty"`
	HasKey     bool              `json:"hasKey,omitempty"`
}

// Input is what Apply writes. BaseURL is FastRouter's root (no /v1).
type Input struct {
	BaseURL string
	APIKey  string
	Model   string
	Roles   map[string]string
	Audio   *Audio
}

// Audio is Hermes' speech-to-text option.
type Audio struct {
	Enabled bool   `json:"enabled"`
	Model   string `json:"model"`
}

// Result lists the files an Apply or Reset wrote and where the originals went.
type Result struct {
	Written []string `json:"written"`
	Backups []string `json:"backups"`
}

const provID = "fastrouter"

var tools []*Tool

func reg(t *Tool) { tools = append(tools, t) }

// All returns every tool in 9Router's order.
func All() []*Tool { return tools }

// Find returns the tool with id.
func Find(id string) *Tool {
	for _, t := range tools {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// StatusOf reads a tool's current state (read-only).
func StatusOf(e *Env, t *Tool) Status {
	var st Status
	if t.status != nil {
		st = t.status(e)
	}
	if !st.Installed && len(t.bins) > 0 {
		st.Installed = e.onPath(t.bins...)
	}
	return st
}

var errGuide = errors.New("this tool is set up by hand; follow the guide")

// Apply writes FastRouter into the tool's config, backing up every file first.
func Apply(e *Env, t *Tool, in Input) (Result, error) {
	if t.apply == nil {
		return Result{}, errGuide
	}
	if !strings.HasPrefix(in.BaseURL, "http://") && !strings.HasPrefix(in.BaseURL, "https://") || strings.ContainsAny(in.BaseURL, " \"'\n\r\t") {
		return Result{}, errors.New("invalid endpoint")
	}
	in.BaseURL = strings.TrimSuffix(strings.TrimSuffix(in.BaseURL, "/"), "/v1")
	if in.APIKey != "" && !keyRe.MatchString(in.APIKey) {
		return Result{}, errors.New("invalid API key")
	}
	if t.NeedsModel && in.Model == "" {
		return Result{}, errors.New("choose a default model")
	}
	for _, m := range append([]string{in.Model}, mapVals(in.Roles)...) {
		if m != "" && !modelRe.MatchString(m) {
			return Result{}, fmt.Errorf("invalid model id %q", m)
		}
	}
	for r := range in.Roles {
		if !roleKnown(t, r) {
			return Result{}, fmt.Errorf("unknown role %q", r)
		}
	}
	if in.Audio != nil && in.Audio.Enabled && !modelRe.MatchString(in.Audio.Model) {
		return Result{}, errors.New("choose an STT model")
	}
	e.begin(t.ID)
	err := t.apply(e, in)
	return Result{Written: e.written, Backups: e.backups}, err
}

// Reset removes FastRouter's entries from the tool's config (backed up first).
func Reset(e *Env, t *Tool) (Result, error) {
	if t.reset == nil {
		return Result{}, errGuide
	}
	e.begin(t.ID)
	err := t.reset(e)
	return Result{Written: e.written, Backups: e.backups}, err
}

func roleKnown(t *Tool, id string) bool {
	for _, r := range t.Roles {
		if r.ID == id {
			return true
		}
	}
	return false
}

func mapVals(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func keyOr(k string) string {
	if k == "" {
		return "fastrouter-local" // no FastRouter keys: local clients are allowed without one
	}
	return k
}

func (in Input) v1() string { return in.BaseURL + "/v1" }

// st fills the common fields from a base URL.
func st(path, base string, installed bool) Status {
	s := Status{Installed: installed || exists(path), ConfigPath: path, BaseURL: base}
	return s
}

func (s *Status) mark(e *Env, base string) {
	if base == "" {
		return
	}
	if e.ours(base) {
		s.Connected = true
	} else if o := other(base); o != "" {
		s.Other = o
	}
}

func objArr(v any) []any {
	a, _ := v.([]any)
	return a
}

func init() {
	// ---------------------------------------------------------------- Claude Code
	reg(&Tool{ID: "claude", Name: "Claude Code", Logo: "claude.png", Description: "Anthropic Claude Code CLI", Kind: "config", NeedsModel: false,
		bins: []string{"claude"}, Docs: "https://docs.anthropic.com/en/docs/claude-code",
		Roles: []Role{{"opus", "Opus slot"}, {"sonnet", "Sonnet slot"}, {"haiku", "Haiku slot (background tasks)"}, {"fable", "Fable slot"}}, RolesTitle: "Model slots", RolesOpen: true,
		Notes: []Note{{"info", "Writes the env block of ~/.claude/settings.json: ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, ANTHROPIC_MODEL and the per-slot model variables."}},
		status: func(e *Env) Status {
			p := e.p(".claude", "settings.json")
			env := peekObj(p).path("env")
			s := st(p, env.Str("ANTHROPIC_BASE_URL"), false)
			s.mark(e, s.BaseURL)
			s.Model, s.HasKey = env.Str("ANTHROPIC_MODEL"), env.Str("ANTHROPIC_AUTH_TOKEN") != ""
			s.Roles = map[string]string{}
			for r, k := range claudeSlots {
				if v := env.Str(k); v != "" {
					s.Roles[r] = v
				}
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".claude", "settings.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			o.Set("hasCompletedOnboarding", true)
			env := o.Child("env", true)
			env.Set("ANTHROPIC_BASE_URL", in.BaseURL)
			env.Set("ANTHROPIC_AUTH_TOKEN", keyOr(in.APIKey))
			env.Set("API_TIMEOUT_MS", "600000")
			setOrDel(env, "ANTHROPIC_MODEL", in.Model)
			for r, k := range claudeSlots {
				setOrDel(env, k, in.Roles[r])
			}
			return e.writeObj(p, o)
		},
		reset: func(e *Env) error {
			p := e.p(".claude", "settings.json")
			if !exists(p) {
				return nil
			}
			o, err := readObj(p)
			if err != nil {
				return err
			}
			if env := o.Child("env", false); env != nil {
				for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "API_TIMEOUT_MS", "CLAUDE_CODE_AUTO_COMPACT_WINDOW"} {
					env.Del(k)
				}
				for _, k := range claudeSlots {
					env.Del(k)
				}
				if env.Len() == 0 {
					o.Del("env")
				}
			}
			return e.writeObj(p, o)
		},
	})

	// ---------------------------------------------------------------- Open Claw
	reg(&Tool{ID: "openclaw", Name: "Open Claw", Logo: "openclaw.png", Description: "Open Claw AI Assistant", Kind: "config", NeedsModel: true,
		bins: []string{"openclaw"}, Notes: []Note{{"info", "Adds a \"fastrouter\" provider to ~/.openclaw/openclaw.json and makes it the default agent model."}},
		status: func(e *Env) Status {
			p := e.p(".openclaw", "openclaw.json")
			o := peekObj(p)
			prov := o.path("models", "providers", provID)
			s := st(p, prov.Str("baseUrl"), false)
			s.mark(e, s.BaseURL)
			if s.BaseURL == "" && o.path("models", "providers", "9router") != nil {
				s.Other = "9Router"
			}
			s.Model = strings.TrimPrefix(o.path("agents", "defaults", "model").Str("primary"), provID+"/")
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".openclaw", "openclaw.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			def := o.Child("agents", true).Child("defaults", true)
			def.Child("model", true).Set("primary", provID+"/"+in.Model)
			ms := def.Child("models", true)
			for _, k := range ms.Keys() {
				if strings.HasPrefix(k, provID+"/") {
					ms.Del(k)
				}
			}
			ms.Set(provID+"/"+in.Model, newObj())
			o.Child("models", true).Child("providers", true).Set(provID, obj("baseUrl", in.v1(), "apiKey", keyOr(in.APIKey), "api", "openai-completions",
				"models", []any{obj("id", in.Model, "name", lastPart(in.Model))}))
			return e.writeObj(p, o)
		},
		reset: jsonReset(func(e *Env) string { return e.p(".openclaw", "openclaw.json") }, func(o *Obj) {
			if ps := o.path("models", "providers"); ps != nil {
				ps.Del(provID)
			}
			if ms := o.path("agents", "defaults", "models"); ms != nil {
				for _, k := range ms.Keys() {
					if strings.HasPrefix(k, provID+"/") {
						ms.Del(k)
					}
				}
			}
			if m := o.path("agents", "defaults", "model"); m != nil && strings.HasPrefix(m.Str("primary"), provID+"/") {
				m.Del("primary")
			}
		}),
	})

	// ---------------------------------------------------------------- Codex
	reg(&Tool{ID: "codex", Name: "OpenAI Codex CLI / App", Logo: "codex.png", Description: "OpenAI Codex CLI", Kind: "config", NeedsModel: true,
		bins: []string{"codex"}, Roles: []Role{{"subagent", "Subagent model"}},
		Notes: []Note{{"info", "Writes model_provider = \"fastrouter\" and a [model_providers.fastrouter] table (Responses API) to ~/.codex/config.toml."}},
		status: func(e *Env) Status {
			p := e.p(".codex", "config.toml")
			t := readText(p)
			s := st(p, tomlGet(t, "model_providers."+provID, "base_url"), false)
			if tomlGet(t, "", "model_provider") == provID {
				s.mark(e, s.BaseURL)
				s.Model = tomlGet(t, "", "model")
			} else if tomlGet(t, "", "model_provider") == "9router" {
				s.Other = "9Router"
			}
			if v := tomlGet(t, "agents", "default_subagent_model"); v != "" {
				s.Roles = map[string]string{"subagent": v}
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".codex", "config.toml")
			t := readText(p)
			t = tomlSet(t, "", "model", tomlStr(in.Model))
			t = tomlSet(t, "", "model_provider", tomlStr(provID))
			t = tomlReplaceSection(t, "model_providers."+provID, []string{
				`name = "FastRouter"`, "base_url = " + tomlStr(in.v1()), `wire_api = "responses"`,
				"http_headers = { Authorization = " + tomlStr("Bearer "+keyOr(in.APIKey)) + " }"})
			sub := in.Roles["subagent"]
			if sub == "" {
				sub = in.Model
			}
			t = tomlSet(t, "agents", "default_subagent_model", tomlStr(sub))
			return e.write(p, []byte(t))
		},
		reset: func(e *Env) error {
			p := e.p(".codex", "config.toml")
			if !exists(p) {
				return nil
			}
			t := readText(p)
			if tomlGet(t, "", "model_provider") == provID {
				t = tomlDel(t, "", "model")
				t = tomlDel(t, "", "model_provider")
			}
			t = tomlRemoveSection(t, "model_providers."+provID)
			t = tomlDel(t, "agents", "default_subagent_model")
			return e.write(p, []byte(t))
		},
	})

	// ---------------------------------------------------------------- GitHub Copilot (VS Code)
	reg(&Tool{ID: "copilot", Name: "GitHub Copilot", Logo: "copilot.png", Description: "GitHub Copilot Chat in VS Code (bring your own model)", Kind: "config", NeedsModel: true,
		bins: []string{"code"}, Roles: []Role{{"extra1", "Extra model 1"}, {"extra2", "Extra model 2"}, {"extra3", "Extra model 3"}}, RolesTitle: "More models (optional)",
		Notes: []Note{{"info", "Adds a \"FastRouter\" entry to VS Code's chatLanguageModels.json. In Copilot Chat open the model picker → Manage Models… and tick the FastRouter models."}},
		status: func(e *Env) Status {
			p := filepath.Join(e.vscodeUser(), "chatLanguageModels.json")
			s := st(p, "", exists(e.vscodeUser()))
			if b, err := os.ReadFile(p); err == nil {
				if v, err := parseJSON(b); err == nil {
					for _, it := range objArr(v) {
						if o, ok := it.(*Obj); ok && (o.Str("name") == "FastRouter" || o.Str("name") == "9Router") {
							if ms := objArr(o.Get("models")); len(ms) > 0 {
								m, _ := ms[0].(*Obj)
								if o.Str("name") == "FastRouter" {
									s.BaseURL, s.Model = m.Str("url"), m.Str("id")
									s.mark(e, s.BaseURL)
								} else if !s.Connected {
									s.Other = "9Router"
								}
							}
						}
					}
				}
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := filepath.Join(e.vscodeUser(), "chatLanguageModels.json")
			var arr []any
			if b, err := os.ReadFile(p); err == nil {
				v, err := parseJSON(b)
				if err != nil {
					return fmt.Errorf("chatLanguageModels.json is not valid JSON (%v)", err)
				}
				if a, ok := v.([]any); ok {
					arr = a
				}
			}
			var models []any
			for _, m := range append([]string{in.Model}, in.Roles["extra1"], in.Roles["extra2"], in.Roles["extra3"]) {
				if m != "" {
					models = append(models, obj("id", m, "name", m, "url", in.v1()+"/chat/completions#models.ai.azure.com", "toolCalling", true,
						"vision", false, "maxInputTokens", 128000, "maxOutputTokens", 16000))
				}
			}
			entry := obj("name", "FastRouter", "vendor", "azure", "apiKey", keyOr(in.APIKey), "models", models)
			done := false
			for i, it := range arr {
				if o, ok := it.(*Obj); ok && o.Str("name") == "FastRouter" {
					arr[i], done = entry, true
				}
			}
			if !done {
				arr = append(arr, entry)
			}
			return e.write(p, marshalJSON(arr))
		},
		reset: func(e *Env) error {
			p := filepath.Join(e.vscodeUser(), "chatLanguageModels.json")
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			v, err := parseJSON(b)
			if err != nil {
				return err
			}
			var out []any
			for _, it := range objArr(v) {
				if o, ok := it.(*Obj); ok && o.Str("name") == "FastRouter" {
					continue
				}
				out = append(out, it)
			}
			return e.write(p, marshalJSON(append([]any{}, out...)))
		},
	})

	// ---------------------------------------------------------------- OpenCode
	reg(&Tool{ID: "opencode", Name: "OpenCode", Logo: "opencode.png", Description: "OpenCode AI Terminal Assistant", Kind: "config", NeedsModel: true,
		bins: []string{"opencode"}, Roles: []Role{{"subagent", "Explorer subagent"}},
		Notes: []Note{{"info", "Adds provider \"fastrouter\" (@ai-sdk/openai-compatible) to ~/.config/opencode/opencode.json and selects the model."}},
		status: func(e *Env) Status {
			p := e.p(".config", "opencode", "opencode.json")
			o := peekObj(p)
			prov := o.path("provider", provID)
			s := st(p, prov.path("options").Str("baseURL"), false)
			s.mark(e, s.BaseURL)
			if s.BaseURL == "" && o.path("provider", "9router") != nil {
				s.Other = "9Router"
			}
			if m := o.Str("model"); strings.HasPrefix(m, provID+"/") {
				s.Model = strings.TrimPrefix(m, provID+"/")
			}
			if m := o.path("agent", "explorer").Str("model"); strings.HasPrefix(m, provID+"/") {
				s.Roles = map[string]string{"subagent": strings.TrimPrefix(m, provID+"/")}
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".config", "opencode", "opencode.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			prov := o.Child("provider", true).Child(provID, true)
			if !prov.Has("npm") {
				prov.Set("npm", "@ai-sdk/openai-compatible")
			}
			prov.Set("name", "FastRouter")
			opts := prov.Child("options", true)
			opts.Set("baseURL", in.v1())
			opts.Set("apiKey", keyOr(in.APIKey))
			ms := prov.Child("models", true)
			sub := in.Roles["subagent"]
			if sub == "" {
				sub = in.Model
			}
			for _, m := range []string{in.Model, sub} {
				if !ms.Has(m) {
					ms.Set(m, obj("name", m, "modalities", obj("input", []any{"text", "image"}, "output", []any{"text"})))
				}
			}
			o.Set("model", provID+"/"+in.Model)
			o.Child("agent", true).Set("explorer", obj("description", "Fast explorer subagent for codebase exploration", "mode", "subagent", "model", provID+"/"+sub))
			return e.writeObj(p, o)
		},
		reset: jsonReset(func(e *Env) string { return e.p(".config", "opencode", "opencode.json") }, func(o *Obj) {
			if pv := o.Child("provider", false); pv != nil {
				pv.Del(provID)
			}
			if strings.HasPrefix(o.Str("model"), provID+"/") {
				o.Del("model")
			}
			if ag := o.Child("agent", false); ag != nil && strings.HasPrefix(ag.path("explorer").Str("model"), provID+"/") {
				ag.Del("explorer")
				if ag.Len() == 0 {
					o.Del("agent")
				}
			}
		}),
	})

	// ---------------------------------------------------------------- Claude Cowork (guide)
	reg(&Tool{ID: "cowork", Name: "Claude Cowork", Logo: "claude.png", Description: "Claude Desktop Cowork (third-party inference)", Kind: "guide",
		status: coworkStatus,
		Notes:  []Note{{"warning", "FastRouter does not write Claude Desktop's config: 9Router's writer also relaxes Desktop security settings (extension signatures, egress, telemetry). Enter the gateway by hand instead."}},
		Steps: []Step{
			{Title: "Switch Claude Desktop to third-party mode", Desc: "In Claude Desktop open Settings → Developer → Third-party inference (deployment mode \"3p\")."},
			{Title: "Gateway base URL", Desc: "Choose the Gateway provider and paste:", Value: "{{baseUrl}}"},
			{Title: "API key", Type: "apiKey"},
			{Title: "Models", Desc: "Add the model ids Cowork should offer:", Type: "model"},
			{Title: "Restart", Desc: "Quit and reopen Claude Desktop."}}})

	// ---------------------------------------------------------------- Hermes Agent
	reg(&Tool{ID: "hermes", Name: "Hermes Agent", Logo: "hermes.png", Description: "Nous Research self-improving AI agent", Kind: "config", NeedsModel: true, Audio: true,
		bins: []string{"hermes"}, Install: "curl -fsSL https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.sh | bash",
		RolesTitle: "Model Roles (optional)", Roles: hermesRoles,
		Notes: []Note{{"info", "Writes custom-provider model blocks to ~/.hermes/config.yaml and OPENAI_API_KEY to ~/.hermes/.env (Hermes reads ${OPENAI_API_KEY} from there)."}},
		status: func(e *Env) Status {
			p := e.p(".hermes", "config.yaml")
			y := readText(p)
			s := st(p, "", false)
			if m := hermesModel(y); m != nil && m.Provider == "custom" {
				s.BaseURL, s.Model = m.BaseURL, m.Model
				s.mark(e, m.BaseURL)
			}
			s.Roles = map[string]string{}
			if d := hermesDelegation(y); d != nil && d.Provider == "custom" && e.ours(d.BaseURL) {
				s.Roles["delegation"] = d.Model
				s.Connected = true
			}
			for r, b := range hermesAux(y) {
				if b.Provider == "custom" && e.ours(b.BaseURL) {
					s.Roles[r] = b.Model
					s.Connected = true
				}
			}
			if a := hermesSTT(y); a.ours() && e.ours(a.BaseURL) {
				s.Audio = a.Model
			}
			s.HasKey = strings.Contains(readText(e.p(".hermes", ".env")), "OPENAI_API_KEY=")
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".hermes", "config.yaml")
			y := readText(p)
			base := in.v1()
			y = hermesSetModel(y, in.Model, base)
			if m := in.Roles["delegation"]; m != "" {
				y = hermesSetDelegation(y, m, base)
			} else if d := hermesDelegation(y); d != nil && d.Provider == "custom" && e.ours(d.BaseURL) {
				y = strings.TrimLeft(replaceFirst(hDelegation, y, ""), "\n")
			}
			cur := hermesAux(y)
			for _, r := range hermesRoles {
				if r.ID == "delegation" {
					continue
				}
				if m := in.Roles[r.ID]; m != "" {
					y = hermesSetAux(y, r.ID, m, base)
				} else if b, ok := cur[r.ID]; ok && b.Provider == "custom" && e.ours(b.BaseURL) {
					y = hermesDelAux(y, r.ID) // cleared: inherit the default again
				}
			}
			if in.Audio != nil {
				if in.Audio.Enabled {
					y = hermesSetSTT(y, in.Audio.Model, base)
				} else if hermesSTT(y).ours() {
					y = hermesDelSTT(y)
				}
			}
			if err := e.write(p, []byte(y)); err != nil {
				return err
			}
			envP := e.p(".hermes", ".env")
			return e.write(envP, []byte(upsertEnv(readText(envP), "OPENAI_API_KEY", keyOr(in.APIKey))))
		},
		reset: func(e *Env) error {
			p := e.p(".hermes", "config.yaml")
			if !exists(p) {
				return nil
			}
			y := readText(p)
			if m := hermesModel(y); m != nil && m.Provider == "custom" && e.ours(m.BaseURL) {
				y = strings.TrimLeft(replaceFirst(hModelBlock, y, ""), "\n")
			}
			if d := hermesDelegation(y); d != nil && d.Provider == "custom" && e.ours(d.BaseURL) {
				y = replaceFirst(hDelegation, y, "")
			}
			for r, b := range hermesAux(y) {
				if b.Provider == "custom" && e.ours(b.BaseURL) {
					y = hermesDelAux(y, r)
				}
			}
			if a := hermesSTT(y); a.ours() && e.ours(a.BaseURL) {
				y = hermesDelSTT(y)
			}
			return e.write(p, []byte(strings.TrimLeft(y, "\n")))
		},
	})

	// ---------------------------------------------------------------- Factory Droid
	reg(&Tool{ID: "droid", Name: "Factory Droid", Logo: "droid.png", Description: "Factory Droid AI Assistant", Kind: "config", NeedsModel: true,
		bins: []string{"droid"}, Notes: []Note{{"info", "Adds custom models (custom:FastRouter-N) to ~/.factory/settings.json; the default model is listed first."}},
		status: func(e *Env) Status {
			p := e.p(".factory", "settings.json")
			s := st(p, "", false)
			for _, it := range objArr(peekObj(p).Get("customModels")) {
				o, _ := it.(*Obj)
				id := o.Str("id")
				if strings.HasPrefix(id, "custom:FastRouter") && s.Model == "" {
					s.BaseURL, s.Model = o.Str("baseUrl"), o.Str("model")
					s.mark(e, s.BaseURL)
				} else if strings.HasPrefix(id, "custom:9Router") && !s.Connected {
					s.Other = "9Router"
				}
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".factory", "settings.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			list := []any{obj("model", in.Model, "id", "custom:FastRouter-0", "index", 0, "baseUrl", in.v1(), "apiKey", keyOr(in.APIKey),
				"displayName", in.Model, "maxOutputTokens", 131072, "noImageSupport", false, "provider", "openai")}
			for _, it := range objArr(o.Get("customModels")) {
				if c, ok := it.(*Obj); ok && strings.HasPrefix(c.Str("id"), "custom:FastRouter") {
					continue
				}
				list = append(list, it)
			}
			for i, it := range list {
				if c, ok := it.(*Obj); ok && c.Has("index") {
					c.Set("index", i)
				}
			}
			o.Set("customModels", list)
			return e.writeObj(p, o)
		},
		reset: jsonReset(func(e *Env) string { return e.p(".factory", "settings.json") }, func(o *Obj) {
			var keep []any
			for _, it := range objArr(o.Get("customModels")) {
				if c, ok := it.(*Obj); ok && strings.HasPrefix(c.Str("id"), "custom:FastRouter") {
					continue
				}
				keep = append(keep, it)
			}
			if len(keep) == 0 {
				o.Del("customModels")
			} else {
				o.Set("customModels", keep)
			}
		}),
	})

	// ---------------------------------------------------------------- Cursor (guide)
	reg(&Tool{ID: "cursor", Name: "Cursor", Logo: "cursor.png", Description: "Cursor AI Code Editor", Kind: "guide",
		Notes: []Note{{"warning", "Requires a Cursor Pro account."}, {"warning", "Cursor sends requests from its own servers, so a localhost URL does not work: FastRouter must be reachable from the internet (for example through a tunnel)."}},
		Steps: []Step{{Title: "Open Settings", Desc: "Go to Settings → Models."}, {Title: "Enable OpenAI API", Desc: "Turn on the \"OpenAI API key\" option."},
			{Title: "Base URL", Desc: "Override OpenAI Base URL with your public FastRouter address:", Value: "{{baseUrl}}"}, {Title: "API Key", Type: "apiKey"},
			{Title: "Add Custom Model", Desc: "Click \"View All Models\" → \"Add Custom Model\"."}, {Title: "Select Model", Type: "model"}}})

	// ---------------------------------------------------------------- Cline
	reg(&Tool{ID: "cline", Name: "Cline", Logo: "cline.png", Description: "Cline AI Coding Assistant", Kind: "config", NeedsModel: true,
		bins: []string{"cline"}, Notes: []Note{{"info", "Sets Cline's plan and act modes to the OpenAI Compatible provider in ~/.cline/data/globalState.json; the key goes to secrets.json."}},
		status: func(e *Env) Status {
			p := e.p(".cline", "data", "globalState.json")
			o := peekObj(p)
			s := st(p, "", false)
			if o.Str("actModeApiProvider") == "openai" || o.Str("planModeApiProvider") == "openai" {
				s.BaseURL, s.Model = o.Str("openAiBaseUrl"), o.Str("openAiModelId")
				s.mark(e, s.BaseURL)
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".cline", "data", "globalState.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			for k, v := range map[string]string{"actModeApiProvider": "openai", "planModeApiProvider": "openai", "openAiBaseUrl": in.v1(), "openAiModelId": in.Model, "planModeOpenAiModelId": in.Model, "actModeOpenAiModelId": in.Model} {
				o.Set(k, v)
			}
			if err := e.writeObj(p, o); err != nil {
				return err
			}
			sp := e.p(".cline", "data", "secrets.json")
			sec, err := readObj(sp)
			if err != nil {
				return err
			}
			sec.Set("openAiApiKey", keyOr(in.APIKey))
			return e.writeObj(sp, sec)
		},
		reset: func(e *Env) error {
			p := e.p(".cline", "data", "globalState.json")
			o := peekObj(p)
			if o == nil {
				return nil
			}
			if e.ours(o.Str("openAiBaseUrl")) {
				for _, k := range []string{"openAiBaseUrl", "openAiModelId", "planModeOpenAiModelId", "actModeOpenAiModelId"} {
					o.Del(k)
				}
				o.Set("actModeApiProvider", "cline")
				o.Set("planModeApiProvider", "cline")
				if err := e.writeObj(p, o); err != nil {
					return err
				}
				sp := e.p(".cline", "data", "secrets.json")
				if sec := peekObj(sp); sec != nil {
					sec.Del("openAiApiKey")
					return e.writeObj(sp, sec)
				}
			}
			return nil
		},
	})

	// ---------------------------------------------------------------- Kilo Code
	reg(&Tool{ID: "kilo", Name: "Kilo Code", Logo: "kilocode.png", Description: "Kilo Code AI Assistant", Kind: "config", NeedsModel: true,
		bins: []string{"kilo", "kilocode"}, Notes: []Note{{"info", "Writes an openai-compatible entry to Kilo CLI's ~/.local/share/kilo/auth.json. The VS Code extension keeps its own settings (pick OpenAI Compatible there and use the same values)."}},
		status: func(e *Env) Status {
			p := e.p(".local", "share", "kilo", "auth.json")
			ent := peekObj(p).path("openai-compatible")
			s := st(p, ent.Str("baseUrl"), false)
			s.Model = ent.Str("model")
			s.mark(e, s.BaseURL)
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".local", "share", "kilo", "auth.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			o.Set("openai-compatible", obj("type", "api-key", "apiKey", keyOr(in.APIKey), "baseUrl", in.v1(), "model", in.Model))
			return e.writeObj(p, o)
		},
		reset: jsonReset(func(e *Env) string { return e.p(".local", "share", "kilo", "auth.json") }, func(o *Obj) {
			if e := o.path("openai-compatible"); e != nil {
				o.Del("openai-compatible")
			}
		}),
	})

	// ---------------------------------------------------------------- Roo / Continue / Amp / Qwen (guides)
	reg(&Tool{ID: "roo", Name: "Roo", Logo: "roo.png", Description: "Roo AI Assistant", Kind: "guide",
		Steps: []Step{{Title: "Open Settings", Desc: "Open the Roo settings panel."}, {Title: "Select Provider", Desc: "API Provider → OpenAI Compatible."},
			{Title: "Base URL", Value: "{{baseUrl}}"}, {Title: "API Key", Type: "apiKey"}, {Title: "Select Model", Type: "model"}}})
	reg(&Tool{ID: "continue", Name: "Continue", Logo: "continue.png", Description: "Continue AI Assistant", Kind: "guide",
		Steps: []Step{{Title: "Open Config", Desc: "Open Continue's configuration file."}, {Title: "API Key", Type: "apiKey"}, {Title: "Select Model", Type: "model"},
			{Title: "Add Model Config", Desc: "Add this entry to your models array:"}},
		Code: "{\n  \"apiBase\": \"{{baseUrl}}\",\n  \"title\": \"{{model}}\",\n  \"model\": \"{{model}}\",\n  \"provider\": \"openai\",\n  \"apiKey\": \"{{apiKey}}\"\n}"})
	reg(&Tool{ID: "amp", Name: "Amp CLI", Logo: "amp.png", Description: "Sourcegraph Amp coding assistant CLI", Kind: "guide", bins: []string{"amp"},
		Notes: []Note{{"info", "Point Amp at FastRouter's OpenAI-compatible endpoint; FastRouter model ids (prefix/model) or combos work as model names."}},
		Steps: []Step{{Title: "Install Amp", Desc: "Install the Amp CLI with your package manager."}, {Title: "API Key", Type: "apiKey"}, {Title: "Base URL", Value: "{{baseUrl}}"},
			{Title: "Select Model", Type: "model"}, {Title: "Run", Desc: "Export the variables and start Amp:"}},
		Code: "export OPENAI_API_KEY=\"{{apiKey}}\"\nexport OPENAI_BASE_URL=\"{{baseUrl}}\"\namp --model \"{{model}}\""})
	reg(&Tool{ID: "qwen", Name: "Qwen Code", Logo: "qwen.png", Description: "Alibaba Qwen Code CLI (OpenAI-compatible provider)", Kind: "guide", bins: []string{"qwen"},
		Docs:  "https://qwenlm.github.io/qwen-code-docs/en/users/configuration/model-providers/",
		Notes: []Note{{"info", "Any FastRouter model works, not just Qwen models."}, {"warning", "Config path: Linux/macOS ~/.qwen/settings.json • Windows %USERPROFILE%\\.qwen\\settings.json"}},
		Steps: []Step{{Title: "Install Qwen Code", Value: "npm install -g @qwen-code/qwen-code"}, {Title: "API Key", Type: "apiKey"}, {Title: "Base URL", Value: "{{baseUrl}}"},
			{Title: "Select Model", Type: "model"}, {Title: "Save Config", Desc: "Merge this into ~/.qwen/settings.json:"}},
		Code: "{\n  \"security\": {\n    \"auth\": {\n      \"selectedType\": \"openai\",\n      \"apiKey\": \"{{apiKey}}\",\n      \"baseUrl\": \"{{baseUrl}}\"\n    }\n  },\n  \"model\": {\n    \"name\": \"{{model}}\"\n  }\n}"})

	// ---------------------------------------------------------------- DeepSeek TUI
	reg(&Tool{ID: "deepseek-tui", Name: "DeepSeek TUI", Logo: "deepseek-tui.png", Description: "DeepSeek Terminal Coding Agent (Rust TUI)", Kind: "config", NeedsModel: true,
		bins: []string{"deepseek"}, Docs: "https://github.com/DeepSeek-TUI/DeepSeek-TUI",
		Notes: []Note{{"info", "Sets provider = \"openai\" and the [providers.openai] table in ~/.deepseek/config.toml."}},
		status: func(e *Env) Status {
			p := e.p(".deepseek", "config.toml")
			t := readText(p)
			s := st(p, "", false)
			if tomlGet(t, "", "provider") == "openai" {
				s.BaseURL, s.Model = tomlGet(t, "providers.openai", "base_url"), tomlGet(t, "providers.openai", "model")
				s.mark(e, s.BaseURL)
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".deepseek", "config.toml")
			t := tomlSet(readText(p), "", "provider", `"openai"`)
			t = tomlReplaceSection(t, "providers.openai", []string{"base_url = " + tomlStr(in.v1()), "api_key = " + tomlStr(keyOr(in.APIKey)), "model = " + tomlStr(in.Model)})
			return e.write(p, []byte(t))
		},
		reset: func(e *Env) error {
			p := e.p(".deepseek", "config.toml")
			t := readText(p)
			if !e.ours(tomlGet(t, "providers.openai", "base_url")) {
				return nil
			}
			t = tomlSet(tomlRemoveSection(t, "providers.openai"), "", "provider", `"deepseek"`)
			return e.write(p, []byte(t))
		},
	})

	// ---------------------------------------------------------------- jcode
	reg(&Tool{ID: "jcode", Name: "jcode", Logo: "jcode.png", Description: "High-performance Rust-based coding agent harness", Kind: "config", NeedsModel: true,
		bins: []string{"jcode"}, Docs: "https://github.com/1jehuang/jcode",
		Notes: []Note{{"info", "Adds [providers.fastrouter] to ~/.jcode/config.toml; the key goes to ~/.config/jcode/provider-fastrouter.env."}},
		status: func(e *Env) Status {
			p := e.p(".jcode", "config.toml")
			t := readText(p)
			s := st(p, tomlGet(t, "providers."+provID, "base_url"), exists(e.p(".jcode")))
			s.Model = tomlGet(t, "providers."+provID, "default_model")
			s.mark(e, s.BaseURL)
			if !s.Connected && tomlHasSection(t, "providers.9router") {
				s.Other = "9Router"
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".jcode", "config.toml")
			t := tomlReplaceSection(readText(p), "providers."+provID, []string{`type = "openai-compatible"`, "base_url = " + tomlStr(in.v1()), `auth = "bearer"`,
				`api_key_env = "JCODE_FASTROUTER_API_KEY"`, `env_file = "provider-fastrouter.env"`, "default_model = " + tomlStr(in.Model), "requires_api_key = true"})
			if err := e.write(p, []byte(t)); err != nil {
				return err
			}
			envP := e.xdg("jcode", "provider-fastrouter.env")
			return e.write(envP, []byte(upsertEnv(readText(envP), "JCODE_FASTROUTER_API_KEY", tomlStr(keyOr(in.APIKey)))))
		},
		reset: func(e *Env) error {
			p := e.p(".jcode", "config.toml")
			if t := readText(p); tomlHasSection(t, "providers."+provID) {
				if err := e.write(p, []byte(tomlRemoveSection(t, "providers."+provID))); err != nil {
					return err
				}
			}
			envP := e.xdg("jcode", "provider-fastrouter.env")
			if exists(envP) {
				return e.write(envP, []byte(removeEnv(readText(envP), "JCODE_FASTROUTER_API_KEY")))
			}
			return nil
		},
	})

	// ---------------------------------------------------------------- Grok Build
	reg(&Tool{ID: "grok-build", Name: "Grok Build", Logo: "grok-cli.png", Description: "xAI Grok Build TUI coding agent", Kind: "config", NeedsModel: true,
		bins: []string{"grok"}, Docs: "https://x.ai/cli",
		Notes: []Note{{"info", "Writes a [model.fastrouter] custom model to ~/.grok/config.toml and makes it the default (the previous default is remembered for Reset)."},
			{"info", "After Apply run grok (or /model fastrouter). Switch back anytime with /model grok-build."}},
		status: func(e *Env) Status {
			p := e.p(".grok", "config.toml")
			t := readText(p)
			s := st(p, tomlGet(t, "model."+provID, "base_url"), exists(e.p(".grok", "bin")))
			s.Model = tomlGet(t, "model."+provID, "model")
			s.mark(e, s.BaseURL)
			if !s.Connected && tomlHasSection(t, "model.9router") {
				s.Other = "9Router"
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".grok", "config.toml")
			t := readText(p)
			if cur := tomlGet(t, "models", "default"); cur != "" && cur != provID && !strings.Contains(t, "# fastrouter-prev-default") {
				t = "# fastrouter-prev-default = " + tomlStr(cur) + "\n" + t
			}
			t = tomlReplaceSection(t, "model."+provID, []string{"model = " + tomlStr(in.Model), "base_url = " + tomlStr(in.v1()), `name = "FastRouter"`,
				`description = "Routed via FastRouter"`, `api_backend = "chat_completions"`, "api_key = " + tomlStr(keyOr(in.APIKey))})
			t = tomlSet(t, "models", "default", tomlStr(provID))
			return e.write(p, []byte(t))
		},
		reset: func(e *Env) error {
			p := e.p(".grok", "config.toml")
			t := readText(p)
			if t == "" {
				return nil
			}
			prev := "grok-build"
			if m := regexp.MustCompile(`(?m)^# fastrouter-prev-default = "([^"]*)"\n?`).FindStringSubmatch(t); m != nil {
				prev = m[1]
				t = strings.Replace(t, m[0], "", 1)
			}
			if tomlGet(t, "models", "default") == provID {
				t = tomlSet(t, "models", "default", tomlStr(prev))
			}
			return e.write(p, []byte(tomlRemoveSection(t, "model."+provID)))
		},
	})

	// ---------------------------------------------------------------- Devin CLI / OpenDesign (guides)
	reg(&Tool{ID: "devin", Name: "Devin CLI", Logo: "devin-cli.png", Description: "Cognition Devin CLI", Kind: "guide", bins: []string{"devin"}, Install: "https://cli.devin.ai",
		Notes: []Note{{"info", "Devin CLI uses its own Cognition account; it is not routed through FastRouter. Listed for parity with 9Router."}},
		Steps: []Step{{Title: "Install Devin CLI", Desc: "Use the official installer at cli.devin.ai."}, {Title: "Authenticate", Value: "devin auth login"}, {Title: "Verify", Value: "devin --version"}}})
	reg(&Tool{ID: "opendesign", Name: "OpenDesign", Logo: "opendesign.png", Description: "Agent-native design skills pack (claude.ai/design open-sourced)", Kind: "guide",
		Docs:  "https://github.com/manalkaff/opendesign",
		Notes: []Note{{"info", "OpenDesign runs inside your host agent and uses its model config: once the host (Claude Code, Codex, OpenCode…) points at FastRouter, /opendesign does too."}},
		Steps: []Step{{Title: "Install the plugin", Desc: "Run the install command for your host (below)."}, {Title: "No extra config", Desc: "It inherits the host agent's model settings."},
			{Title: "Start designing", Value: "/opendesign make a pitch deck for a seed-stage AI company, 10 slides"}},
		Code: "# Claude Code\n/plugin marketplace add manalkaff/opendesign\n/plugin install opendesign@opendesign\n\n# Cursor\n/add-plugin opendesign\n\n# OpenAI Codex CLI\n/plugins   # search \"opendesign\" -> Install Plugin\n\n# Gemini CLI\ngemini extensions install https://github.com/manalkaff/opendesign"})

	// ---------------------------------------------------------------- Pi / Oh My Pi / Crush / Forge / Smelt / CodeWhale
	reg(&Tool{ID: "pi", Name: "Pi", Logo: "pi.svg", Description: "Pi coding agent — minimal, extensible agent harness (pi.dev)", Kind: "config", NeedsModel: true,
		bins: []string{"pi"}, Docs: "https://pi.dev", Notes: []Note{{"info", "Adds providers.fastrouter (openai-completions) to ~/.pi/agent/models.json."}},
		status: func(e *Env) Status {
			p := piPath(e)
			prov := peekObj(p).path("providers", provID)
			s := st(p, prov.Str("baseUrl"), false)
			if ms := objArr(prov.Get("models")); len(ms) > 0 {
				m, _ := ms[0].(*Obj)
				s.Model = m.Str("id")
			}
			s.mark(e, s.BaseURL)
			return s
		},
		apply: func(e *Env, in Input) error {
			p := piPath(e)
			o, err := readObj(p)
			if err != nil {
				return err
			}
			o.Child("providers", true).Set(provID, obj("baseUrl", in.v1(), "apiKey", keyOr(in.APIKey), "api", "openai-completions",
				"models", []any{obj("id", in.Model, "name", in.Model, "contextWindow", 200000, "maxTokens", 64000)}))
			return e.writeObj(p, o)
		},
		reset: jsonReset(piPath, func(o *Obj) {
			if ps := o.Child("providers", false); ps != nil {
				ps.Del(provID)
				if ps.Len() == 0 {
					o.Del("providers")
				}
			}
		}),
	})
	reg(&Tool{ID: "omp", Name: "Oh My Pi", Logo: "omp.png", Description: "Oh My Pi terminal AI agent with auto-discovery support", Kind: "config",
		bins: []string{"omp"}, Docs: "https://github.com/can1357/oh-my-pi",
		Notes: []Note{{"info", "Adds a fastrouter provider with proxy discovery to ~/.omp/agent/models.yml, so every FastRouter model shows up under /model. No default model needed."}},
		status: func(e *Env) Status {
			p := e.p(".omp", "agent", "models.yml")
			y := readText(p)
			s := st(p, "", exists(e.p(".omp", "agent", "agent.db")))
			if b := ompBlock(y, provID); b != "" {
				s.BaseURL = yget(b, "baseUrl")
				s.mark(e, s.BaseURL)
			} else if ompBlock(y, "9router") != "" {
				s.Other = "9Router"
			}
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".omp", "agent", "models.yml")
			block := "  " + provID + ":\n    baseUrl: " + in.v1() + "\n    apiKey: " + keyOr(in.APIKey) + "\n    api: openai-completions\n    authHeader: true\n    disableStrictTools: true\n    discovery:\n      type: proxy\n"
			return e.write(p, []byte(ompUpsert(readText(p), block)))
		},
		reset: func(e *Env) error {
			p := e.p(".omp", "agent", "models.yml")
			y := readText(p)
			if ompBlock(y, provID) == "" {
				return nil
			}
			return e.write(p, []byte(ompRemove(y)))
		},
	})
	reg(&Tool{ID: "crush", Name: "Crush", Logo: "crush.png", Description: "Charm Crush terminal AI coding agent", Kind: "config", NeedsModel: true,
		bins: []string{"crush"}, Docs: "https://github.com/charmbracelet/crush", Notes: []Note{{"info", "Registers fastrouter as an openai-compat provider in ~/.config/crush/crush.json."}},
		status: func(e *Env) Status {
			p := e.xdg("crush", "crush.json")
			prov := peekObj(p).path("providers", provID)
			s := st(p, prov.Str("base_url"), false)
			if ms := objArr(prov.Get("models")); len(ms) > 0 {
				m, _ := ms[0].(*Obj)
				s.Model = m.Str("id")
			}
			s.mark(e, s.BaseURL)
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.xdg("crush", "crush.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			o.Child("providers", true).Set(provID, obj("type", "openai-compat", "base_url", in.v1(), "api_key", keyOr(in.APIKey),
				"models", []any{obj("id", in.Model, "name", in.Model, "context_window", 128000)}))
			return e.writeObj(p, o)
		},
		reset: jsonReset(func(e *Env) string { return e.xdg("crush", "crush.json") }, func(o *Obj) {
			if ps := o.Child("providers", false); ps != nil {
				ps.Del(provID)
				if ps.Len() == 0 {
					o.Del("providers")
				}
			}
		}),
	})
	reg(openaiSectionTool("forge", "ForgeCode", "forge.png", "Antinomy HQ ForgeCode agent harness", "https://github.com/antinomyhq/forge", []string{"forge"}, ".forge"))
	reg(&Tool{ID: "smelt", Name: "Smelt", Logo: "smelt.svg", Description: "Smelt terminal AI coding assistant", Kind: "config", NeedsModel: true,
		bins: []string{"smelt"}, Docs: "https://github.com/leonardcser/smelt", Notes: []Note{{"info", "Sets baseUrl, apiKey and model in ~/.smelt/config.json."}},
		status: func(e *Env) Status {
			p := e.p(".smelt", "config.json")
			o := peekObj(p)
			s := st(p, o.Str("baseUrl"), false)
			s.Model = o.Str("model")
			s.mark(e, s.BaseURL)
			return s
		},
		apply: func(e *Env, in Input) error {
			p := e.p(".smelt", "config.json")
			o, err := readObj(p)
			if err != nil {
				return err
			}
			o.Set("baseUrl", in.v1())
			o.Set("apiKey", keyOr(in.APIKey))
			o.Set("model", in.Model)
			o.Set("_managedBy", provID)
			return e.writeObj(p, o)
		},
		reset: jsonReset(func(e *Env) string { return e.p(".smelt", "config.json") }, func(o *Obj) {
			if o.Str("_managedBy") == provID {
				for _, k := range []string{"baseUrl", "apiKey", "model", "_managedBy"} {
					o.Del(k)
				}
			}
		}),
	})
	reg(openaiSectionTool("codewhale", "CodeWhale", "codewhale.svg", "CodeWhale terminal coding agent (successor to DeepSeek TUI)", "https://github.com/Hmbown/CodeWhale", []string{"codewhale"}, ".codewhale"))
}

var claudeSlots = map[string]string{"opus": "ANTHROPIC_DEFAULT_OPUS_MODEL", "sonnet": "ANTHROPIC_DEFAULT_SONNET_MODEL", "haiku": "ANTHROPIC_DEFAULT_HAIKU_MODEL", "fable": "ANTHROPIC_DEFAULT_FABLE_MODEL"}

var hermesRoles = []Role{{"delegation", "Delegation (subagents)"}, {"vision", "Vision"}, {"compression", "Compression"}, {"title_generation", "Title Generation"},
	{"approval", "Approval"}, {"skills_hub", "Skills Hub"}, {"mcp", "MCP"}, {"review", "Review (/review subagent)"}, {"memory_query_rewrite", "Memory Query Rewrite"},
	{"background_review", "Background Review"}, {"curator", "Curator"}, {"monitor", "Monitor"}, {"tts_audio_tags", "TTS Audio Tags"},
	{"triage_specifier", "Triage Specifier"}, {"kanban_decomposer", "Kanban Decomposer"}, {"profile_describer", "Profile Describer"}, {"goal_judge", "Goal Judge"}}

func setOrDel(o *Obj, k, v string) {
	if v == "" {
		o.Del(k)
	} else {
		o.Set(k, v)
	}
}

func lastPart(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

// jsonReset edits a JSON config when it exists.
func jsonReset(path func(*Env) string, edit func(*Obj)) func(*Env) error {
	return func(e *Env) error {
		p := path(e)
		if !exists(p) {
			return nil
		}
		o, err := readObj(p)
		if err != nil {
			return err
		}
		edit(o)
		return e.writeObj(p, o)
	}
}

func piPath(e *Env) string {
	agent, root := e.p(".pi", "agent", "models.json"), e.p(".pi", "models.json")
	if !exists(agent) && exists(root) {
		return root
	}
	return agent
}

// openaiSectionTool covers ForgeCode and CodeWhale: an [openai] table in ~/.<dir>/config.toml.
func openaiSectionTool(id, name, logo, desc, docs string, bins []string, dir string) *Tool {
	path := func(e *Env) string { return e.p(dir, "config.toml") }
	return &Tool{ID: id, Name: name, Logo: logo, Description: desc, Kind: "config", NeedsModel: true, bins: bins, Docs: docs,
		Notes: []Note{{"info", name + " uses ~/" + dir + "/config.toml; FastRouter sets base_url, api_key and model in its [openai] table."}},
		status: func(e *Env) Status {
			t := readText(path(e))
			s := st(path(e), tomlGet(t, "openai", "base_url"), false)
			s.Model = tomlGet(t, "openai", "model")
			s.mark(e, s.BaseURL)
			return s
		},
		apply: func(e *Env, in Input) error {
			t := tomlReplaceSection(readText(path(e)), "openai", []string{"api_key = " + tomlStr(keyOr(in.APIKey)), "base_url = " + tomlStr(in.v1()), "model = " + tomlStr(in.Model)})
			return e.write(path(e), []byte(t))
		},
		reset: func(e *Env) error {
			t := readText(path(e))
			if !e.ours(tomlGet(t, "openai", "base_url")) {
				return nil
			}
			return e.write(path(e), []byte(tomlRemoveSection(t, "openai")))
		},
	}
}

// ompBlock returns a provider's block under "providers:" in models.yml.
func ompBlock(y, name string) string {
	ls := strings.Split(y, "\n")
	for i, l := range ls {
		if strings.TrimRight(l, " \t") == "  "+name+":" {
			j := i + 1
			for j < len(ls) && (strings.TrimSpace(ls[j]) == "" || strings.HasPrefix(ls[j], "    ")) {
				j++
			}
			for j > i+1 && strings.TrimSpace(ls[j-1]) == "" {
				j--
			}
			return strings.Join(ls[i:j], "\n") + "\n"
		}
	}
	return ""
}

func ompRemove(y string) string {
	if b := ompBlock(y, provID); b != "" {
		y = strings.Replace(y, b, "", 1)
	}
	if strings.TrimSpace(y) == "providers:" {
		return ""
	}
	return y
}

func ompUpsert(y, block string) string {
	y = ompRemove(y)
	switch {
	case strings.TrimSpace(y) == "":
		return "providers:\n" + block
	case regexp.MustCompile(`(?m)^providers:[ \t]*$`).MatchString(y):
		return replaceFirst(regexp.MustCompile(`(?m)^providers:[ \t]*\n`), ensureNL(y), "providers:\n"+block)
	}
	return ensureNL(y) + "\nproviders:\n" + block
}

// coworkStatus reads Claude Desktop's third-party gateway config (read-only).
func coworkStatus(e *Env) Status {
	var roots []string
	switch e.GOOS {
	case "darwin":
		base := e.p("Library", "Application Support")
		roots = []string{filepath.Join(base, "Claude-3p"), filepath.Join(base, "Claude")}
	case "windows":
		roots = []string{filepath.Join(e.LocalAppData, "Claude-3p"), filepath.Join(e.AppData, "Claude-3p"), filepath.Join(e.LocalAppData, "Claude"), filepath.Join(e.AppData, "Claude")}
	default:
		roots = []string{e.p(".config", "Claude-3p"), e.p(".config", "Claude")}
	}
	s := Status{}
	for _, r := range roots {
		if exists(r) {
			s.Installed = true
		}
		dir := filepath.Join(r, "configLibrary")
		meta := peekObj(filepath.Join(dir, "_meta.json"))
		if id := meta.Str("appliedId"); id != "" && !strings.ContainsAny(id, `/\.`) {
			p := filepath.Join(dir, id+".json")
			c := peekObj(p)
			if c.Str("inferenceProvider") == "gateway" {
				s.ConfigPath, s.BaseURL = p, c.Str("inferenceGatewayBaseUrl")
				s.mark(e, s.BaseURL)
				return s
			}
		}
	}
	return s
}
