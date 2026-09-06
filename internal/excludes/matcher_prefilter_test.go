package excludes

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMatcherPrefilterPreservesAuthoritativeMatch(t *testing.T) {
	patternSets := [][]string{
		Builtin,
		{"node_modules/", "!node_modules/keep/"},
		{"a/b/*", "**/foo", "foo/**", "/foo/", "*.tmp"},
		{"foo", "*", "!keep"},
		{"foo", "obj/[Dd]ebug/", "[ab]", "?x"},
		{"!foo", "!bar/"},
		{"foo", `\!literal`, `\#literal`, `space\ name`},
		{"café/", "*.réserve", "!café/keep"},
		{"*$", "foo(bar)", "foo+bar", "foo|bar", "foo^bar"},
	}
	paths := []string{
		"", ".", "./", "a/b", "a/b/", "a/b/file", "foo", "foo/", "foo/bar", "a/foo", "a/foo/",
		"assets/logo.sketch", "internal/app/main.go", "node_modules/pkg/file", "node_modules/keep/file",
		"scratch.tmp", "scratch.tmp/child", "keep", "obj/Debug", "obj/Debug/file", "obj/debug/file",
		"a", "b", "ax", "!literal", "#literal", "space name", "café/file", "cafe\u0301/file", "café/keep",
		"doc.re\u0301serve", "foo$", "foo(bar)", "foo+bar", "foo|bar", "foo^bar", "./assets/logo.sketch",
	}
	root := t.TempDir()
	for _, patterns := range patternSets {
		m := New(patterns)
		for _, path := range paths {
			want, _ := m.Explain(path)
			if got := m.MatchRel(path); got != want {
				t.Errorf("patterns=%q path=%q: MatchRel=%v, Explain=%v", patterns, path, got, want)
			}
			abs := filepath.Join(root, filepath.FromSlash(path))
			for _, isDir := range []bool{false, true} {
				want, _ := m.ExplainAbsDir(abs, root, isDir)
				if got := m.MatchAbsDir(abs, root, isDir); got != want {
					t.Errorf("patterns=%q path=%q dir=%v: MatchAbsDir=%v, ExplainAbsDir=%v", patterns, path, isDir, got, want)
				}
			}
		}
	}
}

func FuzzMatcherPrefilterParity(f *testing.F) {
	for _, seed := range []struct{ patterns, path string }{
		{"a/b/*", "a/b"},
		{"foo/**", "foo"},
		{"**/foo", "foo"},
		{"node_modules/\n!node_modules/keep/", "node_modules/keep/file"},
		{"foo\nobj/[Dd]ebug/", "obj/Debug/file"},
		{"café/", "cafe\u0301/file"},
		{"*$", "ordinary"},
	} {
		f.Add(seed.patterns, seed.path)
	}
	f.Fuzz(func(t *testing.T, patterns, path string) {
		if len(patterns) > 1024 || len(path) > 1024 {
			t.Skip()
		}
		m := New(strings.Split(patterns, "\n"))
		want, _ := m.Explain(path)
		if got := m.MatchRel(path); got != want {
			t.Fatalf("patterns=%q path=%q: MatchRel=%v, Explain=%v", patterns, path, got, want)
		}
	})
}
