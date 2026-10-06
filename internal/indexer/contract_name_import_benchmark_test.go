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

// Both arms use warm on-disk SQLite stores but remove the prior scratch rows
// outside the timer. Each measured first lookup imports256 matching rows into
// fresh evidence; no claim about the historical cohort's match distribution.
func BenchmarkContractNameImportPages(b *testing.B) {
	source, err := store_sqlite.Open(filepath.Join(b.TempDir(), "source.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { _ = source.Close() })
	var nodes []*graph.Node
	for _, repo := range []string{"a", "b", "c", "d"} {
		for i := 0; i < 64; i++ {
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s/f::Match%03d", repo, i), Name: "Match", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/f", Meta: map[string]any{"doc": "persisted payload", "owner": repo}})
		}
	}
	require.NoError(b, source.AddBatchChecked(nodes, nil))
	for _, mode := range []string{"single_node_reference", "pages"} {
		b.Run(mode, func(b *testing.B) {
			scratch, err := store_sqlite.Open(filepath.Join(b.TempDir(), "scratch.sqlite"))
			require.NoError(b, err)
			b.Cleanup(func() { _ = scratch.Close() })
			readers := map[string]graph.Reader{}
			var counts []*capturedRepoNameCounter
			for _, repo := range []string{"a", "b", "c", "d"} {
				r := &capturedRepoNameCounter{Reader: source, store: source}
				readers[repo] = r
				counts = append(counts, r)
			}
			core := &contractCapturedCore{Reader: source, readers: readers}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				removed := 0
				for _, repo := range []string{"a", "b", "c", "d"} {
					n, edges, resetErr := scratch.EvictRepoAllGenerationsChecked(repo)
					require.NoError(b, resetErr)
					require.Zero(b, edges)
					removed += n
				}
				if i != 0 {
					require.Equal(b, 256, removed)
				}
				b.StartTimer()
				e := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: context.Background(), core: core, allowedRepos: map[string]bool{"a": true, "b": true, "c": true, "d": true}}
				var got map[string][]*graph.Node
				if mode == "single_node_reference" {
					got = legacyContractCompletedNameReference(e, []string{"Match"})
				} else {
					got = e.FindNodesByNames([]string{"Match"})
				}
				if e.err != nil || len(got["Match"]) != 256 || e.writtenNodes != 256 {
					b.Fatalf("parity rows=%d written=%d err=%v", len(got["Match"]), e.writtenNodes, e.err)
				}
			}
			b.StopTimer()
			decoded, visits := 0, 0
			for _, r := range counts {
				decoded += r.decoded
				visits += r.scoped
				require.Zero(b, r.broad)
			}
			b.ReportMetric(float64(decoded)/float64(b.N), "decoded_core_rows/op")
			b.ReportMetric(float64(visits)/float64(b.N), "scoped_core_visits/op")
			b.ReportMetric(256, "imported_rows/op")
		})
	}
}
