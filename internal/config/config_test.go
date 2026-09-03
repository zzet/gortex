package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"
)

// Feature: gortex-enhancements, Property 3: Guard rule config round-trip

// genGuardRule generates a random GuardRule with valid field values.
func genGuardRule() *rapid.Generator[GuardRule] {
	return rapid.Custom(func(t *rapid.T) GuardRule {
		kind := rapid.SampledFrom([]string{"co-change", "boundary"}).Draw(t, "kind")
		return GuardRule{
			Name:    rapid.StringMatching(`[a-z][a-z0-9\-]{0,29}`).Draw(t, "name"),
			Kind:    kind,
			Source:  rapid.StringMatching(`[a-z][a-z0-9/]{0,49}`).Draw(t, "source"),
			Target:  rapid.StringMatching(`[a-z][a-z0-9/]{0,49}`).Draw(t, "target"),
			Message: rapid.StringMatching(`[A-Za-z0-9 .,!?]{1,100}`).Draw(t, "message"),
		}
	})
}

// genGuardsConfig generates a random GuardsConfig with 0-10 rules.
func genGuardsConfig() *rapid.Generator[GuardsConfig] {
	return rapid.Custom(func(t *rapid.T) GuardsConfig {
		n := rapid.IntRange(0, 10).Draw(t, "numRules")
		rules := make([]GuardRule, n)
		for i := range n {
			rules[i] = genGuardRule().Draw(t, "rule")
		}
		return GuardsConfig{Rules: rules}
	})
}

// yamlConfig is a helper struct for writing YAML that viper can load.
// We use explicit yaml tags to ensure the keys match what viper/mapstructure expects.
type yamlConfig struct {
	Guards yamlGuardsConfig `yaml:"guards"`
}

type yamlGuardsConfig struct {
	Rules []yamlGuardRule `yaml:"rules"`
}

type yamlGuardRule struct {
	Name     string   `yaml:"name"`
	Kind     string   `yaml:"kind"`
	Source   string   `yaml:"source"`
	Target   string   `yaml:"target"`
	Message  string   `yaml:"message"`
	Severity string   `yaml:"severity"`
	Except   []string `yaml:"except"`
}

func toYAMLConfig(gc GuardsConfig) yamlConfig {
	rules := make([]yamlGuardRule, len(gc.Rules))
	for i, r := range gc.Rules {
		rules[i] = yamlGuardRule(r)
	}
	return yamlConfig{Guards: yamlGuardsConfig{Rules: rules}}
}

func TestGlobalConfigRemoveRepoIfPresentIsIdempotent(t *testing.T) {
	repoPath := t.TempDir()
	global := &GlobalConfig{Repos: []RepoEntry{{Path: repoPath}}}

	removed, err := global.RemoveRepoIfPresent(repoPath)
	if err != nil || !removed || len(global.Repos) != 0 {
		t.Fatalf("first removal: removed=%v repos=%d err=%v", removed, len(global.Repos), err)
	}
	removed, err = global.RemoveRepoIfPresent(repoPath)
	if err != nil || removed || len(global.Repos) != 0 {
		t.Fatalf("replayed removal: removed=%v repos=%d err=%v", removed, len(global.Repos), err)
	}
	if err := global.RemoveRepo(repoPath); err == nil {
		t.Fatal("legacy RemoveRepo on an absent repository returned nil")
	}
}

func TestGlobalConfigRemoveRepoIfPresentMatchesFilesystemIdentity(t *testing.T) {
	realPath := t.TempDir()
	aliasPath := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	global := &GlobalConfig{Repos: []RepoEntry{{Path: aliasPath}}}

	removed, err := global.RemoveRepoIfPresent(realPath)
	if err != nil || !removed || len(global.Repos) != 0 {
		t.Fatalf("filesystem-identity removal: removed=%v repos=%d err=%v",
			removed, len(global.Repos), err)
	}
}

func TestPropertyGuardConfigRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		original := genGuardsConfig().Draw(rt, "guardsConfig")

		// Serialize to YAML
		yc := toYAMLConfig(original)
		data, err := yaml.Marshal(yc)
		if err != nil {
			rt.Fatalf("failed to marshal YAML: %v", err)
		}

		// Write to a temp file named .gortex.yaml so viper can find it
		dir := t.TempDir()
		configPath := filepath.Join(dir, ".gortex.yaml")
		if err := os.WriteFile(configPath, data, 0644); err != nil {
			rt.Fatalf("failed to write config file: %v", err)
		}

		// Load via config.Load
		loaded, err := Load(configPath)
		if err != nil {
			rt.Fatalf("failed to load config: %v", err)
		}

		// Assert the loaded GuardsConfig matches the original
		if len(loaded.Guards.Rules) != len(original.Rules) {
			rt.Fatalf("rule count mismatch: got %d, want %d",
				len(loaded.Guards.Rules), len(original.Rules))
		}

		for i, want := range original.Rules {
			got := loaded.Guards.Rules[i]
			if got.Name != want.Name {
				rt.Errorf("rule[%d].Name: got %q, want %q", i, got.Name, want.Name)
			}
			if got.Kind != want.Kind {
				rt.Errorf("rule[%d].Kind: got %q, want %q", i, got.Kind, want.Kind)
			}
			if got.Source != want.Source {
				rt.Errorf("rule[%d].Source: got %q, want %q", i, got.Source, want.Source)
			}
			if got.Target != want.Target {
				rt.Errorf("rule[%d].Target: got %q, want %q", i, got.Target, want.Target)
			}
			if got.Message != want.Message {
				rt.Errorf("rule[%d].Message: got %q, want %q", i, got.Message, want.Message)
			}
		}
	})
}

