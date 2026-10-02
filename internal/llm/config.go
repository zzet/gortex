// Package llm — config loader for the LLM service.
//
// This file is pure Go (no build tag) so every build can compile it.
// The actual provider construction lives under internal/llm/provider/
// — the `local` provider is the only one that needs `-tags llama`.
// `claudecli` shells out to the user's `claude` binary, so it only
// needs that binary on $PATH.
//
// Resolution order: file values are populated by the gortex config
// loader; MergeEnv overlays any GORTEX_LLM_* env var that's set (env
// wins); ApplyDefaults fills any remaining zero fields. A repo-local
// Config can additionally be layered over a global one via MergedWith.
package llm

import (
	"maps"
	"os"
	"strconv"
	"strings"
)

// Config is the YAML-friendly `llm:` block. The active backend is
// chosen by Provider; each provider reads its own sub-block, so a
// single config file can carry settings for several providers and
// switch between them by changing one key.
type Config struct {
	// Provider selects the inference backend: "local" (llama.cpp,
	// in-process, requires a `-tags llama` build), "anthropic",
	// "openai", "ollama", "claudecli" (subprocess against the
	// user's `claude` binary), "codex" (subprocess against the
	// user's OpenAI `codex` binary), "gemini" (Google Gemini REST
	// API), "bedrock" (AWS Bedrock Converse API, SigV4-signed),
	// "deepseek" (DeepSeek Chat Completions, OpenAI-compatible) or
	// "requesty" (Requesty gateway, OpenAI-compatible).
	// Empty defaults to "local".
	Provider string `mapstructure:"provider" yaml:"provider,omitempty"`

	// MaxSteps caps the agent tool-loop. Provider-agnostic. Defaults
	// to 16.
	MaxSteps int `mapstructure:"max_steps" yaml:"max_steps,omitempty"`

	// Local configures the in-process llama.cpp provider.
	Local LocalConfig `mapstructure:"local" yaml:"local,omitempty"`
	// Anthropic configures the hosted Anthropic Messages API provider —
	// a RemoteConfig plus Anthropic-only knobs (prompt caching, extended
	// thinking).
	Anthropic AnthropicConfig `mapstructure:"anthropic" yaml:"anthropic,omitempty"`
	// OpenAI configures the hosted OpenAI Chat Completions provider.
	OpenAI RemoteConfig `mapstructure:"openai" yaml:"openai,omitempty"`
	// Azure configures the Azure OpenAI Service provider — the OpenAI
	// Chat Completions wire format with a deployment-in-path /
	// api-version-query / api-key-header auth model.
	Azure AzureConfig `mapstructure:"azure" yaml:"azure,omitempty"`
	// Ollama configures a local/remote Ollama daemon provider.
	Ollama OllamaConfig `mapstructure:"ollama" yaml:"ollama,omitempty"`
	// ClaudeCLI configures the Claude Code CLI subprocess provider.
	ClaudeCLI ClaudeCLIConfig `mapstructure:"claudecli" yaml:"claudecli,omitempty"`
	// Gemini configures the Google Gemini generateContent REST provider.
	Gemini RemoteConfig `mapstructure:"gemini" yaml:"gemini,omitempty"`
	// Bedrock configures the AWS Bedrock Converse API provider. It is
	// SigV4-signed; the model is picked by `model_id`
	// (e.g. "anthropic.claude-sonnet-4-20250514-v1:0").
	Bedrock BedrockConfig `mapstructure:"bedrock" yaml:"bedrock,omitempty"`
	// DeepSeek configures the hosted DeepSeek Chat Completions
	// provider (api.deepseek.com, OpenAI-compatible wire format).
	DeepSeek RemoteConfig `mapstructure:"deepseek" yaml:"deepseek,omitempty"`
	// Requesty configures the hosted Requesty gateway provider
	// (router.requesty.ai, OpenAI-compatible wire format). Model ids
	// carry a vendor prefix, e.g. "openai/gpt-4o-mini".
	Requesty RemoteConfig `mapstructure:"requesty" yaml:"requesty,omitempty"`
	// Codex configures the OpenAI Codex CLI subprocess provider.
	Codex CodexConfig `mapstructure:"codex" yaml:"codex,omitempty"`
	// Copilot configures the GitHub Copilot CLI subprocess provider
	// (`copilot -p`), reusing the user's `gh` / Copilot sign-in.
	Copilot CLIConfig `mapstructure:"copilot" yaml:"copilot,omitempty"`
	// Cursor configures the Cursor Agent CLI subprocess provider
	// (`cursor-agent -p`), reusing the user's Cursor sign-in.
	Cursor CLIConfig `mapstructure:"cursor" yaml:"cursor,omitempty"`
	// Opencode configures the opencode CLI subprocess provider
	// (`opencode run`), reusing the user's opencode credentials.
	Opencode CLIConfig `mapstructure:"opencode" yaml:"opencode,omitempty"`

	// Routing configures graph-aware model routing for the `ask`
	// research agent — see RoutingConfig. Disabled by default: every
	// request runs on the active provider's configured model.
	Routing RoutingConfig `mapstructure:"routing" yaml:"routing,omitempty"`

	// Custom holds user-registered OpenAI-compatible providers, keyed
	// by name. Entries may come from an inline `llm.custom:` block or,
	// more commonly, from the registry file managed by `gortex provider
	// add/remove` (providers.json) — the config loader merges the two.
	// Selecting one is just setting Provider to its name.
	Custom map[string]CustomProvider `mapstructure:"custom" yaml:"custom,omitempty"`
}

