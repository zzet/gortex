package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

func TestWatcherAgentConfigIsExcluded(t *testing.T) {
	root := t.TempDir()
	idx := &Indexer{rootPath: root}
	idx.config.Exclude = []string{"indexer-only.go"}
	patterns := append(append([]string{}, excludes.Builtin...), "custom.go")
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	writeExcludeFixture(t, filepath.Join(root, "sub/.gortexignore"), "ignored.go\n")
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"claude_mcp", ".claude/mcp.json", false},
		{"kiro_mcp", ".kiro/mcp.json", false},
		{"agent_state", ".claude/state.unknown", true},
		{"per_folder_ignore", "sub/ignored.go", true},
		{"watcher_own_flat_list", "custom.go", true},
		{"indexer_only_flat_list", "indexer-only.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.path)
			writeExcludeFixture(t, path, "fixture\n")
			require.Equal(t, tc.want, w.isExcluded(path))
		})
	}
	t.Run("escaping_symlink", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside.go")
		writeFile(t, outside, "package outside\n")
		path := filepath.Join(root, "escape.go")
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		require.True(t, w.isExcluded(path))
	})
}

func TestWatcherAgentConfigBackendFilter(t *testing.T) {
	root := t.TempDir()
	patterns := append(append([]string{}, excludes.Builtin...), "gen", "!gen/keep.go")
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	filter := &watcherExcludeFilter{w: w}
	// These paths need not exist: the backend filter must make no filesystem reads.
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"probe_first", ".git/" + probeMarker + "check", true},
		{"claude_mcp", ".claude/mcp.json", true},
		{"kiro_settings", ".kiro/settings", true},
		{"kiro_settings_mcp", ".kiro/settings/mcp.json", true},
		{"claude_subdirectory", ".claude/sub", false},
		{"agent_state", ".claude/state.unknown", false},
		{"worktree_mcp", ".claude/worktrees/foo/.mcp.json", false},
		{"negated_child_folder", "gen", false},
		{"ordinary_file", "main.go", true},
		{"ignored_tree", "node_modules/pkg/main.js", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, filter.ShouldInclude(filepath.Join(root, tc.path)))
		})
	}
	t.Run("rootless_match_rel", func(t *testing.T) {
		w, err := NewWatcher(&Indexer{}, config.WatchConfig{Exclude: []string{"gen", "!gen/keep.go"}}, zap.NewNop())
		require.NoError(t, err)
		filter := &watcherExcludeFilter{w: w}
		require.False(t, filter.ShouldInclude("gen"))
		require.False(t, filter.ShouldInclude("gen/keep.go"))
		require.True(t, filter.ShouldInclude("main.go"))
		require.True(t, w.isExcluded("gen"))
		require.False(t, w.isExcluded("main.go"))
		reopened, err := NewWatcher(&Indexer{}, config.WatchConfig{Exclude: []string{"gen/*", "!gen/keep/"}}, zap.NewNop())
		require.NoError(t, err)
		require.True(t, (&watcherExcludeFilter{w: reopened}).ShouldInclude("gen/keep"))
	})
}

func TestWatcherAgentConfigMCPEdit(t *testing.T) {
	root := t.TempDir()
	reg := parser.NewRegistry()
	reg.Register(languages.NewMCPConfigExtractor())
	idx := New(graph.New(), reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(root)
	path := filepath.Join(root, ".claude/mcp.json")
	writeExcludeFixture(t, path, `{"mcpServers":{"edited":{"command":"npx"}}}`)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	// No backend loop was started; signal it stopped before joining owned timers.
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	scheduled := make(chan string, 1)
	w.pointMutationPatch = func(path string, kind ChangeKind, generation uint64) error {
		scheduled <- path
		return nil
	}
	w.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}})
	select {
	case got := <-scheduled:
		require.Equal(t, path, got, "MCP edit must reach the file reindex queue")
	case <-time.After(2 * time.Second):
		t.Fatal("MCP edit did not reach the file reindex queue")
	}
}

