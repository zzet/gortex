package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Verbatim affe production lookup, benchmark reference only. It includes checked
// core visit/import and always performs the same live scratch query.
func legacyContractCompletedNameReference(e *contractFollowupEvidence, names []string) map[string][]*graph.Node {
	count := 0
	err := graph.VisitNodesByNamesContext(e.ctx, e.core, names, func(node *graph.Node) bool {
		count++
		if count > graph.ContractProjectionRowLimit {
			e.fail(graph.ErrContractProjectionLimit)
			return false
		}
		if node == nil {
			return true
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			return true
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return false
		}
		prior, readErr := e.scratch.GetNodesByIDsContext(e.ctx, []string{copyNode.ID})
		if readErr != nil {
			e.fail(readErr)
			return false
		}
		if prior[copyNode.ID] == nil {
			e.AddNode(copyNode)
		}
		return true
	})
	if err != nil {
		e.fail(err)
	}
	if e.err != nil {
		return nil
	}
	out := make(map[string][]*graph.Node)
	err = graph.VisitNodesByNamesContext(e.ctx, e.scratch, names, func(node *graph.Node) bool { out[node.Name] = append(out[node.Name], node); return true })
	if err != nil {
		e.fail(err)
		return nil
	}
	return out
}

// Each arm reuses warm scratch rows while creating fresh evidence per operation.
// Sixteen repeated lookups measure this fixture only, not the historical cohort.
func BenchmarkContractCompletedCoreNames(b *testing.B) {
	s, err := store_sqlite.Open(filepath.Join(b.TempDir(), "source.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	for _, repo := range []string{"a", "b", "c", "d"} {
		for i := range 16 {
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s/f::Shared%d", repo, i), Name: "Shared", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/f", Meta: map[string]any{"owner": repo}})
		}
	}
	require.NoError(b, s.AddBatchChecked(nodes, nil))
	for _, mode := range []string{"legacy", "completed"} {
		b.Run(mode, func(b *testing.B) {
			readers := map[string]graph.Reader{}
			var counts []*capturedRepoNameCounter
			for _, repo := range []string{"a", "b", "c", "d"} {
				r := &capturedRepoNameCounter{Reader: s, store: s}
				readers[repo] = r
				counts = append(counts, r)
			}
			core := &contractCapturedCore{Reader: s, readers: readers}
			scratch, err := store_sqlite.Open(filepath.Join(b.TempDir(), "scratch.sqlite"))
			require.NoError(b, err)
			b.Cleanup(func() { _ = scratch.Close() })
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				e := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: context.Background(), core: core, allowedRepos: map[string]bool{"a": true, "b": true, "c": true, "d": true}}
				for range 16 {
					var got map[string][]*graph.Node
					if mode == "legacy" {
						got = legacyContractCompletedNameReference(e, []string{"Shared", "Missing"})
					} else {
						got = e.FindNodesByNames([]string{"Shared", "Missing"})
					}
					if e.err != nil || len(got["Shared"]) != 64 || len(got["Missing"]) != 0 {
						b.Fatalf("parity: counts=%d/%d err=%v", len(got["Shared"]), len(got["Missing"]), e.err)
					}
				}
			}
			b.StopTimer()
			visits, decoded := 0, 0
			for _, r := range counts {
				visits += r.scoped
				decoded += r.decoded
				require.Zero(b, r.broad)
			}
			b.ReportMetric(16, "lookups/op")
			b.ReportMetric(float64(visits)/float64(b.N), "scoped_core_visits/op")
			b.ReportMetric(float64(decoded)/float64(b.N), "decoded_core_rows/op")
		})
	}
}
