package mcp

// Tests for the uniform repo/project/workspace/scope narrowing added to
// the `analyze` MCP tool. They mirror the fixtures and assertion shapes
// of scope_resolve_test.go, tools_search_text_test.go and
// workspace_isolation_test.go.
//
// Substring checks on the response text ("repo-a" / "repo-b") are a
// format-agnostic leak probe: multi-repo node IDs and file paths are
// prefixed with the configured repo name, so a result narrowed to
// repo-a must never contain the string "repo-b" (and vice versa). The
// fixtures use repo dir / config names "repo-a" and "repo-b" exactly so
// this probe works across every analyze kind regardless of its row
// schema or wire format.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// ─── fixtures ───────────────────────────────────────────────────────────────

// analyzeRepoSpec declares one repo for newAnalyzeServer: its config
// name, workspace + project slugs (project omitted from the .gortex.yaml
// when ""), and the main.go body.
type analyzeRepoSpec struct {
	name      string
	workspace string
	project   string
	body      string
}

// newAnalyzeServer indexes the given repos into one multi-repo graph and
// returns a *Server plus a name→absolute-path map (the path doubles as
// the session CWD anchor). It mirrors newSharedWorkspaceServer's wiring
// but lets each test shape the repos/bodies it needs — the shared
// fixtures only carry exported leaf functions, which the structural
// kinds (dead_code/hotspots/cycles) deliberately exclude.
func newAnalyzeServer(t *testing.T, flagOn bool, repos ...analyzeRepoSpec) (*Server, map[string]string) {
	t.Helper()

	paths := map[string]string{}
	entries := make([]config.RepoEntry, 0, len(repos))
	for _, r := range repos {
		dir := filepath.Join(t.TempDir(), r.name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		yaml := "workspace: " + r.workspace + "\n"
		if r.project != "" {
			yaml += "project: " + r.project + "\n"
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(yaml), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(r.body), 0o644))
		paths[r.name] = dir
		entries = append(entries, config.RepoEntry{Path: dir, Name: r.name, Project: r.project})
	}

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: entries}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	reg := testRegistry()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, reg, bm, cm, zap.NewNop())
	registerMCPFixtureRepos(t, mi, cm)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)

	flagVal := flagOn
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:        mi,
		ConfigManager:       cm,
		ScopeIntentDefaults: &flagVal,
	})
	return srv, paths
}

// structuralBody returns a package-main body whose functions exercise the
// structural analyze kinds: <p>Hub gains fan-in (hotspot candidate),
// <p>One/<p>Two/<p>Three/<p>Dead are unreferenced + unexported (dead
// code), and <p>MutualX/<p>MutualY form a call cycle. p is a per-repo
// prefix so symbol names never collide across repos.
func structuralBody(p string) string {
	return fmt.Sprintf(`package main

func %[1]sHub() {}
func %[1]sOne() { %[1]sHub() }
func %[1]sTwo() { %[1]sHub() }
func %[1]sThree() { %[1]sHub() }
func %[1]sDead() {}
func %[1]sMutualX() { %[1]sMutualY() }
func %[1]sMutualY() { %[1]sMutualX() }
`, p)
}

// newStructuralWorkspaceServer builds a two-repo single-workspace
// ("shared-ws", project "backend") server whose repos carry the
// structural body — so dead_code / hotspots / cycles actually emit rows.
func newStructuralWorkspaceServer(t *testing.T, flagOn bool) (*Server, map[string]string) {
	t.Helper()
	return newAnalyzeServer(t, flagOn,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", project: "backend", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", project: "backend", body: structuralBody("b")},
	)
}

// ─── handler helper ──────────────────────────────────────────────────────────

// runAnalyze drives handleAnalyze and returns the (non-error) result
// text, the scope_applied meta value, and the raw result.
func runAnalyze(t *testing.T, srv *Server, ctx context.Context, args map[string]any) (string, string, *mcplib.CallToolResult) {
	t.Helper()
	res, err := srv.handleAnalyze(ctx, makeReq("analyze", args))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "analyze %v errored: %s", args, toolResultText(res))
	require.NotNil(t, res.Meta, "every analyze response must be decorated with scope meta")
	applied, _ := res.Meta.AdditionalFields["scope_applied"].(string)
	return toolResultText(res), applied, res
}

