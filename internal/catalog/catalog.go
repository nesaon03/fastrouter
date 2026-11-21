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
	// Login names the sign-in that handles this provider: "codex" or "glm"
	// (internal/login) or "oauth" (internal/oauth). Its accounts use tokens.
	Login string
	// Extra marks providers generated from 9Router's registry (registry_gen.go).
	// They never shadow a custom endpoint's prefix.
	Extra bool

	// DecisionURL is a typed-judgment endpoint (TypeSafe Jev, OpenAI Decisions)
	// that answers choice/score/predicate questions instead of chat turns. Such
	// providers are not listed in /v1/models; POST /v1/decisions reaches them.
	DecisionURL string
	// DecisionModel is the id sent when the client does not name one.
	DecisionModel string
}

// Display is how the dashboard shows a provider.
type Display struct {
	Name, Color, Category, KeyURL string
	TextIcon                      string // fallback initials when there is no logo
	Priority                      int    // 9Router's sort order (999 = unset)
	LLM                           bool   // listed on 9Router's Providers page (chat models)
	NoAuth                        bool   // works without any account (e.g. OpenCode Free)
	APIKeyMode                    bool   // 9Router also accepts a plain API key for this provider
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
	{ID: "openai", ChatURL: "https://api.openai.com/v1/chat/completions", ResponsesURL: "https://api.openai.com/v1/responses", Official: true,
		DecisionURL: "https://api.openai.com/v1/decisions", DecisionModel: "gpt-6-luna"},
	{ID: "gemini", ChatURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		GeminiURL: "https://generativelanguage.googleapis.com/v1beta/models", PreferNative: true, Official: true},
	{ID: "nvidia", ChatURL: "https://integrate.api.nvidia.com/v1/chat/completions"},
	{ID: "xai", ChatURL: "https://api.x.ai/v1/chat/completions"},
	{ID: "ollama", ChatURL: "https://ollama.com/v1/chat/completions"}, // Ollama Cloud's OpenAI-compatible API
	{ID: "anthropic", MessagesURL: "https://api.anthropic.com/v1/messages"},

	// Decision engines: typed answers with probabilities, not prose. They are
	// reached through POST /v1/decisions, never /v1/chat/completions.
	{ID: "typesafe", Aliases: []string{"jev"}, DecisionURL: "https://api.typesafe.ai/v1/systemone", DecisionModel: "jev-latest"},

	// Official logins handled by internal/login (ChatGPT sign-in, Z.ai sign-in).
	{ID: "codex", Aliases: []string{"cx"}, ResponsesURL: "https://chatgpt.com/backend-api/codex/responses", Login: "codex"},
	{ID: "glm", ChatURL: "https://api.z.ai/api/coding/paas/v4/chat/completions",
		MessagesURL: "https://api.z.ai/api/anthropic/v1/messages?beta=true", Login: "glm"},

	// Subscription sign-ins handled by internal/oauth (9Router's OAuth providers).
	// Adapters there pick the exact URL per model/account and add client headers.
	{ID: "antigravity", Aliases: []string{"ag"}, GeminiURL: "https://daily-cloudcode-pa.googleapis.com/v1internal", PreferNative: true, Login: "oauth",
		Upstream: map[string]string{"gemini-4-carbon-high": "gemini-4-carbon-high(high)", "gemini-4-carbon-medium": "gemini-4-carbon-medium(medium)", "gemini-4-carbon-low": "gemini-4-carbon-low(low)", "gemini-4-carbon": "gemini-4-carbon-medium(medium)", "gemini-4-argon-high": "gemini-4-argon-high(high)", "gemini-4-argon-medium": "gemini-4-argon-medium(medium)", "gemini-4-argon-low": "gemini-4-argon-low(low)", "gemini-3.8-flash-high": "gemini-3.8-flash-high(high)", "gemini-3.8-flash-medium": "gemini-3.8-flash-medium(medium)", "gemini-3.8-flash-low": "gemini-3.8-flash-low(low)", "gemini-3.8-flash": "gemini-3.8-flash-medium(medium)", "gemini-3.7-flash-high": "gemini-3.7-flash-tiered(high)", "gemini-3.7-flash-medium": "gemini-3.7-flash-tiered(medium)", "gemini-3.7-flash-low": "gemini-3.7-flash-tiered(low)", "gemini-3.6-flash-high": "gemini-3.6-flash-tiered(high)", "gemini-3.6-flash-medium": "gemini-3.6-flash-tiered(medium)", "gemini-3.6-flash-low": "gemini-3.6-flash-tiered(low)", "gemini-3.1-pro-high": "gemini-pro-agent", "gemini-3.1-pro-low": "gemini-3.1-pro-low(low)", "claude-sonnet-5-5-high": "claude-sonnet-5-5-high(high)", "claude-sonnet-5-5-medium": "claude-sonnet-5-5-medium(medium)", "claude-sonnet-5-5-low": "claude-sonnet-5-5-low(low)", "claude-sonnet-5-5": "claude-sonnet-5-5-high(high)", "claude-opus-5-5-high": "claude-opus-5-5-high(high)", "claude-opus-5-5-medium": "claude-opus-5-5-medium(medium)", "claude-opus-5-5-low": "claude-opus-5-5-low(low)", "claude-opus-5-5": "claude-opus-5-5-high(high)"}},
	{ID: "gemini-cli", Aliases: []string{"gc"}, GeminiURL: "https://cloudcode-pa.googleapis.com/v1internal", PreferNative: true, Login: "oauth"},
	{ID: "vertex", GeminiURL: "https://aiplatform.googleapis.com/v1", PreferNative: true, Login: "oauth"},
	{ID: "claude", Aliases: []string{"cc"}, MessagesURL: "https://api.anthropic.com/v1/messages?beta=true", Login: "oauth"},
	{ID: "github", Aliases: []string{"gh"}, ChatURL: "https://api.githubcopilot.com/chat/completions", MessagesURL: "https://api.githubcopilot.com/v1/messages",
		ResponsesURL: "https://api.githubcopilot.com/responses", Login: "oauth"},
	{ID: "kimi", MessagesURL: "https://api.kimi.com/coding/v1/messages?beta=true", Login: "oauth"},
	{ID: "kilocode", Aliases: []string{"kc"}, ChatURL: "https://api.kilo.ai/api/openrouter/chat/completions", Login: "oauth"},
	{ID: "cline", Aliases: []string{"cl"}, ChatURL: "https://api.cline.bot/api/v1/chat/completions", Login: "oauth"},
	{ID: "clinepass", ChatURL: "https://api.cline.bot/api/v1/chat/completions", Login: "oauth"},
	{ID: "codebuddy-cn", Aliases: []string{"cbcn"}, ChatURL: "https://copilot.tencent.com/v2/chat/completions", Login: "oauth"},
	{ID: "codebuddy-intl", Aliases: []string{"cbai"}, ChatURL: "https://www.codebuddy.ai/v2/chat/completions", Login: "oauth"},
	{ID: "minimax-code", Aliases: []string{"mm"}, MessagesURL: "https://agent.minimax.cn/mavis/api/v1/llm/v1/messages", Login: "oauth"},
	{ID: "minimax-code-global", Aliases: []string{"mmg"}, MessagesURL: "https://agent.minimax.io/mavis/api/v1/llm/v1/messages", Login: "oauth"},
	{ID: "iflow", Aliases: []string{"if"}, ChatURL: "https://apis.iflow.cn/v1/chat/completions", Login: "oauth"},
	{ID: "grok-cli", Aliases: []string{"gcli"}, ResponsesURL: "https://cli-chat-proxy.grok.com/v1/responses", Login: "oauth"},

	// Cursor IDE: Connect-RPC over HTTP/2 (internal/oauth/cursor.go); token imported from the Cursor app.
	{ID: "cursor", Aliases: []string{"cu"}, ChatURL: "https://agent.api5.cursor.sh/agent.v1.AgentService/Run", Login: "oauth"},

	// Logins that need provider workarounds (custom binary protocols): imported, but left for a plugin.
	{ID: "opencode", Aliases: []string{"oc"}, Plugin: true},
}

