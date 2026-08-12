package llm

// The vendor catalog, in the shape ORCHESTRATOR.md §2b borrowed from OpenClaw.
//
// Four things that shape carries, and all four are load-bearing:
//
//  1. Two levels, not one flat list. Vendors are groups, each with a one-line
//     hint naming the auth methods behind it, and you drill in only after
//     picking one. Thirty vendors times three auth flows is ninety unreadable
//     rows as one list; as two steps it is a short menu and then a short menu.
//  2. Subscription auth is a first-class row where it exists, not a special
//     case to apologise for.
//  3. Risk is a hint on the row, not a wall. The option exists, the warning is
//     attached to it, and the user decides. Inform, do not paternalise, and do
//     not quietly omit.
//  4. Custom Provider is always the last row, so the list is never a cage.
//
// This file is data. The installer (internal/install) renders it; it does not
// invent it.

// AuthKind is how a credential is obtained.
type AuthKind string

const (
	AuthAPIKey AuthKind = "api_key"
	// AuthSubscription is a plan the user already pays for, reached through the
	// vendor's own OAuth. ORCHESTRATOR.md §2b: usable where sanctioned.
	AuthSubscription AuthKind = "subscription"
	AuthOAuth        AuthKind = "oauth"
	AuthDeviceCode   AuthKind = "device_code"
)

// Auth is one authentication method on a vendor.
type Auth struct {
	ID    string
	Label string
	Kind  AuthKind
	// Risk is the hint shown on the row when there is one. Empty for most.
	Risk string
}

// VendorEntry is one group in the first-level menu.
type VendorEntry struct {
	ID    string
	Label string
	// Hint is the one line naming the auth methods behind the group.
	Hint    string
	API     API
	BaseURL string
	Auths   []Auth
	// Recommended marks the shortest path. Exactly one vendor carries it.
	Recommended bool
	// Note is a paragraph the installer prints when this vendor is chosen and
	// something about it needs pre-empting.
	Note string
	// Custom marks the escape hatch, which sorts last.
	Custom bool
}

// SmallModelDefault and BigModelDefault are ORCHESTRATOR.md §2b's two
// recommendations: a small fast one that speaks, and the strongest available
// one to do the work.
//
// OpenClaw's wizard attaches a security note to this exact choice that applies
// to us more, not less: our big model holds the MCP registry and a shell, so it
// is not the place to economise — weaker tiers are easier to prompt-inject.
const (
	SmallModelDefault = "openai/gpt-5.6-luna"
	BigModelDefault   = "anthropic/opus-5"
	RecommendedVendor = "openrouter"
)

