package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func newProducerWithdrawalTestGeneration(t testing.TB, store *Store, producers ...string) (int64, *Store) {
	t.Helper()
	generationID, generation, err := store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
		OwnerKind:      "withdrawal_test",
		GenerationKind: "withdrawal_test",
		TreeOID:        fmt.Sprintf("tree-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano()),
		ConfigHash:     "withdrawal-test",
		CreatedAt:      time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("begin payload generation: %v", err)
	}
	rows := make([]ProducerCompleteness, 0, len(producers))
	for _, producer := range producers {
		rows = append(rows, ProducerCompleteness{Producer: producer, State: ProducerStateComplete})
	}
	if err := generation.SetProducerStates(rows); err != nil {
		t.Fatalf("seed producer states: %v", err)
	}
	return generationID, generation
}

func waitForProducerState(t testing.TB, catalog *Catalog, generationID int64, producer string, want ProducerState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		availability, err := catalog.ReadProducerAvailability(context.Background(), generationID, producer)
		if err != nil {
			t.Fatalf("read producer availability: %v", err)
		}
		if availability.Declared && availability.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer %q did not reach %q; last=%+v", producer, want, availability)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStoreProducerWithdrawalDrainBeforeClosePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "withdrawal.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID, derived := newProducerWithdrawalTestGeneration(t, store,
		"source.snapshot", "source.invalid_before", "source.invalid_boundary", "graph.syntax")
	if err := derived.Close(); err != nil {
		t.Fatalf("derived close: %v", err)
	}

	longReason := strings.Repeat("é", maxProducerWithdrawalReasonBytes)
	if !derived.ScheduleProducerWithdrawal(generationID, "source.snapshot", longReason) {
		t.Fatal("schedule rejected before owner close")
	}
	invalidBefore := "bad\xffreason"
	if !derived.ScheduleProducerWithdrawal(generationID, "source.invalid_before", invalidBefore) {
		t.Fatal("invalid-before reason schedule rejected")
	}
	invalidBoundary := strings.Repeat("a", maxProducerWithdrawalReasonBytes-1) + "\xfftail"
	if !derived.ScheduleProducerWithdrawal(generationID, "source.invalid_boundary", invalidBoundary) {
		t.Fatal("invalid-boundary reason schedule rejected")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("owner close: %v", err)
	}
	if derived.ScheduleProducerWithdrawal(generationID, "source.snapshot", "after close") {
		t.Fatal("schedule accepted after owner close")
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	sourceState, err := reopened.Catalog().ReadProducerAvailability(context.Background(), generationID, "source.snapshot")
	if err != nil {
		t.Fatalf("read source state after reopen: %v", err)
	}
	if !sourceState.Declared || sourceState.State != ProducerStateUnavailable {
		t.Fatalf("source state after reopen = %+v, want unavailable", sourceState)
	}
	structural, err := reopened.Catalog().ReadProducerAvailability(context.Background(), generationID, "graph.syntax")
	if err != nil {
		t.Fatalf("read structural state after reopen: %v", err)
	}
	if !structural.Declared || structural.State != ProducerStateComplete {
		t.Fatalf("structural state after reopen = %+v, want complete", structural)
	}
	var persistedReason string
	if err := reopened.db.QueryRow(`SELECT reason FROM generation_producer_completeness WHERE view_gen = ? AND producer = ?`, generationID, "source.snapshot").Scan(&persistedReason); err != nil {
		t.Fatalf("read persisted reason: %v", err)
	}
	if len(persistedReason) > maxProducerWithdrawalReasonBytes || !utf8.ValidString(persistedReason) {
		t.Fatalf("persisted reason is not bounded valid UTF-8: bytes=%d valid=%v", len(persistedReason), utf8.ValidString(persistedReason))
	}
	if err := reopened.db.QueryRow(`SELECT reason FROM generation_producer_completeness WHERE view_gen = ? AND producer = ?`, generationID, "source.invalid_before").Scan(&persistedReason); err != nil {
		t.Fatalf("read invalid-before reason: %v", err)
	}
	if want := strings.ToValidUTF8(invalidBefore, "\uFFFD"); persistedReason != want {
		t.Fatalf("invalid-before reason = %q, want %q", persistedReason, want)
	}
	if err := reopened.db.QueryRow(`SELECT reason FROM generation_producer_completeness WHERE view_gen = ? AND producer = ?`, generationID, "source.invalid_boundary").Scan(&persistedReason); err != nil {
		t.Fatalf("read invalid-boundary reason: %v", err)
	}
	if want := strings.Repeat("a", maxProducerWithdrawalReasonBytes-1); persistedReason != want {
		t.Fatalf("invalid-boundary reason bytes=%d value=%q, want %d-byte valid prefix", len(persistedReason), persistedReason, len(want))
	}
	if !utf8.ValidString(persistedReason) {
		t.Fatal("invalid-boundary persisted reason is not valid UTF-8")
	}
}

func TestStoreProducerWithdrawalScheduleRacesOwnerClose(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID, _ := newProducerWithdrawalTestGeneration(t, store, "source.snapshot")

	start := make(chan struct{})
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
					store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "race")
				}
			}
		}()
	}
	close(start)
	closeErr := make(chan error, 1)
	go func() {
		closeErr <- store.Close()
		close(stop)
	}()
	workers.Wait()
	if err := <-closeErr; err != nil {
		t.Fatalf("close: %v", err)
	}
	if store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "late") {
		t.Fatal("schedule accepted after close")
	}
}

