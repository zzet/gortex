package mcp

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// TestAnalyzeOwnership_PartialCoverageInsideOneRepoKeepsTheCaveat is the
// review case. blame.EnrichGraph is best-effort per file — Run failing or
// returning nothing skips that file and the pass still reports success — so
// one repository holds both stamped and unstamped eligible symbols. Reading a
// single stamp as coverage dropped the caveat and published a silently
// incomplete answer, which is the failure this whole change exists to stop,
// re-entered one level down.
func TestAnalyzeOwnership_PartialCoverageInsideOneRepoKeepsTheCaveat(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameNodeInRepo(srv.graph, "repo-a", "repo-a/f.go::A", "repo-a/f.go", "alice@x", time.Now().Unix())
	// Same repository, same kind, eligible for blame — the pass simply never
	// stamped it.
	addBlameNodeInRepo(srv.graph, "repo-a", "repo-a/g.go::B", "repo-a/g.go", "", 0)

	out := callAnalyzeOwnership(t, srv, map[string]any{})

	owners, _ := out["owners"].([]any)
	require.NotEmpty(t, owners, "the stamped symbol must still produce a row")
	state := ownershipDataStateOf(t, out)
	require.Equal(t, "partial", state["state"],
		"one stamp in a repository is not coverage of it")
	repos, _ := state["repos"].([]any)
	require.Contains(t, repos, "repo-a",
		"the partly covered repository must be named, not just the wholly unmined ones")

	eligible, _ := state["symbols_eligible"].(float64)
	stamped, _ := state["symbols_stamped"].(float64)
	require.Greater(t, eligible, stamped, "the shortfall must be visible as a size, not only a verdict")
}

// TestAnalyzeOwnership_IneligibleSymbolsAreNotCountedAsAShortfall is the other
// direction, and the reason coverage is counted against blame's own admission
// set. A symbol the pass never looks at is not a coverage hole: counting it
// would report a shortfall that no enrichment could ever close, and a caveat
// that never clears is one a caller learns to ignore.
func TestAnalyzeOwnership_IneligibleSymbolsAreNotCountedAsAShortfall(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNodeInRepo(srv.graph, "repo-a", "repo-a/f.go::A", "repo-a/f.go", "alice@x", now)
	// StartLine 0: no position to blame, so blame.EnrichGraph skips it before
	// it ever reaches git.
	srv.graph.AddNode(&graph.Node{
		ID: "repo-a/f.go::NoPosition", Kind: graph.KindFunction, Name: "NoPosition",
		FilePath: "repo-a/f.go", RepoPrefix: "repo-a",
	})

	out := callAnalyzeOwnership(t, srv, map[string]any{"path_prefix": "repo-a/"})

	state, present := out["data_state"].(map[string]any)
	if present {
		repos, _ := state["repos"].([]any)
		require.NotContains(t, repos, "repo-a",
			"an unblameable symbol was reported as a coverage shortfall")
	}
}

// TestAnalyzeOwnership_ReindexAfterEnrichmentReportsPartialCoverage runs the
// real pass over a real git repository, then does what a working day does:
// adds code and re-indexes. The new symbols are eligible and unstamped, so the
// ownership answer that follows is an undercount — and it has rows, which is
// the shape that reads as complete.
func TestAnalyzeOwnership_ReindexAfterEnrichmentReportsPartialCoverage(t *testing.T) {
	refIsolateGit(t)
	dir := t.TempDir()
	refWriteFiles(t, dir, map[string]string{
		"keep.go": "package repo\n\nfunc Keeper() {}\n",
	})
	refGit(t, dir, "init", "--initial-branch=main")
	refGit(t, dir, "add", "-A")
	refGit(t, dir, "commit", "-m", "initial")

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	enriched, err := blame.EnrichGraph(g, dir)
	require.NoError(t, err)
	require.Positive(t, enriched, "the blame pass stamped nothing; the fixture proves nothing")

	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	before := callAnalyzeOwnership(t, srv, map[string]any{})
	require.NotEmpty(t, before["owners"], "the enriched repo must have an owner")
	_, caveated := before["data_state"]
	require.False(t, caveated,
		"a fully stamped scope must not carry a caveat, or partial means nothing")

	// A working day: new code lands and the index catches up. Blame does not.
	refWriteFiles(t, dir, map[string]string{
		"added.go": "package repo\n\nfunc Added() {}\n\nfunc AlsoAdded() {}\n",
	})
	_, err = idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "added.go")})
	require.NoError(t, err)

	after := callAnalyzeOwnership(t, srv, map[string]any{})
	require.NotEmpty(t, after["owners"], "the previously stamped owner must still be reported")
	state := ownershipDataStateOf(t, after)
	require.Equal(t, "partial", state["state"],
		"symbols indexed after the blame pass are invisible to this answer and nothing said so")
	eligible, _ := state["symbols_eligible"].(float64)
	stamped, _ := state["symbols_stamped"].(float64)
	require.Greater(t, eligible, stamped)
}

// TestAnalyzeOwnership_AStampOnAnIneligibleSymbolCannotFillTheGap closes the
// other side of the eligibility rule. The stamped tally is filtered too,
// because a stamp on a symbol blame would never admit — a stale meta value
// left behind when an edit moved a symbol to StartLine 0, say — would
// otherwise be counted against an eligible population it is not part of, and
// one such stamp is enough to make a short repository read as covered.
func TestAnalyzeOwnership_AStampOnAnIneligibleSymbolCannotFillTheGap(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNodeInRepo(srv.graph, "repo-a", "repo-a/f.go::A", "repo-a/f.go", "alice@x", now)
	addBlameNodeInRepo(srv.graph, "repo-a", "repo-a/g.go::B", "repo-a/g.go", "", 0)
	// Eligible for nothing (no position), yet carrying authorship.
	srv.graph.AddNode(&graph.Node{
		ID: "repo-a/h.go::Stale", Kind: graph.KindFunction, Name: "Stale",
		FilePath: "repo-a/h.go", RepoPrefix: "repo-a",
		Meta: map[string]any{
			"last_authored": map[string]any{"email": "bob@x", "timestamp": now},
		},
	})

	out := callAnalyzeOwnership(t, srv, map[string]any{"path_prefix": "repo-a/"})

	state := ownershipDataStateOf(t, out)
	require.Equal(t, "partial", state["state"],
		"a stamp outside the eligible population was counted as coverage of it")
	stamped, _ := state["symbols_stamped"].(float64)
	eligible, _ := state["symbols_eligible"].(float64)
	require.Equal(t, float64(1), stamped, "the ineligible stamp was tallied")
	require.Equal(t, float64(2), eligible)
}
