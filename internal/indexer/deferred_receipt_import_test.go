package indexer

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// receiptImportStore is the store surface these tests need: the shared
// query/mutation interface plus the receipt window and file eviction.
type receiptImportStore interface {
	graph.Store
	graph.MutationReceiptStore
}

func openReceiptImportStores(t *testing.T) map[string]receiptImportStore {
	t.Helper()
	sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "receipt-import.sqlite"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlite.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	return map[string]receiptImportStore{
		"graph":  graph.New(),
		"sqlite": sqlite,
	}
}

// seedAmbiguousImportPackages adds n same-qualified-name package candidates
// (in repos one, two, three, ...) and one pending import stub parked under
// stubTarget. The caller file node, when callerRepo is non-empty, anchors the
// edge to that repository so repo-prefixed stub forms resolve against it.
func seedAmbiguousImportPackages(store receiptImportStore, n int, callerRepo, stubTarget string) {
	repos := []string{"one", "two", "three", "four"}
	nodes := make([]*graph.Node, 0, n+1)
	for i := 0; i < n; i++ {
		repo := repos[i]
		nodes = append(nodes, &graph.Node{
			ID: repo + "::pkg", Kind: graph.KindPackage, Name: "pkg",
			QualName: "example/pkg", FilePath: "src/" + repo + ".go", RepoPrefix: repo,
		})
	}
	from := "src/c.go"
	if callerRepo != "" {
		from = callerRepo + "/src/c.go"
		nodes = append(nodes, &graph.Node{
			ID: from, Kind: graph.KindFile, Name: "c.go",
			FilePath: "src/c.go", RepoPrefix: callerRepo,
		})
	}
	store.AddBatch(nodes, []*graph.Edge{{
		From: from, To: stubTarget, Kind: graph.EdgeImports, FilePath: "src/c.go",
	}})
}

func importEdgeTarget(t *testing.T, store graph.Store, candidates ...string) string {
	t.Helper()
	for _, id := range candidates {
		for _, e := range store.GetInEdgesByNodeIDs([]string{id})[id] {
			if e != nil && e.Kind == graph.EdgeImports {
				return id
			}
		}
	}
	return ""
}

// TestMutationReceiptEvictAmbiguousImportPackageCandidateBindsSurvivor is
// the production exact-receipt consumption path over a real eviction: two
// foreign package candidates share a qualified name, so the import stays
// correctly unresolved; evicting one must mark the receipt
// resolution-relevant under the import-prefixed stub name so the exact
// consumption retries and binds the survivor — previously the receipt
// certified the eviction as unaffected and only the whole-graph fallback
// could bind it.
func TestMutationReceiptEvictAmbiguousImportPackageCandidateBindsSurvivor(t *testing.T) {
	for name, store := range openReceiptImportStores(t) {
		t.Run(name, func(t *testing.T) {
			seedAmbiguousImportPackages(store, 2, "", graph.UnresolvedMarker+"import::example/pkg")
			mi := &MultiIndexer{graph: store, logger: zap.NewNop()}

			token := store.BeginMutationReceipt()
			store.EvictFile("src/one.go")
			receipt := store.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant {
				t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
			}

			mode, ok := mi.resolveDeferredMutations(&receipt, false, nil, false)
			if mode != deferredResolveExact || !ok {
				t.Fatalf("mode = %q ok = %v, want %q via the exact path", mode, ok, deferredResolveExact)
			}
			if got := importEdgeTarget(t, store, "two::pkg"); got != "two::pkg" {
				t.Fatalf("exact consumption left the import unbound, want two::pkg")
			}
		})
	}
}

// TestMutationReceiptEvictAmbiguousImportPackageCandidateKeepsAmbiguity: with
// three candidates, evicting one leaves two — the exact retry must refuse the
// remaining ambiguity exactly like ResolveAll.
func TestMutationReceiptEvictAmbiguousImportPackageCandidateKeepsAmbiguity(t *testing.T) {
	for name, store := range openReceiptImportStores(t) {
		t.Run(name, func(t *testing.T) {
			seedAmbiguousImportPackages(store, 3, "", graph.UnresolvedMarker+"import::example/pkg")
			mi := &MultiIndexer{graph: store, logger: zap.NewNop()}

			token := store.BeginMutationReceipt()
			store.EvictFile("src/one.go")
			receipt := store.EndMutationReceipt(token)

			mode, ok := mi.resolveDeferredMutations(&receipt, false, nil, false)
			if mode != deferredResolveExact || !ok {
				t.Fatalf("mode = %q ok = %v, want %q via the exact path", mode, ok, deferredResolveExact)
			}
			if got := importEdgeTarget(t, store, "two::pkg", "three::pkg"); got != "" {
				t.Fatalf("surviving two-way ambiguity bound to %q, want unresolved", got)
			}
		})
	}
}

// TestMutationReceiptEvictAmbiguousImportPackageCandidateRepoPrefixedStub is
// the multi-repo COPY-rewrite form: the pending stub carries the caller's
// repository prefix, so the receipt-name retry must enumerate the prefixed
// stub key too.
func TestMutationReceiptEvictAmbiguousImportPackageCandidateRepoPrefixedStub(t *testing.T) {
	for name, store := range openReceiptImportStores(t) {
		t.Run(name, func(t *testing.T) {
			seedAmbiguousImportPackages(store, 2, "three",
				"three::"+graph.UnresolvedMarker+"import::example/pkg")
			mi := &MultiIndexer{
				graph:    store,
				logger:   zap.NewNop(),
				indexers: map[string]*Indexer{"one": nil, "two": nil, "three": nil},
			}

			token := store.BeginMutationReceipt()
			store.EvictFile("src/one.go")
			receipt := store.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant {
				t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
			}

			mode, ok := mi.resolveDeferredMutations(&receipt, false, nil, false)
			if mode != deferredResolveExact || !ok {
				t.Fatalf("mode = %q ok = %v, want %q via the exact path", mode, ok, deferredResolveExact)
			}
			if got := importEdgeTarget(t, store, "two::pkg"); got != "two::pkg" {
				t.Fatalf("exact consumption left the repo-prefixed import unbound, want two::pkg")
			}
		})
	}
}
