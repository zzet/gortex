package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/resolver"
)

// Given a caller recovery that reads the file while a save of that file is
// invalidated, when the recovery stores its result, then the stat is not
// trusted, the next recovery reads again, and the one after reuses the facts.
// "cached" stores through the equal-digest path; "first" stores through the
// extract path for a file that had no cache entry when the save landed.
func TestGoModuleSourceStoreDropsTrustAfterConcurrentSave(t *testing.T) {
	for _, mode := range []string{"cached", "first"} {
		t.Run(mode, func(t *testing.T) {
			mi, _, repos := goModuleLifecycleFixture(t, "memory")
			main := filepath.Join(repos[0].Path, "main.go")
			writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other(){}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			idx := mi.GetIndexer("a")
			require.Contains(t, idx.goModuleSourceCache, "main.go")
			if mode == "cached" {
				// A new stat with equal bytes makes the recovery verify by reading.
				touched := time.Now().Add(-time.Hour)
				require.NoError(t, os.Chtimes(main, touched, touched))
			} else {
				delete(idx.goModuleSourceCache, "main.go")
			}
			original := readGoModuleCallerFile
			reads := 0
			readGoModuleCallerFile = func(path string) ([]byte, error) {
				reads++
				src, err := original(path)
				if reads == 1 {
					idx.invalidateGoModuleSourceStamp("main.go") // a save lands during the read
				}
				return src, err
			}
			t.Cleanup(func() { readGoModuleCallerFile = original })
			cr := mi.newCrossRepoResolver()
			mi.restoreGoModuleSourceFile(cr, "a/main.go")
			require.Equal(t, 1, reads)
			stored := idx.goModuleSourceCache["main.go"]
			assert.Zero(t, stored.size, "a result read before a known save must not be trusted by stat")
			assert.Zero(t, stored.modified, "a result read before a known save must not be trusted by stat")
			mi.restoreGoModuleSourceFile(cr, "a/main.go")
			assert.Equal(t, 2, reads, "the next recovery verifies content again")
			mi.restoreGoModuleSourceFile(cr, "a/main.go")
			assert.Equal(t, 2, reads, "an undisturbed verification restores stat trust")
		})
	}
}

// Given a caller recovery that reads the file while it is deleted and
// evicted, when the recovery stores its result, then the stat is not trusted,
// and a file that comes back with other bytes but the same size and
// modification time is read again instead of restoring the old calls.
func TestGoModuleSourceStoreDropsTrustAfterConcurrentEviction(t *testing.T) {
	mi, _, repos := goModuleLifecycleFixture(t, "memory")
	main := filepath.Join(repos[0].Path, "main.go")
	writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Hello(){}\nfunc Other(){}\n")
	for _, entry := range repos[:2] {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	idx := mi.GetIndexer("a")
	delete(idx.goModuleSourceCache, "main.go")
	info, err := os.Stat(main)
	require.NoError(t, err)
	original := readGoModuleCallerFile
	reads := 0
	readGoModuleCallerFile = func(path string) ([]byte, error) {
		reads++
		src, err := original(path)
		if reads == 1 {
			// The file is deleted and evicted during the read.
			require.NoError(t, os.Remove(main))
			idx.evictDeletedFilesBatched([]string{"main.go"}, &DerivedInvalidationPlan{}, nil)
		}
		return src, err
	}
	t.Cleanup(func() { readGoModuleCallerFile = original })
	cr := mi.newCrossRepoResolver()
	mi.restoreGoModuleSourceFile(cr, "a/main.go")
	require.Equal(t, 1, reads)
	assert.Zero(t, idx.goModuleSourceCache["main.go"].size, "a result read before an eviction must not be trusted by stat")
	// The file comes back with a different call, the same size and the same modification time.
	writeFile(t, main, "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Other() }\n")
	require.NoError(t, os.Chtimes(main, info.ModTime(), info.ModTime()))
	mi.restoreGoModuleSourceFile(cr, "a/main.go")
	assert.Equal(t, 2, reads, "the returned file is read again")
	var specs []string
	for _, e := range idx.goModuleSourceCache["main.go"].edges {
		specs = append(specs, resolver.GoModuleCallSpec(e))
	}
	assert.Equal(t, []string{"example.org/library::Other"}, specs)
}