// ─── 1. AUTO narrowing (health_score routes through scopedNodesByKinds) ──────

func TestAnalyzeScope_HealthScore_RepoNarrows(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	// No narrowing arg → whole (bound) workspace: both repos visible.
	text, applied, _ := runAnalyze(t, fx.srv, ctx, map[string]any{"kind": "health_score"})
	assert.Contains(t, text, "repo-a", "unnarrowed health_score must include repo-a")
	assert.Contains(t, text, "repo-b", "unnarrowed health_score must include repo-b (whole workspace)")
	assert.Equal(t, "workspace", applied)

	// repo=repo-a → rows only from repo-a.
	text, applied, _ = runAnalyze(t, fx.srv, ctx, map[string]any{"kind": "health_score", "repo": "repo-a"})
	assert.Contains(t, text, "repo-a", "narrowed health_score must include repo-a rows")
	assert.NotContains(t, text, "repo-b", "narrowed health_score must not leak repo-b rows")
	assert.Equal(t, "repo:repo-a", applied)
}

func TestAnalyzeScope_HealthScore_ProjectNarrows(t *testing.T) {
	// repo-a → project frontend, repo-b → project backend, both in one
	// workspace, so project=backend genuinely narrows to repo-b.
	fx := newSplitProjectWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	text, applied, _ := runAnalyze(t, fx.srv, ctx, map[string]any{"kind": "health_score", "project": "backend"})
	assert.Contains(t, text, "repo-b", "project=backend must include the backend repo (repo-b)")
	assert.NotContains(t, text, "repo-a", "project=backend must exclude the frontend repo (repo-a)")
	assert.Equal(t, "project:backend", applied)
}

// ─── 2. Tier-2 narrowing (dead_code/hotspots/cycles via analyzeNodeVisible) ──

func TestAnalyzeScope_DeadCode_RepoNarrows(t *testing.T) {
	srv, paths := newStructuralWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", paths["repo-a"])

	// Whole workspace: dead functions from both repos.
	text, applied, _ := runAnalyze(t, srv, ctx, map[string]any{"kind": "dead_code"})
	assert.Contains(t, text, "repo-a", "unnarrowed dead_code must include repo-a dead funcs")
	assert.Contains(t, text, "repo-b", "unnarrowed dead_code must include repo-b dead funcs")
	assert.Equal(t, "workspace", applied)

	// repo=repo-a → only repo-a dead funcs, no repo-b leak.
	text, applied, _ = runAnalyze(t, srv, ctx, map[string]any{"kind": "dead_code", "repo": "repo-a"})
	assert.Contains(t, text, "repo-a", "narrowed dead_code must include repo-a rows")
	assert.NotContains(t, text, "repo-b", "narrowed dead_code must not leak repo-b rows")
	assert.Equal(t, "repo:repo-a", applied)
}

func TestAnalyzeScope_Hotspots_Smoke(t *testing.T) {
	srv, paths := newStructuralWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", paths["repo-a"])

	// Unnarrowed: must not error (graph has >=10 nodes).
	r, err := srv.handleAnalyze(ctx, makeReq("analyze", map[string]any{"kind": "hotspots"}))
	require.NoError(t, err)
	require.False(t, r.IsError, "unnarrowed hotspots errored: %s", toolResultText(r))

	// repo=repo-a: no error, no repo-b leak, scope stamped.
	text, applied, _ := runAnalyze(t, srv, ctx, map[string]any{"kind": "hotspots", "repo": "repo-a"})
	assert.NotContains(t, text, "repo-b", "narrowed hotspots must not leak repo-b rows")
	assert.Equal(t, "repo:repo-a", applied)
}

func TestAnalyzeScope_Cycles_Smoke(t *testing.T) {
	srv, paths := newStructuralWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", paths["repo-a"])

	text, applied, _ := runAnalyze(t, srv, ctx, map[string]any{"kind": "cycles", "repo": "repo-a"})
	assert.NotContains(t, text, "repo-b", "narrowed cycles must not leak repo-b cycle members")
	assert.Equal(t, "repo:repo-a", applied)
}

// ─── 7. cycles `scope` collision regression (§D resolve-view strip) ──────────

