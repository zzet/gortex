package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func captureStorageEmissionPanic(fn func()) (value any) {
	defer func() { value = recover() }()
	fn()
	return nil
}

// This exercises an actual catalog-allocated handle, public retirement, and the
// legacy AddBatch panic wrapper. It does not assert that IndexCtx or the whole
// physical builder invokes the classifier for every escaping storage failure.
func TestRetiredAddBatchStorageClassification(t *testing.T) {
	f := newSealedEmissionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.owner.AddBatch([]*graph.Node{{ID: "other/keep.go::Keep", Kind: graph.KindFunction, Name: "Keep", FilePath: "other/keep.go", RepoPrefix: "other"}}, nil)
	f.handle.AddBatch([]*graph.Node{{ID: "repo/a.go::Unread", Kind: graph.KindFunction, Name: "Unread", FilePath: "repo/a.go", RepoPrefix: "repo"}}, nil)
	if f.owner.AtGeneration(f.id).GetNode("repo/a.go::Unread") == nil {
		t.Fatal("allocated generation did not contain the fixture payload")
	}
	if err := f.owner.RetirePayloadGeneration(ctx, f.id, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.owner.Catalog().GetViewGeneration(ctx, f.id); err != nil || found {
		t.Fatalf("public retirement did not remove generation: found=%v err=%v", found, err)
	}
	value := captureStorageEmissionPanic(func() {
		f.handle.AddBatch([]*graph.Node{{ID: "repo/a.go::TooLate", Kind: graph.KindFunction, Name: "TooLate", FilePath: "repo/a.go", RepoPrefix: "repo"}}, nil)
	})
	if value == nil {
		t.Fatal("retained-handle AddBatch did not expose its sealed-write refusal")
	}
	err, recognized := StorageErrorFromPanic(value)
	t.Logf("generation=%d panic_type=%T recognized=%v classified_type=%T error=%v", f.id, value, recognized, err, err)
	panicErr, isError := value.(error)
	var wrappedStorage *StorageError
	t.Logf("panic_is_error=%v panic_wraps_sealed=%v panic_wraps_storage_error=%v", isError, errors.Is(panicErr, ErrPayloadGenerationSealed), errors.As(panicErr, &wrappedStorage))
	for current, depth := panicErr, 0; current != nil && depth < 8; current, depth = errors.Unwrap(current), depth+1 {
		t.Logf("panic_chain[%d]=%T", depth, current)
	}
	if !recognized || err == nil {
		t.Errorf("actual sealed AddBatch panic is not classified: type=%T value=%v", value, value)
	}
	if !isError || !errors.Is(panicErr, ErrPayloadGenerationSealed) {
		t.Errorf("actual panic lost sealed-generation cause: type=%T value=%v", value, value)
	}
	if recognized && !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("classification lost sealed-generation cause: %v", err)
	}
	var storageErr *StorageError
	if recognized && (!errors.As(err, &storageErr) || storageErr == nil) {
		t.Fatalf("classified result is not a typed StorageError: %T %v", err, err)
	}
	if f.owner.AtGeneration(f.id).GetNode("repo/a.go::TooLate") != nil {
		t.Error("retained handle resurrected retired payload")
	}
	if f.owner.GetNode("other/keep.go::Keep") == nil {
		t.Error("unrelated generation-zero payload was lost")
	}
	f.owner.AddBatch([]*graph.Node{{ID: "other/keep.go::After", Kind: graph.KindFunction, Name: "After", FilePath: "other/keep.go", RepoPrefix: "other"}}, nil)
	if f.owner.GetNode("other/keep.go::After") == nil {
		t.Error("generation zero could not accept a later healthy write")
	}
}

func TestStorageClassifierPreservesUnrelatedPanics(t *testing.T) {
	runtimePanic := captureStorageEmissionPanic(func() {
		values := []int{}
		_ = values[0]
	})
	if runtimePanic == nil {
		t.Fatal("runtime control did not panic")
	}
	var nilStorage *StorageError
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "nil", value: nil},
		{name: "typed-nil-storage", value: nilStorage},
		{name: "programmer-error", value: errors.New("private unrelated programmer failure")},
		{name: "raw-lifecycle-error", value: ErrPayloadGenerationSealed},
		{name: "wrapped-lifecycle-error", value: fmt.Errorf("not a store wrapper: %w", ErrPayloadGenerationSealed)},
		{name: "lookalike-string", value: "store_sqlite: payload generation is published and no longer writable"},
		{name: "runtime-bounds", value: runtimePanic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value
			if value != nil {
				value = captureStorageEmissionPanic(func() { panic(tc.value) })
			}
			if err, recognized := StorageErrorFromPanic(value); recognized || err != nil {
				t.Fatalf("unrelated panic classified: type=%T recognized=%v err=%v", value, recognized, err)
			}
		})
	}
}

