package llm

import "testing"

func TestProfileForProvider(t *testing.T) {
	cases := map[string]PromptProfile{
		"anthropic": ProfileFrontier,
		"openai":    ProfileFrontier,
		"claudecli": ProfileFrontier,
		"gemini":    ProfileFrontier,
		"bedrock":   ProfileFrontier,
		"deepseek":  ProfileFrontier,
		"local":     ProfileSmall,
		"ollama":    ProfileSmall,
		"":          ProfileSmall,
		"unknown":   ProfileSmall,
	}
	for name, want := range cases {
		if got := ProfileForProvider(name); got != want {
			t.Errorf("ProfileForProvider(%q)=%v want %v", name, got, want)
		}
	}
}

func TestSystemPrompts_DifferByTier(t *testing.T) {
	if ExpandSystemPrompt(ProfileSmall) == ExpandSystemPrompt(ProfileFrontier) {
		t.Error("expand prompt identical across tiers")
	}
	if RerankSystemPrompt(ProfileSmall) == RerankSystemPrompt(ProfileFrontier) {
		t.Error("rerank prompt identical across tiers")
	}
	if VerifySystemPrompt(ProfileSmall) == VerifySystemPrompt(ProfileFrontier) {
		t.Error("verify prompt identical across tiers")
	}
	for _, s := range []string{
		ExpandSystemPrompt(ProfileSmall), ExpandSystemPrompt(ProfileFrontier),
		RerankSystemPrompt(ProfileSmall), RerankSystemPrompt(ProfileFrontier),
		VerifySystemPrompt(ProfileSmall), VerifySystemPrompt(ProfileFrontier),
	} {
		if s == "" {
			t.Error("empty system prompt")
		}
	}
}

func TestJSONSchemaFor_Freeform(t *testing.T) {
	if JSONSchemaFor(ShapeFreeform, nil) != nil {
		t.Error("freeform shape must have no schema")
	}
}

func TestJSONSchemaFor_ListShapes(t *testing.T) {
	cases := map[JSONShape]string{
		ShapeExpandTerms: "terms",
		ShapeRerankOrder: "order",
		ShapeVerifyKeep:  "keep",
	}
	for shape, key := range cases {
		s := JSONSchemaFor(shape, nil)
		if s == nil {
			t.Fatalf("shape %d: nil schema", shape)
		}
		if s["type"] != "object" {
			t.Errorf("shape %d: type=%v want object", shape, s["type"])
		}
		props, ok := s["properties"].(map[string]any)
		if !ok {
			t.Fatalf("shape %d: properties not a map: %v", shape, s["properties"])
		}
		if _, ok := props[key]; !ok {
			t.Errorf("shape %d: missing property %q (have %v)", shape, key, props)
		}
	}
}

func TestJSONSchemaFor_ToolCallEnumeratesNames(t *testing.T) {
	s := JSONSchemaFor(ShapeToolCall, []ToolSpec{{Name: "search_symbols"}, {Name: "get_callers"}})
	props := s["properties"].(map[string]any)
	tool := props["tool"].(map[string]any)
	enum, ok := tool["enum"].([]any)
	if !ok || len(enum) != 2 {
		t.Fatalf("tool enum=%v", tool["enum"])
	}
	if enum[0] != "search_symbols" || enum[1] != "get_callers" {
		t.Errorf("tool enum order=%v", enum)
	}
	if _, ok := props["args"]; !ok {
		t.Error("tool-call schema missing args property")
	}
}

func TestProfileFor_CustomProviderPromptProfile(t *testing.T) {
	custom := func(profile string) Config {
		return Config{Provider: "mygw", Custom: map[string]CustomProvider{
			"mygw": {BaseURL: "https://x/v1", Model: "m", PromptProfile: profile},
		}}
	}
	cases := map[string]struct {
		cfg  Config
		want PromptProfile
	}{
		"custom frontier":         {custom("frontier"), ProfileFrontier},
		"custom frontier mixed":   {custom(" Frontier "), ProfileFrontier},
		"custom small":            {custom("small"), ProfileSmall},
		"custom default is small": {custom(""), ProfileSmall},
		"custom invalid is small": {custom("huge"), ProfileSmall},
		"builtin anthropic":       {Config{Provider: "anthropic"}, ProfileFrontier},
		"builtin local":           {Config{}, ProfileSmall},
		"requesty preset":         {Config{Provider: "requesty"}, ProfileFrontier},
		"unregistered name":       {Config{Provider: "nope"}, ProfileSmall},
	}
	for name, tc := range cases {
		if got := ProfileFor(tc.cfg); got != tc.want {
			t.Errorf("%s: ProfileFor=%v want %v", name, got, tc.want)
		}
	}
}

func TestParsePromptProfile(t *testing.T) {
	for _, ok := range []string{"", "small", "frontier", "FRONTIER"} {
		if _, err := ParsePromptProfile(ok); err != nil {
			t.Errorf("ParsePromptProfile(%q) err=%v", ok, err)
		}
	}
	if _, err := ParsePromptProfile("large"); err == nil {
		t.Error("ParsePromptProfile(large) should fail")
	}
}
