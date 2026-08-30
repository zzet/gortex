package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestReloadDoesNotPublishConfigReadBeforeRelocation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	oldRoot := filepath.Join(dir, "worktree-a")
	newRoot := filepath.Join(dir, "worktree-b")
	require.NoError(t, os.MkdirAll(oldRoot, 0o755))
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: oldRoot, Name: "stable"}}}
	gc.SetConfigPath(configPath)
	require.NoError(t, gc.Save())

	cm, err := NewConfigManager(configPath)
	require.NoError(t, err)
	require.NoError(t, os.Rename(oldRoot, newRoot))

	reloadRead := make(chan struct{})
	allowReloadPublish := make(chan struct{})
	var blockFirst sync.Once
	cm.reloadAfterRead = func() {
		blockFirst.Do(func() {
			close(reloadRead)
			<-allowReloadPublish
		})
	}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- cm.Reload() }()
	<-reloadRead

	moved, err := cm.RelocateRepoAndSaveIfPresent(
		[]string{oldRoot}, newRoot, "stable",
		RepoRelocationSources{TopLevel: true},
	)
	require.NoError(t, err)
	require.True(t, moved)
	close(allowReloadPublish)
	require.NoError(t, <-reloadDone)

	require.Len(t, cm.Global().Repos, 1)
	require.True(t, pathkey.EqualPaths(
		canonicalConfiguredPath(cm.Global().Repos[0].Path),
		canonicalConfiguredPath(newRoot),
	))
	reloaded, err := LoadGlobal(configPath)
	require.NoError(t, err)
	require.Len(t, reloaded.Repos, 1)
	require.True(t, pathkey.EqualPaths(
		canonicalConfiguredPath(reloaded.Repos[0].Path),
		canonicalConfiguredPath(newRoot),
	))
}

// --- Unit Tests ---

func TestNewConfigManager_MissingGlobalConfig(t *testing.T) {
	// A non-existent global config path should not error (returns empty config).
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)
	require.NotNil(t, cm)
	assert.NotNil(t, cm.Global())
	assert.Empty(t, cm.Global().Repos)
}

func TestNewConfigManager_ValidGlobalConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: my-proj
repos:
  - path: /home/user/repo1
    name: repo1
projects:
  my-proj:
    repos:
      - path: /home/user/repo1
        name: repo1
        ref: work
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)
	assert.Equal(t, "my-proj", cm.Global().ActiveProject)
	assert.Len(t, cm.Global().Repos, 1)
}

func TestGetRepoConfig_NoWorkspaceConfig(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	cfg := cm.GetRepoConfig("unknown-repo")
	require.NotNil(t, cfg)
	// No global, no workspace, no RepoEntry → effective = builtin.
	assert.Equal(t, excludes.Builtin, cfg.Index.Exclude)
	assert.Equal(t, excludes.Builtin, cfg.Watch.Exclude)
}

func TestGetRepoConfig_WithWorkspaceConfig(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Create a temp repo with .gortex.yaml using the new top-level key.
	repoDir := t.TempDir()
	wsContent := `
exclude:
  - "custom/**"
guards:
  rules:
    - name: test-rule
      kind: co-change
      source: "src"
      target: "test"
      message: "test changes required"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))

	cm.LoadWorkspaceConfig("my-repo", repoDir)

	cfg := cm.GetRepoConfig("my-repo")
	require.NotNil(t, cfg)
	// Effective list is builtin + workspace; workspace patterns at the tail.
	assert.Equal(t, "custom/**", cfg.Index.Exclude[len(cfg.Index.Exclude)-1])
	assert.Contains(t, cfg.Index.Exclude, ".git/")
	assert.Len(t, cfg.Guards.Rules, 1)
	assert.Equal(t, "test-rule", cfg.Guards.Rules[0].Name)
}

func TestEffectiveExclude_WorkspaceAppendsToBuiltin(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
exclude:
  - "ws-vendor/**"
  - "ws-build/**"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("repo-a", repoDir)

	got := cm.EffectiveExclude("repo-a")
	// Builtin leads, workspace tails.
	assert.Equal(t, excludes.Builtin[0], got[0])
	assert.Equal(t, []string{"ws-vendor/**", "ws-build/**"}, got[len(got)-2:])
}

func TestEffectiveExclude_LegacyIndexExcludeStillRead(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Old-shape .gortex.yaml: exclude under index.exclude, no top-level key.
	repoDir := t.TempDir()
	wsContent := `
index:
  exclude:
    - "legacy/**"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("legacy-repo", repoDir)

	got := cm.EffectiveExclude("legacy-repo")
	assert.Contains(t, got, "legacy/**", "legacy index.exclude must still be honoured")
}

