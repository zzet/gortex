package llm

import (
	"reflect"
	"testing"
)

func TestConfig_ProviderName_DefaultsToLocal(t *testing.T) {
	if got := (Config{}).ProviderName(); got != "local" {
		t.Fatalf("empty provider: got %q want local", got)
	}
	if got := (Config{Provider: "  Anthropic "}).ProviderName(); got != "anthropic" {
		t.Fatalf("normalisation: got %q want anthropic", got)
	}
}

func TestConfig_IsEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"local with model", Config{Provider: "local", Local: LocalConfig{Model: "/m.gguf"}}, true},
		{"local no model", Config{Provider: "local"}, false},
		{"anthropic with model", Config{Provider: "anthropic", Anthropic: AnthropicConfig{RemoteConfig: RemoteConfig{Model: "claude"}}}, true},
		{"anthropic no model", Config{Provider: "anthropic"}, false},
		{"openai with model", Config{Provider: "openai", OpenAI: RemoteConfig{Model: "gpt"}}, true},
		{"azure with deployment", Config{Provider: "azure", Azure: AzureConfig{Deployment: "gpt4o"}}, true},
		{"azure no deployment", Config{Provider: "azure"}, false},
		{"ollama with model", Config{Provider: "ollama", Ollama: OllamaConfig{Model: "qwen"}}, true},
		{"ollama no model", Config{Provider: "ollama"}, false},
		{"claudecli no model", Config{Provider: "claudecli"}, true},
		{"claudecli with model", Config{Provider: "claudecli", ClaudeCLI: ClaudeCLIConfig{Model: "sonnet"}}, true},
		{"unknown provider", Config{Provider: "bogus", Local: LocalConfig{Model: "/m.gguf"}}, false},
		{"gemini with model", Config{Provider: "gemini", Gemini: RemoteConfig{Model: "gemini-2.5-pro"}}, true},
		{"gemini no model", Config{Provider: "gemini"}, false},
		{"bedrock with model_id", Config{Provider: "bedrock", Bedrock: BedrockConfig{ModelID: "anthropic.claude-sonnet-4-20250514-v1:0"}}, true},
		{"bedrock no model_id", Config{Provider: "bedrock"}, false},
		{"deepseek with model", Config{Provider: "deepseek", DeepSeek: RemoteConfig{Model: "deepseek-chat"}}, true},
		{"deepseek no model", Config{Provider: "deepseek"}, false},
		{"requesty with model", Config{Provider: "requesty", Requesty: RemoteConfig{Model: "openai/gpt-4o-mini"}}, true},
		{"requesty no model", Config{Provider: "requesty"}, false},
		{"codex no model", Config{Provider: "codex"}, true},
		{"codex with model", Config{Provider: "codex", Codex: CodexConfig{Model: "gpt-5-codex"}}, true},
		{"copilot no model", Config{Provider: "copilot"}, true},
		{"cursor no model", Config{Provider: "cursor"}, true},
		{"opencode no model", Config{Provider: "opencode"}, true},
		{"custom with model", Config{Provider: "mygw", Custom: map[string]CustomProvider{"mygw": {BaseURL: "https://x/v1", Model: "m"}}}, true},
		{"custom no model", Config{Provider: "mygw", Custom: map[string]CustomProvider{"mygw": {BaseURL: "https://x/v1"}}}, false},
		{"custom name not registered", Config{Provider: "mygw"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled=%v want %v", got, tc.want)
			}
		})
	}
}

