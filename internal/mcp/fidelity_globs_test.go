package mcp

import (
	"runtime"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/elide"
)

func TestParseFidelityGlobs(t *testing.T) {
	rules, err := parseFidelityGlobs("internal/**:full,*_test.go:omit,vendor/**:compress")
	require.NoError(t, err)
	require.Len(t, rules, 3)
	assert.Equal(t, "internal/**", rules[0].glob)
	assert.Equal(t, elide.FidelityFull, rules[0].fidelity)
	assert.Equal(t, "*_test.go", rules[1].glob)
	assert.Equal(t, elide.FidelityOmit, rules[1].fidelity)
	assert.Equal(t, "vendor/**", rules[2].glob)
	assert.Equal(t, elide.FidelityCompress, rules[2].fidelity)

	// Malformed clauses are skipped, not fatal.
	for _, spec := range []string{"", "nofidelity", "glob:bogus", ":full"} {
		got, err := parseFidelityGlobs(spec)
		assert.NoErrorf(t, err, "a malformed clause stays fail-soft: %q", spec)
		assert.Emptyf(t, got, "%q yields no usable rule", spec)
	}
	mixed, err := parseFidelityGlobs("good/**:full, ,bad, *.go:omit")
	require.NoError(t, err)
	require.Len(t, mixed, 2, "only the two well-formed clauses survive")
}

func TestMatchFidelityGlob(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		// Trailing /** matches the dir and everything beneath.
		{"internal/**", "internal/foo/bar.go", true},
		{"internal/**", "internal", true},
		{"internal/**", "internalish/x.go", false},
		{"internal/**", "cmd/main.go", false},
		// Basename glob works without a **/ prefix.
		{"*_test.go", "internal/mcp/foo_test.go", true},
		{"*_test.go", "foo_test.go", true},
		{"*_test.go", "foo.go", false},
		// Leading **/ matches any depth.
		{"**/*.go", "a/b/c/x.go", true},
		{"**/testdata/*.json", "a/testdata/fixture.json", true},
		{"**/testdata/*.json", "a/b/testdata/fixture.json", true},
		// Bare ** matches everything.
		{"**", "anything/at/all.rs", true},
		// Bare directory prefix.
		{"vendor", "vendor/x/y.go", true},
		{"vendor", "vendored/x.go", false},
		// Single-segment * never crosses a slash. On Windows this is the
		// whole point of using path.Match: filepath.Match's separator is
		// the platform's, so '/' is an ordinary character there and the
		// second case answers true. Both cases pass on linux/macos with
		// either matcher, so only the Windows runner can tell them apart
		// — see the FidelityGlob selector in ci.yml.
		{"internal/*.go", "internal/x.go", true},
		{"internal/*.go", "internal/sub/x.go", false},
	}
	for _, c := range cases {
		got := matchFidelityGlob(c.pattern, c.rel)
		assert.Equalf(t, c.want, got, "matchFidelityGlob(%q, %q)", c.pattern, c.rel)
	}
}

// TestMatchFidelityGlob_DirStarStaysRecursive pins the one shape where
// the segment rule above does not decide the verdict. A trailing `/*`
// never reaches path.Match for a nested path — matchSegmentGlob's
// directory-prefix shortcut answers first — so `internal/*` has the same
// reach as `internal` and `internal/**`. That predates the path.Match
// change and callers depend on it; this test exists so the compatibility
// is a stated contract rather than an accident, and so a later cleanup of
// the shortcut cannot silently narrow a rule someone already wrote.
func TestMatchFidelityGlob_DirStarStaysRecursive(t *testing.T) {
	const pattern = "internal/*"

	assert.True(t, matchFidelityGlob(pattern, "internal"),
		"the directory itself")
	assert.True(t, matchFidelityGlob(pattern, "internal/a.go"),
		"a direct child")
	assert.True(t, matchFidelityGlob(pattern, "internal/sub/x.go"),
		"a nested child — the documented exception to the segment rule")
	assert.True(t, matchFidelityGlob(pattern, "internal/sub/deep/y.go"),
		"an arbitrarily deep child")

	// The shortcut is still segment-anchored: a sibling that merely
	// starts with the same bytes must not match.
	assert.False(t, matchFidelityGlob(pattern, "internalx/a.go"),
		"a sibling directory sharing the prefix")

	// And the segment-bounded form is still available, unchanged.
	assert.False(t, matchFidelityGlob("internal/*.go", "internal/sub/x.go"),
		"internal/*.go stays segment-bounded")
}

