package store_sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// nameIndexNames exercise the folding and escaping edges of the SQL
// predicates: mixed case, non-ASCII letters (which LIKE compares exactly and
// strings.ToLower folds), LIKE metacharacters, and ids out of insertion order.
var nameIndexNames = []string{
	"matchesSkipRule", "matchesSkipRuleRen2", "xMatchesSkipRule", "MATCHESSKIPRULE",
	"Config", "defaultConfig", "reconfigure", "ÉCOLE", "école", "Straße", "STRASSE",
	"with_underscore", "with%percent", `back\slash`, "withXunderscore", "plain", "Ω", "ω",
}

func seedNameIndexGeneration(t *testing.T, publish bool) (*Store, *Store) {
	t.Helper()
	store := openCatalogStore(t)
	ctx := context.Background()
	id, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-names", LayerID: "layer-names",
		GenerationKind: "commit", TreeOID: "tree-names", CreatedAt: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*graph.Node
	for i := len(nameIndexNames) - 1; i >= 0; i-- {
		name := nameIndexNames[i]
		for copyN := range 3 {
			nodes = append(nodes, &graph.Node{
				ID: fmt.Sprintf("repo/f%02d.go::%s#%d", (i*7)%11, name, copyN), Name: name,
				Kind: graph.KindFunction, FilePath: fmt.Sprintf("repo/f%02d.go", (i*7)%11), RepoPrefix: "repo",
			})
		}
	}
	nodes = append(nodes, &graph.Node{ID: "repo/f00.go", Kind: graph.KindFile, FilePath: "repo/f00.go", RepoPrefix: "repo"})
	handle.AddBatch(nodes, nil)
	if publish {
		if err := store.PublishPayloadGeneration(ctx, id, 20); err != nil {
			t.Fatal(err)
		}
	}
	return store, store.AtGeneration(id)
}

// sqlFindContaining and sqlVisitFolded are the SQL scans exactly as they run
// without an index — the reference the indexed answers must equal.
func sqlFindContaining(t *testing.T, s *Store, substr string, limit int) []*graph.Node {
	t.Helper()
	pattern := "%" + escapeLikePattern(substr) + "%"
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE name LIKE ? ESCAPE '\' AND view_gen = ? ORDER BY id`
	var (
		out []*graph.Node
		err error
	)
	if limit > 0 {
		out, err = s.queryNodesSQLContext(context.Background(), q+` LIMIT ?`, pattern, s.viewGen, limit)
	} else {
		out, err = s.queryNodesSQLContext(context.Background(), q, pattern, s.viewGen)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func collectVisit(t *testing.T, visit func(func(*graph.Node) bool) error, stopAfter int) []*graph.Node {
	t.Helper()
	var out []*graph.Node
	if err := visit(func(n *graph.Node) bool {
		out = append(out, n)
		return stopAfter <= 0 || len(out) < stopAfter
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSealedNameIndexAnswersExactlyLikeTheSQLScan(t *testing.T) {
	_, handle := seedNameIndexGeneration(t, true)
	ctx := context.Background()
	if _, ok := handle.sealedNameIndex(ctx); !ok {
		t.Fatal("a published generation was not indexed")
	}
	substrs := []string{"matches", "MATCHES", "skipRule", "Ren2", "config", "CONFIG", "école", "ÉCOLE", "cole",
		"straße", "STRASSE", "ss", "_", "%", `\`, "x", "ω", "Ω", "absentName", "e"}
	for _, substr := range substrs {
		for _, limit := range []int{0, 1, 2, 5, 1000} {
			got, err := handle.FindNodesByNameContainingContext(ctx, substr, limit)
			if err != nil {
				t.Fatal(err)
			}
			if want := sqlFindContaining(t, handle, substr, limit); !reflect.DeepEqual(got, want) {
				t.Fatalf("FindNodesByNameContaining(%q, %d) differs\n got=%v\nwant=%v", substr, limit, nodeIDs(got), nodeIDs(want))
			}
		}
		for _, stop := range []int{0, 1, 3} {
			got := collectVisit(t, func(y func(*graph.Node) bool) error {
				return handle.VisitNodesByNameContainingFoldedContext(ctx, substr, y)
			}, stop)
			// The reference is the same visitor with the index withheld.
			want := collectVisit(t, func(y func(*graph.Node) bool) error {
				return handle.visitNodesByNameContainingFoldedSQL(ctx, substr, y)
			}, stop)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("VisitNodesByNameContainingFolded(%q, stop %d) differs\n got=%v\nwant=%v", substr, stop, nodeIDs(got), nodeIDs(want))
			}
		}
	}
}

func TestNameIndexIsOnlyBuiltForPublishedGenerations(t *testing.T) {
	store, building := seedNameIndexGeneration(t, false)
	if _, ok := building.sealedNameIndex(context.Background()); ok {
		t.Fatal("a building generation was indexed")
	}
	// A handle whose shared seal has not resolved yet asks the catalog.
	building.seal.state.Store(payloadSealUnknown)
	if _, ok := building.sealedNameIndex(context.Background()); ok {
		t.Fatal("a building generation with an unresolved seal was indexed")
	}
	_, published := seedNameIndexGeneration(t, true)
	published.seal.state.Store(payloadSealUnknown)
	if _, ok := published.sealedNameIndex(context.Background()); !ok {
		t.Fatal("a published generation with an unresolved seal was not indexed")
	}
	if _, ok := store.AtGeneration(0).sealedNameIndex(context.Background()); ok {
		t.Fatal("the mutable base corpus was indexed")
	}
}

func nodeIDs(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// TestLargeGenerationIsIndexedInTheBackground pins the no-regression rule for
// big generations: the first lookup is answered by the SQL scan while the
// index builds, and once it is ready lookups use it with the same answer.
func TestLargeGenerationIsIndexedInTheBackground(t *testing.T) {
	previous := generationNameIndexSyncRows
	generationNameIndexSyncRows = 0
	t.Cleanup(func() { generationNameIndexSyncRows = previous })
	_, handle := seedNameIndexGeneration(t, true)
	ctx := context.Background()
	if _, ok := handle.sealedNameIndex(ctx); ok {
		t.Fatal("a large generation's first lookup waited for the index")
	}
	want := sqlFindContaining(t, handle, "config", 0)
	got, err := handle.FindNodesByNameContainingContext(ctx, "config", 0)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("lookup while the index builds = %v, %v; want %v", nodeIDs(got), err, nodeIDs(want))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := handle.sealedNameIndex(ctx); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background index never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err = handle.FindNodesByNameContainingContext(ctx, "config", 0)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("indexed lookup = %v, %v; want %v", nodeIDs(got), err, nodeIDs(want))
	}
}