func TestConfig_ApplyDefaults(t *testing.T) {
	c := Config{}.ApplyDefaults()
	if c.Provider != "local" {
		t.Errorf("provider=%q want local", c.Provider)
	}
	if c.MaxSteps != 16 {
		t.Errorf("max_steps=%d want 16", c.MaxSteps)
	}
	if c.Local.Ctx != 4096 || c.Local.GPULayers != 999 || c.Local.Template != "chatml" {
		t.Errorf("local defaults wrong: %+v", c.Local)
	}
	if c.Anthropic.Model != defaultAnthropicModel || c.Anthropic.APIKeyEnv != defaultAnthropicKeyEnv || c.Anthropic.BaseURL != defaultAnthropicBaseURL {
		t.Errorf("anthropic defaults wrong: %+v", c.Anthropic)
	}
	if c.OpenAI.Model != defaultOpenAIModel || c.OpenAI.APIKeyEnv != defaultOpenAIKeyEnv || c.OpenAI.BaseURL != defaultOpenAIBaseURL {
		t.Errorf("openai defaults wrong: %+v", c.OpenAI)
	}
	if c.Azure.APIVersion != defaultAzureAPIVersion || c.Azure.EndpointEnv != defaultAzureEndpointEnv || c.Azure.APIKeyEnv != defaultAzureKeyEnv {
		t.Errorf("azure defaults wrong: %+v", c.Azure)
	}
	if c.Ollama.Host != defaultOllamaHost {
		t.Errorf("ollama host=%q want %q", c.Ollama.Host, defaultOllamaHost)
	}
	if c.ClaudeCLI.Binary != defaultClaudeCLIBinary {
		t.Errorf("claudecli binary=%q want %q", c.ClaudeCLI.Binary, defaultClaudeCLIBinary)
	}
	if c.Codex.Binary != defaultCodexBinary {
		t.Errorf("codex binary=%q want %q", c.Codex.Binary, defaultCodexBinary)
	}
	if c.Copilot.Binary != defaultCopilotBinary || c.Cursor.Binary != defaultCursorBinary || c.Opencode.Binary != defaultOpencodeBinary {
		t.Errorf("cli provider binary defaults wrong: copilot=%q cursor=%q opencode=%q", c.Copilot.Binary, c.Cursor.Binary, c.Opencode.Binary)
	}
	if c.Gemini.Model != defaultGeminiModel || c.Gemini.APIKeyEnv != defaultGeminiKeyEnv || c.Gemini.BaseURL != defaultGeminiBaseURL {
		t.Errorf("gemini defaults wrong: %+v", c.Gemini)
	}
	if c.Bedrock.Region != defaultBedrockRegion || c.Bedrock.AccessKeyEnv != defaultBedrockAccessKeyEnv || c.Bedrock.SecretKeyEnv != defaultBedrockSecretKeyEnv || c.Bedrock.SessionTokenEnv != defaultBedrockSessionTokenEnv {
		t.Errorf("bedrock defaults wrong: %+v", c.Bedrock)
	}
	if c.DeepSeek.Model != defaultDeepSeekModel || c.DeepSeek.APIKeyEnv != defaultDeepSeekKeyEnv || c.DeepSeek.BaseURL != defaultDeepSeekBaseURL {
		t.Errorf("deepseek defaults wrong: %+v", c.DeepSeek)
	}
	if c.Requesty.Model != defaultRequestyModel || c.Requesty.APIKeyEnv != defaultRequestyKeyEnv || c.Requesty.BaseURL != defaultRequestyBaseURL {
		t.Errorf("requesty defaults wrong: %+v", c.Requesty)
	}
}

func TestConfig_MergeEnv_GeminiModel(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "gemini")
	t.Setenv("GORTEX_LLM_MODEL", "gemini-2.5-flash")
	c := Config{}.MergeEnv()
	if c.Gemini.Model != "gemini-2.5-flash" {
		t.Errorf("gemini model=%q — GORTEX_LLM_MODEL should target the active provider", c.Gemini.Model)
	}
}

func TestConfig_MergeEnv_BedrockModelAndRegion(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "bedrock")
	t.Setenv("GORTEX_LLM_MODEL", "anthropic.claude-opus-4-20250514-v1:0")
	t.Setenv("GORTEX_LLM_BEDROCK_REGION", "eu-west-1")
	c := Config{}.MergeEnv()
	if c.Bedrock.ModelID != "anthropic.claude-opus-4-20250514-v1:0" {
		t.Errorf("bedrock model_id=%q", c.Bedrock.ModelID)
	}
	if c.Bedrock.Region != "eu-west-1" {
		t.Errorf("bedrock region=%q want eu-west-1", c.Bedrock.Region)
	}
}

