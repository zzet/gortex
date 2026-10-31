// Package registry manages the user's custom OpenAI-compatible LLM
// providers — the on-disk providers.json that `gortex provider
// add/list/show/remove` edits, and the merge that hands those entries
// to llm.Config so they dispatch like any built-in provider.
//
// Two locations are consulted, mirroring the safety posture of the rest
// of the toolchain:
//
//   - the global file at <ConfigDir>/providers.json (always trusted);
//   - a repo-local .gortex/providers.json, loaded ONLY when
//     GORTEX_ALLOW_LOCAL_PROVIDERS=1, so a cloned repo cannot silently
//     point gortex's LLM calls at an attacker-controlled endpoint.
//
// Entries are validated on load: the base_url must be http/https and
// the name must not shadow a built-in provider. Invalid entries are
// skipped with a warning rather than failing the whole load, so one bad
// row never disables the agent.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/platform"
)

// LocalOptInEnv gates loading the repo-local providers.json. Loading an
// endpoint definition from a cloned repo is a supply-chain risk, so it
// is off unless the user explicitly opts in.
const LocalOptInEnv = "GORTEX_ALLOW_LOCAL_PROVIDERS"

// GlobalPath is the trusted, user-global providers.json next to the
// rest of the gortex config.
func GlobalPath() string {
	return filepath.Join(platform.ConfigDir(), "providers.json")
}

// LocalPath is the opt-in, repo-local providers.json relative to the
// current working directory.
func LocalPath() string {
	return filepath.Join(".gortex", "providers.json")
}

// Load reads the global providers.json (and the repo-local one when
// GORTEX_ALLOW_LOCAL_PROVIDERS=1), validates each entry, and returns
// the merged map keyed by provider name. A repo-local entry overrides a
// global one of the same name. warnings carries human-readable reasons
// for any skipped entry; a missing file is not an error.
func Load() (providers map[string]llm.CustomProvider, warnings []string, err error) {
	providers = map[string]llm.CustomProvider{}

	g, gw, err := loadFile(GlobalPath())
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, gw...)
	for name, cp := range g {
		providers[name] = cp
	}

	if optedInLocal() {
		l, lw, err := loadFile(LocalPath())
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, lw...)
		for name, cp := range l {
			providers[name] = cp // repo-local overrides global
		}
	}
	return providers, warnings, nil
}

// Augment merges the registry's custom providers into cfg.Custom. An
// inline `llm.custom:` entry from the config file wins over a
// providers.json entry of the same name. The returned warnings describe
// any skipped on-disk entries.
func Augment(cfg llm.Config) (llm.Config, []string) {
	loaded, warnings, err := Load()
	if err != nil {
		return cfg, []string{fmt.Sprintf("custom providers: %v", err)}
	}
	if len(loaded) == 0 {
		return cfg, warnings
	}
	merged := make(map[string]llm.CustomProvider, len(loaded)+len(cfg.Custom))
	for name, cp := range loaded {
		merged[name] = cp
	}
	for name, cp := range cfg.Custom { // inline config wins
		merged[name] = cp
	}
	cfg.Custom = merged
	return cfg, warnings
}

