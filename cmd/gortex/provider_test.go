package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/registry"
)

// isolateProviders points the providers.json registry at a temp config
// dir so the tests never touch the developer's real ~/.gortex.
func isolateProviders(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(registry.LocalOptInEnv, "")
}

// runProviderAddWith runs `provider add <name>` with the given flags,
// resetting the package-level flag state afterwards.
func runProviderAddWith(t *testing.T, name string, flags map[string]string) (string, error) {
	t.Helper()
	fs := providerAddCmd.Flags()
	reset := func() {
		fs.VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() != "stringArray" {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		})
		providerHeaders = nil
	}
	reset()
	t.Cleanup(reset)
	for k, v := range flags {
		if err := fs.Set(k, v); err != nil {
			t.Fatalf("set --%s: %v", k, err)
		}
	}
	var out bytes.Buffer
	providerAddCmd.SetOut(&out)
	err := runProviderAdd(providerAddCmd, []string{name})
	return out.String(), err
}

func runProviderShowOut(t *testing.T, name string) string {
	t.Helper()
	var out bytes.Buffer
	providerShowCmd.SetOut(&out)
	if err := runProviderShow(providerShowCmd, []string{name}); err != nil {
		t.Fatalf("provider show %s: %v", name, err)
	}
	return out.String()
}

func runProviderListOut(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	providerListCmd.SetOut(&out)
	if err := runProviderList(providerListCmd, nil); err != nil {
		t.Fatalf("provider list: %v", err)
	}
	return out.String()
}

func TestProviderAdd_PromptProfile(t *testing.T) {
	isolateProviders(t)
	if _, err := runProviderAddWith(t, "gw", map[string]string{
		"base-url": "https://gw.example/v1", "model": "big-model", "prompt-profile": "frontier",
	}); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := registry.Get("gw")
	if err != nil || !ok || cp.PromptProfile != "frontier" {
		t.Fatalf("stored entry %+v ok=%v err=%v, want prompt_profile frontier", cp, ok, err)
	}
	if out := runProviderShowOut(t, "gw"); !strings.Contains(out, "prompt_profile:   frontier") {
		t.Errorf("show output missing prompt_profile:\n%s", out)
	}
	if out := runProviderListOut(t); !strings.Contains(out, "PROFILE") || !strings.Contains(out, "frontier") {
		t.Errorf("list output missing profile column:\n%s", out)
	}
}

func TestProviderAdd_PromptProfileDefaultAndInvalid(t *testing.T) {
	isolateProviders(t)
	if _, err := runProviderAddWith(t, "plain", map[string]string{
		"base-url": "https://gw.example/v1", "model": "m",
	}); err != nil {
		t.Fatal(err)
	}
	if out := runProviderShowOut(t, "plain"); !strings.Contains(out, "prompt_profile:   small (default)") {
		t.Errorf("show output should name the small default:\n%s", out)
	}
	if _, err := runProviderAddWith(t, "bad", map[string]string{
		"base-url": "https://gw.example/v1", "model": "m", "prompt-profile": "huge",
	}); err == nil {
		t.Error("an unknown --prompt-profile must be rejected")
	}
}

func TestProviderAdd_PresetCopyAndOverride(t *testing.T) {
	isolateProviders(t)
	if _, err := runProviderAddWith(t, "myreq", map[string]string{
		"preset": "requesty", "model": "anthropic/claude-sonnet-4-5", "header": "X-Title=gortex",
	}); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := registry.Get("myreq")
	if err != nil || !ok {
		t.Fatalf("Get myreq: ok=%v err=%v", ok, err)
	}
	preset, _ := llm.Preset("requesty")
	if cp.BaseURL != preset.BaseURL || cp.APIKeyEnv != preset.APIKeyEnv || cp.PromptProfile != preset.PromptProfile || cp.SchemaMode != preset.SchemaMode {
		t.Errorf("preset values not copied: %+v", cp)
	}
	if cp.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("--model must override the preset model, got %q", cp.Model)
	}
	if cp.Headers["X-Title"] != "gortex" {
		t.Errorf("--header not applied: %v", cp.Headers)
	}
	if p, _ := llm.Preset("requesty"); p.Model == cp.Model {
		t.Error("overriding a copied field must not mutate the preset table")
	}

	if _, err := runProviderAddWith(t, "x", map[string]string{"preset": "no-such-gateway"}); err == nil {
		t.Error("an unknown --preset must be rejected")
	}
}

func TestProviderListShow_Presets(t *testing.T) {
	isolateProviders(t)
	out := runProviderListOut(t)
	if !strings.Contains(out, "No custom providers registered") || !strings.Contains(out, "Gateway presets") || !strings.Contains(out, "requesty") {
		t.Errorf("list should show presets separately from (absent) user entries:\n%s", out)
	}
	out = runProviderShowOut(t, "requesty")
	if !strings.Contains(out, "source:           gateway preset") || !strings.Contains(out, "router.requesty.ai") {
		t.Errorf("show <preset> output:\n%s", out)
	}
}

// TestProviderAdd_PresetNameOverrides pins that a preset name is not
// reserved: `provider add requesty` registers a user entry, which list
// and show report as replacing the preset.
func TestProviderAdd_PresetNameOverrides(t *testing.T) {
	isolateProviders(t)
	if _, err := runProviderAddWith(t, "requesty", map[string]string{
		"preset": "requesty", "base-url": "https://proxy.internal/v1",
	}); err != nil {
		t.Fatalf("a preset name must be registrable: %v", err)
	}
	if out := runProviderListOut(t); !strings.Contains(out, "requesty (overridden)") {
		t.Errorf("list should mark the overridden preset:\n%s", out)
	}
	out := runProviderShowOut(t, "requesty")
	if !strings.Contains(out, "replaces the gateway preset") || !strings.Contains(out, "https://proxy.internal/v1") {
		t.Errorf("show should report the user entry:\n%s", out)
	}
}
