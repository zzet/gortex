package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// guardRulesTestManager returns a ConfigManager with one repo per entry of
// bodies, each body written as that repo's `.gortex.yaml`.
func guardRulesTestManager(t *testing.T, bodies map[string]string) *config.ConfigManager {
	t.Helper()
	cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	for prefix, body := range bodies {
		repoDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(body), 0o644))
		cm.LoadWorkspaceConfig(prefix, repoDir)
	}
	return cm
}

const guardRulesRepoA = `
guards:
  rules:
    - name: a-rule
      kind: co-change
      source: "repo-a/pkg/handler"
      target: "repo-a/pkg/store"
      message: "repo-a handler changed without its store"
`

const guardRulesRepoB = `
guards:
  rules:
    - name: b-rule
      kind: co-change
      source: "repo-b/pkg/handler"
      target: "repo-b/pkg/store"
      message: "repo-b handler changed without its store"
`

func TestGuardRulesFor_ResolvesPerRepoWorkspaceConfig(t *testing.T) {
	// The daemon's own process config declares nothing — the normal case
	// for a multi-repo daemon, whose working directory belongs to none of
	// the tracked repos.
	s := &Server{configManager: guardRulesTestManager(t, map[string]string{
		"repo-a": guardRulesRepoA,
		"repo-b": guardRulesRepoB,
	})}

	a := s.guardRulesFor("repo-a")
	require.Len(t, a, 1)
	assert.Equal(t, "a-rule", a[0].Name)

	b := s.guardRulesFor("repo-b")
	require.Len(t, b, 1)
	assert.Equal(t, "b-rule", b[0].Name)

	assert.Empty(t, s.guardRulesFor("repo-c"), "a repo with no rules must resolve to none")
}

func TestGuardRulesFor_FallsBackToProcessSnapshot(t *testing.T) {
	// Embedded single-repo mode and tests wire no ConfigManager; the
	// snapshot the server was constructed with must still apply.
	snapshot := []config.GuardRule{{Name: "proc-rule", Kind: "co-change"}}

	s := &Server{guardRules: snapshot}
	assert.Equal(t, snapshot, s.guardRulesFor("repo-a"))

	withCM := &Server{
		guardRules:    snapshot,
		configManager: guardRulesTestManager(t, map[string]string{"repo-a": guardRulesRepoA}),
	}
	assert.Equal(t, snapshot, withCM.guardRulesFor("repo-b"),
		"a repo that declares no rules of its own falls back to the snapshot")
}

func TestHasGuardRules_SeesRulesDeclaredOnlyInWorkspaceConfig(t *testing.T) {
	// The reported bug: check_guards answered "no guard rules configured"
	// because it gated on the process-level snapshot alone, which in a
	// daemon holds the config of the daemon's working directory rather
	// than of any tracked repo.
	s := &Server{configManager: guardRulesTestManager(t, map[string]string{"repo-a": guardRulesRepoA})}

	assert.Empty(t, s.guardRules, "precondition: no process-level rules")
	assert.True(t, s.hasGuardRules([]string{"repo-a/pkg/handler/h.go::Handle"}))
	assert.True(t, s.anyGuardRulesConfigured())

	// repo-c is not tracked, so this ID cannot be attributed to it; with
	// repo-a the only tracked repo, it falls back there.
	assert.True(t, s.hasGuardRules([]string{"repo-c/pkg/handler/h.go::Handle"}))
}

func TestRepoScopeResolver_UnprefixedIDsResolveToTheSoleTrackedRepo(t *testing.T) {
	// A solo repo indexes unprefixed IDs, so the leading path segment is a
	// source directory, not a repo. Resolving it as one would look up
	// guard rules for a repo named "pkg" and find none.
	s := &Server{configManager: guardRulesTestManager(t, map[string]string{"solo": guardRulesRepoA})}

	groups, order := s.groupIDsByRepo([]string{"pkg/handler/h.go::Handle", "internal/x.go::X"})
	assert.Equal(t, []string{"solo"}, order)
	assert.Len(t, groups["solo"], 2)

	rules := s.guardRulesFor(order[0])
	require.Len(t, rules, 1)
	assert.Equal(t, "a-rule", rules[0].Name)
}

func TestRepoScopeResolver_KeepsDistinctTrackedPrefixesApart(t *testing.T) {
	s := &Server{configManager: guardRulesTestManager(t, map[string]string{
		"repo-a": guardRulesRepoA,
		"repo-b": guardRulesRepoB,
	})}

	_, order := s.groupIDsByRepo([]string{
		"repo-a/pkg/x.go::X",
		"repo-b/pkg/y.go::Y",
		"repo-a/pkg/z.go::Z",
	})
	assert.Equal(t, []string{"repo-a", "repo-b"}, order)
}

func TestEvaluateGuards_EvaluatesEachIDAgainstItsOwnRepoRules(t *testing.T) {
	g := graph.New()
	for _, n := range []*graph.Node{
		{ID: "repo-a/pkg/handler/h.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "repo-a/pkg/handler/h.go"},
		{ID: "repo-b/pkg/handler/h.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "repo-b/pkg/handler/h.go"},
	} {
		g.AddNode(n)
	}

	s := &Server{
		graph: g,
		configManager: guardRulesTestManager(t, map[string]string{
			"repo-a": guardRulesRepoA,
			"repo-b": guardRulesRepoB,
		}),
	}

	got := s.evaluateGuards(g, []string{
		"repo-a/pkg/handler/h.go::Handle",
		"repo-b/pkg/handler/h.go::Handle",
	})

	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.RuleName)
	}
	assert.ElementsMatch(t, []string{"a-rule", "b-rule"}, names,
		"each repo's rules must fire for that repo's changed symbols")
}

func TestEvaluateGuards_RulesDoNotLeakAcrossRepos(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "repo-b/pkg/handler/h.go::Handle",
		Name:     "Handle",
		Kind:     graph.KindFunction,
		FilePath: "repo-b/pkg/handler/h.go",
	})

	// Only repo-a declares rules; repo-b's symbols must not be judged by them.
	s := &Server{
		graph:         g,
		configManager: guardRulesTestManager(t, map[string]string{"repo-a": guardRulesRepoA}),
	}

	assert.Empty(t, s.evaluateGuards(g, []string{"repo-b/pkg/handler/h.go::Handle"}))
}

func TestGroupIDsByRepo_NoTrackedSetTrustsTheIDPrefix(t *testing.T) {
	// Neither ConfigManager nor MultiIndexer wired (embedded mode, tests):
	// the ID's own leading segment is the best information available.
	s := &Server{}
	groups, order := s.groupIDsByRepo([]string{"pkg/handler/h.go::Handle", "repo-a/pkg/x.go::X"})
	assert.Equal(t, []string{"pkg", "repo-a"}, order)
	assert.Equal(t, []string{"pkg/handler/h.go::Handle"}, groups["pkg"])
	assert.Equal(t, []string{"repo-a/pkg/x.go::X"}, groups["repo-a"])
}