// CustomProvider describes a user-registered OpenAI-compatible LLM
// endpoint — any service that exposes a /chat/completions API in the
// OpenAI wire format (OpenRouter, Groq, Together, a self-hosted vLLM,
// an internal gateway, …). It is dispatched through the shared
// OpenAI-compatible client; only the addressing, key, and
// structured-output strategy differ per entry. The registry key is the
// provider name, so it is not repeated here.
type CustomProvider struct {
	// BaseURL is the OpenAI-compatible API base, including any version
	// segment (e.g. "https://api.groq.com/openai/v1"). gortex appends
	// "/chat/completions". Must be http or https.
	BaseURL string `mapstructure:"base_url" yaml:"base_url" json:"base_url"`
	// Model is the default model identifier sent in the request body.
	Model string `mapstructure:"model" yaml:"model" json:"model"`
	// APIKeyEnv names the env var holding the bearer key. Empty is
	// allowed for keyless local endpoints (no Authorization header).
	APIKeyEnv string `mapstructure:"api_key_env" yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	// SchemaMode selects the structured-output strategy:
	// "json_schema" (default — strict OpenAI structured outputs),
	// "json_object" (response_format json_object + prompt rider), or
	// "prompt" (prompt rider only). Use the looser modes for gateways
	// that don't implement strict json_schema.
	SchemaMode string `mapstructure:"schema_mode" yaml:"schema_mode,omitempty" json:"schema_mode,omitempty"`
	// MaxTokensField overrides the output-token-cap body key
	// ("max_completion_tokens" by default; some gateways still want the
	// legacy "max_tokens").
	MaxTokensField string `mapstructure:"max_tokens_field" yaml:"max_tokens_field,omitempty" json:"max_tokens_field,omitempty"`
	// Temperature, when set, is sent as the `temperature` body field.
	Temperature *float64 `mapstructure:"temperature" yaml:"temperature,omitempty" json:"temperature,omitempty"`
	// ReasoningEffort, when set, is sent as `reasoning_effort`.
	ReasoningEffort string `mapstructure:"reasoning_effort" yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	// Headers are extra HTTP headers applied to every request (e.g. an
	// OpenRouter `HTTP-Referer`).
	Headers map[string]string `mapstructure:"headers" yaml:"headers,omitempty" json:"headers,omitempty"`
	// Pricing is optional USD-per-1M-token pricing, surfaced by
	// `gortex provider show` and EstimateCost. Purely informational.
	Pricing ProviderPricing `mapstructure:"pricing" yaml:"pricing,omitempty" json:"pricing,omitempty"`
}

// ProviderPricing is the optional USD-per-1M-token rate card for a
// custom provider.
type ProviderPricing struct {
	Input  float64 `mapstructure:"input" yaml:"input,omitempty" json:"input,omitempty"`
	Output float64 `mapstructure:"output" yaml:"output,omitempty" json:"output,omitempty"`
}

// RoutingConfig is the `llm.routing:` sub-block — model routing for
// the `ask` agent. When Enabled, each agent run is classified by
// graph-derived task complexity (chain mode, multi-hop keywords,
// cross-repo scope breadth — see Classify) and dispatched to a
// cheaper or more capable model *within the active provider*: a
// trivial single-hop lookup to SimpleModel, a cross-system trace or
// refactor to ComplexModel. An empty tier model means "use the
// provider's configured model for that tier" — so routing can be set
// up to only upgrade hard tasks, or only downgrade easy ones.
type RoutingConfig struct {
	// Enabled turns model routing on. Off by default.
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	// SimpleModel is the model id for low-complexity agent runs (e.g.
	// "claude-haiku-4-5"). Empty falls back to the configured model.
	SimpleModel string `mapstructure:"simple_model" yaml:"simple_model,omitempty"`
	// ComplexModel is the model id for high-complexity agent runs
	// (e.g. "claude-opus-4-7"). Empty falls back to the configured
	// model.
	ComplexModel string `mapstructure:"complex_model" yaml:"complex_model,omitempty"`
}

// LocalConfig is the `llm.local:` sub-block — settings for the
// in-process llama.cpp provider.
type LocalConfig struct {
	// Model is the path to a .gguf model file. Required for the local
	// provider — empty disables it.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Ctx is the context window in tokens. Defaults to 4096.
	Ctx int `mapstructure:"ctx" yaml:"ctx,omitempty"`
	// GPULayers is the number of layers to offload to GPU (Metal /
	// CUDA). 999 = all, 0 = CPU-only. Defaults to 999.
	GPULayers int `mapstructure:"gpu_layers" yaml:"gpu_layers,omitempty"`
	// Template is the chat-template family: "chatml" (Qwen2.5,
	// Hermes-3) or "llama3" (Llama-3.x native). Defaults to "chatml".
	Template string `mapstructure:"template" yaml:"template,omitempty"`
	// IdleTTL is the idle window after which the loaded model is unloaded to
	// reclaim its (V)RAM — a Go duration string ("10m", the default when
	// empty). "0"/"off"/"none" keeps the model resident once loaded. The
	// GORTEX_LLM_IDLE_TTL env var overrides this at runtime (env wins).
	IdleTTL string `mapstructure:"idle_ttl" yaml:"idle_ttl,omitempty"`
}

