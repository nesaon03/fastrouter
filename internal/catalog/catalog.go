// Package catalog lists the built-in providers FastRouter knows about.
// URLs and aliases follow 9Router's provider registry (MIT, decolua/9router).
package catalog

// Provider describes a built-in upstream.
type Provider struct {
	ID      string
	Aliases []string // model prefixes, e.g. "ds" for deepseek
	ChatURL string   // OpenAI-format chat completions endpoint
	// MessagesURL is an Anthropic-format endpoint, if the provider has one.
	MessagesURL string
	// Regions maps a region id to a base URL (chat URL = base + /chat/completions).
	Regions map[string]string
	// Plugin is true for logins that core does not support (deferred to a plugin).
	Plugin bool
}

var builtins = []Provider{
	{ID: "deepseek", Aliases: []string{"ds"}, ChatURL: "https://api.deepseek.com/chat/completions",
		MessagesURL: "https://api.deepseek.com/anthropic/v1/messages"},
	{ID: "openrouter", ChatURL: "https://openrouter.ai/api/v1/chat/completions"},
	{ID: "opencode-go", Aliases: []string{"ocg"}, ChatURL: "https://opencode.ai/inference/go/openai/v1/chat/completions"},
	{ID: "xiaomi-tokenplan", Aliases: []string{"xmtp"}, ChatURL: "https://token-plan-sgp.xiaomimimo.com/v1/chat/completions",
		Regions: map[string]string{
			"sgp": "https://token-plan-sgp.xiaomimimo.com/v1",
			"cn":  "https://token-plan-cn.xiaomimimo.com/v1",
			"ams": "https://token-plan-ams.xiaomimimo.com/v1",
		}},
	{ID: "openai", ChatURL: "https://api.openai.com/v1/chat/completions"},
	// Gemini's OpenAI-compatible endpoint, so it can use the passthrough path.
	{ID: "gemini", ChatURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
	{ID: "nvidia", ChatURL: "https://integrate.api.nvidia.com/v1/chat/completions"},
	{ID: "xai", ChatURL: "https://api.x.ai/v1/chat/completions"},
	{ID: "anthropic", Aliases: []string{"claude"}, MessagesURL: "https://api.anthropic.com/v1/messages"},

	// Subscription / browser logins: imported, but need the deferred plugin.
	{ID: "antigravity", Aliases: []string{"ag"}, Plugin: true},
	{ID: "codex", Aliases: []string{"cx"}, Plugin: true},
	{ID: "cursor", Aliases: []string{"cu"}, Plugin: true},
	{ID: "glm", Plugin: true},
	{ID: "opencode", Aliases: []string{"oc"}, Plugin: true},
}

var byKey = func() map[string]*Provider {
	m := map[string]*Provider{}
	for i := range builtins {
		p := &builtins[i]
		m[p.ID] = p
		for _, a := range p.Aliases {
			m[a] = p
		}
	}
	return m
}()

// Lookup finds a built-in provider by id or alias.
func Lookup(idOrAlias string) (*Provider, bool) {
	p, ok := byKey[idOrAlias]
	return p, ok
}

// PrimaryAlias is the short prefix shown in model names.
func (p *Provider) PrimaryAlias() string {
	if len(p.Aliases) > 0 {
		return p.Aliases[0]
	}
	return p.ID
}