func TestConfig_MergeEnv_DeepSeekModel(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "deepseek")
	t.Setenv("GORTEX_LLM_MODEL", "deepseek-reasoner")
	c := Config{}.MergeEnv()
	if c.DeepSeek.Model != "deepseek-reasoner" {
		t.Errorf("deepseek model=%q", c.DeepSeek.Model)
	}
}

func TestConfig_MergeEnv_RequestyModelAndEffort(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "requesty")
	t.Setenv("GORTEX_LLM_MODEL", "anthropic/claude-sonnet-4-5")
	t.Setenv("GORTEX_LLM_EFFORT", "high")
	c := Config{}.MergeEnv()
	if c.Requesty.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("requesty model=%q", c.Requesty.Model)
	}
	if c.Requesty.Effort != "high" {
		t.Errorf("requesty effort=%q want high", c.Requesty.Effort)
	}
}

func TestConfig_CustomProvider_ActiveModelAndWithModel(t *testing.T) {
	base := Config{
		Provider: "mygw",
		Custom: map[string]CustomProvider{
			"mygw":  {BaseURL: "https://x/v1", Model: "base-model"},
			"other": {BaseURL: "https://y/v1", Model: "other-model"},
		},
	}
	if got := base.ActiveModel(); got != "base-model" {
		t.Errorf("ActiveModel=%q want base-model", got)
	}
	routed := base.WithModel("routed-model")
	if got := routed.ActiveModel(); got != "routed-model" {
		t.Errorf("WithModel/ActiveModel round-trip failed: %q", got)
	}
	// Copy-on-write: the base config and other providers are untouched.
	if base.Custom["mygw"].Model != "base-model" {
		t.Errorf("WithModel mutated the base config's custom map: %q", base.Custom["mygw"].Model)
	}
	if routed.Custom["other"].Model != "other-model" {
		t.Errorf("WithModel must not disturb other custom providers: %q", routed.Custom["other"].Model)
	}
}

func TestConfig_MergedWith_CustomProviders(t *testing.T) {
	global := Config{
		Provider: "mygw",
		Custom: map[string]CustomProvider{
			"mygw":   {BaseURL: "https://global/v1", Model: "global-model"},
			"shared": {BaseURL: "https://global/v1", Model: "global-shared"},
		},
	}
	local := Config{
		Custom: map[string]CustomProvider{
			"shared": {BaseURL: "https://local/v1", Model: "local-shared"},
			"local":  {BaseURL: "https://local/v1", Model: "local-only"},
		},
	}
	got := local.MergedWith(global)
	if got.Custom["mygw"].Model != "global-model" {
		t.Error("global-only custom provider should be filled in")
	}
	if got.Custom["local"].Model != "local-only" {
		t.Error("local-only custom provider should survive")
	}
	if got.Custom["shared"].Model != "local-shared" {
		t.Errorf("local must win for a name in both, got %q", got.Custom["shared"].Model)
	}
}

func TestConfig_Anthropic_CachingThinking_EnvAndMerge(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "anthropic")
	t.Setenv("GORTEX_LLM_ANTHROPIC_PROMPT_CACHING", "1")
	t.Setenv("GORTEX_LLM_ANTHROPIC_THINKING_MODE", "auto")
	c := Config{}.MergeEnv()
	if !c.Anthropic.PromptCaching {
		t.Error("GORTEX_LLM_ANTHROPIC_PROMPT_CACHING should enable caching")
	}
	if c.Anthropic.ThinkingMode != "auto" {
		t.Errorf("thinking_mode=%q want auto", c.Anthropic.ThinkingMode)
	}

	// MergedWith fills the Anthropic-only knobs (and the embedded model)
	// from global; local wins where set.
	global := Config{
		Provider: "anthropic",
		Anthropic: AnthropicConfig{
			RemoteConfig:  RemoteConfig{Model: "claude-sonnet", APIKeyEnv: "ANTHROPIC_API_KEY"},
			PromptCaching: true,
			CacheTTL:      "1h",
			ThinkingMode:  "manual",
		},
	}
	local := Config{Anthropic: AnthropicConfig{ThinkingMode: "adaptive"}}
	got := local.MergedWith(global)
	if got.Anthropic.Model != "claude-sonnet" {
		t.Errorf("embedded model not merged: %q", got.Anthropic.Model)
	}
	if !got.Anthropic.PromptCaching || got.Anthropic.CacheTTL != "1h" {
		t.Errorf("caching knobs not merged: %+v", got.Anthropic)
	}
	if got.Anthropic.ThinkingMode != "adaptive" {
		t.Errorf("thinking_mode=%q want adaptive (local wins)", got.Anthropic.ThinkingMode)
	}
}

