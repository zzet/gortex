package indexer

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestContractInventorySuccessorReusesOnlyDetachedCurrentValues(t *testing.T) {
	s := sparseShapeStore(t)
	h := s.AtGeneration(10)
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "accepted", Size: 12}
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	first := inventoryProjectionFor(h, h)
	_, err := first.read(t.Context(), "r")
	require.NoError(t, err)
	values := first.detachedValues()
	require.NotNil(t, values)
	require.Equal(t, 1, values.entries["r"].physicalReads)
	current := inventoryProjectionFor(h, h)
	current.seed(values)
	got, err := current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Zero(t, current.entries["r"].physicalReads, "a fresh successor must reuse validated owned rows without borrowing the predecessor's reader")
	require.Zero(t, current.detachedValues().entries["r"].physicalReads)
	delete(got, row.FilePath)
	got, err = current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath], "returned maps cannot mutate retained values")
	row.ContentHash = "changed"
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	changed := inventoryProjectionFor(h, h)
	changed.seed(values)
	got, err = changed.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Equal(t, 1, changed.entries["r"].physicalReads, "a changed revision must reread actual selected rows")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = current.read(cancelled, "r")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
}

func TestContractInventorySuccessorKeepsForeignAndOpaqueReads(t *testing.T) {
	s := sparseShapeStore(t)
	h := s.AtGeneration(10)
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "original", Size: 1}
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	first := inventoryProjectionFor(h, h)
	_, err := first.read(t.Context(), "r")
	require.NoError(t, err)
	values := first.detachedValues()
	foreign := sparseShapeStore(t).AtGeneration(10)
	row.ContentHash = "foreign"
	require.NoError(t, foreign.SetFileMetas("r", []graph.FileMetaRow{row}))
	current := inventoryProjectionFor(foreign, foreign)
	current.seed(values)
	got, err := current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Equal(t, 1, current.entries["r"].physicalReads)
	opaque := &inventoryOpaqueReader{Reader: h, hash: "opaque-current"}
	current = inventoryProjectionFor(opaque, h)
	current.seed(values)
	got, err = current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, "opaque-current", got[row.FilePath].ContentHash)
	require.Equal(t, 1, opaque.calls)
}

func TestContractInventorySuccessorKeepsOverlayPrecedenceAndMaskChanges(t *testing.T) {
	s := sparseShapeStore(t)
	a, b := s.AtGeneration(10), s.AtGeneration(11)
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "a", Size: 1}
	require.NoError(t, a.SetFileMetas("r", []graph.FileMetaRow{row}))
	row.ContentHash = "b"
	require.NoError(t, b.SetFileMetas("r", []graph.FileMetaRow{row}))
	for _, handle := range []*store_sqlite.Store{a, b} {
		require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "r", FilePath: row.FilePath, Mode: store_sqlite.OwnershipReplace}}))
	}
	makeProjection := func(base, overlay graph.Reader) *contractInventoryProjection {
		handle := a
		if overlay == b {
			handle = b
		}
		layer, err := graphview.NewGenerationLayerContext(t.Context(), handle)
		require.NoError(t, err)
		return inventoryProjectionFor(graph.NewOverlaidViewWithLayer(base, layer), a, b)
	}
	first := makeProjection(a, b)
	got, err := first.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, "b", got[row.FilePath].ContentHash)
	values := first.detachedValues()
	current := makeProjection(b, a)
	current.seed(values)
	got, err = current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, "a", got[row.FilePath].ContentHash, "the same handles do not prove the same overlay precedence")
	require.Equal(t, 1, current.entries["r"].physicalReads)
	require.NoError(t, b.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "r", FilePath: row.FilePath, Mode: store_sqlite.OwnershipDelete}}))
	current = makeProjection(a, b)
	current.seed(values)
	got, err = current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Empty(t, got)
	require.Equal(t, 1, current.entries["r"].physicalReads)
}

