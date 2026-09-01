package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// rootOnlyIndexer builds an indexer whose only job is to report repoRoot as its
// RootPath so collectRepoRoots / pickRepoRoot resolve the CODEOWNERS repo. It
// indexes nothing.
func rootOnlyIndexer(repoRoot string) *indexer.Indexer {
	reg := testRegistry()
	idx := indexer.New(graph.New(), reg, config.Default().Index, zap.NewNop())
	idx.SetRootPath(repoRoot)
	return idx
}

// --- rankReviewers pure table tests -----------------------------------------

func TestRankReviewers_CodeownerOutranksWeakCoChange(t *testing.T) {
	// A single CODEOWNERS match (weight 3) must outrank even a few
	// co-change touches (weight 1 each) below the codeowner's score.
	codeowners := map[string]int{"alice": 1} // 3*1 = 3
	authors := map[string]int{}              //
	coChange := map[string]int{"bob": 2}     // 1*2 = 2
	kinds := map[string]string{"alice": "person"}
	matched := map[string][]string{"alice": {"pkg/a.go"}}

	out := rankReviewers(codeowners, authors, coChange, kinds, matched)
	require.Len(t, out, 2)
	require.Equal(t, "alice", out[0].Reviewer, "codeowner must rank first")
	require.Equal(t, 3, out[0].Score)
	require.Equal(t, "bob", out[1].Reviewer)
	require.Equal(t, 2, out[1].Score)
	require.Equal(t, []string{"pkg/a.go"}, out[0].MatchedFiles)
	require.NotEmpty(t, out[0].Reasons)
	require.Empty(t, out[1].MatchedFiles)
}

func TestRankReviewers_BlendAndReasons(t *testing.T) {
	// A reviewer present in all three signals accumulates a blended score
	// and a reason per signal.
	codeowners := map[string]int{"alice": 1} // 3
	authors := map[string]int{"alice": 2}    // 4
	coChange := map[string]int{"alice": 1}   // 1
	out := rankReviewers(codeowners, authors, coChange, nil, nil)
	require.Len(t, out, 1)
	require.Equal(t, "alice", out[0].Reviewer)
	require.Equal(t, 8, out[0].Score) // 3 + 4 + 1
	require.Len(t, out[0].Reasons, 3, "one reason per contributing signal")
}

func TestRankReviewers_RecentAuthorOutranksCoChange(t *testing.T) {
	authors := map[string]int{"alice": 1} // 2
	coChange := map[string]int{"bob": 1}  // 1
	out := rankReviewers(nil, authors, coChange, nil, nil)
	require.Len(t, out, 2)
	require.Equal(t, "alice", out[0].Reviewer)
	require.Equal(t, "bob", out[1].Reviewer)
}

func TestRankReviewers_TieBreaksOnName(t *testing.T) {
	// Equal scores → reviewer name ascending, deterministically.
	authors := map[string]int{"charlie": 1, "alice": 1, "bob": 1}
	out := rankReviewers(nil, authors, nil, nil, nil)
	require.Len(t, out, 3)
	require.Equal(t, []string{"alice", "bob", "charlie"},
		[]string{out[0].Reviewer, out[1].Reviewer, out[2].Reviewer})
}

func TestRankReviewers_EmptyInputs(t *testing.T) {
	require.Empty(t, rankReviewers(nil, nil, nil, nil, nil))
	require.Empty(t, rankReviewers(map[string]int{}, map[string]int{}, map[string]int{}, nil, nil))
	// Zero / empty-name entries are dropped, never panic.
	require.Empty(t, rankReviewers(map[string]int{"": 5, "x": 0}, nil, nil, nil, nil))
}

func TestRankReviewers_KindDefaultsAndTeam(t *testing.T) {
	codeowners := map[string]int{"org/platform": 1, "dave": 1}
	kinds := map[string]string{"org/platform": "team", "dave": "person"}
	out := rankReviewers(codeowners, nil, nil, kinds, nil)
	byName := map[string]ReviewerSuggestion{}
	for _, r := range out {
		byName[r.Reviewer] = r
	}
	require.Equal(t, "team", byName["org/platform"].Kind)
	require.Equal(t, "person", byName["dave"].Kind)

	// A reviewer with no explicit kind is classified from its handle.
	out2 := rankReviewers(nil, map[string]int{"a@b.com": 1, "team/x": 1}, nil, nil, nil)
	by2 := map[string]ReviewerSuggestion{}
	for _, r := range out2 {
		by2[r.Reviewer] = r
	}
	require.Equal(t, "person", by2["a@b.com"].Kind)
	require.Equal(t, "team", by2["team/x"].Kind)
}

// --- handler tests ----------------------------------------------------------