func TestFidelityDecideForPath(t *testing.T) {
	rules, err := parseFidelityGlobs("internal/**:full,*_test.go:omit")
	require.NoError(t, err)
	// First matching rule wins (order matters).
	dFull := fidelityDecideForPath(rules, "internal/mcp/server.go")
	require.NotNil(t, dFull)
	assert.Equal(t, elide.FidelityFull, dFull(elide.Decl{}))

	dOmit := fidelityDecideForPath(rules, "cmd/foo_test.go")
	require.NotNil(t, dOmit)
	assert.Equal(t, elide.FidelityOmit, dOmit(elide.Decl{}))

	// No matching rule -> nil decider (caller falls back to compress).
	assert.Nil(t, fidelityDecideForPath(rules, "cmd/main.go"))
	assert.Nil(t, fidelityDecideForPath(nil, "anything.go"))
}

// TestReadFile_FidelityGlobsOmit exercises the end-to-end MCP path: a
// fidelity rule that omits every declaration in the matched file
// produces omit markers and drops the bodies, while compress_bodies is
// set so the elide path runs.
func TestReadFile_FidelityGlobsOmit(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "omitted", "omit rule must leave a marker")
	assert.NotContains(t, content, `strings.Split(t, ".")`,
		"omitted declaration body must be gone")
	assert.NotContains(t, content, "func ValidateToken",
		"omitted declaration signature must be gone")
	assert.Equal(t, true, m["bodies_elided"])
}

// TestReadFile_FidelityGlobsFull asserts a `full` rule leaves the file
// uncompressed (body present, no stub) even with compress_bodies set.
func TestReadFile_FidelityGlobsFull(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:full",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, `strings.Split(t, ".")`,
		"a full rule must keep the body verbatim")
	assert.NotContains(t, content, "lines elided",
		"a full rule must not stub any body")
}

// TestReadFile_FidelityGlobsCompressFallback asserts that when no rule
// matches the file, the call falls back to the plain compress_bodies
// behaviour (body stubbed, signature kept).
func TestReadFile_FidelityGlobsCompressFallback(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "vendor/**:omit", // does not match service.go
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "func ValidateToken", "signature kept on compress fallback")
	assert.Contains(t, content, "lines elided", "body stubbed on compress fallback")
	assert.NotContains(t, content, "omitted", "no omit marker when the omit rule does not match")
}

// TestReadFile_FidelityGlobsKeepComposes asserts the per-symbol keep
// predicate overrides an omit rule: the kept symbol survives at full
// source while the rest of the file is omitted.
func TestReadFile_FidelityGlobsKeepComposes(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
		"keep":            "ValidateToken",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "func ValidateToken", "kept symbol survives omit rule")
	assert.Contains(t, content, `strings.Split(t, ".")`, "kept symbol keeps its body")
	assert.Contains(t, content, "omitted", "other declarations still omitted")
}

// TestGetEditingContext_FidelityGlobsOmit asserts the same fidelity_globs
// wiring on get_editing_context's source_compressed view.
func TestGetEditingContext_FidelityGlobsOmit(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
	}))
	sc, _ := m["source_compressed"].(string)
	require.NotEmpty(t, sc, "source_compressed must be present")
	assert.Contains(t, sc, "omitted", "omit rule must mark declarations")
	assert.NotContains(t, sc, `strings.Split(t, ".")`, "omitted body must be gone")
}

