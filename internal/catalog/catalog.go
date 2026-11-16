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
	// Extra marks providers generated from 9Router's registry (registry_gen.go).
	// They never shadow a custom endpoint's prefix.
	Extra bool
}

// Display is how the dashboard shows a provider.
type Display struct {
	Name, Color, Category, KeyURL string
}

// Model is one model id a provider lists.
type Model struct {
	ID, Name string
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

var all = append(append([]Provider{}, builtins...), extras...)

var byKey, reserved = func() (map[string]*Provider, map[string]bool) {
	m, core := map[string]*Provider{}, map[string]bool{}
	for i := range all {
		p := &all[i]
		for _, k := range append([]string{p.ID}, p.Aliases...) {
			if _, taken := m[k]; !taken {
				m[k] = p
			}
			if !p.Extra {
				core[k] = true
			}
		}
	}
	return m, core
}()

// All returns the built-in providers: the core list, then the generated extras.
func All() []Provider { return all }

// Reserved reports whether k is an id or alias of a core provider. Custom
// endpoints may not use these prefixes; extras give way to custom endpoints.
func Reserved(k string) bool { return reserved[k] }

// Info returns display details for any 9Router provider id.
func Info(id string) (Display, bool) {
	d, ok := display[id]
	return d, ok
}

// DisplayAll returns display details for every known provider id.
func DisplayAll() map[string]Display { return display }

// Models returns the model ids 9Router lists for a provider.
func Models(id string) []Model { return models[id] }

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
