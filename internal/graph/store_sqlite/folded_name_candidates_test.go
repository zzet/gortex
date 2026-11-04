package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestFoldedNameSnapshotKeepsSelectionHydrationAndLateFieldsTogether(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "folded-name-snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	stopStoreBackgroundReaders(s)
	s.AddBatch([]*graph.Node{
		{ID: "early", Name: "Other", Kind: graph.KindFunction, FilePath: "early.go", Language: "go"},
		{ID: "late", Name: "Needle", Kind: graph.KindFunction, FilePath: "late.go", Language: "go", Meta: map[string]any{"is_test": false}},
	}, nil)
	// A single reader proves hydration reuses the selected transaction rather
	// than borrowing a second pooled connection while its snapshot is pinned.
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	keys, more, selectErr := selectFoldedNameCandidates(ctx, tx, s.viewGen, "Needle", 1, graph.NameSearchFilter{})
	var writeErr error
	func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		write, err := s.beginWriteContext(ctx)
		if err != nil {
			writeErr = err
			return
		}
		changed, _, _, err := insertNodeChunksTx(write, s.viewGen, []*graph.Node{
			{ID: "early", Name: "Needle", Kind: graph.KindFunction, FilePath: "early.go", Language: "rust", Meta: map[string]any{"is_test": true}},
			{ID: "late", Name: "Other", Kind: graph.KindFunction, FilePath: "late.go", Language: "rust", Meta: map[string]any{"is_test": true}},
		}, false)
		if err == nil {
			err = write.Commit()
		}
		if err != nil {
			_ = write.Rollback()
			writeErr = err
			return
		}
		s.finishAnalysisMutationLocked(changed > 0)
	}()
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	nodes, hydrateErr := hydrateNameCandidateIDs(ctx, tx, s.viewGen, ids)
	rollbackErr := tx.Rollback()
	current, currentErr := s.GetNodesByIDsContext(ctx, []string{"early", "late"})
	closeErr := s.Close()
	closed = true
	t.Logf("coherent selected_keys=%v more=%v retained=%v current=%v selection_err=%v writer_err=%v hydration_err=%v rollback_err=%v current_err=%v close_err=%v", keys, more, nodes, current, selectErr, writeErr, hydrateErr, rollbackErr, currentErr, closeErr)
	if selectErr != nil || writeErr != nil || hydrateErr != nil || rollbackErr != nil || currentErr != nil || closeErr != nil {
		t.Fatal("real snapshot/write/hydration did not complete")
	}
	if more || len(keys) != 1 || keys[0].ID != "late" || len(nodes) != 1 {
		t.Fatal("selected name head changed or was not complete")
	}
	if nodes[0].ID != "late" || nodes[0].Name != "Needle" || nodes[0].Language != "go" || nodes[0].Meta["is_test"] != false {
		t.Fatal("hydration/late language or metadata predicate mixed versions")
	}
	if current["early"].Name != "Needle" || current["late"].Name != "Other" || current["late"].Language != "rust" {
		t.Fatal("the real independent write was not visible after snapshot release")
	}
}

func TestFoldedNameReadPoolWaitHonorsOriginalContext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "folded-name-cancel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	stopStoreBackgroundReaders(s)
	s.AddNode(&graph.Node{ID: "target", Name: "Needle", Kind: graph.KindFunction})
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	held, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	type answer struct {
		nodes []*graph.Node
		more  bool
		err   error
	}
	done := make(chan answer, 1)
	before := s.db.Stats().WaitCount
	go func() {
		nodes, more, err := s.FindFoldedNameCandidatesContext(ctx, "Needle", 1, graph.NameSearchFilter{})
		done <- answer{nodes, more, err}
	}()
	admitUntil := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(admitUntil) {
		runtime.Gosched()
	}
	waitObserved := s.db.Stats().WaitCount > before
	cancel()
	var result answer
	returnedBeforeRelease := false
	select {
	case result = <-done:
		returnedBeforeRelease = true
	case <-time.After(time.Second):
	}
	heldErr := held.Rollback()
	if !returnedBeforeRelease {
		result = <-done
	}
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	_, _, expiredErr := s.FindFoldedNameCandidatesContext(expired, "Needle", 1, graph.NameSearchFilter{})
	expire()
	closeErr := s.Close()
	if !waitObserved || !returnedBeforeRelease || !errors.Is(result.err, context.Canceled) || len(result.nodes) != 0 || result.more || !errors.Is(expiredErr, context.DeadlineExceeded) || heldErr != nil || closeErr != nil {
		t.Fatalf("pool_wait=%v returned_before_release=%v result=%+v expired=%v held=%v close=%v", waitObserved, returnedBeforeRelease, result, expiredErr, heldErr, closeErr)
	}
}