// RemoteConfig is the sub-block shared by the HTTP API providers
// (Anthropic, OpenAI).
type RemoteConfig struct {
	// Model is the API model identifier (e.g. "claude-sonnet-4-6",
	// "gpt-4o"). Defaulted per provider by ApplyDefaults.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// APIKeyEnv names the environment variable holding the API key.
	// Defaulted per provider by ApplyDefaults. The key itself is never
	// stored in the config file.
	APIKeyEnv string `mapstructure:"api_key_env" yaml:"api_key_env,omitempty"`
	// BaseURL overrides the API endpoint (proxies, gateways, Azure).
	// Defaulted per provider by ApplyDefaults.
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
	// Effort is an optional reasoning-effort level. Off by default.
	// Anthropic sends it as output_config.effort (low|medium|high|max|
	// xhigh, gated by model capability); OpenAI sends it as
	// reasoning_effort (minimal|low|medium|high). Empty leaves the knob
	// untouched.
	Effort string `mapstructure:"effort" yaml:"effort,omitempty"`
}

// AnthropicConfig is the `llm.anthropic:` sub-block — a RemoteConfig
// (model / api_key_env / base_url / effort) plus knobs unique to the
// Anthropic Messages API. The embedded RemoteConfig is squashed, so its
// fields sit directly under `llm.anthropic:` in config.
type AnthropicConfig struct {
	RemoteConfig `mapstructure:",squash" yaml:",inline"`

	// PromptCaching opts into Anthropic prompt caching: the system
	// prompt (and the structured-output tool) are marked as cache
	// breakpoints so a repeated stable prefix is billed at the cache-hit
	// rate. Off by default — gortex requests rarely reuse a prefix often
	// enough to offset the cache-write surcharge.
	PromptCaching bool `mapstructure:"prompt_caching" yaml:"prompt_caching,omitempty"`
	// CacheTTL is the cache lifetime: "5m" (default when caching is on,
	// free to refresh) or "1h" (2x write cost, for prefixes reused less
	// often than every 5 minutes). Ignored when PromptCaching is off.
	CacheTTL string `mapstructure:"cache_ttl" yaml:"cache_ttl,omitempty"`
	// ThinkingMode controls extended thinking on freeform completions:
	// "off" (default), "auto" (adaptive on capable models, else manual),
	// "manual" (a fixed token budget), or "adaptive" (the model decides).
	// Thinking is incompatible with the forced-tool structured-output
	// path, so it is applied only to freeform requests.
	ThinkingMode string `mapstructure:"thinking_mode" yaml:"thinking_mode,omitempty"`
	// ThinkingBudgetTokens is the manual-mode thinking budget (min 1024).
	// Ignored in adaptive/off modes; defaults to a modest budget when
	// manual mode is selected without one.
	ThinkingBudgetTokens int `mapstructure:"thinking_budget_tokens" yaml:"thinking_budget_tokens,omitempty"`
	// ThinkingDisplay is the adaptive-mode thinking visibility:
	// "summarized" or "omitted". Optional.
	ThinkingDisplay string `mapstructure:"thinking_display" yaml:"thinking_display,omitempty"`
}

// AzureConfig is the `llm.azure:` sub-block — settings for the Azure
// OpenAI Service provider. Azure speaks the OpenAI Chat Completions
// wire format but addresses and authenticates requests differently
// from api.openai.com: the model is selected by a deployment name
// folded into the URL path, the API contract is pinned by an
// `api-version` query parameter, and the key travels in an `api-key`
// header (not a Bearer token). A bare RemoteConfig.BaseURL override
// cannot express that, which is why Azure gets its own sub-block.
type AzureConfig struct {
	// Endpoint is the Azure OpenAI resource endpoint, e.g.
	// "https://my-resource.openai.azure.com". When empty it is read
	// from the env var named by EndpointEnv.
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint,omitempty"`
	// EndpointEnv names the env var holding the endpoint when Endpoint
	// is unset. Defaults to "AZURE_OPENAI_ENDPOINT".
	EndpointEnv string `mapstructure:"endpoint_env" yaml:"endpoint_env,omitempty"`
	// Deployment is the Azure deployment name — the path segment that
	// selects the model. Required; empty disables the provider.
	Deployment string `mapstructure:"deployment" yaml:"deployment,omitempty"`
	// APIVersion pins the Azure REST API contract (e.g. "2024-10-21").
	// Defaults to a recent GA version that supports json_schema
	// structured outputs.
	APIVersion string `mapstructure:"api_version" yaml:"api_version,omitempty"`
	// Model is the logical model identifier sent in the request body.
	// Optional — Azure routes by Deployment, so this defaults to the
	// deployment name and rarely needs setting.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// APIKeyEnv names the env var holding the Azure API key. Defaults
	// to "AZURE_OPENAI_API_KEY". The key itself is never stored in the
	// config file.
	APIKeyEnv string `mapstructure:"api_key_env" yaml:"api_key_env,omitempty"`
}

