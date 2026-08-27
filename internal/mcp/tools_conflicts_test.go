package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// --- communityConflicts: the pure grouping core ----------------------------

func TestCommunityConflicts_TwoPRsSharingOneCommunity(t *testing.T) {
	groups := communityConflicts(
		map[int][]string{
			1: {"c1"},
			2: {"c1"},
		},
		map[string]int{"c1": 4},
		map[int]float64{1: 10, 2: 3},
	)
	require.Len(t, groups, 1, "two PRs sharing a community → exactly one conflict cluster")
	g := groups[0]
	require.Equal(t, "c1", g.Community)
	require.Equal(t, 4, g.Size)
	require.Equal(t, []int{1, 2}, g.PRs, "colliding PRs ascending")
	// Suggested order: lowest-risk PR first. #2 (risk 3) before #1 (risk 10).
	require.Equal(t, []int{2, 1}, g.SuggestedOrder)
	require.Greater(t, g.Risk, 0.0)
}

func TestCommunityConflicts_DisjointPRsProduceNoConflict(t *testing.T) {
	groups := communityConflicts(
		map[int][]string{
			1: {"c1"},
			2: {"c2"},
			3: {"c3"},
		},
		map[string]int{"c1": 4, "c2": 5, "c3": 6},
		map[int]float64{1: 1, 2: 2, 3: 3},
	)
	require.Empty(t, groups, "no community is touched by >1 PR → no clusters")
}

func TestCommunityConflicts_ThreePRsOutrankTwoPRs(t *testing.T) {
	groups := communityConflicts(
		map[int][]string{
			// c-busy touched by 3 PRs.
			1: {"c-busy", "c-pair"},
			2: {"c-busy"},
			3: {"c-busy", "c-pair"},
		},
		// c-pair is far larger, to prove PR-count dominates size.
		map[string]int{"c-busy": 2, "c-pair": 999},
		map[int]float64{1: 5, 2: 5, 3: 5},
	)
	require.Len(t, groups, 2, "both shared communities reported")
	// The 3-PR community ranks first even though it is much smaller.
	require.Equal(t, "c-busy", groups[0].Community)
	require.Equal(t, []int{1, 2, 3}, groups[0].PRs)
	require.Equal(t, "c-pair", groups[1].Community)
	require.Equal(t, []int{1, 3}, groups[1].PRs)
	require.Greater(t, groups[0].Risk, groups[1].Risk, "more colliding PRs → higher risk")
}

func TestCommunityConflicts_EqualPRCountBreaksOnSize(t *testing.T) {
	groups := communityConflicts(
		map[int][]string{
			1: {"small", "big"},
			2: {"small", "big"},
		},
		map[string]int{"small": 3, "big": 50},
		map[int]float64{1: 1, 2: 2},
	)
	require.Len(t, groups, 2)
	// Same PR count (2 each) → larger community ranks first.
	require.Equal(t, "big", groups[0].Community)
	require.Equal(t, "small", groups[1].Community)
	require.Greater(t, groups[0].Risk, groups[1].Risk)
}

func TestCommunityConflicts_SuggestedOrderTieBreaksOnNumber(t *testing.T) {
	groups := communityConflicts(
		map[int][]string{
			5: {"c1"},
			2: {"c1"},
			9: {"c1"},
		},
		map[string]int{"c1": 1},
		// Equal risk → suggested order is PR number ascending.
		map[int]float64{2: 4, 5: 4, 9: 4},
	)
	require.Len(t, groups, 1)
	require.Equal(t, []int{2, 5, 9}, groups[0].PRs)
	require.Equal(t, []int{2, 5, 9}, groups[0].SuggestedOrder)
}

func TestCommunityConflicts_EmptyCommunityIDsIgnored(t *testing.T) {
	groups := communityConflicts(
		map[int][]string{
			1: {"", "c1"},
			2: {"", "c1"},
		},
		map[string]int{"c1": 2},
		map[int]float64{1: 1, 2: 2},
	)
	require.Len(t, groups, 1, "blank community ids must not form a phantom cluster")
	require.Equal(t, "c1", groups[0].Community)
}