var all = append(append([]Provider{}, builtins...), extras...)

// modelAdditions are ids a provider serves but the 9Router registry has not
// caught up with yet. They live here, not in registry_gen.go, because that file
// is generated and regenerating it would drop them.
//
// Only ids that were verified against the live API belong here: an id that the
// provider rejects is worse than a missing one, because it looks available and
// then fails on the first request. Codex has no published model list (its
// /backend-api/models answers with ChatGPT product slugs, which its responses
// endpoint rejects), so this is the only way a new Codex model gets listed.
var modelAdditions = map[string][]Model{
	"codex": {
		{ID: "gpt-6.2-astra", Name: "GPT 6.2 Astra"},
		{ID: "gpt-6.2-astra[1m]", Name: "GPT 6.2 Astra (extended context)"},
		{ID: "gpt-6.2-sol", Name: "GPT 6.2 Sol"},
		{ID: "gpt-6.2-sol[1m]", Name: "GPT 6.2 Sol (extended context)"},
	},
}

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

// Models returns the model ids for a provider: the registry's list plus any
// hand-added ids the registry has not caught up with yet.
func Models(id string) []Model {
	base := models[id]
	add := modelAdditions[id]
	if len(add) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(add))
	out := make([]Model, 0, len(base)+len(add))
	for _, m := range append(append([]Model{}, base...), add...) {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

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

func init() {
	// iFlow is hidden on 9Router's page but still signs in; give it a card.
	if _, ok := display["iflow"]; !ok {
		display["iflow"] = Display{Name: "iFlow", Color: "#5B5BD6", Category: "oauth", TextIcon: "IF", Priority: 150, LLM: true}
	}
	// Decision engines get their own Providers section. They answer typed
	// questions through /v1/decisions and are never advertised as chat models.
	if _, ok := display["typesafe"]; !ok {
		display["typesafe"] = Display{Name: "TypeSafe Jev", Color: "#14B8A6", Category: "decision",
			KeyURL: "https://typesafe.ai", TextIcon: "JV", Priority: 40}
	}
}
