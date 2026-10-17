package excludes

import (
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
	// Exclude all of dist, except dist/keep
	m := New([]string{"dist/", "!dist/keep/**"})
	if !m.MatchRel("dist/junk.txt") {
		t.Error("dist/junk.txt should be excluded")
	}
	if m.MatchRel("dist/keep/foo.txt") {
		t.Error("dist/keep/foo.txt should be re-included")
	}
}

func TestMatcher_HasNegatedDescendant(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		relDir   string
		want     bool
	}{
		{
			// Issue #113: a blanket "dir/*" makes go-gitignore report the
			// parent directory itself as excluded; the re-include lives
			// beneath it, so the parent must keep being descended.
			name:     "blanket plus reinclude under parent",
			patterns: []string{"wp-content/plugins/*", "!wp-content/plugins/foo/"},
			relDir:   "wp-content/plugins",
			want:     true,
		},
		{
			name:     "sibling without reinclude is prunable",
			patterns: []string{"wp-content/plugins/*", "!wp-content/plugins/foo/"},
			relDir:   "wp-content/plugins/other",
			want:     false,
		},
		{
			name:     "unrelated dir is prunable",
			patterns: []string{"wp-content/plugins/*", "!wp-content/plugins/foo/"},
			relDir:   "node_modules",
			want:     false,
		},
		{
			name:     "reinclude two levels down keeps grandparent",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "a",
			want:     true,
		},
		{
			name:     "reinclude two levels down keeps parent",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "a/b",
			want:     true,
		},
		{
			name:     "reinclude two levels down prunes uninvolved sibling",
			patterns: []string{"a/*", "!a/b/c/", "!a/x"},
			relDir:   "a/y",
			want:     false,
		},
		{
			name:     "unanchored basename negation keeps every dir",
			patterns: []string{"build/", "!keep.txt"},
			relDir:   "build",
			want:     true,
		},
		{
			name:     "wildcard-leading negation keeps every dir",
			patterns: []string{"build/", "!**/keep/"},
			relDir:   "build",
			want:     true,
		},
		{
			name:     "no negations never keeps",
			patterns: []string{"build/", "dist/"},
			relDir:   "build",
			want:     false,
		},
		{
			name:     "root-anchored file negation prunes other dirs",
			patterns: []string{"/*", "!/AGENTS.md", ".worktrees/"},
			relDir:   ".worktrees",
			want:     false,
		},
		{
			name:     "root-anchored file negation keeps the root",
			patterns: []string{"/*", "!/AGENTS.md"},
			relDir:   "",
			want:     true,
		},
		{
			name:     "root-anchored dir negation keeps its subtree",
			patterns: []string{"/*", "!/agent/"},
			relDir:   "agent/skills",
			want:     true,
		},
		{
			name:     "root-anchored wildcard negation stays conservative",
			patterns: []string{"/*", "!/*.md"},
			relDir:   ".worktrees",
			want:     true,
		},
		{
			name:     "trailing-slash relDir is tolerated",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "a/b/",
			want:     true,
		},
		{
			name:     "leading-dot-slash relDir is tolerated",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "./a",
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.patterns)
			if got := m.HasNegatedDescendant(tc.relDir); got != tc.want {
				t.Errorf("HasNegatedDescendant(%q) = %v, want %v", tc.relDir, got, tc.want)
			}
		})
	}
}

func TestMatcher_HasNegatedDescendant_Nil(t *testing.T) {
	var m *Matcher
	if m.HasNegatedDescendant("anything") {
		t.Fatal("nil matcher should report no negated descendants")
	}
}

func TestMatcher_CommentsAndEmpty(t *testing.T) {
	m := New([]string{"", "# comment", "foo/"})
	pats := m.Patterns()
	if len(pats) != 1 || pats[0] != "foo/" {
		t.Errorf("expected [foo/], got %v", pats)
	}
}
