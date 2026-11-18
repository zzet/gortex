package indexer

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
)

func TestWalkDisk_PrunesDirectories(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"gen/drop.go", "gen/keep.go", "gen/child/a.go", "plain/a.go", "vendor/x.go",
		"sub/drop/x.go", "sub/keep.go",
	} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(path)), "package sample\n")
	}

	for _, tc := range []struct {
		name  string
		scope string
		want  []string
	}{
		{
			name:  "root",
			scope: ".",
			want: []string{
				".", "plain", "plain/a.go", "sub", "sub/keep.go",
			},
		},
		{
			name:  "below_root",
			scope: "sub",
			want:  []string{"sub", "sub/keep.go"},
		},
		{
			name:  "pruned_scope",
			scope: "vendor",
			want:  nil,
		},
		{
			name:  "inside_pruned_scope",
			scope: "gen/child",
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newAdmissionTestIndexer(t, root, "gen/", "!gen/keep.go", "!gen/child/", "sub/drop/")
			var visited []string
			err := idx.walkDisk(root, filepath.Join(root, tc.scope), func(path string, d fs.DirEntry, err error) error {
				require.NoError(t, err)
				require.NotNil(t, d)
				rel, relErr := filepath.Rel(root, path)
				require.NoError(t, relErr)
				visited = append(visited, filepath.ToSlash(rel))
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, visited)
		})
	}
}

func TestWalkDisk_SkipsGitFolders(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		".git/HEAD", ".git/objects/ab/cdef", "a.go", "sub/.git/config", "sub/b.go", "up/.GIT/config",
	} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(path)), "package sample\n")
	}
	// A linked worktree's .git is a file that points at the repository; it is not skipped.
	writeExcludeFixture(t, filepath.Join(root, "wt", ".git"), "gitdir: ../.git/worktrees/wt\n")

	for _, tc := range []struct {
		name  string
		scope string
		want  []string
	}{
		{name: "root", scope: ".", want: []string{".", "a.go", "sub", "sub/b.go", "up", "wt", "wt/.git"}},
		{name: "below_root", scope: "sub", want: []string{"sub", "sub/b.go"}},
		{name: "git_scope", scope: ".git", want: nil},
		{name: "inside_git", scope: ".git/objects", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A wildcard-led negation must not open Git metadata folders.
			idx := newAdmissionTestIndexer(t, root, "!*.xcodeproj/x")
			var visited []string
			err := idx.walkDisk(root, filepath.Join(root, tc.scope), func(path string, d fs.DirEntry, err error) error {
				require.NoError(t, err)
				rel, relErr := filepath.Rel(root, path)
				require.NoError(t, relErr)
				visited = append(visited, filepath.ToSlash(rel))
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, visited)
		})
	}
}

func TestWalkDisk_CallbackResult(t *testing.T) {
	root := t.TempDir()
	writeExcludeFixture(t, filepath.Join(root, "a", "child.go"), "package sample\n")
	writeFile(t, filepath.Join(root, "b.go"), "package sample\n")
	callbackErr := errors.New("callback failed")

	for _, tc := range []struct {
		name    string
		result  error
		want    []string
		wantErr error
	}{
		{"SkipDir", filepath.SkipDir, []string{".", "a", "b.go"}, nil},
		{"SkipAll", filepath.SkipAll, []string{".", "a"}, nil},
		{"custom_error", callbackErr, []string{".", "a"}, callbackErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newAdmissionTestIndexer(t, root)
			var visited []string
			err := idx.walkDisk(root, root, func(path string, d fs.DirEntry, err error) error {
				require.NoError(t, err)
				require.NotNil(t, d)
				rel, relErr := filepath.Rel(root, path)
				require.NoError(t, relErr)
				visited = append(visited, filepath.ToSlash(rel))
				if rel == "a" {
					return tc.result
				}
				return nil
			})
			if tc.wantErr != nil {
				require.Same(t, tc.wantErr, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, visited)
		})
	}
}

func TestWalkDisk_MissingScope(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "missing")
	idx := newAdmissionTestIndexer(t, root)
	calls := 0
	var walkErr error
	err := idx.walkDisk(root, scope, func(path string, d fs.DirEntry, err error) error {
		calls++
		require.Equal(t, scope, path)
		require.Nil(t, d)
		require.ErrorIs(t, err, fs.ErrNotExist)
		walkErr = err
		return err
	})
	require.Equal(t, 1, calls)
	require.Same(t, walkErr, err)
}

