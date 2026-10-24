package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReindexEdgesChainedRetargetBatch(t *testing.T) {
	for _, delta := range []bool{false, true} {
		for _, crossSource := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				for _, unresolved := range []bool{false, true} {
					t.Run(fmt.Sprintf("delta=%t/crossSource=%t/reverse=%t/unresolved=%t", delta, crossSource, reverse, unresolved), func(t *testing.T) {
						y, z := "b::Y", "b::Z"
						if unresolved {
							y, z = "unresolved::Y", "unresolved::Z"
						}
						first := &Edge{From: "a::A", To: "b::X", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
						second := cloneDeltaEdge(first)
						second.To = y
						store, g := retargetBatchStore(delta, first, second)
						wantFirst, wantSecond := cloneDeltaEdge(first), cloneDeltaEdge(second)
						wantFirst.To, wantSecond.To = y, z
						batch := []EdgeReindex{{Edge: wantFirst, OldTo: first.To}, {Edge: wantSecond, OldTo: second.To}}
						if crossSource {
							wantSecond.From = "c::C"
							batch[1].OldFrom = second.From
							batch[1].OldFilePath = second.FilePath
							batch[1].OldLine = second.Line
							batch[1].RefreshIdentity = true
						}
						// Graph callers mutate live rows; DeltaWriter returns copies.
						byTarget := make(map[string]*Edge)
						for _, row := range store.GetOutEdges(first.From) {
							byTarget[row.To] = row
						}
						for i := range batch {
							row := byTarget[batch[i].OldTo]
							require.NotNil(t, row)
							*row = *batch[i].Edge
							batch[i].Edge = row
						}
						if reverse {
							batch[0], batch[1] = batch[1], batch[0]
						}
						receipts := store.(MutationReceiptStore)
						token := receipts.BeginMutationReceipt()
						store.ReindexEdges(batch)
						receipt := receipts.EndMutationReceipt(token)
						if crossSource && !delta {
							assert.False(t, receipt.Complete, "Graph refreshes intentionally void receipts")
						} else {
							wantReceipt := MutationReceipt{Complete: true}
							if unresolved {
								wantReceipt.ResolutionRelevant = true
								wantReceipt.ChangedFiles = []string{"a.go"}
								wantReceipt.UnresolvedFiles = []string{"a.go"}
								wantReceipt.TargetNames = []string{"Y", "Z"}
								wantReceipt.TargetIDs = []string{y, z}
							}
							assert.Equal(t, wantReceipt, receipt)
						}
						assertRetargetBatchLifecycle(t, store, g, wantFirst, wantSecond)
					})
				}
			}
		}
	}
}

func retargetBatchStore(delta bool, edges ...*Edge) (Store, *Graph) {
	g := New()
	for _, e := range edges {
		g.AddEdge(cloneDeltaEdge(e))
	}
	if delta {
		dw := NewDeltaWriter(g, nil)
		return dw, dw.work
	}
	return g, g
}

func assertRetargetBatchLifecycle(t *testing.T, store Store, g *Graph, survivors ...*Edge) {
	t.Helper()
	require.Len(t, store.AllEdges(), len(survivors), "batch must preserve all distinct survivors")
	require.ElementsMatch(t, survivors, store.AllEdges())
	for _, from := range []string{"a::A", "c::C"} {
		var want []*Edge
		for _, e := range survivors {
			if e.From == from {
				want = append(want, e)
			}
		}
		assert.ElementsMatch(t, want, store.GetOutEdges(from))
	}
	for _, to := range []string{"b::X", "b::Y", "b::Z", "unresolved::Y", "unresolved::Z"} {
		var want []*Edge
		for _, e := range survivors {
			if e.To == to {
				want = append(want, e)
			}
		}
		assert.ElementsMatch(t, want, store.GetInEdges(to))
	}
	require.NoError(t, g.VerifyEdgeIdentities())
	for _, e := range survivors {
		store.AddEdge(cloneDeltaEdge(e))
	}
	require.ElementsMatch(t, survivors, store.AllEdges(), "AddEdge must replace survivors, not add slots")
	require.NoError(t, g.VerifyEdgeIdentities())
	for _, e := range survivors {
		require.True(t, store.RemoveEdge(e.From, e.To, e.Kind))
		assert.Empty(t, store.GetInEdges(e.To))
	}
	assert.Empty(t, store.AllEdges())
	assert.Empty(t, store.GetOutEdges("a::A"))
	assert.Empty(t, store.GetOutEdges("c::C"))
	require.NoError(t, g.VerifyEdgeIdentities())
}

