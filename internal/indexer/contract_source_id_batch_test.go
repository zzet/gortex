package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractSourceIDsUseBoundedWritePagesWithFullProofParity(t *testing.T) {
	core := graph.New()
	var ids []string
	want := make(map[string]*graph.Node)
	for i := 0; i < 257; i++ {
		id := fmt.Sprintf("a/source.go::N%03d", i)
		n := &graph.Node{ID: id, Name: fmt.Sprintf("N%03d", i), Kind: graph.KindFunction, FilePath: "a/source.go", RepoPrefix: "a", Language: "go", StartLine: i + 1, EndLine: i + 2, WorkspaceID: "workspace", ProjectID: "project", Meta: map[string]any{"signature": "func N()", "nested": map[string]any{"kept": []string{"x", "y"}}}}
		core.AddNode(n)
		ids = append(ids, id)
		want[id] = n
	}
	e := completedNameEvidence(t, core)
	e.ledger = newContractUnitLedger()
	before := store_sqlite.WriteTransactionsBegun()
	got := e.GetNodesByIDs(append(append([]string(nil), ids...), ids[0], "a/missing.go::Absent"))
	writes := store_sqlite.WriteTransactionsBegun() - before
	require.NoError(t, e.err)
	require.Equal(t, want, got, "all selected node fields and nested metadata survive")
	require.Equal(t, int64(3), writes, "257 selected nodes use the existing128-row write pages, not257 transactions")
	require.Equal(t, 257, e.writtenNodes)
	require.Len(t, e.ledger.reads, 1)
	require.Equal(t, contractReadIDs, e.ledger.reads[0].kind)
	require.Contains(t, e.ledger.reads[0].args, "a/missing.go::Absent", "negative selected reads remain proven")
	digest, ok := contractAnswerDigest(got)
	require.True(t, ok)
	require.Equal(t, digest, e.ledger.reads[0].digest)
	stored, err := e.scratch.GetNodesByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	for id, n := range want {
		require.Equal(t, n.Meta, stored[id].Meta)
		require.Equal(t, n.ID, stored[id].ID)
	}
	got[ids[0]].Meta["signature"] = "private"
	require.Equal(t, "func N()", core.GetNode(ids[0]).Meta["signature"], "private imports must not mutate selected rows")
}

func TestContractSourceIDBatchKeepsScratchShapeAndInputSeparation(t *testing.T) {
	core := graph.New()
	id := "a/types.go::Reply"
	core.AddNode(&graph.Node{ID: id, Name: "Reply", Kind: graph.KindType, RepoPrefix: "a", FilePath: "a/types.go", Meta: map[string]any{"shape": "old core shape", "kept": true}})
	e := completedNameEvidence(t, core)
	e.ledger = newContractUnitLedger()
	first := e.GetNodesByIDs([]string{id})
	require.NoError(t, e.err)
	require.NotContains(t, first[id].Meta, "shape", "core-derived shape is still removed on import")
	first[id].Meta["shape"] = "private current shape"
	e.AddNode(first[id])
	require.NoError(t, e.err)
	reads := len(e.ledger.reads)
	before := store_sqlite.WriteTransactionsBegun()
	second := e.GetNodesByIDs([]string{id})
	require.NoError(t, e.err)
	require.Equal(t, "private current shape", second[id].Meta["shape"])
	require.Equal(t, before, store_sqlite.WriteTransactionsBegun(), "scratch hit needs no import write")
	require.Len(t, e.ledger.reads, reads, "private derived shape must not become selected-core input")
	require.Equal(t, "old core shape", core.GetNode(id).Meta["shape"])
}

func TestContractSourceIDBatchPreservesImportRefusals(t *testing.T) {
	for _, mode := range []string{"canceled", "namespace", "derived_output", "metadata", "scratch_closed"} {
		t.Run(mode, func(t *testing.T) {
			core := graph.New()
			node := &graph.Node{ID: "a/f.go::N", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", Meta: map[string]any{"kept": true}}
			switch mode {
			case "namespace":
				node.RepoPrefix = "other"
			case "derived_output":
				node.Kind = graph.KindContract
			case "metadata":
				node.Meta["invalid"] = func() {}
			}
			core.AddNode(node)
			e := completedNameEvidence(t, core)
			e.ledger = newContractUnitLedger()
			if mode == "canceled" {
				ctx, cancel := context.WithCancel(e.ctx)
				cancel()
				e.ctx = ctx
			}
			if mode == "scratch_closed" {
				require.NoError(t, e.scratch.Close())
			}
			e.GetNodesByIDs([]string{node.ID})
			require.Error(t, e.err)
			require.Empty(t, e.ledger.reads, "an incomplete import is never recorded as complete selected evidence")
		})
	}
}