// List returns the entries from the global providers.json, sorted by
// name. The repo-local file is intentionally excluded — `provider list`
// reports the trusted set the user manages.
func List() ([]Entry, error) {
	m, _, err := loadFile(GlobalPath())
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(m))
	for name, cp := range m {
		out = append(out, Entry{Name: name, Provider: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one entry from the global providers.json.
func Get(name string) (llm.CustomProvider, bool, error) {
	m, _, err := loadFile(GlobalPath())
	if err != nil {
		return llm.CustomProvider{}, false, err
	}
	cp, ok := m[strings.TrimSpace(name)]
	return cp, ok, nil
}

// Add validates and writes a provider into the global providers.json,
// creating the file (and its directory) if needed. An existing entry of
// the same name is replaced.
func Add(name string, cp llm.CustomProvider) error {
	name = strings.TrimSpace(name)
	if err := validate(name, cp); err != nil {
		return err
	}
	m, _, err := loadFileRaw(GlobalPath())
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]llm.CustomProvider{}
	}
	m[name] = cp
	return save(GlobalPath(), m)
}

// Remove deletes a provider from the global providers.json. It is not
// an error to remove an absent entry; removed reports whether one was
// actually present.
func Remove(name string) (removed bool, err error) {
	name = strings.TrimSpace(name)
	m, _, err := loadFileRaw(GlobalPath())
	if err != nil {
		return false, err
	}
	if _, ok := m[name]; !ok {
		return false, nil
	}
	delete(m, name)
	return true, save(GlobalPath(), m)
}

// EstimateCost returns the USD cost of a call against a custom
// provider's optional pricing (USD per 1M tokens). Zero pricing yields
// zero — pricing is purely informational.
func EstimateCost(cp llm.CustomProvider, inputTokens, outputTokens int64) float64 {
	return (float64(inputTokens)*cp.Pricing.Input + float64(outputTokens)*cp.Pricing.Output) / 1_000_000.0
}

// Entry pairs a provider name with its definition for List.
type Entry struct {
	Name     string
	Provider llm.CustomProvider
}

func optedInLocal() bool {
	v := strings.TrimSpace(os.Getenv(LocalOptInEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// loadFile reads and validates a providers.json, skipping invalid
// entries with a warning. A missing file yields an empty map.
func loadFile(path string) (map[string]llm.CustomProvider, []string, error) {
	raw, _, err := loadFileRaw(path)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]llm.CustomProvider, len(raw))
	var warnings []string
	for name, cp := range raw {
		if err := validate(name, cp); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: skipped provider %q: %v", path, name, err))
			continue
		}
		if w := plaintextWarning(name, cp); w != "" {
			warnings = append(warnings, w)
		}
		out[name] = cp
	}
	return out, warnings, nil
}

// loadFileRaw reads a providers.json without validation (used by the
// mutating CLI paths, which validate explicitly). A missing file yields
// a nil map and no error.
func loadFileRaw(path string) (map[string]llm.CustomProvider, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, true, nil
	}
	var m map[string]llm.CustomProvider
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, true, nil
}

func save(path string, m map[string]llm.CustomProvider) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode providers: %w", err)
	}
	data = append(data, '\n')
	// Write atomically with a random-named temp file so a predictable ".tmp"
	// name cannot be pre-created (or symlinked) by a co-tenant in the
	// directory to redirect this write. os.CreateTemp opens the file 0600.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	return nil
}

// validate enforces the invariants every custom provider must satisfy:
// a non-empty name that does not shadow a built-in, an http/https
// base_url, a model, and a recognised schema_mode and prompt_profile.
func validate(name string, cp llm.CustomProvider) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is empty")
	}
	if llm.IsBuiltinProvider(name) {
		return fmt.Errorf("name shadows the built-in provider %q", strings.ToLower(name))
	}
	base := strings.TrimSpace(cp.BaseURL)
	if base == "" {
		return fmt.Errorf("base_url is empty")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("base_url must be http(s), got %q", base)
	}
	if strings.TrimSpace(cp.Model) == "" {
		return fmt.Errorf("model is empty")
	}
	switch strings.ToLower(strings.TrimSpace(cp.SchemaMode)) {
	case "", "json_schema", "json_object", "prompt", "prompt_only", "none":
	default:
		return fmt.Errorf("unknown schema_mode %q (want json_schema|json_object|prompt)", cp.SchemaMode)
	}
	if _, err := llm.ParsePromptProfile(cp.PromptProfile); err != nil {
		return err
	}
	return nil
}

// plaintextWarning flags a cleartext http endpoint pointed at a
// non-loopback host — allowed, but worth a heads-up since the key would
// travel unencrypted.
func plaintextWarning(name string, cp llm.CustomProvider) string {
	base := strings.TrimSpace(cp.BaseURL)
	if !strings.HasPrefix(base, "http://") {
		return ""
	}
	rest := strings.TrimPrefix(base, "http://")
	host := rest
	if i := strings.IndexAny(rest, ":/"); i >= 0 {
		host = rest[:i]
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return ""
	}
	return fmt.Sprintf("custom provider %q uses cleartext http to non-loopback host %q — the API key travels unencrypted", name, host)
}
