package excludes

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMatcher_SubtreeCoverage checks the directory spelling used by the walk
// and descendants separately: a directory match alone does not prove coverage.
func TestMatcher_SubtreeCoverage(t *testing.T) {
	cases := []struct {
		pattern, dir string
		covers       bool
	}{
		{"*", "foo", true}, {"**", "foo", true}, {"/**", "foo", false},
		{"node_modules/", "x/node_modules", true}, {".venv/", "x/.venv", true},
		{"/agent/plans/", "agent/plans", true}, {"**/node_modules/", "x/node_modules", true},
		{"*.log", "x/foo.log", true}, {"foo/*", "foo", true}, {"a/b/*", "a/b", true},
		{"cache/", "x/cache", true}, {"obj/[Dd]ebug/", "obj/Debug", true},
		{".Trash-*/", "x/.Trash-1000", true},
		{"foo[!a]", "foo", false}, {`foo\x2f`, "foo", false},
		{"foo/**/", "foo", false}, {"foo/**", "foo", true},
		{"foo//**", "foo", false},
	}
	// Give every builtin a directory witness, even the file-like entries.
	for _, pattern := range Builtin {
		dir := strings.TrimSuffix(pattern, "/")
		dir = strings.TrimPrefix(dir, "**/")
		dir = strings.ReplaceAll(dir, "[Dd]", "D")
		dir = strings.ReplaceAll(dir, "[Rr]", "R")
		dir = strings.ReplaceAll(dir, "*", "sample")
		cases = append(cases, struct {
			pattern, dir string
			covers       bool
		}{pattern, dir, true})
	}
	root := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			m := New([]string{tc.pattern})
			if !m.MatchAbsDir(filepath.Join(root, tc.dir), root, true) || !m.MatchRel(tc.dir+"/") {
				t.Fatal("directory witness not excluded")
			}
			all := true
			for _, tail := range []string{"file.go", "x/y/z.go", "a", ".hidden", "child/", "child/deep/", "é/日本語.txt"} {
				if !m.MatchRel(tc.dir + "/" + tail) {
					all = false
				}
			}
			if all != tc.covers {
				t.Errorf("all descendants excluded=%v, want %v", all, tc.covers)
			}
			// A proven covering rule after a negation must close this subtree.
			if tc.covers {
				ordered := New([]string{"!**/keep.go", tc.pattern})
				if ordered.HasNegatedDescendant(tc.dir) {
					t.Error("later covering exclude did not supersede negation")
				}
			}
		})
	}
}

func TestMatcher_HasNegatedDescendant_Order(t *testing.T) {
	prime := []string{"*", "!/agent/", "!/research/", "*", "!/agent/extensions/**", "!/agent/skills/**", "!/agent/AGENTS.md", ".venv/", "node_modules/"}
	cases := []struct {
		name     string
		patterns []string
		dir      string
		want     bool
	}{
		{"prime session artifacts", prime, "agent/session-artifacts", false},
		{"prime research", prime, "research/x", false},
		{"prime venv", prime, "agent/skills/x/.venv", false},
		{"prime node modules", prime, "agent/extensions/x/node_modules", false},
		{"prime skills", prime, "agent/skills/x", true},
		{"prime extensions", prime, "agent/extensions/x", true},
		{"exclude then negation", []string{"vendor/", "!vendor/mylib/"}, "vendor", true},
		{"negation then exclude", []string{"!x/", "x/"}, "x", false},
		{"rooted negation then exclude", []string{"!/name", "name/"}, "name", false},
		{"rooted unrelated", []string{"*", "!/name"}, "other", false},
		{"rooted subtree", []string{"*", "!/name"}, "name/deep", true},
		{"rooted wildcard conservative", []string{"*", "!/*.md"}, "other", true},
		{"new negation after covering exclude", []string{"!x/", "x/", "!x/keep/"}, "x", true},
		{"noncovering slash doublestar", []string{"!keep.go", "/**"}, "foo", true},
		{"noncovering terminal doublestar slash", []string{"!keep.go", "foo/**/"}, "foo", true},
		{"noncovering negated class", []string{"!keep.go", "foo[!a]"}, "foo", true},
		{"noncovering escape", []string{"!keep.go", `foo\x2f`}, "foo", true},
		{"noncovering repeated separator", []string{"!keep.go", "foo//**"}, "foo", true},
		{"covering before uncertain rule", []string{"!keep.go", "*", "/**"}, "foo", false},
		{"unrelated later exclude", []string{"*", "!keep.go", "cache/"}, "foo", true},
		{"root remains conservative", []string{"!/name", "*"}, "", true},
		{"normalized directory", []string{"!**/keep", "cache/"}, "./x/cache/", false},
		{"unicode normalized directory", []string{"!keep.go", "café/"}, "cafe\u0301", false},
		{"comments retain order", []string{"# before", "!keep.go", "", "# middle", "cache/"}, "cache", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.patterns)
			if got := m.HasNegatedDescendant(tc.dir); got != tc.want {
				t.Errorf("HasNegatedDescendant(%q)=%v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

func TestHierarchical_HasNegatedDescendant_Order(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"!x/\nx/\n", false}, {"x/\n!x/keep/\n", true},
	} {
		t.Run(tc.content, func(t *testing.T) {
			root := t.TempDir()
			mkIgnore(t, filepath.Join(root, "pkg", ".gortexignore"), tc.content)
			h := NewHierarchical(root, ".gortexignore")
			if got := h.HasNegatedDescendant(filepath.Join(root, "pkg", "x")); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