func TestPanicOnFatalSealedEmission(t *testing.T) {
	typed := wrapStorageError(fmt.Errorf("typed operation: %w", ErrPayloadGenerationSealed))
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "direct", err: ErrPayloadGenerationSealed},
		{name: "wrapped", err: fmt.Errorf("operation: %w", ErrPayloadGenerationSealed)},
		{name: "already-typed", err: typed},
		{name: "wrapped-typed", err: fmt.Errorf("outer operation: %w", typed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := captureStorageEmissionPanic(func() { panicOnFatal(tc.err) })
			err, recognized := StorageErrorFromPanic(value)
			if !recognized || err == nil {
				t.Fatalf("sealed store emission is not directly typed: panic=%T %v", value, value)
			}
			if !errors.Is(err, ErrPayloadGenerationSealed) || !errors.Is(err, tc.err) {
				t.Errorf("normalization did not preserve full cause: %v", err)
			}
			if want := "store_sqlite: " + tc.err.Error(); err.Error() != want {
				t.Errorf("legacy diagnostic changed: got %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestPanicOnFatalNonSealedBehaviorUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "no-rows", err: sql.ErrNoRows},
		{name: "wrapped-no-rows", err: fmt.Errorf("read: %w", sql.ErrNoRows)},
		{name: "no-rows-precedes-sealed", err: errors.Join(sql.ErrNoRows, ErrPayloadGenerationSealed)},
		{name: "connection-done", err: sql.ErrConnDone},
		{name: "wrapped-connection-done", err: fmt.Errorf("write: %w", sql.ErrConnDone)},
		{name: "connection-done-precedes-sealed", err: errors.Join(sql.ErrConnDone, ErrPayloadGenerationSealed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if value := captureStorageEmissionPanic(func() { panicOnFatal(tc.err) }); value != nil {
				t.Fatalf("existing non-fatal condition now panics: %T %v", value, value)
			}
		})
	}
	other := errors.New("private non-sealed store error")
	value := captureStorageEmissionPanic(func() { panicOnFatal(other) })
	err, ok := value.(error)
	if !ok || !errors.Is(err, other) {
		t.Fatalf("other legacy panic changed/lost cause: %T %v", value, value)
	}
	if want := "store_sqlite: " + other.Error(); err.Error() != want {
		t.Errorf("other legacy diagnostic changed: got %q, want %q", err.Error(), want)
	}
	if classified, recognized := StorageErrorFromPanic(value); recognized || classified != nil {
		t.Errorf("sealed-only normalization broadened another legacy panic: %T %v", value, value)
	}
}

func TestStorageClassifierDirectTypedControl(t *testing.T) {
	cause := errors.New("private typed storage control")
	typed := wrapStorageError(cause)
	value := captureStorageEmissionPanic(func() { panic(typed) })
	err, recognized := StorageErrorFromPanic(value)
	if !recognized || err != typed || !errors.Is(err, cause) {
		t.Fatalf("existing direct typed storage classification changed: recognized=%v err=%v", recognized, err)
	}
}

type sealedEmissionFixture struct {
	owner  *Store
	handle *Store
	id     int64
}

func newSealedEmissionFixture(t *testing.T) sealedEmissionFixture {
	t.Helper()
	root := t.TempDir()
	s, err := openPristine(t, filepath.Join(root, "sealed-emission.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := s.Catalog()
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, "git"), State: "active"}
	if err := c.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	owner := Checkout{CheckoutID: "owner", Incarnation: "incarnation", FamilyID: family.FamilyID, RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated, HeadTree: "tree"}
	if err := c.UpsertCheckout(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertDedicatedGraph(ctx, DedicatedGraph{GraphID: "graph", OwnerCheckoutID: owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	id, handle, err := s.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner", GenerationKind: "commit", LayerID: "layer", TreeOID: "tree"})
	if err != nil {
		t.Fatal(err)
	}
	return sealedEmissionFixture{owner: s, handle: handle, id: id}
}

func BenchmarkPanicOnFatalSealedPayloadBoundary(b *testing.B) {
	for _, tc := range []struct {
		name      string
		err       error
		wantPanic bool
	}{
		{name: "nil"},
		{name: "nonfatal", err: sql.ErrNoRows},
		{name: "sealed", err: ErrPayloadGenerationSealed, wantPanic: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				value := captureStorageEmissionPanic(func() { panicOnFatal(tc.err) })
				if (value != nil) != tc.wantPanic {
					b.Fatalf("unexpected panic result: %T %v", value, value)
				}
			}
		})
	}
}
