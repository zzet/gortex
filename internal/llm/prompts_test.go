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
		"requesty":  ProfileFrontier,
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
