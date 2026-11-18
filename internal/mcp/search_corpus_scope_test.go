package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// newWorkspaceRootBoundServer builds the shared-workspace-root shape zzet's
// PR-484 review calls out: r1 + r2 share a workspace via slug under one
// parent root, z lives elsewhere in its own workspace with a content
// body in the content index. Returns a server whose session, when dialed
// from root, binds to the shared workspace with an empty home repo.
func newWorkspaceRootBoundServer(t *testing.T) (s *Server, root string) {
	t.Helper()
	writeRepo := func(dir string) {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
	}
	root = t.TempDir()
	r1 := filepath.Join(root, "r1")
	r2 := filepath.Join(root, "r2")
	writeRepo(r1)
	writeRepo(r2)
	z := setupMiniRepo(t, "z")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: r1, Name: "r1", Workspace: "shared"},
			{Path: r2, Name: "r2", Workspace: "shared"},
			{Path: z, Name: "z"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "s.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().ReposSnapshot() {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	// A content node owned by z, with its body in the content index.
	store.AddNode(&graph.Node{
		ID:         "z/doc.txt::doc:section-0",
		Kind:       graph.KindDoc,
		FilePath:   "z/doc.txt",
		RepoPrefix: "z",
		Meta:       map[string]any{"data_class": "content", "section_text": "snippet"},
	})
	require.NoError(t, store.AppendContent("z", []graph.ContentFTSItem{
		{NodeID: "z/doc.txt::doc:section-0", FilePath: "z/doc.txt", Body: "the zzleakterm appears only here"},
	}))
	require.NoError(t, store.BuildContentIndex())

	return &Server{
		graph:        store,
		multiIndexer: mi,
		session:      newSessionState(),
		tokenStats:   &tokenStats{},
		symHistory:   &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:     newSessionMap(),
		toolScopes:   newScopeRegistry(),
	}, root
}

// TestMergeContentChannel_WorkspaceRootSessionStaysInWorkspace: a session
// bound to a shared workspace by its ROOT cwd carries an empty repo
// prefix — the content channel must not let that empty prefix widen the
// content search across every workspace. A content body indexed under a
// repo in ANOTHER workspace must stay invisible to the bound session.
func TestMergeContentChannel_WorkspaceRootSessionStaysInWorkspace(t *testing.T) {
	s, root := newWorkspaceRootBoundServer(t)
	ctx := WithSessionCWD(context.Background(), root)

	// Precondition: the root cwd binds to the shared workspace with an
	// empty home repo — the exact state the leak needs.
	ws, _, bound := s.sessionScope(ctx)
	require.True(t, bound, "workspace-root session must bind")
	repoPrefix, _ := s.sessionLocality(ctx)
	require.Empty(t, repoPrefix, "multi-repo workspace-root binding has no home repo")
	require.NotContains(t, ws, unresolvedWorkspacePrefix)

	merged := s.mergeContentChannel(ctx, "zzleakterm", nil, 10)
	for _, n := range merged {
		require.NotEqual(t, "z/doc.txt::doc:section-0", n.ID,
			"content from another workspace must not leak into a bound workspace-root session")
	}

	// Control: an unbound session (no cwd) keeps the pre-existing
	// whole-graph behavior. Fresh server: session scope caches on the
	// default session state, so reusing s would replay the bound scope.
	s2, _ := newWorkspaceRootBoundServer(t)
	unbound := s2.mergeContentChannel(context.Background(), "zzleakterm", nil, 10)
	var ids []string
	for _, n := range unbound {
		ids = append(ids, n.ID)
	}
	require.Contains(t, ids, "z/doc.txt::doc:section-0",
		"unbound sessions keep the server-default whole-graph content channel")
}

// TestActiveProjectName_WorkspaceRootBindingHidesServerDefault: a cwd
// that binds to a shared workspace resolves with an EMPTY project — the
// initialize instructions must not fall through to the server-wide
// active project, which may live outside the session's bound workspace.
func TestActiveProjectName_WorkspaceRootBindingHidesServerDefault(t *testing.T) {
	s, root := newWorkspaceRootBoundServer(t)
	s.activeProject = "elsewhere"

	require.Empty(t, s.activeProjectName(root),
		"a bound-empty project must not advertise the server-wide default")

	// An uncovered cwd keeps the pre-existing fallback.
	require.Equal(t, "elsewhere", s.activeProjectName(filepath.Join(t.TempDir(), "nowhere")))
}
