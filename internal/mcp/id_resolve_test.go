package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func nameResolveServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t) // pkg/foo.go::Bar, pkg/foo.go::Baz
	// Make "Bar" ambiguous across two files.
	s.graph.AddNode(&graph.Node{ID: "pkg/other.go::Bar", Name: "Bar", Kind: graph.KindFunction, FilePath: "pkg/other.go"})
	// A non-definition node named "Bar" must be ignored.
	s.graph.AddNode(&graph.Node{ID: "pkg/foo.go::Bar#local", Name: "Bar", Kind: graph.KindLocal, FilePath: "pkg/foo.go"})
	s.engine = query.NewEngine(s.graph)
	return s
}

// TestResolveSymbolID_WithoutMultiIndexer_StillAnchorsThePath pins the rung
// order inside resolveSymbolID: the cwd rung needs the multi-repo index to map
// a directory to a repo prefix, the graphRelID rung does not. Returning early
// on a nil multiIndexer skipped both, so a server built without
// MultiRepoOptions — cmd/gortex's eval_recall.go and eval_server.go — lost path
// anchoring entirely.
//
// The absolute-path spelling is deliberate: it exercises the rung on every
// platform, so the linux/macos matrix protects this. On Windows the same rung
// additionally reconciles the separator, which is what graphPathSpelling exists
// for — the store holds `pkga\a.go::Foo` while every agent writes
// `pkga/a.go::Foo`, and move_symbol answered "symbol not found" for an indexed
// symbol.
func TestResolveSymbolID_WithoutMultiIndexer_StillAnchorsThePath(t *testing.T) {
	srv, dir := setupMoveInlineRepo(t, map[string]string{
		"pkga/a.go": "package pkga\n\nfunc Foo() int { return 42 }\n",
	})
	require.Nil(t, srv.multiIndexer, "fixture must exercise the nil-multiIndexer path")

	var stored string
	for _, n := range srv.graph.FindNodesByName("Foo") {
		if n != nil && n.Kind == graph.KindFunction {
			stored = n.ID
		}
	}
	require.NotEmpty(t, stored, "fixture must index Foo")

	absID := filepath.Join(dir, "pkga", "a.go") + "::Foo"
	require.Nil(t, srv.graph.GetNode(absID), "the absolute spelling must not be a stored id")

	assert.Equal(t, stored, srv.resolveSymbolID(context.Background(), absID),
		"an absolute-path id must anchor back to the stored id without a multiIndexer")
}

func TestResolveNameToIDs(t *testing.T) {
	s := nameResolveServer(t)
	got := s.resolveNameToIDs("Bar")
	// Two definitions, the KindLocal excluded, sorted.
	require.Equal(t, []string{"pkg/foo.go::Bar", "pkg/other.go::Bar"}, got)

	// Unique name → single id.
	require.Equal(t, []string{"pkg/foo.go::Baz"}, s.resolveNameToIDs("Baz"))
	// No match → nil.
	require.Nil(t, s.resolveNameToIDs("Nope"))
}

func TestSymbolTargetArgExactIDWins(t *testing.T) {
	s := nameResolveServer(t)
	// A full id that exists resolves to itself — never reinterpreted as a name,
	// even though "Bar" is ambiguous.
	id, cands := s.resolveSymbolTarget(context.Background(), "pkg/foo.go::Bar")
	assert.Equal(t, "pkg/foo.go::Bar", id)
	assert.Nil(t, cands)
}

func TestGetCallersUniqueBareName(t *testing.T) {
	s := nameResolveServer(t)
	// "Baz" is unique → resolves, get_callers proceeds (no disambiguation).
	res := callHandler(t, s.handleGetCallers, map[string]any{"id": "Baz"})
	m := unmarshalResult(t, res)
	if _, ambiguous := m["ambiguous"]; ambiguous {
		t.Errorf("unique bare name should resolve, not disambiguate: %+v", m)
	}
}

func TestGetCallersBareNameDisambiguation(t *testing.T) {
	s := nameResolveServer(t)
	// "Bar" is ambiguous → disambiguation result with both candidates.
	res := callHandler(t, s.handleGetCallers, map[string]any{"id": "Bar"})
	m := unmarshalResult(t, res)
	require.Equal(t, true, m["ambiguous"])
	require.Equal(t, "Bar", m["name"])
	cands, ok := m["candidates"].([]any)
	require.True(t, ok, "candidates should be a list, got %T", m["candidates"])
	require.Len(t, cands, 2)
}