// TestMatchFidelityGlob_RepeatedGlobstarsStayBounded is the regression
// test for a denial-of-service, not a performance nicety.
//
// The matcher walks the pattern against the path, and a plain recursion
// re-derives the same (pattern suffix, path suffix) pair once for every
// way of reaching it — so each additional `**` multiplies the work. This
// exact input ran for over a hundred seconds before the memo; `glob` is
// user input and find_files evaluates it against every candidate file
// before applying the result limit, so one request could hold a daemon
// core indefinitely.
//
// The globstars are separated by a literal segment on purpose. Adjacent
// ones collapse in globPatternSegments before the matcher ever sees them,
// so a run of `**/**/**/…` reduces to a single `**` and exercises none of
// the memo — an earlier version of this test used exactly that and stayed
// green with the memo deleted. Alternating `**/x/` survives the collapse
// and is what forces the repeated subproblems.
//
// The size is measured, not guessed. Removing the memo and leaving the
// collapse in place, on this machine:
//
//	globstars  path segments   calls          unmemoised
//	        8             20     803,860           10 ms
//	        8             40 246,777,526          7.48 s
//	       10             30 151,946,378          8.04 s
//	       10             40 3,189,663,472     1 m 53.8 s
//
// The memoised matcher answers every one of those in under a
// millisecond. Ten and forty gives the deadline a margin of more than
// twenty times against the exponential path while sitting several orders
// of magnitude above the memoised one, so a loaded runner cannot trip it
// and a lost memo cannot pass it. Smaller inputs — including 8 and 20 —
// prove the memo matters by call count but finish far inside any
// wall-clock bound, which is how the previous version of this test came
// to be green against a matcher with no memo at all.
func TestMatchFidelityGlob_RepeatedGlobstarsStayBounded(t *testing.T) {
	pattern := "a/" + strings.Repeat("**/x/", 10) + "never"
	rel := "a/" + strings.Repeat("x/", 40) + "q"

	// Guard the guard: if a future change makes these collapse too, the
	// timing assertion below stops testing anything.
	segs := globPatternSegments(pattern)
	globstars := 0
	for _, s := range segs {
		if s == "**" {
			globstars++
		}
	}
	require.Greaterf(t, globstars, 4,
		"the adversarial pattern collapsed to %v — it no longer reaches the memo", segs)

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- matchFidelityGlob(pattern, rel) }()

	select {
	case got := <-done:
		assert.False(t, got, "the path does not end in `never`, so this must not match")
		assert.Lessf(t, time.Since(start), 5*time.Second,
			"matchFidelityGlob(%q, ...) took %s — the globstar walk is enumerating again",
			pattern, time.Since(start))
	case <-time.After(5 * time.Second):
		t.Fatalf("matchFidelityGlob(%q, ...) did not finish within 5s — "+
			"a user-supplied glob can pin a daemon core", pattern)
	}
}

// TestMatchFidelityGlob_TerminalStarKeepsItsRequiredSegment pins the
// depth an ordinary `*` demands. The trailing-star rewrite exists only to
// let the subtree rule survive a globbed prefix; applied to a pattern
// with no globstar it silently dropped a required segment, because `**`
// may consume zero. A `*/*` find_files glob then returned root files, and
// the same pattern in fidelity_globs applied omit/compress rules to them.
func TestMatchFidelityGlob_TerminalStarKeepsItsRequiredSegment(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		rel     string
		want    bool
	}{
		{"*/*", "top.go", false},
		{"src/*/*", "src/top.go", false},
		// The depth they do accept is unchanged.
		{"*/*", "a/b.go", true},
		{"src/*/*", "src/a/b.go", true},
		// And the rewrite still fires where it is meant to.
		{"src/**/internal/*", "src/a/internal/sub/deep.go", true},
	} {
		assert.Equalf(t, tc.want, matchFidelityGlob(tc.pattern, tc.rel),
			"matchFidelityGlob(%q, %q)", tc.pattern, tc.rel)
	}
}

