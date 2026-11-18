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
	sqlite "modernc.org/sqlite"
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

// Observe BEGIN preparation on this connection, not a process-wide sleep
// counter: modernc's cancellable busy handler no longer goes through VFS xSleep.
func observeSQLiteBegin(t *testing.T, conn *sql.Conn) <-chan struct{} {
	t.Helper()
	entered := make(chan struct{})
	var once sync.Once
	require.NoError(t, conn.Raw(func(dc any) error {
		authorizer, ok := dc.(sqlite.AuthorizerRegisterer)
		if !ok {
			return errors.New("SQLite connection lacks authorizer support")
		}
		return authorizer.RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, arg1, _, _, _ string) sqlite.AuthorizerReturnCode {
			if action == sqlite.AuthTransaction && arg1 == "BEGIN" {
				once.Do(func() { close(entered) })
			}
			return sqlite.AuthorizerOK
		})
	}))
	return entered
}

// An owned fold must still return within one busy_timeout after cancellation
// without invoking its payload. modernc v1.61 makes a normal BeginTx cancel
// sooner, but the detached lifetime deliberately retains the old timeout
// bound to keep rollback synchronous under the writer gate.
func TestFoldOwnedLifetimePreservesBusyBeginCancellationBound(t *testing.T) {
	const busyMillis = 250
	for _, mode := range []string{"caller_lifetime", "owned_lifetime"} {
		t.Run(mode, func(t *testing.T) {
			store, handle, id := reparseWindowFixture(t)
			to := reservedGeneration(t, store, "busy-fold")
			setWriterBusyTimeout(t, store, busyMillis)
			opened, err := handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
			require.NoError(t, err)
			require.True(t, opened)
			t.Cleanup(func() { require.NoError(t, handle.EndGenerationBulkLoadFor(id)) })
			blocker, held := holdExternalWriter(t, store.dbPath)
			t.Cleanup(func() { _ = held.Rollback(); _ = blocker.Close() })
			entered := observeSQLiteBegin(t, store.bulkConn)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if mode == "caller_lifetime" {
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
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Error("blocked BEGIN did not finish")
				}
			})
			waitCheckpointSignal(t, entered)
			select {
			case err := <-result:
				t.Fatalf("BEGIN returned before cancellation while the external writer holds the lock: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			started := time.Now()
			cancel()
			err = <-result
			elapsed := time.Since(started)
			t.Logf("%s canceled blocked BEGIN returned after %s: %v", mode, elapsed, err)
			require.ErrorIs(t, err, context.Canceled)
			require.Less(t, elapsed, busyMillis*time.Millisecond+100*time.Millisecond,
				"fold ownership must not add another SQLite busy timeout")
			require.NoError(t, held.Rollback())
			require.NoError(t, blocker.Close())
			require.NoError(t, handle.AddBatchChecked([]*graph.Node{{ID: "repo::AfterCancel", Name: "AfterCancel", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil))
		})
	}
}