func TestWatcherAgentConfigNewDirectoryScan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dir   string
		file  string
		body  string
		extra []string
	}{
		{"reincluded_child", "gen", "keep.go", "package gen\nfunc Keep() {}\n", []string{"gen/*", "!gen/keep.go"}},
		{"reopened_child_folder", "gen/keep", "keep.go", "package keep\nfunc Keep() {}\n", []string{"gen/*", "!gen/keep/"}},
		{"claude_directory", ".claude", "mcp.json", `{"mcpServers":{"newserver":{"command":"npx"}}}`, nil},
		{"kiro_settings", ".kiro/settings", "mcp.json", `{"mcpServers":{"newserver":{"command":"npx"}}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			reg.Register(languages.NewMCPConfigExtractor())
			patterns := append(append([]string{}, excludes.Builtin...), tc.extra...)
			idx := New(graph.New(), reg, config.IndexConfig{Workers: 1, Exclude: patterns}, zap.NewNop())
			idx.SetRootPath(root)
			w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
			dir := filepath.Join(root, tc.dir)
			path := filepath.Join(dir, tc.file)
			writeExcludeFixture(t, path, tc.body)
			rel := tc.dir + "/" + tc.file
			require.Empty(t, idx.graph.GetFileNodes(rel))

			type scanResult struct {
				paths []string
				err   error
			}
			scanned := make(chan scanResult, 1)
			// Observe the scheduled scope, then run the real discovery pipeline.
			w.discoverReindex = func(paths []string) (*IndexResult, error) {
				result, err := idx.incrementalDiscoverWatcherPaths(root, paths)
				scanned <- scanResult{paths: paths, err: err}
				return result, err
			}
			require.True(t, (&watcherExcludeFilter{w: w}).ShouldInclude(dir), "backend must deliver the new-directory event")
			w.handleEvent(fswatcher.WatchEvent{Path: dir, Types: []fswatcher.EventType{fswatcher.EventCreate}})
			select {
			case got := <-scanned:
				require.NoError(t, got.err)
				require.Equal(t, []string{dir}, got.paths, "enqueueDirScan must schedule the new folder")
				require.NotEmpty(t, idx.graph.GetFileNodes(rel), "the scan must reindex the child file")
				require.Contains(t, idx.FileMtimes(), rel)
			case <-time.After(2 * time.Second):
				t.Fatal("new-directory create did not enqueue a discovery scan")
			}
		})
	}
}

// Folder events and file events enforce the same parent-folder rule.
func TestWatcherAgentConfigAnchoredNegations(t *testing.T) {
	for _, tc := range []struct {
		name        string
		patterns    []string
		ignorePath  string
		ignoreRules string
		dir         string
		backend     bool
		scan        bool
	}{
		{"unanchored_gitkeep", []string{"node_modules/", "!.gitkeep"}, "", "", "node_modules/pkg", false, false},
		{"unanchored_env", []string{"node_modules/", "!.env.example"}, "", "", "node_modules/pkg", false, false},
		{"wildcard_led", []string{"node_modules/", "!**/.gitkeep"}, "", "", "node_modules/pkg", false, false},
		{"root_anchored_unrelated", []string{"node_modules/", "!/AGENTS.md"}, "", "", "node_modules/pkg", false, false},
		{"anchored_child", []string{"gen", "!gen/keep.go"}, "", "", "gen", false, false},
		{"root_anchored_child", []string{"gen", "!/gen/keep.go"}, "", "", "gen", false, false},
		{"per_folder_unanchored", nil, ".gortexignore", "vendor2/\n!.gitkeep\n", "vendor2/pkg", true, false},
		{"nested_anchored", nil, "sub/.gortexignore", "gen/\n!gen/keep.go\nother/\n", "sub/gen", true, false},
		{"nested_unrelated", nil, "sub/.gortexignore", "gen/\n!gen/keep.go\nother/\n", "sub/other", true, false},
		{"flat_pruned", []string{"gen/"}, "", "", "gen", true, false},
		{"per_folder_pruned", nil, "sub/.gortexignore", "gen/\n", "sub/gen", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.ignorePath != "" {
				writeExcludeFixture(t, filepath.Join(root, tc.ignorePath), tc.ignoreRules)
			}
			w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: tc.patterns}, zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
			dir := filepath.Join(root, tc.dir)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			filter := &watcherExcludeFilter{w: w}
			assert.Equal(t, tc.backend, filter.ShouldInclude(dir), "backend folder verdict")
			if strings.HasPrefix(tc.dir, "node_modules/") {
				assert.False(t, filter.ShouldInclude(filepath.Join(dir, "index.js")), "backend ignored-tree file verdict")
			}
			assert.Equal(t, !tc.scan, w.isExcluded(dir), "full folder verdict")
			scans := make(chan map[string]struct{}, 1)
			w.scanFn = func(dirs map[string]struct{}) { scans <- dirs }
			w.handleEvent(fswatcher.WatchEvent{Path: dir, Types: []fswatcher.EventType{fswatcher.EventCreate}})
			// handleEvent has finished adding work; join the owned scan without a sleep.
			w.asyncWork.Wait()
			if tc.scan {
				require.Len(t, scans, 1)
				require.Equal(t, map[string]struct{}{dir: {}}, <-scans)
			} else {
				assert.Empty(t, scans, "a pruned folder must schedule no scan")
			}
		})
	}
}

func TestWatcherAgentConfigUnanchoredFileVerdict(t *testing.T) {
	root := t.TempDir()
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: []string{"node_modules/", "!.gitkeep"}}, zap.NewNop())
	require.NoError(t, err)
	path := filepath.Join(root, "node_modules/pkg/.gitkeep")
	writeExcludeFixture(t, path, "")
	// A basename negation cannot bypass an excluded parent folder.
	require.False(t, (&watcherExcludeFilter{w: w}).ShouldInclude(path))
	require.True(t, w.isExcluded(path))
	require.True(t, w.isExcluded(filepath.Dir(path)))
}

// Both the cold walk and watcher close an excluded parent folder.
func TestWatcherAgentConfigColdWalkPrunesUnanchoredNegations(t *testing.T) {
	for _, negation := range []string{"!.gitkeep", "!**/keep/"} {
		t.Run(negation, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "node_modules", "pkg")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			idx := newAdmissionTestIndexer(t, root, "node_modules/", negation)
			require.True(t, idx.shouldPruneDir(dir, root))
			w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: []string{"node_modules/", negation}}, zap.NewNop())
			require.NoError(t, err)
			require.True(t, w.isExcluded(dir))
		})
	}
}

func TestWatcherAgentConfigDeletedDirectory(t *testing.T) {
	root := t.TempDir()
	idx := New(graph.New(), parser.NewRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(root)
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: []string{"gen/"}, DebounceMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	path := filepath.Join(root, "gen")
	require.NoError(t, os.Mkdir(path, 0o755))
	require.True(t, w.isExcluded(path))
	require.NoError(t, os.Remove(path))
	// Lstat now fails, so the trailing-slash rule gets the file verdict.
	require.False(t, w.isExcluded(path))
	require.True(t, (&watcherExcludeFilter{w: w}).ShouldInclude(path))
	w.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventRemove}})
	w.mu.Lock()
	generation := w.nextGeneration
	w.mu.Unlock()
	require.Equal(t, uint64(1), generation)
}

func TestWatcherAgentConfigRenameGuard(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{"into_excluded", "src/a.go", "ignored/a.go"},
		{"out_of_excluded", "ignored/a.go", "src/a.go"},
		{"within_admitted", "src/a.go", "src/b.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := New(graph.New(), reg, config.IndexConfig{Workers: 1}, zap.NewNop())
			idx.SetRootPath(root)
			w, err := NewWatcher(idx, config.WatchConfig{Exclude: []string{"ignored/"}, DebounceMs: 60000}, zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
			from, to := filepath.Join(root, tc.from), filepath.Join(root, tc.to)
			writeExcludeFixture(t, from, "package src\n")
			require.NoError(t, os.MkdirAll(filepath.Dir(to), 0o755))
			require.NoError(t, os.Rename(from, to))
			for _, path := range []string{from, to} {
				excluded := strings.HasPrefix(path, filepath.Join(root, "ignored")+string(filepath.Separator))
				require.Equal(t, excluded, w.isExcluded(path), "main's flat-list file verdict")
				require.Equal(t, !excluded, (&watcherExcludeFilter{w: w}).ShouldInclude(path))
				w.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventRename}})
				w.mu.Lock()
				_, scheduled := w.pendingGeneration[path]
				w.mu.Unlock()
				require.Equal(t, !excluded, scheduled, "rename scheduling, including the renamed-away path")
			}
		})
	}
}

func TestWatcherAgentConfigSymlinkDirectoryGuard(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "target")
	require.NoError(t, os.Mkdir(inside, 0o755))
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: []string{"gen/"}}, zap.NewNop())
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		target string
		want   bool
	}{
		{"gen", inside, false},
		{"escape", t.TempDir(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.Symlink(tc.target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			require.Equal(t, tc.want, w.isExcluded(path), "a directory link has the file verdict, with confinement first")
		})
	}
}

func TestWatcherAgentConfigRootlessGuard(t *testing.T) {
	w, err := NewWatcher(&Indexer{}, config.WatchConfig{Exclude: []string{"gen", "!gen/keep.go", "ignored/"}}, zap.NewNop())
	require.NoError(t, err)
	for _, tc := range []struct {
		path    string
		full    bool
		backend bool
	}{
		{"gen", true, false},
		{"gen/keep.go", false, false},
		{"gen/drop.go", false, false},
		{"ignored", false, true},
		{"ignored/child.go", false, false},
		{"main.go", false, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			// Main's full check matches Base(path); its backend matches path.
			require.Equal(t, tc.full, w.isExcluded(tc.path))
			require.Equal(t, tc.backend, (&watcherExcludeFilter{w: w}).ShouldInclude(tc.path))
		})
	}
}

func TestWatcherAgentConfigEscapingMCPGuard(t *testing.T) {
	root := t.TempDir()
	reg := parser.NewRegistry()
	reg.Register(languages.NewMCPConfigExtractor())
	idx := New(graph.New(), reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(root)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	outside := filepath.Join(t.TempDir(), "mcp.json")
	writeExcludeFixture(t, outside, `{"mcpServers":{"escape":{"command":"npx"}}}`)
	path := filepath.Join(root, ".claude/mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.True(t, (&watcherExcludeFilter{w: w}).ShouldInclude(path))
	require.True(t, w.isExcluded(path))
	w.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventCreate}})
	w.mu.Lock()
	generation := w.nextGeneration
	w.mu.Unlock()
	require.Zero(t, generation, "escaping MCP link must not schedule a reindex")
}

func TestWatcherAgentConfigSnapshotGuard(t *testing.T) {
	for _, isDir := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory_%v", isDir), func(t *testing.T) {
			root := t.TempDir()
			writeExcludeFixture(t, filepath.Join(root, "docs/.gortexignore"), "draft.go\n")
			path := filepath.Join(root, "docs/draft.go")
			if isDir {
				require.NoError(t, os.Mkdir(path, 0o755))
			} else {
				writeExcludeFixture(t, path, "package draft\n")
			}
			idx := &Indexer{rootPath: root}
			w, err := NewWatcher(idx, config.WatchConfig{}, zap.NewNop())
			require.NoError(t, err)
			require.True(t, idx.shouldExclude(path, root, isDir))
			require.True(t, idx.shouldPruneDir(path, root))
			require.True(t, w.isExcluded(path))
			fsSource, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, fsSource.Close()) })
			idx.SetContentSource(fsSource)
			require.False(t, idx.shouldExclude(path, root, isDir))
			require.False(t, idx.shouldPruneDir(path, root))
			// The watcher follows the indexer: no per-folder rules under a snapshot.
			require.False(t, w.isExcluded(path))
		})
	}
}

// Agent state under .claude/ and .kiro/ stays closed: only the folders that
// hold an agent's MCP config are open, so a checkout under .claude/worktrees/
// starts no scan and its MCP config copies stay out of the graph.
func TestWatcherAgentConfigClosedFolders(t *testing.T) {
	root := t.TempDir()
	// Neither an unanchored negation nor the MCP exception opens arbitrary
	// agent state folders.
	patterns := append(append([]string{}, excludes.Builtin...), "!.gitkeep")
	idx := newAdmissionTestIndexer(t, root, "!.gitkeep")
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	checkout := filepath.Join(root, ".claude", "worktrees", "foo")
	writeExcludeFixture(t, filepath.Join(checkout, "main.go"), "package main\n")
	writeExcludeFixture(t, filepath.Join(checkout, ".mcp.json"), `{"mcpServers":{}}`)
	writeExcludeFixture(t, filepath.Join(root, ".kiro", "settings", "mcp.json"), `{"mcpServers":{}}`)

	for _, rel := range []string{".claude/worktrees", ".claude/worktrees/foo"} {
		assert.True(t, idx.shouldPruneDir(filepath.Join(root, rel), root), rel)
		assert.True(t, w.isExcluded(filepath.Join(root, rel)), rel)
	}
	assert.True(t, w.isExcluded(filepath.Join(checkout, ".mcp.json")))
	assert.False(t, w.isExcluded(filepath.Join(root, ".kiro", "settings", "mcp.json")))

	scans := make(chan map[string]struct{}, 1)
	w.scanFn = func(dirs map[string]struct{}) { scans <- dirs }
	w.handleEvent(fswatcher.WatchEvent{Path: checkout, Types: []fswatcher.EventType{fswatcher.EventCreate}})
	w.asyncWork.Wait()
	assert.Empty(t, scans, "a new folder under .claude/worktrees/ must start no scan")

	var walked []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() && idx.shouldPruneDir(path, root) {
			return filepath.SkipDir
		}
		if !d.IsDir() && !idx.shouldExclude(path, root, false) {
			rel, _ := filepath.Rel(root, path)
			walked = append(walked, filepath.ToSlash(rel))
		}
		return nil
	}))
	assert.Equal(t, []string{".kiro/settings/mcp.json"}, walked)
}

// The MCP config exception lifts only the Builtin .claude/ and .kiro/ lines:
// the user's own excludes and per-folder ignore files still apply.
func TestWatcherAgentConfigUserExcludes(t *testing.T) {
	root := t.TempDir()
	writeExcludeFixture(t, filepath.Join(root, ".gortexignore"), ".claude/mcp.json\n")
	patterns := append(append([]string{}, excludes.Builtin...), ".kiro/settings/")
	idx := newAdmissionTestIndexer(t, root, ".kiro/settings/")
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	for _, rel := range []string{".claude/mcp.json", ".kiro/settings/mcp.json"} {
		path := filepath.Join(root, rel)
		writeExcludeFixture(t, path, `{"mcpServers":{}}`)
		assert.True(t, idx.shouldExclude(path, root, false), rel)
		assert.True(t, w.isExcluded(path), rel)
	}
	kiro := filepath.Join(root, ".kiro", "mcp.json")
	writeExcludeFixture(t, kiro, `{"mcpServers":{}}`)
	assert.False(t, w.isExcluded(kiro), "an MCP config no user rule names stays admitted")

	// A user exclude spelled exactly like the Builtin line still applies.
	userCopy := append(append([]string{}, excludes.Builtin...), ".kiro/")
	idx = newAdmissionTestIndexer(t, root, ".kiro/")
	w, err = NewWatcher(idx, config.WatchConfig{Exclude: userCopy}, zap.NewNop())
	require.NoError(t, err)
	assert.True(t, idx.shouldExclude(kiro, root, false))
	assert.True(t, w.isExcluded(kiro))

	// So does the same line in a list without Builtin, as the embedded MCP
	// server's watch.exclude can supply.
	w, err = NewWatcher(idx, config.WatchConfig{Exclude: []string{".kiro/"}}, zap.NewNop())
	require.NoError(t, err)
	assert.True(t, w.isExcluded(kiro))
}

// Ignore-file events pass the backend filter even when the user excludes
// the ignore file, so removing a rule still re-includes its files. An
// ignore file in a pruned folder updates the cache but starts no scan.
func TestWatcherIgnoreFileEventsPassBackendFilter(t *testing.T) {
	root := t.TempDir()
	ignore := filepath.Join(root, ".gortexignore")
	writeExcludeFixture(t, ignore, "generated/\n")
	generated := filepath.Join(root, "generated", "x.go")
	writeExcludeFixture(t, generated, "package generated\n")
	patterns := append(append([]string{}, excludes.Builtin...), ".gortexignore")
	idx := newAdmissionTestIndexer(t, root, ".gortexignore")
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	require.True(t, w.isExcluded(generated))

	filter := &watcherExcludeFilter{w: w}
	assert.True(t, filter.ShouldInclude(ignore))
	assert.True(t, w.isExcluded(ignore), "the ignore file itself stays out of the index")

	scans := make(chan map[string]struct{}, 2)
	w.scanFn = func(dirs map[string]struct{}) { scans <- dirs }
	writeExcludeFixture(t, ignore, "")
	w.handleEvent(fswatcher.WatchEvent{Path: ignore, Types: []fswatcher.EventType{fswatcher.EventMod}})
	w.asyncWork.Wait()
	assert.False(t, w.isExcluded(generated), "removing the rule re-includes the file")
	require.Len(t, scans, 1)
	<-scans

	pruned := filepath.Join(root, "node_modules", "pkg", ".gortexignore")
	writeExcludeFixture(t, pruned, "*.js\n")
	assert.True(t, filter.ShouldInclude(pruned))
	w.handleEvent(fswatcher.WatchEvent{Path: pruned, Types: []fswatcher.EventType{fswatcher.EventCreate}})
	w.asyncWork.Wait()
	assert.Empty(t, scans, "a folder the rules prune is not rescanned")
}

// An edited ignore file takes effect at once: the watcher drops the
// folder's cached rules and rescans the folder.
func TestWatcherForgetsEditedIgnoreFile(t *testing.T) {
	root := t.TempDir()
	ignore := filepath.Join(root, "sub", ".gortexignore")
	writeExcludeFixture(t, ignore, "")
	generated := filepath.Join(root, "sub", "generated", "x.go")
	writeExcludeFixture(t, generated, "package generated\n")
	idx := newAdmissionTestIndexer(t, root)
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: excludes.Builtin}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	require.False(t, w.isExcluded(generated))

	scans := make(chan map[string]struct{}, 1)
	w.scanFn = func(dirs map[string]struct{}) { scans <- dirs }
	writeExcludeFixture(t, ignore, "generated/\n")
	w.handleEvent(fswatcher.WatchEvent{Path: ignore, Types: []fswatcher.EventType{fswatcher.EventMod}})
	w.asyncWork.Wait()
	assert.True(t, w.isExcluded(generated), "the new rule applies without a restart")
	assert.True(t, idx.shouldExclude(generated, root, false), "the indexer shares the refreshed rules")
	require.Len(t, scans, 1)
	assert.Equal(t, map[string]struct{}{filepath.Join(root, "sub"): {}}, <-scans)

	writeExcludeFixture(t, ignore, "")
	w.handleEvent(fswatcher.WatchEvent{Path: ignore, Types: []fswatcher.EventType{fswatcher.EventMod}})
	w.asyncWork.Wait()
	assert.False(t, w.isExcluded(generated), "removing the rule re-includes the file")
}
