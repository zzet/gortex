package store_sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	sqlite "modernc.org/sqlite"
)

type shadowDeleteProbe struct{ fire func(string) }

var shadowDeleteObserver atomic.Pointer[shadowDeleteProbe]
var shadowDeleteRegister sync.Once

func installShadowDeleteFunction(t *testing.T) {
	t.Helper()
	shadowDeleteRegister.Do(func() {
		err := sqlite.RegisterScalarFunction("gortex_test_shadow_delete", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if p := shadowDeleteObserver.Load(); p != nil {
				p.fire(args[0].(string))
			}
			return int64(0), nil
		})
		require.NoError(t, err)
	})
}

func shadowEvictionFixture(t *testing.T, n int) (*Store, []*graph.Node) {
	t.Helper()
	installShadowDeleteFunction(t)
	s, err := Open(filepath.Join(t.TempDir(), "shadow.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	nodes := make([]*graph.Node, n)
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("shadow::N%05d", i), Kind: graph.KindFunction, Name: fmt.Sprintf("N%d", i), FilePath: "shadow::source.go", RepoPrefix: "shadow", Language: "go"}
	}
	s.AddBatch(nodes, nil)
	return s, nodes
}

func shadowDeleteTrigger(t *testing.T, s *Store, table, value string) {
	t.Helper()
	_, err := s.writerDB.Exec(`CREATE TRIGGER test_shadow_delete BEFORE DELETE ON ` + table + ` BEGIN SELECT gortex_test_shadow_delete(` + value + `); END`)
	require.NoError(t, err)
	t.Cleanup(func() { shadowDeleteObserver.Store(nil) })
}

func TestShadowReplacementAdmitsForegroundWriter(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		t.Run(fmt.Sprint(bounded), func(t *testing.T) {
			s, nodes := shadowEvictionFixture(t, 1024)
			_, checkout := beginGenerationEvictionHandle(t, s, 1)
			entered := make(chan struct{})
			var once sync.Once
			shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(string) { once.Do(func() { close(entered) }); time.Sleep(time.Millisecond) }})
			shadowDeleteTrigger(t, s, "nodes", "OLD.id")
			type result struct {
				n, e    int
				err     error
				elapsed time.Duration
			}
			done := make(chan result, 1)
			evictCtx, cancelEviction := context.WithTimeout(t.Context(), 5*time.Second)
			go func() {
				start := time.Now()
				var n, e int
				var err error
				if bounded {
					n, e, err = s.EvictRepoForShadowReplacement(evictCtx, "shadow")
				} else {
					n, e, err = s.evictByPredicateResult(evictRepoPredicate, "shadow", evictThisGeneration)
				}
				done <- result{n, e, err, time.Since(start)}
			}()
			t.Cleanup(func() {
				cancelEviction()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("eviction failed to join")
				}
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("eviction never entered real SQL delete")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
			start := time.Now()
			err := s.writeMu.LockContext(ctx)
			if err == nil {
				tx, txErr := checkout.beginWriteContext(ctx)
				if txErr == nil {
					_, txErr = tx.ExecContext(ctx, `INSERT INTO nodes(id,view_gen,kind,name,file_path,repo_prefix) VALUES(?,?,?,?,?,?)`, "checkout::write", checkout.viewGen, "function", "Write", "checkout::write.go", "checkout")
					if txErr == nil {
						txErr = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
				}
				if txErr == nil {
					checkout.finishAnalysisMutationLocked(true)
				}
				s.writeMu.Unlock()
				err = txErr
			}
			foreground := time.Since(start)
			cancel()
			if bounded {
				require.NoError(t, err)
				require.NotNil(t, checkout.GetNode("checkout::write"))
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			r := <-done
			done <- r
			require.NoError(t, r.err)
			require.Equal(t, len(nodes), r.n)
			require.Zero(t, r.e)
			require.Zero(t, s.NodeCount())
			t.Logf("bounded=%v nodes=%d total=%s foregroundSQL=%s", bounded, r.n, r.elapsed, foreground)
		})
	}
}

