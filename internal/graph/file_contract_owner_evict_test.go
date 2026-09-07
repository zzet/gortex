package graph

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fileOwnerA  = "a/provider.go"
	fileOwnerB  = "b/consumer.go"
	fileOwnerC  = "c/other.go"
	fileOwnerID = "env::FILE_EVICTION_SHARED"
)

func fileOwnerEvict(g *Graph, batch bool, path string) (int, int) {
	if batch {
		return g.EvictFiles([]string{path, path})
	}
	return g.EvictFile(path)
}

func fileOwnerNode(id, repo, path string) *Node {
	return &Node{ID: id, Name: id, Kind: KindFunction, RepoPrefix: repo, FilePath: path}
}

func fileOwnerCanonical(path, repo string, ownerBacked, removed bool) *Node {
	return &Node{ID: fileOwnerID, Name: "SHARED", QualName: "SHARED", Kind: KindContract, FilePath: path, RepoPrefix: repo,
		Meta: map[string]any{"type": "env", "role": "provider", "contract_meta": map[string]any{"var": "FILE_EVICTION_SHARED", "nested": map[string]any{"values": []any{"keep", "metadata"}}}, "contract_owner_record": ownerBacked, "contract_owner_removed": removed}}
}

func fileOwnerEdge(source *Node, path string, kind EdgeKind) *Edge {
	return &Edge{From: source.ID, To: fileOwnerID, Kind: kind, FilePath: path, Line: 7,
		Meta: map[string]any{"contract_owner_repo_prefix": source.RepoPrefix, "contract_owner_type": "env", "contract_owner_meta": map[string]any{"var": "FILE_EVICTION_SHARED", "source": source.ID}}}
}

func fileOwnerEdgeSnapshot(t *testing.T, edges []*Edge) string {
	t.Helper()
	var values []struct {
		Edge *Edge          `json:"edge"`
		Meta map[string]any `json:"meta"`
	}
	for _, edge := range edges {
		values = append(values, struct {
			Edge *Edge          `json:"edge"`
			Meta map[string]any `json:"meta"`
		}{edge, edge.Meta})
	}
	encoded, err := json.Marshal(values)
	require.NoError(t, err)
	return string(encoded)
}

func TestFileEvictionRetainsSharedContractAndPrunesLastOwner(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			name := "single/owner_backed"
			if batch {
				name = "batch/owner_backed"
			}
			if legacy {
				name += "/legacy_scalar"
			}
			t.Run(name, func(t *testing.T) {
				g := New()
				a, b := fileOwnerNode("a::Provide", "a", fileOwnerA), fileOwnerNode("b::Consume", "b", fileOwnerB)
				canonical := fileOwnerCanonical(fileOwnerA, "a", !legacy, false)
				edgeA, edgeB := fileOwnerEdge(a, fileOwnerA, EdgeProvides), fileOwnerEdge(b, fileOwnerB, EdgeConsumes)
				g.AddBatch([]*Node{a, b, canonical}, []*Edge{edgeA, edgeB})
				beforeB := fileOwnerEdgeSnapshot(t, g.GetOutEdges(b.ID))
				beforeRevision := g.nodeMutGen.Load()
				token := g.BeginMutationReceipt()
				nodes, edges := fileOwnerEvict(g, batch, fileOwnerA)
				receipt := g.EndMutationReceipt(token)
				assert.Equal(t, 1, nodes)
				assert.Equal(t, 1, edges)
				assert.Nil(t, g.GetNode(a.ID))
				assert.Same(t, b, g.GetNode(b.ID))
				current := g.GetNode(fileOwnerID)
				assert.NotNil(t, current, "surviving cross-repository owner retains shared canonical ID")
				assert.Equal(t, beforeB, fileOwnerEdgeSnapshot(t, g.GetOutEdges(b.ID)))
				if current != nil && legacy {
					assert.NotSame(t, canonical, current)
					assert.Equal(t, false, canonical.Meta["contract_owner_removed"], "escaped old pointer is not mutated")
					assert.Equal(t, true, current.Meta["contract_owner_removed"])
					assert.Equal(t, canonical.Meta["contract_meta"], current.Meta["contract_meta"])
					assert.Equal(t, []*Node{current}, g.GetFileNodes(fileOwnerA), "file index contains replacement pointer")
					assert.Greater(t, g.nodeMutGen.Load(), beforeRevision)
					assert.False(t, receipt.Complete, "removed scalar record is a semantic mutation")
				}
				// The canonical scalar still names A, not B: last-owner cleanup
				// must follow B's outgoing ownership target outside the B bucket.
				nodes, edges = fileOwnerEvict(g, batch, fileOwnerB)
				assert.Equal(t, 2, nodes)
				assert.Equal(t, 1, edges)
				assert.Zero(t, g.NodeCount())
				assert.Zero(t, g.EdgeCount())
			})
		}
	}
}

