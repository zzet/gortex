package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooksLikeGlob(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"pkg/foo", false},
		{"pkg/*.go", true},
		{"*.tmp", true},
		{"dir/", false},
		{"!keep", true},
		{"/abs", false}, // absolute paths handled by normalizePattern
		{"abc?def", true},
		{"[0-9]", true},
	}
	for _, tc := range cases {
		if got := looksLikeGlob(tc.in); got != tc.want {
			t.Errorf("looksLikeGlob(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizePattern_RawGlob(t *testing.T) {
	p, err := normalizePattern("*.log", "/some/repo")
	require.NoError(t, err)
	assert.Equal(t, "*.log", p)
}

func TestNormalizePattern_NoRepoRoot(t *testing.T) {
	// Without a repo root to anchor against, the arg is passed through.
	p, err := normalizePattern("something", "")
	require.NoError(t, err)
	assert.Equal(t, "something", p)
}

func TestNormalizePattern_ExistingDir(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	nested := filepath.Join(root, "pkg", "generated")
	require.NoError(t, os.MkdirAll(nested, 0755))

	t.Chdir(root)
	p, err := normalizePattern("pkg/generated", root)
	require.NoError(t, err)
	assert.Equal(t, "pkg/generated/", p)
}

func TestNormalizePattern_ExistingFile(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	f := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(f, []byte(""), 0644))

	t.Chdir(root)
	p, err := normalizePattern("notes.txt", root)
	require.NoError(t, err)
	assert.Equal(t, "notes.txt", p)
}

func TestNormalizePattern_OutsideRepoRejected(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	outside := resolvePath(t, t.TempDir())
	t.Chdir(outside)
	// Pass an absolute path under 'outside', matched against 'root'.
	_, err := normalizePattern(outside, root)
	require.Error(t, err)
}

func TestNormalizePattern_RepoRootItselfRejected(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	t.Chdir(root)
	_, err := normalizePattern(".", root)
	require.Error(t, err)
}

// resolvePath normalises temp-dir paths on macOS where /var is a symlink
// to /private/var; without this the test assertions flap between runs.
func resolvePath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return resolved
}

func TestFindWorkspaceConfigPath_ExistingFile(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	configPath := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(""), 0644))

	sub := filepath.Join(root, "pkg", "deep")
	require.NoError(t, os.MkdirAll(sub, 0755))
	t.Chdir(sub)

	path, wsRoot, err := findWorkspaceConfigPath()
	require.NoError(t, err)
	assert.Equal(t, configPath, path)
	assert.Equal(t, root, wsRoot)
}

// TestFindWorkspaceConfigPath_StopsAtGitRoot pins the search floor
// (#761): a .gortex.yaml above the enclosing git root belongs to another
// project and must not be adopted — the prospective path is the repo's own.
func TestFindWorkspaceConfigPath_StopsAtGitRoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		makeGit func(t *testing.T, repo string)
	}{
		{"git dir", func(t *testing.T, repo string) {
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0755))
		}},
		// Linked worktrees and submodules carry a .git file, not a dir.
		{"git file", func(t *testing.T, repo string) {
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /elsewhere\n"), 0644))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outer := resolvePath(t, t.TempDir())
			require.NoError(t, os.WriteFile(filepath.Join(outer, ".gortex.yaml"), []byte("exclude:\n  - outer/\n"), 0644))
			repo := filepath.Join(outer, "repo")
			sub := filepath.Join(repo, "pkg", "deep")
			require.NoError(t, os.MkdirAll(sub, 0755))
			tc.makeGit(t, repo)
			t.Chdir(sub)

			path, wsRoot, err := findWorkspaceConfigPath()
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(repo, ".gortex.yaml"), path)
			assert.Equal(t, repo, wsRoot)
		})
	}
}

// TestFindWorkspaceConfigPath_NoGitBoundedToCwd pins the issue's repro:
// outside any git repo the search does not climb past the cwd.
func TestFindWorkspaceConfigPath_NoGitBoundedToCwd(t *testing.T) {
	outer := resolvePath(t, t.TempDir())
	if gitToplevel(outer) != "" {
		t.Skip("temp dir is inside a git checkout; the no-git case cannot be exercised")
	}
	require.NoError(t, os.WriteFile(filepath.Join(outer, ".gortex.yaml"), []byte("exclude:\n  - outer/\n"), 0644))
	sub := filepath.Join(outer, "nested", "deeper")
	require.NoError(t, os.MkdirAll(sub, 0755))
	t.Chdir(sub)

	path, wsRoot, err := findWorkspaceConfigPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(sub, ".gortex.yaml"), path)
	assert.Equal(t, sub, wsRoot)
}