// reviewerTestServer builds a server over a synthetic graph with two functions
// authored by different people and a CODEOWNERS file mapping one of the files.
func reviewerTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	repoRoot := t.TempDir()

	// CODEOWNERS maps pkg/auth/** to the @org/secteam team.
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".github"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, ".github", "CODEOWNERS"),
		[]byte("pkg/auth/ @org/secteam\n"),
		0o644,
	))

	g := graph.New()
	authID := "pkg/auth/login.go::Login"
	g.AddNode(&graph.Node{
		ID: authID, Kind: graph.KindFunction, Name: "Login",
		FilePath: "pkg/auth/login.go", StartLine: 1, EndLine: 10,
		Meta: map[string]any{"last_authored": map[string]any{
			"email": "alice@example.com", "timestamp": int64(1_700_000_000), "commit": "abc",
		}},
	})
	utilID := "pkg/util/util.go::Helper"
	g.AddNode(&graph.Node{
		ID: utilID, Kind: graph.KindFunction, Name: "Helper",
		FilePath: "pkg/util/util.go", StartLine: 1, EndLine: 10,
		Meta: map[string]any{"last_authored": map[string]any{
			"email": "bob@example.com", "timestamp": int64(1_700_000_100), "commit": "def",
		}},
	})

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	// Wire a co-change link: login.go historically changes with util.go,
	// so util.go's author (bob) becomes a co-change expert.
	srv.cochangeByFile = map[string]map[string]float64{
		"pkg/auth/login.go": {"pkg/util/util.go": 0.8},
	}
	// Point the single indexer's root at the CODEOWNERS repo so the handler
	// resolves it. NewServer has no indexer here, so override collectRepoRoots
	// via the multiIndexer-less path: stash the root on a tiny test indexer.
	srv.indexer = rootOnlyIndexer(repoRoot)
	return srv, authID, repoRoot
}

func callSuggestReviewers(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "suggest_reviewers"
	req.Params.Arguments = args
	res, err := srv.handleSuggestReviewers(t.Context(), req)
	require.NoError(t, err)
	return res
}

func TestSuggestReviewers_Discoverable(t *testing.T) {
	// The tool is registered (eager when lazy tools are disabled — the test
	// default — so it lands on the live MCP server and tools_search seam).
	g := graph.New()
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	srv.registerSuggestReviewersTool()
	// No panic on registration; handler is callable directly.
	res := callSuggestReviewers(t, srv, map[string]any{})
	require.True(t, res.IsError, "no input → error, not panic")
}

func TestSuggestReviewers_RankedWithReasons(t *testing.T) {
	srv, authID, _ := reviewerTestServer(t)

	res := callSuggestReviewers(t, srv, map[string]any{"ids": authID})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Reviewers []struct {
			Reviewer     string   `json:"reviewer"`
			Kind         string   `json:"kind"`
			Score        int      `json:"score"`
			Reasons      []string `json:"reasons"`
			MatchedFiles []string `json:"matched_files"`
		} `json:"reviewers"`
		Total           int  `json:"total"`
		ChangedFiles    int  `json:"changed_files"`
		CodeownersFound bool `json:"codeowners_found"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	require.True(t, out.CodeownersFound)
	require.Equal(t, 1, out.ChangedFiles)
	require.NotEmpty(t, out.Reviewers)

	// org/secteam (CODEOWNERS, weight 3) must outrank alice (recent author,
	// weight 2) which must outrank bob (co-change expert, weight 1).
	require.Equal(t, "org/secteam", out.Reviewers[0].Reviewer)
	require.Equal(t, "team", out.Reviewers[0].Kind)
	require.NotEmpty(t, out.Reviewers[0].Reasons)
	require.Equal(t, []string{"pkg/auth/login.go"}, out.Reviewers[0].MatchedFiles)

	// Scores are strictly non-increasing.
	for i := 1; i < len(out.Reviewers); i++ {
		require.GreaterOrEqual(t, out.Reviewers[i-1].Score, out.Reviewers[i].Score)
	}

	// alice (author) ranks above bob (co-change).
	rank := map[string]int{}
	for i, r := range out.Reviewers {
		rank[r.Reviewer] = i
	}
	require.Contains(t, rank, "alice@example.com")
	require.Contains(t, rank, "bob@example.com")
	require.Less(t, rank["alice@example.com"], rank["bob@example.com"])
}

func TestSuggestReviewers_NoCodeowners(t *testing.T) {
	repoRoot := t.TempDir() // no CODEOWNERS file
	g := graph.New()
	id := "pkg/x/x.go::Foo"
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/x/x.go"})
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	srv.indexer = rootOnlyIndexer(repoRoot)

	res := callSuggestReviewers(t, srv, map[string]any{"ids": id})
	require.False(t, res.IsError)
	var out struct {
		CodeownersFound bool `json:"codeowners_found"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	require.False(t, out.CodeownersFound)
}