func TestEffectiveExclude_IncludeForceReincludesGitignored(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("Pods/\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"),
		[]byte("include:\n  - \"Pods/\"\n"), 0644))
	cm.LoadWorkspaceConfig("repo-inc", repoDir)

	got := cm.EffectiveExclude("repo-inc")
	assert.Contains(t, got, "Pods/", "the .gitignore exclude is present")
	assert.Equal(t, "!Pods/", got[len(got)-1], "the force-include negation lands last so it wins")

	// The compiled matcher must NOT exclude the force-included tree.
	assert.False(t, excludes.New(got).MatchRel("Pods/"),
		"a force-included gitignored dir must be re-included")
}

func TestEffectiveExclude_FallsBackToBuiltin(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// No workspace config, no global Exclude — expect the builtin baseline.
	got := cm.EffectiveExclude("unknown-repo")
	assert.Equal(t, excludes.Builtin, got)
}

func TestEffectiveExclude_EmptyWorkspaceExcludeYieldsBuiltin(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
index:
  workers: 4
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("repo-b", repoDir)

	got := cm.EffectiveExclude("repo-b")
	assert.Equal(t, excludes.Builtin, got)
}

func TestEffectiveExclude_LayersAllFour(t *testing.T) {
	// Global with Exclude + RepoEntry with Exclude + workspace exclude.
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.yaml")
	repoDir := t.TempDir()
	globalContent := `
exclude:
  - "global-pat/**"
repos:
  - path: ` + repoDir + `
    name: my-repo
    exclude:
      - "entry-pat/**"
`
	require.NoError(t, os.WriteFile(globalPath, []byte(globalContent), 0644))

	wsContent := `
exclude:
  - "ws-pat/**"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))

	cm, err := NewConfigManager(globalPath)
	require.NoError(t, err)
	cm.LoadWorkspaceConfig("my-repo", repoDir)

	got := cm.EffectiveExclude("my-repo")
	// Order: builtin → global → entry → workspace.
	tail := got[len(got)-3:]
	assert.Equal(t, []string{"global-pat/**", "entry-pat/**", "ws-pat/**"}, tail)
	assert.Contains(t, got, ".git/", "builtin still at the head")
}

func TestEffectiveExclude_RespectsRepoGitignoreByDefault(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	gitignore := `# repo .gitignore
data/raw/
*.tmp.local
# blank line below

testdata/large/
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(gitignore), 0644))
	// No `.gortex.yaml` — gitignore reading still kicks in because the
	// path is cached on LoadWorkspaceConfig regardless.
	cm.LoadWorkspaceConfig("gi-repo", repoDir)

	got := cm.EffectiveExclude("gi-repo")
	assert.Contains(t, got, "data/raw/", "repo .gitignore entries must be layered in by default")
	assert.Contains(t, got, "*.tmp.local")
	assert.Contains(t, got, "testdata/large/")
	for _, p := range got {
		assert.NotEqual(t, "", p, "no empty patterns")
		assert.NotEqual(t, "#", p[:1], "no comment lines")
	}
}