func TestReindexEdgesTrueRetargetConvergence(t *testing.T) {
	for _, delta := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, unresolved := range []bool{false, true} {
				t.Run(fmt.Sprintf("delta=%t/reverse=%t/unresolved=%t", delta, reverse, unresolved), func(t *testing.T) {
					first := &Edge{From: "a::A", To: "b::X", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
					second := cloneDeltaEdge(first)
					second.To = "b::Y"
					store, g := retargetBatchStore(delta, first, second)
					survivor := cloneDeltaEdge(first)
					survivor.To = "b::Z"
					if unresolved {
						survivor.To = "unresolved::Z"
					}
					batch := []EdgeReindex{{Edge: cloneDeltaEdge(survivor), OldTo: first.To}, {Edge: cloneDeltaEdge(survivor), OldTo: second.To}}
					byTarget := make(map[string]*Edge)
					for _, row := range store.GetOutEdges(first.From) {
						byTarget[row.To] = row
					}
					for i := range batch {
						row := byTarget[batch[i].OldTo]
						require.NotNil(t, row)
						*row = *batch[i].Edge
						batch[i].Edge = row
					}
					if reverse {
						batch[0], batch[1] = batch[1], batch[0]
					}
					receipts := store.(MutationReceiptStore)
					token := receipts.BeginMutationReceipt()
					store.ReindexEdges(batch)
					receipt := receipts.EndMutationReceipt(token)
					wantReceipt := MutationReceipt{Complete: true}
					if unresolved {
						wantReceipt.ResolutionRelevant = true
						wantReceipt.ChangedFiles = []string{"a.go"}
						wantReceipt.UnresolvedFiles = []string{"a.go"}
						wantReceipt.TargetNames = []string{"Z"}
						wantReceipt.TargetIDs = []string{survivor.To}
					}
					assert.Equal(t, wantReceipt, receipt)
					assertRetargetBatchLifecycle(t, store, g, survivor)
				})
			}
		}
	}
}

