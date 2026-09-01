package excludes

import "testing"

// The regression this whole file exists for: pandas' .gitignore carries
// "*$" on line 7 as an Emacs autosave pattern. go-gitignore used to
// compile it into `^(|.*/)([^/]*)$(|/.*)$` — the "$" read as a regexp
// anchor — so every path in the repository matched and `gortex track`
// produced an index holding zero source files, with no error anywhere
// (#624).
func TestMatcher_RegexAnchorIsLiteralText(t *testing.T) {
	for _, pattern := range []string{"*$", "^*"} {
		m := New([]string{pattern})
		for _, p := range []string{
			"pandas/core/frame.py",
			"pandas/core/",
			"pyproject.toml",
			"README.md",
		} {
			if m.MatchRel(p) {
				t.Errorf("pattern %q must not exclude %q", pattern, p)
			}
		}
	}
	// The patterns still do their actual job.
	if !New([]string{"*$"}).MatchRel("core.py$") {
		t.Error(`"*$" should still exclude a file whose name ends in "$"`)
	}
	if !New([]string{"^*"}).MatchRel("^scratch") {
		t.Error(`"^*" should still exclude a file whose name starts with "^"`)
	}
}

// Every regexp-only metacharacter is ordinary text to git. Before the
// fix each one leaked an operator into the compiled pattern.
func TestMatcher_RegexOnlyMetacharactersAreLiteral(t *testing.T) {
	cases := []struct {
		pattern  string
		excluded string   // the file the pattern actually names
		admitted []string // files a leaked regexp operator would have caught
	}{
		{"a|b", "a|b", []string{"a", "b"}},
		{"foo+", "foo+", []string{"foo", "fooo"}},
		{"(tmp)", "(tmp)", []string{"tmp"}},
		{"a{1,2}", "a{1,2}", []string{"a", "aa"}},
		{"gen.d", "gen.d", []string{"genxd"}},
		{"cache]", "cache]", []string{"cache"}},
	}
	for _, tc := range cases {
		m := New([]string{tc.pattern})
		if !m.MatchRel(tc.excluded) {
			t.Errorf("pattern %q should exclude %q", tc.pattern, tc.excluded)
		}
		for _, p := range tc.admitted {
			if m.MatchRel(p) {
				t.Errorf("pattern %q must not exclude %q", tc.pattern, p)
			}
		}
	}
}

// An unbalanced bracket or paren used to make regexp.Compile fail;
// go-gitignore discarded the error and dropped the line, so the ignore
// silently stopped applying. Both must now be honoured as literal text.
func TestMatcher_UncompilablePatternsStillApply(t *testing.T) {
	cases := []struct{ pattern, excluded string }{
		{"lib[", "lib["},
		{"build(", "build("},
		{"a)b", "a)b"},
		{"weird\\", "weird\\"},
	}
	for _, tc := range cases {
		if !New([]string{tc.pattern}).MatchRel(tc.excluded) {
			t.Errorf("pattern %q should exclude %q", tc.pattern, tc.excluded)
		}
	}
}

// "?" is a single-character wildcard in gitignore. go-gitignore escaped
// it into a literal "?", which under-matched: pandas' "Icon?" never
// caught the macOS "Icon\r" file it was written for.
func TestMatcher_QuestionMarkIsSingleCharWildcard(t *testing.T) {
	m := New([]string{"Icon?"})
	for _, p := range []string{"Icon1", "IconX", "sub/IconZ"} {
		if !m.MatchRel(p) {
			t.Errorf(`"Icon?" should exclude %q`, p)
		}
	}
	// A wildcard never crosses a path separator, and never matches the
	// empty string.
	for _, p := range []string{"Icon", "Icon/x"} {
		if m.MatchRel(p) {
			t.Errorf(`"Icon?" must not exclude %q`, p)
		}
	}
}

func TestMatcher_BracketExpressions(t *testing.T) {
	cases := []struct {
		pattern  string
		excluded []string
		admitted []string
	}{
		// pandas' compiled-Python pattern: .pyc / .pyo / .pyd, never .py.
		{"*.py[ocd]", []string{"a.pyc", "a.pyo", "pkg/a.pyd"}, []string{"a.py", "a.pyx"}},
		// git spells class negation with "!"; the regexp engine wants "^".
		{"[!a-z]*", []string{"Build", "9lives"}, []string{"alpha", "zeta"}},
		{"[^a-z]*", []string{"Build"}, []string{"alpha"}},
		// A "*" inside brackets is literal text on both sides.
		{"x[*?]y", []string{"x*y", "x?y"}, []string{"xay", "xy"}},
		// pandas' Emacs lock pattern.
		{"[#]*#", []string{"#frame.py#"}, []string{"frame.py"}},
	}
	for _, tc := range cases {
		m := New([]string{tc.pattern})
		for _, p := range tc.excluded {
			if !m.MatchRel(p) {
				t.Errorf("pattern %q should exclude %q", tc.pattern, p)
			}
		}
		for _, p := range tc.admitted {
			if m.MatchRel(p) {
				t.Errorf("pattern %q must not exclude %q", tc.pattern, p)
			}
		}
	}
}

// Escapes and the "**" forms are go-gitignore's own business; the
// rewrite must leave them working.
func TestMatcher_EscapesAndGlobstarSurvive(t *testing.T) {
	if m := New([]string{`\*.log`}); !m.MatchRel("*.log") || m.MatchRel("app.log") {
		t.Error(`"\*.log" should name the literal file "*.log" only`)
	}
	m := New([]string{"docs/**/build"})
	if !m.MatchRel("docs/a/b/build") {
		t.Error(`"docs/**/build" should exclude "docs/a/b/build"`)
	}
	if m.MatchRel("other/a/build") {
		t.Error(`"docs/**/build" must not exclude "other/a/build"`)
	}
	if n := New([]string{"!keep$"}); !n.HasNegatedDescendant("build") {
		t.Error("a negation must still register after the rewrite")
	}
}

// Explain reports the pattern in the caller's wording, not the rewritten
// form New hands the compiler.
func TestMatcher_ExplainNamesOriginalPattern(t *testing.T) {
	m := New([]string{"node_modules/", "*$", "dist/"})
	matched, pattern := m.Explain("pandas/core/frame.py")
	if matched {
		t.Fatalf("nothing should match after the fix, got pattern %q", pattern)
	}
	matched, pattern = m.Explain("node_modules/x/index.js")
	if !matched || pattern != "node_modules/" {
		t.Fatalf("Explain = (%v, %q), want (true, \"node_modules/\")", matched, pattern)
	}
	var nilm *Matcher
	if matched, pattern = nilm.Explain("a"); matched || pattern != "" {
		t.Error("nil matcher should explain nothing")
	}
}

func TestLiteralizePattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"src/main.go", "src/main.go"},
		{"*$", `*\$`},
		{"!keep$", `!keep\$`},
		{"a|b", `a\|b`},
		{"?", "[^/]"},
		{"[!a-z]", "[^a-z]"},
		{"[abc]", "[abc]"},
		{"lib[", `lib\[`},
		{`\*`, `\*`},
		{`trail\`, `trail\\`},
		{"x[*]y", `x[\*]y`},
	}
	for _, tc := range cases {
		if got := literalizePattern(tc.in); got != tc.want {
			t.Errorf("literalizePattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