func TestEffectiveExclude_RespectGitignoreFalseOptsOut(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("ignored/\n"), 0644))
	wsContent := `
respect_gitignore: false
exclude:
  - "ws-only/**"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("opt-out-repo", repoDir)

	got := cm.EffectiveExclude("opt-out-repo")
	assert.NotContains(t, got, "ignored/", "respect_gitignore=false must skip .gitignore layer")
	assert.Contains(t, got, "ws-only/**", "workspace exclude still applies")
}

func TestEffectiveExclude_NoGitignoreFileNoOp(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	cm.LoadWorkspaceConfig("no-gi", repoDir)

	got := cm.EffectiveExclude("no-gi")
	// Absent .gitignore must not error; output is just the builtin baseline.
	assert.Equal(t, excludes.Builtin, got)
}

func TestEffectiveGuardRules_WorkspaceOverridesGlobal(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
guards:
  rules:
    - name: ws-rule
      kind: boundary
      source: "pkg/a"
      target: "pkg/b"
      message: "boundary violation"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("repo-c", repoDir)

	rules := cm.EffectiveGuardRules("repo-c")
	assert.Len(t, rules, 1)
	assert.Equal(t, "ws-rule", rules[0].Name)
}

func TestEffectiveGuardRules_FallsBackToGlobalDefaults(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Default config has no guard rules.
	rules := cm.EffectiveGuardRules("unknown-repo")
	assert.Empty(t, rules)
}

func TestActiveRepos_WithActiveProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: my-proj
repos:
  - path: /top-level/repo
    name: top-repo
projects:
  my-proj:
    repos:
      - path: /proj/repo1
        name: proj-repo1
      - path: /proj/repo2
        name: proj-repo2
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)

	repos := cm.ActiveRepos()
	assert.Len(t, repos, 2)
	assert.Equal(t, "proj-repo1", repos[0].Name)
	assert.Equal(t, "proj-repo2", repos[1].Name)
}

func TestActiveRepos_NoActiveProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
repos:
  - path: /top/repo1
    name: repo1
  - path: /top/repo2
    name: repo2
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)

	repos := cm.ActiveRepos()
	assert.Len(t, repos, 2)
	assert.Equal(t, "repo1", repos[0].Name)
}

func TestActiveRepos_InvalidActiveProjectFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: nonexistent
repos:
  - path: /fallback/repo
    name: fallback
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)

	repos := cm.ActiveRepos()
	assert.Len(t, repos, 1)
	assert.Equal(t, "fallback", repos[0].Name)
}

func TestLoadWorkspaceConfig_MissingFile(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Loading from a dir without .gortex.yaml should not cache anything.
	cm.LoadWorkspaceConfig("repo-x", t.TempDir())

	cfg := cm.getWorkspaceConfig("repo-x")
	assert.Nil(t, cfg)
}

func TestLoadWorkspaceConfig_MalformedYAML(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoDir, ".gortex.yaml"),
		[]byte(":::invalid yaml content"),
		0644,
	))

	// Should log warning and not cache.
	cm.LoadWorkspaceConfig("bad-repo", repoDir)

	cfg := cm.getWorkspaceConfig("bad-repo")
	assert.Nil(t, cfg)
}

func TestNewConfigManager_EmptyPath(t *testing.T) {
	// Empty path uses default — may or may not exist, but should not panic.
	cm, err := NewConfigManager("")
	// We can't guarantee the default path exists, but it should not error
	// if the file is simply missing.
	if err == nil {
		assert.NotNil(t, cm)
	}
}

// --- Property-Based Tests ---

// workspaceYAML is a helper struct for marshaling workspace config to YAML.
// We use explicit yaml tags since the Config struct uses mapstructure tags.
type workspaceYAML struct {
	Exclude []string   `yaml:"exclude,omitempty"`
	Index   indexYAML  `yaml:"index,omitempty"`
	Guards  guardsYAML `yaml:"guards,omitempty"`
}

type indexYAML struct {
	Exclude []string `yaml:"exclude,omitempty"`
}

type guardsYAML struct {
	Rules []guardRuleYAML `yaml:"rules,omitempty"`
}

type guardRuleYAML struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"`
	Source  string `yaml:"source"`
	Target  string `yaml:"target"`
	Message string `yaml:"message"`
}

// genExcludePatterns generates a random list of exclude patterns (sometimes empty).
func genExcludePatterns() *rapid.Generator[[]string] {
	return rapid.Custom(func(t *rapid.T) []string {
		isEmpty := rapid.Bool().Draw(t, "emptyExclude")
		if isEmpty {
			return nil
		}
		n := rapid.IntRange(1, 5).Draw(t, "numPatterns")
		patterns := make([]string, n)
		for i := range n {
			patterns[i] = rapid.StringMatching(`[a-z]{1,10}/\*\*`).Draw(t, "pattern")
		}
		return patterns
	})
}