func TestConfig_Effort_EnvAndMerge(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "anthropic")
	t.Setenv("GORTEX_LLM_EFFORT", "high")
	c := Config{}.MergeEnv()
	if c.Anthropic.Effort != "high" {
		t.Errorf("GORTEX_LLM_EFFORT should target the active provider, got anthropic.effort=%q", c.Anthropic.Effort)
	}

	global := Config{Provider: "openai", OpenAI: RemoteConfig{Effort: "medium"}}
	local := Config{}
	if got := local.MergedWith(global); got.OpenAI.Effort != "medium" {
		t.Errorf("effort should merge from global, got %q", got.OpenAI.Effort)
	}
}

func TestConfig_IsBuiltinProvider(t *testing.T) {
	for _, name := range []string{"openai", "azure", "copilot", " Anthropic ", "BEDROCK"} {
		if !IsBuiltinProvider(name) {
			t.Errorf("IsBuiltinProvider(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"mygw", "groq", ""} {
		if IsBuiltinProvider(name) {
			t.Errorf("IsBuiltinProvider(%q) = true, want false", name)
		}
	}
}

func TestConfig_MergeEnv_AzureDeploymentEndpointVersion(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "azure")
	t.Setenv("GORTEX_LLM_MODEL", "gpt4o-routed")
	t.Setenv("GORTEX_LLM_AZURE_ENDPOINT", "https://r.openai.azure.com")
	t.Setenv("GORTEX_LLM_AZURE_DEPLOYMENT", "gpt4o-deploy")
	t.Setenv("GORTEX_LLM_AZURE_API_VERSION", "2025-01-01-preview")
	c := Config{}.MergeEnv()
	// GORTEX_LLM_MODEL targets the active provider — for azure that's
	// the deployment (the routing key). The dedicated
	// GORTEX_LLM_AZURE_DEPLOYMENT then overrides it.
	if c.Azure.Deployment != "gpt4o-deploy" {
		t.Errorf("azure deployment=%q want gpt4o-deploy", c.Azure.Deployment)
	}
	if c.Azure.Endpoint != "https://r.openai.azure.com" {
		t.Errorf("azure endpoint=%q", c.Azure.Endpoint)
	}
	if c.Azure.APIVersion != "2025-01-01-preview" {
		t.Errorf("azure api_version=%q", c.Azure.APIVersion)
	}
}

func TestConfig_MergeEnv_CLIProviderBinaryAndModel(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "copilot")
	t.Setenv("GORTEX_LLM_MODEL", "claude-opus-4.1")
	t.Setenv("GORTEX_LLM_COPILOT_BINARY", "/opt/gh/copilot")
	t.Setenv("GORTEX_LLM_CURSOR_BINARY", "/opt/cursor/cursor-agent")
	t.Setenv("GORTEX_LLM_OPENCODE_BINARY", "/opt/oc/opencode")
	c := Config{}.MergeEnv()
	if c.Copilot.Model != "claude-opus-4.1" {
		t.Errorf("copilot model=%q — GORTEX_LLM_MODEL should target the active provider", c.Copilot.Model)
	}
	if c.Copilot.Binary != "/opt/gh/copilot" {
		t.Errorf("copilot binary=%q", c.Copilot.Binary)
	}
	if c.Cursor.Binary != "/opt/cursor/cursor-agent" {
		t.Errorf("cursor binary=%q", c.Cursor.Binary)
	}
	if c.Opencode.Binary != "/opt/oc/opencode" {
		t.Errorf("opencode binary=%q", c.Opencode.Binary)
	}
}