func TestAnalyzeScope_Cycles_ScopeArgDoesNotCollide(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	// cycles owns `scope` as a path/pkg prefix. resolveScope must NOT try
	// to look it up as a saved scope (which would hard-error
	// "unknown scope"). The handler still reads its own scope arg.
	r, err := fx.srv.handleAnalyze(ctx, makeReq("analyze", map[string]any{
		"kind": "cycles", "scope": "internal/auth/",
	}))
	require.NoError(t, err)
	require.False(t, r.IsError, "cycles+scope must not error: %s", toolResultText(r))
	assert.NotContains(t, toolResultText(r), "unknown scope",
		"the path-prefix scope arg must not be mis-read as a saved-scope name")
	require.NotNil(t, r.Meta)
	assert.Equal(t, "workspace", r.Meta.AdditionalFields["scope_applied"])

	// A repo arg alongside the path-prefix scope still narrows.
	r2, err := fx.srv.handleAnalyze(ctx, makeReq("analyze", map[string]any{
		"kind": "cycles", "scope": "internal/auth/", "repo": "repo-a",
	}))
	require.NoError(t, err)
	require.False(t, r2.IsError)
	require.NotNil(t, r2.Meta)
	assert.Equal(t, "repo:repo-a", r2.Meta.AdditionalFields["scope_applied"])
}

// TestAnalyzeScope_Images_RefArgDoesNotCollide is a regression for the
// `ref` collision: analyze kind=images owns `ref` as the image reference
// filter (e.g. "ghcr.io/acme"). resolveScope must NOT mis-read it as a
// git ref / scope dimension — left unstripped it errors with
// "configuration manager is not available" on a server without a config
// manager. Mirrors the cycles/`scope` and cross_repo/`repo` strips (§D).
func TestAnalyzeScope_Images_RefArgDoesNotCollide(t *testing.T) {
	srv, _ := setupTestServer(t) // single-repo server, no MultiIndexer scope

	r, err := srv.handleAnalyze(context.Background(), makeReq("analyze", map[string]any{
		"kind": "images", "ref": "ghcr.io/acme",
	}))
	require.NoError(t, err)
	require.False(t, r.IsError, "images+ref must not error: %s", toolResultText(r))
	assert.NotContains(t, toolResultText(r), "configuration manager is not available",
		"the image-ref arg must not be mis-read as a git ref / saved scope")
}

// ─── 3. Workspace isolation preserved (SECURITY INVARIANT, must-pass) ────────

func TestAnalyzeScope_WorkspaceIsolation_HealthScore(t *testing.T) {
	srv, repoA, _ := newIsolationServer(t) // repo-a→alpha, repo-b→beta
	ctxA := sessionCtx("s-alpha", repoA)

	// Without any narrowing arg, a bound-alpha session must never surface
	// beta nodes.
	text, _, _ := runAnalyze(t, srv, ctxA, map[string]any{"kind": "health_score"})
	assert.Contains(t, text, "repo-a", "alpha session must see its own repo")
	assert.NotContains(t, text, "repo-b", "alpha session must NOT see beta nodes via health_score")
	assert.NotContains(t, text, "BetaThing", "alpha session must NOT see the beta symbol")

	// With an in-workspace narrowing arg, still no beta.
	text, _, _ = runAnalyze(t, srv, ctxA, map[string]any{"kind": "health_score", "repo": "repo-a"})
	assert.NotContains(t, text, "repo-b", "alpha+repo arg must NOT leak beta nodes")
}

func TestAnalyzeScope_WorkspaceIsolation_DeadCode(t *testing.T) {
	// Structural repos in two distinct workspaces so dead_code emits rows
	// AND the cross-workspace boundary is exercised.
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)
	ctxA := sessionCtx("s-alpha", paths["repo-a"])

	text, _, _ := runAnalyze(t, srv, ctxA, map[string]any{"kind": "dead_code"})
	assert.Contains(t, text, "repo-a", "alpha session must see its own dead funcs")
	assert.NotContains(t, text, "repo-b", "alpha session must NOT see beta dead funcs via dead_code")

	text, _, _ = runAnalyze(t, srv, ctxA, map[string]any{"kind": "dead_code", "repo": "repo-a"})
	assert.NotContains(t, text, "repo-b", "alpha+repo arg must NOT leak beta dead funcs")
}

