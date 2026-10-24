package excludes

import (
	"math/rand"
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

// A no-negation list cannot enter the parent loop. Keep its cheap rejection
// ahead of the full compiled match, even for uncertain compiler forms.
func TestMatcherNoNegationUsesPrefilter(t *testing.T) {
	for _, pattern := range []string{"/**", "foo/**/", `\#*\#`} {
		m := New(append(append([]string{}, Builtin...), pattern))
		const rel = "src/pkg/server/main.go"
		if m.hasNegation || !m.checkUnmatchedParents || m.couldMatch(rel) {
			t.Fatalf("invalid prefilter fixture for %q", pattern)
		}
		// A sentinel full matcher makes an unnecessary evaluation observable
		// without a timing assertion on a loaded test host.
		full := New([]string{"*"})
		m.ign, m.dirs = full.ign, full.dirs
		if m.MatchRel(rel) {
			t.Errorf("%q: file miss reached the full matcher", pattern)
		}
		if m.MatchRelEither(rel) {
			t.Errorf("%q: either miss reached the full matcher", pattern)
		}
	}
}

func TestMatcherNoNegationPrefilterDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(951))
	pool := []string{"/**", "foo/**/", `\#*\#`, `.\#*`, "gen/", "*.tmp", "src/*", "a/**/b", "/root.go", "[ab]", "?x"}
	paths := []string{"main.go", "src/pkg/server/main.go", "foo/bar.go", "gen/keep.go", "root.go", "#lock#", ".#lock", "a/b", "a/deep/b", "cache.tmp", "ax", "b"}
	for range 400 {
		patterns := append(append([]string{}, Builtin...), `\#*\#`)
		for range 1 + rng.Intn(len(pool)) {
			patterns = append(patterns, pool[rng.Intn(len(pool))])
		}
		m := New(patterns)
		prefiltered := New(patterns)
		prefiltered.checkUnmatchedParents = false // 08391bef's no-negation prefilter path.
		for _, path := range paths {
			for _, rel := range []string{path, "sub/" + path, "deep/sub/" + path} {
				for _, candidate := range []string{rel, rel + "/"} {
					if got, want := m.MatchRel(candidate), prefiltered.MatchRel(candidate); got != want {
						t.Fatalf("patterns=%q path=%q: got %v, prefiltered %v", patterns, candidate, got, want)
					}
				}
				if got, want := m.MatchRelEither(rel), prefiltered.MatchRel(rel) && prefiltered.MatchRel(rel+"/"); got != want {
					t.Fatalf("patterns=%q path=%q: either %v, prefiltered pair %v", patterns, rel, got, want)
				}
			}
		}
	}
}

func TestMatchRelEitherNoNegationSkipsDirectoryPass(t *testing.T) {
	for _, tc := range []struct{ pattern, path string }{
		{"node_modules/", "src/node_modules/pkg/deep/index.js"},
		{"*.log", "src/deep/build.log"}, {"out*", "out/file.go"},
	} {
		m := New([]string{tc.pattern})
		if !m.MatchRel(tc.path) || !m.MatchRel(tc.path+"/") {
			t.Fatal("invalid fixture")
		}
		// A sentinel miss makes a redundant second pass observable.
		m.dirs = New(nil).dirs
		if !m.MatchRelEither(tc.path) {
			t.Errorf("%q: equivalent directory form was queried again", tc.pattern)
		}
	}
}

func TestMatchRelEitherDirectoryFormBoundaries(t *testing.T) {
	for _, patterns := range [][]string{
		{"gen/"}, {"gen//"}, {"gen/**"}, {"gen/**/"},
		{"*.log", "gen/**"}, {"gen/", "!keep.go"},
		{"gen/", "!gen/"}, {"/**", "!keep.go"},
	} {
		m := New(patterns)
		for _, path := range []string{"gen", "gen/a.go", "sub/gen", "sub/gen/a.go", "a.log", "keep.go"} {
			want := m.MatchRel(path) && m.MatchRel(path+"/")
			if got := m.MatchRelEither(path); got != want {
				t.Errorf("%q %q: got %v want %v", patterns, path, got, want)
			}
		}
	}
}

