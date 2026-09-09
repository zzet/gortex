package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/zzet/gortex/internal/graph"
)

// Uses the actual sole writer connection of a private non-bulk Store. This
// changes only that test database's SQLite page ceiling, never filesystem space.
func privateFullPageLimit(ctx context.Context, s *Store, limit int64) (value int64, err error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.writeMu.Unlock()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	statement := "PRAGMA max_page_count"
	if limit > 0 {
		statement += fmt.Sprintf(" = %d", limit)
	}
	err = conn.QueryRowContext(ctx, statement).Scan(&value)
	return value, err
}

func TestPrivateSQLiteFullAddBatchClassification(t *testing.T) {
	for _, mode := range []string{"healthy", "sqlite_full"} {
		t.Run(mode, func(t *testing.T) {
			f := newSealedEmissionFixture(t)
			if f.owner.bulkConn != nil {
				t.Fatal("this page-limit fixture requires the actual non-bulk writer pool")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			base := &graph.Node{ID: "other/keep.go::Base", Name: "Base", Kind: graph.KindFunction,
				FilePath: "other/keep.go", RepoPrefix: "other"}
			prior := &graph.Node{ID: "repo/prior.go::Prior", Name: "Prior", Kind: graph.KindFunction,
				FilePath: "repo/prior.go", RepoPrefix: "repo"}
			f.owner.AddBatch([]*graph.Node{base}, nil)
			f.handle.AddBatch([]*graph.Node{prior}, nil)
			var pages, pageSize, freePages int64
			for query, dest := range map[string]*int64{
				"PRAGMA page_count": &pages, "PRAGMA page_size": &pageSize, "PRAGMA freelist_count": &freePages,
			} {
				if err := f.owner.writerDB.QueryRowContext(ctx, query).Scan(dest); err != nil {
					t.Fatal(err)
				}
			}
			payloadBytes := (freePages+8)*pageSize + 256*1024
			if pages <= 0 || pageSize <= 0 || freePages < 0 || payloadBytes > 8*1024*1024 {
				t.Fatalf("fixture resource bounds: pages=%d page_size=%d free=%d payload=%d", pages, pageSize, freePages, payloadBytes)
			}
			previousLimit, err := privateFullPageLimit(ctx, f.owner, 0)
			if err != nil {
				t.Fatal(err)
			}
			restore := func() error {
				restoreCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				_, err := privateFullPageLimit(restoreCtx, f.owner, previousLimit)
				return err
			}
			// Defer restoration before the fixture's owning Store.Close even
			// if an assertion exits early.
			defer func() {
				if err := restore(); err != nil {
					t.Errorf("private page ceiling restoration: %v", err)
				}
			}()
			if mode == "sqlite_full" {
				capped, err := privateFullPageLimit(ctx, f.owner, pages)
				if err != nil || capped != pages {
					t.Fatalf("page limit not installed on writer: got=%d want=%d err=%v", capped, pages, err)
				}
			}
			big := &graph.Node{ID: "repo/full.go::Large", Name: strings.Repeat("x", int(payloadBytes)),
				Kind: graph.KindFunction, FilePath: "repo/full.go", RepoPrefix: "repo"}
			value := captureStorageEmissionPanic(func() { f.handle.AddBatch([]*graph.Node{big}, nil) })
			panicErr, isError := value.(error)
			classified, recognized := StorageErrorFromPanic(value)
			var coded interface{ Code() int }
			hasSQLiteCode := errors.As(panicErr, &coded)
			code := -1
			if hasSQLiteCode {
				code = coded.Code()
			}
			t.Logf("mode=%s generation=%d pages=%d page_size=%d free_pages=%d payload_bytes=%d panic_type=%T driver_code=%d recognized=%v classified_type=%T",
				mode, f.id, pages, pageSize, freePages, payloadBytes, value, code, recognized, classified)
			for current, depth := panicErr, 0; current != nil && depth < 8; current, depth = errors.Unwrap(current), depth+1 {
				t.Logf("panic_chain[%d]=%T", depth, current)
			}
			if mode == "healthy" {
				if value != nil || f.handle.GetNode(big.ID) == nil {
					t.Fatalf("healthy payload control failed: panic=%T %v", value, value)
				}
			} else {
				// 13 is SQLite's primary SQLITE_FULL code, also reported in
				// the user's actual crash. A message alone is not evidence.
				if !isError || !hasSQLiteCode || code&0xff != 13 {
					t.Fatalf("fixture did not cause actual SQLITE_FULL: panic=%T %v code=%d", value, value, code)
				}
				if errors.Is(panicErr, ErrPayloadGenerationSealed) {
					t.Fatal("disk-full fixture accidentally hit the sealed lifecycle path")
				}
				if !recognized || classified == nil {
					t.Errorf("actual SQLITE_FULL AddBatch panic is not recognized by storage recovery: %T %v", value, value)
				}
				if recognized && !errors.Is(classified, panicErr) {
					t.Error("classification dropped the original full error chain")
				}
				if f.handle.GetNode(big.ID) != nil {
					t.Error("failed full batch persisted its node")
				}
			}
			if f.owner.GetNode(base.ID) == nil || f.handle.GetNode(prior.ID) == nil {
				t.Error("full batch damaged preexisting payload")
			}
			if f.owner.GetNode(big.ID) != nil {
				t.Error("positive payload leaked into generation zero")
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			after := &graph.Node{ID: "repo/after.go::After", Name: "After", Kind: graph.KindFunction,
				FilePath: "repo/after.go", RepoPrefix: "repo"}
			if value := captureStorageEmissionPanic(func() { f.handle.AddBatch([]*graph.Node{after}, nil) }); value != nil {
				t.Fatalf("healthy write after lifting page limit: %T %v", value, value)
			}
			if f.handle.GetNode(after.ID) == nil {
				t.Error("healthy post-limit write was not persisted")
			}
		})
	}
}

// Capture a real driver error once, outside benchmark timing. No fake driver
// constructors, repeated timed database filling, or production replacements.
func privateCaptureRealSQLiteFull(t testing.TB) *sqlite.Error {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "emitter.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var pages int64
	if err := s.writerDB.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	oldLimit, err := privateFullPageLimit(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		restoreCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if _, err := privateFullPageLimit(restoreCtx, s, oldLimit); err != nil {
			t.Errorf("restore private capture ceiling: %v", err)
		}
	}()
	if actual, err := privateFullPageLimit(ctx, s, pages); err != nil || actual != pages {
		t.Fatalf("capture ceiling: actual=%d want=%d err=%v", actual, pages, err)
	}
	id := "private/full.go::CapturedFull"
	value := captureStorageEmissionPanic(func() {
		s.AddBatch([]*graph.Node{{ID: id, Name: strings.Repeat("x", 256*1024), Kind: graph.KindFunction,
			FilePath: "private/full.go", RepoPrefix: "private"}}, nil)
	})
	panicErr, ok := value.(error)
	var driverErr *sqlite.Error
	if !ok || !errors.As(panicErr, &driverErr) || driverErr == nil || driverErr.Code()&0xff != 13 {
		t.Fatalf("expected real driver FULL, got %T %v", value, value)
	}
	if s.GetNode(id) != nil {
		t.Fatal("capture FULL batch persisted payload")
	}
	return driverErr
}

type privateNonSQLiteFullCode struct{}

func (privateNonSQLiteFullCode) Error() string { return "database or disk is full (13)" }
func (privateNonSQLiteFullCode) Code() int     { return 13 }

func TestPrivateSQLiteFullEmitterCauseMatrix(t *testing.T) {
	full := privateCaptureRealSQLiteFull(t)
	typed := &StorageError{err: full}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"direct", full},
		{"wrapped", fmt.Errorf("operation: %w", full)},
		{"typed", typed},
		{"wrapped_typed", fmt.Errorf("operation: %w", typed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := captureStorageEmissionPanic(func() { panicOnFatal(tc.err) })
			classified, ok := StorageErrorFromPanic(value)
			if !ok || classified == nil {
				t.Fatalf("real FULL emitter was not classified: %T %v", value, value)
			}
			if !errors.Is(classified, tc.err) || !errors.Is(classified, full) {
				t.Error("normalization lost original wrapper or driver cause")
			}
			if classified.Error() != "store_sqlite: "+tc.err.Error() {
				t.Errorf("message changed: %q", classified.Error())
			}
		})
	}
}

