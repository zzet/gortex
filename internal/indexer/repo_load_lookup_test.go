package indexer

import (
	"fmt"
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
	queries := []struct{ name, path string }{
		{"clean_ascii", "/canonical/a"},
		{"cleaning", "/canonical//./a"},
		{"unicode_and_case", "/canonical/Cafe\u0301/A"},
		{"windows_slashes", "c:/Canonical/A"},
		{"windows_backslashes", `c:\Canonical\A`},
	}
	for _, count := range []int{200, 1000} {
		for _, query := range queries {
			t.Run(fmt.Sprintf("records=%d/%s", count, query.name), func(t *testing.T) {
				records := make([]RepoLoad, count)
				for i := range records {
					root := fmt.Sprintf("/owned/repo-%04d", i)
					records[i] = RepoLoad{Root: root, Prefix: fmt.Sprintf("repo-%04d", i), State: RepoLoadReady, key: pathkey.IdentityKey(root)}
				}
				records[0].canonicalKey = pathkey.IdentityKey(query.path)
				// Build the reference index OUTSIDE the measured lookup. Both routes
				// retain exactly one IdentityKey call, whose unavoidable normalization
				// allocation varies by host/path (for example Windows slash cleaning).
				snapshot := newRepoLoadSnapshot(records)
				var baselineIndex, actualIndex int
				var baselineFound, actualFound bool
				baseline := testing.AllocsPerRun(20, func() { baselineIndex, baselineFound = snapshot.find(query.path) })
				actual := testing.AllocsPerRun(20, func() { actualIndex, actualFound = findRepoLoadRecord(records, query.path) })
				if !baselineFound || baselineIndex != 0 || actualFound != baselineFound || actualIndex != baselineIndex {
					t.Fatal("lookup lost canonical identity or differs from the existing index")
				}
				var legacyIndex int
				var legacyFound bool
				legacy := testing.AllocsPerRun(20, func() { legacyIndex, legacyFound = newRepoLoadSnapshot(records).find(query.path) })
				if !legacyFound || legacyIndex != baselineIndex {
					t.Fatal("original temporary snapshot lost the same canonical identity")
				}
				if legacy <= baseline {
					t.Fatal("positive control did not expose the original temporary snapshot allocation")
				}
				t.Logf("prebuilt-index normalization allocations=%g direct=%g original-temporary=%g", baseline, actual, legacy)
				if actual != baseline {
					t.Fatalf("single-root lookup allocated %g objects, prebuilt-index normalization allocated %g; temporary snapshot allocation is not needed", actual, baseline)
				}
			})
		}
	}
}