func TestMatcherUnrelatedIncludeSkipsCompiledMatchers(t *testing.T) {
	const rel = "src/a/b/c/d/e/f/g.go"
	root := t.TempDir()
	for _, surface := range []string{"walk", "watcher", "include"} {
		t.Run(surface, func(t *testing.T) {
			m := NewWithIncludes(Builtin, []string{"*.proto"})
			// A nil compiled matcher panics if a prefilter miss evaluates it.
			// Keep the include prefilter open when testing the outer prefilter,
			// so neither guard can conceal a regression in the other.
			m.includes.dirs, m.includes.ign = nil, nil
			if surface != "include" {
				m.includes.requiredLiterals = nil
			}
			var included, excluded bool
			switch surface {
			case "walk":
				excluded, _ = m.ExplainAbsDirInOpenDir(filepath.Join(root, filepath.FromSlash(rel)), root, false)
			case "watcher":
				excluded = m.MatchRelEither(rel)
			case "include":
				included = m.IncludesRel(rel)
			}
			if included || excluded {
				t.Fatal("unrelated include and exclude rules must not match")
			}
		})
	}
}

// One compiled file query is sufficient when no negation can match and its
// positive verdict is identical in directory form. A nil directory matcher
// makes any extra compiled evaluation fail without a timing assertion.
func TestMatchRelEitherNonmatchingNegationSkipsParents(t *testing.T) {
	for _, rel := range []string{"node_modules/pkg/index.js", "node_modules/pkg/a/b/c/d/e/f/index.js"} {
		m := New(append(append([]string{}, Builtin...), "!.gitkeep"))
		m.dirs = nil
		if !m.MatchRelEither(rel) {
			t.Fatalf("excluded event admitted: %s", rel)
		}
	}
}

func TestMatcherNegationPrefilterDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(95103))
	pool := []string{"gen/", "node_modules/", "*.tmp", "/**", "gen/**/", "gen/**", "a/b/*", "[ab]", "?x", `\#*\#`, "!.gitkeep", "!*.proto", "!gen/", "!gen/keep/", "!**/keep.go", "!a/**/b", "!gen?/", "!/[ab]/", `!a\*b/`, "!*", "!/**", "!café/", "!foo//", "!!bang", "!!bang/", "bang", "!#x", "src/"}
	includePool := []string{"*.proto", "gen/", "vendor/acme/lib/", "/gen/keep/", "**/keep.go", "a?/", "[ab]/"}
	paths := []string{"main.go", "index.js", ".gitkeep", "schema.proto", "keep.go", "keep/file.go", "gen", "gen/lib/file.go", "gen/keep/a.tmp", "node_modules/pkg/index.js", "node_modules/pkg/.gitkeep", "node_modules/pkg/schema.proto", "node_modules/pkg/a/b/c/d/index.js", "a/b", "a/deep/b", "a/b/file.go", "foo", "foo//child", "cache.tmp", "a", "b", "ax", "#lock#", "cafe\u0301/x", "café/keep.go", "vendor/x.proto", "vendor/acme/lib/x.go", "!bang/x", "bang", "src/bang", "src/x/y.go"}
	queries := 0
	for range 2000 {
		var patterns, includes []string
		if rng.Intn(2) == 0 {
			patterns = append(patterns, Builtin...)
		}
		for range 1 + rng.Intn(12) {
			patterns = append(patterns, pool[rng.Intn(len(pool))])
		}
		for range rng.Intn(4) {
			includes = append(includes, includePool[rng.Intn(len(includePool))])
		}
		m := NewWithIncludes(patterns, includes)
		ungated := NewWithIncludes(patterns, includes)
		ungated.negationLiterals = nil // Force the old, unconditional parent check.
		for _, path := range paths {
			for _, prefix := range []string{"", "sub/", "deep/sub/"} {
				rel := prefix + path
				got, want := m.MatchRelEither(rel), ungated.MatchRelEither(rel)
				queries++
				if got != want {
					t.Fatalf("patterns=%q includes=%q path=%q: got %v, ungated %v", patterns, includes, rel, got, want)
				}
				for _, form := range []string{rel, rel + "/"} {
					if got, want := m.MatchRel(form), ungated.MatchRel(form); got != want {
						t.Fatalf("MatchRel patterns=%q includes=%q path=%q: got %v, ungated %v", patterns, includes, form, got, want)
					}
				}
			}
		}
	}
	if queries != 186000 {
		t.Fatalf("checked %d paths, want 186000", queries)
	}
}