func TestReindexEdgesUnchangedRetargetBatch(t *testing.T) {
	for _, delta := range []bool{false, true} {
		t.Run(fmt.Sprintf("delta=%t", delta), func(t *testing.T) {
			edge := &Edge{From: "a::A", To: "unresolved::Y", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
			store, g := retargetBatchStore(delta, edge)
			receipts := store.(MutationReceiptStore)
			token := receipts.BeginMutationReceipt()
			emptyReceipt := receipts.EndMutationReceipt(token)
			token = receipts.BeginMutationReceipt()
			store.ReindexEdges([]EdgeReindex{{}, {Edge: cloneDeltaEdge(edge), OldTo: edge.To}})
			assert.Equal(t, emptyReceipt, receipts.EndMutationReceipt(token), "unchanged unresolved rows must not create resolver work")
			assertRetargetBatchLifecycle(t, store, g, edge)
		})
	}
}

func TestDeltaWriterRetargetBatchPreservesStoredRowSemantics(t *testing.T) {
	base := New()
	edge := &Edge{From: "a::A", To: "unresolved::Y", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
	base.AddEdge(cloneDeltaEdge(edge))
	dw := NewDeltaWriter(base, nil)
	updated := cloneDeltaEdge(edge)
	updated.Confidence = 0.8
	missing := cloneDeltaEdge(edge)
	missing.To = "unresolved::Z"
	missing.Line++
	token := dw.BeginMutationReceipt()
	dw.ReindexEdges([]EdgeReindex{{Edge: updated, OldTo: edge.To}, {Edge: missing, OldTo: edge.To}})
	assert.Equal(t, MutationReceipt{Complete: true}, dw.EndMutationReceipt(token))
	assertRetargetBatchLifecycle(t, dw, dw.work, updated)
	assert.Equal(t, []*Edge{edge}, base.AllEdges(), "copied payload must not mutate the base")
}

func TestDeltaWriterReindexDuplicateOldIdentityDifferentTargets(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%t", refresh), func(t *testing.T) {
			base := New()
			edge := &Edge{From: "a::A", To: "unresolved::foo", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
			base.AddEdge(cloneDeltaEdge(edge))
			dw := NewDeltaWriter(base, nil)
			first, second := cloneDeltaEdge(edge), cloneDeltaEdge(edge)
			first.To, second.To = "b::X", "b::Y"
			dw.ReindexEdges([]EdgeReindex{
				{Edge: first, OldTo: edge.To, RefreshIdentity: refresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
				{Edge: second, OldTo: edge.To, RefreshIdentity: refresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
			})
			assert.Equal(t, []*Edge{first}, dw.GetOutEdges(edge.From), "the first row wins")
			assert.Equal(t, []*Edge{first}, dw.GetInEdges(first.To))
			assert.Empty(t, dw.GetInEdges(edge.To))
			assert.Empty(t, dw.GetInEdges(second.To), "a skipped row must not leave a phantom incoming edge")
			assert.NoError(t, dw.VerifyEdgeIdentities())
			require.True(t, dw.RemoveEdge(edge.From, first.To, edge.Kind))
			assert.Empty(t, dw.GetOutEdges(edge.From))
			assert.Empty(t, dw.GetInEdges(first.To))
			assert.Empty(t, dw.GetInEdges(second.To), "removing the survivor must leave both buckets empty")
			assert.Empty(t, dw.AllEdges())
		})
	}
}

func TestDeltaWriterReindexDuplicateOldIdentitySameTarget(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%t", refresh), func(t *testing.T) {
			base := New()
			edge := &Edge{From: "a::A", To: "unresolved::foo", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
			base.AddEdge(cloneDeltaEdge(edge))
			dw := NewDeltaWriter(base, nil)
			updated := cloneDeltaEdge(edge)
			updated.To = "b::X"
			dw.ReindexEdges([]EdgeReindex{
				{Edge: updated, OldTo: edge.To, RefreshIdentity: refresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
				{Edge: cloneDeltaEdge(updated), OldTo: edge.To, RefreshIdentity: refresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
			})
			// DeltaWriter reads return copies; inspect the working graph's stored pointers.
			out, in := dw.work.GetOutEdges(edge.From), dw.work.GetInEdges(updated.To)
			require.Len(t, out, 1)
			require.Len(t, in, 1)
			assert.Same(t, out[0], in[0], "incoming and outgoing slots must share the stored row")
			assert.NoError(t, dw.VerifyEdgeIdentities())
			updated.Confidence = 0.9
			dw.ReindexEdges([]EdgeReindex{{Edge: updated, OldTo: updated.To}})
			assert.Equal(t, []*Edge{updated}, dw.GetOutEdges(edge.From))
			assert.Equal(t, []*Edge{updated}, dw.GetInEdges(updated.To), "payload updates must reach the incoming row")
			assert.NoError(t, dw.VerifyEdgeIdentities())
		})
	}
}

func TestDeltaWriterRetargetPreservesExistingDestinationPayload(t *testing.T) {
	for _, target := range []string{"b::Y", "unresolved::Y"} {
		t.Run(target, func(t *testing.T) {
			existing := &Edge{From: "a::A", To: target, Kind: EdgeReads, FilePath: "a.go", Line: 10,
				Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: "ast_resolved"}
			candidate := cloneDeltaEdge(existing)
			candidate.To = "unresolved::valueref::Y"
			if IsUnresolvedTarget(target) {
				candidate.To = "b::X"
			}
			candidate.ConfidenceLabel = ""
			candidate.Meta = map[string]any{"synthesized_by": "value-ref"}
			store, g := retargetBatchStore(true, existing, candidate)
			dw := store.(*DeltaWriter)
			updated := cloneDeltaEdge(candidate)
			updated.To = target
			token := dw.BeginMutationReceipt()
			batch := []EdgeReindex{{Edge: updated, OldTo: candidate.To}}
			if !IsUnresolvedTarget(target) {
				batch = append(batch, EdgeReindex{Edge: cloneDeltaEdge(existing), OldTo: target,
					OldFilePath: existing.FilePath, OldLine: existing.Line, RefreshIdentity: true})
			}
			dw.ReindexEdges(batch)
			assert.Equal(t, MutationReceipt{Complete: true}, dw.EndMutationReceipt(token),
				"an existing destination creates no new resolver work")
			assert.Empty(t, dw.GetInEdges(candidate.To))
			out, in := g.GetOutEdges(existing.From), g.GetInEdges(target)
			require.Len(t, out, 1)
			require.Len(t, in, 1)
			assert.Same(t, out[0], in[0])
			assertRetargetBatchLifecycle(t, dw, g, existing)
		})
	}
}

func TestDeltaWriterRetargetPreservesRefreshedDestinationPayload(t *testing.T) {
	for _, target := range []string{"b::Y", "unresolved::Y"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("target=%s/reverse=%t", target, reverse), func(t *testing.T) {
				existing := &Edge{From: "a::A", To: target, Kind: EdgeReads, FilePath: "a.go", Line: 10,
					Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: "ast_resolved",
					Meta: map[string]any{"version": "stored"}}
				candidate := cloneDeltaEdge(existing)
				candidate.To = "b::X"
				candidate.Meta = map[string]any{"version": "candidate"}
				store, g := retargetBatchStore(true, existing, candidate)
				dw := store.(*DeltaWriter)
				refreshed := cloneDeltaEdge(existing)
				refreshed.Confidence = 0.9
				refreshed.Meta = map[string]any{"version": "refreshed"}
				retargeted := cloneDeltaEdge(candidate)
				retargeted.To = target
				batch := []EdgeReindex{
					{Edge: refreshed, OldTo: target, OldFilePath: existing.FilePath, OldLine: existing.Line, RefreshIdentity: true},
					{Edge: retargeted, OldTo: candidate.To},
				}
				if reverse {
					batch[0], batch[1] = batch[1], batch[0]
				}
				token := dw.BeginMutationReceipt()
				dw.ReindexEdges(batch)
				wantReceipt := MutationReceipt{Complete: true}
				if IsUnresolvedTarget(target) {
					wantReceipt.ResolutionRelevant = true
					wantReceipt.ChangedFiles = []string{"a.go"}
					wantReceipt.UnresolvedFiles = []string{"a.go"}
					wantReceipt.TargetNames = []string{"Y"}
					wantReceipt.TargetIDs = []string{target}
				}
				assert.Equal(t, wantReceipt, dw.EndMutationReceipt(token))
				assert.Empty(t, dw.GetInEdges(candidate.To))
				out, in := g.GetOutEdges(existing.From), g.GetInEdges(target)
				require.Len(t, out, 1)
				require.Len(t, in, 1)
				assert.Same(t, out[0], in[0])
				assertRetargetBatchLifecycle(t, dw, g, refreshed)
			})
		}
	}
}

