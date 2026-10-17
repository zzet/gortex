package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func TestAnalysisProjectionParkedConsumerReleasesSnapshot(t *testing.T) {
	for _, kind := range []string{"nodes", "edges"} {
		t.Run(kind, func(t *testing.T) {
			s, path := openTempStore(t)
			s.stopCheckpointLoop()
			s = s.BindAnalysisPages().(*Store)
			s.stopMaintenanceLane()
			writeDeadCodeFixture(t, s, []int64{0}, 300)
			ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
			require.NoError(t, err)
			defer ckpt.Close()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unpark := func() { once.Do(func() { close(release) }) }
			defer func() {
				unpark()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("parked consumer did not exit")
				}
			}()
			go func() {
				defer close(done)
				park := func() { close(entered); <-release }
				if kind == "nodes" {
					for range s.NodesLightSeq() {
						park()
						break
					}
				} else {
					for range s.EdgesLightSeq(graph.EdgeCalls) {
						park()
						break
					}
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("consumer never received its first row")
			}
			// The callback remains parked while a writer extends the WAL.
			// A legacy cursor still owns the older read snapshot here.
			growWALForReadTest(t, s)
			result, err := checkpointWALOnceOn(t.Context(), ckpt, "TRUNCATE")
			t.Logf("parked %s: pooled readers=%d checkpoint=%+v err=%v", kind, s.db.Stats().InUse, result, err)
			require.NoError(t, err, "the consumer's pause must not retain a SQLite snapshot")
			require.Zero(t, s.db.Stats().InUse, "no reader connection may survive yield")
			require.Zero(t, walFileSize(path+"-wal"))
			select {
			case <-done:
				t.Fatal("consumer must still be parked at checkpoint completion")
			default:
			}
		})
	}
}

// Exercise the actual Leiden Tick/Pace park, rather than only a stand-in
// blocking callback. The second predicate call witnesses entry into park().
func TestPacedLeidenReleasesSQLiteSnapshotWhileParked(t *testing.T) {
	s, path := openTempStore(t)
	s.stopCheckpointLoop()
	s = s.BindAnalysisPages().(*Store)
	s.stopMaintenanceLane()
	writeDeadCodeFixture(t, s, []int64{0}, 300)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	var editing atomic.Bool
	editing.Store(true)
	var checks atomic.Int32
	parked, done := make(chan struct{}), make(chan struct{})
	pace := analysis.NewPace(func() bool {
		if checks.Add(1) == 2 {
			close(parked)
		}
		return editing.Load()
	})
	defer func() {
		editing.Store(false)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("paced Leiden did not stop after unpark")
		}
	}()
	go func() {
		defer close(done)
		analysis.DetectCommunitiesLeidenIncrementalPaced(s, nil, pace)
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("Leiden never entered its actual Pace park")
	}
	growWALForReadTest(t, s)
	_, err = checkpointWALOnceOn(t.Context(), ckpt, "TRUNCATE")
	require.NoError(t, err, "a paced Leiden scan must not pin the WAL while parked")
	require.Zero(t, s.db.Stats().InUse)
	require.Zero(t, walFileSize(path+"-wal"))
}

