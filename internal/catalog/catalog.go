// Package catalog lists the built-in providers FastRouter knows about.
// URLs and aliases follow 9Router's provider registry (MIT, decolua/9router).
package catalog

// Provider describes a built-in upstream and the wire formats it accepts.
type Provider struct {
	ID      string
	Aliases []string // model prefixes, e.g. "ds" for deepseek

	ChatURL      string // OpenAI Chat Completions endpoint
	MessagesURL  string // Anthropic Messages endpoint
	ResponsesURL string // OpenAI Responses endpoint
	GeminiURL    string // generateContent base (…/v1beta/models)

	// PreferNative makes translated requests use the provider's native format
	// (Gemini) instead of its OpenAI-compatible endpoint.
	PreferNative bool
	// Official marks api.openai.com-style strictness: max_completion_tokens,
	// no vendor reasoning fields.
	Official bool
	// CacheControl forwards Anthropic cache_control inside OpenAI-format bodies
	// (gateways that front Claude, e.g. OpenRouter).
	CacheControl bool

	// Upstream maps a catalog model id to the id the API actually serves
	// (9Router's upstreamModelId), e.g. DeepSeek rejects "deepseek-v4.1-flash".
	Upstream map[string]string

	// Regions maps a region id to a base URL (chat URL = base + /chat/completions).
	Regions map[string]string
	// Plugin is true for logins that core does not support (deferred to a plugin).
	Plugin bool
	// Login names the official-login plugin that handles this provider
	// ("codex" or "glm"); its accounts use OAuth tokens instead of API keys.
	Login string
}

var builtins = []Provider{
	{ID: "deepseek", Aliases: []string{"ds"}, ChatURL: "https://api.deepseek.com/chat/completions",
		MessagesURL: "https://api.deepseek.com/anthropic/v1/messages",
		Upstream:    map[string]string{"deepseek-v4.1-flash": "deepseek-flash", "deepseek-v4-pro-max": "deepseek-v4-pro", "deepseek-v4-pro-none": "deepseek-v4-pro"}},
	{ID: "openrouter", ChatURL: "https://openrouter.ai/api/v1/chat/completions", CacheControl: true},
	{ID: "opencode-go", Aliases: []string{"ocg"}, ChatURL: "https://opencode.ai/inference/go/openai/v1/chat/completions"},
	{ID: "xiaomi-tokenplan", Aliases: []string{"xmtp"}, ChatURL: "https://token-plan-sgp.xiaomimimo.com/v1/chat/completions",
		Regions: map[string]string{
			"sgp": "https://token-plan-sgp.xiaomimimo.com/v1",
			"cn":  "https://token-plan-cn.xiaomimimo.com/v1",
			"ams": "https://token-plan-ams.xiaomimimo.com/v1",
		}},
	{ID: "openai", ChatURL: "https://api.openai.com/v1/chat/completions", ResponsesURL: "https://api.openai.com/v1/responses", Official: true},
	{ID: "gemini", ChatURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		GeminiURL: "https://generativelanguage.googleapis.com/v1beta/models", PreferNative: true, Official: true},
	{ID: "nvidia", ChatURL: "https://integrate.api.nvidia.com/v1/chat/completions"},
	{ID: "xai", ChatURL: "https://api.x.ai/v1/chat/completions"},
	{ID: "anthropic", Aliases: []string{"claude"}, MessagesURL: "https://api.anthropic.com/v1/messages"},

	// Official logins handled by internal/login (ChatGPT sign-in, Z.ai sign-in).
	{ID: "codex", Aliases: []string{"cx"}, ResponsesURL: "https://chatgpt.com/backend-api/codex/responses", Login: "codex"},
	{ID: "glm", ChatURL: "https://api.z.ai/api/coding/paas/v4/chat/completions",
		MessagesURL: "https://api.z.ai/api/anthropic/v1/messages?beta=true", Login: "glm"},

	// Logins that need provider workarounds: imported, but left for a plugin.
	{ID: "antigravity", Aliases: []string{"ag"}, Plugin: true},
	{ID: "cursor", Aliases: []string{"cu"}, Plugin: true},
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