func TestShadowReplacementRetiresLateIncomingEdges(t *testing.T) {
	s, nodes := shadowEvictionFixture(t, 600)
	survivor := &graph.Node{ID: "other::survivor", Kind: graph.KindFunction, Name: "Survivor", FilePath: "other::file.go", RepoPrefix: "other"}
	s.AddNode(survivor)
	edges := make([]*graph.Edge, 600)
	for i := range edges {
		edges[i] = &graph.Edge{From: survivor.ID, To: nodes[i].ID, Kind: graph.EdgeCalls, Line: i + 1}
	}
	s.AddBatch(nil, edges)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(string) { once.Do(func() { close(entered); <-release }) }})
	shadowDeleteTrigger(t, s, "edges", "CAST(OLD.id AS TEXT)")
	done := make(chan error, 1)
	wrote := make(chan struct{})
	var writerStarted bool
	t.Cleanup(func() {
		cancel()
		unpark()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("retirement did not join")
		}
		if writerStarted {
			select {
			case <-wrote:
			case <-time.After(5 * time.Second):
				t.Error("late writer did not join")
			}
		}
	})
	go func() {
		n, e, err := s.EvictRepoForShadowReplacement(ctx, "shadow")
		if err == nil && (n != 600 || e != 601) {
			err = fmt.Errorf("counts %d/%d, want600/601", n, e)
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no edge deletion")
	}
	writerStarted = true
	go func() {
		s.AddEdge(&graph.Edge{From: survivor.ID, To: nodes[599].ID, Kind: graph.EdgeCalls, Line: 9999})
		close(wrote)
	}()
	// Positively witness an actual sibling drain writer queued before releasing
	// the first delete batch. It must commit before the next retirement step.
	deadline := time.Now().Add(5 * time.Second)
	for s.writeMu.waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Positive(t, s.writeMu.waiting())
	unpark()
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("late edge writer did not complete")
	}
	select {
	case err := <-done:
		done <- err
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not finish")
	}
	require.NotNil(t, s.GetNode(survivor.ID))
	require.Equal(t, 1, s.NodeCount())
	require.Zero(t, s.EdgeCount())
}