// Feature: multi-repo-support, Property 17: Backward-compatible config loading

// yamlLegacyConfig is a helper struct representing a .gortex.yaml file
// that only contains existing (pre-multi-repo) fields — no repos, projects,
// workspace, or active_project sections.
type yamlLegacyConfig struct {
	Index  *yamlLegacyIndex  `yaml:"index,omitempty"`
	Watch  *yamlLegacyWatch  `yaml:"watch,omitempty"`
	Guards *yamlGuardsConfig `yaml:"guards,omitempty"`
}

type yamlLegacyIndex struct {
	Exclude []string `yaml:"exclude,omitempty"`
	Workers int      `yaml:"workers,omitempty"`
}

type yamlLegacyWatch struct {
	Enabled    bool     `yaml:"enabled"`
	DebounceMs int      `yaml:"debounce_ms,omitempty"`
	Exclude    []string `yaml:"exclude,omitempty"`
}

// genLegacyConfig generates a random .gortex.yaml content using only
// existing fields (no repos/projects/workspace/active_project).
func genLegacyConfig() *rapid.Generator[yamlLegacyConfig] {
	return rapid.Custom(func(t *rapid.T) yamlLegacyConfig {
		var cfg yamlLegacyConfig

		// Optionally include index section.
		if rapid.Bool().Draw(t, "hasIndex") {
			idx := &yamlLegacyIndex{}
			if rapid.Bool().Draw(t, "hasExclude") {
				idx.Exclude = genExcludePatterns().Draw(t, "indexExclude")
			}
			if rapid.Bool().Draw(t, "hasWorkers") {
				idx.Workers = rapid.IntRange(1, 32).Draw(t, "workers")
			}
			cfg.Index = idx
		}

		// Optionally include watch section.
		if rapid.Bool().Draw(t, "hasWatch") {
			w := &yamlLegacyWatch{}
			w.Enabled = rapid.Bool().Draw(t, "watchEnabled")
			if rapid.Bool().Draw(t, "hasDebounce") {
				w.DebounceMs = rapid.IntRange(50, 1000).Draw(t, "debounceMs")
			}
			if rapid.Bool().Draw(t, "hasWatchExclude") {
				w.Exclude = genExcludePatterns().Draw(t, "watchExclude")
			}
			cfg.Watch = w
		}

		// Optionally include guards section.
		if rapid.Bool().Draw(t, "hasGuards") {
			gc := genGuardsConfig().Draw(t, "guards")
			yg := &yamlGuardsConfig{Rules: make([]yamlGuardRule, len(gc.Rules))}
			for i, r := range gc.Rules {
				yg.Rules[i] = yamlGuardRule(r)
			}
			cfg.Guards = yg
		}

		return cfg
	})
}

