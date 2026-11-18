package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// isolate points the global providers.json at a temp config dir so the
// registry never touches the developer's real ~/.gortex.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(LocalOptInEnv, "")
}

func TestAdd_List_Get_Remove(t *testing.T) {
	isolate(t)

	if err := Add("groq", llm.CustomProvider{BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.3-70b", APIKeyEnv: "GROQ_API_KEY"}); err != nil {
		t.Fatal(err)
	}
	if err := Add("local-vllm", llm.CustomProvider{BaseURL: "http://localhost:8000/v1", Model: "qwen"}); err != nil {
		t.Fatal(err)
	}

	entries, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// List is sorted by name.
	if entries[0].Name != "groq" || entries[1].Name != "local-vllm" {
		t.Errorf("entries not sorted: %v", entries)
	}

	cp, ok, err := Get("groq")
	if err != nil || !ok {
		t.Fatalf("Get groq: ok=%v err=%v", ok, err)
	}
	if cp.Model != "llama-3.3-70b" {
		t.Errorf("model=%q", cp.Model)
	}

	removed, err := Remove("groq")
	if err != nil || !removed {
		t.Fatalf("Remove groq: removed=%v err=%v", removed, err)
	}
	if _, ok, _ := Get("groq"); ok {
		t.Error("groq should be gone after remove")
	}
	if removed, _ := Remove("groq"); removed {
		t.Error("removing an absent provider should report removed=false")
	}
}

func TestAdd_RejectsBuiltinShadow(t *testing.T) {
	isolate(t)
	if err := Add("openai", llm.CustomProvider{BaseURL: "https://x/v1", Model: "m"}); err == nil {
		t.Fatal("expected an error when shadowing a built-in provider")
	}
}

func TestAdd_RejectsBadSchemeAndMissingFields(t *testing.T) {
	isolate(t)
	cases := map[string]llm.CustomProvider{
		"bad scheme":  {BaseURL: "ftp://x/v1", Model: "m"},
		"no base_url": {Model: "m"},
		"no model":    {BaseURL: "https://x/v1"},
		"bad schema":  {BaseURL: "https://x/v1", Model: "m", SchemaMode: "wat"},
	}
	for name, cp := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Add("custom", cp); err == nil {
				t.Errorf("expected an error for %s", name)
			}
		})
	}
}

func TestLoad_SkipsInvalidEntriesWithWarning(t *testing.T) {
	isolate(t)
	// Write a providers.json by hand with one valid and one shadowing
	// (invalid) entry.
	dir := filepath.Dir(GlobalPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
      "good": {"base_url": "https://api.example.com/v1", "model": "m"},
      "openai": {"base_url": "https://evil/v1", "model": "m"}
    }`
	if err := os.WriteFile(GlobalPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	providers, warnings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := providers["good"]; !ok {
		t.Error("valid entry should load")
	}
	if _, ok := providers["openai"]; ok {
		t.Error("an entry shadowing a built-in must be skipped")
	}
	if len(warnings) == 0 {
		t.Error("expected a warning for the skipped entry")
	}
}

func TestAugment_FileFillsConfig_InlineWins(t *testing.T) {
	isolate(t)
	if err := Add("fromfile", llm.CustomProvider{BaseURL: "https://file/v1", Model: "file-model"}); err != nil {
		t.Fatal(err)
	}
	if err := Add("shared", llm.CustomProvider{BaseURL: "https://file/v1", Model: "file-shared"}); err != nil {
		t.Fatal(err)
	}
	cfg := llm.Config{Custom: map[string]llm.CustomProvider{
		"shared": {BaseURL: "https://inline/v1", Model: "inline-shared"},
		"inline": {BaseURL: "https://inline/v1", Model: "inline-only"},
	}}
	got, _ := Augment(cfg)
	if got.Custom["fromfile"].Model != "file-model" {
		t.Error("file-only provider should be merged in")
	}
	if got.Custom["inline"].Model != "inline-only" {
		t.Error("inline-only provider should survive")
	}
	if got.Custom["shared"].Model != "inline-shared" {
		t.Errorf("inline must win for a name in both, got %q", got.Custom["shared"].Model)
	}
}

func TestLoad_LocalOptIn(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	t.Chdir(dir)
	localDir := filepath.Join(dir, ".gortex")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"repolocal": {"base_url": "https://repo/v1", "model": "m"}}`
	if err := os.WriteFile(filepath.Join(localDir, "providers.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without the opt-in env, the repo-local file is ignored.
	providers, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := providers["repolocal"]; ok {
		t.Error("repo-local providers must not load without the opt-in env")
	}

	// With the opt-in env set, it loads.
	t.Setenv(LocalOptInEnv, "1")
	providers, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := providers["repolocal"]; !ok {
		t.Error("repo-local provider should load with GORTEX_ALLOW_LOCAL_PROVIDERS=1")
	}
}

func TestEstimateCost(t *testing.T) {
	cp := llm.CustomProvider{Pricing: llm.ProviderPricing{Input: 3.0, Output: 15.0}}
	// 1M input + 1M output = $3 + $15 = $18.
	if got := EstimateCost(cp, 1_000_000, 1_000_000); got != 18.0 {
		t.Errorf("EstimateCost=%v want 18", got)
	}
	if got := EstimateCost(llm.CustomProvider{}, 1_000_000, 1_000_000); got != 0 {
		t.Errorf("zero pricing should yield zero cost, got %v", got)
	}
}

func TestLoad_PlaintextHTTPWarning(t *testing.T) {
	isolate(t)
	if err := Add("insecure", llm.CustomProvider{BaseURL: "http://remote.example.com/v1", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Error("expected a cleartext-http warning for a non-loopback host")
	}
}

func TestAdd_PromptProfile(t *testing.T) {
	isolate(t)
	if err := Add("gw", llm.CustomProvider{BaseURL: "https://x/v1", Model: "m", PromptProfile: "huge"}); err == nil {
		t.Error("an unknown prompt_profile must be rejected")
	}
	if err := Add("gw", llm.CustomProvider{BaseURL: "https://x/v1", Model: "m", PromptProfile: "frontier"}); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := Get("gw")
	if err != nil || !ok || cp.PromptProfile != "frontier" {
		t.Errorf("Get gw: %+v ok=%v err=%v, want prompt_profile frontier", cp, ok, err)
	}
}

// TestAdd_PresetNameAllowedAndWins pins that a preset name is not
// reserved: the user may register "requesty", and the merged entry
// replaces the gateway preset.
func TestAdd_PresetNameAllowedAndWins(t *testing.T) {
	isolate(t)
	if err := Add("requesty", llm.CustomProvider{BaseURL: "https://proxy.internal/v1", Model: "my-model"}); err != nil {
		t.Fatalf("a preset name must be registrable: %v", err)
	}
	cfg, _ := Augment(llm.Config{Provider: "requesty"}.MergeEnv())
	cp, ok := cfg.ActiveCustom()
	if !ok || cp.BaseURL != "https://proxy.internal/v1" || cp.Model != "my-model" {
		t.Errorf("providers.json entry must win over the preset: %+v ok=%v", cp, ok)
	}
}
