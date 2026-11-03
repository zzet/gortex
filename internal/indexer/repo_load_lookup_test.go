package indexer

import (
	"slices"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/pathkey"
)

func TestRepoLoadRecordLookupPreservesLastAlias(t *testing.T) {
	records := []RepoLoad{
		{Root: "/root/a", key: pathkey.IdentityKey("/root/a"), canonicalKey: pathkey.IdentityKey("/canonical/shared")},
		{Root: "/root/b", key: pathkey.IdentityKey("/root/b"), canonicalKey: pathkey.IdentityKey("/canonical/shared")},
		{Root: "/root/c", key: pathkey.IdentityKey("/canonical/shared"), canonicalKey: pathkey.IdentityKey("/canonical/c")},
		{Root: "/root/a", key: pathkey.IdentityKey("/root/a"), canonicalKey: pathkey.IdentityKey("/canonical/last-a")},
	}
	original := newRepoLoadSnapshot(records)
	for _, path := range []string{"", "/missing", "/root/a", "/root/b", "/canonical/shared", "/canonical/c", "/root/a/../a", "/ROOT/A"} {
		wi, wok := original.find(path)
		gi, gok := findRepoLoadRecord(records, path)
		if wi != gi || wok != gok {
			t.Fatalf("%q: original(%d,%t), direct(%d,%t)", path, wi, wok, gi, gok)
		}
	}
	i, ok := findRepoLoadRecord(records, "/canonical/shared")
	if !ok || i != 2 {
		t.Fatal("last lexical/canonical alias must win")
	}
	i, ok = findRepoLoadRecord(records, "/root/a")
	if !ok || i != 3 {
		t.Fatal("last duplicate lexical key must win")
	}
}

func TestRepoLoadRecordLookupConcurrentImmutableAndABA(t *testing.T) {
	records := []RepoLoad{{Root: "/root/a", Prefix: "old", State: RepoLoadQueued, key: pathkey.IdentityKey("/root/a"), canonicalKey: pathkey.IdentityKey("/canonical/a")}}
	mi := &MultiIndexer{}
	table := &mi.repoLoads
	table.snap.Store(newRepoLoadSnapshot(slices.Clone(records)))
	frozen := table.load()
	stop := make(chan struct{})
	errors := make(chan string, 1)
	var readers sync.WaitGroup
	for reader := 0; reader < 3; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := table.load()
				want, wok := snapshot.find("/canonical/a")
				got, gok := findRepoLoadRecord(snapshot.records, "/canonical/a")
				if want != got || wok != gok {
					select {
					case errors <- "lookup diverged during publication":
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 64; i++ {
		mi.setRepoLoad("/canonical/a", func(rec *RepoLoad) bool {
			rec.Prefix = "replacement"
			if i%2 == 1 {
				rec.Prefix = "old"
			}
			return true
		})
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	if frozen.records[0].Prefix != "old" {
		t.Fatal("old published record changed across ABA")
	}
}

func TestRepoLoadRecordLookupAllocatesNoTemporarySnapshot(t *testing.T) {
	records := []RepoLoad{{Root: "/owned/a", key: pathkey.IdentityKey("/owned/a"), canonicalKey: pathkey.IdentityKey("/canonical/a")}}
	var index int
	var found bool
	allocated := testing.AllocsPerRun(20, func() { index, found = findRepoLoadRecord(records, "/canonical/a") })
	if !found || index != 0 {
		t.Fatal("lookup lost canonical identity")
	}
	if allocated != 0 {
		t.Fatalf("single-root lookup allocated %g objects; no temporary snapshot is needed", allocated)
	}
}
