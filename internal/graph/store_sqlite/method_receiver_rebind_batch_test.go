package store_sqlite

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestRebindGoMethodReceiversForFilesScopesAndDedupes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "receiver-batch.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	canonical := &graph.Node{ID: "pkg/type.go::Thing", Kind: graph.KindType, Name: "Thing", FilePath: "pkg/type.go", Language: "go", RepoPrefix: "repo"}
	methodA := &graph.Node{ID: "pkg/a.go::Thing.A", Kind: graph.KindMethod, Name: "A", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"}
	methodB := &graph.Node{ID: "pkg/b.go::Thing.B", Kind: graph.KindMethod, Name: "B", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"}
	untouched := &graph.Node{ID: "pkg/c.go::Thing.C", Kind: graph.KindMethod, Name: "C", FilePath: "pkg/c.go", Language: "go", RepoPrefix: "repo"}
	store.AddBatch([]*graph.Node{canonical, methodA, methodB, untouched}, []*graph.Edge{
		{From: methodA.ID, To: "pkg/a.go::Thing", Kind: graph.EdgeMemberOf, FilePath: "pkg/a.go", Line: 2},
		{From: methodB.ID, To: "pkg/b.go::Thing", Kind: graph.EdgeMemberOf, FilePath: "pkg/b.go", Line: 2},
		{From: untouched.ID, To: "pkg/c.go::Thing", Kind: graph.EdgeMemberOf, FilePath: "pkg/c.go", Line: 2},
	})

	store.db.SetMaxOpenConns(1)
	heldReader, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer heldReader.Close() //nolint:errcheck // explicit close below; cleanup safety
	type result struct {
		changed int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changed, rebindErr := store.RebindGoMethodReceiversForFiles([]string{"pkg/a.go", "pkg/b.go", "pkg/a.go", ""})
		done <- result{changed: changed, err: rebindErr}
	}()
	var changed int
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		changed = got.changed
	case <-time.After(2 * time.Second):
		t.Fatal("batch receiver rebind waited for saturated reader pool")
	}
	if err := heldReader.Close(); err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}
	for _, methodID := range []string{methodA.ID, methodB.ID} {
		out := store.GetOutEdges(methodID)
		if len(out) != 1 || out[0].To != canonical.ID {
			t.Fatalf("%s edges = %#v, want canonical receiver %s", methodID, out, canonical.ID)
		}
	}
	out := store.GetOutEdges(untouched.ID)
	if len(out) != 1 || out[0].To != "pkg/c.go::Thing" {
		t.Fatalf("untouched file changed: %#v", out)
	}
}

// The batched rebind writes exactly the rows the per-file rebind writes,
// including the two cleanup cases its DELETEs exist for: a rebound key that
// already exists as another edge, and two candidates rebinding to one key.
func TestRebindGoMethodReceiversForFilesMatchesPerFileRows(t *testing.T) {
	build := func(name string) *Store {
		store, err := Open(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		canonical := &graph.Node{ID: "pkg/type.go::Thing", Kind: graph.KindType, Name: "Thing", FilePath: "pkg/type.go", Language: "go", RepoPrefix: "repo"}
		nodes := []*graph.Node{canonical}
		var edges []*graph.Edge
		for _, file := range []string{"pkg/a.go", "pkg/b.go", "pkg/c.go"} {
			method := &graph.Node{ID: file + "::Thing.M", Kind: graph.KindMethod, Name: "M", FilePath: file, Language: "go", RepoPrefix: "repo"}
			nodes = append(nodes, method)
			edges = append(edges,
				&graph.Edge{From: method.ID, To: file + "::Thing", Kind: graph.EdgeMemberOf, FilePath: file, Line: 2},
				// Duplicate: a second phantom receiver at the same site.
				&graph.Edge{From: method.ID, To: "pkg/other.go::Thing", Kind: graph.EdgeMemberOf, FilePath: file, Line: 2},
			)
		}
		// Conflict: a.go already carries the canonical key.
		edges = append(edges, &graph.Edge{From: "pkg/a.go::Thing.M", To: canonical.ID, Kind: graph.EdgeMemberOf, FilePath: "pkg/a.go", Line: 2})
		store.AddBatch(nodes, edges)
		return store
	}
	rows := func(s *Store) []string {
		var out []string
		for _, file := range []string{"pkg/a.go", "pkg/b.go", "pkg/c.go"} {
			for _, e := range s.GetOutEdges(file + "::Thing.M") {
				out = append(out, e.From+" -> "+e.To+" "+string(e.Kind)+" "+e.FilePath)
			}
		}
		sort.Strings(out)
		return out
	}
	batched := build("batched.sqlite")
	if _, err := batched.RebindGoMethodReceiversForFiles([]string{"pkg/a.go", "pkg/b.go"}); err != nil {
		t.Fatal(err)
	}
	perFile := build("per-file.sqlite")
	for _, file := range []string{"pkg/a.go", "pkg/b.go"} {
		if _, err := perFile.RebindGoMethodReceivers(file); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := rows(batched), rows(perFile); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("batched rebind rows differ from per-file rows:\nbatched:\n%s\nper-file:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, file := range []string{"pkg/a.go", "pkg/b.go"} {
		out := batched.GetOutEdges(file + "::Thing.M")
		if len(out) != 1 || out[0].To != "pkg/type.go::Thing" {
			t.Fatalf("%s: want exactly one member_of to the canonical type, got %d rows %v", file, len(out), rows(batched))
		}
	}
	for _, row := range rows(batched) {
		if strings.HasPrefix(row, "pkg/c.go") && !strings.Contains(row, "pkg/c.go::Thing") && !strings.Contains(row, "pkg/other.go::Thing") {
			t.Fatalf("a file outside the frontier was rebound: %s", row)
		}
	}
}
