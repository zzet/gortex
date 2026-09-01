package review

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/platform"
)

// RuleResolver grounds each changed file to the review rule that governs
// it, across four configuration layers. The rule list is flattened and
// its globs precompiled once at construction; RuleFor is a read-only hot
// path with no further compilation.
//
// Layer precedence, highest first (first match across the merged list
// wins): custom (an explicit --rules file or the repo-local
// .gortex/review.yaml) → project (.gortex.yaml `review:`) → global
// (platform.ConfigDir()/config.yaml `review:`) → embedded defaults.
type RuleResolver struct {
	rules []compiledRule
}

// compiledRule pairs a config rule with its precompiled gitignore
// matcher so RuleFor never recompiles. The matcher is read-only after
// construction, so RuleFor is safe to call concurrently.
//
// Compilation goes through excludes.New, which neutralises the regexp
// metacharacters go-gitignore would otherwise splice into its pattern
// verbatim — a rule path holding "$", "^" or "|" would otherwise govern
// files it never named (#624).
type compiledRule struct {
	rule    config.ReviewRule
	matcher *excludes.Matcher
}

// NewRuleResolver merges the four rule layers in precedence order,
// drops Disabled rules, precompiles each remaining rule's glob, and
// returns a resolver ready for RuleFor.
//
// customPath, when non-empty, names an explicit review-rule file (the
// --rules flag). When empty, the repo-local .gortex/review.yaml under
// repoRoot is used as the custom layer if it exists. repoRoot is the
// repository root the project layer (.gortex.yaml) is loaded from; it
// may be empty to skip the project layer.
func NewRuleResolver(customPath, repoRoot string) (*RuleResolver, error) {
	var merged []config.ReviewRule

	// Layer 1 (highest precedence) — custom: an explicit --rules file,
	// else the repo-local .gortex/review.yaml.
	custom := customPath
	if custom == "" && repoRoot != "" {
		repoLocal := filepath.Join(repoRoot, ".gortex", "review.yaml")
		if _, err := os.Stat(repoLocal); err == nil {
			custom = repoLocal
		}
	}
	if custom != "" {
		rc, err := loadReviewFile(custom)
		if err != nil {
			return nil, err
		}
		merged = append(merged, rc.Rules...)
	}

	// Layer 2 — project: .gortex.yaml `review:` under repoRoot.
	if repoRoot != "" {
		projectPath := filepath.Join(repoRoot, ".gortex.yaml")
		if _, err := os.Stat(projectPath); err == nil {
			cfg, err := config.Load(projectPath)
			if err != nil {
				return nil, err
			}
			merged = append(merged, cfg.Review.Rules...)
		}
	}

	// Layer 3 — global: platform.ConfigDir()/config.yaml `review:`.
	globalPath := filepath.Join(platform.ConfigDir(), "config.yaml")
	if _, err := os.Stat(globalPath); err == nil {
		cfg, err := config.Load(globalPath)
		if err != nil {
			return nil, err
		}
		merged = append(merged, cfg.Review.Rules...)
	}

	// Layer 4 (lowest precedence) — embedded defaults.
	merged = append(merged, defaultReviewRules()...)

	compiled := make([]compiledRule, 0, len(merged))
	for _, r := range merged {
		if r.Disabled || r.Path == "" {
			continue
		}
		compiled = append(compiled, compiledRule{
			rule:    r,
			matcher: excludes.New([]string{r.Path}),
		})
	}

	return &RuleResolver{rules: compiled}, nil
}

// RuleFor returns the first rule whose glob matches filePath, scanning
// the merged layers in precedence order (custom → project → global →
// embedded). filePath should be repo-root-relative with forward-slash
// separators. The second return is false only when no rule matched —
// which cannot happen once the embedded `**` catch-all is present, but
// callers should still check it.
func (r *RuleResolver) RuleFor(filePath string) (config.ReviewRule, bool) {
	for _, cr := range r.rules {
		if cr.matcher.MatchRel(filePath) {
			return cr.rule, true
		}
	}
	return config.ReviewRule{}, false
}

// loadReviewFile reads a standalone review-rule file (--rules or
// .gortex/review.yaml). The file's top-level shape is a ReviewConfig:
// a bare `rules:` list plus the optional gating knobs.
func loadReviewFile(path string) (config.ReviewConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config.ReviewConfig{}, err
	}
	var rc config.ReviewConfig
	if err := yaml.Unmarshal(data, &rc); err != nil {
		return config.ReviewConfig{}, err
	}
	return rc, nil
}

// defaultReviewRules is the embedded lowest-precedence layer: a test
// rulepack for *_test.go files and a general catch-all for everything
// else. The catch-all `**` guarantees RuleFor always resolves.
func defaultReviewRules() []config.ReviewRule {
	return []config.ReviewRule{
		{Name: "test", Path: "**/*_test.go", Rulepack: "test"},
		{Name: "general", Path: "**", Rulepack: "general"},
	}
}
