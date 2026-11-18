package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// This regression demonstrates a limitation of the cache-token proof,
// not a new snapshot guarantee for the existing paged scanner. The writer
// uses the production mutex, transaction, UPSERT and post-commit finish
// primitives; only the local pause between commit and finish is test-owned.
func TestPagedSearchCacheTokenCannotCertifyCommittedSnapshot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "snapshot-witness.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	stopStoreBackgroundReaders(store)
	const needle = "Needle"
	pageSize := graph.DefaultNodeSearchKeyPageSize
	nodes := make([]*graph.Node, pageSize+1)
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("fixture.go::Node%06d", i), Name: "Other", Kind: graph.KindFunction, FilePath: "fixture.go"}
	}
	earlyID, lateID := nodes[0].ID, nodes[pageSize].ID
	nodes[pageSize].Name = needle
	store.AddBatch(nodes, nil)
	countMatches := func(ctx context.Context) (int, error) {
		var count int
		err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE view_gen = ? AND name = ?`, store.viewGen, needle).Scan(&count)
		return count, err
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	countBefore, err := countMatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if countBefore != 1 {
		t.Fatalf("setup exact matches = %d, want 1", countBefore)
	}
	revisionBefore := store.MutationRevision()
	startWriter := make(chan struct{})
	committed := make(chan error, 1)
	releaseFinish := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		<-startWriter
		writeSwap := func() error {
			store.writeMu.Lock()
			defer store.writeMu.Unlock()
			tx, writeErr := store.beginWriteContext(ctx)
			if writeErr != nil {
				committed <- writeErr
				return writeErr
			}
			changed, _, _, writeErr := insertNodeChunksTx(tx, store.viewGen, []*graph.Node{
				{ID: earlyID, Name: needle, Kind: graph.KindFunction, FilePath: "fixture.go"},
				{ID: lateID, Name: "Other", Kind: graph.KindFunction, FilePath: "fixture.go"},
			}, false)
			if writeErr == nil && changed != 2 {
				writeErr = fmt.Errorf("swapped rows = %d, want 2", changed)
			}
			if writeErr == nil {
				writeErr = tx.Commit()
			}
			if writeErr != nil {
				_ = tx.Rollback()
				committed <- writeErr
				return writeErr
			}
			committed <- nil
			// The same production ordering is commit, then finish while writeMu
			// remains held. Do not synthesize or overwrite either revision.
			<-releaseFinish
			store.finishAnalysisMutationLocked(changed > 0)
			return nil
		}
		writerDone <- writeSwap()
	}()

	var pageSizes []int
	var matches []string
	var commitErr, countErr error
	countWhilePaused := -1
	firstPageLastID := ""
	readerConnectionsInUse := -1
	scanErr := store.ScanNodeSearchKeys(ctx, pageSize, func(page []graph.NodeSearchKey) bool {
		pageSizes = append(pageSizes, len(page))
		for _, key := range page {
			if strings.Contains(strings.ToLower(key.Name), strings.ToLower(needle)) {
				matches = append(matches, key.ID)
			}
		}
		if len(pageSizes) == 1 {
			firstPageLastID = page[len(page)-1].ID
			readerConnectionsInUse = store.db.Stats().InUse
			close(startWriter)
			select {
			case commitErr = <-committed:
			case <-ctx.Done():
				commitErr = ctx.Err()
			}
			if commitErr != nil {
				return false
			}
			countWhilePaused, countErr = countMatches(ctx)
			return countErr == nil
		}
		return true
	})
	// Always release and join the real writer before closing the store or
	// asserting the result. A failed scan must not leave a test-owned gate.
	if len(pageSizes) == 0 {
		close(startWriter)
	}
	revisionWhilePaused := store.MutationRevision()
	close(releaseFinish)
	writerErr := <-writerDone
	revisionAfterFinish := store.MutationRevision()
	countAfter, afterErr := countMatches(ctx)
	closeErr := store.Close()
	closed = true

	t.Logf("snapshot witness page_size=%d pages=%v first_page_last=%q early=%q late=%q SQL_matches_before=%d committed_paused=%d after_finish=%d scanned_matches=%v revision_before=%d while_paused=%d after_finish=%d first_callback_reader_connections=%d scan_error=%v commit_error=%v writer_error=%v count_error=%v after_error=%v close_error=%v",
		pageSize, pageSizes, firstPageLastID, earlyID, lateID, countBefore, countWhilePaused, countAfter, matches, revisionBefore, revisionWhilePaused, revisionAfterFinish, readerConnectionsInUse, scanErr, commitErr, writerErr, countErr, afterErr, closeErr)
	if scanErr != nil || commitErr != nil || writerErr != nil || countErr != nil || afterErr != nil || closeErr != nil {
		t.Fatal("the real SQL transaction/scan must complete before this witness can classify the cache-token proof")
	}
	if len(pageSizes) != 2 || pageSizes[0] != pageSize || pageSizes[1] != 1 || firstPageLastID != nodes[pageSize-1].ID {
		t.Fatal("the actual default page boundary was not exercised")
	}
	if readerConnectionsInUse != 0 {
		t.Fatal("the first page retained a reader cursor during the writer rendezvous")
	}
	if countBefore != 1 || countWhilePaused != 1 || countAfter != 1 {
		t.Fatal("an exact match must exist on both sides of the single atomic swap")
	}
	if revisionBefore != revisionWhilePaused || revisionAfterFinish <= revisionWhilePaused {
		t.Fatal("the real post-commit revision ordering was not witnessed")
	}
	if len(matches) != 0 {
		t.Fatal("this controlled stitched scan did not witness the cache-token proof limitation")
	}
}