// --- handler: graph join + forge seam --------------------------------------

// conflictsTestServer builds a server whose graph has two distinct hub
// files, each defining one symbol, both placed (via a synthetic
// CommunityResult) into the SAME community. Two PRs that each touch one of
// the files therefore collide in that community. Returns the server and
// the two file paths.
func conflictsTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	g := graph.New()
	// The returned names are the forge spelling a caller supplies; the graph
	// keys use native separators, which is what the indexer writes even with
	// no repo prefix.
	fileA := "internal/a/svc.go"
	fileB := "internal/b/svc.go"
	storedA := filepath.FromSlash(fileA)
	storedB := filepath.FromSlash(fileB)
	idA := storedA + "::DoA"
	idB := storedB + "::DoB"
	g.AddNode(&graph.Node{ID: idA, Kind: graph.KindFunction, Name: "DoA", FilePath: storedA, StartLine: 1, EndLine: 5})
	g.AddNode(&graph.Node{ID: idB, Kind: graph.KindFunction, Name: "DoB", FilePath: storedB, StartLine: 1, EndLine: 5})

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	installCommunitiesForTest(srv, &analysis.CommunityResult{
		Communities: []analysis.Community{
			{ID: "comm-shared", Size: 7, Members: []string{idA, idB}},
		},
		NodeToComm: map[string]string{idA: "comm-shared", idB: "comm-shared"},
	})
	return srv, fileA, fileB
}

func TestConflictsPRs_SuppliedDataYieldsCluster(t *testing.T) {
	srv, fileA, fileB := conflictsTestServer(t)

	// Both seams fail the test if hit — supplied data must short-circuit.
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)

	prs := []forge.PR{{Number: 1, Title: "a", Author: "x"}, {Number: 2, Title: "b", Author: "y"}}
	prsJSON, _ := json.Marshal(prs)
	filesJSON, _ := json.Marshal(map[string][]string{
		"1": {fileA},
		"2": {fileB},
	})

	res := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{
		"prs":   string(prsJSON),
		"files": string(filesJSON),
	})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Total     int `json:"total"`
		Conflicts []struct {
			Community      string  `json:"community"`
			Size           int     `json:"size"`
			PRs            []int   `json:"prs"`
			SuggestedOrder []int   `json:"suggested_order"`
			Risk           float64 `json:"risk"`
		} `json:"conflicts"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, 1, out.Total, "the two PRs collide in exactly one community")
	require.Len(t, out.Conflicts, 1)
	c := out.Conflicts[0]
	require.Equal(t, "comm-shared", c.Community)
	require.Equal(t, 7, c.Size)
	require.Equal(t, []int{1, 2}, c.PRs)
	require.Len(t, c.SuggestedOrder, 2)
	require.Greater(t, c.Risk, 0.0)
}

func TestConflictsPRs_DisjointPRsNoCluster(t *testing.T) {
	srv, fileA, _ := conflictsTestServer(t)
	// Only PR #1 touches a known file; PR #2 touches an unindexed file with
	// no symbols → no community → no overlap.
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)
	prsJSON, _ := json.Marshal([]forge.PR{{Number: 1}, {Number: 2}})
	filesJSON, _ := json.Marshal(map[string][]string{
		"1": {fileA},
		"2": {"unindexed/none.go"},
	})
	res := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{
		"prs":   string(prsJSON),
		"files": string(filesJSON),
	})
	require.False(t, res.IsError)
	var out struct {
		Total     int              `json:"total"`
		Conflicts []map[string]any `json:"conflicts"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, 0, out.Total, "no shared community → no clusters")
	require.Empty(t, out.Conflicts)
}

func TestConflictsPRs_ForgeUnavailable(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")

	srv, _, _ := conflictsTestServer(t)
	seamHit := false
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { seamHit = true; return nil, nil },
		func(context.Context, string, int) ([]string, error) { seamHit = true; return nil, nil },
	)
	res := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{})
	require.False(t, res.IsError, "must degrade, not error")
	var out struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, "forge unavailable", out.Error)
	require.Contains(t, out.Hint, "GH_TOKEN")
	require.False(t, seamHit, "an unavailable forge must short-circuit before the seam")
}

