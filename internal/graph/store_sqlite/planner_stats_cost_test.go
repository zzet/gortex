package store_sqlite

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// What an owed refresh costs, and each index's ANALYZE (one hold of the
// writer each), against the graph's size: nodes with five out-edges each, at
// the sizes in GORTEX_STORE_STATS_COST_FILES (files of 30 nodes, comma list;
// default 1000,4000). Gated (GORTEX_STORE_STATS_COST=1): it writes up to
// 120,000 nodes and 600,000 edges.
func TestPlannerStatsRefreshCostBySize(t *testing.T) {
	if os.Getenv("GORTEX_STORE_STATS_COST") != "1" {
		t.Skip("set GORTEX_STORE_STATS_COST=1")
	}
	sizes := []int{1000, 4000}
	if raw := os.Getenv("GORTEX_STORE_STATS_COST_FILES"); raw != "" {
		sizes = nil
		for _, f := range splitComma(raw) {
			n, err := strconv.Atoi(f)
			require.NoError(t, err)
			sizes = append(sizes, n)
		}
	}
	for _, files := range sizes {
		s := openPayloadStore(t)
		for f := 0; f < files; f += 100 {
			var nodes []*graph.Node
			var edges []*graph.Edge
			for g := f; g < min(f+100, files); g++ {
				repo := []string{"repo", "other"}[g%2]
				file := fmt.Sprintf("%s/pkg%d/f%d.go", repo, g%40, g)
				for i := 0; i < 30; i++ {
					id := fmt.Sprintf("%s::S%d_%d", file, g, i)
					nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("N%d", (g*7+i)%5000),
						FilePath: file, RepoPrefix: repo, Language: []string{"go", "python", "typescript"}[g%3]})
					for e := 0; e < 5; e++ {
						to := fmt.Sprintf("%s::S%d_%d", file, (g+e*13)%files, (i+e)%30)
						edges = append(edges, &graph.Edge{From: id, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: i*10 + e})
					}
				}
			}
			s.AddBatch(nodes, edges)
		}
		_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
		_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
		require.NoError(t, err)
		_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
		started := time.Now()
		h, err := s.EnsurePlannerStatsFresh(context.Background())
		require.NoError(t, err)
		whole := time.Since(started)
		require.True(t, h.Refreshed, "reason=%q", h.Reason)
		present, err := s.plannerStatsPresentIndexList(context.Background())
		require.NoError(t, err)
		var longest time.Duration
		var longestName string
		for _, name := range present {
			s.writeMu.Lock()
			at := time.Now()
			_, err := s.analyzePlannerStatsIndexLocked(context.Background(), name)
			took := time.Since(at)
			s.writeMu.Unlock()
			require.NoError(t, err)
			if took > longest {
				longest, longestName = took, name
			}
		}
		nodes, edges := files*30, files*150
		t.Logf("files=%d nodes=%d edges=%d owed refresh=%s (%.2f µs/node, %.2f µs/edge) longest one-index hold=%s (%s) indexes=%d",
			files, nodes, edges, whole.Round(time.Millisecond), float64(whole.Microseconds())/float64(nodes),
			float64(whole.Microseconds())/float64(edges), longest.Round(time.Millisecond), longestName, len(present))
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
