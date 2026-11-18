package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// The primary watcher writes generation zero without a checkout refresh ticket.
// Its read/resolve gaps must remain visible to the background retirement/reclaim
// machinery after an individual SQLite transaction releases the writer.
func TestPrimaryWatcherKeepsWriteDemandBetweenGraphWrites(t *testing.T) {
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("primary")
	idx.rootPath = t.TempDir()
	lifecycle := &CheckoutLifecycle{store: store}
	failure := errors.New("stop after the primary mutation")
	for _, bodyError := range []error{nil, failure} {
		var writerWanted, retirementSeesMutation bool
		err := idx.coordinateRepositoryMutation(context.Background(), OutputEntryWatcherPatchGraph, func() error {
			store.AddNode(&graph.Node{ID: "primary/a.go::Before", Name: "Before", Kind: graph.KindFunction, FilePath: "primary/a.go"})
			writerWanted = store.WriteWanted()
			retirementSeesMutation = lifecycle.interactiveWriteWanted(nil)
			store.AddNode(&graph.Node{ID: "primary/a.go::After", Name: "After", Kind: graph.KindFunction, FilePath: "primary/a.go"})
			return bodyError
		})
		if !errors.Is(err, bodyError) {
			t.Fatalf("mutation error = %v, want %v", err, bodyError)
		}
		if store.WriteWanted() {
			t.Fatal("completed or failed watcher mutation leaked write demand")
		}
		if !writerWanted || !retirementSeesMutation {
			t.Fatalf("primary watcher disappeared between actual SQLite writes: writer wanted=%t, retirement sees mutation=%t", writerWanted, retirementSeesMutation)
		}
	}
}

func TestPrimaryWatcherAnnouncesBeforeRepositoryLaneAndReleasesOnCancel(t *testing.T) {
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("primary")
	idx.rootPath = t.TempDir()
	coordinator := idx.repositoryMutations()
	<-coordinator.lane // An unrelated owner has the stable lane before admission.
	// A cold index may hold this lane while graph points at a temporary shadow.
	// Queued watcher admission must announce the original durable store safely.
	idx.graph = graph.New()
	admitted := make(chan bool, 1)
	coordinator.batchAdmissionHook = func() { admitted <- store.WriteWanted() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	bodyRan := false
	go func() {
		done <- idx.coordinateRepositoryMutation(ctx, OutputEntryWatcherPatchGraph, func() error {
			bodyRan = true
			return nil
		})
	}()
	wantedBeforeLane := <-admitted
	cancel()
	err := <-done
	idx.graph = store
	coordinator.lane <- struct{}{}
	coordinator.batchAdmissionHook = nil
	if !errors.Is(err, context.Canceled) || bodyRan {
		t.Fatalf("cancelled queued primary watcher: error=%v, body ran=%t", err, bodyRan)
	}
	if store.WriteWanted() {
		t.Fatal("cancelled admission leaked write demand")
	}
	if !wantedBeforeLane {
		t.Fatal("primary watcher waited for its repository lane without announcing writer demand")
	}
}

func TestPrimaryWatcherWriteIntentDoesNotCoverColdOrOtherEntries(t *testing.T) {
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("primary")
	idx.rootPath = t.TempDir()
	for _, entry := range []OutputMutationEntry{OutputEntryIndexCtx, OutputEntryIndexFile, OutputEntryDeferredPasses, OutputEntryRepositoryTopology, OutputEntryWatcherDirScan} {
		err := idx.coordinateRepositoryMutation(context.Background(), entry, func() error {
			if store.WriteWanted() {
				return errors.New("unrelated mutation entry announced primary watcher intent")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
	}
	panicValue := errors.New("programmer panic")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = idx.coordinateRepositoryMutation(context.Background(), OutputEntryWatcherPatchGraph, func() error { panic(panicValue) })
	}()
	if recovered != panicValue || store.WriteWanted() {
		t.Fatalf("panic cleanup: recovered=%v, leaked demand=%t", recovered, store.WriteWanted())
	}
}