func TestFileEvictionPreservesOffFrontierLegacyScalar(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct {
			name                  string
			backed, removed, keep bool
		}{
			{name: "genuine_legacy_scalar", keep: true},
			{name: "already_removed_scalar", removed: true},
			{name: "owner_backed_scalar", backed: true},
		} {
			name := "single/" + tc.name
			if batch {
				name = "batch/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				g := New()
				a, b := fileOwnerNode("a::Provide", "a", fileOwnerA), fileOwnerNode("b::Source", "b", fileOwnerB)
				canonical := fileOwnerCanonical(fileOwnerB, "b", tc.backed, tc.removed)
				g.AddBatch([]*Node{a, b, canonical}, []*Edge{fileOwnerEdge(a, fileOwnerA, EdgeProvides)})
				fileOwnerEvict(g, batch, fileOwnerA)
				if tc.keep {
					assert.Same(t, canonical, g.GetNode(fileOwnerID), "A's final incoming owner removal does not delete B's recoverable scalar-only record")
				} else {
					assert.Nil(t, g.GetNode(fileOwnerID), "removed or owner-backed off-file scalar cannot keep an orphan alive")
				}
				assert.Empty(t, g.GetInEdges(fileOwnerID))
				fileOwnerEvict(g, batch, fileOwnerB)
				assert.Zero(t, g.NodeCount(), "deleting B itself removes its scalar-only record")
				assert.Zero(t, g.EdgeCount())
			})
		}
	}
}

func TestFileEvictionDoesNotCountInvalidOutsideOwner(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			name := "single/wrong_owner_repo"
			if batch {
				name = "batch/wrong_owner_repo"
			}
			if missing {
				name += "/missing_source"
			}
			t.Run(name, func(t *testing.T) {
				g := New()
				a, b := fileOwnerNode("a::Provide", "a", fileOwnerA), fileOwnerNode("b::Consume", "b", fileOwnerB)
				edgeB := fileOwnerEdge(b, fileOwnerB, EdgeConsumes)
				nodes := []*Node{a, fileOwnerCanonical(fileOwnerA, "a", true, false)}
				if !missing {
					nodes = append(nodes, b)
					edgeB.Meta["contract_owner_repo_prefix"] = "wrong"
				}
				g.AddBatch(nodes, []*Edge{fileOwnerEdge(a, fileOwnerA, EdgeProvides), edgeB})
				fileOwnerEvict(g, batch, fileOwnerA)
				assert.Nil(t, g.GetNode(fileOwnerID))
				assert.Zero(t, g.EdgeCount(), "unrecoverable owner row cannot retain the canonical")
			})
		}
	}
}