func TestStoreProducerWithdrawalRetriesWriterContention(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	generationID, _ := newProducerWithdrawalTestGeneration(t, store, "source.snapshot")

	store.producerWithdrawals.close()
	events := make(chan producerWithdrawalEvent, 16)
	catalog := store.Catalog()
	store.producerWithdrawals = newProducerWithdrawalManager(
		catalog.WithdrawProducer,
		catalog.classifyProducerWithdrawal,
		producerWithdrawalConfig{
			attemptTimeout: 10 * time.Millisecond,
			initialBackoff: time.Millisecond,
			maxBackoff:     5 * time.Millisecond,
			shutdownBudget: time.Second,
			observe: func(event producerWithdrawalEvent) {
				store.observeProducerWithdrawal(event)
				select {
				case events <- event:
				default:
				}
			},
		},
	)

	store.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			store.writeMu.Unlock()
		}
	}()
	if !store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "contention") {
		t.Fatal("schedule rejected")
	}
	select {
	case event := <-events:
		if event.Disposition != producerWithdrawalTransient {
			t.Fatalf("first contention disposition = %s, want transient", event.Disposition)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for contention attempt")
	}
	store.writeMu.Unlock()
	locked = false
	waitForProducerState(t, catalog, generationID, "source.snapshot", ProducerStateUnavailable)
}

