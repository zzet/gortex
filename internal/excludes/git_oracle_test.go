package excludes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/zzet/gortex/internal/pathkey"
)

// Git, not a second implementation of its rules, supplies the expected verdicts.
func TestGitIgnoreOracle(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	cases := []struct {
		name     string
		root     string
		nested   map[string]string
		excludes []string
		paths    []string // a trailing slash creates a directory
	}{
		{name: "repeated_trailing_slash_a", root: "a//\n", paths: []string{"a/", "a/x/", "a/x/y.go", "open/", "open/a.go"}},
		{name: "repeated_trailing_slash_root", root: "//\n", paths: []string{"a/", "a/x/", "a/x/y.go", "open/", "open/a.go"}},
		{name: "repeated_trailing_slash_build", root: "build//\n", paths: []string{"build/", "build/x/", "build/x/y.go", "open/", "open/a.go"}},
		{name: "plain", root: "dir/\n", paths: []string{"dir/", "dir/sub/", "dir/sub/a.go", "open/a.go", "else/dir/a.go"}},
		{name: "excluded_parent", root: "dir/\n!dir/keep\n", paths: []string{"dir/", "dir/keep", "dir/drop"}},
		{name: "open_child", root: "dir/*\n!dir/keep/\n", paths: []string{"dir/", "dir/keep/", "dir/keep/sub/a.go", "dir/drop/", "dir/drop/a.go", "other/dir/drop/a.go"}},
		{name: "deep_blanket", root: "a/b/*\n!a/b/keep/\n", paths: []string{"a/", "a/b/", "a/b/keep/", "a/b/keep/a.go", "a/b/drop/a.go"}},
		{name: "globstar", root: "**/dir/\n!**/dir/keep.go\n", paths: []string{"dir/keep.go", "x/dir/", "x/dir/keep.go", "x/else/a.go"}},
		{name: "builtin_allow_list", root: strings.Join(Builtin, "\n") + "\n/*\n!/agent/\n/agent/*\n!/agent/skills/\n", paths: []string{"agent/", "agent/skills/", "agent/skills/keep.go", "agent/skills/node_modules/pkg/index.js", "agent/skills/dist/bundle.js", "agent/skills/scratch.tmp"}},
		{name: "bare_globstar", root: "/**\n", paths: []string{"root.go", "dir/", "dir/deep.go"}},
		{name: "terminal_directory_globstar", root: "foo/**/\n", paths: []string{"foo/", "foo/direct.go", "foo/sub/", "foo/sub/deep.go"}},
		{name: "globstar_contents", root: "dir/**\n!dir/keep/\n", paths: []string{"dir/", "dir/keep/", "dir/keep/a.go", "dir/drop/a.go"}},
		{name: "globstar_directory_parent", root: "dir/**/\n!dir/keep/\n", paths: []string{"dir/", "dir/keep/", "dir/drop/"}},
		{name: "globstar_middle", root: "a/**/b/*\n!a/**/b/keep/\n", paths: []string{"a/b/", "a/b/keep/a.go", "a/x/b/keep/a.go", "a/x/b/drop/a.go"}},
		{name: "wildcard_negations", root: "node_modules/\n!*.proto\n!.gitkeep\n!**/keep.go\n", paths: []string{"node_modules/", "node_modules/x/y.proto", "node_modules/.gitkeep", "node_modules/sub/keep.go", "open/y.proto", "open/.gitkeep"}},
		{name: "anchored", root: "/dir/\n!/dir/keep.go\n", paths: []string{"dir/", "dir/keep.go", "x/dir/keep.go"}},
		{name: "reopen_folder", root: "build/\n!build/\n", paths: []string{"build/", "build/keep.go", "x/build/a.go"}},
		{name: "nested_ignored", root: "dir/\n!dir/keep.go\n", nested: map[string]string{"dir/.gitignore": "!keep.go\n!sub/\n"}, paths: []string{"dir/", "dir/keep.go", "dir/sub/a.go"}},
		{name: "nested_override", root: "*.log\n", nested: map[string]string{"sub/.gitignore": "!keep.log\ndir/*\n!dir/keep/\n"}, paths: []string{"drop.log", "sub/keep.log", "sub/drop.log", "sub/dir/", "sub/dir/keep/a.go", "sub/dir/drop/a.go"}},
		{name: "escaped_bang", root: "\\!literal/\n!*.proto\n", paths: []string{"!literal/", "!literal/a.proto", "literal/a.proto"}},
		{name: "spaces", root: "dir/   \n!dir/keep.go   \nspace\\ \n", paths: []string{"dir/", "dir/keep.go", "space ", "space", "open/a.go"}},
		{name: "case", root: "Dir/\n!dir/keep.go\n", paths: []string{"Dir/keep.go", "dir/keep.go", "other/Dir/a.go"}},
		{name: "source_precedence", root: "keep.go\n", excludes: []string{"!keep.go"}, paths: []string{"keep.go", "other.go"}},
		{name: "gortex_excludes", excludes: []string{"generated/", "!*.proto", "!generated/keep.go"}, paths: []string{"generated/", "generated/a.proto", "generated/keep.go", "src/a.proto"}},
	}
	// These pre-existing dependency differences are outside this prototype.
	// Keep asserting them so the oracle never silently treats them as Git.
	knownDifferences := map[string]map[string]string{
		"builtin_allow_list":          {"agent/skills/node_modules/pkg/index.js": "folder negation propagates over Builtin node_modules exclusion", "agent/skills/dist/bundle.js": "folder negation propagates over Builtin dist exclusion", "agent/skills/scratch.tmp": "folder negation propagates over Builtin file exclusion"},
		"bare_globstar":               {"root.go": "go-gitignore's bare /** file regexp does not match root files", "dir/deep.go": "bare /** does not propagate to file descendants without the parent loop"},
		"terminal_directory_globstar": {"foo/sub/deep.go": "terminal **/ does not propagate its folder match to file descendants"},
		"open_child":                  {"other/dir/drop/a.go": "slash-bearing patterns are not anchored by go-gitignore"},
		"globstar_contents":           {"dir/keep/a.go": "go-gitignore propagates a folder negation to descendants even under /**"},
		"nested_override":             {"sub/keep.log": "Hierarchical gives an outer exclusion priority over an inner negation"},
		"spaces":                      {"space ": "existing TrimSpace loses an escaped final space"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			run := func(args ...string) {
				t.Helper()
				cmd := exec.Command(git, args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			run("init", "-q")
			run("config", "core.ignoreCase", "false")
			run("config", "core.excludesFile", filepath.Join(root, "no-global-excludes"))
			write := func(rel, body string) {
				t.Helper()
				p := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(".gitignore", tc.root)
			for path, body := range tc.nested {
				write(path, body)
			}
			write(".git/info/exclude", strings.Join(tc.excludes, "\n"))
			flat := New(tc.excludes)
			hierarchical := NewHierarchical(root, ".gitignore")
			for _, rel := range tc.paths {
				isDir := strings.HasSuffix(rel, "/")
				p := filepath.Join(root, filepath.FromSlash(rel))
				if isDir {
					if err := os.MkdirAll(p, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					write(rel, "fixture\n")
				}
			}
			for _, rel := range tc.paths {
				isDir := strings.HasSuffix(rel, "/")
				p := filepath.Join(root, filepath.FromSlash(rel))
				cmd := exec.Command(git, "check-ignore", "--no-index", "-q", "--", strings.TrimSuffix(rel, "/"))
				cmd.Dir = root
				err := cmd.Run()
				want := err == nil
				if err != nil {
					if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
						t.Fatalf("git check-ignore %q: %v", rel, err)
					}
				}
				got := flat.MatchAbsDir(p, root, isDir) || hierarchical.Match(p, isDir)
				assertParentOrderParity(t, flat, rel)
				for dir, matcher := range hierarchical.cache {
					relative, err := filepath.Rel(dir, p)
					if err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
						if isDir {
							relative += "/"
						}
						assertParentOrderParity(t, matcher, relative)
					}
				}
				if reason := knownDifferences[tc.name][rel]; reason != "" {
					t.Logf("known difference for %q: %s (Gortex=%v, Git=%v)", rel, reason, got, want)
					if got == want {
						t.Errorf("known difference disappeared; update the oracle: %q", rel)
					}
					continue
				}
				if got != want {
					t.Errorf("%q: Gortex excluded=%v, git excluded=%v", rel, got, want)
				}
			}
		})
	}
}

// The pinned dependency already propagates a positive folder exclusion.
// This is why Matcher can omit ancestor checks when no negation is present.
func TestMatcherPositiveOnlyDescendants(t *testing.T) {
	for _, pattern := range []string{"dir/", "**/dir/", "/dir/", "a/b/*", "a/b/**"} {
		raw := ignore.CompileIgnoreLines(literalizePattern(pattern))
		m := New([]string{pattern})
		path := "dir/sub/a.go"
		if strings.HasPrefix(pattern, "a/") {
			path = "a/b/sub/a.go"
		}
		if !raw.MatchesPath(path) || !m.MatchRel(path) {
			t.Errorf("%q must exclude %q with and without the wrapper", pattern, path)
		}
	}
}

// Keep the pre-optimization order as a small independent reference.
func assertParentOrderParity(t *testing.T, m *Matcher, path string) {
	t.Helper()
	rel := strings.TrimPrefix(pathkey.Normalize(filepath.ToSlash(path)), "./")
	if rel == "" || rel == "." {
		return
	}
	want, wantRule := false, ""
	if m != nil {
		isDir := strings.HasSuffix(rel, "/")
		node := strings.TrimSuffix(rel, "/")
		if m.hasNegation {
			for i := 0; i < len(node); i++ {
				if node[i] == '/' {
					if matched, ip := m.dirs.MatchesPathHow(node[:i]); matched {
						want, wantRule = m.explainPattern(matched, ip)
						break
					}
				}
			}
		}
		if !want {
			ign := m.ign
			if isDir {
				ign = m.dirs
			}
			matched, ip := ign.MatchesPathHow(node)
			want, wantRule = m.explainPattern(matched, ip)
		}
	}
	if got, rule := m.Explain(path); got != want || rule != wantRule {
		t.Errorf("old/new parent order differs for %q: new=(%v,%q), old=(%v,%q)", path, got, rule, want, wantRule)
	}
	if got := m.MatchRel(path); got != want {
		t.Errorf("MatchRel parity for %q: got %v, want %v", path, got, want)
	}
}

func TestMatcherParentOrderEdgeCases(t *testing.T) {
	for _, patterns := range [][]string{
		{"/**", "!keep.go"}, {"foo/**/", "!keep.go"}, {"a//", "!keep.go"}, {"//", "!keep.go"}, {"build//", "!keep.go"}, {"!keep.go", "foo[!a]"},
		{"foo/*", "!foo/keep/"}, {"a/b/**", "!a/b/keep/"},
		append(append([]string{}, Builtin...), "/**", "!keep.go"),
		append(append([]string{}, Builtin...), "foo/**/", "!keep.go"),
		append(append([]string{}, Builtin...), "*.log", "!keep.log", ".env", "!.env.example"),
	} {
		m := New(patterns)
		for _, path := range []string{"a/", "a/x/", "a/x/y.go", "build/", "build/x/", "build/x/y.go", "foo/", "foo/bar/", "foo/keep.go", "foo/bar/keep.go", "a/b/", "a/b/keep/a.go", "internal/indexer/builder/sub/deep/file_build.go"} {
			assertParentOrderParity(t, m, path)
		}
	}
}
