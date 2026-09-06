package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// PathIndexability returns the zero PathSkip both for "indexable" and for "this
// repo cannot answer", so counting the second as a vote let one non-answering
// repo bail the whole verdict to "no reason known".
func TestUnanimousPathSkip(t *testing.T) {
	var (
		indexable = indexer.PathSkip{}
		byRule    = indexer.PathSkip{Skipped: true, ByRule: true}
		unclaimed = indexer.PathSkip{Skipped: true}

		answered = func(s indexer.PathSkip) pathSkipVote { return pathSkipVote{Skip: s, Answered: true} }
		abstain  = pathSkipVote{}
	)

	for _, tc := range []struct {
		name     string
		votes    []pathSkipVote
		want     indexer.PathSkip
		wantOK   bool
		scenario string
	}{{
		name:     "one repo, excluded by rule",
		votes:    []pathSkipVote{answered(byRule)},
		want:     byRule,
		wantOK:   true,
		scenario: "the ordinary single-answer case",
	}, {
		name:     "every answering repo agrees, one abstains",
		votes:    []pathSkipVote{answered(byRule), abstain},
		want:     byRule,
		wantOK:   true,
		scenario: "repoB has no stored root yet; its silence must not veto repoA",
	}, {
		name:     "abstention first, then a real vote",
		votes:    []pathSkipVote{abstain, answered(byRule)},
		want:     byRule,
		wantOK:   true,
		scenario: "iteration order over RepoPrefixes() is unspecified",
	}, {
		name:     "abstentions on both sides of a vote",
		votes:    []pathSkipVote{abstain, answered(unclaimed), abstain},
		want:     unclaimed,
		wantOK:   true,
		scenario: "any number of non-answering repos still leaves one verdict",
	}, {
		name:     "genuine disagreement",
		votes:    []pathSkipVote{answered(byRule), answered(indexable)},
		wantOK:   false,
		scenario: "two repos that both looked and reached opposite verdicts",
	}, {
		name:     "disagreement on the REASON alone still bails",
		votes:    []pathSkipVote{answered(byRule), answered(unclaimed)},
		wantOK:   false,
		scenario: "ByRule drives the rendered message, so a split on it is a split",
	}, {
		name:     "nobody could answer",
		votes:    []pathSkipVote{abstain, abstain},
		wantOK:   false,
		scenario: "no evidence at all leaves enforcement on",
	}, {
		name:     "no repos at all",
		votes:    nil,
		wantOK:   false,
		scenario: "an empty workspace has nothing to say",
	}, {
		name:     "unanimous indexable",
		votes:    []pathSkipVote{answered(indexable), answered(indexable)},
		want:     indexable,
		wantOK:   true,
		scenario: "agreeing that the walk WOULD hold it is a verdict too",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := unanimousPathSkip(tc.votes)
			if ok != tc.wantOK {
				t.Fatalf("unanimousPathSkip() ok = %v, want %v (%s)", ok, tc.wantOK, tc.scenario)
			}
			if ok && got != tc.want {
				t.Errorf("unanimousPathSkip() = %+v, want %+v (%s)", got, tc.want, tc.scenario)
			}
		})
	}
}

// The two wire flags differ in width on purpose: Excluded is the narrow "a RULE
// is the reason", Unindexable the full verdict. Wiring off Excluded alone
// under-silences.
func TestSkipStateFlagWidths(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip indexer.PathSkip
		want fileNotIndexedState
	}{
		{"indexable", indexer.PathSkip{}, fileNotIndexedState{}},
		{"excluded by rule", indexer.PathSkip{Skipped: true, ByRule: true}, fileNotIndexedState{Unindexable: true, Excluded: true}},
		{"unindexable, no rule", indexer.PathSkip{Skipped: true}, fileNotIndexedState{Unindexable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipState(tc.skip); got != tc.want {
				t.Errorf("skipState(%+v) = %+v, want %+v", tc.skip, got, tc.want)
			}
		})
	}
}

// Same distinction as the fold. The two render identically today, so this pins
// the intent before they drift.
func TestPathIndexability_SingleRepoCannotAnswer(t *testing.T) {
	idx := indexer.New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	srv := &Server{indexer: idx}

	if got := srv.pathIndexability("node_modules/dpack/lib/Block.js"); got != (fileNotIndexedState{}) {
		t.Errorf("an indexer with no stored root must yield no reason, got %+v", got)
	}
}

// newPrefixFixture wires two tracked repos so the multi-repo fallback has real
// prefixes to route against.
func newPrefixFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoA, Name: "repo-a"},
		{Path: repoB, Name: "repo-b"},
	}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	return &Server{multiIndexer: mi}, repoA, repoB
}

// A graph path that names its corpus must be answered by that repo, against a
// path relative to ITS root. Asking every repo about the prefixed spelling
// either reaches nothing (the common case, so the fallback contributes
// nothing) or reaches an unrelated file of the same name in another repo.
func TestPathIndexability_PrefixedPathRoutesToItsOwnRepo(t *testing.T) {
	srv, repoA, _ := newPrefixFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repoA, "vendor", "dep"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoA, "vendor", "dep", "a.go"), []byte("package dep\n"), 0o644))

	got := srv.pathIndexability("repo-a/vendor/dep/a.go")
	if !got.Excluded {
		t.Errorf("a vendored path in repo-a must answer excluded, got %+v", got)
	}
}

// The prefix must not be stripped off a path that merely starts with the same
// letters.
func TestSplitRepoPrefix(t *testing.T) {
	srv, _, _ := newPrefixFixture(t)

	prefix, rel, ok := srv.splitRepoPrefix("repo-a/vendor/dep/a.go")
	if !ok || prefix != "repo-a" || rel != "vendor/dep/a.go" {
		t.Errorf("splitRepoPrefix = (%q, %q, %v), want (repo-a, vendor/dep/a.go, true)", prefix, rel, ok)
	}
	if _, _, ok := srv.splitRepoPrefix("repo-abc/main.go"); ok {
		t.Error("a path that only shares a prefix's letters must not be split")
	}
	if _, _, ok := srv.splitRepoPrefix("node_modules/dpack/lib/Block.js"); ok {
		t.Error("a bare unprefixed path must fall through to the vote")
	}
}