func TestWalkDisk_DirectoryReadError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not force a read error on Windows or as root")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	writeExcludeFixture(t, filepath.Join(locked, "child.go"), "package sample\n")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })

	idx := newAdmissionTestIndexer(t, root)
	callbackErr := errors.New("directory read failed")
	var visited []string
	err := idx.walkDisk(root, root, func(path string, d fs.DirEntry, err error) error {
		require.NotNil(t, d)
		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		if err != nil {
			require.Equal(t, locked, path)
			require.True(t, d.IsDir())
			require.ErrorIs(t, err, fs.ErrPermission)
			visited = append(visited, filepath.ToSlash(rel)+":error")
			return callbackErr
		}
		visited = append(visited, filepath.ToSlash(rel))
		return nil
	})
	require.Same(t, callbackErr, err)
	require.Equal(t, []string{".", "locked", "locked:error"}, visited)
}

// A re-include under .git no longer reaches the walk, so each one is
// reported when the ignore list is compiled.
func TestExcludeMatcher_WarnsAboutGitReincludes(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	idx := newAdmissionTestIndexer(t, t.TempDir())
	idx.logger = zap.New(core)
	idx.SetExcludePatterns(append(append([]string{}, excludes.Builtin...),
		"!.git/hooks/", "!/.GIT/info/", "!**/.git/config", "!.git/hooks/"))

	var warned []string
	for _, entry := range logs.All() {
		warned = append(warned, entry.ContextMap()["pattern"].(string))
	}
	assert.Equal(t, []string{"!.git/hooks/", "!/.GIT/info/", "!**/.git/config"}, warned)
}

func TestExcludeMatcher_WarnsAboutBlockedNegations(t *testing.T) {
	if os.Getenv("GORTEX_TEST_BLOCKED_NEGATIONS") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExcludeMatcher_WarnsAboutBlockedNegations$")
		cmd.Env = append(os.Environ(), "GORTEX_TEST_BLOCKED_NEGATIONS=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	for _, tc := range []struct {
		name                   string
		patterns               []string
		warned, parents, rules []string
	}{
		{"closed_agent", []string{"*", "!/agent/skills/**"}, []string{"!/agent/skills/**"}, []string{"agent"}, []string{"*"}},
		{"closed_dependency", []string{"node_modules/", "!node_modules/@org/**"}, []string{"!node_modules/@org/**"}, []string{"node_modules"}, []string{"node_modules/"}},
		{"unrooted_under_root_only_exclude", []string{"/foo/", "!foo/keep.go"}, []string{"!foo/keep.go"}, nil, nil},
		{"rooted_under_root_only_exclude", []string{"/foo/", "!/foo/keep.go"}, []string{"!/foo/keep.go"}, []string{"foo"}, []string{"/foo/"}},
		{"reopen_self", []string{"build/", "!build/"}, nil, nil, nil},
		{"open_child", []string{"dist/*", "!dist/keep/"}, nil, nil, nil},
		{"reopen_every_parent", []string{"/*", "!/agent/", "/agent/*", "!/agent/skills/"}, nil, nil, nil},
		{"later_parent_reopened", []string{"*", "!/agent/skills/**", "!/agent/"}, nil, nil, nil},
		{"later_parent_closed", []string{"*", "!/agent/", "!/agent/skills/**", "agent/"}, []string{"!/agent/skills/**"}, []string{"agent"}, []string{"agent/"}},
		{"deeper_parent", []string{"agent/*", "!/agent/skills/deep/**"}, []string{"!/agent/skills/deep/**"}, []string{"agent/skills"}, []string{"agent/*"}},
		{"duplicate", []string{"*", "!/agent/skills/**", "!/agent/skills/**"}, []string{"!/agent/skills/**"}, []string{"agent"}, []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			idx := &Indexer{rootPath: t.TempDir(), config: config.IndexConfig{Exclude: tc.patterns}, logger: zap.New(core)}
			// One lazy build, no repeated per-path warnings.
			idx.excludeMatcher()
			idx.excludeMatcher()
			var warned, parents, rules []string
			for _, entry := range logs.All() {
				warned = append(warned, entry.ContextMap()["pattern"].(string))
				if _, ok := entry.ContextMap()["parent"]; !ok {
					require.Contains(t, entry.Message, "negations no longer reach inside excluded folders")
					continue
				}
				parents = append(parents, entry.ContextMap()["parent"].(string))
				rules = append(rules, entry.ContextMap()["excluded_by"].(string))
				require.Contains(t, entry.Message, "reopen each parent folder")
				require.Contains(t, entry.Message, "!/agent/")
				require.Contains(t, entry.Message, "/agent/*")
				require.Contains(t, entry.Message, "!/agent/skills/")
			}
			require.Equal(t, tc.warned, warned)
			require.Equal(t, tc.parents, parents)
			require.Equal(t, tc.rules, rules)
			// All diagnostics are keyed by repository and pattern across reloads.
			firstBuild := logs.Len()
			idx.SetExcludePatterns(tc.patterns)
			require.Equal(t, firstBuild, logs.Len())
		})
	}
}