func TestAnalysisProjectionPagesMatchRowsAndBoundChurn(t *testing.T) {
	previous := analysisProjectionPageSize
	analysisProjectionPageSize = 5
	t.Cleanup(func() { analysisProjectionPageSize = previous })
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0, 4}, 90)
	s = s.BindAnalysisPages().(*Store)
	for _, generation := range []int64{0, 4} {
		h := s.AtGeneration(generation).BindAnalysisPages().(*Store)
		t.Run(string(rune('0'+generation)), func(t *testing.T) {
			var wantNodes []*graph.Node
			// Compare the exact summary projection through the same scanner.
			rows, err := h.db.Query(`SELECT `+lookupNodeSummaryCols+` FROM nodes WHERE view_gen = ? ORDER BY id`, generation)
			require.NoError(t, err)
			for rows.Next() {
				node, err := scanNodeSummary(rows)
				require.NoError(t, err)
				wantNodes = append(wantNodes, node)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Equal(t, wantNodes, collectAnalysisSeq(h.NodesLightSeq()))
			kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeImports, graph.EdgeCalls, ""}
			wantEdges := h.queryEdgesLightSQL(`SELECT `+edgeColsLight+` FROM edges WHERE view_gen = ? AND kind IN (?, ?) ORDER BY id`, generation, "calls", "imports")
			require.Equal(t, wantEdges, collectAnalysisSeq(h.EdgesLightSeq(kinds...)))
			require.Empty(t, collectAnalysisSeq(h.EdgesLightSeq()))
			require.Empty(t, collectAnalysisSeq(h.EdgesLightSeq("")))
			ctx, cancel := context.WithCancel(t.Context())
			bound := h.WithReadContext(ctx)
			count := 0
			for range bound.NodesLightSeq() {
				count++
				cancel()
			}
			require.Equal(t, 1, count, "cancel must stop within the materialized page")
			ctx, cancel = context.WithCancel(t.Context())
			bound = h.WithReadContext(ctx)
			count = 0
			for range bound.EdgesLightSeq(graph.EdgeCalls) {
				count++
				cancel()
			}
			require.Equal(t, 1, count)
			cancel()
		})
	}
	// Insert beyond each captured high-water while its first page is yielded.
	count := 0
	for range s.NodesLightSeq() {
		if count == 0 {
			require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "zzz-new", Kind: graph.KindFunction}}, nil))
		}
		count++
	}
	require.Equal(t, 90, count)
	before := len(collectAnalysisSeq(s.EdgesLightSeq(graph.EdgeCalls)))
	count = 0
	for range s.EdgesLightSeq(graph.EdgeCalls) {
		if count == 0 {
			require.NoError(t, s.AddBatchChecked(nil, []*graph.Edge{{From: "zzz-new", To: "another", Kind: graph.EdgeCalls}}))
		}
		count++
	}
	require.Equal(t, before, count)
	// Generation indexes are optional during bulk loading and on old stores.
	_, err := s.writerDB.Exec(`DROP INDEX nodes_by_generation; DROP INDEX edges_by_generation`)
	require.NoError(t, err)
	require.Len(t, collectAnalysisSeq(s.NodesLightSeq()), 91)
	require.Len(t, collectAnalysisSeq(s.EdgesLightSeq(graph.EdgeCalls)), before+1)
}

func collectAnalysisSeq[T any](seq iter.Seq[T]) []T {
	var rows []T
	for row := range seq {
		rows = append(rows, row)
	}
	return rows
}

func TestAnalysisProjectionPagePlansLockedAcrossStatisticsRegimes(t *testing.T) {
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0, 4}, 90)
	assertPlans := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		for _, generation := range []int64{0, 4} {
			for name, planRows := range map[string][]string{
				"nodes": explainOnConn(t, ctx, conn, nodesLightPageSQL(true, false), generation, "after", "zzz", 5),
				"edges": explainOnConn(t, ctx, conn, edgesLightPageSQL(true, 2), generation, int64(1), int64(999999), "calls", "imports", 5),
			} {
				plan := strings.Join(planRows, "\n")
				require.Contains(t, plan, name+"_by_generation (view_gen=? AND id>? AND id<?)")
				require.NotContains(t, plan, "SCAN "+name)
				require.NotContains(t, plan, "USE TEMP B-TREE")
			}
		}
	}
	for _, regime := range []string{"no_stats", "live_store_rows", "refreshed"} {
		t.Run(regime, func(t *testing.T) {
			withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
				if regime != "no_stats" {
					_, err := conn.ExecContext(ctx, `ANALYZE`)
					require.NoError(t, err)
				}
				if regime == "live_store_rows" {
					_, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1; DELETE FROM sqlite_stat4`)
					require.NoError(t, err)
					for _, row := range receiverMutationCallStatRows {
						_, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl,idx,stat) VALUES (?,?,?)`, row[0], row[1], row[2])
						require.NoError(t, err)
					}
					_, err = conn.ExecContext(ctx, `ANALYZE sqlite_schema`)
					require.NoError(t, err)
				}
				assertPlans(t, ctx, conn)
			})
		})
	}
}

