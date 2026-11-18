package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer/source"
)

func newWalkGateOrderFixture(t *testing.T) (string, *Indexer, *admissionOpenRecorder) {
	t.Helper()
	root := t.TempDir()
	writeExcludeFixture(t, filepath.Join(root, "src", "admitted.go"), "package sample\n")
	writeExcludeFixture(t, filepath.Join(root, "src", "ignored.walksniff"), "#!/usr/bin/env python3\npass\n")
	idx := newAdmissionTestIndexer(t, root, "ignored.*")
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	recorder := &admissionOpenRecorder{ContentSource: fsSource}
	idx.SetContentSource(recorder)
	return root, idx, recorder
}

func TestWalkGateOrderExclusionPrecedesSniff(t *testing.T) {
	for _, walk := range []string{"mtime_probe", "census", "intake"} {
		t.Run(walk, func(t *testing.T) {
			root, idx, recorder := newWalkGateOrderFixture(t)
			info, err := os.Stat(filepath.Join(root, "src", "admitted.go"))
			require.NoError(t, err)
			idx.SetFileMtimes(map[string]int64{"src/admitted.go": info.ModTime().UnixNano()})

			switch walk {
			case "mtime_probe":
				assert.False(t, idx.HasChangesSinceMtimes(root), "the admitted file is unchanged")
			case "census":
				changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
				require.NoError(t, err)
				assert.Empty(t, changed)
				assert.Empty(t, deleted)
				assert.Equal(t, 1, detected)
			case "intake":
				manifest, err := idx.dryRunIntake(t.Context(), root)
				require.NoError(t, err)
				assert.Equal(t, 1, manifest.FilesAdmitted)
				assert.Equal(t, []FileBucket{{PathHash: hashPath("src/admitted.go"), Ext: "go", Bytes: info.Size()}}, manifest.LargestAdmittedFiles)
			}
			// The only unclaimed extension in this fixture is the excluded
			// shebang file. The admitted .go file needs no sniff read.
			t.Logf("excluded shebang content opens: %d", recorder.opens)
			assert.Zero(t, recorder.opens, "excluded shebang file must not be opened")
		})
	}
}

func TestWalkGateOrderEscapingSymlinkNotSniffed(t *testing.T) {
	for _, walk := range []string{"mtime_probe", "census", "intake"} {
		t.Run(walk, func(t *testing.T) {
			root, idx, recorder := newWalkGateOrderFixture(t)
			outside := filepath.Join(t.TempDir(), "script")
			writeFile(t, outside, "#!/usr/bin/env python3\npass\n")
			if err := os.Symlink(outside, filepath.Join(root, "src", "link.walklink")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			info, err := os.Stat(filepath.Join(root, "src", "admitted.go"))
			require.NoError(t, err)
			idx.SetFileMtimes(map[string]int64{"src/admitted.go": info.ModTime().UnixNano()})

			switch walk {
			case "mtime_probe":
				assert.False(t, idx.HasChangesSinceMtimes(root))
			case "census":
				_, _, _, err := idx.changedSinceMtimesCensus(root)
				require.NoError(t, err)
			case "intake":
				manifest, err := idx.dryRunIntake(t.Context(), root)
				require.NoError(t, err)
				assert.Equal(t, 1, manifest.FilesAdmitted)
			}
			assert.Zero(t, recorder.opens, "an escaping symlink must not be opened for a shebang sniff")
		})
	}
}

func TestWalkGateOrderAdmittedSets(t *testing.T) {
	// Each path gets its own root so the boolean mtime probe observes its
	// admission without short-circuiting on another newly admitted file.
	for _, tc := range []struct {
		path   string
		body   string
		admit  bool
		census bool
	}{
		{"src/admitted.go", "package sample\n", true, true},
		{"src/ignored.go", "package sample\n", false, false},
		{"src/script.walksniff", "#!/usr/bin/env python3\npass\n", true, true},
		{"src/ignored.walksniff", "#!/usr/bin/env python3\npass\n", false, false},
		{"src/plain.walkunclaimed", "not source\n", false, false},
		{"go.mod", "module example.com/sample\n", false, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(tc.path)), tc.body)
			idx := newAdmissionTestIndexer(t, root, "ignored.*")
			// The probe counts the same files as the census.
			assert.Equal(t, tc.census, idx.HasChangesSinceMtimes(root))

			idx.SetFileMtimes(map[string]int64{tc.path: 1})
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			if tc.census {
				assert.Equal(t, []string{tc.path}, changed)
				assert.Equal(t, 1, detected)
			} else {
				assert.Empty(t, changed)
				assert.Zero(t, detected)
			}
			if tc.path == "src/ignored.go" || tc.path == "src/ignored.walksniff" {
				assert.Equal(t, []string{tc.path}, deleted)
			} else {
				assert.Empty(t, deleted)
			}

			manifest, err := idx.dryRunIntake(t.Context(), root)
			require.NoError(t, err)
			if tc.admit {
				assert.Equal(t, 1, manifest.FilesAdmitted)
				require.Len(t, manifest.LargestAdmittedFiles, 1)
				assert.Equal(t, hashPath(tc.path), manifest.LargestAdmittedFiles[0].PathHash)
			} else {
				assert.Zero(t, manifest.FilesAdmitted)
				assert.Empty(t, manifest.LargestAdmittedFiles)
			}
		})
	}
}

func TestWalkGateOrderIntakeExcludedReason(t *testing.T) {
	root := t.TempDir()
	writeExcludeFixture(t, filepath.Join(root, "src", "ignored.walkreason"), "no shebang\n")
	idx := newAdmissionTestIndexer(t, root, "ignored.*")
	manifest, err := idx.dryRunIntake(t.Context(), root)
	require.NoError(t, err)
	assert.Zero(t, manifest.FilesAdmitted)
	assert.Equal(t, 1, manifest.FilesSkipped)
	bucket, ok := bucketByKey(manifest.TopSkippedExtensionsByBytes, "walkreason")
	require.True(t, ok)
	assert.Equal(t, "excluded", bucket.Reason)
}