// TestFidelityGlobTerminalStarDepthAtTheConsumer runs the same depth rule
// through the path a configured fidelity rule actually takes, so a
// widening cannot reach users' files while only the matcher test is
// watched.
func TestFidelityGlobTerminalStarDepthAtTheConsumer(t *testing.T) {
	rules, err := parseFidelityGlobs("*/*:omit")
	require.NoError(t, err)

	assert.Nil(t, fidelityDecideForPath(rules, "top.go"),
		"a root file must not be caught by `*/*`")

	nested := fidelityDecideForPath(rules, "a/b.go")
	require.NotNil(t, nested, "`*/*` still has to match one level down")
	assert.Equal(t, elide.FidelityOmit, nested(elide.Decl{}))
}

// TestMatchFidelityGlob_GlobstarComposesWithTrailingSubtree pins the two
// documented rules working together: `**` crosses directories anywhere,
// and a trailing `/*` covers a whole subtree. Each held on its own, but
// `src/**/internal/*` resolved neither — the segment walk spent the final
// `*` on one segment, and the legacy prefix fallback cannot help because
// it reads `src/**/internal` literally.
//
// This is also a Windows regression guard: the older filepath.Match
// accepted the deep path here, because '/' is an ordinary character when
// the separator is '\'.
func TestMatchFidelityGlob_GlobstarComposesWithTrailingSubtree(t *testing.T) {
	const pattern = "src/**/internal/*"

	assert.True(t, matchFidelityGlob(pattern, "src/a/internal"),
		"the directory itself, exactly as `internal/*` matches `internal`")
	assert.True(t, matchFidelityGlob(pattern, "src/a/internal/x.go"),
		"a direct child")
	assert.True(t, matchFidelityGlob(pattern, "src/a/internal/sub/deep.go"),
		"a deeply nested child — the case that regressed")
	assert.True(t, matchFidelityGlob(pattern, "src/a/b/c/internal/deep/y.go"),
		"the globstar itself spanning several directories")

	assert.False(t, matchFidelityGlob(pattern, "src/a/other/deep.go"),
		"a sibling directory that is not `internal`")
	assert.False(t, matchFidelityGlob(pattern, "other/a/internal/x.go"),
		"the anchored first segment still has to match")
}

// TestFidelityGlobDecideForPath_SubtreeComposition runs the same
// composition through the consumer that fidelity rules actually reach, so
// the contract is pinned at the level users configure rather than only at
// the matcher.
func TestFidelityGlobDecideForPath_SubtreeComposition(t *testing.T) {
	rules, err := parseFidelityGlobs("src/**/internal/*:full,**:omit")
	require.NoError(t, err)

	for _, rel := range []string{
		"src/a/internal/x.go",
		"src/a/internal/sub/deep.go",
	} {
		d := fidelityDecideForPath(rules, rel)
		require.NotNilf(t, d, "%s matched no rule at all", rel)
		assert.Equalf(t, elide.FidelityFull, d(elide.Decl{}), "%s should take the first rule", rel)
	}

	other := fidelityDecideForPath(rules, "src/a/other/deep.go")
	require.NotNil(t, other)
	assert.Equal(t, elide.FidelityOmit, other(elide.Decl{}),
		"a path outside the subtree must fall through to the catch-all")
}

// BenchmarkMatchFidelityGlob_LongTerminalStarWithoutGlobstar keeps the
// non-globstar amplification from coming back.
//
// A pattern ending in `/*` used to enter the globstar walk on the theory
// that normalisation might rewrite the terminal star — but that rewrite is
// gated on an existing `**`, so a pattern without one could never gain a
// match there and only paid for the split and the working row. find_files
// runs the matcher against every candidate ahead of `limit`, which made the
// cost of a long pattern a multiplier on the whole scan. Keep this fixture
// within the admission bounds so it exercises the gate rather than being
// rejected before compilation.
func BenchmarkMatchFidelityGlob_LongTerminalStarWithoutGlobstar(b *testing.B) {
	pattern := strings.Repeat("segment/", 63) + "*"
	rel := strings.Repeat("segment/", 39) + "leaf.go"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if matchFidelityGlob(pattern, rel) {
			b.Fatal("the deliberately shallower path must not match")
		}
	}
}

