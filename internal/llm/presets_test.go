package llm

import "testing"

// withPreset installs a gateway preset for the duration of a test.
func withPreset(t *testing.T, name string, cp CustomProvider) {
	t.Helper()
	gatewayPresets[name] = cp
	t.Cleanup(func() { delete(gatewayPresets, name) })
}

func TestPreset_ReturnsCopy(t *testing.T) {
	withPreset(t, "testgw", CustomProvider{BaseURL: "https://preset/v1", Model: "m", Headers: map[string]string{"x": "1"}})
	cp, ok := Preset(" TestGW ")
	if !ok {
		t.Fatal("preset lookup should be case-insensitive")
	}
	cp.Headers["x"] = "mutated"
	if again, _ := Preset("testgw"); again.Headers["x"] != "1" {
		t.Error("Preset must hand out a copy of the headers map")
	}
	if _, ok := Preset("nope"); ok {
		t.Error("unknown preset must not resolve")
	}
}

func TestPresetNames_SortedAndNotReserved(t *testing.T) {
	names := PresetNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("PresetNames not sorted: %v", names)
		}
	}
	for _, name := range names {
		if _, ok := Preset(name); !ok {
			t.Errorf("listed preset %q does not resolve", name)
		}
	}
}

// TestConfig_ActiveCustom_PresetResolution pins the resolution order for
// llm.provider: a true built-in, then the user's custom provider, then
// a gateway preset.
func TestConfig_ActiveCustom_PresetResolution(t *testing.T) {
	preset := CustomProvider{BaseURL: "https://preset/v1", Model: "preset-model", APIKeyEnv: "PRESET_KEY", PromptProfile: "frontier"}
	withPreset(t, "testgw", preset)
	withPreset(t, "openai", preset) // a built-in name must never resolve to a preset

	// Preset only.
	c := Config{Provider: "testgw"}
	cp, ok := c.ActiveCustom()
	if !ok || cp.BaseURL != "https://preset/v1" || cp.Model != "preset-model" {
		t.Fatalf("preset not resolved: %+v ok=%v", cp, ok)
	}
	if !c.IsEnabled() || c.ActiveModel() != "preset-model" || ProfileFor(c) != ProfileFrontier {
		t.Errorf("preset config: enabled=%v model=%q profile=%v", c.IsEnabled(), c.ActiveModel(), ProfileFor(c))
	}

	// A shipped preset resolves with no setup.
	ci := Config{Provider: "cheaperinference"}
	cp, ok = ci.ActiveCustom()
	if !ok || cp.BaseURL != "https://api.cheaperinference.com/v1" || cp.Model != "gpt-5.4-mini" || cp.APIKeyEnv != "CHEAPER_INFERENCE_API_KEY" || ProfileFor(ci) != ProfileFrontier {
		t.Errorf("cheaperinference preset: %+v ok=%v profile=%v", cp, ok, ProfileFor(ci))
	}

	route := Config{Provider: "api-route"}
	cp, ok = route.ActiveCustom()
	if !ok || cp.BaseURL != "https://global.api-route.com/v1" || cp.Model != "gpt-6.1-sol" || cp.APIKeyEnv != "API_ROUTE_API_KEY" || cp.SchemaMode != "json_schema" || ProfileFor(route) != ProfileFrontier {
		t.Errorf("api-route preset: %+v ok=%v profile=%v", cp, ok, ProfileFor(route))
	}

	buzz := Config{Provider: "llm-buzz"}
	cp, ok = buzz.ActiveCustom()
	if !ok || cp.BaseURL != "https://api.llm.buzz/api/v1" || cp.Model != "openai/gpt-5.4-mini" || cp.APIKeyEnv != "LLM_BUZZ_API_KEY" || cp.SchemaMode != "json_schema" || ProfileFor(buzz) != ProfileFrontier {
		t.Errorf("llm-buzz preset: %+v ok=%v profile=%v", cp, ok, ProfileFor(buzz))
	}

	opper := Config{Provider: "opper"}
	cp, ok = opper.ActiveCustom()
	if !ok || cp.BaseURL != "https://api.opper.ai/v3/compat" || cp.Model != "gpt-6.1-terra" || cp.APIKeyEnv != "OPPER_API_KEY" || cp.SchemaMode != "json_schema" || ProfileFor(opper) != ProfileFrontier {
		t.Errorf("opper preset: %+v ok=%v profile=%v", cp, ok, ProfileFor(opper))
	}

	rc := Config{Provider: "runcrate"}
	cp, ok = rc.ActiveCustom()
	if !ok || cp.BaseURL != "https://api.runcrate.ai/v1" || cp.Model != "deepseek-ai/DeepSeek-V4.1-Flash" || cp.APIKeyEnv != "RUNCRATE_API_KEY" || cp.SchemaMode != "json_schema" || ProfileFor(rc) != ProfileFrontier {
		t.Errorf("runcrate preset: %+v ok=%v profile=%v", cp, ok, ProfileFor(rc))
	}

	// A user custom provider of the same name always wins, wholesale —
	// no preset field leaks into it.
	c.Custom = map[string]CustomProvider{"testgw": {BaseURL: "https://mine/v1", Model: "my-model"}}
	cp, _ = c.ActiveCustom()
	if cp.BaseURL != "https://mine/v1" || cp.Model != "my-model" || cp.APIKeyEnv != "" || cp.PromptProfile != "" {
		t.Errorf("user entry must win over the preset: %+v", cp)
	}
	if ProfileFor(c) != ProfileSmall {
		t.Error("user entry without prompt_profile must not inherit the preset's tier")
	}

	// Env / routing overrides apply on top of a preset.
	t.Setenv("GORTEX_LLM_PROVIDER", "testgw")
	t.Setenv("GORTEX_LLM_MODEL", "env-model")
	t.Setenv("GORTEX_LLM_EFFORT", "high")
	c = Config{}.MergeEnv()
	cp, _ = c.ActiveCustom()
	if cp.Model != "env-model" || cp.ReasoningEffort != "high" || cp.BaseURL != "https://preset/v1" {
		t.Errorf("env overrides over preset: %+v", cp)
	}
	if got := c.WithModel("routed").ActiveModel(); got != "routed" {
		t.Errorf("WithModel over preset = %q want routed", got)
	}

	// A built-in name never resolves to a preset.
	if _, ok := (Config{Provider: "openai"}).ActiveCustom(); ok {
		t.Error("built-in provider must not resolve to a preset")
	}
}

