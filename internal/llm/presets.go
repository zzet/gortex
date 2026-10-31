// Package llm — OpenAI-compatible gateway presets.
//
// A gateway that speaks the OpenAI Chat Completions wire format with a
// Bearer key needs no code of its own: it is a CustomProvider with known
// defaults. Presets are those defaults, shipped in the binary, so
// `llm.provider: <preset>` works without a `gortex provider add` step.
//
// The rule for new providers: a new wire protocol or auth scheme is a
// built-in provider package; a new OpenAI-compatible gateway is one
// entry in gatewayPresets.
package llm

import (
	"maps"
	"sort"
	"strings"
)

// gatewayPresets maps a preset name to its CustomProvider defaults.
// Preset names are deliberately not reserved: a user custom provider of
// the same name (providers.json or inline `llm.custom`) always wins.
var gatewayPresets = map[string]CustomProvider{
	// API Route (https://www.api-route.com/docs/overview) uses the
	// OpenAI Chat Completions API with bare model ids.
	"api-route": {
		BaseURL:       "https://global.api-route.com/v1",
		Model:         "gpt-6.1-sol",
		APIKeyEnv:     "API_ROUTE_API_KEY",
		SchemaMode:    "json_schema",
		PromptProfile: "frontier",
	},
	// Cheaper Inference (https://cheaperinference.com/docs) is an
	// OpenAI-compatible gateway; model ids are bare (no vendor prefix).
	"cheaperinference": {
		BaseURL:       "https://api.cheaperinference.com/v1",
		Model:         "gpt-5.4-mini",
		APIKeyEnv:     "CHEAPER_INFERENCE_API_KEY",
		SchemaMode:    "json_schema",
		PromptProfile: "frontier",
	},
	// llm.buzz (https://llm.buzz) is an OpenAI-compatible gateway; model
	// ids carry a vendor prefix (list: GET /api/v1/models).
	"llm-buzz": {
		BaseURL:       "https://api.llm.buzz/api/v1",
		Model:         "openai/gpt-5.4-mini",
		APIKeyEnv:     "LLM_BUZZ_API_KEY",
		SchemaMode:    "json_schema",
		PromptProfile: "frontier",
	},
	// Opper (https://docs.opper.ai) is an EU-hosted OpenAI-compatible
	// gateway at /v3/compat (no /v1); model ids are bare pool names, and a
	// "provider/model" id such as "azure/gpt-5.5" pins one route.
	"opper": {
		BaseURL:       "https://api.opper.ai/v3/compat",
		Model:         "gpt-6.1-terra",
		APIKeyEnv:     "OPPER_API_KEY",
		SchemaMode:    "json_schema",
		PromptProfile: "frontier",
	},
	// Requesty (https://docs.requesty.ai) fronts many vendors behind one
	// key; model ids carry a vendor prefix. Regional routers:
	// https://router.eu.requesty.ai/v1 (and .us / .ap).
	"requesty": {
		BaseURL:       "https://router.requesty.ai/v1",
		Model:         "openai/gpt-4o-mini",
		APIKeyEnv:     "REQUESTY_API_KEY",
		SchemaMode:    "json_schema",
		PromptProfile: "frontier",
	},
	// Runcrate (https://www.runcrate.ai/inference/pricing) serves open
	// models over the OpenAI Chat Completions API; model ids carry a
	// vendor prefix.
	"runcrate": {
		BaseURL:       "https://api.runcrate.ai/v1",
		Model:         "deepseek-ai/DeepSeek-V4.1-Flash",
		APIKeyEnv:     "RUNCRATE_API_KEY",
		SchemaMode:    "json_schema",
		PromptProfile: "frontier",
	},
}

// Preset returns the named gateway preset (matched case-insensitively
// after trimming). The returned value is a copy the caller may modify.
func Preset(name string) (CustomProvider, bool) {
	cp, ok := gatewayPresets[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return CustomProvider{}, false
	}
	cp.Headers = maps.Clone(cp.Headers)
	return cp, true
}

// withLegacyPresetBlock overlays a preset's pre-preset config block —
// `llm.requesty:` from when requesty was a built-in — on the preset
// defaults, so existing configs resolve to exactly the endpoint, model,
// key and effort they did before. Set fields win over the preset.
func (c Config) withLegacyPresetBlock(name string, cp CustomProvider) CustomProvider {
	if name != "requesty" {
		return cp
	}
	r := c.Requesty
	if v := strings.TrimSpace(r.Model); v != "" {
		cp.Model = v
	}
	if v := strings.TrimSpace(r.APIKeyEnv); v != "" {
		cp.APIKeyEnv = v
	}
	if v := strings.TrimSpace(r.BaseURL); v != "" {
		// The built-in accepted the router URL with or without its "/v1"
		// segment and always called "<host>/v1/chat/completions".
		cp.BaseURL = strings.TrimSuffix(strings.TrimRight(v, "/"), "/v1") + "/v1"
	}
	if v := strings.TrimSpace(r.Effort); v != "" {
		cp.ReasoningEffort = v
	}
	return cp
}

// PresetNames returns the gateway preset names, sorted.
func PresetNames() []string {
	names := make([]string, 0, len(gatewayPresets))
	for name := range gatewayPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
