// Package provider is the llm.Provider factory. It is the one place
// that imports every concrete provider implementation; everything else
// (the svc layer, the agent loop, the cmd demos) depends only on the
// llm.Provider interface and calls New here.
//
// The build-tag split is contained entirely within the `local`
// subpackage: provider.New compiles in every build, and selecting
// "local" without a `-tags llama` binary surfaces as a runtime error
// from local.New, not a compile failure.
package provider

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/provider/anthropic"
	"github.com/zzet/gortex/internal/llm/provider/azure"
	"github.com/zzet/gortex/internal/llm/provider/bedrock"
	"github.com/zzet/gortex/internal/llm/provider/claudecli"
	"github.com/zzet/gortex/internal/llm/provider/codex"
	"github.com/zzet/gortex/internal/llm/provider/copilot"
	"github.com/zzet/gortex/internal/llm/provider/cursor"
	"github.com/zzet/gortex/internal/llm/provider/deepseek"
	"github.com/zzet/gortex/internal/llm/provider/gemini"
	"github.com/zzet/gortex/internal/llm/provider/local"
	"github.com/zzet/gortex/internal/llm/provider/ollama"
	"github.com/zzet/gortex/internal/llm/provider/openai"
	"github.com/zzet/gortex/internal/llm/provider/openaicompat"
	"github.com/zzet/gortex/internal/llm/provider/opencode"
	"github.com/zzet/gortex/internal/llm/provider/requesty"
)

// New builds the llm.Provider selected by cfg.Provider. cfg should
// already have defaults applied (see llm.Config.ApplyDefaults) — the
// HTTP providers rely on the defaulted model / endpoint / key-env
// values. Returns an error when the provider is unknown or
// misconfigured (missing model, unset API key) or, for "local", when
// the binary was built without `-tags llama`; for "claudecli" /
// "codex", when the `claude` / `codex` binary is not on $PATH.
func New(cfg llm.Config) (llm.Provider, error) {
	switch cfg.ProviderName() {
	case "local":
		return local.New(cfg.Local)
	case "anthropic":
		ac := cfg.Anthropic
		return anthropic.New(ac.RemoteConfig,
			anthropic.WithPromptCaching(ac.PromptCaching, ac.CacheTTL),
			anthropic.WithThinking(ac.ThinkingMode, ac.ThinkingBudgetTokens, ac.ThinkingDisplay),
		)
	case "openai":
		return openai.New(cfg.OpenAI)
	case "azure":
		return azure.New(cfg.Azure)
	case "ollama":
		return ollama.New(cfg.Ollama)
	case "claudecli":
		return claudecli.New(cfg.ClaudeCLI)
	case "codex":
		return codex.New(cfg.Codex)
	case "copilot":
		return copilot.New(cfg.Copilot)
	case "cursor":
		return cursor.New(cfg.Cursor)
	case "opencode":
		return opencode.New(cfg.Opencode)
	case "gemini":
		return gemini.New(cfg.Gemini)
	case "bedrock":
		return bedrock.New(cfg.Bedrock)
	case "deepseek":
		return deepseek.New(cfg.DeepSeek)
	case "requesty":
		return requesty.New(cfg.Requesty)
	default:
		if cp, ok := cfg.Custom[cfg.ProviderName()]; ok {
			return newCustom(cfg.ProviderName(), cp)
		}
		return nil, fmt.Errorf("llm: unknown provider %q (want local|anthropic|openai|azure|ollama|claudecli|codex|copilot|cursor|opencode|gemini|bedrock|deepseek|requesty, or a registered custom provider)", cfg.ProviderName())
	}
}

// newCustom builds a registered OpenAI-compatible custom provider from
// its CustomProvider entry. The endpoint is BaseURL + "/chat/completions";
// the bearer key (when APIKeyEnv is set) and any extra headers are
// applied to every request; the structured-output strategy comes from
// SchemaMode. Name() reports "custom" (frontier prompt tier) while the
// httpx/error tag carries the specific "custom:<name>".
func newCustom(name string, cp llm.CustomProvider) (llm.Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cp.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("llm: custom provider %q has no base_url", name)
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("llm: custom provider %q base_url must be http(s), got %q", name, base)
	}
	if strings.TrimSpace(cp.Model) == "" {
		return nil, fmt.Errorf("llm: custom provider %q has no model", name)
	}

	headers := make(map[string]string, len(cp.Headers)+1)
	for k, v := range cp.Headers {
		headers[k] = v
	}
	if env := strings.TrimSpace(cp.APIKeyEnv); env != "" {
		key := strings.TrimSpace(os.Getenv(env))
		if key == "" {
			return nil, fmt.Errorf("llm: custom provider %q API key env %q is not set", name, env)
		}
		headers["authorization"] = "Bearer " + key
	}

	var mode openaicompat.SchemaMode
	switch strings.ToLower(strings.TrimSpace(cp.SchemaMode)) {
	case "", "json_schema":
		mode = openaicompat.SchemaJSONSchema
	case "json_object":
		mode = openaicompat.SchemaJSONObject
	case "prompt", "prompt_only", "none":
		mode = openaicompat.SchemaPromptOnly
	default:
		return nil, fmt.Errorf("llm: custom provider %q unknown schema_mode %q (want json_schema|json_object|prompt)", name, cp.SchemaMode)
	}

	return &openaicompat.Client{
		ProviderID:      "custom",
		Tag:             "custom:" + name,
		Model:           cp.Model,
		URL:             base + "/chat/completions",
		Headers:         headers,
		HTTPClient:      &http.Client{Timeout: 120 * time.Second},
		SchemaMode:      mode,
		MaxTokensField:  cp.MaxTokensField,
		Temperature:     cp.Temperature,
		ReasoningEffort: cp.ReasoningEffort,
	}, nil
}
