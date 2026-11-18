package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestInitialCopyYieldsAfterSQLAndRollsBackBeforeWriter(t *testing.T) {
	store := openCatalogStore(t)
	store.stopCheckpointLoop()
	seedGenerationZero(t, store, copyFixtureRepo, 32, 64)
	to := reservedGeneration(t, store, "cooperative")
	handle, err := store.AtManagedGeneration(to)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := handle.BeginGenerationBulkLoad(to)
	if err != nil || !opened {
		t.Fatalf("bulk open=%v err=%v", opened, err)
	}
	defer func() {
		if err := handle.EndGenerationBulkLoad(); err != nil {
			t.Error(err)
		}
	}()
	entered := make(chan struct{})
	copied, writer := make(chan error, 1), make(chan error, 1)
	go func() {
		copied <- store.withInitialCopyTx(t.Context(), handle, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := copyGenerationTableTx(ctx, tx, "nodes", 0, to, copyFixtureRepo); err != nil {
				return err
			}
			close(entered) // The copy has executed real SQL inside its pinned transaction.
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-entered
	if got := handle.GetRepoNodes(copyFixtureRepo); len(got) != 0 {
		t.Fatalf("uncommitted copy became visible: %d", len(got))
	}
	go func() {
		writer <- store.AddBatchChecked([]*graph.Node{{ID: "foreground", Name: "foreground", Kind: graph.KindFunction}}, nil)
	}()
	select {
	case err := <-copied:
		if !errors.Is(err, ErrGenerationCopyYielded) {
			t.Fatalf("copy: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued writer did not interrupt copy")
	}
	select {
	case err := <-writer:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer inherited held transaction")
	}
	if got := handle.GetRepoNodes(copyFixtureRepo); len(got) != 0 {
		t.Fatalf("rolled-back payload survived: %d", len(got))
	}
	if store.GetNode("foreground") == nil {
		t.Fatal("foreground write was lost")
	}
	if _, err := store.CopyPayloadGenerationCooperatively(t.Context(), 0, to, copyFixtureRepo); err != nil {
		t.Fatalf("bulk connection unusable after rollback: %v", err)
	}
	for _, table := range append([]string{"nodes", "edges"}, payloadSweepTables()...) {
		if !reflect.DeepEqual(generationRows(t, store, table, 0, copyFixtureRepo), generationRows(t, store, table, to, copyFixtureRepo)) {
			t.Fatalf("retry changed %s payload", table)
		}
	}
	for _, fts := range generationFTSDocidMaps {
		if !reflect.DeepEqual(ftsDocuments(t, store, fts, 0, copyFixtureRepo), ftsDocuments(t, store, fts, to, copyFixtureRepo)) {
			t.Fatalf("retry changed %s", fts.fts)
		}
	}
}

func TestInitialCopyCancellationRollsBackAndLeavesWriterUsable(t *testing.T) {
	store := openCatalogStore(t)
	store.stopCheckpointLoop()
	to := reservedGeneration(t, store, "cancel-copy")
	handle, err := store.AtManagedGeneration(to)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = store.withInitialCopyTx(ctx, handle, func(step context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(step, `INSERT INTO nodes(id,view_gen,kind,name,file_path,repo_prefix)VALUES('canceled',?,'function','canceled','repo/cancel.go','repo')`, to); err != nil {
			return err
		}
		cancel()
		return step.Err()
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrGenerationCopyYielded) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	if handle.GetNode("canceled") != nil {
		t.Fatal("canceled copy committed")
	}
	if err := store.AddBatchChecked([]*graph.Node{{ID: "after", Name: "after", Kind: graph.KindFunction}}, nil); err != nil {
		t.Fatal(err)
	}
}
