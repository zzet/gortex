package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type foldBlockedScanner struct {
	entered chan struct{}
	release chan struct{}
}

func (s foldBlockedScanner) Scan(any) error {
	close(s.entered)
	<-s.release
	return nil
}

// Cancellation can mark sql.Tx done while rollback still waits for a scan to
// finish. A pinned bulk connection cannot be handed to the next writer until
// SQLite, rather than only database/sql's done flag, has ended the transaction.
func TestCanceledFoldKeepsPinnedWriterUntilRollbackCompletes(t *testing.T) {
	store, handle, id := reparseWindowFixture(t)
	obsolete := &graph.Node{ID: "repo::Obsolete", Kind: graph.KindFunction, Name: "Obsolete", RepoPrefix: "repo"}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{obsolete}, nil))
	opened, err := handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.NoError(t, err)
	require.True(t, opened)
	t.Cleanup(func() { require.NoError(t, handle.EndGenerationBulkLoadFor(id)) })
	to := reservedGeneration(t, store, "fold-transaction")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scanner := foldBlockedScanner{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseScan := func() { releaseOnce.Do(func() { close(scanner.release) }) }
	t.Cleanup(releaseScan)
	callbackDone := make(chan struct{})
	scanDone := make(chan error, 1)
	foldDone := make(chan error, 1)
	go func() {
		foldDone <- store.withFoldTx(ctx, to, func(stepCtx context.Context, tx *sql.Tx) error {
			defer close(callbackDone)
			if _, err := tx.ExecContext(stepCtx, `INSERT INTO nodes(id,view_gen,kind,name,file_path,repo_prefix) VALUES('fold::Canceled',?,'function','Canceled','repo/fold.go','repo')`, to); err != nil {
				return err
			}
			rows, err := tx.QueryContext(context.WithoutCancel(stepCtx), `SELECT 1`)
			if err != nil {
				return err
			}
			if !rows.Next() {
				err := rows.Err()
				_ = rows.Close()
				if err != nil {
					return err
				}
				return errors.New("fold scan fixture has no row")
			}
			go func() {
				err := rows.Scan(scanner)
				_ = rows.Close()
				scanDone <- err
			}()
			<-scanner.entered
			cancel()
			// On the broken path, wait until automatic rollback has claimed the
			// Tx but is stuck behind Scan. An owned lifetime stays live throughout
			// this bounded observation and rolls back synchronously on return.
			deadline := time.Now().Add(25 * time.Millisecond)
			for time.Now().Before(deadline) {
				_, err := tx.ExecContext(context.WithoutCancel(stepCtx), `SELECT 1`)
				if errors.Is(err, sql.ErrTxDone) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			return stepCtx.Err()
		})
	}()
	waitCheckpointSignal(t, callbackDone)
	retired := make(chan error, 1)
	go func() {
		_, _, err := handle.EvictRepoForShadowReplacement(t.Context(), "repo")
		retired <- err
	}()
	var retirementErr error
	retiredBeforeCleanup := false
	select {
	case retirementErr = <-retired:
		retiredBeforeCleanup = true
	case <-time.After(25 * time.Millisecond):
	}
	releaseScan()
	require.NoError(t, <-scanDone)
	require.ErrorIs(t, <-foldDone, context.Canceled)
	if !retiredBeforeCleanup {
		retirementErr = <-retired
	}
	require.NoError(t, retirementErr, "the next writer must never inherit the canceled fold's SQLite transaction")
	require.False(t, retiredBeforeCleanup, "the writer gate must cover the actual rollback")
	require.Nil(t, handle.GetNode(obsolete.ID))
	require.Nil(t, store.AtGeneration(to).GetNode("fold::Canceled"), "the canceled fold must roll back its payload")
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{{ID: "repo::Fresh", Kind: graph.KindFunction, Name: "Fresh", RepoPrefix: "repo"}}, nil))
}

// The SQLite busy handler already holds a blocked IMMEDIATE begin until its
// busy_timeout expires. Detaching the fold's transaction lifetime must preserve
// that bound and return cancellation without invoking the payload callback.
func TestFoldOwnedLifetimePreservesBusyBeginCancellationBound(t *testing.T) {
	const busyMillis = 250
	var elapsed [2]time.Duration
	for mode := range 2 {
		store, handle, id := reparseWindowFixture(t)
		to := reservedGeneration(t, store, "busy-fold")
		setWriterBusyTimeout(t, store, busyMillis)
		opened, err := handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
		require.NoError(t, err)
		require.True(t, opened)
		blocker, held := holdExternalWriter(t, store.dbPath)
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		before := sqliteSleeps.busyCalls.Load()
		started := time.Now()
		go func() {
			if mode == 0 {
				store.writeMu.Lock()
				defer store.writeMu.Unlock()
				tx, err := store.AtGeneration(to).beginWriteContext(ctx)
				if tx != nil {
					_ = tx.Rollback()
				}
				result <- err
				return
			}
			result <- store.withFoldTx(ctx, to, func(context.Context, *sql.Tx) error {
				return errors.New("canceled busy begin reached payload work")
			})
		}()
		require.Eventually(t, func() bool { return sqliteSleeps.busyCalls.Load() > before }, time.Second, time.Millisecond)
		cancel()
		err = <-result
		elapsed[mode] = time.Since(started)
		t.Logf("owned=%t canceled blocked BEGIN returned after %s: %v", mode == 1, elapsed[mode], err)
		require.Error(t, err)
		require.NoError(t, held.Rollback())
		require.NoError(t, blocker.Close())
		require.NoError(t, handle.EndGenerationBulkLoadFor(id))
		if mode == 1 {
			require.ErrorIs(t, err, context.Canceled)
		}
	}
	require.Less(t, elapsed[1], elapsed[0]+100*time.Millisecond, "fold ownership must not add another SQLite busy timeout")
}