func TestFileEvictionOnlyCleansRecordPathsOnTouchedCanonicals(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, touched := range []bool{false, true} {
			name := "single/outside_endpoints_unchanged"
			if batch {
				name = "batch/outside_endpoints_unchanged"
			}
			if touched {
				name += "/canonical_in_frontier"
			}
			t.Run(name, func(t *testing.T) {
				g := New()
				a, b, c := fileOwnerNode("a::Source", "a", fileOwnerA), fileOwnerNode("a::BoundHandler", "a", "a/handler.go"), fileOwnerNode("c::Owner", "c", fileOwnerC)
				canonicalPath := fileOwnerC
				if touched {
					canonicalPath = fileOwnerA
				}
				canonical := fileOwnerCanonical(canonicalPath, "a", true, false)
				g.AddBatch([]*Node{a, b, c, canonical}, []*Edge{fileOwnerEdge(b, fileOwnerA, EdgeProvides), fileOwnerEdge(c, fileOwnerC, EdgeHandlesRoute)})
				beforeB, beforeC := fileOwnerEdgeSnapshot(t, g.GetOutEdges(b.ID)), fileOwnerEdgeSnapshot(t, g.GetOutEdges(c.ID))
				_, edges := fileOwnerEvict(g, batch, fileOwnerA)
				assert.NotNil(t, g.GetNode(fileOwnerID))
				assert.Equal(t, beforeC, fileOwnerEdgeSnapshot(t, g.GetOutEdges(c.ID)), "unaffected C keeps exact metadata")
				if touched {
					assert.Equal(t, 1, edges)
					assert.Empty(t, g.GetOutEdges(b.ID), "incident A-owned record is removed even when its bound handler survives")
				} else {
					assert.Zero(t, edges)
					assert.Equal(t, beforeB, fileOwnerEdgeSnapshot(t, g.GetOutEdges(b.ID)), "do not broaden file eviction into global edge.FilePath cleanup")
				}
			})
		}
	}
}

func TestFileEvictionPreservesEmptyPathAndOrdinaryRevisionBehavior(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, path := range []string{"", "ordinary.go"} {
			name := "single/" + path
			if batch {
				name = "batch/" + path
			}
			t.Run(name, func(t *testing.T) {
				g := New()
				g.AddNode(&Node{ID: "ordinary", Kind: KindFile, FilePath: path})
				before := g.nodeMutGen.Load()
				nodes, edges := fileOwnerEvict(g, batch, path)
				assert.Zero(t, edges)
				if batch && path == "" {
					assert.Zero(t, nodes)
					assert.NotNil(t, g.GetNode("ordinary"))
				} else {
					assert.Equal(t, 1, nodes)
					assert.Nil(t, g.GetNode("ordinary"))
				}
				want := before
				if !batch {
					want++
				}
				assert.Equal(t, want, g.nodeMutGen.Load(), "preserve existing direct-versus-batch deletion revision behavior explicitly")
			})
		}
	}
}

func TestFileEvictionSelectiveOwnerRemovalUsesStoredHashes(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			g := New()
			a, b, c := fileOwnerNode("a::Source", "a", fileOwnerA), fileOwnerNode("a::Handler", "a", "a/handler.go"), fileOwnerNode("c::Owner", "c", fileOwnerC)
			bound := fileOwnerEdge(b, fileOwnerA, EdgeProvides)
			g.AddBatch([]*Node{a, b, c, fileOwnerCanonical(fileOwnerA, "a", true, false)}, []*Edge{bound, fileOwnerEdge(c, fileOwnerC, EdgeConsumes)})
			require.Len(t, g.GetOutEdges(b.ID), 1)
			require.Len(t, g.GetInEdges(fileOwnerID), 2)
			sourceShard := g.shardFor(b.ID)
			beforeRepoEdges := sourceShard.repoEdgeCount["a"]
			require.Positive(t, beforeRepoEdges)
			beforeC := fileOwnerEdgeSnapshot(t, g.GetOutEdges(c.ID))
			// A non-endpoint identity field changes before the buckets are
			// reindexed. Endpoints remain stable, as existing evictEdgesLocked
			// requires; the insertion-time hash must still remove the entry.
			bound.Line++
			_, edges := fileOwnerEvict(g, batch, fileOwnerA)
			assert.Equal(t, 1, edges)
			assert.Empty(t, g.GetOutEdges(b.ID), "old indexed source bucket is cleared")
			assert.Len(t, g.GetInEdges(fileOwnerID), 1, "old indexed target bucket retains only C")
			assert.Equal(t, beforeRepoEdges-1, sourceShard.repoEdgeCount["a"], "debit the inserted source repository counter")
			assert.Equal(t, beforeC, fileOwnerEdgeSnapshot(t, g.GetOutEdges(c.ID)))
			assert.NotNil(t, g.GetNode(fileOwnerID))
		})
	}
}