func TestShadowReplacementCancelInvalidatesPriorProvenanceAndRetries(t *testing.T) {
	s, nodes := shadowEvictionFixture(t, 600)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "shadow", IndexedSHA: "unchanged-clean-head", NodeCount: 600}))
	_, checkout := beginGenerationEvictionHandle(t, s, 1)
	checkout.AddBatch(nodes, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var count atomic.Int32
	shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(string) {
		if count.Add(1) == 257 {
			cancel()
		}
	}})
	shadowDeleteTrigger(t, s, "nodes", "OLD.id")
	n, e, err := s.EvictRepoForShadowReplacement(ctx, "shadow")
	require.True(t, errors.Is(err, context.Canceled), "%v", err)
	require.Equal(t, 256, n)
	require.Zero(t, e)
	var states int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM repo_index_state WHERE view_gen=0 AND repo_prefix='shadow'`).Scan(&states))
	require.Zero(t, states)
	require.Equal(t, 600, checkout.NodeCount())
	shadowDeleteObserver.Store(nil)
	n, e, err = s.EvictRepoForShadowReplacement(t.Context(), "shadow")
	require.NoError(t, err)
	require.Equal(t, 344, n)
	require.Zero(t, e)
	require.Zero(t, s.NodeCount())
	require.Equal(t, 600, checkout.NodeCount())
	_, _, err = s.AtGeneration(checkout.viewGen).EvictRepoForShadowReplacement(t.Context(), "shadow")
	require.Error(t, err)
}

func TestShadowReplacementCompletesUnderUnrelatedWriters(t *testing.T) {
	s, nodes := shadowEvictionFixture(t, 600)
	_, checkout := beginGenerationEvictionHandle(t, s, 1)
	anchors := []*graph.Node{{ID: "other::A", Kind: graph.KindFunction, Name: "A", FilePath: "other::a.go", RepoPrefix: "other"}, {ID: "other::B", Kind: graph.KindFunction, Name: "B", FilePath: "other::b.go", RepoPrefix: "other"}}
	s.AddBatch(anchors, nil)
	checkout.AddBatch(anchors, nil)
	edges := make([]*graph.Edge, 8000)
	for i := range edges {
		edges[i] = &graph.Edge{From: anchors[0].ID, To: anchors[1].ID, Kind: graph.EdgeCalls, Line: i + 1}
	}
	s.AddBatch(nil, edges)
	checkout.AddBatch(nil, edges)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	var baseWrites, checkoutWrites atomic.Int32
	for _, producer := range []struct {
		store *Store
		count *atomic.Int32
	}{{s, &baseWrites}, {checkout, &checkoutWrites}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ctx.Err() == nil; i++ {
				node := *anchors[0]
				node.Name = fmt.Sprint(i)
				producer.store.AddBatch([]*graph.Node{&node}, []*graph.Edge{{From: anchors[0].ID, To: anchors[1].ID, Kind: graph.EdgeCalls, Line: 10000 + i}})
				producer.count.Add(1)
			}
		}()
	}
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("producer did not join")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for (baseWrites.Load() < 2 || checkoutWrites.Load() < 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.GreaterOrEqual(t, baseWrites.Load(), int32(2))
	require.GreaterOrEqual(t, checkoutWrites.Load(), int32(2))
	beforeBase, beforeCheckout := baseWrites.Load(), checkoutWrites.Load()
	attempt, cancelAttempt := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelAttempt()
	start := time.Now()
	n, e, err := s.EvictRepoForShadowReplacement(attempt, "shadow")
	require.NoError(t, err)
	require.Equal(t, len(nodes), n)
	require.Zero(t, e)
	require.NoError(t, ctx.Err(), "producers were stopped before retirement completed")
	require.Greater(t, baseWrites.Load(), beforeBase)
	require.Greater(t, checkoutWrites.Load(), beforeCheckout)
	t.Logf("total=%s removed=%d active base commits=%d checkout commits=%d", time.Since(start), n, baseWrites.Load()-beforeBase, checkoutWrites.Load()-beforeCheckout)
	cancel()
	<-joined
	require.Equal(t, 2, s.NodeCount())
	require.Equal(t, 2, checkout.NodeCount())
	require.GreaterOrEqual(t, s.EdgeCount(), 8000)
	require.GreaterOrEqual(t, checkout.EdgeCount(), 8000)
}

func TestShadowReplacementTracksRetargetBehindPhysicalCursor(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprint(managed), func(t *testing.T) {
			base, nodes := shadowEvictionFixture(t, 600)
			s := base
			if managed {
				_, s = beginGenerationEvictionHandle(t, base, 1)
				require.NoError(t, s.AddBatchChecked(nodes, nil))
			}
			survivor := &graph.Node{ID: "other::survivor", Kind: graph.KindFunction, Name: "Survivor", FilePath: "other::file.go", RepoPrefix: "other"}
			s.AddNode(survivor)
			oldTo := graph.UnresolvedMarker + "Missing"
			oldEdge := &graph.Edge{From: survivor.ID, To: oldTo, Kind: graph.EdgeCalls, Line: 9999}
			s.AddEdge(oldEdge)
			edges := make([]*graph.Edge, 700)
			for i := range edges {
				edges[i] = &graph.Edge{From: survivor.ID, To: nodes[i%600].ID, Kind: graph.EdgeCalls, Line: i + 1}
			}
			s.AddBatch(nil, edges)
			var originalID int64
			require.NoError(t, s.db.QueryRow(`SELECT id FROM edges WHERE view_gen=? AND line=9999`, s.viewGen).Scan(&originalID))
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unpark := func() { releaseOnce.Do(func() { close(release) }) }
			shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(string) { once.Do(func() { close(entered); <-release }) }})
			shadowDeleteTrigger(t, s, "edges", "CAST(OLD.id AS TEXT)")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			retargeted := make(chan error, 1)
			var writerStarted bool
			t.Cleanup(func() {
				cancel()
				unpark()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("evict join")
				}
				if writerStarted {
					select {
					case <-retargeted:
					case <-time.After(5 * time.Second):
						t.Error("retarget join")
					}
				}
			})
			go func() {
				n, e, err := s.EvictRepoForShadowReplacement(ctx, "shadow")
				if err == nil && (n != 600 || e != 701) {
					err = fmt.Errorf("removed %d/%d want600/701", n, e)
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no deletion witness")
			}
			writerStarted = true
			go func() {
				stats, err := s.reindexEdgesSetOriented([]graph.EdgeReindex{{OldTo: oldTo, Edge: &graph.Edge{From: survivor.ID, To: nodes[599].ID, Kind: graph.EdgeCalls, Line: 9999}}})
				if err == nil && stats.updatedRows != 1 {
					err = fmt.Errorf("not an in-place UPDATE: %+v", stats)
				}
				retargeted <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for s.writeMu.waiting() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			require.Positive(t, s.writeMu.waiting())
			unpark()
			select {
			case err := <-retargeted:
				retargeted <- err
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("retarget stalled")
			}
			select {
			case err := <-done:
				done <- err
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("eviction stalled")
			}
			require.Positive(t, s.shadowEndpointRevision().Load())
			require.Zero(t, s.EdgeCount())
			require.Equal(t, 1, s.NodeCount())
			t.Logf("retargeted actual physical edge %d behind first scan page; surviving edges=0", originalID)
		})
	}
}

func TestShadowReplacementHighFanoutWithoutEndpointIndexes(t *testing.T) {
	s, nodes := shadowEvictionFixture(t, 3)
	_, checkout := beginGenerationEvictionHandle(t, s, 1)
	checkout.AddBatch(nodes, nil)
	survivor := &graph.Node{ID: "other::survivor", Kind: graph.KindFunction, Name: "Survivor", FilePath: "other::file.go", RepoPrefix: "other"}
	s.AddNode(survivor)
	edges := make([]*graph.Edge, 4000)
	for i := range edges {
		from, to := survivor.ID, nodes[0].ID
		if i%2 == 0 {
			from, to = nodes[1].ID, survivor.ID
		}
		edges[i] = &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, Line: i + 1}
	}
	s.AddBatch(nil, edges)
	checkout.AddBatch(nil, edges)
	require.NoError(t, s.UpsertSymbolFTS(nodes[0].ID, "Witness"))
	_, err := s.writerDB.Exec(`INSERT INTO semantic_binding_types(view_gen,repo_prefix,file_path,line,name,type_name) VALUES(0,'shadow','shadow::source.go',1,'N0','int'),(0,'other','other::file.go',1,'Survivor','int'); INSERT INTO file_index_failures(view_gen,repo_prefix,file_path,error) VALUES(0,'shadow','shadow::source.go','old failure'),(0,'other','other::file.go','kept')`)
	require.NoError(t, err)
	for _, index := range []string{"edges_by_from", "edges_by_to"} {
		_, err = s.writerDB.Exec(`DROP INDEX ` + index)
		require.NoError(t, err)
	}
	buildMinimalAnalysisGeneration(t, s, "shadow-analysis", 0, true)
	before := s.AnalysisMutationRevision()
	baseToken, checkoutToken := s.BeginMutationReceipt(), checkout.BeginMutationReceipt()
	n, e, err := s.EvictRepoForShadowReplacement(t.Context(), "shadow")
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Equal(t, 4000, e)
	require.Equal(t, 1, s.NodeCount())
	require.Zero(t, s.EdgeCount())
	require.Equal(t, 3, checkout.NodeCount())
	require.Equal(t, 4000, checkout.EdgeCount())
	require.Greater(t, s.AnalysisMutationRevision(), before)
	receipt := s.EndMutationReceipt(baseToken)
	require.True(t, receipt.Complete)
	require.True(t, receipt.ResolutionRelevant)
	require.Contains(t, receipt.TargetIDs, nodes[0].ID)
	untouched := checkout.EndMutationReceipt(checkoutToken)
	require.True(t, untouched.Complete)
	require.False(t, untouched.ResolutionRelevant)
	count, err := s.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, 1, count, "FTS reset remains the caller's separate operation")
	for _, table := range []string{"semantic_binding_types", "file_index_failures"} {
		var count int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE view_gen=0 AND repo_prefix='shadow'`).Scan(&count))
		require.Zero(t, count)
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE view_gen=0 AND repo_prefix='other'`).Scan(&count))
		require.Equal(t, 1, count)
	}
}

func TestShadowEndpointRevisionIgnoresPositiveWritesAndRollback(t *testing.T) {
	s, _ := shadowEvictionFixture(t, 0)
	_, checkout := beginGenerationEvictionHandle(t, s, 1)
	old := graph.UnresolvedMarker + "Missing"
	seed := &graph.Edge{From: "other::Caller", To: old, Kind: graph.EdgeCalls, FilePath: "other::file.go", Line: 1}
	batch := []graph.EdgeReindex{{OldTo: old, Edge: &graph.Edge{From: seed.From, To: "other::Target", Kind: seed.Kind, FilePath: seed.FilePath, Line: seed.Line}}}
	checkout.AddEdge(seed)
	stats, err := checkout.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.updatedRows)
	require.Zero(t, s.baseEdgeEndpointRevision.Load())
	require.Equal(t, uint64(1), checkout.shadowEndpointRevision().Load())
	_, other := beginGenerationEvictionHandle(t, s, 2)
	require.Zero(t, other.shadowEndpointRevision().Load())
	s.AddEdge(seed)
	secondSeed := *seed
	secondSeed.Line = 2
	checkout.AddEdge(&secondSeed)
	secondBatch := []graph.EdgeReindex{{OldTo: old, Edge: &graph.Edge{From: seed.From, To: "other::SecondTarget", Kind: seed.Kind, FilePath: seed.FilePath, Line: 2}}}
	_, err = s.writerDB.Exec(`CREATE TRIGGER reject_test_retarget BEFORE UPDATE OF to_id ON edges BEGIN SELECT RAISE(ABORT,'controlled rollback'); END`)
	require.NoError(t, err)
	_, err = s.reindexEdgesSetOriented(batch)
	require.Error(t, err)
	require.Zero(t, s.baseEdgeEndpointRevision.Load())
	_, err = checkout.reindexEdgesSetOriented(secondBatch)
	require.Error(t, err)
	require.Equal(t, uint64(1), checkout.shadowEndpointRevision().Load(), "rolled-back positive endpoint update advanced its clock")
	_, err = s.writerDB.Exec(`DROP TRIGGER reject_test_retarget`)
	require.NoError(t, err)
	stats, err = s.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.updatedRows)
	require.Equal(t, uint64(1), s.baseEdgeEndpointRevision.Load())
}
