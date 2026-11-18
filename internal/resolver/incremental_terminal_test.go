package resolver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestIncrementalTerminalCarryRechecksExactPriorProof(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"same", "unproved", "changed_meta", "changed_alias", "changed_confidence", "moved_line", "deleted", "resolved", "new_definition", "lsp", "lookup_error", "row_cap", "foreign_file", "unsupported"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(t.TempDir() + "/graph.sqlite")
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				const caller = "caller.go::Run"
				g.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "Run", Language: "go", FilePath: "caller.go"})
				old := &graph.Edge{From: caller, To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 4, Confidence: .2, Meta: map[string]any{"keep": "original"}}
				setEdgeTerminal(old, terminalReasonNoDefinition)
				if mode == "unproved" {
					clearEdgeTerminal(old)
				}
				r := New(g)
				r.SetIncrementalSkip([]*graph.Edge{old})
				fresh := *old
				fresh.Meta = map[string]any{"keep": "original"}
				switch mode {
				case "changed_meta":
					fresh.Meta["keep"] = "changed"
				case "changed_alias":
					fresh.Alias = "other"
				case "changed_confidence":
					fresh.Confidence = .3
				case "moved_line":
					fresh.Line++
				case "resolved":
					fresh.To = "definition.go::Found"
				case "new_definition":
					g.AddNode(&graph.Node{ID: "definition.go::Missing", Name: "Missing", Kind: graph.KindFunction, FilePath: "definition.go", Language: "go"})
				case "lsp":
					r.SetLSPHelper(&fakeLSPHelper{})
				case "foreign_file":
					old.FilePath = "foreign.go"
					r.SetIncrementalSkip([]*graph.Edge{old})
				}
				if mode != "deleted" {
					g.AddEdge(&fresh)
					if backend == "sqlite" && mode == "changed_alias" {
						stored := g.GetOutEdges(caller)
						require.Len(t, stored, 1)
						require.Empty(t, stored[0].Alias, "non-import Alias is not persisted; the durable payload is unchanged")
					}
				}
				if mode == "lookup_error" || mode == "row_cap" {
					r.graph = &terminalCarryVisitorStore{Store: g, fail: mode == "lookup_error", overflow: mode == "row_cap"}
				}
				if mode == "unsupported" {
					r.graph = &terminalCarryUnknownStore{Store: g}
				}
				r.ResolveFilesAndIncoming([]string{"caller.go"})
				if mode == "deleted" {
					require.Empty(t, g.GetOutEdges(caller))
					return
				}
				rows := g.GetOutEdges(caller)
				require.Len(t, rows, 1)
				if mode == "same" || (backend == "sqlite" && mode == "changed_alias") {
					require.True(t, edgeTerminalFlag(rows[0]))
					require.Equal(t, terminalReasonNoDefinition, rows[0].Meta[metaResolveTerminalReason])
					require.Equal(t, "original", rows[0].Meta["keep"])
				} else {
					require.False(t, edgeTerminalFlag(rows[0]), "only an unchanged still-valid prior proof may carry")
				}
				if mode == "new_definition" {
					r.SetIncrementalSkip(nil)
					r.ResolveFilesAndIncoming([]string{"definition.go"})
					rows = g.GetOutEdges(caller)
					require.Len(t, rows, 1)
					require.Equal(t, "definition.go::Missing", rows[0].To, "normal incoming definition rebind is retained")
					require.False(t, edgeTerminalFlag(rows[0]))
				}
			})
		}
	}
}

type terminalCarryVisitorStore struct {
	graph.Store
	fail, overflow bool
}

func (s *terminalCarryVisitorStore) FindEdgesByIdentities(keys []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	return s.Store.(graph.EdgeIdentityBatchFinder).FindEdgesByIdentities(keys)
}
func (s *terminalCarryVisitorStore) VisitNodeDeclarationsByNamesContext(_ context.Context, names []string, yield func(*graph.Node) bool) error {
	if s.fail {
		return errors.New("lookup failed")
	}
	if s.overflow {
		for i := 0; i <= attributionReindexBatchSize; i++ {
			if !yield(&graph.Node{Name: names[0], Kind: graph.KindLocal}) {
				break
			}
		}
	}
	return nil
}

type terminalCarryUnknownStore struct{ graph.Store }

func (s *terminalCarryUnknownStore) FindEdgesByIdentities(keys []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	return s.Store.(graph.EdgeIdentityBatchFinder).FindEdgesByIdentities(keys)
}
