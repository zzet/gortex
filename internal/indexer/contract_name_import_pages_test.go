package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func namePageNode(id string) *graph.Node {
	return &graph.Node{ID: "a/" + id, Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", Meta: map[string]any{"doc": id}}
}
func TestContractNameImportPagesKeepFirstAndDerivedRows(t *testing.T) {
	first := namePageNode("duplicate")
	second := namePageNode("duplicate")
	second.Meta["doc"] = "must not replace first"
	derived := namePageNode("derived")
	derived.Meta["doc"] = "scratch wins"
	coreDerived := namePageNode("derived")
	coreDerived.Meta["doc"] = "core loses"
	r := &completedNameRows{Reader: graph.New(), rows: []*graph.Node{first, second, coreDerived}}
	e := completedNameEvidence(t, r)
	e.AddNode(derived)
	got := e.FindNodesByNames([]string{"Match"})
	require.NoError(t, e.err)
	require.Len(t, got["Match"], 2)
	found, err := e.scratch.GetNodesByIDsContext(t.Context(), []string{first.ID, derived.ID})
	require.NoError(t, err)
	require.Equal(t, "duplicate", found[first.ID].Meta["doc"])
	require.Equal(t, "scratch wins", found[derived.ID].Meta["doc"])
	require.Equal(t, 3, e.completedCoreNames["Match"], "duplicate raw rows still count")
	e.AddNode(&graph.Node{ID: "a/later", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "a"})
	require.Len(t, e.FindNodesByName("Match"), 3, "later derived scratch remains live")
}
func TestContractNameImportPagesWithholdMarkersOnFailure(t *testing.T) {
	for _, mode := range []string{"after_committed_page", "tail_read_failure", "tail_write_failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			r := &completedNameRows{Reader: graph.New()}
			n := 1
			if mode == "after_committed_page" {
				n = 129
			}
			for i := 0; i < n; i++ {
				r.rows = append(r.rows, namePageNode(fmt.Sprint(i)))
			}
			e := completedNameEvidence(t, r)
			cause := errors.New("source failed after enumeration")
			if mode == "after_committed_page" || mode == "tail_read_failure" {
				r.finish = cause
			}
			if mode == "tail_write_failure" {
				observer := &namePageObservedReader{Reader: r, finish: func() { require.NoError(t, e.scratch.Close()) }}
				e.core = observer
			}
			if mode == "canceled" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				e.ctx = ctx
				r.cancel = cancel
			}
			require.Nil(t, e.FindNodesByNames([]string{"Match"}))
			require.Error(t, e.err)
			require.Empty(t, e.completedCoreNames)
			if mode == "after_committed_page" {
				require.Equal(t, 128, e.writtenNodes)
				require.ErrorIs(t, e.err, cause)
			} else {
				require.Zero(t, e.writtenNodes)
			}
			if mode == "canceled" {
				require.ErrorIs(t, e.err, context.Canceled)
			}
		})
	}
}
func TestContractNameImportPageEncodedEnvelope(t *testing.T) {
	first := namePageNode("one")
	second := namePageNode("two")
	encoded, err := json.Marshal(struct {
		Nodes []*graph.Node
		Edges []*graph.Edge
	}{[]*graph.Node{first}, nil})
	require.NoError(t, err)
	nodeJSON, err := json.Marshal(first)
	require.NoError(t, err)
	require.Equal(t, len(encoded), contractNameImportEnvelopeBytes+len(nodeJSON))
	e := completedNameEvidence(t, graph.New())
	// Exercise the actual page algorithm with a small byte limit; production
	// always supplies the existing128MiB compact limit, avoiding a huge fixture.
	page := contractNameImportPage{evidence: e, byteLimit: len(encoded)}
	require.True(t, page.add(first))
	require.Zero(t, e.writtenNodes)
	require.True(t, page.add(second))
	require.Equal(t, 1, e.writtenNodes)
	require.True(t, page.flush())
	require.Equal(t, 2, e.writtenNodes)
	other := completedNameEvidence(t, graph.New())
	tooSmall := contractNameImportPage{evidence: other, byteLimit: len(encoded) - 1}
	require.False(t, tooSmall.add(first))
	require.ErrorIs(t, other.err, graph.ErrContractProjectionLimit)
	require.Zero(t, other.writtenNodes)
	// Multiple-node comma accounting must agree with the real envelope.
	both, err := json.Marshal(struct {
		Nodes []*graph.Node
		Edges []*graph.Edge
	}{[]*graph.Node{first, second}, nil})
	require.NoError(t, err)
	exact := completedNameEvidence(t, graph.New())
	fit := contractNameImportPage{evidence: exact, byteLimit: len(both)}
	require.True(t, fit.add(first))
	require.True(t, fit.add(second))
	require.Zero(t, exact.writtenNodes)
	require.Equal(t, len(both), contractNameImportEnvelopeBytes+fit.encodedBytes)
	require.True(t, fit.flush())
}
