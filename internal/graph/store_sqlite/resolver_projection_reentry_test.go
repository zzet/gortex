package store_sqlite

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolverScopedProjectionYieldCanReenterStore(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	stopStoreBackgroundReaders(store)
	store.AddBatch([]*graph.Node{
		{ID: "a-id", Kind: graph.KindFile, FilePath: "a-id", RepoPrefix: "a"},
		{ID: "b-id", Kind: graph.KindFile, FilePath: "b-id", RepoPrefix: "b"},
	}, nil)
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	count := 0
	for range store.FileNodeIdentitiesSeq([]string{"b", "a"}) {
		if inUse := store.db.Stats().InUse; inUse != 0 {
			t.Fatalf("scoped yield retained %d read connection(s)", inUse)
		}
		var one int
		if err := store.db.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("scoped re-entry query: %v", err)
		}
		if one != 1 {
			t.Fatalf("SELECT 1 = %d", one)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("scoped projection yielded %d rows, want 2", count)
	}
}