// BenchmarkMatchFidelityGlob_GlobstarWalkWorkingSet measures the shape that
// does reach the walk, so the O(len(rel)) working row stays visible next to
// the case above rather than being asserted only in a comment.
func BenchmarkMatchFidelityGlob_GlobstarWalkWorkingSet(b *testing.B) {
	pattern := "a/" + strings.Repeat("**/x/", 8) + "never"
	rel := "a/" + strings.Repeat("x/", 20) + "q"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if matchFidelityGlob(pattern, rel) {
			b.Fatal("the path does not end in `never`")
		}
	}
}

// TestMatchFidelityGlob_GlobstarGateIgnoresNonSegmentStars pins what counts
// as a globstar for the entry gate. `a**b` is two ordinary stars to
// path.Match, not a whole-segment `**`, so it must not pull a pattern into
// the walk — the gate has to agree with globPatternSegments about that or
// the two disagree on which patterns the rewrite applies to.
func TestMatchFidelityGlob_GlobstarGateIgnoresNonSegmentStars(t *testing.T) {
	for _, pattern := range []string{"**", "**/x.go", "a/**", "a/**/b"} {
		assert.Truef(t, patternHasGlobstarSegment(pattern),
			"%q carries a whole-segment globstar", pattern)
		assert.Truef(t, hasGlobstarSegment(strings.Split(pattern, "/")),
			"%q: the gate and the split must agree", pattern)
	}
	for _, pattern := range []string{"a**b", "a**b/c", "*", "a/*", "a/*.go", "a**"} {
		assert.Falsef(t, patternHasGlobstarSegment(pattern),
			"%q has no whole-segment globstar", pattern)
		assert.Falsef(t, hasGlobstarSegment(strings.Split(pattern, "/")),
			"%q: the gate and the split must agree", pattern)
	}

	// `a**b` still behaves as an ordinary segment glob.
	assert.True(t, matchFidelityGlob("a**b", "axxb"))
	assert.False(t, matchFidelityGlob("a**b", "a/x/b"))
}

// TestParseFidelityGlobs_RejectsOversizedClause holds the parser to the
// distinction the contract now draws. A malformed clause is still skipped
// — a typo has never aborted a read. An over-budget clause is refused,
// because dropping it rewrites the caller's policy: the rules are
// first-match, so an oversized `omit` disappearing lets a later `full`
// win and the content the request asked to hide comes back in a read that
// looks like it succeeded.
func TestParseFidelityGlobs_RejectsOversizedClause(t *testing.T) {
	huge := strings.Repeat("segment/", maxGlobSegments+10) + "**"

	rules, err := parseFidelityGlobs("internal/**:full," + huge + ":omit")
	require.Error(t, err, "an over-budget rule must not be silently dropped")
	assert.Nil(t, rules)
	assert.Contains(t, err.Error(), "too large")

	// The ordered-policy consequence, stated directly: this is the shape
	// that used to turn a requested `omit` into `full`.
	deep := strings.Repeat("x/", maxGlobSegments) + "**"
	_, err = parseFidelityGlobs(deep + ":omit,**:full")
	require.Error(t, err, "an oversized first-match omit must refuse, not defer to the later rule")

	// Total spec size and rule count are bounded too; both are per-request
	// multipliers on a per-file scan.
	_, err = parseFidelityGlobs(strings.Repeat("a", maxFidelitySpecBytes+1) + ":omit")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")

	_, err = parseFidelityGlobs(strings.Repeat("never-*:omit,", maxFidelityRules+1) + "**:full")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than")

	// Exactly at the rule cap is still served.
	atCap, err := parseFidelityGlobs(strings.Repeat("never-*:omit,", maxFidelityRules-1) + "**:full")
	require.NoError(t, err)
	assert.Len(t, atCap, maxFidelityRules)
}