// genGuardRules generates a random list of guard rules (sometimes empty).
func genGuardRules() *rapid.Generator[[]guardRuleYAML] {
	return rapid.Custom(func(t *rapid.T) []guardRuleYAML {
		isEmpty := rapid.Bool().Draw(t, "emptyRules")
		if isEmpty {
			return nil
		}
		n := rapid.IntRange(1, 3).Draw(t, "numRules")
		rules := make([]guardRuleYAML, n)
		for i := range n {
			rules[i] = guardRuleYAML{
				Name:    rapid.StringMatching(`[a-z]{3,10}-rule`).Draw(t, "ruleName"),
				Kind:    rapid.SampledFrom([]string{"co-change", "boundary"}).Draw(t, "kind"),
				Source:  rapid.StringMatching(`[a-z]{2,8}/[a-z]{2,8}`).Draw(t, "source"),
				Target:  rapid.StringMatching(`[a-z]{2,8}/[a-z]{2,8}`).Draw(t, "target"),
				Message: rapid.StringMatching(`[a-z ]{5,30}`).Draw(t, "message"),
			}
		}
		return rules
	})
}

// TestPropertyConfigLayeringSemantics verifies the current contract:
// workspace excludes are appended to the builtin baseline (plus global
// and per-RepoEntry layers when present). Guard rules remain
// replace-semantics — workspace rules win wholesale when present.
func TestPropertyConfigLayeringSemantics(t *testing.T) {
	globalDefaults := Default()

	rapid.Check(t, func(rt *rapid.T) {
		wsExcludeNew := genExcludePatterns().Draw(rt, "wsExclude")
		wsRules := genGuardRules().Draw(rt, "wsRules")

		repoPrefix := rapid.StringMatching(`[a-z]{3,10}`).Draw(rt, "repoPrefix")
		repoDir := t.TempDir()

		wsCfg := workspaceYAML{
			Exclude: wsExcludeNew,
			Guards:  guardsYAML{Rules: wsRules},
		}
		data, err := yaml.Marshal(&wsCfg)
		require.NoError(rt, err)
		err = os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), data, 0644)
		require.NoError(rt, err)

		cm, err := NewConfigManager("/tmp/nonexistent-gortex-pbt-" + repoPrefix + "/config.yaml")
		require.NoError(rt, err)
		cm.LoadWorkspaceConfig(repoPrefix, repoDir)

		// --- Exclude: append semantics ---
		effectiveExclude := cm.EffectiveExclude(repoPrefix)
		// Builtin always at the head.
		assert.Equal(rt, excludes.Builtin[0], effectiveExclude[0],
			"builtin list must lead the effective excludes")
		// Workspace patterns appended at the tail.
		if len(wsExcludeNew) > 0 {
			tail := effectiveExclude[len(effectiveExclude)-len(wsExcludeNew):]
			assert.Equal(rt, wsExcludeNew, tail,
				"workspace excludes should be appended after builtin")
		}

		// --- Guard rules: replace semantics (unchanged) ---
		effectiveRules := cm.EffectiveGuardRules(repoPrefix)
		if len(wsRules) > 0 {
			assert.Len(rt, effectiveRules, len(wsRules),
				"workspace guard rules should override global when present")
			for i, rule := range effectiveRules {
				assert.Equal(rt, wsRules[i].Name, rule.Name)
				assert.Equal(rt, wsRules[i].Kind, rule.Kind)
				assert.Equal(rt, wsRules[i].Source, rule.Source)
				assert.Equal(rt, wsRules[i].Target, rule.Target)
				assert.Equal(rt, wsRules[i].Message, rule.Message)
			}
		} else {
			assert.Equal(rt, globalDefaults.Guards.Rules, effectiveRules,
				"global default guard rules should apply when workspace is empty")
		}

		// Unknown repo → builtin baseline only.
		unknownPrefix := repoPrefix + "-unknown"
		assert.Equal(rt, excludes.Builtin, cm.EffectiveExclude(unknownPrefix),
			"repo without workspace config should get the builtin baseline")
		assert.Equal(rt, globalDefaults.Guards.Rules, cm.EffectiveGuardRules(unknownPrefix),
			"repo without workspace config should get global default guard rules")
	})
}

// --- Reload re-reads per-repo `.gortex.yaml` (issue #320) ---

// writeWorkspaceConfig writes a `.gortex.yaml` into repoDir.
func writeWorkspaceConfig(t *testing.T, repoDir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(body), 0644))
}

