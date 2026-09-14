package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// bracketBuilder is the smallest builder withGenerationBulkLoad needs: the
// bracket touches the loader and the logger and nothing else, so a fixture
// store would only hide which of the two the behaviour comes from.
func bracketBuilder() *SparseGenerationBuilder {
	return &SparseGenerationBuilder{Logger: zap.NewNop()}
}

// A window that is opened must be closed on EVERY way out of the bracket.
//
// The store's generation window pins one writer connection and disables that
// connection's automatic checkpoints (store_sqlite/bulk_load.go,
// BeginGenerationBulkLoad), so a leaked window is a pinned writer in front of
// an unbounded WAL that no later caller owns: EndGenerationBulkLoad is inert
// for anybody else, and the next FlushBulk would close it as if it were its
// own and charge that build a TRUNCATE it never asked for.
//
// The four exits are not the same mechanism. A close written after run() is
// reached by the ordinary return and by an error return, but skipped by a
// panic and by a runtime.Goexit; a recover/re-panic pair adds the panic and
// still misses the Goexit. Only a deferred close covers all four, which is why
// this test enumerates them rather than trusting the happy path.
func TestGenerationBulkBracketClosesOnEveryExitPath(t *testing.T) {
	failed := errors.New("the payload write failed")
	for _, tc := range []struct {
		name string
		// drive runs the bracket the way this exit path leaves it.
		drive func(t *testing.T, b *SparseGenerationBuilder, loader *bulkLoadProbe)
	}{
		{
			name: "returns",
			drive: func(t *testing.T, b *SparseGenerationBuilder, loader *bulkLoadProbe) {
				if err := b.withGenerationBulkLoad(loader, 7, func() error { return nil }); err != nil {
					t.Fatalf("a successful write reported %v", err)
				}
			},
		},
		{
			name: "fails",
			drive: func(t *testing.T, b *SparseGenerationBuilder, loader *bulkLoadProbe) {
				err := b.withGenerationBulkLoad(loader, 7, func() error { return failed })
				if !errors.Is(err, failed) {
					t.Fatalf("bracket error = %v, want the write's own failure", err)
				}
			},
		},
		{
			name: "panics",
			drive: func(t *testing.T, b *SparseGenerationBuilder, loader *bulkLoadProbe) {
				recovered := func() (recovered any) {
					defer func() { recovered = recover() }()
					_ = b.withGenerationBulkLoad(loader, 7, func() error { panic(failed) })
					return nil
				}()
				// The panic must still reach the caller: closing the window is
				// not licence to swallow the failure that caused it.
				cause, isError := recovered.(error)
				if !isError || !errors.Is(cause, failed) {
					t.Fatalf("recovered %v, want the panic to propagate", recovered)
				}
			},
		},
		{
			name: "goexits",
			drive: func(t *testing.T, b *SparseGenerationBuilder, loader *bulkLoadProbe) {
				// runtime.Goexit is how a t.Fatal inside a helper goroutine,
				// and any future early exit, leaves a function: its deferred
				// calls run and its return statements do not.
				done := make(chan struct{})
				go func() {
					defer close(done)
					_ = b.withGenerationBulkLoad(loader, 7, func() error {
						runtime.Goexit()
						return nil
					})
				}()
				<-done
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loader := &bulkLoadProbe{}
			tc.drive(t, bracketBuilder(), loader)
			begun, ended := loader.counts()
			if len(begun) != 1 || begun[0] != 7 {
				t.Fatalf("windows begun = %v, want exactly one on generation 7", begun)
			}
			if ended != 1 {
				t.Fatalf("the %s exit closed the window %d time(s), want exactly one close: a leaked "+
					"window pins the store's writer with automatic checkpoints disabled", tc.name, ended)
			}
			if loader.isOpen() {
				t.Fatalf("the %s exit left the generation bulk window open", tc.name)
			}
		})
	}
}

// A begin that reports the destination generation already holds rows is a
// precondition failure, not an optimisation that did not land.
//
// The predicate proved the reservation empty before choosing the copy route,
// but it reads nodes only; the store re-proves it over nodes AND edges
// (generationPayloadEmpty), so an edges-only residue from a writer that
// vanished is refused here and nowhere else. Stepping over that refusal would
// run the whole-generation copy UNBRACKETED on top of the residue and publish
// a base that is neither generation zero's payload nor a re-parse of the tree.
func TestClaimedDedicatedBaseFailsClosedOnAPopulatedGeneration(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	populated := fmt.Errorf("%w: generation %d", store_sqlite.ErrGenerationBulkLoadPopulated, claim.GenerationID)
	loader := &bulkLoadProbe{fail: populated}
	probe := &copyProbe{store: builder.Store, bulk: loader}

	_, _, err := builder.BuildClaimedDedicatedBase(context.Background(), ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, BulkLoad: loader, CopySource: probe,
	})

	if route, reason := claimedRoute(t, logs); route != "copy_generation_zero" {
		t.Fatalf("route = %q (%s), want the copy plan so the refusal is actually exercised", route, reason)
	}
	if !errors.Is(err, store_sqlite.ErrGenerationBulkLoadPopulated) {
		t.Fatalf("build error = %v, want the populated-generation refusal to fail the build", err)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy ran over a generation the store says already carries rows: %+v", probe.calls)
	}
	if begun, ended := loader.counts(); len(begun) != 0 || ended != 0 {
		t.Fatalf("a refused window was driven: begun=%v closed=%d", begun, ended)
	}
}

