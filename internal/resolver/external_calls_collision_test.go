package resolver

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Two calls into one package at one site (`zap.String(...)` and
// `zap.Error(...)` on one line) retarget to one external-call edge key, and
// the store keeps one payload. Which one must not depend on the order the
// candidates were written in (a whole index writes them from parallel
// workers), on where a chunk boundary falls, or on whether the whole graph or
// only the edited file is synthesized: the survivor is always the candidate
// with the smallest original terminal.
func TestExternalCallCollisionSurvivorIsIndependentOfOrder(t *testing.T) {
	const (
		caller = "repo/svc/x.go::Caller"
		file   = "repo/svc/x.go"
		line   = 42
	)
	colliding := []string{"String", "Error", "Int"} // Error sorts first
	fillers := externalCallMutationChunk - 1        // the group straddles a chunk boundary

	build := func(t *testing.T, store graph.Store, reverse bool) {
		nodes := []*graph.Node{{
			ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: file, Language: "go", RepoPrefix: "repo",
		}, {
			ID: "repo/svc/a.go::Filler", Kind: graph.KindFunction, Name: "Filler", FilePath: "repo/svc/a.go", Language: "go", RepoPrefix: "repo",
		}}
		var edges []*graph.Edge
		for i := 0; i < fillers; i++ {
			edges = append(edges, &graph.Edge{
				From: "repo/svc/a.go::Filler", To: "dep::example.com/other::F", Kind: graph.EdgeCalls,
				FilePath: "repo/svc/a.go", Line: i + 1,
			})
		}
		group := make([]*graph.Edge, 0, len(colliding))
		for _, sym := range colliding {
			group = append(group, &graph.Edge{
				From: caller, To: "dep::go.uber.org/zap::" + sym, Kind: graph.EdgeCalls,
				FilePath: file, Line: line, Meta: map[string]any{"callee": sym},
			})
		}
		if reverse {
			for i, j := 0, len(group)-1; i < j; i, j = i+1, j-1 {
				group[i], group[j] = group[j], group[i]
			}
			edges = append(group, edges...)
		} else {
			edges = append(edges, group...)
		}
		store.AddBatch(nodes, edges)
	}
	survivor := func(t *testing.T, store graph.Store) *graph.Edge {
		var found []*graph.Edge
		for _, e := range store.GetOutEdges(caller) {
			if e.To == "external-call::dep::go.uber.org/zap" {
				found = append(found, e)
			}
		}
		require.Len(t, found, 1, "the converging calls collapse onto one edge")
		return found[0]
	}

	// Both production stores: the SQLite store collapses a key collision on
	// its own, the in-memory graph (the whole index's shadow corpus, drained
	// to disk afterwards) would keep the converging edges side by side.
	backends := map[string]func(t *testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": func(t *testing.T) graph.Store {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "external.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for _, name := range []string{"memory", "sqlite"} {
		for _, scope := range []string{"whole", "file"} {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/reverse=%t", name, scope, reverse), func(t *testing.T) {
					store := backends[name](t)
					build(t, store, reverse)
					if scope == "whole" {
						SynthesizeExternalCalls(store, true)
					} else {
						SynthesizeExternalCallsForFiles(store, true, []string{file, "repo/svc/a.go"})
					}
					got := survivor(t, store)
					callee, _ := got.Meta["callee"].(string)
					require.Equal(t, "Error", callee, "the smallest original terminal's payload survives")
					for _, e := range store.GetOutEdges(caller) {
						require.False(t, strings.HasPrefix(e.To, "dep::go.uber.org/zap::"),
							"a converging member is removed, not left on its terminal: %s", e.To)
					}
				})
			}
		}
	}
}
