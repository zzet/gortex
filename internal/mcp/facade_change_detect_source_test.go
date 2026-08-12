package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChangeDetectSourceFailClosedBeforeActiveRepoFallback(t *testing.T) {
	spec := facadeOperationSpec{Facade: "change", Operation: "detect", Legacy: "detect_changes"}
	result := validateFacadeInput(spec, map[string]any{
		"source": map[string]any{
			"scope":     "unstaged",
			"repo_path": `C:\work\repo-b`,
		},
	})
	if result == nil || !result.IsError {
		t.Fatalf("validateFacadeInput() = %#v, want structured tool error", result)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, want := range []string{
		`\"error_code\":\"invalid_argument\"`,
		`source.repo_path`,
		`unsupported_field`,
		`options.repo`,
	} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("error payload %s does not contain %q", payload, want)
		}
	}
}

func TestChangeDetectSourceFailClosedForUnknownField(t *testing.T) {
	spec := facadeOperationSpec{Facade: "change", Operation: "detect", Legacy: "detect_changes"}
	result := validateFacadeInput(spec, map[string]any{
		"source": map[string]any{"paths": []string{"README.md"}},
	})
	if result == nil || !result.IsError {
		t.Fatalf("validateFacadeInput() = %#v, want structured tool error", result)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, want := range []string{`source.paths`, `\"allowed_fields\":[\"scope\"]`} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("error payload %s does not contain %q", payload, want)
		}
	}
	if strings.Contains(string(payload), `options.repo`) {
		t.Errorf("generic unknown field should not receive repo-specific guidance: %s", payload)
	}
}

func TestChangeDetectSourceAllowsOptionsRepo(t *testing.T) {
	spec := facadeOperationSpec{Facade: "change", Operation: "detect", Legacy: "detect_changes"}
	input := map[string]any{
		"source":  map[string]any{"scope": "compare"},
		"options": map[string]any{"repo": `C:\work\repo-b`, "base_ref": "upstream/main"},
	}
	if result := validateFacadeInput(spec, input); result != nil {
		t.Fatalf("validateFacadeInput() rejected supported selector: %#v", result)
	}
	normalized := normalizeFacadeArguments(spec, input)
	if got := normalized["repo"]; got != `C:\work\repo-b` {
		t.Fatalf("normalized repo = %#v, want explicit repo B path", got)
	}
	if got := normalized["scope"]; got != "compare" {
		t.Fatalf("normalized scope = %#v, want compare", got)
	}
	if got := normalized["base_ref"]; got != "upstream/main" {
		t.Fatalf("normalized base_ref = %#v, want upstream/main", got)
	}
	if _, leaked := normalized["repo_path"]; leaked {
		t.Fatalf("normalized arguments unexpectedly contain repo_path: %#v", normalized)
	}
}