func TestConfig_MergedWith_CLIProviders(t *testing.T) {
	global := Config{
		Provider: "opencode",
		Opencode: CLIConfig{Binary: "/usr/local/bin/opencode", Args: []string{"--mode", "ask"}, TimeoutSeconds: 90},
	}
	local := Config{Opencode: CLIConfig{Model: "anthropic/claude-sonnet-4-6"}}
	got := local.MergedWith(global)
	if got.Opencode.Binary != "/usr/local/bin/opencode" {
		t.Errorf("binary=%q — global should fill", got.Opencode.Binary)
	}
	if got.Opencode.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("model=%q — local should win", got.Opencode.Model)
	}
	if len(got.Opencode.Args) != 2 || got.Opencode.TimeoutSeconds != 90 {
		t.Errorf("args/timeout not filled from global: %+v", got.Opencode)
	}
}

func TestConfig_MergedWith_Azure(t *testing.T) {
	global := Config{
		Provider: "azure",
		Azure:    AzureConfig{Endpoint: "https://r.openai.azure.com", APIVersion: "2024-10-21", APIKeyEnv: "AZURE_OPENAI_API_KEY"},
	}
	local := Config{Azure: AzureConfig{Deployment: "gpt4o"}}
	got := local.MergedWith(global)
	if got.Provider != "azure" {
		t.Errorf("provider=%q want azure", got.Provider)
	}
	if got.Azure.Deployment != "gpt4o" {
		t.Errorf("deployment=%q want gpt4o (local should win)", got.Azure.Deployment)
	}
	if got.Azure.Endpoint != "https://r.openai.azure.com" || got.Azure.APIVersion != "2024-10-21" {
		t.Errorf("azure block not filled from global: %+v", got.Azure)
	}
}

func TestConfig_MergedWith_NewProviders(t *testing.T) {
	global := Config{
		Provider: "bedrock",
		Bedrock:  BedrockConfig{ModelID: "anthropic.claude-sonnet-4-20250514-v1:0", Region: "us-east-1"},
		Gemini:   RemoteConfig{APIKeyEnv: "GEMINI_API_KEY", Model: "gemini-2.5-pro"},
		DeepSeek: RemoteConfig{APIKeyEnv: "DEEPSEEK_API_KEY"},
		Requesty: RemoteConfig{APIKeyEnv: "REQUESTY_API_KEY"},
	}
	local := Config{Bedrock: BedrockConfig{Region: "eu-west-1"}}
	got := local.MergedWith(global)
	if got.Bedrock.Region != "eu-west-1" {
		t.Errorf("bedrock region=%q want eu-west-1 (local should win)", got.Bedrock.Region)
	}
	if got.Bedrock.ModelID != "anthropic.claude-sonnet-4-20250514-v1:0" {
		t.Errorf("bedrock model_id=%q — global should fill", got.Bedrock.ModelID)
	}
	if got.Gemini.Model != "gemini-2.5-pro" {
		t.Errorf("gemini model=%q — global should fill", got.Gemini.Model)
	}
	if got.DeepSeek.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Errorf("deepseek api_key_env=%q — global should fill", got.DeepSeek.APIKeyEnv)
	}
	if got.Requesty.APIKeyEnv != "REQUESTY_API_KEY" {
		t.Errorf("requesty api_key_env=%q, global should fill", got.Requesty.APIKeyEnv)
	}
}

func TestConfig_MergeEnv_ClaudeCLIModel(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "claudecli")
	t.Setenv("GORTEX_LLM_MODEL", "opus")
	t.Setenv("GORTEX_LLM_CLAUDECLI_BINARY", "/opt/anthropic/claude")
	c := Config{}.MergeEnv()
	if c.ClaudeCLI.Model != "opus" {
		t.Errorf("claudecli model=%q want opus", c.ClaudeCLI.Model)
	}
	if c.ClaudeCLI.Binary != "/opt/anthropic/claude" {
		t.Errorf("claudecli binary=%q want /opt/anthropic/claude", c.ClaudeCLI.Binary)
	}
}

func TestConfig_MergeEnv_CodexModel(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "codex")
	t.Setenv("GORTEX_LLM_MODEL", "o4-mini")
	t.Setenv("GORTEX_LLM_CODEX_BINARY", "/opt/openai/codex")
	c := Config{}.MergeEnv()
	if c.Codex.Model != "o4-mini" {
		t.Errorf("codex model=%q want o4-mini", c.Codex.Model)
	}
	if c.Codex.Binary != "/opt/openai/codex" {
		t.Errorf("codex binary=%q want /opt/openai/codex", c.Codex.Binary)
	}
}