// TestWorkspaceTarget_DoesNotRewriteAncestorConfig drives a save from a
// nested repo and checks the ancestor project's config is left byte-for-byte
// intact while the repo's own .gortex.yaml receives the pattern.
func TestWorkspaceTarget_DoesNotRewriteAncestorConfig(t *testing.T) {
	outer := resolvePath(t, t.TempDir())
	ancestor := "# A comment that must survive.\nexclude:\n  - \"vendor/**\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(outer, ".gortex.yaml"), []byte(ancestor), 0644))
	repo := filepath.Join(outer, "repo")
	sub := filepath.Join(repo, "nested", "deeper")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0755))
	t.Chdir(sub)

	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)
	require.NoError(t, tgt.save([]string{"a/"}))

	got, err := os.ReadFile(filepath.Join(outer, ".gortex.yaml"))
	require.NoError(t, err)
	assert.Equal(t, ancestor, string(got))
	patterns, err := tgt.load()
	require.NoError(t, err)
	assert.Equal(t, []string{"a/"}, patterns)
	assert.FileExists(t, filepath.Join(repo, ".gortex.yaml"))
}

func TestFindWorkspaceConfigPath_GitRootFallback(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	sub := filepath.Join(root, "pkg", "deep")
	require.NoError(t, os.MkdirAll(sub, 0755))
	t.Chdir(sub)

	path, wsRoot, err := findWorkspaceConfigPath()
	require.NoError(t, err)
	// No .gortex.yaml exists, so we get a prospective path at git root.
	assert.Equal(t, filepath.Join(root, ".gortex.yaml"), path)
	assert.Equal(t, root, wsRoot)
}

func TestWorkspaceTarget_AddRemove(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)

	// Initial load: file doesn't exist → nil.
	patterns, err := tgt.load()
	require.NoError(t, err)
	assert.Empty(t, patterns)

	// Save creates the file.
	require.NoError(t, tgt.save([]string{"a/", "b/"}))
	patterns, err = tgt.load()
	require.NoError(t, err)
	assert.Equal(t, []string{"a/", "b/"}, patterns)

	// Round-trip preserves only exclude (legacy keys scrubbed).
	data, err := os.ReadFile(filepath.Join(root, ".gortex.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "exclude:")
	assert.NotContains(t, string(data), "index:")
	assert.NotContains(t, string(data), "watch:")
}

func TestWorkspaceTarget_LegacyFallback(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	// Simulate an existing legacy-shape .gortex.yaml.
	legacyYAML := "index:\n  exclude:\n    - legacy-pat/**\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(legacyYAML), 0644))

	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)
	patterns, err := tgt.load()
	require.NoError(t, err)
	// Legacy key surfaces through the workspace target's load().
	assert.Equal(t, []string{"legacy-pat/**"}, patterns)
}

// runConfigExcludeList writes rows with fmt.Printf; captureStdout
// (savings_test.go) redirects os.Stdout for the assertions below.

func TestRunConfigExcludeList_MalformedWorkspaceAnnotated(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	// The stray-quote shape: a single bad quote makes the whole file
	// unparseable, which previously showed as the workspace layer simply
	// being absent.
	malformed := "exclude:\n  - \"wine-mt4/\"\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(malformed), 0644))

	var errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errOut)

	out := captureStdout(t, func() {
		require.NoError(t, runConfigExcludeList(cmd, nil))
	})

	assert.Contains(t, out, "[workspace]",
		"the workspace layer must be listed even when it loads zero patterns")
	assert.Contains(t, out, "0 patterns loaded — file failed to parse")
}

func TestRunConfigExcludeList_UnknownKeyWarning(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"),
		[]byte("index:\n  ignore:\n    - build/**\n"), 0644))

	var errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errOut)

	_ = captureStdout(t, func() {
		require.NoError(t, runConfigExcludeList(cmd, nil))
	})

	assert.Contains(t, errOut.String(), "keys gortex does not recognize")
	assert.Contains(t, errOut.String(), "index.ignore")
}

// TestRunConfigExcludeList_Agreement pins that the workspace layer's
// reporting agrees with the daemon loader and the init warning on which
// files parse — same acceptance semantics via the shared
// config.ParseWorkspaceFileInto parser. The table mirrors
// TestWorkspaceParseAgreementWithSurfaces (internal/config) and
// TestWarnIfWorkspaceConfigIgnored_Agreement — keep the three in sync.
func TestRunConfigExcludeList_Agreement(t *testing.T) {
	cases := []struct {
		name             string
		body             string
		wantMalformedRow bool
	}{
		{"valid list", "exclude:\n  - vendor/**\n", false},
		// viper's weak decode accepts a scalar exclude; yaml.Unmarshal
		// rejects it — the row must say so instead of the layer silently
		// disappearing.
		{"scalar exclude", "exclude: vendor/\n", true},
		// Schema violations are not parse failures: the file parses, so
		// no failure annotation (and no stderr warning).
		{"schema violation, project+projects", "project: a\nprojects:\n  - name: p\n    paths: [\"x/**\"]\n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
			t.Chdir(root)
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(tc.body), 0644))

			var errOut bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetErr(&errOut)

			out := captureStdout(t, func() {
				require.NoError(t, runConfigExcludeList(cmd, nil))
			})

			if tc.wantMalformedRow {
				assert.Contains(t, out, "[workspace]")
				assert.Contains(t, out, "0 patterns loaded — file failed to parse")
			} else {
				assert.NotContains(t, out, "file failed to parse")
			}
			assert.Empty(t, errOut.String())
		})
	}
}

