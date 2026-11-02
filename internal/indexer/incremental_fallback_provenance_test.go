package indexer

import (
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/search"
)

// Unlike refreshMismatchSource, version B remains on disk and is the version
// the real legacy fallback parses. No concurrent scheduling race is needed.
type persistentRefreshSource struct {
	source.ContentSource
	name, absolute string
	next           []byte
	reads          atomic.Int32
	beforeMove     func()
}

func (s *persistentRefreshSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	if path == s.name && s.reads.Add(1) == 2 {
		if s.beforeMove != nil {
			s.beforeMove()
		}
		if err := os.WriteFile(s.absolute, s.next, 0o644); err != nil {
			return nil, source.FileMeta{}, err
		}
	}
	return s.ContentSource.Open(path)
}

func TestIncrementalFallbackUsesAppliedSourceProvenance(t *testing.T) {
	for _, inert := range []bool{false, true} {
		name, file := "changed-probe", "edit.go"
		if inert {
			name, file = "inert-forced-probe", "edit_test.go"
		}
		t.Run(name, func(t *testing.T) {
			const original = "package p\n\nfunc Before() {}\n"
			root := t.TempDir()
			path := filepath.Join(root, file)
			writeTestFile(t, path, original)
			idx := newTestIndexer(graph.New())
			idx.search = search.NewNull()
			idx.SetRootPath(root)
			_, err := idx.Index(root)
			require.NoError(t, err)
			require.NoError(t, idx.IndexFile(path))
			versionA := "package p\n\nfunc After() {}\n"
			if inert {
				versionA = original // a forced unchanged probe is eligible for inert restatement
				require.Nil(t, idx.contractCoreInputs, "exercise the existing per-file legacy inert marker path")
				idx.inertReparsed = make(map[string]struct{})
				idx.reparseKeepingResolutions = map[string]struct{}{path: {}}
			}
			bumpMtime(t, path, versionA)
			const versionB = "package p\nimport \"fmt\"\ntype Latest interface { Print() }\nfunc After() { fmt.Println(\"latest\") }\n"
			fs, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, fs.Close()) })
			changing := &persistentRefreshSource{ContentSource: fs, name: file, absolute: path, next: []byte(versionB)}
			speculativeInert := false
			if inert {
				changing.beforeMove = func() {
					_, speculativeInert = idx.inertReparsed[file]
				}
			}
			idx.SetContentSource(changing)
			t.Cleanup(func() { idx.SetContentSource(nil) })
			_, plan, reparsed, failed, _ := idx.reindexIncrementalChunk([]string{path}, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
			require.Empty(t, failed)
			require.Contains(t, reparsed, path)
			require.GreaterOrEqual(t, changing.reads.Load(), int32(3), "probe A, refresh B, and fallback B must all execute")
			require.NotNil(t, nodeByName(t, idx, file, "Latest"), "prove the fallback actually applied B")
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, versionB, string(current))
			assert.True(t, plan.Flags.Has(DerivedInvalidatesImports), "B added an import even though speculative A did not")
			assert.NotContains(t, plan.HierarchyUnchanged, file, "B introduced a new interface")
			if inert {
				require.True(t, speculativeInert, "prove the original unchanged probe was marked inert before refresh")
				assert.True(t, plan.Flags.Has(DerivedInvalidatesTests), "the applied test file changed semantically")
				assert.NotContains(t, idx.inertReparsed, file, "never restate old rows over a newer applied source")
			}
		})
	}
}
