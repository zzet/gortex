package store_sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	sqlite "modernc.org/sqlite"
)

func TestLanguageCensusCancellationDoesNotMemoizeFailure(t *testing.T) {
	s, _ := openTempStore(t)
	h := s.AtGeneration(7)
	if err := h.AddBatchChecked([]*graph.Node{{ID: "r/f::F", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/f", Language: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := h.RepoLanguageCountsContext(ctx, []string{"r"}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel: %v/%v", got, err)
	}
	// A pool-blocked read must release promptly on the caller's deadline, and
	// must not install an empty census in the immutable-generation memo.
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
	got, err := h.PublishedRepoLanguageCountsContext(ctx, "r")
	cancel()
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if got != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool cancellation: %v/%v", got, err)
	}
	if _, ok := s.publishedLanguageCounts.Load(publishedLanguageCountKey{generation: 7, repoPrefix: "r"}); ok {
		t.Fatal("failed read was memoized")
	}
	got, err = h.PublishedRepoLanguageCountsContext(context.Background(), "r")
	if err != nil || got["go"] != 1 {
		t.Fatalf("retry: %v/%v", got, err)
	}
	want, err := h.RepoLanguageCountsContext(context.Background(), []string{"r"})
	if err != nil || !reflect.DeepEqual(got, want["r"]) {
		t.Fatalf("counts parity %v/%v", want, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if cached, err := h.PublishedRepoLanguageCountsContext(ctx, "r"); cached != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache hit: %v/%v", cached, err)
	}
	// A SQL error must not become a successful empty projection either.
	if _, err := s.writerDB.Exec("DROP TABLE nodes"); err != nil {
		t.Fatal(err)
	}
	if failed, err := s.AtGeneration(8).PublishedRepoLanguageCountsContext(context.Background(), "r"); failed != nil || err == nil {
		t.Fatalf("SQL failure: %v/%v", failed, err)
	}
	if _, ok := s.publishedLanguageCounts.Load(publishedLanguageCountKey{generation: 8, repoPrefix: "r"}); ok {
		t.Fatal("SQL failure was memoized")
	}
}

// The callback runs inside SQLite's node projection, so this witnesses actual
// VM work before cancellation rather than cancellation at pool admission.
var censusStepRegister sync.Once
var censusStepObserver atomic.Pointer[censusStepProbe]

type censusStepProbe struct {
	rows       int
	canceledAt time.Time
	cancel     context.CancelFunc
}

func TestLanguageCensusCancelsAnExecutingSQLiteStep(t *testing.T) {
	censusStepRegister.Do(func() {
		if err := sqlite.RegisterScalarFunction("gortex_test_census_language", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			if p := censusStepObserver.Load(); p != nil {
				p.rows++
				if p.rows == 128 {
					p.canceledAt = time.Now()
					p.cancel()
				}
			}
			return "go", nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	db, _ := openGenerationLookupDB(t)
	if _, err := db.Exec(`DROP TABLE nodes`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW nodes AS WITH RECURSIVE seq(i) AS (
 SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<1000000)
 SELECT 'r' AS repo_prefix, gortex_test_census_language() AS language,
 'function' AS kind, '' AS data_class, 7 AS view_gen FROM seq`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &censusStepProbe{cancel: cancel}
	censusStepObserver.Store(probe)
	defer censusStepObserver.Store(nil)
	s := &Store{storeCore: &storeCore{db: db}, viewGen: 7}
	started := time.Now()
	got, err := s.PublishedRepoLanguageCountsContext(ctx, "r")
	t.Logf("SQLite projected %d rows; total=%s canceled_interval=%s", probe.rows, time.Since(started), time.Since(probe.canceledAt))
	if probe.rows < 128 || probe.rows >= 1000000 || got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("executing step rows=%d got=%v err=%v", probe.rows, got, err)
	}
	if time.Since(probe.canceledAt) > time.Second {
		t.Fatalf("cancellation did not finish promptly: %s", time.Since(probe.canceledAt))
	}
	if _, ok := s.publishedLanguageCounts.Load(publishedLanguageCountKey{generation: 7, repoPrefix: "r"}); ok {
		t.Fatal("interrupted SQLite step was memoized")
	}
	t.Logf("SQLite executed %d language projections before cancellation; elapsed=%s", probe.rows, time.Since(started))
}
