package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openNodeSearchKeyTestStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "graph.db"))
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})
	return store
}

func TestScanNodeSearchKeysUsesBoundedOrderedPagesAndClosesRows(t *testing.T) {
	store := openNodeSearchKeyTestStore(t)
	for _, node := range []*graph.Node{
		{ID: "c.go::Gamma", Kind: graph.KindFunction, Name: "Gamma", FilePath: "c.go"},
		{ID: "a.go::Alpha", Kind: graph.KindMethod, Name: "Alpha", FilePath: "a.go"},
		{ID: "b.go::Beta", Kind: graph.KindFunction, Name: "Beta", FilePath: "b.go"},
	} {
		store.AddNode(node)
	}

	// A single reader connection makes an accidentally open page cursor
	// observable in the callback: InUse would remain one and a point lookup
	// would have no connection available.
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	var got []graph.NodeSearchKey
	var pageSizes []int
	insertedAfterHighWater := false
	err := store.ScanNodeSearchKeys(context.Background(), 1, func(page []graph.NodeSearchKey) bool {
		pageSizes = append(pageSizes, len(page))
		if inUse := store.db.Stats().InUse; inUse != 0 {
			t.Fatalf("reader connections in use during callback = %d, want 0", inUse)
		}
		if store.GetNode(page[0].ID) == nil {
			t.Fatalf("callback could not re-enter Store for %q", page[0].ID)
		}
		got = append(got, page...)
		if !insertedAfterHighWater {
			insertedAfterHighWater = true
			store.AddNode(&graph.Node{ID: "z.go::Later", Kind: graph.KindFunction, Name: "Later", FilePath: "z.go"})
		}
		return true
	})
	if err != nil {
		t.Fatalf("ScanNodeSearchKeys: %v", err)
	}
	want := []graph.NodeSearchKey{
		{ID: "a.go::Alpha", Kind: graph.KindMethod, Name: "Alpha", FilePath: "a.go"},
		{ID: "b.go::Beta", Kind: graph.KindFunction, Name: "Beta", FilePath: "b.go"},
		{ID: "c.go::Gamma", Kind: graph.KindFunction, Name: "Gamma", FilePath: "c.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(pageSizes, []int{1, 1, 1}) {
		t.Fatalf("page sizes = %v, want [1 1 1]", pageSizes)
	}
}

func TestScanNodeSearchKeysReturnsCancellationBetweenPages(t *testing.T) {
	store := openNodeSearchKeyTestStore(t)
	for _, id := range []string{"a.go::A", "b.go::B", "c.go::C"} {
		store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: graph.IDFile(id)})
	}

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := store.ScanNodeSearchKeys(ctx, 1, func([]graph.NodeSearchKey) bool {
		calls++
		cancel()
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("callbacks = %d, want 1", calls)
	}
}

var nodeSearchKeyBenchmarkCount int

func BenchmarkScanNodeSearchKeysVsAllNodes(b *testing.B) {
	store := openNodeSearchKeyTestStore(b)
	const nodeCount = 2048
	payload := strings.Repeat("nontrivial metadata payload ", 64)
	nodes := make([]*graph.Node, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		file := fmt.Sprintf("pkg/file_%05d.go", i)
		nodes = append(nodes, &graph.Node{
			ID:       fmt.Sprintf("%s::Symbol_%05d", file, i),
			Kind:     graph.KindFunction,
			Name:     fmt.Sprintf("Symbol_%05d", i),
			FilePath: file,
			Language: "go",
			Meta: map[string]any{
				"payload": payload,
				"ordinal": i,
				"active":  i%2 == 0,
			},
		})
	}
	store.AddBatch(nodes, nil)

	b.Run("AllNodes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			nodeSearchKeyBenchmarkCount = len(store.AllNodes())
		}
	})

	b.Run("ScanNodeSearchKeys", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			err := store.ScanNodeSearchKeys(context.Background(), graph.DefaultNodeSearchKeyPageSize, func(page []graph.NodeSearchKey) bool {
				count += len(page)
				return true
			})
			if err != nil {
				b.Fatalf("ScanNodeSearchKeys: %v", err)
			}
			nodeSearchKeyBenchmarkCount = count
		}
	})
}