// conflictsBudgetServer builds a graph with several shared communities so
// the conflicts output has multiple rows — enough of a tail for the GCX
// budget trimmer to bite. Three communities, each touched by two PRs.
func conflictsBudgetServer(t *testing.T) (*Server, map[string][]string, []forge.PR) {
	t.Helper()
	g := graph.New()
	nodeToComm := map[string]string{}
	var members3 [3][]string
	files := map[string][]string{} // PR number → files
	prs := []forge.PR{}
	for c := 0; c < 3; c++ {
		comm := "comm" + string(rune('A'+c))
		// Two PRs per community, each touching its own file in that community.
		for p := 0; p < 2; p++ {
			prNum := c*2 + p + 1
			file := "internal/x/" + comm + "_" + string(rune('a'+p)) + ".go"
			// Same split as conflictsTestServer: supplied in the forge
			// spelling, stored with native separators.
			stored := filepath.FromSlash(file)
			id := stored + "::F"
			g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "F", FilePath: stored, StartLine: 1, EndLine: 3})
			nodeToComm[id] = comm
			members3[c] = append(members3[c], id)
			files[strconv.Itoa(prNum)] = []string{file}
			prs = append(prs, forge.PR{Number: prNum, Title: "p" + strconv.Itoa(prNum)})
		}
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	comms := make([]analysis.Community, 3)
	for c := 0; c < 3; c++ {
		comms[c] = analysis.Community{ID: "comm" + string(rune('A'+c)), Size: 5 + c, Members: members3[c]}
	}
	installCommunitiesForTest(srv, &analysis.CommunityResult{Communities: comms, NodeToComm: nodeToComm})
	return srv, files, prs
}

func TestConflictsPRs_GCXTOONBudget(t *testing.T) {
	srv, files, prs := conflictsBudgetServer(t)
	prsJSON, _ := json.Marshal(prs)
	filesJSON, _ := json.Marshal(files)

	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)

	// GCX round-trip carries both the summary and conflicts sections.
	g := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{
		"prs": string(prsJSON), "files": string(filesJSON), "format": "gcx",
	})
	require.False(t, g.IsError)
	gtext := g.Content[0].(mcplib.TextContent).Text
	require.Contains(t, gtext, "conflicts_prs.summary")
	require.Contains(t, gtext, "conflicts_prs.conflicts")

	// TOON round-trip keeps a known key.
	tn := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{
		"prs": string(prsJSON), "files": string(filesJSON), "format": "toon",
	})
	require.False(t, tn.IsError)
	require.Contains(t, tn.Content[0].(mcplib.TextContent).Text, "total")

	// max_bytes budget trims the GCX response below the unbudgeted size:
	// the three-cluster tail gives the row trimmer something to drop.
	full := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{
		"prs": string(prsJSON), "files": string(filesJSON), "format": "gcx",
	})
	require.False(t, full.IsError)
	b := callPRTool(t, srv, "conflicts_prs", srv.handleConflictsPRs, map[string]any{
		"prs": string(prsJSON), "files": string(filesJSON), "format": "gcx", "max_bytes": float64(150),
	})
	require.False(t, b.IsError)
	require.Less(t, len(b.Content[0].(mcplib.TextContent).Text), len(full.Content[0].(mcplib.TextContent).Text),
		"max_bytes must trim the response below the unbudgeted size")
}

func TestConflictsPRs_DiscoverableViaToolsSearch(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.NotNil(t, srv.lazy)

	hits := srv.lazy.Query("select:conflicts_prs", 1)
	require.Len(t, hits, 1, "conflicts_prs must be discoverable by exact name")
	require.Equal(t, "conflicts_prs", hits[0].tool.Name)
	require.False(t, hotEagerTools["conflicts_prs"], "conflicts_prs must be a deferred tool")
}
