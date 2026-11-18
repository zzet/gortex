package mcp

import (
	"context"
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
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// callPRReviewContext invokes the registered pr_review_context tool through
// the full dispatch path and asserts no transport error.
func callPRReviewContext(t *testing.T, srv *Server, ctx context.Context, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	return callToolByName(t, srv, ctx, "pr_review_context", args)
}

// prReviewOut is the decoded JSON envelope.
type prReviewOut struct {
	Verdict        string   `json:"verdict"`
	ChangedSymbols int      `json:"changed_symbols"`
	ChangedFiles   []string `json:"changed_files"`
	Gates          []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"gates"`
	DiffContext []struct {
		ID      string   `json:"id"`
		Risk    string   `json:"risk"`
		Callers []string `json:"callers"`
	} `json:"diff_context"`
	Verify *struct {
		Clean      bool `json:"clean"`
		Violations []struct {
			SymbolID string `json:"symbol_id"`
			Kind     string `json:"kind"`
		} `json:"violations"`
	} `json:"verify"`
	Simulation *struct {
		Ran            bool   `json:"ran"`
		Note           string `json:"note"`
		GraphUntouched bool   `json:"graph_untouched"`
		TotalSteps     int    `json:"total_steps"`
		SessionID      string `json:"session_id"`
	} `json:"simulation"`
	ConfigAudit map[string]any `json:"config_audit"`
}

func decodePRReview(t *testing.T, res *mcplib.CallToolResult) prReviewOut {
	t.Helper()
	require.False(t, res.IsError, "errored: %s", toolText(res))
	var out prReviewOut
	require.NoError(t, json.Unmarshal([]byte(toolText(res)), &out))
	return out
}

func gateStatus(out prReviewOut, name string) (string, bool) {
	for _, g := range out.Gates {
		if g.Name == name {
			return g.Status, true
		}
	}
	return "", false
}

// TestPRReviewContext_ToolRegistered confirms the tool wired up.
func TestPRReviewContext_ToolRegistered(t *testing.T) {
	dir, _, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)
	require.NotNil(t, srv.MCPServer().GetTool("pr_review_context"),
		"pr_review_context must be registered")
}

// TestPRReviewContext_AllSections runs the rollup over a real changeset: the
// diff_context section is populated, the audit section is present, and the
// verdict is PASS (the body-only edits break no callers).
func TestPRReviewContext_AllSections(t *testing.T) {
	dir, fileA, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodePRReview(t, callPRReviewContext(t, srv, context.Background(), map[string]any{
		"base": "base-ref",
	}))

	require.Greater(t, out.ChangedSymbols, 0, "the changeset has changed symbols")
	require.NotEmpty(t, out.DiffContext, "diff_context section is populated")
	require.NotEmpty(t, out.ChangedFiles)
	require.Contains(t, out.ChangedFiles, fileA)

	// verify_change ran (signature-bearing symbols) and found nothing broken.
	require.NotNil(t, out.Verify, "verify section present for signature-bearing changes")
	require.True(t, out.Verify.Clean, "body-only edits break no callers")

	// audit section is always present (gate row + config_audit block).
	_, hasAudit := gateStatus(out, "audit_agent_config")
	require.True(t, hasAudit, "audit gate present")

	// diff_context gate present.
	_, hasDC := gateStatus(out, "diff_context")
	require.True(t, hasDC)

	require.Equal(t, prReviewPass, out.Verdict, "clean changeset is PASS")
}

// TestPRReviewContext_EmptyDiff: a working tree with no diff yields a PASS
// verdict and zero changed symbols.
func TestPRReviewContext_EmptyDiff(t *testing.T) {
	dir, _, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	// scope=staged with nothing staged => empty diff.
	out := decodePRReview(t, callPRReviewContext(t, srv, context.Background(), map[string]any{
		"scope": "staged",
	}))
	require.Equal(t, 0, out.ChangedSymbols)
	require.Equal(t, prReviewPass, out.Verdict)
}

// TestPRReviewContext_DiffsTheWorktreeView: a request routed to a linked
// worktree's view diffs that worktree's working copy, not the tracked root.
func TestPRReviewContext_DiffsTheWorktreeView(t *testing.T) {
	// Given a tracked repo with a clean working tree, and a linked worktree
	// outside it whose working copy edits fileB only.
	dir, _, fileB, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)
	worktree := filepath.Join(t.TempDir(), "wt")
	runGitT(t, dir, "worktree", "add", "--detach", worktree, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, filepath.FromSlash(fileB)),
		[]byte("package alpha\n\nfunc Beta() int {\n\treturn 4\n}\n"), 0o644))

	// When pr_review_context runs the default unstaged diff through that view.
	ctx := withRequestView(t.Context(), &requestView{kind: requestViewKindWorktree, viewRoot: worktree, reader: srv.graph})
	req := mcplib.CallToolRequest{}
	req.Params.Name = "pr_review_context"
	req.Params.Arguments = map[string]any{"sections": "diff_context"}
	res, err := srv.handlePRReviewContext(ctx, req)
	require.NoError(t, err)

	// Then the changeset is the worktree's edit; the tracked root has none.
	require.Equal(t, []string{fileB}, decodePRReview(t, res).ChangedFiles)
}

// TestPRReviewContext_WorktreeViewKeepsTheRepoPrefix: in a multi-repo store
// the worktree's diff joins graph nodes under the tracked repo's prefix, even
// though the worktree sits outside every tracked root.
func TestPRReviewContext_WorktreeViewKeepsTheRepoPrefix(t *testing.T) {
	// Given a repo tracked under the prefix "repo" and a linked worktree
	// outside it whose working copy edits Beta's body.
	dir, _, fileB, _ := siblingDiffGitRepo(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, testRegistry(), search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().ReposSnapshot() {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{ConfigManager: cm, MultiIndexer: mi})
	worktree := filepath.Join(t.TempDir(), "wt")
	runGitT(t, dir, "worktree", "add", "--detach", worktree, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, filepath.FromSlash(fileB)),
		[]byte("package alpha\n\nfunc Beta() int {\n\treturn 4\n}\n"), 0o644))

	// When pr_review_context diffs through a view of that worktree.
	ctx := withRequestView(t.Context(), &requestView{
		kind:         requestViewKindWorktree,
		viewRoot:     worktree,
		reader:       g,
		materialized: &graphview.RepoView{ID: graphview.RepoViewID{RepoPrefix: "repo"}},
	})
	req := mcplib.CallToolRequest{}
	req.Params.Name = "pr_review_context"
	req.Params.Arguments = map[string]any{"repo": "repo", "sections": "diff_context"}
	res, err := srv.handlePRReviewContext(ctx, req)
	require.NoError(t, err)

	// Then Beta's change maps to its graph node under "repo/".
	out := decodePRReview(t, res)
	ids := make([]string, 0, len(out.DiffContext))
	for _, sym := range out.DiffContext {
		ids = append(ids, sym.ID)
	}
	require.Contains(t, ids, "repo/"+fileB+"::Beta")
}

// TestPRReviewContext_SectionsFilter: a `sections` filter drops the
// unrequested blocks.
func TestPRReviewContext_SectionsFilter(t *testing.T) {
	dir, _, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodePRReview(t, callPRReviewContext(t, srv, context.Background(), map[string]any{
		"base":     "base-ref",
		"sections": "diff_context",
	}))
	require.NotEmpty(t, out.DiffContext, "requested diff_context present")
	require.Nil(t, out.Verify, "verify omitted when not in sections")
	require.Nil(t, out.ConfigAudit, "config_audit omitted when not in sections")
	_, hasVerifyGate := gateStatus(out, "verify_change")
	require.False(t, hasVerifyGate, "no verify gate when section filtered out")
}

// buildInterfaceBreakGraph builds a graph with an interface Speaker and two
// implementors Dog (Speak(loud bool)) and Cat (Speak() — mismatching arity).
// Returns the server and the changed method's ID (Dog.Speak). Verifying
// Dog.Speak's surface must flag Cat.Speak as a broken implementor.
func buildInterfaceBreakGraph(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()

	add := func(id, name string, kind graph.NodeKind, sig string) {
		n := &graph.Node{ID: id, Name: name, Kind: kind, FilePath: pathOf(id), Language: "go"}
		if sig != "" {
			n.Meta = map[string]any{"signature": sig}
		}
		g.AddNode(n)
	}
	add("pkg/iface.go::Speaker", "Speaker", graph.KindInterface, "")
	add("pkg/dog.go::Dog", "Dog", graph.KindType, "")
	add("pkg/cat.go::Cat", "Cat", graph.KindType, "")
	add("pkg/dog.go::Dog.Speak", "Speak", graph.KindMethod, "func (d Dog) Speak(loud bool)")
	add("pkg/cat.go::Cat.Speak", "Speak", graph.KindMethod, "func (c Cat) Speak()")

	// method → type (member_of); type → interface (implements).
	g.AddEdge(&graph.Edge{From: "pkg/dog.go::Dog.Speak", To: "pkg/dog.go::Dog", Kind: graph.EdgeMemberOf})
	g.AddEdge(&graph.Edge{From: "pkg/cat.go::Cat.Speak", To: "pkg/cat.go::Cat", Kind: graph.EdgeMemberOf})
	g.AddEdge(&graph.Edge{From: "pkg/dog.go::Dog", To: "pkg/iface.go::Speaker", Kind: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: "pkg/cat.go::Cat", To: "pkg/iface.go::Speaker", Kind: graph.EdgeImplements})

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	return srv, "pkg/dog.go::Dog.Speak"
}

func pathOf(id string) string {
	if i := strings.Index(id, "::"); i >= 0 {
		return id[:i]
	}
	return id
}

// TestPRReviewContext_VerifyBrokenImplementor: when a changed interface
// method's surface no longer matches a sibling implementor, the verify
// section reports the broken implementor and the verdict is BLOCK. This
// exercises VerifyChanges threaded with s.engine.
func TestPRReviewContext_VerifyBrokenImplementor(t *testing.T) {
	srv, changedID := buildInterfaceBreakGraph(t)

	out := decodePRReview(t, callPRReviewContext(t, srv, context.Background(), map[string]any{
		"ids":          changedID,
		"audit_config": false, // no working tree
	}))

	require.NotNil(t, out.Verify, "verify section present")
	require.False(t, out.Verify.Clean, "a broken implementor makes the verify result dirty")
	require.NotEmpty(t, out.Verify.Violations, "Cat.Speak is a broken implementor")
	var sawCat bool
	for _, v := range out.Verify.Violations {
		if v.SymbolID == "pkg/cat.go::Cat.Speak" {
			sawCat = true
		}
	}
	require.True(t, sawCat, "the mismatching sibling implementor is reported")

	status, ok := gateStatus(out, "verify_change")
	require.True(t, ok)
	require.Equal(t, prReviewBlock, status)
	require.Equal(t, prReviewBlock, out.Verdict, "a broken implementor blocks")
}

// TestPRReviewContext_SimulateOmittedWithoutSession: edits supplied but no
// overlay session id => the simulation section is omitted-with-note (no
// panic), and the base graph is left untouched.
func TestPRReviewContext_SimulateOmittedWithoutSession(t *testing.T) {
	srv, dir, targetFile, _ := setupOverlayServer(t)
	before := baseNodeIDs(srv)

	edits := "[" + buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\nfunc Added() {}\n") + "]"

	out := decodePRReview(t, callPRReviewContext(t, srv, context.Background(), map[string]any{
		"repo":         dir,
		"scope":        "staged", // empty diff is fine; we exercise the simulate gate
		"edits":        edits,
		"sections":     "simulate",
		"audit_config": false,
	}))

	require.NotNil(t, out.Simulation, "simulation section present (omitted-with-note form)")
	require.False(t, out.Simulation.Ran, "no session => simulation does not run")
	require.NotEmpty(t, out.Simulation.Note, "a clear note explains the omission")
	require.Contains(t, out.Simulation.Note, "session")
	require.True(t, out.Simulation.GraphUntouched)

	status, ok := gateStatus(out, "simulate_chain")
	require.True(t, ok)
	require.Equal(t, prReviewPass, status, "a skipped simulation does not block")

	require.Equal(t, before, baseNodeIDs(srv),
		"base graph node set is unchanged by the (skipped) simulation")
}

// TestPRReviewContext_SimulateRunsWithSession: with an explicit overlay
// session id the simulation runs, and the base graph is still left
// untouched.
func TestPRReviewContext_SimulateRunsWithSession(t *testing.T) {
	srv, dir, targetFile, _ := setupOverlayServer(t)
	before := baseNodeIDs(srv)

	sessID := "pr-review-sim"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))

	edits := "[" + buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\nfunc Added() {}\n") + "]"

	// Drive via the explicit session_id param (not the context) to prove the
	// param is the gate.
	out := decodePRReview(t, callPRReviewContext(t, srv, context.Background(), map[string]any{
		"repo":         dir,
		"scope":        "staged",
		"edits":        edits,
		"session_id":   sessID,
		"sections":     "simulate",
		"audit_config": false,
	}))

	require.NotNil(t, out.Simulation)
	require.True(t, out.Simulation.Ran, "an explicit session id enables the simulation")
	require.Equal(t, sessID, out.Simulation.SessionID)
	require.GreaterOrEqual(t, out.Simulation.TotalSteps, 1)
	require.True(t, out.Simulation.GraphUntouched)

	require.Equal(t, before, baseNodeIDs(srv),
		"base graph node set is unchanged after a kept-false simulation")
}

// TestPRReviewContext_SimulateUsesOverlayCohort: with no `session_id`
// param, the simulation falls back to the overlay cohort on the context
// (X-Gortex-Overlay-Session), not the caller's real transport session; an
// explicit `session_id` still wins over the cohort.
func TestPRReviewContext_SimulateUsesOverlayCohort(t *testing.T) {
	srv, dir, targetFile, _ := setupOverlayServer(t)

	cohort := "pr-review-cohort"
	explicit := "pr-review-explicit"
	require.NoError(t, srv.OverlayManager().RegisterWithID(cohort, ""))
	require.NoError(t, srv.OverlayManager().RegisterWithID(explicit, ""))
	ctx := WithOverlayCohortID(WithSessionID(context.Background(), "real-transport-session"), cohort)

	edits := "[" + buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\nfunc Added() {}\n") + "]"
	args := map[string]any{
		"repo":         dir,
		"scope":        "staged",
		"edits":        edits,
		"sections":     "simulate",
		"audit_config": false,
	}

	out := decodePRReview(t, callPRReviewContext(t, srv, ctx, args))
	require.NotNil(t, out.Simulation)
	require.True(t, out.Simulation.Ran)
	require.Equal(t, cohort, out.Simulation.SessionID)

	args["session_id"] = explicit
	out = decodePRReview(t, callPRReviewContext(t, srv, ctx, args))
	require.NotNil(t, out.Simulation)
	require.True(t, out.Simulation.Ran)
	require.Equal(t, explicit, out.Simulation.SessionID)
}

// TestPRReviewContext_GCXAndBudget: the gcx, toon, and max_bytes paths all
// round-trip.
func TestPRReviewContext_GCXAndBudget(t *testing.T) {
	dir, _, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	t.Run("gcx", func(t *testing.T) {
		res := callPRReviewContext(t, srv, context.Background(), map[string]any{
			"base":   "base-ref",
			"format": "gcx",
		})
		require.False(t, res.IsError, "%s", toolText(res))
		txt := toolText(res)
		require.Contains(t, txt, "pr_review_context.summary")
		require.Contains(t, txt, "pr_review_context.gates")
	})

	t.Run("toon", func(t *testing.T) {
		res := callPRReviewContext(t, srv, context.Background(), map[string]any{
			"base":   "base-ref",
			"format": "toon",
		})
		require.False(t, res.IsError, "%s", toolText(res))
		require.NotEmpty(t, toolText(res))
	})

	t.Run("max_bytes", func(t *testing.T) {
		res := callPRReviewContext(t, srv, context.Background(), map[string]any{
			"base":      "base-ref",
			"max_bytes": 400,
		})
		require.False(t, res.IsError, "%s", toolText(res))
		// A tight budget still returns a parseable result (verdict is on the
		// first/summary block, never trimmed).
		require.NotEmpty(t, toolText(res))
	})
}