// OllamaConfig is the `llm.ollama:` sub-block.
type OllamaConfig struct {
	// Model is the Ollama model tag (e.g. "qwen2.5-coder:7b").
	// Required for the Ollama provider — empty disables it.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Host is the Ollama daemon base URL. Defaults to
	// "http://localhost:11434".
	Host string `mapstructure:"host" yaml:"host,omitempty"`
}

// BedrockConfig is the `llm.bedrock:` sub-block — settings for the
// AWS Bedrock Converse API provider. Authentication is by SigV4 over
// the two AWS credential env vars (key id + secret), with an optional
// session token for STS-issued credentials.
type BedrockConfig struct {
	// ModelID is the Bedrock model identifier
	// (e.g. "anthropic.claude-sonnet-4-20250514-v1:0",
	// "amazon.nova-pro-v1:0"). Required — empty disables the provider.
	ModelID string `mapstructure:"model_id" yaml:"model_id,omitempty"`
	// Region is the AWS region the model is hosted in
	// (e.g. "us-east-1"). Defaults to "us-east-1".
	Region string `mapstructure:"region" yaml:"region,omitempty"`
	// AccessKeyEnv names the env var holding the AWS access key id.
	// Defaults to "AWS_ACCESS_KEY_ID".
	AccessKeyEnv string `mapstructure:"access_key_env" yaml:"access_key_env,omitempty"`
	// SecretKeyEnv names the env var holding the AWS secret key.
	// Defaults to "AWS_SECRET_ACCESS_KEY".
	SecretKeyEnv string `mapstructure:"secret_key_env" yaml:"secret_key_env,omitempty"`
	// SessionTokenEnv names the env var holding an optional STS
	// session token. Defaults to "AWS_SESSION_TOKEN". Unset is fine.
	SessionTokenEnv string `mapstructure:"session_token_env" yaml:"session_token_env,omitempty"`
	// BaseURL overrides the endpoint — useful for VPC endpoints.
	// Defaults to "https://bedrock-runtime.<region>.amazonaws.com".
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
}

// ClaudeCLIConfig is the `llm.claudecli:` sub-block — settings for
// the subprocess provider that shells out to the user's local
// Claude Code CLI. The binary must already be installed and signed
// in; gortex never touches credentials directly.
type ClaudeCLIConfig struct {
	// Binary is the executable name or absolute path. Empty defaults
	// to "claude" (resolved via $PATH).
	Binary string `mapstructure:"binary" yaml:"binary,omitempty"`
	// Model is the Claude model alias forwarded as `--model` (e.g.
	// "sonnet", "opus", "claude-sonnet-4-6"). Empty lets the CLI
	// pick its own default.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Args is a list of extra arguments appended after the provider's
	// own flags. It also overrides them: the provider skips any of its
	// headless defaults (`--tools ""`, `--strict-mcp-config`,
	// `--settings '{"disableAllHooks":true}'`, `--system-prompt`) whose
	// flag appears here. `["--tools", "default"]` restores the built-in
	// toolset; `["--permission-mode", "plan"]` picks a read-only
	// profile.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// TimeoutSeconds caps one Complete call. 0 → 120s.
	TimeoutSeconds int `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
}

// CodexConfig is the `llm.codex:` sub-block — settings for the
// subprocess provider that shells out to the user's local OpenAI
// Codex CLI. The binary must already be installed and signed in;
// gortex never touches credentials directly. Field shape mirrors
// ClaudeCLIConfig — both are CLI-subprocess providers.
type CodexConfig struct {
	// Binary is the executable name or absolute path. Empty defaults
	// to "codex" (resolved via $PATH).
	Binary string `mapstructure:"binary" yaml:"binary,omitempty"`
	// Model is the model slug forwarded as `--model` (e.g. "gpt-5-codex",
	// "o4-mini"). Empty lets the CLI pick its own default.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Args is a list of extra arguments inserted before the prompt
	// positional. Useful for `--sandbox workspace-write` or a `--config`
	// override when the defaults don't fit.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// TimeoutSeconds caps one Complete call. 0 → 180s.
	TimeoutSeconds int `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
}

