package graph

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
)

func TestScanNodeSearchKeysGraphIsPagedCompleteAndReentrant(t *testing.T) {
	g := New()
	want := map[string]NodeSearchKey{
		"c.go::Gamma": {ID: "c.go::Gamma", Kind: KindFunction, Name: "Gamma", FilePath: "c.go"},
		"a.go::Alpha": {ID: "a.go::Alpha", Kind: KindMethod, Name: "Alpha", FilePath: "a.go"},
		"b.go::Beta":  {ID: "b.go::Beta", Kind: KindFunction, Name: "Beta", FilePath: "b.go"},
	}
	for _, key := range want {
		g.AddNode(&Node{ID: key.ID, Kind: key.Kind, Name: key.Name, FilePath: IDFile(key.ID)})
	}

	got := make(map[string]NodeSearchKey)
	var pageSizes []int
	err := ScanNodeSearchKeys(context.Background(), g, 2, func(page []NodeSearchKey) bool {
		pageSizes = append(pageSizes, len(page))
		if len(page) == 0 || len(page) > 2 {
			t.Fatalf("page size = %d, want 1..2", len(page))
		}
		// Graph releases its shard lock before callbacks, so point lookup is
		// safe even when a page lands in the middle of a shard.
		if g.GetNode(page[0].ID) == nil {
			t.Fatalf("callback could not re-enter Graph for %q", page[0].ID)
		}
		for _, key := range page {
			if _, duplicate := got[key.ID]; duplicate {
				t.Fatalf("duplicate key %q", key.ID)
			}
			got[key.ID] = key
		}
		return true
	})
	if err != nil {
		t.Fatalf("ScanNodeSearchKeys: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %#v, want %#v", got, want)
	}
	sort.Ints(pageSizes)
	if !reflect.DeepEqual(pageSizes, []int{1, 2}) {
		t.Fatalf("page sizes = %v, want [1 2]", pageSizes)
	}
}

func TestScanNodeSearchKeysHonorsCancellationAndEarlyStop(t *testing.T) {
	g := New()
	for _, id := range []string{"a.go::A", "b.go::B", "c.go::C"} {
		g.AddNode(&Node{ID: id, Kind: KindFunction, Name: id})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ScanNodeSearchKeys(ctx, g, 1, func([]NodeSearchKey) bool {
		t.Fatal("callback ran after cancellation")
		return true
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	calls := 0
	if err := ScanNodeSearchKeys(context.Background(), g, 1, func([]NodeSearchKey) bool {
		calls++
		return false
	}); err != nil {
		t.Fatalf("early-stop scan: %v", err)
	}
	if calls != 1 {
		t.Fatalf("callbacks = %d, want 1", calls)
	}
}

func TestOverlaidViewScanNodeSearchKeysAppliesMasksAndProxiesRevision(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go"})
	base.AddNode(&Node{ID: "replace.go::Old", Kind: KindMethod, Name: "Old", FilePath: "replace.go"})
	base.AddNode(&Node{ID: "module::Removed", Kind: KindModule, Name: "Removed"})

	layer := NewOverlayLayer()
	layer.MarkFile("replace.go", false)
	layer.AddNode("replace.go", &Node{ID: "replace.go::New", Kind: KindMethod, Name: "New", FilePath: "replace.go"})
	layer.MarkRemoved("Removed", "module::Removed")
	view := NewOverlaidView(base, layer)

	got := make(map[string]NodeSearchKey)
	if err := view.ScanNodeSearchKeys(context.Background(), 1, func(page []NodeSearchKey) bool {
		if len(page) != 1 {
			t.Fatalf("page size = %d, want 1", len(page))
		}
		if view.GetNode(page[0].ID) == nil {
			t.Fatalf("callback could not re-enter overlay for %q", page[0].ID)
		}
		got[page[0].ID] = page[0]
		return true
	}); err != nil {
		t.Fatalf("ScanNodeSearchKeys: %v", err)
	}
	want := map[string]NodeSearchKey{
		"keep.go::Keep":   {ID: "keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go"},
		"replace.go::New": {ID: "replace.go::New", Kind: KindMethod, Name: "New", FilePath: "replace.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay keys = %#v, want %#v", got, want)
	}

	before, known := view.SearchMutationRevision()
	if !known {
		t.Fatal("overlay revision should be known for a Graph base")
	}
	base.AddNode(&Node{ID: "later.go::Later", Kind: KindFunction, Name: "Later", FilePath: "later.go"})
	if after, known := view.SearchMutationRevision(); !known || after <= before {
		t.Fatalf("overlay revision did not follow base: before=%d after=%d known=%v", before, after, known)
	}
}