func TestAnalysisProjectionPageHandlePreservesGenerationAndContext(t *testing.T) {
	s, _ := openTempStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := s.AtGeneration(4).WithReadContext(ctx)
	paged := original.BindAnalysisPages().(*Store)
	require.Same(t, original.storeCore, paged.storeCore)
	require.Equal(t, original.viewGen, paged.viewGen)
	require.Equal(t, original.analysisViewScoped, paged.analysisViewScoped)
	require.Equal(t, original.managedPayloadGeneration, paged.managedPayloadGeneration)
	require.Same(t, original.seal, paged.seal)
	require.Same(t, original.resolveLane, paged.resolveLane)
	require.Equal(t, original.readContext(), paged.readContext())
	require.False(t, paged.ownsCore)
	require.False(t, original.analysisPaged)
	require.True(t, paged.analysisPaged)
	require.Empty(t, collectAnalysisSeq(paged.NodesLightSeq()))
	require.Empty(t, collectAnalysisSeq(paged.EdgesLightSeq(graph.EdgeCalls)))
	require.Empty(t, collectAnalysisSeq(graph.NodesLightSeq(s.BindAnalysisPages())))
}

func TestDefaultAnalysisProjectionKeepsSingleStatementSnapshot(t *testing.T) {
	s, _ := openTempStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	writeDeadCodeFixture(t, s, []int64{0}, 90)
	for range s.NodesLightSeq() {
		require.Equal(t, 1, s.db.Stats().InUse)
		break
	}
	for range s.EdgesLightSeq(graph.EdgeImports) {
		require.Equal(t, 1, s.db.Stats().InUse)
		break
	}
}

func TestAnalysisProjectionPagesRefuseSupersededPublication(t *testing.T) {
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0}, 90)
	paged := s.BindAnalysisPages().(*Store)
	revision := s.AnalysisMutationRevision()
	seen := 0
	for range paged.NodesLightSeq() {
		if seen == 0 {
			s.AddNode(&graph.Node{ID: "zzz-during-analysis", Kind: graph.KindFunction})
		}
		seen++
	}
	installed := false
	require.False(t, s.CommitAnalysisSnapshot(revision, func() { installed = true }))
	require.False(t, installed, "a result spanning changed revisions must not publish")
	require.True(t, s.CommitAnalysisSnapshot(s.AnalysisMutationRevision(), func() { installed = true }))
	require.True(t, installed)
}

// This is a bounded scan-cost control, not a daemon latency measurement.
// The default path is also the unchanged resolver one-statement control.
func BenchmarkAnalysisLightSequencePages(b *testing.B) {
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeSpawns, graph.EdgeMemberOf, graph.EdgeParamOf,
		graph.EdgeReferences, graph.EdgeReturns, graph.EdgeTypedAs, graph.EdgeImplements,
		graph.EdgeExtends, graph.EdgeAliases, graph.EdgeComposes, graph.EdgeImports,
		graph.EdgeDependsOnModule, graph.EdgeInstantiates}
	for _, projection := range []string{"nodes", "sparse_kind", "14_kinds"} {
		for _, paged := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/paged=%t", projection, paged), func(b *testing.B) {
				s, err := Open(b.TempDir() + "/analysis.sqlite")
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				s.stopCheckpointLoop()
				s.stopMaintenanceLane()
				const count = 10000
				for _, generation := range []int64{0, 4} {
					nodes, edges := make([]*graph.Node, count), make([]*graph.Edge, count)
					for i := range count {
						id := fmt.Sprintf("node-%05d", i)
						nodes[i] = &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "repo/main.go", RepoPrefix: "repo"}
						kind := graph.EdgeReferences
						if i%100 == 0 {
							kind = graph.EdgeImports
						}
						edges[i] = &graph.Edge{From: id, To: "target", Kind: kind, FilePath: "repo/main.go", Line: i + 1}
					}
					require.NoError(b, s.AtGeneration(generation).AddBatchChecked(nodes, edges))
				}
				read := s
				if paged {
					read = s.BindAnalysisPages().(*Store)
				}
				want := count
				selected := kinds
				if projection == "sparse_kind" {
					selected = []graph.EdgeKind{graph.EdgeImports}
					want = count / 100
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					seen := 0
					if projection == "nodes" {
						for range read.NodesLightSeq() {
							seen++
						}
					} else {
						for range read.EdgesLightSeq(selected...) {
							seen++
						}
					}
					if seen != want {
						b.Fatalf("rows=%d want=%d", seen, want)
					}
				}
				b.StopTimer()
			})
		}
	}
}