func TestFileEvictionClosesOnlyNewlyDoomedOwnerChains(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, directC := range []bool{false, true} {
			for _, legacyC := range []bool{false, true} {
				name := "single/chain"
				if batch {
					name = "batch/chain"
				}
				if directC {
					name += "/A_also_owns_C"
				}
				if legacyC {
					name += "/legacy_C"
				}
				t.Run(name, func(t *testing.T) {
					g := New()
					a := fileOwnerNode("a::Source", "a", fileOwnerA)
					b := fileOwnerCanonical(fileOwnerB, "b", true, false)
					b.ID, b.Name, b.QualName = "env::CHAIN_B", "CHAIN_B", "CHAIN_B"
					c := fileOwnerCanonical(fileOwnerC, "c", !legacyC, false)
					c.ID, c.Name, c.QualName = "env::CHAIN_C", "CHAIN_C", "CHAIN_C"
					aToB := fileOwnerEdge(a, fileOwnerA, EdgeProvides)
					aToB.To = b.ID
					bToC := fileOwnerEdge(b, fileOwnerB, EdgeConsumes)
					bToC.To = c.ID
					edges := []*Edge{aToB, bToC}
					if directC {
						aToC := fileOwnerEdge(a, fileOwnerA, EdgeProvides)
						aToC.To = c.ID
						edges = append(edges, aToC)
					}
					g.AddBatch([]*Node{a, b, c}, edges)
					require.Equal(t, 3, g.NodeCount())
					require.Equal(t, len(edges), g.EdgeCount())
					beforeC, err := json.Marshal(c)
					require.NoError(t, err)
					beforeMeta, err := json.Marshal(c.Meta)
					require.NoError(t, err)
					nodesRemoved, edgesRemoved := fileOwnerEvict(g, batch, fileOwnerA)
					assert.Nil(t, g.GetNode(a.ID))
					assert.Nil(t, g.GetNode(b.ID), "off-file B loses its final owner A")
					if legacyC {
						assert.Equal(t, 2, nodesRemoved)
						assert.Same(t, c, g.GetNode(c.ID), "genuine off-frontier scalar C protects the record without incoming owners")
						afterC, marshalErr := json.Marshal(g.GetNode(c.ID))
						require.NoError(t, marshalErr)
						assert.Equal(t, string(beforeC), string(afterC))
						afterMeta, marshalErr := json.Marshal(c.Meta)
						require.NoError(t, marshalErr)
						assert.Equal(t, string(beforeMeta), string(afterMeta))
					} else {
						assert.Equal(t, 3, nodesRemoved)
						assert.Nil(t, g.GetNode(c.ID), "newly doomed B cannot keep C alive, including when C was already touched by A")
					}
					assert.Equal(t, len(edges), edgesRemoved)
					assert.Zero(t, g.EdgeCount())
					assert.Empty(t, g.GetOutEdges(b.ID))
					assert.Empty(t, g.GetInEdges(c.ID))
					if legacyC {
						fileOwnerEvict(g, batch, fileOwnerC)
					}
					assert.Zero(t, g.NodeCount())
				})
			}
		}
	}
}
