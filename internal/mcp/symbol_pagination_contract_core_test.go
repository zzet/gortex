package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// The daemon installs the contract analysis runtime by default, so every
// ordinary request reads through the contract-core adjacency wrapper. A JSON
// caller's next_cursor must continue in the same session either way.
func TestSearchSymbolsJSONCursorContinuesUnderContractRuntime(t *testing.T) {
	for _, installed := range []bool{true, false} {
		t.Run(fmt.Sprintf("runtime_installed_%v", installed), func(t *testing.T) {
			g := graph.New()
			backend := newOrderedBackend()
			for i := range 12 {
				id := fmt.Sprintf("repo/f%02d.go::Opaque%02d", i, i)
				g.AddNode(&graph.Node{ID: id, Name: fmt.Sprintf("Opaque%02d", i), Kind: graph.KindFunction,
					FilePath: fmt.Sprintf("repo/f%02d.go", i), RepoPrefix: "repo", Language: "go"})
				backend.put("needle", id)
			}
			eng := query.NewEngine(g)
			eng.SetSearch(backend)
			eng.SetRerank(nil)
			srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
			if installed {
				installContractCoreKindTestRuntime(t, srv)
			}
			tool := srv.MCPServer().GetTool("search_symbols")
			require.NotNil(t, tool)
			ctx := WithSessionID(t.Context(), "json-cursor-session")
			call := func(cursor string) map[string]any {
				t.Helper()
				args := map[string]any{"query": "Needle", "limit": 3, "format": "json", "max_bytes": 0}
				if cursor != "" {
					args["cursor"] = cursor
				}
				req := mcplib.CallToolRequest{Params: mcplib.CallToolParams{Name: "search_symbols", Arguments: args}}
				result, err := tool.Handler(ctx, req)
				require.NoError(t, err)
				require.NotEmpty(t, result.Content)
				text := result.Content[0].(mcplib.TextContent).Text
				require.Falsef(t, result.IsError, "search_symbols refused: %s", text)
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(text), &body))
				return body
			}
			first := call("")
			cursor, _ := first["next_cursor"].(string)
			require.NotEmpty(t, cursor, "page 1 must mint a continuation")
			if installed {
				_, isSequence := parseSymbolCursor(cursor)
				require.True(t, isSequence, "a JSON caller must receive the sequence cursor")
			}
			firstIDs := respIDs(first)
			require.Len(t, firstIDs, 3)
			second := call(cursor)
			secondIDs := respIDs(second)
			require.NotEmpty(t, secondIDs, "page 2 must return rows")
			for id := range secondIDs {
				require.NotContainsf(t, firstIDs, id, "page 2 repeated page-1 row %s", id)
			}
		})
	}
}

type contractCoreRevisionlessReader struct{ graph.Reader }

type contractCoreSearchRevisionReader struct {
	graph.Reader
	revision uint64
	known    bool
}

func (r contractCoreSearchRevisionReader) SearchMutationRevision() (uint64, bool) {
	return r.revision, r.known
}

// The wrapper forwards the selected reader's revision and never invents one.
func TestContractCoreEdgesForwardsSearchMutationRevision(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
	// The daemon's SQLite store selects the deepest composite wrapper shape.
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "revision.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"}}, nil)
	cases := []struct {
		name     string
		inner    graph.Reader
		revision uint64
		known    bool
	}{
		{"mutation_revision", g, g.MutationRevision(), true},
		{"sqlite_store", store, store.MutationRevision(), true},
		{"search_revision_known", contractCoreSearchRevisionReader{Reader: g, revision: 42, known: true}, 42, true},
		{"search_revision_unknown", contractCoreSearchRevisionReader{Reader: g, revision: 42, known: false}, 0, false},
		{"no_revision", contractCoreRevisionlessReader{g}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := newContractCoreEdges(tc.inner, t.Context(), nil)
			revisioned, ok := wrapped.(interface{ SearchMutationRevision() (uint64, bool) })
			require.True(t, ok)
			revision, known := revisioned.SearchMutationRevision()
			require.Equal(t, tc.known, known)
			if tc.known {
				require.Equal(t, tc.revision, revision)
			}
			require.Equal(t, tc.known, symbolPageRevisionKnown(wrapped))
		})
	}
}

// A base-narrowed graph selector reads the corpus, so its continuation is
// as authoritative as the corpus revision, with or without the runtime.
func TestBaseGraphReaderForwardsCorpusMutationRevision(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
	scoped := newBaseGraphReader(g, "repo")
	require.True(t, symbolPageRevisionKnown(scoped))
	require.True(t, symbolPageRevisionKnown(newContractCoreEdges(scoped, t.Context(), nil)))
	before, _ := scoped.(interface{ SearchMutationRevision() (uint64, bool) }).SearchMutationRevision()
	g.AddNode(&graph.Node{ID: "other/b.go::B", Name: "B", Kind: graph.KindFunction, FilePath: "other/b.go", RepoPrefix: "other"})
	after, _ := scoped.(interface{ SearchMutationRevision() (uint64, bool) }).SearchMutationRevision()
	require.NotEqual(t, before, after, "a corpus write must invalidate the narrowed view's cursor")
	require.False(t, symbolPageRevisionKnown(newBaseGraphReader(contractCoreRevisionlessReader{g}, "repo")))
}