func TestPrivateSQLiteFullEmitterUnrelatedAndPrecedence(t *testing.T) {
	full := privateCaptureRealSQLiteFull(t)
	for _, err := range []error{nil, errors.Join(sql.ErrNoRows, full), errors.Join(sql.ErrConnDone, full)} {
		if value := captureStorageEmissionPanic(func() { panicOnFatal(err) }); value != nil {
			t.Errorf("existing nonfatal precedence changed: %T %v", value, value)
		}
	}
	fake := privateNonSQLiteFullCode{}
	for _, err := range []error{fake, fmt.Errorf("operation: %w", fake), errors.New("ordinary programmer error")} {
		value := captureStorageEmissionPanic(func() { panicOnFatal(err) })
		if value == nil {
			t.Error("unrelated error ceased to panic")
		}
		if _, ok := StorageErrorFromPanic(value); ok {
			t.Errorf("non-SQLite programmer error became classified: %T %v", value, value)
		}
	}
	// The direct-only classifier is not being relaxed by this emitter fix.
	for _, value := range []any{full, fmt.Errorf("wrapper: %w", full), fake} {
		if _, ok := StorageErrorFromPanic(value); ok {
			t.Errorf("classifier accepted non-StorageError payload: %T", value)
		}
	}
}

// Nonparallel observable sinks keep the direct classifier in the timed loop.
var privateSQLiteEmissionErrorSink error
var privateSQLiteEmissionRecognizedSink bool

