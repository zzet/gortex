package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeltaWriterRefreshIdentityConvergenceSingleSlot(t *testing.T) {
	for _, moveSource := range []bool{false, true} {
		for _, target := range []string{"b::B", "unresolved::B"} {
			t.Run(fmt.Sprintf("moveSource=%t/target=%s", moveSource, target), func(t *testing.T) {
				base := New()
				base.AddNode(&Node{ID: "a::A", RepoPrefix: "repo"})
				base.AddNode(&Node{ID: "c::C", RepoPrefix: "repo"})
				existing := &Edge{From: "a::A", To: target, Kind: EdgeCalls, FilePath: "a.go", Line: 3, Origin: "stationary"}
				moving := *existing
				moving.Line = 9
				moving.Origin = "mover"
				if moveSource {
					moving.From = "c::C"
				}
				base.AddEdge(cloneDeltaEdge(existing))
				base.AddEdge(cloneDeltaEdge(&moving))
				dw := NewDeltaWriter(base, nil)
				require.Len(t, dw.AllEdges(), 2)
				token := dw.BeginMutationReceipt()
				updated := cloneDeltaEdge(&moving)
				updated.From, updated.Line = existing.From, existing.Line
				dw.ReindexEdges([]EdgeReindex{{
					Edge: updated, OldFrom: moving.From, OldTo: moving.To,
					OldFilePath: moving.FilePath, OldLine: moving.Line, RefreshIdentity: true,
				}})
				receipt := dw.EndMutationReceipt(token)
				assert.True(t, receipt.Complete, "delta refresh must preserve exact receipts")
				assert.Empty(t, receipt.UnresolvedFiles, "the stationary destination creates no resolver work")
				assert.Empty(t, receipt.ChangedFiles)
				assert.Equal(t, []*Edge{existing}, dw.GetOutEdges("a::A"))
				assert.Len(t, dw.GetInEdges(target), 1)
				assert.Len(t, dw.AllEdges(), 1, "cached outgoing slots must converge too")
				assert.Empty(t, dw.GetOutEdges("c::C"))
				require.NoError(t, dw.work.VerifyEdgeIdentities())
				// Replacing and removing the survivor must use consistent sidecars.
				dw.AddEdge(cloneDeltaEdge(existing))
				assert.Len(t, dw.AllEdges(), 1)
				require.True(t, dw.RemoveEdge(existing.From, target, EdgeCalls))
				assert.Empty(t, dw.GetOutEdges("a::A"))
				assert.Empty(t, dw.GetInEdges(target))
				assert.Empty(t, dw.AllEdges())
				require.NoError(t, dw.work.VerifyEdgeIdentities())
			})
		}
	}
}

func TestDeltaWriterRefreshIdentityPreservesMovingSiblings(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			base := New()
			first := &Edge{From: "a::A", To: "b::B", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
			second := cloneDeltaEdge(first)
			second.Line = 11
			base.AddEdge(first)
			base.AddEdge(second)
			dw := NewDeltaWriter(base, nil)
			batch := []EdgeReindex{}
			for _, e := range []*Edge{first, second} {
				updated := cloneDeltaEdge(e)
				updated.Line++
				batch = append(batch, EdgeReindex{Edge: updated, OldTo: e.To,
					OldFilePath: e.FilePath, OldLine: e.Line, RefreshIdentity: true})
			}
			if reverse {
				batch[0], batch[1] = batch[1], batch[0]
			}
			dw.ReindexEdges(batch)
			out := dw.GetOutEdges("a::A")
			require.Len(t, out, 2, "a moving sibling's old key is not convergence")
			assert.ElementsMatch(t, []int{11, 12}, []int{out[0].Line, out[1].Line})
			assert.Len(t, dw.GetInEdges("b::B"), 2)
			require.NoError(t, dw.work.VerifyEdgeIdentities())
		})
	}
}