func TestReload_RereadsWorkspaceConfigForTrackedRepos(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	writeWorkspaceConfig(t, repoDir, `
exclude:
  - "old-dir/**"
guards:
  rules:
    - name: old-rule
      kind: boundary
      source: "pkg/a"
      target: "pkg/b"
      message: "boundary violation"
`)
	cm.LoadWorkspaceConfig("repo", repoDir)
	require.Contains(t, cm.EffectiveExclude("repo"), "old-dir/**")

	// The user edits the repo's config and asks the daemon to reload. No
	// LoadWorkspaceConfig call follows — that is the whole point: nothing
	// on the reload path used to make one for an already-tracked repo.
	writeWorkspaceConfig(t, repoDir, `
exclude:
  - "new-dir/**"
guards:
  rules:
    - name: new-rule
      kind: boundary
      source: "pkg/c"
      target: "pkg/d"
      message: "boundary violation"
`)
	require.NoError(t, cm.Reload())

	got := cm.EffectiveExclude("repo")
	assert.Contains(t, got, "new-dir/**", "reload must pick up the edited exclude list")
	assert.NotContains(t, got, "old-dir/**", "reload must drop the superseded exclude list")

	rules := cm.EffectiveGuardRules("repo")
	require.Len(t, rules, 1)
	assert.Equal(t, "new-rule", rules[0].Name, "reload must pick up the edited guard rules")
}

func TestReload_KeepsGitignoreLayerForTrackedRepos(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("tmp/\n"), 0644))
	cm.LoadWorkspaceConfig("repo", repoDir)
	require.Contains(t, cm.EffectiveExclude("repo"), "tmp/")

	// Reload used to clear workspacePaths, which is how EffectiveExclude
	// locates the repo's own `.gitignore` — so the whole gitignore layer
	// died with the workspace config and the walk fell back to
	// builtin-only admission.
	require.NoError(t, cm.Reload())

	assert.Contains(t, cm.EffectiveExclude("repo"), "tmp/",
		"reload must not sever the repo's .gitignore layer")
}

func TestReload_DropsDeletedWorkspaceConfig(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	writeWorkspaceConfig(t, repoDir, "exclude:\n  - \"gone/**\"\n")
	cm.LoadWorkspaceConfig("repo", repoDir)
	require.Contains(t, cm.EffectiveExclude("repo"), "gone/**")

	require.NoError(t, os.Remove(filepath.Join(repoDir, ".gortex.yaml")))
	require.NoError(t, cm.Reload())

	assert.NotContains(t, cm.EffectiveExclude("repo"), "gone/**",
		"a deleted .gortex.yaml must stop applying")
	assert.Nil(t, cm.getWorkspaceConfig("repo"))
}

func TestReload_KeepsLastGoodParseOnMalformedEdit(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	writeWorkspaceConfig(t, repoDir, "exclude:\n  - \"good/**\"\n")
	cm.LoadWorkspaceConfig("repo", repoDir)
	require.Contains(t, cm.EffectiveExclude("repo"), "good/**")

	// A half-saved edit must not silently downgrade the repo to global
	// defaults — that is indistinguishable from the bug being fixed here.
	writeWorkspaceConfig(t, repoDir, ":::invalid yaml content")
	require.NoError(t, cm.Reload())

	assert.Contains(t, cm.EffectiveExclude("repo"), "good/**",
		"a malformed edit must keep the last good parse")
}

func TestLoadWorkspaceConfig_DeletedFileDropsCachedConfig(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	writeWorkspaceConfig(t, repoDir, "exclude:\n  - \"gone/**\"\n")
	cm.LoadWorkspaceConfig("repo", repoDir)
	require.NotNil(t, cm.getWorkspaceConfig("repo"))

	require.NoError(t, os.Remove(filepath.Join(repoDir, ".gortex.yaml")))
	cm.LoadWorkspaceConfig("repo", repoDir)

	assert.Nil(t, cm.getWorkspaceConfig("repo"),
		"re-reading a repo whose .gortex.yaml was deleted must drop the cached config")
}

func TestLoadWorkspaceConfig_MalformedEditKeepsLastGoodParse(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	writeWorkspaceConfig(t, repoDir, "exclude:\n  - \"good/**\"\n")
	cm.LoadWorkspaceConfig("repo", repoDir)

	writeWorkspaceConfig(t, repoDir, ":::invalid yaml content")
	cm.LoadWorkspaceConfig("repo", repoDir)

	cfg := cm.getWorkspaceConfig("repo")
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"good/**"}, cfg.Exclude)
}