func TestProducerWithdrawalManagerCloseBoundedByHeldSQLiteWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held-writer.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID, _ := newProducerWithdrawalTestGeneration(t, store, "source.snapshot")

	store.producerWithdrawals.close()
	var events []producerWithdrawalEvent
	catalog := store.Catalog()
	store.producerWithdrawals = newProducerWithdrawalManager(
		catalog.WithdrawProducer,
		catalog.classifyProducerWithdrawal,
		producerWithdrawalConfig{
			attemptTimeout: 15 * time.Millisecond,
			initialBackoff: time.Millisecond,
			maxBackoff:     2 * time.Millisecond,
			shutdownBudget: 40 * time.Millisecond,
			observe: func(event producerWithdrawalEvent) {
				store.observeProducerWithdrawal(event)
				events = append(events, event)
			},
		},
	)

	writerConn, err := store.writerDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("writer connection: %v", err)
	}
	var originalBusyTimeout int
	if err := writerConn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&originalBusyTimeout); err != nil {
		t.Fatalf("read original busy timeout: %v", err)
	}
	if err := writerConn.Close(); err != nil {
		t.Fatalf("close writer connection: %v", err)
	}

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

	if !store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "held writer") {
		t.Fatal("schedule rejected")
	}
	started := time.Now()
	store.producerWithdrawals.close()
	elapsed := time.Since(started)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("manager close exceeded bounded shutdown: %s", elapsed)
	}
	stats := store.ProducerWithdrawalStats()
	if stats.FinalFailures != 1 {
		t.Fatalf("final failures=%d, want 1; stats=%+v events=%+v", stats.FinalFailures, stats, events)
	}
	if store.producerWithdrawals.pending() != 0 {
		t.Fatalf("pending tasks remain after joined close: %d", store.producerWithdrawals.pending())
	}
	if store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "late") {
		t.Fatal("closed manager accepted a task")
	}
	var finalErr error
	for _, event := range events {
		if event.Final {
			finalErr = event.Err
		}
	}
	if finalErr == nil {
		t.Fatalf("no final failed event observed: %+v", events)
	}
	// An invalid generation would make any readback fail. Transient here proves
	// direct BUSY/retry-exhaustion classification returns before readback.
	disposition, classifyErr := catalog.classifyProducerWithdrawal(context.Background(), -1, "source.snapshot", finalErr)
	if classifyErr != nil || disposition != producerWithdrawalTransient {
		t.Fatalf("direct busy classification = (%s, %v), want transient without readback", disposition, classifyErr)
	}

	availability, err := catalog.ReadProducerAvailability(context.Background(), generationID, "source.snapshot")
	if err != nil {
		t.Fatalf("read state while writer held: %v", err)
	}
	if !availability.Declared || availability.State != ProducerStateComplete {
		t.Fatalf("failed withdrawal changed producer state: %+v", availability)
	}
	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release external writer: %v", err)
	}
	locked = false

	writerConn, err = store.writerDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("writer connection after withdrawal: %v", err)
	}
	var restoredBusyTimeout int
	if err := writerConn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&restoredBusyTimeout); err != nil {
		t.Fatalf("read restored busy timeout: %v", err)
	}
	if err := writerConn.Close(); err != nil {
		t.Fatalf("close restored writer connection: %v", err)
	}
	if restoredBusyTimeout != originalBusyTimeout {
		t.Fatalf("busy_timeout=%d after withdrawal, want prior %d", restoredBusyTimeout, originalBusyTimeout)
	}
	if err := catalog.WithdrawProducer(context.Background(), generationID, "source.snapshot", "ordinary writer after restore"); err != nil {
		t.Fatalf("ordinary catalog writer after restore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("owner close after releasing external writer: %v", err)
	}
}

