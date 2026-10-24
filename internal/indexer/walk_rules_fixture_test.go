package indexer

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestWalkRulesFixture(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"src/a.go", "vendor/x.go", "build/keep.go", "build/drop.go",
		"docs/ok.go", "docs/draft.go", ".claude/state.go",
	} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(path)), "package sample\n\nfunc Example() {}\n")
	}
	writeExcludeFixture(t, filepath.Join(root, "docs", ".gortexignore"), "draft.go\n")
	writeFile(t, filepath.Join(root, "tool.unknownext"), "#!/usr/bin/env python3\ndef example():\n    return 1\n")

	// An unavailable symlink skips only this fixture entry, not the four walks.
	outside := filepath.Join(t.TempDir(), "outside.go")
	t.Run("escaping_symlink", func(t *testing.T) {
		writeFile(t, outside, "package outside\n\nfunc Outside() {}\n")
		if err := os.Symlink(outside, filepath.Join(root, "link.go")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})

	newIndexer := func() *Indexer {
		reg := parser.NewRegistry()
		reg.Register(languages.NewGoExtractor())
		reg.Register(languages.NewPythonExtractor())
		cfg := config.Default().Index
		cfg.Workers = 2
		cfg.Exclude = append(append([]string{}, excludes.Builtin...), "build/", "!build/keep.go")
		return New(graph.New(), reg, cfg, zap.NewNop())
	}

	// These paths come from the fixture rules, not from any walk's output.
	// build/keep.go stays excluded: build/ itself was never re-included.
	want := []string{"docs/ok.go", "src/a.go", "tool.unknownext"}

	t.Run("bulk", func(t *testing.T) {
		idx := newIndexer()
		result, err := idx.Index(root)
		require.NoError(t, err)
		require.Equal(t, len(want), result.FileCount)
		require.Equal(t, want, slices.Sorted(maps.Keys(idx.FileMtimes())))
	})

	t.Run("scoped_discovery", func(t *testing.T) {
		idx := newIndexer()
		result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: false})
		require.NoError(t, err)
		require.Equal(t, len(want), result.FileCount)
		require.Equal(t, want, slices.Sorted(maps.Keys(idx.FileMtimes())))
	})

	t.Run("census", func(t *testing.T) {
		idx := newIndexer()
		_, err := idx.Index(root)
		require.NoError(t, err)
		stale := make(map[string]int64, len(want))
		for _, path := range want {
			stale[path] = 1
		}
		idx.SetFileMtimes(stale)
		changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
		require.NoError(t, err)
		require.Equal(t, want, changed)
		require.Empty(t, deleted)
		require.Equal(t, len(want), detected)
	})

	t.Run("intake", func(t *testing.T) {
		manifest, err := newIndexer().dryRunIntake(t.Context(), root)
		require.NoError(t, err)
		require.Equal(t, len(want), manifest.FilesAdmitted)
		require.Len(t, manifest.LargestAdmittedFiles, len(want), "the hashed list must include every admitted file")
		var wantHashes, gotHashes []string
		for _, path := range want {
			wantHashes = append(wantHashes, hashPath(path))
		}
		for _, file := range manifest.LargestAdmittedFiles {
			gotHashes = append(gotHashes, file.PathHash)
		}
		slices.Sort(wantHashes)
		slices.Sort(gotHashes)
		require.Equal(t, wantHashes, gotHashes)
	})
}