// ─── 5. scope_note disclosure for non-scope-aware (bypass) kinds ─────────────

func TestAnalyzeScope_ScopeNote_BypassKind(t *testing.T) {
	// Membership gate is the source of truth.
	require.True(t, analyzeScopeAwareKinds["health_score"], "health_score must be scope-aware")
	require.False(t, analyzeScopeAwareKinds["clusters"], "clusters must NOT be scope-aware (community-detection bypass)")
	require.True(t, analyzeWorkspaceClampedKinds["clusters"], "clusters is workspace-clamped and self-discloses in the body")

	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	// clusters is workspace-clamped but not repo-narrowed: a narrowing
	// arg is disclosed in the response BODY (visible to every client),
	// not via the now-stale _meta scope_note. scope_applied stays uniform
	// and truthful about the resolved scope.
	r, err := fx.srv.handleAnalyze(ctx, makeReq("analyze", map[string]any{"kind": "clusters", "repo": "repo-a"}))
	require.NoError(t, err)
	require.False(t, r.IsError, "clusters errored: %s", toolResultText(r))
	require.NotNil(t, r.Meta)
	assert.Contains(t, toolResultText(r), "not repo/project-narrowed",
		"a workspace-clamped bypass kind asked to narrow must disclose the no-op in the body")
	_, hasMetaNote := r.Meta.AdditionalFields["scope_note"]
	assert.False(t, hasMetaNote, "a self-disclosing clamped kind must not also carry the stale _meta scope_note")
	assert.Equal(t, "repo:repo-a", r.Meta.AdditionalFields["scope_applied"],
		"scope_applied stays uniform/truthful about the resolved scope")

	// AUTO kind + narrowing arg → no scope_note (the narrowing is real).
	r2, err := fx.srv.handleAnalyze(ctx, makeReq("analyze", map[string]any{"kind": "health_score", "repo": "repo-a"}))
	require.NoError(t, err)
	require.NotNil(t, r2.Meta)
	_, hasNote := r2.Meta.AdditionalFields["scope_note"]
	assert.False(t, hasNote, "an AUTO kind that genuinely narrows must NOT carry a scope_note")
}

// ─── 6. Flag OFF — workspace-scoped default preserved, args still narrow ─────

func TestAnalyzeScope_FlagOff(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false) // intent defaults OFF
	ctx := sessionCtx("s-a", fx.repoA)

	// Default (no arg): still workspace breadth — both repos — no
	// regression to a home-repo narrowing.
	text, _, _ := runAnalyze(t, fx.srv, ctx, map[string]any{"kind": "health_score"})
	assert.Contains(t, text, "repo-a", "flag-off default must keep repo-a")
	assert.Contains(t, text, "repo-b", "flag-off default must keep repo-b (workspace breadth)")

	// New args still narrow on demand.
	text, applied, _ := runAnalyze(t, fx.srv, ctx, map[string]any{"kind": "health_score", "repo": "repo-a"})
	assert.Contains(t, text, "repo-a")
	assert.NotContains(t, text, "repo-b", "flag-off + repo arg must still narrow")
	assert.Equal(t, "repo:repo-a", applied)
}

// ─── 8. Byte-for-byte unbound — no narrowing, full graph ─────────────────────

func TestAnalyzeScope_Unbound_FullSet(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	bg := context.Background() // unbound: no session cwd

	// scopedNodes returns the whole graph unchanged for an unbound,
	// un-narrowed session.
	all := fx.srv.graph.AllNodes()
	got := fx.srv.scopedNodes(bg)
	assert.Equal(t, len(all), len(got),
		"unbound scopedNodes must return every node (byte-for-byte legacy behaviour)")

	// analyze with no narrowing arg likewise spans the full graph.
	text, _, _ := runAnalyze(t, fx.srv, bg, map[string]any{"kind": "health_score"})
	assert.Contains(t, text, "repo-a")
	assert.Contains(t, text, "repo-b")
}

// ─── ctx-layer: withRepoAllow folds into the scoped-node accessors ───────────

