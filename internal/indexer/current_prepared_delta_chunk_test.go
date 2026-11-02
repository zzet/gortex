package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// A real editor save lands after the original bytes were read for extraction.
// Check the production chunk's graph/derived provenance and completed legacy
// application after it discards the obsolete speculative parse.
func TestMovedRefreshChunkUsesFreshLegacyApply(t *testing.T) {
	for _, forcedInert := range []bool{false, true} {
		name, file := "changed-probe", "edit.go"
		if forcedInert {
			name, file = "forced-inert-probe", "edit_test.go"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, file)
			const original = "package p\nfunc Before(){}\n"
			writeTestFile(t, path, original)
			idx := newTestIndexer(graph.New())
			idx.search = search.NewNull()
			idx.SetRootPath(root)
			t.Cleanup(idx.Close)
			_, err := idx.Index(root)
			require.NoError(t, err)
			require.NoError(t, idx.IndexFile(path))
			versionA := "package p\nfunc After(){}\n"
			if forcedInert {
				versionA = original
				idx.inertReparsed = make(map[string]struct{})
				idx.reparseKeepingResolutions = map[string]struct{}{path: {}}
			}
			bumpMtime(t, path, versionA)
			extractor, ok := idx.registry.GetByLanguage("go")
			require.True(t, ok)
			addition := "\ntype Latest interface { Print() }\n"
			mover := &savingExtractor{Extractor: extractor, trigger: file, path: path, line: addition}
			mover.armed.Store(true)
			idx.registry.Register(mover)
			core, logs := observer.New(zap.InfoLevel)
			idx.logger = zap.New(core)
			_, plan, reparsed, failed, changed := idx.reindexIncrementalChunk([]string{path}, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
			require.False(t, mover.armed.Load(), "prove the actual newer save happened during extraction")
			_, saveErr := mover.saved()
			require.NoError(t, saveErr)
			require.Empty(t, failed)
			require.Empty(t, changed, "the legacy receipt must belong to the fresh B read")
			require.Contains(t, reparsed, path)
			require.NotNil(t, nodeByName(t, idx, file, "Latest"), "the production chunk must apply B, never obsolete A")
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, versionA+addition, string(current))
			require.NotContains(t, plan.HierarchyUnchanged, file, "new interface must invalidate hierarchy")
			if forcedInert {
				require.True(t, plan.Flags.Has(DerivedInvalidatesTests))
				require.NotContains(t, idx.inertReparsed, file, "no obsolete A restatement may overwrite applied B")
			}
			parsePhases, fallbackPhases := 0, 0
			for _, entry := range logs.FilterMessage("indexer: reconcile phase complete").All() {
				fields := entry.ContextMap()
				if fields["phase"] == "chunk_fallback_parse_apply" {
					fallbackPhases++
					require.EqualValues(t, 1, fields["files"])
					require.EqualValues(t, 0, fields["failed_files"])
				}
				if fields["phase"] == "chunk_parse" {
					parsePhases++
					require.EqualValues(t, 1, fields["fallback_files"])
					require.EqualValues(t, 0, fields["staged_files"], "the obsolete parse must be discarded")
				}
			}
			require.Equal(t, 1, parsePhases)
			require.Equal(t, 1, fallbackPhases, "the authoritative legacy parse/apply must complete")
		})
	}
}