func TestContractInventorySuccessorDoesNotHideStaleMaterializedMasks(t *testing.T) {
	s := sparseShapeStore(t)
	base, overlay := s.AtGeneration(10), s.AtGeneration(11)
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "base", Size: 1}
	require.NoError(t, base.SetFileMetas("r", []graph.FileMetaRow{row}))
	staleLayer, err := graphview.NewGenerationLayerContext(t.Context(), overlay)
	require.NoError(t, err)
	require.NoError(t, overlay.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "r", FilePath: row.FilePath, Mode: store_sqlite.OwnershipDelete}}))
	freshLayer, err := graphview.NewGenerationLayerContext(t.Context(), overlay)
	require.NoError(t, err)
	current := inventoryProjectionFor(graph.NewOverlaidViewWithLayer(base, freshLayer), base, overlay)
	got, err := current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Empty(t, got)
	values := current.detachedValues()
	stale := inventoryProjectionFor(graph.NewOverlaidViewWithLayer(base, staleLayer), base, overlay)
	stale.seed(values)
	got, err = stale.read(t.Context(), "r")
	require.Error(t, err, "current physical handles do not validate old materialized layer masks")
	require.Nil(t, got)
}

func TestContractInventorySuccessorKeepsOwnerCloseRefusal(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "borrowed"
		if owner {
			name = "owner"
		}
		t.Run(name, func(t *testing.T) {
			s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "close.sqlite"))
			require.NoError(t, err)
			closed := false
			t.Cleanup(func() {
				if !closed {
					require.NoError(t, s.Close())
				}
			})
			h := s.AtGeneration(10)
			row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "accepted", Size: 1}
			require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
			first := inventoryProjectionFor(h, h)
			_, err = first.read(t.Context(), "r")
			require.NoError(t, err)
			values := first.detachedValues()
			if owner {
				require.NoError(t, s.Close())
				closed = true
			} else {
				require.NoError(t, h.Close())
			}
			current := inventoryProjectionFor(h, h)
			current.seed(values)
			got, err := current.read(t.Context(), "r")
			if owner {
				require.Error(t, err, "detached rows must not hide an owning store's teardown")
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, row, got[row.FilePath])
				require.Zero(t, current.entries["r"].physicalReads, "closing a borrowed handle does not close the input owner")
			}
		})
	}
}

func TestContractInventoryDetachedValuesRetainNoResources(t *testing.T) {
	var check func(reflect.Type)
	seen := map[reflect.Type]bool{}
	check = func(typ reflect.Type) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		require.NotEqual(t, "storeCore", typ.Name(), "a detached revision must not retain a database owner")
		switch typ.Kind() {
		case reflect.Func, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
			t.Fatalf("retained capture values contain resource-capable type %s", typ)
		case reflect.Pointer, reflect.Slice, reflect.Array:
			check(typ.Elem())
		case reflect.Map:
			check(typ.Key())
			check(typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				check(typ.Field(i).Type)
			}
		}
	}
	check(reflect.TypeFor[contractInventoryValues]())
}

func TestContractInventoryPrimaryCaptureKeepsFreshCensusWithoutRetention(t *testing.T) {
	initial := "package fixture\nfunc Initial() {}\n"
	changed := "package fixture\nfunc Changed() {}\n"
	added := "package fixture\nfunc Added() {}\n"
	f := newContractJobFixture(t, map[string]string{"f.go": initial})
	capture := func(expected map[string]string) ContractFollowupSnapshot {
		t.Helper()
		inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
		require.NoError(t, err)
		snapshot, targets, err := NewContractFollowupSourceCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
		require.NoError(t, err)
		t.Cleanup(snapshot.Release)
		require.Len(t, targets, 1)
		require.Len(t, snapshot.Files, len(expected))
		for _, file := range snapshot.Files {
			text, found := expected[file.Path]
			require.True(t, found, "primary capture must preserve the current census")
			accepted, err := snapshot.ReadAccepted(t.Context(), file)
			require.NoError(t, err)
			require.Equal(t, text, string(accepted.Bytes))
			require.Equal(t, file.SourceFingerprint, accepted.SourceFingerprint)
			require.Equal(t, file.Policy, accepted.Policy)
		}
		if snapshot.inventoryValues != nil {
			t.Error("a nil-source primary capture has no successor inventory consumer")
		}
		return snapshot
	}
	first := capture(map[string]string{"fixture/f.go": initial})
	writeFile(t, filepath.Join(f.root, "f.go"), changed)
	_, err := first.ReadAccepted(t.Context(), first.Files[0])
	require.ErrorIs(t, err, graph.ErrContractProjectionStale, "no retained inventory must not weaken dirty-source refusal")
	first.Release()
	f.save("f.go", changed)
	f.save("added.go", added)
	current := capture(map[string]string{"fixture/f.go": changed, "fixture/added.go": added})
	current.Release()
}