func TestAnalyzeScope_RepoAllowCtx_NarrowsScopedNodes(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctxA := sessionCtx("s-a", fx.repoA)
	ctxAllow := withRepoAllow(ctxA, map[string]bool{"repo-a": true})

	// Plain bound ctx sees the whole workspace (both repos) — the hard
	// boundary is workspace-shaped, not repo-shaped.
	plain := fx.srv.scopedNodes(ctxA)
	hasA, hasB := false, false
	for _, n := range plain {
		switch n.RepoPrefix {
		case "repo-a":
			hasA = true
		case "repo-b":
			hasB = true
		}
	}
	require.True(t, hasA && hasB, "bound ctx must see both repos before RepoAllow narrows")

	// withRepoAllow narrows scopedNodes to repo-a only.
	narrowed := fx.srv.scopedNodes(ctxAllow)
	require.NotEmpty(t, narrowed)
	for _, n := range narrowed {
		assert.Equal(t, "repo-a", n.RepoPrefix,
			"withRepoAllow must drop every non-repo-a node, leaked: %s", n.ID)
	}

	// analyzeNodeVisible mirrors the same gate per-node.
	var repoBNode *graph.Node
	for _, n := range plain {
		if n.RepoPrefix == "repo-b" {
			repoBNode = n
			break
		}
	}
	require.NotNil(t, repoBNode)
	assert.True(t, fx.srv.analyzeNodeVisible(ctxA, repoBNode),
		"without RepoAllow a repo-b node is visible inside the workspace")
	assert.False(t, fx.srv.analyzeNodeVisible(ctxAllow, repoBNode),
		"with RepoAllow{repo-a} a repo-b node must be filtered out")
}

// ─── 9. Per-gate-shape narrowing for the category-(a)/(c) kinds ──────────────
//
// Wave 1 made the category-(a) edge-walk / graph-algorithm / framework
// kinds and the category-(c) file/AST scans scope-aware via the per-row
// analyzeNodeVisible gate (or, for the (c) scans, resolveRepoFilter).
// The cases below pin ONE representative kind per gate shape; each one
// is verified non-vacuous (BOTH repos present in the broad result)
// before the narrowing assertion runs, so a kind that silently stopped
// emitting rows fails loudly instead of passing on an empty set.

// richAnalyzeBody returns a package-main body that exercises a broad set
// of category-(a) analyze kinds plus the category-(c) AST scan in one
// fixture:
//   - <p>Box.Set / <p>Box.Bump write the <p>Box.Val field  → field_writers
//     (SubjectActor: subject=field, actors=writers).
//   - the <p>Hub fan-in and the <p>MutualX/<p>MutualY cycle populate the
//     call / reference graph → ref_facts (TwoPeer), wcc/scc (MemberList),
//     pagerank/kcore (SingleNode), edge_audit (ReTally).
//   - <p>Boom's panic() call trips the panic-in-library detector →
//     unsafe_patterns (category-c file/AST scan).
//
// p is a per-repo prefix so symbol names never collide across repos and
// the "repo-a" / "repo-b" leak probe stays unambiguous. It is a superset
// of structuralBody (adds the field writes + the panic site) so the
// existing structural tests are untouched.
func richAnalyzeBody(p string) string {
	return fmt.Sprintf(`package main

type %[1]sBox struct{ Val int }

func (b *%[1]sBox) Set()  { b.Val = 1 }
func (b *%[1]sBox) Bump() { b.Val = b.Val + 1 }

func %[1]sHub()     {}
func %[1]sOne()     { %[1]sHub() }
func %[1]sTwo()     { %[1]sHub() }
func %[1]sBoom()    { panic("boom") }
func %[1]sMutualX() { %[1]sMutualY() }
func %[1]sMutualY() { %[1]sMutualX() }
`, p)
}

// newRichWorkspaceServer builds a two-repo single-workspace ("shared-ws",
// project "backend") server whose repos carry richAnalyzeBody — so every
// gate-shape kind below emits rows in BOTH repos.
func newRichWorkspaceServer(t *testing.T, flagOn bool) (*Server, map[string]string) {
	t.Helper()
	return newAnalyzeServer(t, flagOn,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", project: "backend", body: richAnalyzeBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", project: "backend", body: richAnalyzeBody("b")},
	)
}

