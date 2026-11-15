package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// Two real tool receipts, not an OS echo. The first callback has passed the
// generation gate and owns the repository lane before the second admission.
// Only the actual read and the existing post-relations/pre-derived hook pause;
// parsing, SQLite writes, version validation, resolution and derived work are real.
func TestPointRelationsCoverOnlyToolsAdmittedBeforeValidatedRead(t *testing.T) {
	for _, beforeRead := range []bool{true, false} {
		name := "second_after_read_stays_pending"
		if beforeRead {
			name = "second_before_read_is_already_covered"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			base := "package main\nfunc Target() {}\n"
			first := base + "func Earlier() { Target() }\n"
			latest := first + "func Later() { Target() }\n"
			require.NoError(t, os.WriteFile(path, []byte(base), 0600))
			db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "covered-tool.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			registry := parser.NewRegistry()
			registry.Register(languages.NewGoExtractor())
			idx := New(db, registry, config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			_, err = idx.Index(root)
			require.NoError(t, err)
			require.NoError(t, idx.IndexFile(path))
			watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
			require.NoError(t, err)
			// Isolate the proven two-tool branch; echoes have their own controls.
			require.NoError(t, watcher.Start([]string{t.TempDir()}))
			enteredRead, releaseRead := make(chan struct{}), make(chan struct{})
			enteredDerived, releaseDerived := make(chan struct{}), make(chan struct{})
			unblockRead := sync.OnceFunc(func() { close(releaseRead) })
			unblockDerived := sync.OnceFunc(func() { close(releaseDerived) })
			t.Cleanup(func() {
				unblockRead()
				unblockDerived()
				cancel()
				require.NoError(t, watcher.Stop())
			})
			firstTicket := make(chan *MutationTicket, 1)
			var secondGeneration atomic.Uint64
			var calls atomic.Int32
			beforeReadFrontier := make(chan uint64, 1)
			watcher.pointReindexRaw = func(file string, stamps *PublicationStamps) (*IndexResult, error) {
				generation := secondGeneration.Load()
				if calls.Add(1) == 1 {
					var one *MutationTicket
					select {
					case one = <-firstTicket:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					generation = one.Generation
					close(enteredRead)
					select {
					case <-releaseRead:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					beforeReadFrontier <- watcher.mutationTierFrontier([]string{file})[file]
				}
				// This is the exact standalone production mode from
				// reindexPointPathRaw, including its read-bound relations coverage.
				return idx.incrementalWatcherPaths(root, []string{file}, watcher.pointRelationsMode(file, generation, stamps))
			}
			var derivedCalls atomic.Int32
			idx.incrementalCatchupHook = func(kind string, _ []string) {
				if kind == "derived" && derivedCalls.Add(1) == 1 {
					close(enteredDerived)
					select {
					case <-releaseDerived:
					case <-ctx.Done():
					}
				}
			}
			require.NoError(t, os.WriteFile(path, []byte(first), 0600))
			one, err := watcher.EnqueueToolMutation(ctx, path)
			require.NoError(t, err)
			require.NotNil(t, one)
			firstTicket <- one
			select {
			case <-enteredRead:
			case <-ctx.Done():
				t.Fatal("first callback did not reach its real read boundary")
			}
			var two *MutationTicket
			admitSecond := func() {
				require.NoError(t, os.WriteFile(path, []byte(latest), 0600))
				two, err = watcher.EnqueueToolMutation(ctx, path)
				require.NoError(t, err)
				require.NotNil(t, two)
				require.Equal(t, one.Generation+1, two.Generation, "only the second real tool admission creates a successor")
				secondGeneration.Store(two.Generation)
			}
			if beforeRead {
				admitSecond()
			}
			unblockRead()
			select {
			case <-enteredDerived:
			case <-ctx.Done():
				t.Fatal("real resolution did not reach the pre-derived boundary")
			}
			require.False(t, one.TierState().RelationsAt.IsZero())
			earlier := db.GetOutEdges("main.go::Earlier")
			require.Len(t, earlier, 1)
			require.Equal(t, "main.go::Target", earlier[0].To)
			if beforeRead {
				require.Equal(t, two.Generation, <-beforeReadFrontier)
				later := db.GetOutEdges("main.go::Later")
				require.Len(t, later, 1)
				require.Equal(t, "main.go::Target", later[0].To, "the first actual executor resolved the second tool's bytes")
				require.True(t, idx.indexedFileReceiptMatches(path), "real stable content receipt must match the second physical write")
				if two.TierState().RelationsAt.IsZero() {
					t.Error("validated read covered second admitted tool bytes, but its own relations tier remains unobserved")
				}
			} else {
				require.Equal(t, one.Generation, <-beforeReadFrontier)
				require.Nil(t, db.GetNode("main.go::Later"))
				admitSecond()
				require.False(t, idx.indexedFileReceiptMatches(path), "a post-read tool write is not covered by the old receipt")
				require.True(t, two.TierState().RelationsAt.IsZero(), "never borrow a prior read for newly admitted bytes")
			}
			for _, ticket := range []*MutationTicket{one, two} {
				select {
				case result := <-ticket.Done:
					t.Fatalf("derived work is held: no complete receipt may be invented: %+v", result)
				default:
				}
			}
			unblockDerived()
			for _, ticket := range []*MutationTicket{one, two} {
				select {
				case result := <-ticket.Done:
					require.NoError(t, result.Err)
					require.True(t, result.Reindexed)
					require.GreaterOrEqual(t, result.AppliedGeneration, ticket.Generation)
					require.False(t, ticket.TierState().RelationsAt.IsZero())
				case <-ctx.Done():
					t.Fatal("owned receipt did not settle after real derived work")
				}
			}
			for _, symbol := range []string{"Earlier", "Later"} {
				edges := db.GetOutEdges("main.go::" + symbol)
				require.Len(t, edges, 1)
				require.Equal(t, "main.go::Target", edges[0].To)
			}
		})
	}
}
