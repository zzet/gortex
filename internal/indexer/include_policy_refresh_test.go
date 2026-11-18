package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestIncludePolicyRefreshLifecycle(t *testing.T) {
	for _, scenario := range []string{"add_retrack", "remove_retrack", "add_reload"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0755))
			target := filepath.Join(root, "vendor/acme/keep.go")
			writeExcludeFixture(t, target, "package app\nfunc LiveIncluded() {}\n")
			cfgPath := filepath.Join(root, ".gortex.yaml")
			body := "include:\n  - vendor/acme/keep.go\n"
			if scenario == "remove_retrack" {
				writeFile(t, cfgPath, body)
			}
			mi, w, reconciled := newGitignoreRefreshWatcher(t, root, "", "")
			idx := mi.GetIndexer("app")
			before := idx.excludeMatcher()
			watchBefore := w.currentExcludeMatcher()
			_, err := mi.IncrementalReindexRepo("app", nil)
			require.NoError(t, err)
			require.Same(t, before, idx.excludeMatcher(), "unchanged producer policy reuses matcher")
			require.Same(t, watchBefore, w.currentExcludeMatcher(), "unchanged watcher policy reuses matcher")
			select {
			case err := <-reconciled:
				t.Fatalf("unchanged policy reconciled: %v", err)
			default:
			}
			if scenario == "remove_retrack" {
				require.NoError(t, os.Remove(cfgPath))
			} else {
				writeFile(t, cfgPath, body)
			}
			if scenario == "add_reload" {
				mi.RefreshRepoConfigs()
			} else {
				_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root, Name: "app"})
				require.NoError(t, err)
			}
			_, err = mi.IncrementalReindexRepo("app", nil)
			require.NoError(t, err)
			want := scenario != "remove_retrack"
			require.Equal(t, want, len(mi.graph.FindNodesByName("LiveIncluded")) > 0, "producer membership after include change")
			require.Equal(t, want, (&watcherExcludeFilter{w: w}).ShouldInclude(target), "backend must refresh includes")
			require.Equal(t, !want, w.isExcluded(target), "semantic watcher must refresh includes")
			awaitGitignoreReconcile(t, reconciled)
			writeFile(t, target, "package app\nfunc LiveIncludedEdited() {}\n")
			if want {
				require.NoError(t, w.patchGraphAfterReceiptCheck(target, ChangeModified))
				require.NotEmpty(t, mi.graph.FindNodesByName("LiveIncludedEdited"), "later save reaches the graph")
			} else {
				ticket, err := w.EnqueueFileMutation(context.Background(), target)
				require.NoError(t, err)
				require.Nil(t, ticket, "removed include cannot acquire a mutation")
			}
			before = idx.excludeMatcher()
			watchBefore = w.currentExcludeMatcher()
			_, err = mi.IncrementalReindexRepo("app", nil)
			require.NoError(t, err)
			require.Same(t, before, idx.excludeMatcher())
			require.Same(t, watchBefore, w.currentExcludeMatcher())
		})
	}
}

func TestSourceSearchIncludeCompilationHoldsLock(t *testing.T) {
	for _, buffer := range []bool{false, true} {
		t.Run(map[bool]string{false: "disk", true: "buffer"}[buffer], func(t *testing.T) {
			root := t.TempDir()
			idx := newAdmissionTestIndexer(t, root)
			idx.logger = nil
			idx.SetIgnorePatterns([]string{"vendor/"}, []string{"*.go"})
			idx.excludeMatcher()
			warnings := 0
			// A real policy notice runs during compilation, before any filesystem walk.
			core := zap.NewExample().Core()
			idx.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
				warnings++
				if idx.excludeMu.TryLock() {
					idx.excludeMu.Unlock()
					t.Error("include compilation read config without excludeMu")
				}
				return nil
			}))
			writeFile(t, filepath.Join(root, "main.go"), "package app\n")
			if buffer {
				require.True(t, idx.AdmitSourceSearchBuffer(context.Background(), root, filepath.Join(root, "main.go"), []byte("package app\n"), []string{"vendor/"}))
			} else {
				files, err := idx.CurrentSourceSearchFiles(context.Background(), root, nil, nil, 0, []string{"vendor/"})
				require.NoError(t, err)
				require.Equal(t, []string{"main.go"}, files)
			}
			require.Positive(t, warnings)
		})
	}
}

// Pin the real WalkDir contract rather than synthesizing an impossible Type().
func TestWalkDiskRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	writeFile(t, outside, "package outside\nfunc Escaped() {}\n")
	link := filepath.Join(root, "escape.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	idx := newAdmissionTestIndexer(t, root)
	seen := false
	require.NoError(t, idx.walkDisk(root, root, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if path == link {
			seen = true
			require.NotZero(t, d.Type()&os.ModeSymlink)
			require.False(t, idx.admitFileInOpenDir(root, path, -1, d.Type()).admit)
		}
		return nil
	}))
	require.True(t, seen)
}

func TestExcludeMatcherIncludeOnlyRefresh(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	patterns := idx.config.Exclude
	before := idx.excludeMatcher()
	includes := []string{"vendor/keep.go"}
	idx.refreshEffectiveExcludePatterns(patterns, includes)
	require.NotSame(t, before, idx.excludeMatcher())
	require.True(t, idx.excludeMatcher().IncludesRel("vendor/keep.go"))
	current := idx.excludeMatcher()
	idx.refreshEffectiveExcludePatterns(patterns, includes)
	require.Same(t, current, idx.excludeMatcher())
	idx.refreshEffectiveExcludePatterns(patterns, nil)
	require.False(t, idx.excludeMatcher().IncludesRel("vendor/keep.go"))
}