// TestAnalyzeScope_GateShapes_RepoNarrows pins the per-row-filtered gate
// shapes that emit node-ID rows (SubjectActor / TwoPeer / MemberList /
// SingleNode) plus the category-(c) AST scan. For each: the broad
// (whole-workspace) result must carry BOTH repos (non-vacuous guard),
// then repo=repo-a must (i) keep repo-a rows, (ii) drop every repo-b
// row, (iii) stamp scope_applied truthfully, and (iv) carry NO
// scope_note (the kind is in analyzeScopeAwareKinds — the narrowing is
// real, not a documented no-op).
func TestAnalyzeScope_GateShapes_RepoNarrows(t *testing.T) {
	cases := []struct {
		kind  string
		shape string
	}{
		{"field_writers", "SubjectActor (pubsub vacuous on a non-pubsub fixture)"},
		{"ref_facts", "TwoPeer (routes vacuous on a non-routing fixture)"},
		{"wcc", "MemberList"},
		{"pagerank", "SingleNode"},
		{"unsafe_patterns", "category-c file/AST scan"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			require.True(t, analyzeScopeAwareKinds[tc.kind],
				"%s (%s) must be registered scope-aware", tc.kind, tc.shape)

			srv, paths := newRichWorkspaceServer(t, true)
			ctx := sessionCtx("s-a", paths["repo-a"])

			// Broad: whole bound workspace → BOTH repos present.
			// This is the non-vacuous guard — without it a kind that
			// emits zero rows would pass the narrowing assertion for
			// the wrong reason.
			broad, applied, _ := runAnalyze(t, srv, ctx, map[string]any{"kind": tc.kind})
			require.Contains(t, broad, "repo-a", "%s broad must include repo-a rows", tc.kind)
			require.Contains(t, broad, "repo-b",
				"%s broad must include repo-b rows — else the narrowing assertion is vacuous", tc.kind)
			assert.Equal(t, "workspace", applied)

			// repo=repo-a → only repo-a rows, no repo-b leak.
			narrow, applied, r := runAnalyze(t, srv, ctx, map[string]any{"kind": tc.kind, "repo": "repo-a"})
			assert.Contains(t, narrow, "repo-a", "%s narrowed must keep repo-a rows", tc.kind)
			assert.NotContains(t, narrow, "repo-b", "%s narrowed must not leak repo-b rows", tc.kind)
			assert.Equal(t, "repo:repo-a", applied)

			// A genuinely-narrowing scope-aware kind carries no scope_note.
			_, hasNote := r.Meta.AdditionalFields["scope_note"]
			assert.False(t, hasNote, "%s genuinely narrows — must NOT carry a scope_note", tc.kind)
		})
	}
}

// TestAnalyzeScope_EdgeAudit_ReTallyNarrows covers the ReTally gate
// shape. The plan's preferred ReTally kind (temporal_orphans) emits only
// empty lists on a non-Temporal fixture, so edge_audit — emitted by ANY
// edges — is the same-shape fallback. edge_audit returns no node-ID rows,
// only re-tallied counts, so the leak probe is replaced by a count
// assertion: narrowing to one repo must drop summary.total_edges below
// the whole-workspace tally while staying > 0. scope_applied is stamped
// and (the kind being scope-aware) no scope_note is attached.
func TestAnalyzeScope_EdgeAudit_ReTallyNarrows(t *testing.T) {
	require.True(t, analyzeScopeAwareKinds["edge_audit"], "edge_audit must be scope-aware")

	srv, paths := newRichWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", paths["repo-a"])

	broadText, applied, _ := runAnalyze(t, srv, ctx, map[string]any{"kind": "edge_audit"})
	assert.Equal(t, "workspace", applied)
	broadEdges := edgeAuditTotalEdges(t, broadText)
	require.Greater(t, broadEdges, 0, "whole-workspace edge_audit must tally >0 edges (non-vacuous)")

	narrowText, applied, r := runAnalyze(t, srv, ctx, map[string]any{"kind": "edge_audit", "repo": "repo-a"})
	assert.Equal(t, "repo:repo-a", applied)
	narrowEdges := edgeAuditTotalEdges(t, narrowText)
	require.Greater(t, narrowEdges, 0, "repo-a edge_audit must still tally its own edges")
	assert.Less(t, narrowEdges, broadEdges,
		"narrowing edge_audit to repo-a must re-tally below the whole-workspace edge count")

	_, hasNote := r.Meta.AdditionalFields["scope_note"]
	assert.False(t, hasNote, "edge_audit genuinely re-tallies under scope — must NOT carry a scope_note")
}

