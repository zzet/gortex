package svc

import (
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestService_Provider exercises the accessor's nil-safety: a nil service
// and an unconfigured service both report no provider, so callers (the
// wiki enhancer) can gate on it without a panic.
func TestService_Provider(t *testing.T) {
	var nilSvc *Service
	if nilSvc.Provider() != nil {
		t.Error("nil service must return a nil provider")
	}

	s := NewService(llm.Config{}, llm.MockBackend{})
	if s.Provider() != nil {
		t.Error("a service with no configured provider must return nil")
	}
	if s.Enabled() {
		t.Error("a service with no provider must not be Enabled()")
	}
}

// TestService_CustomProviderPromptProfile pins that a custom provider's
// prompt_profile selects the assist prompt tier.
func TestService_CustomProviderPromptProfile(t *testing.T) {
	for profile, want := range map[string]llm.PromptProfile{"": llm.ProfileSmall, "frontier": llm.ProfileFrontier} {
		cfg := llm.Config{Provider: "gw", Custom: map[string]llm.CustomProvider{
			"gw": {BaseURL: "http://localhost:1/v1", Model: "m", PromptProfile: profile},
		}}
		s := NewService(cfg, llm.MockBackend{})
		if !s.Enabled() {
			t.Fatalf("profile %q: service not enabled: %v", profile, s.ProviderErr())
		}
		if s.profile != want {
			t.Errorf("profile %q: service tier=%v want %v", profile, s.profile, want)
		}
		_ = s.Close()
	}
}