func TestSuggestReviewers_GCXAndTOONAndBudget(t *testing.T) {
	srv, authID, _ := reviewerTestServer(t)

	// GCX round-trip: the section headers must appear.
	gcx := callSuggestReviewers(t, srv, map[string]any{"ids": authID, "format": "gcx"})
	require.False(t, gcx.IsError)
	gtext := gcx.Content[0].(mcplib.TextContent).Text
	require.Contains(t, gtext, "suggest_reviewers.summary")
	require.Contains(t, gtext, "suggest_reviewers.reviewers")
	require.Contains(t, gtext, "secteam")

	// max_bytes budget is honoured (response stays bounded).
	budgeted := callSuggestReviewers(t, srv, map[string]any{"ids": authID, "format": "gcx", "max_bytes": float64(120)})
	require.False(t, budgeted.IsError)
	require.LessOrEqual(t, len(budgeted.Content[0].(mcplib.TextContent).Text), 400)

	// TOON round-trip: still carries the reviewers key.
	toon := callSuggestReviewers(t, srv, map[string]any{"ids": authID, "format": "toon"})
	require.False(t, toon.IsError)
	require.Contains(t, toon.Content[0].(mcplib.TextContent).Text, "reviewer")
}

// TestSuggestReviewers_IdsOnPrefixedGraphKeepsAllSignals pins the path domain
// of the ids branch. resolveReviewerChangeset's ids case returns graph node
// FilePaths, which are already graph-keyed; base and number return
// repo-relative paths. Forcing one domain on all three prefixes the ids
// entries a second time on a multi-repo graph — `repo-a/pkg/auth/login.go`
// looked up as `repo-a/repo-a/pkg/auth/login.go` — and the ownership and
// co-change signals silently disappear while CODEOWNERS (which matches on the
// repo-relative spelling) still answers, so the tool looks healthy.
//
// The existing ids tests build an unprefixed graph, where both domains
// coincide and the regression is invisible.
func TestSuggestReviewers_IdsOnPrefixedGraphKeepsAllSignals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo-a")
	write := func(rel, src string) {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}
	write("pkg/auth/login.go", "package auth\n\nfunc Login() error { return nil }\n")
	write("pkg/util/util.go", "package util\n\nfunc Helper() error { return nil }\n")
	// Root-anchored on purpose: `/pkg/auth/` matches only the repo-relative
	// spelling. An unanchored `pkg/auth/` rule would also match
	// `repo-a/pkg/auth/login.go`, masking a graph-keyed path reaching
	// CODEOWNERS in ids mode.
	write(".github/CODEOWNERS", "/pkg/auth/ @org/secteam\n")

	srv, prefix := prefixedServerOver(t, dir, "repo-a")
	require.Equal(t, "repo-a", prefix)

	nodeIDNamed := func(name string) (id, file string) {
		t.Helper()
		for _, n := range srv.graph.FindNodesByName(name) {
			if n != nil && n.Kind == graph.KindFunction {
				return n.ID, n.FilePath
			}
		}
		t.Fatalf("fixture did not index %q", name)
		return "", ""
	}
	loginID, loginFile := nodeIDNamed("Login")
	helperID, helperFile := nodeIDNamed("Helper")

	// Both file paths are graph keys, so they carry the repo prefix. That is
	// exactly what the ids branch hands on.
	require.True(t, strings.HasPrefix(loginFile, prefix+"/"), "fixture must be prefixed, got %q", loginFile)

	stampAuthor := func(id, email string, ts int64) {
		n := srv.graph.GetNode(id)
		require.NotNil(t, n, "node %q missing", id)
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["last_authored"] = map[string]any{
			"email": email, "timestamp": ts, "commit": "deadbee",
		}
	}
	stampAuthor(loginID, "alice@example.com", 1_700_000_000)
	stampAuthor(helperID, "bob@example.com", 1_700_000_100)

	// login.go historically changes with util.go, so util.go's author is a
	// co-change expert. The co-change index is keyed by graph path.
	srv.cochangeByFile = map[string]map[string]float64{
		loginFile: {helperFile: 0.8},
	}

	res := callSuggestReviewers(t, srv, map[string]any{"ids": loginID, "repo": dir})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Reviewers []struct {
			Reviewer string   `json:"reviewer"`
			Kind     string   `json:"kind"`
			Reasons  []string `json:"reasons"`
		} `json:"reviewers"`
		CodeownersFound bool `json:"codeowners_found"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	seen := map[string]bool{}
	for _, r := range out.Reviewers {
		seen[r.Reviewer] = true
	}
	require.True(t, out.CodeownersFound, "CODEOWNERS must still resolve: %+v", out)
	require.True(t, seen["org/secteam"], "codeowner signal missing: %+v", out.Reviewers)
	require.True(t, seen["alice@example.com"],
		"recent-author signal missing — the ids branch is graph-keyed and must not be prefixed twice: %+v", out.Reviewers)
	require.True(t, seen["bob@example.com"],
		"co-change signal missing — the ids branch is graph-keyed and must not be prefixed twice: %+v", out.Reviewers)
}
