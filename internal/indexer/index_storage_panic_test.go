package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// panicBeginBulkStore injects a legacy store panic from indexCtxRaw's deferred
// shadow drain. That is the same synchronous boundary at which a durable
// backend used to let SQLITE_FULL escape a cold-index worker goroutine.
type panicBeginBulkStore struct {
	*store_sqlite.Store
	panicValue any
}

func (s *panicBeginBulkStore) BeginBulkLoad() {
	panic(s.panicValue)
}

func (*panicBeginBulkStore) FlushBulk() error { return nil }

func indexCtxRawStorageError(t testing.TB) (*store_sqlite.Store, *store_sqlite.StorageError) {
	t.Helper()
	store := builderOpenStore(t, "index-ctx-raw-storage-error")
	err := store.AddBatchChecked([]*graph.Node{{
		ID:   "invalid-meta",
		Kind: graph.KindFunction,
		Meta: map[string]any{"unsupported": make(chan int)},
	}}, nil)
	if err == nil {
		t.Fatal("unsupported metadata unexpectedly produced no storage error")
	}
	var storageErr *store_sqlite.StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("storage error = %T %v, want *StorageError", err, err)
	}
	return store, storageErr
}

func TestIndexCtxRawConvertsStoragePanicToError(t *testing.T) {
	sqliteStore, storageErr := indexCtxRawStorageError(t)
	store := &panicBeginBulkStore{Store: sqliteStore, panicValue: storageErr}
	idx := newTestIndexer(store)

	result, err := idx.indexCtxRaw(context.Background(), t.TempDir())
	if result != nil {
		t.Fatalf("result = %+v, want nil after storage panic", result)
	}
	var typed *store_sqlite.StorageError
	if !errors.As(err, &typed) {
		t.Fatalf("indexCtxRaw error = %T %v, want *StorageError", err, err)
	}
	if typed != storageErr {
		t.Fatalf("returned StorageError = %p, want original %p", typed, storageErr)
	}
	if idx.graph != store {
		t.Fatalf("indexCtxRaw retained shadow graph %T after storage panic; want original %T", idx.graph, store)
	}
	if idx.contentSink != nil || idx.contractStateSink != nil {
		t.Fatalf("indexCtxRaw retained shadow sinks after storage panic: content=%T contract=%T",
			idx.contentSink, idx.contractStateSink)
	}
}

func TestIndexCtxRawRepanicsArbitraryPanic(t *testing.T) {
	wantPanic := &struct{ label string }{label: "programmer panic"}
	store := &panicBeginBulkStore{
		Store:      builderOpenStore(t, "index-ctx-raw-arbitrary-panic"),
		panicValue: wantPanic,
	}
	idx := newTestIndexer(store)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = idx.indexCtxRaw(context.Background(), t.TempDir())
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, wantPanic)
	}
	if idx.graph != store {
		t.Fatalf("indexCtxRaw retained shadow graph %T after arbitrary panic; want original %T", idx.graph, store)
	}
	if idx.contentSink != nil || idx.contractStateSink != nil {
		t.Fatalf("indexCtxRaw retained shadow sinks after arbitrary panic: content=%T contract=%T",
			idx.contentSink, idx.contractStateSink)
	}
}

func BenchmarkIndexCtxRawStoragePanicBoundarySuccess(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		var result *IndexResult
		var err error
		func() {
			defer recoverIndexCtxRawStoragePanic(&result, &err)
		}()
		if result != nil || err != nil {
			b.Fatalf("success boundary mutated result=%v err=%v", result, err)
		}
	}
}