func TestExcludeMatcher_WarnsBroadNegationsOnce(t *testing.T) {
	if os.Getenv("GORTEX_TEST_BROAD_NEGATIONS") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExcludeMatcher_WarnsBroadNegationsOnce$")
		cmd.Env = append(os.Environ(), "GORTEX_TEST_BROAD_NEGATIONS=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	core, logs := observer.New(zap.WarnLevel)
	for _, pattern := range []string{"!/keep.go", "!/*.proto"} {
		idx := &Indexer{config: config.IndexConfig{Exclude: []string{"gen/", pattern}}, logger: zap.New(core)}
		idx.excludeMatcher()
	}
	assert.Zero(t, logs.Len(), "root-level targets have no parent that can block them")
	logs.TakeAll()
	patterns := []string{"!*.proto", "!.gitkeep", "!**/@org/**", "!src/**/*.proto"}
	repos := []string{t.TempDir(), t.TempDir()}
	for _, repo := range repos {
		for _, pattern := range patterns {
			for range 2 {
				idx := &Indexer{rootPath: repo, config: config.IndexConfig{Exclude: []string{"gen/", pattern, pattern}}, logger: zap.New(core)}
				idx.excludeMatcher()
				idx.SetExcludePatterns(idx.config.Exclude)
				idx.warnExcludedNegationsAt(excludes.New([]string{"gen/", pattern}), "sub")
			}
		}
	}
	require.Equal(t, 4*len(patterns), logs.Len(), "one warning per repo, ignore directory and pattern, including across indexer rebuilds")
	for i, entry := range logs.All() {
		require.Equal(t, repos[i/(2*len(patterns))], entry.ContextMap()["repo"])
		require.Equal(t, patterns[(i/2)%len(patterns)], entry.ContextMap()["pattern"])
		require.Contains(t, entry.Message, "negations no longer reach inside excluded folders")
		require.Contains(t, entry.Message, "unanchored")
	}
}

func TestExcludeMatcher_WarnsPerFolderNegations(t *testing.T) {
	for _, filename := range dirIgnoreFiles {
		for _, pattern := range []string{"!gen/keep.go"} {
			t.Run(filename+"/"+pattern, func(t *testing.T) {
				root := t.TempDir()
				writeExcludeFixture(t, filepath.Join(root, "sub", filename), "gen/\n"+pattern+"\n")
				core, logs := observer.New(zap.WarnLevel)
				idx := newAdmissionTestIndexer(t, root)
				idx.logger = zap.New(core)
				path := filepath.Join(root, "sub/gen/keep.go")
				require.True(t, idx.shouldExcludeRules(path, root, false))
				idx.shouldExcludeRules(path, root, false)
				require.Equal(t, 1, logs.Len())
				require.Equal(t, filepath.Join(root, "sub"), logs.All()[0].ContextMap()["ignore_dir"])
				idx.dirIgnore.Forget(filepath.Join(root, "sub"))
				idx.shouldExcludeRules(path, root, false)
				require.Equal(t, 1, logs.Len(), "the reloaded list must not repeat the warning")
			})
		}
	}
}

func TestExcludeMatcher_AgentNegationAdvice(t *testing.T) {
	for _, patterns := range [][]string{
		{"!.claude/skills/"}, {"!.claude/", ".claude/*", "!.claude/skills/"},
	} {
		core, logs := observer.New(zap.WarnLevel)
		idx := newAdmissionTestIndexer(t, t.TempDir())
		idx.logger = zap.New(core)
		idx.SetExcludePatterns(append(append([]string{}, excludes.Builtin...), patterns...))
		require.Equal(t, 1, logs.Len())
		require.Contains(t, logs.All()[0].Message, "agent state is never indexed")
		require.NotContains(t, logs.All()[0].Message, "reopen")
	}
}

func TestExcludeMatcher_WarnsUnanchoredIncludes(t *testing.T) {
	if os.Getenv("GORTEX_TEST_UNANCHORED_INCLUDES") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExcludeMatcher_WarnsUnanchoredIncludes$")
		cmd.Env = append(os.Environ(), "GORTEX_TEST_UNANCHORED_INCLUDES=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	core, logs := observer.New(zap.InfoLevel)
	idx := &Indexer{rootPath: t.TempDir(), config: config.IndexConfig{Include: []string{"vendor/acme/"}}, logger: zap.New(core)}
	idx.excludeMatcher()
	require.Equal(t, 1, logs.Len(), "inner-slash anchoring gets one notice")
	idx.SetIgnorePatterns(nil, []string{"Pods/"})
	require.Equal(t, 2, logs.Len(), "a basename literal gets a parent-rule Info notice")
	idx.SetIgnorePatterns(nil, []string{"*.proto"})
	require.Equal(t, 3, logs.Len())
	require.Zero(t, logs.FilterLevelExact(zap.WarnLevel).Len())
	require.Equal(t, "vendor/acme/", logs.All()[0].ContextMap()["pattern"])
	require.Equal(t, "Pods/", logs.All()[1].ContextMap()["pattern"])
	require.Equal(t, "*.proto", logs.All()[2].ContextMap()["pattern"])
	require.Contains(t, logs.All()[1].Message, "cannot reach through excluded ancestors")
	require.Contains(t, logs.All()[2].Message, "cannot reach through excluded ancestors")
}

func TestExcludeMatcher_SkipsOpenParentWarnings(t *testing.T) {
	for _, tc := range []struct {
		name             string
		exclude, include []string
	}{
		{"root_negation_literal", []string{"gen/", "!/keep.go"}, nil},
		{"root_negation_glob", []string{"gen/", "!/*.proto"}, nil},
		{"root_include_glob", []string{"gen/"}, []string{"/*.proto"}},
		{"open_negation_parents", []string{"*", "!/agent/", "!/agent/skills/**"}, nil},
		{"later_open_negation_parents", []string{"*", "!/agent/skills/**", "!/agent/"}, nil},
		{"open_include_parents", []string{"*.go"}, []string{"src/*.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("GORTEX_TEST_OPEN_PARENT_WARNING") != tc.name {
				cmd := exec.Command(os.Args[0], "-test.run=^TestExcludeMatcher_SkipsOpenParentWarnings$/^"+tc.name+"$")
				cmd.Env = append(os.Environ(), "GORTEX_TEST_OPEN_PARENT_WARNING="+tc.name)
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
				return
			}
			core, logs := observer.New(zap.WarnLevel)
			idx := &Indexer{rootPath: t.TempDir(), config: config.IndexConfig{Exclude: tc.exclude, Include: tc.include}, logger: zap.New(core)}
			idx.excludeMatcher()
			require.Zero(t, logs.Len())
		})
	}
}

func TestWalkDiskOpenParentsDifferential(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"src/main.go", "node_modules/pkg/schema.proto", "node_modules/pkg/lib/a.js",
		"gen/keep/a.go", "gen/drop/a.go", "sub/gen/keep/a.go", "sub/gen/drop/a.go",
		"vendor/acme/lib/a.go", "vendor/other/a.go", ".git/objects/x", ".claude/mcp.json", ".claude/worktrees/x/a.go",
	} {
		writeExcludeFixture(t, filepath.Join(root, rel), "fixture\n")
	}
	writeExcludeFixture(t, filepath.Join(root, "sub", ".gortexignore"), "gen/*\n!gen/keep/\n")
	for _, patterns := range [][]string{
		nil, {"!*.proto"}, walkNegationPatterns,
		{"gen/*", "!gen/keep/"}, {"gen/", "!gen/keep/"},
		{"/**", "!src/"}, {"gen/**/", "!gen/keep/"},
	} {
		for _, includes := range [][]string{nil, {"*.proto"}, {"vendor/acme/lib/"}, {"gen*/"}} {
			idx := newAdmissionTestIndexer(t, root, patterns...)
			idx.SetIgnorePatterns(idx.config.Exclude, includes)
			for _, scope := range []string{root, filepath.Join(root, "gen"), filepath.Join(root, "gen/keep"), filepath.Join(root, "node_modules/pkg"), filepath.Join(root, "sub")} {
				var got, want []string
				err := idx.walkDisk(root, scope, func(path string, d fs.DirEntry, err error) error {
					require.NoError(t, err)
					got = append(got, path)
					return nil
				})
				require.NoError(t, err)
				err = filepath.WalkDir(scope, func(path string, d fs.DirEntry, err error) error {
					require.NoError(t, err)
					if d.IsDir() && idx.shouldPruneDir(path, root) {
						return filepath.SkipDir
					}
					want = append(want, path)
					return nil
				})
				require.NoError(t, err)
				require.Equal(t, want, got, "patterns=%q includes=%q scope=%s", patterns, includes, scope)
			}
		}
	}
}
