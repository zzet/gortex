package excludes

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMatcher_Nil(t *testing.T) {
	var m *Matcher
	if m.MatchRel("anything") {
		t.Fatal("nil matcher should never match")
	}
}

func TestMatcher_Builtin(t *testing.T) {
	m := New(Builtin)
	cases := []struct {
		path string
		want bool
	}{
		{".git/HEAD", true},
		{"pkg/.git/HEAD", true},
		{"src/node_modules/foo/index.js", true},
		{"vendor/lib/x.go", true},
		{"pkg/foo.go", false},
		{"README.md", false},
		{"tmp.tmp", true},
		{"deep/nested/file.swp", true},
		{".Makefile.swpx", true},
		{"src/.foo.swo", true},
		{"src/.foo.swn", true},
		{"backup~", true},
	}
	for _, tc := range cases {
		if got := m.MatchRel(tc.path); got != tc.want {
			t.Errorf("MatchRel(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatcher_Negation(t *testing.T) {
	// Keep the parent open, then re-include its keep folder.
	m := New([]string{"dist/*", "!dist/keep/"})
	if !m.MatchRel("dist/junk.txt") {
		t.Error("dist/junk.txt should be excluded")
	}
	if m.MatchRel("dist/keep/foo.txt") {
		t.Error("dist/keep/foo.txt should be re-included")
	}
}

func TestMatcher_CommentsAndEmpty(t *testing.T) {
	m := New([]string{"", "# comment", "foo/"})
	pats := m.Patterns()
	if len(pats) != 1 || pats[0] != "foo/" {
		t.Errorf("expected [foo/], got %v", pats)
	}
}

func TestMatcherParentRuleBoundaries(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		path     string
		excluded bool
		pattern  string
	}{
		{[]string{"café/", "!*.go"}, "cafe\u0301/keep.go", true, "café/"},
		{[]string{"dir/", "!dir/keep.go"}, "./dir/sub/keep.go", true, "dir/"},
		{[]string{"dir/", "!dir/"}, "dir/sub/keep.go", false, ""},
		{[]string{"a/b/*", "!a/b/keep/"}, "a/b/", false, ""},
		{[]string{"a/b/**", "!a/b/keep/"}, "a/b/", false, ""},
		{[]string{"a/b/**/", "!a/b/keep/"}, "a/b/", false, ""},
		{[]string{"dir/", "!*.go"}, "dir/", true, "dir/"},
	} {
		m := New(tc.patterns)
		if got := m.MatchRel(tc.path); got != tc.excluded {
			t.Errorf("MatchRel(%q)=%v, want %v", tc.path, got, tc.excluded)
		}
		if got, pattern := m.Explain(tc.path); got != tc.excluded || pattern != tc.pattern {
			t.Errorf("Explain(%q)=(%v,%q), want (%v,%q)", tc.path, got, pattern, tc.excluded, tc.pattern)
		}
	}
	var m *Matcher
	root := t.TempDir()
	if m.MatchAbsDir(filepath.Join(root, "dir"), root, true) {
		t.Error("nil directory matcher excludes nothing")
	}
	if got, pattern := m.Explain("dir/"); got || pattern != "" {
		t.Error("nil matcher explains nothing")
	}
}

func TestMatcherExcludedNegationParent(t *testing.T) {
	for _, tc := range []struct {
		patterns               []string
		negation, parent, rule string
	}{
		{[]string{"foo/"}, "!foo/bar*/keep.go", "foo", "foo/"},
		{[]string{"/foo/"}, "!foo/keep.go", "", ""},
		{[]string{"/foo/"}, "!/foo/keep.go", "foo", "/foo/"},
		{[]string{"foo/"}, "!foo*/bar/keep.go", "", ""},
		{[]string{"foo/"}, `!foo\*/bar/keep.go`, "", ""},
		{[]string{"*"}, `\!literal/path`, "", ""},
		{[]string{"*"}, "!", "", ""},
		{[]string{"*"}, "!/", "", ""},
		{Builtin, "!.claude/mcp.json", "", ""},
		{Builtin, "!.kiro/settings/mcp.json", "", ""},
		{append(append([]string{}, Builtin...), ".kiro/"), "!.kiro/settings/mcp.json", ".kiro", ".kiro/"},
		{[]string{".kiro/"}, "!.kiro/settings/mcp.json", ".kiro", ".kiro/"},
	} {
		parent, rule := New(tc.patterns).ExcludedNegationParent(tc.negation)
		if parent != tc.parent || rule != tc.rule {
			t.Errorf("%q: got (%q,%q), want (%q,%q)", tc.negation, parent, rule, tc.parent, tc.rule)
		}
	}
	var m *Matcher
	if parent, rule := m.ExcludedNegationParent("!foo/bar"); parent != "" || rule != "" {
		t.Error("nil matcher reports no excluded parent")
	}
}

// Every Builtin line gets its own witness, so another Builtin cannot mask
// a regression in the directory compiler, even for file-like patterns.
func TestMatcherBuiltinDirectoryWitnesses(t *testing.T) {
	root := t.TempDir()
	for _, pattern := range Builtin {
		t.Run(pattern, func(t *testing.T) {
			dir := strings.TrimPrefix(strings.TrimSuffix(pattern, "/"), "**/")
			dir = strings.ReplaceAll(dir, "[Dd]", "D")
			dir = strings.ReplaceAll(dir, "[Rr]", "R")
			dir = strings.ReplaceAll(dir, "*", "sample")
			m := New([]string{pattern})
			if !m.MatchAbsDir(filepath.Join(root, dir), root, true) || !m.MatchRel(dir+"/") {
				t.Fatalf("%q does not exclude directory %q", pattern, dir)
			}
			for _, tail := range []string{"file.go", "x/y/file.go"} {
				if !m.MatchRel(dir + "/" + tail) {
					t.Errorf("%q does not exclude %q", pattern, dir+"/"+tail)
				}
			}
		})
	}
}