func BenchmarkPrivateSQLiteFullEmission(b *testing.B) {
	full := privateCaptureRealSQLiteFull(b)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"nonfatal_no_rows", sql.ErrNoRows},
		{"real_full", full},
		{"wrapped_real_full", fmt.Errorf("operation: %w", full)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				value := captureStorageEmissionPanic(func() { panicOnFatal(tc.err) })
				privateSQLiteEmissionErrorSink, privateSQLiteEmissionRecognizedSink = StorageErrorFromPanic(value)
			}
			recognized := float64(0)
			if privateSQLiteEmissionRecognizedSink {
				recognized = 1
			}
			b.ReportMetric(recognized, "recognized/op")
		})
	}
}

// PrivateSQLiteFullWriterState exists only in the Store test binary.
type PrivateSQLiteFullWriterState struct {
	MaxPageCount int64
	PageCount    int64
	PageSize     int64
	FreePages    int64
}

// PrivateSQLiteFullWriterStateForTest changes only the fixture writer's ceiling.
// The external Store test uses this test-only export; no production API is added.
func PrivateSQLiteFullWriterStateForTest(ctx context.Context, s *Store, limit int64) (state PrivateSQLiteFullWriterState, err error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return state, err
	}
	defer s.writeMu.Unlock()
	if s.bulkConn != nil {
		return state, fmt.Errorf("private SQLITE_FULL fixture requires non-bulk writer")
	}
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return state, err
	}
	defer release()
	statement := "PRAGMA max_page_count"
	if limit > 0 {
		statement += fmt.Sprintf(" = %d", limit)
	}
	if err := conn.QueryRowContext(ctx, statement).Scan(&state.MaxPageCount); err != nil {
		return state, err
	}
	for query, destination := range map[string]*int64{
		"PRAGMA page_count":     &state.PageCount,
		"PRAGMA page_size":      &state.PageSize,
		"PRAGMA freelist_count": &state.FreePages,
	} {
		if err := conn.QueryRowContext(ctx, query).Scan(destination); err != nil {
			return state, err
		}
	}
	return state, nil
}