// CLIConfig is the shared `llm.<copilot|cursor|opencode>:` sub-block —
// the simple coding-agent CLI subprocess providers. Each shells out to
// a locally installed binary that is already signed in (GitHub Copilot,
// Cursor, opencode), so gortex never handles an API key. Field shape
// mirrors ClaudeCLIConfig / CodexConfig; the per-provider binary
// default and argv shape live in each provider package.
type CLIConfig struct {
	// Binary is the executable name or absolute path. Empty defaults to
	// the provider's canonical binary (copilot / cursor-agent / opencode).
	Binary string `mapstructure:"binary" yaml:"binary,omitempty"`
	// Model is the model slug forwarded with the provider's model flag.
	// Empty lets the CLI pick its own default.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Args is a list of extra arguments appended after the provider's
	// own flags (e.g. a sandbox or permission flag).
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// TimeoutSeconds caps one Complete call. 0 → 180s.
	TimeoutSeconds int `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
}

// Default endpoints / key env vars, applied by ApplyDefaults.
const (
	defaultAnthropicModel   = "claude-sonnet-4-6"
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultAnthropicKeyEnv  = "ANTHROPIC_API_KEY"

	defaultOpenAIModel   = "gpt-4o"
	defaultOpenAIBaseURL = "https://api.openai.com"
	defaultOpenAIKeyEnv  = "OPENAI_API_KEY"

	defaultAzureAPIVersion  = "2024-10-21"
	defaultAzureEndpointEnv = "AZURE_OPENAI_ENDPOINT"
	defaultAzureKeyEnv      = "AZURE_OPENAI_API_KEY"

	defaultOllamaHost = "http://localhost:11434"

	defaultClaudeCLIBinary = "claude"

	defaultCodexBinary = "codex"

	defaultCopilotBinary  = "copilot"
	defaultCursorBinary   = "cursor-agent"
	defaultOpencodeBinary = "opencode"

	defaultGeminiModel   = "gemini-2.5-pro"
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	defaultGeminiKeyEnv  = "GEMINI_API_KEY"

	defaultBedrockRegion          = "us-east-1"
	defaultBedrockAccessKeyEnv    = "AWS_ACCESS_KEY_ID"
	defaultBedrockSecretKeyEnv    = "AWS_SECRET_ACCESS_KEY"
	defaultBedrockSessionTokenEnv = "AWS_SESSION_TOKEN"

	defaultDeepSeekModel   = "deepseek-chat"
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekKeyEnv  = "DEEPSEEK_API_KEY"

	defaultRequestyModel   = "openai/gpt-4o-mini"
	defaultRequestyBaseURL = "https://router.requesty.ai/v1"
	defaultRequestyKeyEnv  = "REQUESTY_API_KEY"
)

// builtinProviders is the set of reserved provider names — the ones
// provider.New dispatches by a fixed case. A custom provider may not
// shadow these.
var builtinProviders = map[string]bool{
	"local": true, "anthropic": true, "openai": true, "azure": true,
	"ollama": true, "claudecli": true, "codex": true, "copilot": true,
	"cursor": true, "opencode": true, "gemini": true, "bedrock": true,
	"deepseek": true, "requesty": true,
}

// IsBuiltinProvider reports whether name is a reserved built-in provider
// (so the custom-provider registry can refuse to shadow one). The name
// is matched case-insensitively after trimming.
func IsBuiltinProvider(name string) bool {
	return builtinProviders[strings.ToLower(strings.TrimSpace(name))]
}

// ProviderName returns the effective provider, applying the "local"
// default for an empty value.
func (c Config) ProviderName() string {
	if strings.TrimSpace(c.Provider) == "" {
		return "local"
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

// ActiveModel returns the model id of the active provider — the field
// the GORTEX_LLM_MODEL env var and Config.WithModel target. For the
// local provider this is the .gguf path; for bedrock the model_id.
func (c Config) ActiveModel() string {
	switch c.ProviderName() {
	case "anthropic":
		return c.Anthropic.Model
	case "openai":
		return c.OpenAI.Model
	case "azure":
		return c.Azure.Deployment
	case "ollama":
		return c.Ollama.Model
	case "claudecli":
		return c.ClaudeCLI.Model
	case "codex":
		return c.Codex.Model
	case "copilot":
		return c.Copilot.Model
	case "cursor":
		return c.Cursor.Model
	case "opencode":
		return c.Opencode.Model
	case "gemini":
		return c.Gemini.Model
	case "bedrock":
		return c.Bedrock.ModelID
	case "deepseek":
		return c.DeepSeek.Model
	case "requesty":
		return c.Requesty.Model
	default:
		if cp, ok := c.Custom[c.ProviderName()]; ok {
			return cp.Model
		}
		return c.Local.Model
	}
}

// WithModel returns a copy of c with the active provider's model field
// set to model. An empty model is a no-op (returns c unchanged). Used
// by model routing to derive a per-request provider config without
// disturbing any other provider's sub-block.
func (c Config) WithModel(model string) Config {
	if strings.TrimSpace(model) == "" {
		return c
	}
	switch c.ProviderName() {
	case "anthropic":
		c.Anthropic.Model = model
	case "openai":
		c.OpenAI.Model = model
	case "azure":
		c.Azure.Deployment = model
	case "ollama":
		c.Ollama.Model = model
	case "claudecli":
		c.ClaudeCLI.Model = model
	case "codex":
		c.Codex.Model = model
	case "copilot":
		c.Copilot.Model = model
	case "cursor":
		c.Cursor.Model = model
	case "opencode":
		c.Opencode.Model = model
	case "gemini":
		c.Gemini.Model = model
	case "bedrock":
		c.Bedrock.ModelID = model
	case "deepseek":
		c.DeepSeek.Model = model
	case "requesty":
		c.Requesty.Model = model
	default:
		// For a custom provider, copy-on-write the Custom map so routing
		// (which derives per-request configs via WithModel) never
		// mutates the shared base config.
		if cp, ok := c.Custom[c.ProviderName()]; ok {
			nc := make(map[string]CustomProvider, len(c.Custom))
			maps.Copy(nc, c.Custom)
			cp.Model = model
			nc[c.ProviderName()] = cp
			c.Custom = nc
			return c
		}
		c.Local.Model = model
	}
	return c
}

// IsEnabled reports whether the config carries enough to start the
// active provider. A provider is enabled once its required fields are
// set: the local and Ollama providers need a model; the hosted
// providers need a model (defaulted) — the API key is validated at
// provider-construction time, not here. The Claude CLI provider has
// no required field — `binary` defaults to "claude" and `model` is
// optional — so selecting it via Provider is sufficient.
func (c Config) IsEnabled() bool {
	switch c.ProviderName() {
	case "local":
		return strings.TrimSpace(c.Local.Model) != ""
	case "anthropic":
		return strings.TrimSpace(c.Anthropic.Model) != ""
	case "openai":
		return strings.TrimSpace(c.OpenAI.Model) != ""
	case "azure":
		return strings.TrimSpace(c.Azure.Deployment) != ""
	case "ollama":
		return strings.TrimSpace(c.Ollama.Model) != ""
	case "claudecli", "codex", "copilot", "cursor", "opencode":
		return true
	case "gemini":
		return strings.TrimSpace(c.Gemini.Model) != ""
	case "bedrock":
		return strings.TrimSpace(c.Bedrock.ModelID) != ""
	case "deepseek":
		return strings.TrimSpace(c.DeepSeek.Model) != ""
	case "requesty":
		return strings.TrimSpace(c.Requesty.Model) != ""
	default:
		// A registered custom OpenAI-compatible provider is enabled
		// once it carries a model; the API key (if any) is validated at
		// construction time.
		if cp, ok := c.Custom[c.ProviderName()]; ok {
			return strings.TrimSpace(cp.Model) != ""
		}
		return false
	}
}

// MergeEnv overlays any GORTEX_LLM_* env var on top of the file
// values, then applies defaults. Env wins over file. GORTEX_LLM_MODEL
// targets the *active* provider's model so the common "swap the
// model" case needs only one variable.
func (c Config) MergeEnv() Config {
	if v := os.Getenv("GORTEX_LLM_PROVIDER"); v != "" {
		c.Provider = v
	}
	if v := os.Getenv("GORTEX_LLM_MAX_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxSteps = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_MODEL"); v != "" {
		switch c.ProviderName() {
		case "anthropic":
			c.Anthropic.Model = v
		case "openai":
			c.OpenAI.Model = v
		case "azure":
			c.Azure.Deployment = v
		case "ollama":
			c.Ollama.Model = v
		case "claudecli":
			c.ClaudeCLI.Model = v
		case "codex":
			c.Codex.Model = v
		case "copilot":
			c.Copilot.Model = v
		case "cursor":
			c.Cursor.Model = v
		case "opencode":
			c.Opencode.Model = v
		case "gemini":
			c.Gemini.Model = v
		case "bedrock":
			c.Bedrock.ModelID = v
		case "deepseek":
			c.DeepSeek.Model = v
		case "requesty":
			c.Requesty.Model = v
		default:
			c.Local.Model = v
		}
	}
	if v := os.Getenv("GORTEX_LLM_CLAUDECLI_BINARY"); v != "" {
		c.ClaudeCLI.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_CODEX_BINARY"); v != "" {
		c.Codex.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_COPILOT_BINARY"); v != "" {
		c.Copilot.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_CURSOR_BINARY"); v != "" {
		c.Cursor.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_OPENCODE_BINARY"); v != "" {
		c.Opencode.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_BEDROCK_REGION"); v != "" {
		c.Bedrock.Region = v
	}
	if v := os.Getenv("GORTEX_LLM_AZURE_ENDPOINT"); v != "" {
		c.Azure.Endpoint = v
	}
	if v := os.Getenv("GORTEX_LLM_AZURE_DEPLOYMENT"); v != "" {
		c.Azure.Deployment = v
	}
	if v := os.Getenv("GORTEX_LLM_AZURE_API_VERSION"); v != "" {
		c.Azure.APIVersion = v
	}
	if v := os.Getenv("GORTEX_LLM_ANTHROPIC_PROMPT_CACHING"); v != "" {
		c.Anthropic.PromptCaching = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("GORTEX_LLM_ANTHROPIC_THINKING_MODE"); v != "" {
		c.Anthropic.ThinkingMode = v
	}
	if v := os.Getenv("GORTEX_LLM_EFFORT"); v != "" {
		switch c.ProviderName() {
		case "anthropic":
			c.Anthropic.Effort = v
		case "openai":
			c.OpenAI.Effort = v
		case "gemini":
			c.Gemini.Effort = v
		case "deepseek":
			c.DeepSeek.Effort = v
		case "requesty":
			c.Requesty.Effort = v
		}
	}
	if v := os.Getenv("GORTEX_LLM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Local.Ctx = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_GPU_LAYERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Local.GPULayers = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_TEMPLATE"); v != "" {
		c.Local.Template = v
	}
	return c.ApplyDefaults()
}

// ApplyDefaults fills zero-valued fields with the canonical defaults.
// Called by MergeEnv; safe to call standalone and idempotent.
func (c Config) ApplyDefaults() Config {
	if strings.TrimSpace(c.Provider) == "" {
		c.Provider = "local"
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = 16
	}

	// local
	if c.Local.Ctx == 0 {
		c.Local.Ctx = 4096
	}
	if c.Local.GPULayers == 0 {
		// 0 is indistinguishable from "unset" at the struct level; the
		// default offloads all layers. A user wanting CPU-only sets a
		// negative value? No — convention: explicit 0 in YAML still
		// reads as 0 here, so we can't honour CPU-only via this field
		// cleanly. 999 is the safe, fast default.
		c.Local.GPULayers = 999
	}
	if c.Local.Template == "" {
		c.Local.Template = "chatml"
	}

	// anthropic
	if c.Anthropic.Model == "" {
		c.Anthropic.Model = defaultAnthropicModel
	}
	if c.Anthropic.APIKeyEnv == "" {
		c.Anthropic.APIKeyEnv = defaultAnthropicKeyEnv
	}
	if c.Anthropic.BaseURL == "" {
		c.Anthropic.BaseURL = defaultAnthropicBaseURL
	}

	// openai
	if c.OpenAI.Model == "" {
		c.OpenAI.Model = defaultOpenAIModel
	}
	if c.OpenAI.APIKeyEnv == "" {
		c.OpenAI.APIKeyEnv = defaultOpenAIKeyEnv
	}
	if c.OpenAI.BaseURL == "" {
		c.OpenAI.BaseURL = defaultOpenAIBaseURL
	}

	// azure
	if c.Azure.APIVersion == "" {
		c.Azure.APIVersion = defaultAzureAPIVersion
	}
	if c.Azure.EndpointEnv == "" {
		c.Azure.EndpointEnv = defaultAzureEndpointEnv
	}
	if c.Azure.APIKeyEnv == "" {
		c.Azure.APIKeyEnv = defaultAzureKeyEnv
	}

	// ollama
	if c.Ollama.Host == "" {
		c.Ollama.Host = defaultOllamaHost
	}

	// claudecli
	if c.ClaudeCLI.Binary == "" {
		c.ClaudeCLI.Binary = defaultClaudeCLIBinary
	}

	// codex
	if c.Codex.Binary == "" {
		c.Codex.Binary = defaultCodexBinary
	}

	// copilot / cursor / opencode CLI providers
	if c.Copilot.Binary == "" {
		c.Copilot.Binary = defaultCopilotBinary
	}
	if c.Cursor.Binary == "" {
		c.Cursor.Binary = defaultCursorBinary
	}
	if c.Opencode.Binary == "" {
		c.Opencode.Binary = defaultOpencodeBinary
	}

	// gemini
	if c.Gemini.Model == "" {
		c.Gemini.Model = defaultGeminiModel
	}
	if c.Gemini.APIKeyEnv == "" {
		c.Gemini.APIKeyEnv = defaultGeminiKeyEnv
	}
	if c.Gemini.BaseURL == "" {
		c.Gemini.BaseURL = defaultGeminiBaseURL
	}

	// bedrock
	if c.Bedrock.Region == "" {
		c.Bedrock.Region = defaultBedrockRegion
	}
	if c.Bedrock.AccessKeyEnv == "" {
		c.Bedrock.AccessKeyEnv = defaultBedrockAccessKeyEnv
	}
	if c.Bedrock.SecretKeyEnv == "" {
		c.Bedrock.SecretKeyEnv = defaultBedrockSecretKeyEnv
	}
	if c.Bedrock.SessionTokenEnv == "" {
		c.Bedrock.SessionTokenEnv = defaultBedrockSessionTokenEnv
	}

	// deepseek
	if c.DeepSeek.Model == "" {
		c.DeepSeek.Model = defaultDeepSeekModel
	}
	if c.DeepSeek.APIKeyEnv == "" {
		c.DeepSeek.APIKeyEnv = defaultDeepSeekKeyEnv
	}
	if c.DeepSeek.BaseURL == "" {
		c.DeepSeek.BaseURL = defaultDeepSeekBaseURL
	}

	// requesty
	if c.Requesty.Model == "" {
		c.Requesty.Model = defaultRequestyModel
	}
	if c.Requesty.APIKeyEnv == "" {
		c.Requesty.APIKeyEnv = defaultRequestyKeyEnv
	}
	if c.Requesty.BaseURL == "" {
		c.Requesty.BaseURL = defaultRequestyBaseURL
	}

	return c
}

// MergedWith returns c with each zero-valued field filled from fb.
// Non-zero fields of c always win — including an explicit per-repo
// override of an inherited global value. Used to layer a repo-local
// Config (c) over a global user Config (fb). Call before ApplyDefaults
// so genuine zero values still merge.
func (c Config) MergedWith(fb Config) Config {
	if c.Provider == "" {
		c.Provider = fb.Provider
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = fb.MaxSteps
	}
	c.Local = c.Local.mergedWith(fb.Local)
	c.Anthropic = c.Anthropic.mergedWith(fb.Anthropic)
	c.OpenAI = c.OpenAI.mergedWith(fb.OpenAI)
	c.Azure = c.Azure.mergedWith(fb.Azure)
	c.Ollama = c.Ollama.mergedWith(fb.Ollama)
	c.ClaudeCLI = c.ClaudeCLI.mergedWith(fb.ClaudeCLI)
	c.Gemini = c.Gemini.mergedWith(fb.Gemini)
	c.Bedrock = c.Bedrock.mergedWith(fb.Bedrock)
	c.DeepSeek = c.DeepSeek.mergedWith(fb.DeepSeek)
	c.Requesty = c.Requesty.mergedWith(fb.Requesty)
	c.Codex = c.Codex.mergedWith(fb.Codex)
	c.Copilot = c.Copilot.mergedWith(fb.Copilot)
	c.Cursor = c.Cursor.mergedWith(fb.Cursor)
	c.Opencode = c.Opencode.mergedWith(fb.Opencode)
	c.Routing = c.Routing.mergedWith(fb.Routing)
	if len(fb.Custom) > 0 {
		merged := make(map[string]CustomProvider, len(fb.Custom)+len(c.Custom))
		maps.Copy(merged, fb.Custom) // global base
		maps.Copy(merged, c.Custom)  // local overrides per-key
		c.Custom = merged
	}
	return c
}

func (r RoutingConfig) mergedWith(fb RoutingConfig) RoutingConfig {
	if !r.Enabled {
		r.Enabled = fb.Enabled
	}
	if r.SimpleModel == "" {
		r.SimpleModel = fb.SimpleModel
	}
	if r.ComplexModel == "" {
		r.ComplexModel = fb.ComplexModel
	}
	return r
}

func (l LocalConfig) mergedWith(fb LocalConfig) LocalConfig {
	if l.Model == "" {
		l.Model = fb.Model
	}
	if l.Ctx == 0 {
		l.Ctx = fb.Ctx
	}
	if l.GPULayers == 0 {
		l.GPULayers = fb.GPULayers
	}
	if l.Template == "" {
		l.Template = fb.Template
	}
	return l
}

func (a AnthropicConfig) mergedWith(fb AnthropicConfig) AnthropicConfig {
	a.RemoteConfig = a.RemoteConfig.mergedWith(fb.RemoteConfig)
	if !a.PromptCaching {
		a.PromptCaching = fb.PromptCaching
	}
	if a.CacheTTL == "" {
		a.CacheTTL = fb.CacheTTL
	}
	if a.ThinkingMode == "" {
		a.ThinkingMode = fb.ThinkingMode
	}
	if a.ThinkingBudgetTokens == 0 {
		a.ThinkingBudgetTokens = fb.ThinkingBudgetTokens
	}
	if a.ThinkingDisplay == "" {
		a.ThinkingDisplay = fb.ThinkingDisplay
	}
	return a
}

func (r RemoteConfig) mergedWith(fb RemoteConfig) RemoteConfig {
	if r.Model == "" {
		r.Model = fb.Model
	}
	if r.APIKeyEnv == "" {
		r.APIKeyEnv = fb.APIKeyEnv
	}
	if r.BaseURL == "" {
		r.BaseURL = fb.BaseURL
	}
	if r.Effort == "" {
		r.Effort = fb.Effort
	}
	return r
}

func (a AzureConfig) mergedWith(fb AzureConfig) AzureConfig {
	if a.Endpoint == "" {
		a.Endpoint = fb.Endpoint
	}
	if a.EndpointEnv == "" {
		a.EndpointEnv = fb.EndpointEnv
	}
	if a.Deployment == "" {
		a.Deployment = fb.Deployment
	}
	if a.APIVersion == "" {
		a.APIVersion = fb.APIVersion
	}
	if a.Model == "" {
		a.Model = fb.Model
	}
	if a.APIKeyEnv == "" {
		a.APIKeyEnv = fb.APIKeyEnv
	}
	return a
}

func (o OllamaConfig) mergedWith(fb OllamaConfig) OllamaConfig {
	if o.Model == "" {
		o.Model = fb.Model
	}
	if o.Host == "" {
		o.Host = fb.Host
	}
	return o
}

func (b BedrockConfig) mergedWith(fb BedrockConfig) BedrockConfig {
	if b.ModelID == "" {
		b.ModelID = fb.ModelID
	}
	if b.Region == "" {
		b.Region = fb.Region
	}
	if b.AccessKeyEnv == "" {
		b.AccessKeyEnv = fb.AccessKeyEnv
	}
	if b.SecretKeyEnv == "" {
		b.SecretKeyEnv = fb.SecretKeyEnv
	}
	if b.SessionTokenEnv == "" {
		b.SessionTokenEnv = fb.SessionTokenEnv
	}
	if b.BaseURL == "" {
		b.BaseURL = fb.BaseURL
	}
	return b
}

func (c ClaudeCLIConfig) mergedWith(fb ClaudeCLIConfig) ClaudeCLIConfig {
	if c.Binary == "" {
		c.Binary = fb.Binary
	}
	if c.Model == "" {
		c.Model = fb.Model
	}
	if len(c.Args) == 0 {
		c.Args = fb.Args
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = fb.TimeoutSeconds
	}
	return c
}

func (c CodexConfig) mergedWith(fb CodexConfig) CodexConfig {
	if c.Binary == "" {
		c.Binary = fb.Binary
	}
	if c.Model == "" {
		c.Model = fb.Model
	}
	if len(c.Args) == 0 {
		c.Args = fb.Args
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = fb.TimeoutSeconds
	}
	return c
}

func (c CLIConfig) mergedWith(fb CLIConfig) CLIConfig {
	if c.Binary == "" {
		c.Binary = fb.Binary
	}
	if c.Model == "" {
		c.Model = fb.Model
	}
	if len(c.Args) == 0 {
		c.Args = fb.Args
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = fb.TimeoutSeconds
	}
	return c
}
