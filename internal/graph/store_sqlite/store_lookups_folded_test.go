package store_sqlite

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestVisitNodesByNameContainingFoldedUnicodeLiteralAndCursorClose(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "folded.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	nodes := []*graph.Node{
		{ID: "a", Name: "plainKite", FilePath: "a.go", Kind: graph.NodeKind("function")},
		{ID: "b", Name: "Kite", FilePath: "b.go", Kind: graph.NodeKind("function")},
		{ID: "c", Name: "Δelta", FilePath: "c.go", Kind: graph.NodeKind("function")},
		{ID: "d", Name: `100%_\done`, FilePath: "d.go", Kind: graph.NodeKind("function")},
		{ID: "e", Name: "kiteAgain", FilePath: "e.go", Kind: graph.NodeKind("function")},
	}
	if err := store.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	ids := func(query string) []string {
		var out []string
		store.VisitNodesByNameContainingFolded(query, func(n *graph.Node) bool { out = append(out, n.ID); return true })
		sort.Strings(out)
		return out
	}
	if got, want := ids("k"), []string{"a", "b", "e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("k: got %v want %v", got, want)
	}
	if got, want := ids("δE"), []string{"c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Greek: got %v want %v", got, want)
	}
	if got, want := ids(`%_\`), []string{"d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("literal: got %v want %v", got, want)
	}
	if got := ids("absent"); len(got) != 0 {
		t.Fatalf("no result: %v", got)
	}
	called := false
	store.VisitNodesByNameContainingFolded("", func(*graph.Node) bool { called = true; return true })
	if called {
		t.Fatal("empty query visited row")
	}
	seen := 0
	store.VisitNodesByNameContainingFolded("kite", func(*graph.Node) bool { seen++; return false })
	if seen != 1 {
		t.Fatalf("early visits=%d", seen)
	}
	done := make(chan *graph.Node, 1)
	go func() { done <- store.GetNode("e") }()
	select {
	case node := <-done:
		if node == nil || node.ID != "e" {
			t.Fatalf("subsequent query=%v", node)
		}
	case <-time.After(time.Second):
		store.db.SetMaxOpenConns(2)
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("early stop retained sole read connection")
	}
}