// TestConfig_RequestyLegacyBlockEquivalence pins that configs written
// for the former requesty built-in resolve to the same endpoint, model,
// key env, effort, schema mode and prompt tier through the preset.
func TestConfig_RequestyLegacyBlockEquivalence(t *testing.T) {
	cases := []struct {
		name   string
		block  RemoteConfig
		url    string // base_url + "/chat/completions", as the built-in called it
		model  string
		keyEnv string
		effort string
	}{
		{"defaults", RemoteConfig{}, "https://router.requesty.ai/v1/chat/completions", "openai/gpt-4o-mini", "REQUESTY_API_KEY", ""},
		{"model and effort", RemoteConfig{Model: "anthropic/claude-sonnet-4-5", Effort: "high"}, "https://router.requesty.ai/v1/chat/completions", "anthropic/claude-sonnet-4-5", "REQUESTY_API_KEY", "high"},
		{"key env", RemoteConfig{APIKeyEnv: "MY_REQUESTY_KEY"}, "https://router.requesty.ai/v1/chat/completions", "openai/gpt-4o-mini", "MY_REQUESTY_KEY", ""},
		{"eu base with v1", RemoteConfig{BaseURL: "https://router.eu.requesty.ai/v1"}, "https://router.eu.requesty.ai/v1/chat/completions", "openai/gpt-4o-mini", "REQUESTY_API_KEY", ""},
		{"base without v1", RemoteConfig{BaseURL: "https://router.requesty.ai"}, "https://router.requesty.ai/v1/chat/completions", "openai/gpt-4o-mini", "REQUESTY_API_KEY", ""},
		{"base with v1 slash", RemoteConfig{BaseURL: "https://router.requesty.ai/v1/"}, "https://router.requesty.ai/v1/chat/completions", "openai/gpt-4o-mini", "REQUESTY_API_KEY", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Provider: "requesty", Requesty: tc.block}.ApplyDefaults()
			cp, ok := c.ActiveCustom()
			if !ok {
				t.Fatal("requesty did not resolve")
			}
			if got := cp.BaseURL + "/chat/completions"; got != tc.url {
				t.Errorf("url=%q want %q", got, tc.url)
			}
			if cp.Model != tc.model || cp.APIKeyEnv != tc.keyEnv || cp.ReasoningEffort != tc.effort {
				t.Errorf("model=%q key=%q effort=%q, want %q %q %q", cp.Model, cp.APIKeyEnv, cp.ReasoningEffort, tc.model, tc.keyEnv, tc.effort)
			}
			if cp.SchemaMode != "json_schema" || ProfileFor(c) != ProfileFrontier || !c.IsEnabled() {
				t.Errorf("schema=%q profile=%v enabled=%v", cp.SchemaMode, ProfileFor(c), c.IsEnabled())
			}
		})
	}
}

// TestConfig_RequestyUserCustomWins pins the safety property: a user
// custom provider named requesty replaces both the preset and the legacy
// block.
func TestConfig_RequestyUserCustomWins(t *testing.T) {
	c := Config{
		Provider: "requesty",
		Requesty: RemoteConfig{Model: "legacy-model", BaseURL: "https://legacy.example"},
		Custom:   map[string]CustomProvider{"requesty": {BaseURL: "https://mine.example/v1", Model: "my-model"}},
	}
	cp, ok := c.ActiveCustom()
	if !ok || cp.BaseURL != "https://mine.example/v1" || cp.Model != "my-model" || cp.APIKeyEnv != "" {
		t.Errorf("user entry must win over preset and legacy block: %+v", cp)
	}
}