func TestConfig_MergedWith_Codex(t *testing.T) {
	global := Config{
		Provider: "codex",
		Codex:    CodexConfig{Binary: "/usr/local/bin/codex", Args: []string{"--sandbox", "read-only"}, TimeoutSeconds: 90},
	}
	local := Config{Codex: CodexConfig{Model: "gpt-5-codex"}}
	got := local.MergedWith(global)
	if got.Provider != "codex" {
		t.Errorf("provider=%q want codex", got.Provider)
	}
	if got.Codex.Binary != "/usr/local/bin/codex" {
		t.Errorf("binary=%q — global should fill", got.Codex.Binary)
	}
	if got.Codex.Model != "gpt-5-codex" {
		t.Errorf("model=%q want gpt-5-codex (local should win)", got.Codex.Model)
	}
	if len(got.Codex.Args) != 2 {
		t.Errorf("args=%v — global should fill when local is empty", got.Codex.Args)
	}
	if got.Codex.TimeoutSeconds != 90 {
		t.Errorf("timeout=%d — global should fill", got.Codex.TimeoutSeconds)
	}
}

func TestConfig_ActiveModelAndWithModel(t *testing.T) {
	cases := []struct {
		provider string
		cfg      Config
	}{
		{"anthropic", Config{Provider: "anthropic"}},
		{"openai", Config{Provider: "openai"}},
		{"azure", Config{Provider: "azure"}},
		{"ollama", Config{Provider: "ollama"}},
		{"claudecli", Config{Provider: "claudecli"}},
		{"codex", Config{Provider: "codex"}},
		{"copilot", Config{Provider: "copilot"}},
		{"cursor", Config{Provider: "cursor"}},
		{"opencode", Config{Provider: "opencode"}},
		{"gemini", Config{Provider: "gemini"}},
		{"bedrock", Config{Provider: "bedrock"}},
		{"deepseek", Config{Provider: "deepseek"}},
		{"requesty", Config{Provider: "requesty"}},
		{"local", Config{Provider: "local"}},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			got := c.cfg.WithModel("routed-model-x")
			if got.ActiveModel() != "routed-model-x" {
				t.Errorf("WithModel/ActiveModel round-trip failed for %s: %q", c.provider, got.ActiveModel())
			}
		})
	}
}

func TestConfig_WithModel_EmptyIsNoOp(t *testing.T) {
	c := Config{Provider: "anthropic", Anthropic: AnthropicConfig{RemoteConfig: RemoteConfig{Model: "claude-sonnet-4-6"}}}
	if got := c.WithModel("").ActiveModel(); got != "claude-sonnet-4-6" {
		t.Errorf("WithModel(\"\") must be a no-op, got %q", got)
	}
}

func TestConfig_WithModel_DoesNotTouchOtherProviders(t *testing.T) {
	c := Config{
		Provider:  "anthropic",
		Anthropic: AnthropicConfig{RemoteConfig: RemoteConfig{Model: "claude-sonnet-4-6"}},
		OpenAI:    RemoteConfig{Model: "gpt-4o"},
	}
	got := c.WithModel("claude-haiku-4-5")
	if got.OpenAI.Model != "gpt-4o" {
		t.Errorf("WithModel must not disturb the openai sub-block, got %q", got.OpenAI.Model)
	}
	if got.Anthropic.Model != "claude-haiku-4-5" {
		t.Errorf("WithModel = %q, want claude-haiku-4-5", got.Anthropic.Model)
	}
}

func TestConfig_MergedWith_Routing(t *testing.T) {
	global := Config{
		Provider: "anthropic",
		Routing:  RoutingConfig{Enabled: true, SimpleModel: "claude-haiku-4-5", ComplexModel: "claude-opus-4-7"},
	}
	local := Config{Routing: RoutingConfig{ComplexModel: "claude-opus-4-custom"}}
	got := local.MergedWith(global)
	if !got.Routing.Enabled {
		t.Error("routing enabled should be filled from global")
	}
	if got.Routing.SimpleModel != "claude-haiku-4-5" {
		t.Errorf("simple_model=%q — global should fill", got.Routing.SimpleModel)
	}
	if got.Routing.ComplexModel != "claude-opus-4-custom" {
		t.Errorf("complex_model=%q — local should win", got.Routing.ComplexModel)
	}
}