// edgeAuditTotalEdges pulls summary.total_edges out of an edge_audit JSON
// response.
func edgeAuditTotalEdges(t *testing.T, text string) int {
	t.Helper()
	var payload struct {
		Summary struct {
			TotalEdges int `json:"total_edges"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &payload), "edge_audit response must be JSON: %s", text)
	return payload.Summary.TotalEdges
}

// ─── 10. Workspace isolation for a category-(a) and a category-(c) kind ──────
//
// Mirrors TestAnalyzeScope_WorkspaceIsolation_DeadCode (§11 security
// invariant) but for a category-(a) per-row kind (pagerank, SingleNode)
// and a category-(c) AST scan (unsafe_patterns). The two repos live in
// DISTINCT workspaces (alpha / beta) and both carry richAnalyzeBody so
// each kind emits rows in BOTH repos; a session bound to the alpha repo
// must NEVER surface beta nodes — with or without a narrowing arg.
func TestAnalyzeScope_WorkspaceIsolation_GateShapes(t *testing.T) {
	for _, kind := range []string{"pagerank", "unsafe_patterns"} {
		t.Run(kind, func(t *testing.T) {
			srv, paths := newAnalyzeServer(t, true,
				analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: richAnalyzeBody("a")},
				analyzeRepoSpec{name: "repo-b", workspace: "beta", body: richAnalyzeBody("b")},
			)
			ctxA := sessionCtx("s-alpha", paths["repo-a"])

			// No narrowing arg: a bound-alpha session sees only its own
			// repo — never the beta repo across the workspace boundary.
			text, _, _ := runAnalyze(t, srv, ctxA, map[string]any{"kind": kind})
			assert.Contains(t, text, "repo-a", "%s: alpha session must see its own rows", kind)
			assert.NotContains(t, text, "repo-b",
				"%s: alpha session must NOT see beta nodes (workspace isolation)", kind)

			// In-workspace narrowing arg: still no beta leak.
			text, _, _ = runAnalyze(t, srv, ctxA, map[string]any{"kind": kind, "repo": "repo-a"})
			assert.NotContains(t, text, "repo-b", "%s: alpha+repo arg must NOT leak beta nodes", kind)
		})
	}
}

// ─── 11. Full analyzeScopeAwareKinds sweep — cross-workspace leak guard ───────
//
// REGRESSION GUARD. Until now no test iterated the whole
// analyzeScopeAwareKinds set, which is exactly how `releases` shipped
// mislabeled as scope-aware while leaking release nodes across the
// workspace boundary. This table binds a session to workspace alpha and
// asserts that EVERY scope-aware kind keeps repo-b (workspace beta) rows
// out of the response — so a future kind added to the set without real
// narrowing fails here loudly instead of silently leaking.
//
// `releases` is the motivating case: it reads s.graph directly and only
// became safe once the per-row analyzeNodeVisible gate landed in
// handleAnalyzeReleases. KindRelease nodes are normally materialised by
// the git-tag enricher, so the test injects one per repo (attributed
// exactly like the repo's real nodes) — without that, `releases` would
// emit an empty timeline and pass vacuously, hiding the very regression
// this test exists to catch. Revert the analyzeNodeVisible gate in
// handleAnalyzeReleases and the `releases` sub-test fails: the beta
// release node leaks into the alpha-bound session.
func TestAnalyzeScope_AllScopeAwareKinds_NoCrossWorkspaceLeak(t *testing.T) {
	// Two repos in DISTINCT workspaces so the workspace boundary is
	// exercised; leakProbeBody emits rows for the structural / edge-walk /
	// field-write / panic / TODO kinds in BOTH repos.
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: leakProbeBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: leakProbeBody("b")},
	)

	// Make `releases` a non-vacuous probe: one KindRelease node per repo,
	// scoped exactly like the repo's genuine nodes.
	injectReleaseNode(t, srv, "repo-a")
	injectReleaseNode(t, srv, "repo-b")

	ctxA := sessionCtx("s-alpha", paths["repo-a"])

	// Sanity: the injected repo-b release leaks into an UNBOUND session
	// (no workspace ceiling), proving the probe can actually surface
	// "repo-b" — so the bound-session assertion below is meaningful and
	// not vacuously green.
	unboundReleases, _, _ := runAnalyze(t, srv, context.Background(), map[string]any{"kind": "releases"})
	require.Contains(t, unboundReleases, "repo-b",
		"unbound releases must see the injected repo-b release node — else the leak probe is vacuous")

	// Kinds whose rows only exist after an external enrichment pass we
	// cannot reproduce in-memory (git blame timestamps, a coverage
	// profile). They emit empty here rather than leak, so exercising them
	// would only assert a vacuous truth; skip + log instead. (The blame /
	// coverage WRITER kinds are not in analyzeScopeAwareKinds at all, so
	// they never reach this loop.)
	skip := map[string]string{
		"stale_code":       "needs git-blame meta.last_authored",
		"ownership":        "needs git-blame author data",
		"stale_flags":      "needs feature-flag toggles + git-blame timestamps",
		"coverage_gaps":    "needs a coverage profile",
		"coverage_summary": "needs a coverage profile",
	}

	kinds := make([]string, 0, len(analyzeScopeAwareKinds))
	for k := range analyzeScopeAwareKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	for _, kind := range kinds {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			if reason, ok := skip[kind]; ok {
				t.Skipf("skipping %s — %s (cannot fixture in-memory; emits empty, never leaks)", kind, reason)
			}
			res, err := srv.handleAnalyze(ctxA, makeReq("analyze", map[string]any{"kind": kind}))
			require.NoError(t, err)
			require.NotNil(t, res)
			// SECURITY INVARIANT: an alpha-bound session must NEVER surface
			// a repo-b (workspace beta) row, with or without a narrowing
			// arg. A leak shows up as the literal "repo-b" in a node ID /
			// file path / repo_prefix field.
			assert.NotContains(t, toolResultText(res), "repo-b",
				"%s: alpha-bound session leaked a beta (repo-b) row — cross-workspace isolation breach", kind)
		})
	}
}

// leakProbeBody is a superset of richAnalyzeBody that also carries a TODO
// comment and an unreferenced func, so the workspace-leak sweep exercises
// the AUTO (todos), Tier-2 (dead_code), category-(a) (field_writers,
// ref_facts, pagerank, …) and category-(c) (unsafe_patterns) kinds with
// real rows in each repo. p prefixes every symbol so the "repo-b" leak
// probe stays unambiguous.
func leakProbeBody(p string) string {
	return fmt.Sprintf(`package main

// TODO(%[1]s): exercise the todos analyzer
type %[1]sBox struct{ Val int }

func (b *%[1]sBox) Set()  { b.Val = 1 }
func (b *%[1]sBox) Bump() { b.Val = b.Val + 1 }

func %[1]sHub()     {}
func %[1]sOne()     { %[1]sHub() }
func %[1]sTwo()     { %[1]sHub() }
func %[1]sBoom()    { panic("boom") }
func %[1]sDead()    {}
func %[1]sMutualX() { %[1]sMutualY() }
func %[1]sMutualY() { %[1]sMutualX() }
`, p)
}

// injectReleaseNode adds one synthetic KindRelease node for the named
// repo, copying WorkspaceID / ProjectID / RepoPrefix from a real indexed
// node of that repo so nodeInSessionScope narrows it exactly like the
// repo's genuine nodes. Release nodes are normally materialised only by
// the git-tag enricher, which the in-memory test graph has no way to run.
func injectReleaseNode(t *testing.T, srv *Server, repo string) {
	t.Helper()
	var tmpl *graph.Node
	for _, n := range srv.graph.AllNodes() {
		if n.RepoPrefix == repo {
			tmpl = n
			break
		}
	}
	require.NotNil(t, tmpl, "no indexed node found for repo %q to anchor a release node", repo)
	const tag = "v1.0.0"
	srv.graph.AddNode(&graph.Node{
		ID:          "release::" + repo + "::" + tag,
		Kind:        graph.KindRelease,
		Name:        tag,
		RepoPrefix:  tmpl.RepoPrefix,
		WorkspaceID: tmpl.WorkspaceID,
		ProjectID:   tmpl.ProjectID,
		Meta:        map[string]any{"tag": tag, "file_count": 1, "order": 0},
	})
}