func TestDeltaWriterReindexRepeatedOldIdentityInOrder(t *testing.T) {
	for _, retarget := range []bool{false, true} {
		for _, refresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("retarget=%t/refresh=%t", retarget, refresh), func(t *testing.T) {
				edge := &Edge{From: "a::A", To: "b::X", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
				store, g := retargetBatchStore(true, edge)
				first, last := cloneDeltaEdge(edge), cloneDeltaEdge(edge)
				first.Origin, last.Origin = "first", "last"
				if retarget {
					last.To = "unresolved::Y"
				}
				receipts := store.(MutationReceiptStore)
				token := receipts.BeginMutationReceipt()
				store.ReindexEdges([]EdgeReindex{
					{Edge: first, OldTo: edge.To, RefreshIdentity: refresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
					{Edge: last, OldTo: edge.To, RefreshIdentity: refresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
				})
				receipt := receipts.EndMutationReceipt(token)
				assert.True(t, receipt.Complete)
				if retarget {
					assert.Equal(t, []string{"unresolved::Y"}, receipt.TargetIDs)
				} else {
					assert.Empty(t, receipt.TargetIDs)
				}
				assertRetargetBatchLifecycle(t, store, g, last)
			})
		}
	}
}

func TestDeltaWriterReindexMixedPayloadRefreshesInOrder(t *testing.T) {
	for _, firstRefresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("firstRefresh=%t", firstRefresh), func(t *testing.T) {
			edge := &Edge{From: "a::A", To: "b::X", Kind: EdgeCalls, FilePath: "a.go", Line: 10}
			store, g := retargetBatchStore(true, edge)
			first, last := cloneDeltaEdge(edge), cloneDeltaEdge(edge)
			first.Origin, last.Origin = "first", "last"
			store.ReindexEdges([]EdgeReindex{
				{Edge: first, OldTo: edge.To, RefreshIdentity: firstRefresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
				{Edge: last, OldTo: edge.To, RefreshIdentity: !firstRefresh, OldFilePath: edge.FilePath, OldLine: edge.Line},
			})
			assertRetargetBatchLifecycle(t, store, g, last)
		})
	}
}