func TestWildcardIncludeFolderSurfaces(t *testing.T) {
	for _, tc := range []struct {
		include, target string
		want            bool
	}{
		{"gen*/", "gen1", true}, {"gen?/", "gen1", true},
		{"**/generated/", "generated", true}, {"**/generated/", "src/generated", true},
		{"out*", "out", true}, {"vendor/**", "vendor", false}, {"build/**", "build", false},
	} {
		t.Run(tc.include+tc.target, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.target, "a.go")
			writeExcludeFixture(t, path, "package app\n")
			idx := newAdmissionTestIndexer(t, root, tc.target+"/")
			idx.SetIgnorePatterns(idx.config.Exclude, []string{tc.include})
			var walked []string
			require.NoError(t, idx.walkDisk(root, root, func(p string, d os.DirEntry, err error) error {
				require.NoError(t, err)
				if !d.IsDir() && idx.admitFileInOpenDir(root, p, -1, d.Type()).admit {
					walked = append(walked, p)
				}
				return nil
			}))
			require.Equal(t, tc.want, len(walked) == 1)
			require.Equal(t, !tc.want, idx.shouldExclude(path, root, false))
			filter := &checkoutWatchFilter{excludes: idx.excludeMatcher(), roots: []string{root}}
			// The backend conservatively admits folder events when the file form
			// of a contents glob matches; semantic admission still rejects it.
			require.True(t, filter.ShouldInclude(filepath.Dir(path)))
			require.Equal(t, tc.want, filter.ShouldInclude(path))
			files, err := idx.CurrentSourceSearchFiles(context.Background(), root, nil, nil, 0, nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, len(files) == 1)
			require.Equal(t, tc.want, idx.AdmitSourceSearchBuffer(context.Background(), root, path, []byte("package app\n"), nil))
			manifest, err := idx.dryRunIntake(context.Background(), root)
			require.NoError(t, err)
			require.Equal(t, tc.want, manifest.FilesAdmitted == 1)
		})
	}
}

func TestWatcherExcludeMatcherIncludeOnlyRefresh(t *testing.T) {
	patterns := []string{"vendor/"}
	includes := []string(nil)
	w := &Watcher{
		stopping:               true, // No asynchronous reconcile is needed for a matcher-only check.
		currentExcludePatterns: func() []string { return patterns },
		currentIncludePatterns: func() []string { return includes },
	}
	before := w.currentExcludeMatcher()
	require.True(t, before.MatchRel("vendor/keep.go"))
	includes = []string{"vendor/keep.go"}
	after := w.currentExcludeMatcher()
	require.NotSame(t, before, after)
	require.False(t, after.MatchRel("vendor/keep.go"))
	require.Same(t, after, w.currentExcludeMatcher())
	includes = nil
	require.True(t, w.currentExcludeMatcher().MatchRel("vendor/keep.go"))
}

func TestWalkAdmissionUsesParentsOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "closed/keep.go")
	writeExcludeFixture(t, path, "package app\n")
	idx := newAdmissionTestIndexer(t, root, "closed/", "!keep.go")
	// This deliberately bypasses walkDisk. Its caller proof is the only
	// reason the file gate may skip parents: explicit paths must not do so.
	require.False(t, idx.admitWalkFileKnownType(root, path, -1, 0).admit)
	require.True(t, idx.admitFileInOpenDir(root, path, -1, 0).admit)
	idx.SetExcludePatterns([]string{"closed/", "keep.go"})
	require.False(t, idx.admitFileInOpenDir(root, path, -1, 0).admit, "the final entry is still checked")
}

func TestSourceSearchMatcherReusesImmutableSnapshot(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	path := filepath.Join(root, "main.go")
	writeExcludeFixture(t, path, "package app\n")
	patterns := []string{"vendor/"}
	for range 2 {
		files, err := idx.CurrentSourceSearchFiles(context.Background(), root, nil, nil, 0, patterns)
		require.NoError(t, err)
		require.Equal(t, []string{"main.go"}, files)
		require.NotNil(t, idx.sourceSearchCompiled, "enumeration must populate the snapshot cache")
	}
	before := idx.sourceSearchCompiled
	require.Same(t, before, idx.sourceSearchMatcher(patterns))
	idx.SetIgnorePatterns(idx.config.Exclude, []string{"vendor/keep.go"})
	require.True(t, idx.AdmitSourceSearchBuffer(context.Background(), root, path, []byte("package app\n"), patterns))
	after := idx.sourceSearchCompiled
	require.NotSame(t, before, after, "buffer admission must refresh the shared cache when includes change")
	require.True(t, after.IncludesRel("vendor/keep.go"))
	require.Same(t, after, idx.sourceSearchMatcher(patterns))
	changed := []string{"vendor/", "*.go"}
	require.False(t, idx.AdmitSourceSearchBuffer(context.Background(), root, path, []byte("package app\n"), changed))
	require.NotSame(t, after, idx.sourceSearchCompiled)
	require.Same(t, idx.excludeMatcher(), idx.sourceSearchMatcher(nil))
}