// TestMatchFidelityGlob_NonGlobstarPatternDoesNotEnterTheWalk binds what
// the benchmark above only reports. A benchmark is not run by CI and
// nothing fails when its numbers regress, so the gate itself is asserted here.
// The fixture must be within the admission bounds; otherwise compileGlob
// rejects it before the globstar gate is reached.
func TestMatchFidelityGlob_NonGlobstarPatternDoesNotEnterTheWalk(t *testing.T) {
	pattern := strings.Repeat("segment/", 63) + "*"
	rel := strings.Repeat("segment/", 39) + "leaf.go"

	require.False(t, patternHasGlobstarSegment(pattern),
		"the fixture must have no whole-segment globstar, or it proves nothing")
	compiled := compileGlob(pattern)
	require.False(t, compiled.tooComplex(),
		"the fixture must be admitted so the globstar gate is exercised")
	assert.Nil(t, compiled.segments,
		"a pattern with no whole-segment globstar must not compile a segment walk")
	require.False(t, matchFidelityGlob(pattern, rel),
		"the deliberately shallower path must not match")
}

// TestFidelityGlobsOversizedRuleCannotWeakenThePolicy_Endpoints is the
// consumer-side proof for the ordered-policy hazard. Dropping an
// over-budget rule is not a cosmetic difference: the rules are
// first-match, so an oversized `omit` disappearing let a later `full` win
// and the file the request asked to hide came back in full, in a response
// that looked like a normal successful read.
//
// Both endpoints that accept fidelity_globs are covered, because the
// parser is reached separately from each.
func TestFidelityGlobsOversizedRuleCannotWeakenThePolicy_Endpoints(t *testing.T) {
	srv, _ := setupCompressTestServer(t)

	// An oversized first-match `omit` followed by a catch-all `full` —
	// exactly the pair that used to resolve to `full`.
	oversized := strings.Repeat("x/", maxGlobSegments) + "**"
	spec := oversized + ":omit,**:full"

	for _, tool := range []string{"read_file", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			args := map[string]any{
				"compress_bodies": true,
				"fidelity_globs":  spec,
			}
			args["path"] = "service.go"
			res := callTool(t, srv, tool, args)
			require.Truef(t, res.IsError,
				"%s must refuse the request rather than serve it under a weakened policy", tool)
			text := res.Content[0].(mcplib.TextContent).Text
			require.Contains(t, text, "too large")
			require.NotContains(t, text, `strings.Split(t, ".")`,
				"no file body may be returned when the policy was rejected")
		})
	}
}

// TestMatchFidelityGlob_LeadingGlobstarNonmatchStaysLinear pins the
// allocation of the legacy leading-`**/` fallback.
//
// After the linear walk declines, that branch tries the suffix glob at
// every depth. It used to split the path and re-join every tail, which
// rebuilt the whole remainder once per segment — quadratic in path depth,
// and paid per candidate file: 17 MB for one 2000-segment nonmatch.
// Slicing the original string shares its bytes instead.
//
// The ceiling sits far below the old figure and far above the new one, so
// allocator noise cannot trip it and the re-join cannot pass it.
func TestMatchFidelityGlob_LeadingGlobstarNonmatchStaysLinear(t *testing.T) {
	rel := strings.Repeat("segment/", 2000) + "leaf.go"
	require.False(t, matchFidelityGlob("**/never", rel))

	const runs = 20
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		_ = matchFidelityGlob("**/never", rel)
	}
	runtime.ReadMemStats(&after)

	perOp := (after.TotalAlloc - before.TotalAlloc) / runs
	assert.Lessf(t, perOp, uint64(1<<20),
		"a leading-globstar nonmatch allocated %d B for one candidate; "+
			"the suffix scan must not rebuild the path", perOp)
}