// Every OTHER begin failure keeps the ordinary write. The window is an
// optimisation over a write that is correct without it, so a store that could
// not take the cheaper shape must cost a slower build and not a lost one.
// This is the discrimination the clause above needs to be a clause and not a
// blanket refusal.
func TestClaimedDedicatedBaseCopiesThroughAnUnavailableBulkWindow(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	loader := &bulkLoadProbe{fail: errors.New("the store could not pin a writer")}
	probe := &copyProbe{store: builder.Store, bulk: loader}

	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, BulkLoad: loader, CopySource: probe,
	})

	if route, reason := claimedRoute(t, logs); route != "copy_generation_zero" {
		t.Fatalf("route = %q (%s), want the copy plan", route, reason)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("an unavailable window changed the write: the copy ran %d time(s)", len(probe.calls))
	}
	if begun, ended := loader.counts(); len(begun) != 0 || ended != 0 {
		t.Fatalf("a window that never opened was driven: begun=%v closed=%d", begun, ended)
	}
	if view := materializeClaimedBase(t, builder, request.Identity.GraphID, id); len(view.Reader.AllNodes()) == 0 {
		t.Fatal("an unavailable window lost the payload")
	}
}

// The bracket closes through the store's DEFERRED drain door, and the
// difference between the two doors is observable on the real store.
//
// EndGenerationBulkLoad measures the residue with one bounded PASSIVE
// checkpoint — which never waits for a reader and never resizes the log — and
// hands the follow-up TRUNCATE to the maintenance lane. FlushBulk, the door an
// index pass uses, runs the TRUNCATE inline: it waits out the live readers of
// the generations underneath and resets the log file to zero bytes on the
// caller's thread. A committed base is published beside exactly those readers,
// so a bracket closed the second way would put that wait inside the publish
// window.
//
// Both arms run against *store_sqlite.Store itself, in the same test, over the
// same shape of payload: arm one is the production bracket, arm two is the
// control that proves the observable can tell the doors apart (a test where
// the log is never truncated by anybody would pass with either door).
func TestGenerationBulkBracketEndsThroughTheDeferredDrainDoor(t *testing.T) {
	// A residue gate that fires would drain the log from the lane, which is
	// the RIGHT behaviour and the wrong thing to race here: this test is about
	// the drain the close runs INLINE. A line far above this payload means
	// nothing is owed, so an empty log after the close can only be an inline
	// TRUNCATE.
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "1000000")
	builder, request, claim := privateClaimedDedicatedFixture(t)
	store := builder.Store
	wal := request.StorePath + "-wal"

	if err := builder.withGenerationBulkLoad(store, claim.GenerationID, func() error {
		return writeBracketProbeRows(store, request.RepoPrefix, "deferred")
	}); err != nil {
		t.Fatalf("the bracket over the real store failed: %v", err)
	}
	deferred := walBytes(t, wal)
	if deferred == 0 {
		t.Fatal("closing the generation window emptied the WAL on the caller's thread: the bracket is " +
			"no longer closing through EndGenerationBulkLoad's deferred drain, and a committed-base " +
			"publish now waits out the readers of the generations underneath it")
	}
	// The window really was released, not merely left unmeasured.
	reopened, err := store.BeginGenerationBulkLoad(claim.GenerationID)
	if err != nil {
		t.Fatalf("reopen the generation window after the bracket: %v", err)
	}
	if !reopened {
		t.Fatal("the bracket returned with the generation window still open")
	}

	// The control, on the same store and the same log: the other door.
	if err := writeBracketProbeRows(store, request.RepoPrefix, "inline"); err != nil {
		t.Fatalf("write the control payload: %v", err)
	}
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush the index-pass bracket: %v", err)
	}
	if inline := walBytes(t, wal); inline != 0 {
		t.Fatalf("the control door left %d WAL bytes, so this test cannot tell the two doors apart; "+
			"the deferred arm's %d bytes prove nothing", inline, deferred)
	}
}

// writeBracketProbeRows writes a payload into the mutable working-copy view.
// The rows go to generation zero on purpose: the window is store-global, so
// the payload it brackets need not be the generation it names, and leaving the
// reserved generation empty is what lets both arms open a window over it.
func writeBracketProbeRows(store *store_sqlite.Store, repoPrefix, tag string) error {
	nodes := make([]*graph.Node, 0, 512)
	for i := range 512 {
		id := fmt.Sprintf("%s/bracket_%s_%04d.go", repoPrefix, tag, i)
		nodes = append(nodes, &graph.Node{
			ID: id, Name: fmt.Sprintf("bracket_%s_%04d.go", tag, i), Kind: graph.KindFile,
			FilePath: id, RepoPrefix: repoPrefix,
		})
	}
	return store.AtGeneration(0).AddBatchChecked(nodes, nil)
}

// walBytes is the size of the store's write-ahead log. A TRUNCATE checkpoint
// resets it to zero; a PASSIVE one copies frames back and leaves the file
// where it is, which is the whole difference this file measures.
func walBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat the write-ahead log %s: %v", path, err)
	}
	return info.Size()
}
