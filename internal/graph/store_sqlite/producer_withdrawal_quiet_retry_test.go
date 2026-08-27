package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProducerWithdrawalHeldWriterDoesNotLogPerRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet-retry.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID, _ := newProducerWithdrawalTestGeneration(t, store, "source.snapshot")
	store.producerWithdrawals.close()
	catalog := store.Catalog()
	store.producerWithdrawals = newProducerWithdrawalManager(
		catalog.WithdrawProducer,
		catalog.classifyProducerWithdrawal,
		producerWithdrawalConfig{
			attemptTimeout: 10 * time.Millisecond,
			initialBackoff: time.Millisecond,
			maxBackoff:     2 * time.Millisecond,
			shutdownBudget: 25 * time.Millisecond,
			observe:        store.observeProducerWithdrawal,
		},
	)

	locker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open external writer: %v", err)
	}
	defer locker.Close()
	lockConn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("external writer connection: %v", err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold external writer: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var retryLogs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&retryLogs)
	defer log.SetOutput(previousLogWriter)
	if !store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "quiet held writer") {
		t.Fatal("schedule rejected")
	}
	store.producerWithdrawals.close()
	log.SetOutput(previousLogWriter)

	if strings.Contains(retryLogs.String(), "withdraw producer") || retryLogs.Len() != 0 {
		t.Fatalf("withdrawal busy retry wrote synchronous logs: %q", retryLogs.String())
	}
	stats := store.ProducerWithdrawalStats()
	if stats.FinalFailures != 1 {
		t.Fatalf("final failures=%d, want one aggregate final event; stats=%+v", stats.FinalFailures, stats)
	}
	if stats.Attempts > 2 {
		t.Fatalf("attempt observer count=%d, want at most normal+shutdown aggregation", stats.Attempts)
	}

	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release external writer: %v", err)
	}
	locked = false
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}