func TestConfig_MergedWith_ClaudeCLI(t *testing.T) {
	global := Config{
		Provider:  "claudecli",
		ClaudeCLI: ClaudeCLIConfig{Binary: "/usr/local/bin/claude", Args: []string{"--allowed-tools", ""}, TimeoutSeconds: 60},
	}
	local := Config{ClaudeCLI: ClaudeCLIConfig{Model: "sonnet"}}
	got := local.MergedWith(global)
	if got.Provider != "claudecli" {
		t.Errorf("provider=%q want claudecli", got.Provider)
	}
	if got.ClaudeCLI.Binary != "/usr/local/bin/claude" {
		t.Errorf("binary=%q — global should fill", got.ClaudeCLI.Binary)
	}
	if got.ClaudeCLI.Model != "sonnet" {
		t.Errorf("model=%q want sonnet (local should win)", got.ClaudeCLI.Model)
	}
	if len(got.ClaudeCLI.Args) != 2 {
		t.Errorf("args=%v — global should fill when local is empty", got.ClaudeCLI.Args)
	}
	if got.ClaudeCLI.TimeoutSeconds != 60 {
		t.Errorf("timeout=%d — global should fill", got.ClaudeCLI.TimeoutSeconds)
	}
}

func TestConfig_ApplyDefaults_Idempotent(t *testing.T) {
	once := Config{Provider: "anthropic", Anthropic: AnthropicConfig{RemoteConfig: RemoteConfig{Model: "m"}}}.ApplyDefaults()
	twice := once.ApplyDefaults()
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("ApplyDefaults not idempotent:\n once=%+v\n twice=%+v", once, twice)
	}
}

func TestConfig_MergeEnv(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "anthropic")
	t.Setenv("GORTEX_LLM_MODEL", "claude-opus-4-7")
	t.Setenv("GORTEX_LLM_MAX_STEPS", "8")
	c := Config{}.MergeEnv()
	if c.Provider != "anthropic" {
		t.Errorf("provider=%q want anthropic", c.Provider)
	}
	if c.Anthropic.Model != "claude-opus-4-7" {
		t.Errorf("anthropic model=%q — GORTEX_LLM_MODEL should target the active provider", c.Anthropic.Model)
	}
	if c.MaxSteps != 8 {
		t.Errorf("max_steps=%d want 8", c.MaxSteps)
	}
}

func TestConfig_MergeEnv_ModelTargetsLocalByDefault(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "")
	t.Setenv("GORTEX_LLM_MODEL", "/local/m.gguf")
	c := Config{}.MergeEnv()
	if c.Local.Model != "/local/m.gguf" {
		t.Errorf("local model=%q want /local/m.gguf", c.Local.Model)
	}
}

func TestConfig_MergedWith(t *testing.T) {
	global := Config{
		Provider:  "local",
		MaxSteps:  16,
		Local:     LocalConfig{Model: "/g.gguf", Template: "chatml", Ctx: 4096},
		Anthropic: AnthropicConfig{RemoteConfig: RemoteConfig{APIKeyEnv: "ANTHROPIC_API_KEY"}},
	}
	local := Config{Local: LocalConfig{Model: "/repo.gguf"}} // overrides only the model

	got := local.MergedWith(global)
	if got.Provider != "local" {
		t.Errorf("provider=%q — global should fill", got.Provider)
	}
	if got.Local.Model != "/repo.gguf" {
		t.Errorf("local model=%q — repo should win", got.Local.Model)
	}
	if got.Local.Template != "chatml" || got.Local.Ctx != 4096 {
		t.Errorf("local sub-fields not filled from global: %+v", got.Local)
	}
	if got.Anthropic.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic block not merged: %+v", got.Anthropic)
	}
	if got.MaxSteps != 16 {
		t.Errorf("max_steps=%d — global should fill", got.MaxSteps)
	}
}