// Vendors returns the grouped vendor list, OpenRouter first because it is
// recommended and Custom Provider last because it is the escape hatch.
func Vendors() []VendorEntry {
	return []VendorEntry{
		{
			ID:          "openrouter",
			Label:       "OpenRouter",
			Hint:        "API key",
			API:         APIOpenAI,
			BaseURL:     "https://openrouter.ai/api/v1",
			Recommended: true,
			Note: "One key covers both models, and either can be swapped later without " +
				"re-running setup. Everything else on this list works; this is just the " +
				"shortest path.",
			Auths: []Auth{{ID: "openrouter-key", Label: "API key", Kind: AuthAPIKey}},
		},
		{
			ID:      "openai",
			Label:   "OpenAI",
			Hint:    "Codex OAuth + API key",
			API:     APIOpenAI,
			BaseURL: "https://api.openai.com/v1",
			Auths: []Auth{
				{ID: "openai-codex", Label: "OpenAI Codex (ChatGPT OAuth)", Kind: AuthSubscription},
				{ID: "openai-key", Label: "API key", Kind: AuthAPIKey},
			},
		},
		{
			ID:      "anthropic",
			Label:   "Anthropic",
			Hint:    "API key",
			API:     APIAnthropic,
			BaseURL: "https://api.anthropic.com",
			// The confusion here is predictable, so it is pre-empted rather than
			// left for a support ticket.
			Note: "Your Claude Max plan still powers Claude Code on this machine — that is " +
				"Anthropic's own client using its own login, and nothing about it changes. " +
				"What it cannot do is power our orchestrator. That part needs an API key. " +
				"claude setup-token and the community max-api-proxy both technically work, " +
				"and both are subscription credentials used outside Anthropic's own client; " +
				"Anthropic has blocked that before. We do not ship it as a row and will not " +
				"support it when it breaks.",
			Auths: []Auth{{ID: "anthropic-key", Label: "API key", Kind: AuthAPIKey}},
		},
		{
			ID:      "google",
			Label:   "Google",
			Hint:    "Gemini API key + OAuth",
			API:     APIOpenAI,
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			Auths: []Auth{
				{ID: "google-key", Label: "Gemini API key", Kind: AuthAPIKey},
				{
					ID: "google-gemini-cli", Label: "Gemini CLI (OAuth)", Kind: AuthOAuth,
					Risk: "Unofficial flow; review the account-risk warning before use.",
				},
			},
		},
		{
			ID:      "github-copilot",
			Label:   "GitHub Copilot",
			Hint:    "device login",
			API:     APIOpenAI,
			BaseURL: "https://api.githubcopilot.com",
			Auths:   []Auth{{ID: "copilot-device", Label: "GitHub device login", Kind: AuthDeviceCode}},
		},
		{
			ID:      "zai",
			Label:   "Z.AI",
			Hint:    "coding plan + API key",
			API:     APIOpenAI,
			BaseURL: "https://api.z.ai/api/paas/v4",
			Auths: []Auth{
				{ID: "zai-coding-plan", Label: "Coding plan", Kind: AuthSubscription},
				{ID: "zai-key", Label: "API key", Kind: AuthAPIKey},
			},
		},
		{
			ID:      "minimax",
			Label:   "MiniMax",
			Hint:    "coding plan + API key",
			API:     APIOpenAI,
			BaseURL: "https://api.minimax.io/v1",
			Auths: []Auth{
				{ID: "minimax-coding-plan", Label: "Coding plan", Kind: AuthSubscription},
				{ID: "minimax-key", Label: "API key", Kind: AuthAPIKey},
			},
		},
		{
			ID:      "qwen",
			Label:   "Qwen",
			Hint:    "OAuth + API key",
			API:     APIOpenAI,
			BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
			Auths: []Auth{
				{ID: "qwen-oauth", Label: "Qwen OAuth", Kind: AuthOAuth},
				{ID: "qwen-key", Label: "API key", Kind: AuthAPIKey},
			},
		},
		{
			ID:      "moonshot",
			Label:   "Moonshot (Kimi)",
			Hint:    "API key",
			API:     APIOpenAI,
			BaseURL: "https://api.moonshot.ai/v1",
			Auths:   []Auth{{ID: "moonshot-key", Label: "API key", Kind: AuthAPIKey}},
		},
		{
			ID:      "deepseek",
			Label:   "DeepSeek",
			Hint:    "API key",
			API:     APIOpenAI,
			BaseURL: "https://api.deepseek.com/v1",
			Auths:   []Auth{{ID: "deepseek-key", Label: "API key", Kind: AuthAPIKey}},
		},
		{
			ID:      "groq",
			Label:   "Groq",
			Hint:    "API key",
			API:     APIOpenAI,
			BaseURL: "https://api.groq.com/openai/v1",
			Auths:   []Auth{{ID: "groq-key", Label: "API key", Kind: AuthAPIKey}},
		},
		{
			ID:     "custom",
			Label:  "Custom Provider",
			Hint:   "Any OpenAI or Anthropic compatible endpoint",
			API:    APIOpenAI,
			Custom: true,
			Note:   "Base URL and model id, plus which shape it speaks. The list is never a cage.",
			Auths:  []Auth{{ID: "custom-key", Label: "API key", Kind: AuthAPIKey}},
		},
	}
}

// Vendor looks a vendor up by id.
func Vendor(id string) (VendorEntry, bool) {
	for _, v := range Vendors() {
		if v.ID == id {
			return v, true
		}
	}
	return VendorEntry{}, false
}