func TestCatalogClassifiesProducerWithdrawalReadback(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	generationID, _ := newProducerWithdrawalTestGeneration(t, store, "source.snapshot")
	catalog := store.Catalog()
	persistentErr := errors.New("permanent withdrawal failure")

	disposition, err := catalog.classifyProducerWithdrawal(context.Background(), generationID, "missing", persistentErr)
	if err != nil || disposition != producerWithdrawalSatisfied {
		t.Fatalf("missing producer classification = (%s, %v), want satisfied", disposition, err)
	}
	disposition, err = catalog.classifyProducerWithdrawal(context.Background(), generationID, "source.snapshot", persistentErr)
	if err != nil || disposition != producerWithdrawalPersistent {
		t.Fatalf("available producer classification = (%s, %v), want persistent", disposition, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	disposition, err = catalog.classifyProducerWithdrawal(canceled, generationID, "source.snapshot", context.Canceled)
	if err != nil || disposition != producerWithdrawalTransient {
		t.Fatalf("canceled classification = (%s, %v), want transient", disposition, err)
	}
	if err := catalog.WithdrawProducer(context.Background(), generationID, "source.snapshot", "gone"); err != nil {
		t.Fatalf("withdraw producer: %v", err)
	}
	staleErr := catalog.WithdrawProducer(context.Background(), generationID, "source.snapshot", "again")
	if staleErr == nil {
		t.Fatal("repeat withdrawal unexpectedly succeeded")
	}
	disposition, err = catalog.classifyProducerWithdrawal(context.Background(), generationID, "source.snapshot", staleErr)
	if err != nil || disposition != producerWithdrawalSatisfied {
		t.Fatalf("stale unavailable classification = (%s, %v), want satisfied", disposition, err)
	}
}

func TestProducerWithdrawalObserverIsNonBlocking(t *testing.T) {
	store := &Store{storeCore: &storeCore{}}
	done := make(chan struct{})
	go func() {
		for range 100_000 {
			store.observeProducerWithdrawal(producerWithdrawalEvent{Disposition: producerWithdrawalTransient})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("atomics-only observer blocked")
	}
	stats := store.ProducerWithdrawalStats()
	if stats.Attempts != 100_000 || stats.Transient != 100_000 {
		t.Fatalf("observer counters = %+v", stats)
	}
}

func BenchmarkProducerWithdrawalCloseHeldWriter(b *testing.B) {
	path := filepath.Join(b.TempDir(), "held-writer-benchmark.sqlite")
	store, err := Open(path)
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	generationID, _ := newProducerWithdrawalTestGeneration(b, store, "source.snapshot")
	store.producerWithdrawals.close()
	catalog := store.Catalog()

	locker, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatalf("open external writer: %v", err)
	}
	defer locker.Close()
	lockConn, err := locker.Conn(context.Background())
	if err != nil {
		b.Fatalf("external writer connection: %v", err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		b.Fatalf("hold external writer: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		manager := newProducerWithdrawalManager(
			catalog.WithdrawProducer,
			catalog.classifyProducerWithdrawal,
			producerWithdrawalConfig{
				attemptTimeout: 3 * time.Millisecond,
				initialBackoff: time.Millisecond,
				maxBackoff:     time.Millisecond,
				shutdownBudget: 8 * time.Millisecond,
			},
		)
		store.producerWithdrawals = manager
		if !manager.schedule(generationID, "source.snapshot", "held writer benchmark") {
			b.Fatal("schedule rejected")
		}
		b.StartTimer()
		manager.close()
	}
	b.StopTimer()
	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		b.Fatalf("release external writer: %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("close store: %v", err)
	}
}

func BenchmarkScheduleProducerWithdrawalContended(b *testing.B) {
	store := &Store{storeCore: &storeCore{}}
	store.producerWithdrawals = newProducerWithdrawalManager(
		func(ctx context.Context, _ int64, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
		nil,
		producerWithdrawalConfig{attemptTimeout: time.Hour, shutdownBudget: time.Millisecond},
	)
	b.Cleanup(store.producerWithdrawals.close)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			store.ScheduleProducerWithdrawal(1, "source.snapshot", "missing object")
		}
	})
}

func BenchmarkCatalogWithdrawProducerContendedSynchronous(b *testing.B) {
	path := filepath.Join(b.TempDir(), "synchronous-baseline.sqlite")
	store, err := Open(path)
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer store.Close()
	generationID, _ := newProducerWithdrawalTestGeneration(b, store, "source.snapshot")

	locker, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatalf("open writer locker: %v", err)
	}
	defer locker.Close()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		b.Fatalf("writer locker conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		b.Fatalf("begin writer lock: %v", err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := store.Catalog().WithdrawProducer(ctx, generationID, "source.snapshot", "synchronous request-path baseline")
		cancel()
		if err == nil {
			b.Fatal("contended synchronous withdrawal unexpectedly succeeded")
		}
	}
}

func BenchmarkCatalogProducerWithdrawalDrain(b *testing.B) {
	store, err := Open(":memory:")
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer store.Close()
	producers := make([]string, b.N)
	for i := range producers {
		producers[i] = fmt.Sprintf("source.snapshot.%d", i)
	}
	generationID, _ := newProducerWithdrawalTestGeneration(b, store, producers...)
	b.ReportAllocs()
	b.ResetTimer()
	for _, producer := range producers {
		if !store.ScheduleProducerWithdrawal(generationID, producer, "missing object") {
			b.Fatalf("schedule %s rejected", producer)
		}
	}
	store.producerWithdrawals.close()
	b.StopTimer()
	stats := store.ProducerWithdrawalStats()
	if stats.Satisfied != uint64(b.N) {
		b.Fatalf("satisfied=%d, want %d", stats.Satisfied, b.N)
	}
}