// TestPropertyBackwardCompatibleConfigLoading verifies that existing .gortex.yaml files
// without repos/projects/workspace/active_project sections load successfully via config.Load()
// and return a valid Config with defaults for any new fields.
func TestPropertyBackwardCompatibleConfigLoading(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		legacy := genLegacyConfig().Draw(rt, "legacyConfig")

		// Serialize to YAML.
		data, err := yaml.Marshal(legacy)
		if err != nil {
			rt.Fatalf("failed to marshal legacy config: %v", err)
		}

		// Write to a temp .gortex.yaml file.
		dir := t.TempDir()
		configPath := filepath.Join(dir, ".gortex.yaml")
		if err := os.WriteFile(configPath, data, 0644); err != nil {
			rt.Fatalf("failed to write config file: %v", err)
		}

		// Load via config.Load — this must succeed without error.
		loaded, err := Load(configPath)
		if err != nil {
			rt.Fatalf("config.Load() failed on legacy config: %v\nYAML content:\n%s", err, string(data))
		}

		// Verify loaded config is non-nil.
		if loaded == nil {
			rt.Fatal("config.Load() returned nil Config")
		}

		// Verify existing fields match generated values.
		if legacy.Index != nil {
			if legacy.Index.Exclude != nil {
				if len(loaded.Index.Exclude) != len(legacy.Index.Exclude) {
					rt.Errorf("index.exclude count: got %d, want %d",
						len(loaded.Index.Exclude), len(legacy.Index.Exclude))
				}
				for i, want := range legacy.Index.Exclude {
					if i < len(loaded.Index.Exclude) && loaded.Index.Exclude[i] != want {
						rt.Errorf("index.exclude[%d]: got %q, want %q",
							i, loaded.Index.Exclude[i], want)
					}
				}
			}
			if legacy.Index.Workers > 0 {
				if loaded.Index.Workers != legacy.Index.Workers {
					rt.Errorf("index.workers: got %d, want %d",
						loaded.Index.Workers, legacy.Index.Workers)
				}
			}
		}

		if legacy.Watch != nil {
			if loaded.Watch.Enabled != legacy.Watch.Enabled {
				rt.Errorf("watch.enabled: got %v, want %v",
					loaded.Watch.Enabled, legacy.Watch.Enabled)
			}
			if legacy.Watch.DebounceMs > 0 {
				if loaded.Watch.DebounceMs != legacy.Watch.DebounceMs {
					rt.Errorf("watch.debounce_ms: got %d, want %d",
						loaded.Watch.DebounceMs, legacy.Watch.DebounceMs)
				}
			}
			if legacy.Watch.Exclude != nil {
				if len(loaded.Watch.Exclude) != len(legacy.Watch.Exclude) {
					rt.Errorf("watch.exclude count: got %d, want %d",
						len(loaded.Watch.Exclude), len(legacy.Watch.Exclude))
				}
				for i, want := range legacy.Watch.Exclude {
					if i < len(loaded.Watch.Exclude) && loaded.Watch.Exclude[i] != want {
						rt.Errorf("watch.exclude[%d]: got %q, want %q",
							i, loaded.Watch.Exclude[i], want)
					}
				}
			}
		}

		if legacy.Guards != nil {
			if len(loaded.Guards.Rules) != len(legacy.Guards.Rules) {
				rt.Errorf("guards.rules count: got %d, want %d",
					len(loaded.Guards.Rules), len(legacy.Guards.Rules))
			}
			for i, want := range legacy.Guards.Rules {
				if i < len(loaded.Guards.Rules) {
					got := loaded.Guards.Rules[i]
					if got.Name != want.Name {
						rt.Errorf("guards.rules[%d].name: got %q, want %q", i, got.Name, want.Name)
					}
					if got.Kind != want.Kind {
						rt.Errorf("guards.rules[%d].kind: got %q, want %q", i, got.Kind, want.Kind)
					}
					if got.Source != want.Source {
						rt.Errorf("guards.rules[%d].source: got %q, want %q", i, got.Source, want.Source)
					}
					if got.Target != want.Target {
						rt.Errorf("guards.rules[%d].target: got %q, want %q", i, got.Target, want.Target)
					}
					if got.Message != want.Message {
						rt.Errorf("guards.rules[%d].message: got %q, want %q", i, got.Message, want.Message)
					}
				}
			}
		}

		// Verify that the YAML content does NOT contain multi-repo fields.
		// This is a sanity check on the generator — the generated YAML should
		// never include repos, projects, workspace, or active_project.
		content := string(data)
		for _, forbidden := range []string{"repos:", "projects:", "workspace:", "active_project:"} {
			if containsField(content, forbidden) {
				rt.Errorf("generated YAML unexpectedly contains %q", forbidden)
			}
		}
	})
}

// containsField checks if a YAML string contains a top-level field key.
func containsField(yamlContent, field string) bool {
	// Simple check: the field appears at the start of a line.
	for _, line := range splitLines(yamlContent) {
		if len(line) >= len(field) && line[:len(field)] == field {
			return true
		}
	}
	return false
}

func TestPubsubCoverageDomain(t *testing.T) {
	// Default-off: like observability, the pub/sub extractor is a
	// heuristic with a non-zero false-positive surface, so it stays
	// off until a repo opts in.
	var cov CoverageConfig
	if cov.IsEnabled("pubsub") {
		t.Errorf("pubsub coverage domain should default to off")
	}

	// Explicit enable in YAML flips it on through the tri-state pointer.
	enabled := true
	cov.Pubsub.Enabled = &enabled
	if !cov.IsEnabled("pubsub") {
		t.Errorf("pubsub coverage domain should be on when explicitly enabled")
	}

	// Explicit disable is distinguishable from unset.
	disabled := false
	cov.Pubsub.Enabled = &disabled
	if cov.IsEnabled("pubsub") {
		t.Errorf("pubsub coverage domain should be off when explicitly disabled")
	}
}

// splitLines splits a string into lines.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
