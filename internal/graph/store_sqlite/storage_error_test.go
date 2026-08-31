package store_sqlite

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	sqlite "modernc.org/sqlite"
)

func TestStorageErrorFromPanicDiscriminatesStorePanics(t *testing.T) {
	want := errors.New("store failed")
	typed := wrapStorageError(want)
	recovered, ok := StorageErrorFromPanic(typed)
	if !ok || recovered != typed || !errors.Is(recovered, want) {
		t.Fatalf("typed panic = (%v, %t), want recognized %v", recovered, ok, typed)
	}
	for _, value := range []any{nil, "panic", want, fmt.Errorf("wrapped: %w", typed)} {
		if got, recognized := StorageErrorFromPanic(value); recognized || got != nil {
			t.Fatalf("StorageErrorFromPanic(%T) = (%v, %t), want unrecognized", value, got, recognized)
		}
	}
	if got := SafeStorageFailureReason(want); got != "graph generation build failed; see daemon log" {
		t.Fatalf("plain failure reason = %q", got)
	}
	if got := SafeStorageFailureReason(typed); got != "graph storage write failed; see daemon log" {
		t.Fatalf("generic storage failure reason = %q", got)
	}
}

func TestAddBatchCheckedReturnsSQLiteFullWithoutProcessPanic(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_JSONB_INGEST", "0")
	s, _ := openTempStore(t)
	s.BeginBulkLoad()
	if s.bulkConn == nil {
		t.Fatal("cold bulk writer did not engage")
	}

	var pageCount int64
	if err := s.bulkConn.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pageCount); err != nil {
		t.Fatalf("read page_count: %v", err)
	}
	setLimit := func(limit int64) {
		t.Helper()
		var applied int64
		if err := s.bulkConn.QueryRowContext(
			t.Context(), fmt.Sprintf("PRAGMA max_page_count = %d", limit),
		).Scan(&applied); err != nil {
			t.Fatalf("set max_page_count %d: %v", limit, err)
		}
		if applied != limit {
			t.Fatalf("max_page_count = %d, want %d", applied, limit)
		}
	}
	setLimit(pageCount + 8)

	callerID := "repo/a.go::caller"
	builtinID := "repo::builtin::go::make"
	large := &graph.Node{
		ID: callerID, Kind: graph.KindFunction, Name: "caller", FilePath: "repo/a.go",
		RepoPrefix: "repo", Meta: map[string]any{"payload": strings.Repeat("x", 256<<10)},
	}
	edge := &graph.Edge{From: callerID, To: builtinID, Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 3}
	err := s.AddBatchChecked([]*graph.Node{large}, []*graph.Edge{edge})
	if err == nil {
		t.Fatal("checked write unexpectedly succeeded under max_page_count")
	}
	var storageErr *StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("checked write error %T = %v, want *StorageError", err, err)
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != 13 {
		t.Fatalf("checked write cause = %v, want SQLITE_FULL (13)", err)
	}
	if got := SafeStorageFailureReason(err); got != "graph storage volume is full (SQLite code 13); free disk space and retry" {
		t.Fatalf("safe failure reason = %q", got)
	}
	if s.GetNode(callerID) != nil || s.GetNode(builtinID) != nil {
		t.Fatal("failed transaction left caller or builtin rows behind")
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		s.AddBatch([]*graph.Node{large}, []*graph.Edge{edge})
	}()
	legacyErr, ok := StorageErrorFromPanic(recovered)
	if !ok || !errors.As(legacyErr, &sqliteErr) || sqliteErr.Code()&0xff != 13 {
		t.Fatalf("legacy panic = %T %v, want typed SQLITE_FULL", recovered, recovered)
	}

	setLimit(1 << 30)
	small := *large
	small.Meta = nil
	if err := s.AddBatchChecked([]*graph.Node{&small}, []*graph.Edge{edge}); err != nil {
		t.Fatalf("retry after restoring capacity: %v", err)
	}
	if s.GetNode(builtinID) == nil {
		t.Fatal("failed transaction poisoned builtinSeen; retry omitted builtin stub")
	}
	if err := s.FlushBulk(); err != nil {
		t.Fatalf("FlushBulk: %v", err)
	}
}