// addViaWorkspaceTarget mirrors runConfigExcludeAdd's load → append → save
// against the workspace target (without the daemon reload ping).
func addViaWorkspaceTarget(t *testing.T, pattern string) {
	t.Helper()
	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)
	existing, err := tgt.load()
	require.NoError(t, err)
	require.NoError(t, tgt.save(append(existing, pattern)))
}

// TestWorkspaceTarget_SavePreservesUnrelatedContent pins the lossless
// write (#761): only the exclude list changes — comments, key order,
// scalar quoting and keys config.Config drops on a round trip
// (watch.enabled: false, unknown keys) all survive.
func TestWorkspaceTarget_SavePreservesUnrelatedContent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	before := `# A comment that must survive.
index:
  languages:
    - go
  workers: 8 # trailing comment
watch:
  enabled: false
# Excludes below.
exclude:
  - "vendor/**" # keep vendored code out
  - 'node_modules/**'
custom_key: value
`
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(before), 0644))

	addViaWorkspaceTarget(t, "a/")

	got, err := os.ReadFile(filepath.Join(root, ".gortex.yaml"))
	require.NoError(t, err)
	want := `# A comment that must survive.
index:
  languages:
    - go
  workers: 8 # trailing comment
watch:
  enabled: false
# Excludes below.
exclude:
  - "vendor/**" # keep vendored code out
  - 'node_modules/**'
  - 'a/'
custom_key: value
`
	assert.Equal(t, want, string(got))
}

// TestWorkspaceTarget_LegacyMigrationKeepsSiblings is the issue's repro:
// adding to a file that only has the legacy index.exclude / watch.exclude
// migrates the list to top-level exclude: without dropping the comment,
// the other index keys or watch.enabled.
func TestWorkspaceTarget_LegacyMigrationKeepsSiblings(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	before := `# A comment that must survive.
index:
  languages:
    - go
  exclude:
    - "vendor/**"
    - "node_modules/**"
  workers: 8
watch:
  enabled: false
  exclude:
    - "tmp/"
`
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(before), 0644))

	addViaWorkspaceTarget(t, "a/")

	got, err := os.ReadFile(filepath.Join(root, ".gortex.yaml"))
	require.NoError(t, err)
	want := `# A comment that must survive.
index:
  languages:
    - go
  workers: 8
watch:
  enabled: false
exclude:
  - "vendor/**"
  - "node_modules/**"
  - "tmp/"
  - "a/"
`
	assert.Equal(t, want, string(got))
}

// TestWorkspaceTarget_SaveEdgeShapes covers files with no list to edit:
// absent, empty, comment-only, and a null `exclude:` value.
func TestWorkspaceTarget_SaveEdgeShapes(t *testing.T) {
	cases := []struct {
		name   string
		before string // "-" means no file
		want   string
	}{
		{"absent", "-", "exclude:\n  - a/\n"},
		{"empty", "", "exclude:\n  - a/\n"},
		{"comment only", "# just a note\n", "# just a note\nexclude:\n  - a/\n"},
		{"null exclude", "# head\nexclude:\nworkers: 2\n", "# head\nexclude:\n  - a/\nworkers: 2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
			t.Chdir(root)
			path := filepath.Join(root, ".gortex.yaml")
			if tc.before != "-" {
				require.NoError(t, os.WriteFile(path, []byte(tc.before), 0644))
			}

			addViaWorkspaceTarget(t, "a/")

			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}
}

// TestWorkspaceTarget_RemoveLastPatternDropsKey checks that emptying the
// list removes the exclude key and leaves every other line alone.
func TestWorkspaceTarget_RemoveLastPatternDropsKey(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	t.Chdir(root)

	before := "# header\nwatch:\n  enabled: false\nexclude:\n  - a/\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(before), 0644))

	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)
	require.NoError(t, tgt.save(nil))

	got, err := os.ReadFile(filepath.Join(root, ".gortex.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "# header\nwatch:\n  enabled: false\n", string(got))
}
