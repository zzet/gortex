package mcp

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/elide"
)

// fidelityGlobsParamDescription documents the fidelity_globs param for
// read_file / get_editing_context. Kept as a constant so both tool
// registrations share one source of truth.
const fidelityGlobsParamDescription = "Per-glob fidelity tiers, applied when compress_bodies is set: a comma-separated, ordered list of `glob:fidelity` rules where fidelity is one of full | compress | omit (e.g. \"internal/**:full,*_test.go:omit,vendor/**:compress\"). The first rule whose glob matches the file's repo-relative path wins; a file matching no rule falls back to the compress_bodies boolean (compress). Glob semantics: `*` stays within one path segment, basenames are matched too (so `*_test.go` works without a `**/` prefix), a trailing `/**`, a bare prefix (`internal`) and a trailing `/*` each match a directory and everything below it; a leading `**/` matches at any depth. An over-size rule, or more than 64 rules, is an error, not a silent drop — a first-match policy is never quietly weakened. The per-symbol `keep` predicate still composes: a kept symbol stays full even when its file's rule says compress or omit."

// fidelityRule is one parsed `glob:fidelity` clause. Rules are matched
// in declaration order; the first matching glob wins.
type fidelityRule struct {
	glob     string
	compiled compiledGlob
	fidelity elide.Fidelity
}

// Admission bounds for a whole fidelity_globs value. The rules are an
// ordered list scanned per file, so both the total size and the count are
// per-request multipliers on a per-file loop: 1.3 MB of spec parsed into
// 100,001 rules before these existed.
const (
	maxFidelitySpecBytes = 8192
	maxFidelityRules     = 64
)

// parseFidelityGlobs parses the fidelity_globs param value into an
// ordered rule list.
//
// Two failure modes, deliberately different:
//
//   - A malformed clause is skipped. A typo has never aborted a read and
//     still does not.
//   - A clause that breaks an admission bound returns an error, and the
//     caller must refuse the request. Dropping it would silently rewrite
//     the caller's policy: the rules are first-match, so an over-budget
//     `omit` disappearing lets a later `full` win and the content the
//     request asked to hide comes back instead. That is not a typo the
//     user can see in the output — it looks like a successful read.
//
// Returns nil rules when the value yields none, so the caller falls back
// to the plain compress_bodies boolean.
func parseFidelityGlobs(spec string) ([]fidelityRule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if len(spec) > maxFidelitySpecBytes {
		return nil, fmt.Errorf(
			"fidelity_globs is too large (%d bytes); the limit is %d",
			len(spec), maxFidelitySpecBytes)
	}
	var rules []fidelityRule
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		// Split on the LAST colon so a Windows-style or
		// colon-bearing glob keeps its colons; the fidelity token is
		// always the trailing field.
		idx := strings.LastIndex(clause, ":")
		if idx <= 0 || idx == len(clause)-1 {
			continue
		}
		glob := strings.TrimSpace(clause[:idx])
		fid, ok := parseFidelity(clause[idx+1:])
		if glob == "" || !ok {
			continue
		}
		compiled := compileGlob(glob)
		if compiled.tooComplex() {
			return nil, fmt.Errorf(
				"fidelity_globs rule %q is too large (%d bytes, %d segments); "+
					"the limits are %d bytes and %d segments",
				glob, len(compiled.pattern), compiled.segmentCount(),
				maxGlobBytes, maxGlobSegments)
		}
		if len(rules) == maxFidelityRules {
			return nil, fmt.Errorf(
				"fidelity_globs has more than %d rules", maxFidelityRules)
		}
		rules = append(rules, fidelityRule{glob: glob, compiled: compiled, fidelity: fid})
	}
	return rules, nil
}

// parseFidelity maps a fidelity token to the elide enum.
func parseFidelity(tok string) (elide.Fidelity, bool) {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case "full":
		return elide.FidelityFull, true
	case "compress", "compressed":
		return elide.FidelityCompress, true
	case "omit", "omitted":
		return elide.FidelityOmit, true
	default:
		return elide.FidelityCompress, false
	}
}

// fidelityDecideForPath builds an elide.Decide that applies the first
// matching rule's fidelity to every declaration in the file at relPath.
// Returns nil when no rule matches (the caller then falls back to plain
// compress). The verdict is per-file, not per-declaration: all decls in
// one file share the file's tier. The per-symbol keep predicate is
// layered on separately by elide.Options.verdict.
func fidelityDecideForPath(rules []fidelityRule, relPath string) func(elide.Decl) elide.Fidelity {
	if len(rules) == 0 {
		return nil
	}
	rel := filepath.ToSlash(relPath)
	for _, r := range rules {
		// The compiled form was built once at parse time; matching here
		// per file must not re-normalise or re-split the pattern.
		if r.compiled.match(rel) {
			fid := r.fidelity
			return func(elide.Decl) elide.Fidelity { return fid }
		}
	}
	return nil
}

// matchFidelityGlob matches a glob against a forward-slash relative
// path. It extends matchPathPattern's basename/prefix semantics with
// explicit `**` support so the documented `internal/**` / `**/*.go`
// forms work as written. Glob matching is segment-bounded — a single `*`
// never crosses `/`, which is why matchSegmentGlob needs path.Match and
// not filepath.Match — but matchSegmentGlob also carries a separate
// directory-prefix rule that is not glob matching at all, and a trailing
// `/*` goes through it. See matchSegmentGlob.
func matchFidelityGlob(pattern, rel string) bool {
	return compileGlob(pattern).match(rel)
}

// compiledGlob is a pattern normalised and split once, so a request pays
// for that work per request rather than per candidate file, and so the
// size bound can only ever be read off the same representation the matcher
// uses. Counting separators in the raw string was a bypass: on Windows a
// native-separator pattern counts as one segment before ToSlash and as
// many after it, which admitted a 130-byte glob that expands to 65
// segments at match time.
type compiledGlob struct {
	pattern  string   // '/'-spelled
	segments []string // nil unless a whole-segment `**` makes the walk relevant
}

// compileGlob normalises, then bounds, then splits. The order matters:
// an over-budget pattern must not pay for the split, and nothing outside
// this function should see the un-normalised form.
func compileGlob(pattern string) compiledGlob {
	g := compiledGlob{pattern: filepath.ToSlash(pattern)}
	if g.tooComplex() {
		return g
	}
	if patternHasGlobstarSegment(g.pattern) {
		g.segments = globPatternSegments(g.pattern)
	}
	return g
}

// tooComplex reports whether the normalised pattern exceeds the admission
// bounds. It is a method so that the only way to ask is to hold a
// compiledGlob, which is by construction already normalised.
func (g compiledGlob) tooComplex() bool {
	return len(g.pattern) > maxGlobBytes ||
		strings.Count(g.pattern, "/")+1 > maxGlobSegments
}

// segmentCount reports the normalised segment count, for error messages
// that have to agree with the check that rejected the pattern.
func (g compiledGlob) segmentCount() int { return strings.Count(g.pattern, "/") + 1 }

func (g compiledGlob) match(rel string) bool {
	pattern := g.pattern
	rel = filepath.ToSlash(rel)

	// `**` in any position, matching zero or more whole segments. The
	// branches below only ever recognised it bare, leading or trailing, so
	// a middle `**` fell through to path.Match and came back
	// segment-bounded — `internal/**/*_test.go`, the example in the
	// find_files schema, missed every file more than one directory down.
	//
	// This runs first and only ever adds a match: every branch below is
	// left exactly as it was, so the directory-prefix rules and the
	// basename fallback keep deciding everything they decided before.
	//
	// Only a real globstar reaches the walk. A trailing `/*` used to enter
	// too, on the theory that globPatternSegments might rewrite it — but
	// that rewrite is itself gated on an existing `**`, so for a pattern
	// without one the walk could never add a match and only paid for the
	// split and the working row. find_files runs this against every
	// candidate before applying the limit, so that overhead was
	// user-controlled: a 999-segment `segment/.../*` measured 99 kB per
	// call with no `**` in it at all.
	if g.segments != nil &&
		matchGlobstarSegments(g.segments, strings.Split(rel, "/")) {
		return true
	}

	// Trailing `/**` (or bare `**`): match the directory and the whole
	// subtree beneath it.
	if pattern == "**" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return rel == prefix || strings.HasPrefix(rel, prefix+"/")
	}

	// Leading `**/`: match the suffix glob at any directory depth.
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if matchSegmentGlob(suffix, rel) {
			return true
		}
		// Try the suffix against every trailing path component so
		// `**/foo/*.go` matches `a/b/foo/x.go`.
		//
		// Walk the separator offsets and slice `rel` instead of rebuilding
		// each suffix: a substring shares the original bytes, while
		// splitting and re-joining allocated every suffix in turn. That was
		// quadratic in path depth for a nonmatch — 17 MB for a single
		// 2000-segment candidate, paid once per candidate file.
		for i := 0; i < len(rel); i++ {
			if rel[i] != '/' {
				continue
			}
			if matchSegmentGlob(suffix, rel[i+1:]) {
				return true
			}
		}
		return false
	}

	return matchSegmentGlob(pattern, rel)
}

// globPatternSegments splits a pattern into the segments the matcher
// consumes, with two normalisations.
//
// A trailing `*` becomes `**`, but only when some earlier segment is a
// whole-segment `**`. The rewrite exists for one job: letting the
// directory-prefix rule survive a globbed prefix, since the legacy
// fallback treats `src/**/internal` as a literal and cannot resolve it.
//
// The condition is what keeps it from being a widening. `dir/*` has
// always meant the whole subtree here, and the legacy fallback still
// says so for literal prefixes — but `**` can consume zero segments, so
// rewriting unconditionally also made `*/*` match `top.go` and
// `src/*/*` match `src/top.go`, dropping a segment that an ordinary `*`
// requires. Patterns with no globstar keep their old depth exactly.
//
// Adjacent globstars collapse. `a/**/**/b` means exactly `a/**/b`, and
// leaving the duplicates in multiplies the matcher's state space for no
// added meaning — which is how a short pattern turned into minutes of CPU.
func globPatternSegments(pattern string) []string {
	segs := strings.Split(pattern, "/")
	if n := len(segs); n > 1 && segs[n-1] == "*" && hasGlobstarSegment(segs[:n-1]) {
		segs[n-1] = "**"
	}
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "**" && len(out) > 0 && out[len(out)-1] == "**" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// hasGlobstarSegment reports whether any segment is a bare `**`. A `**`
// inside a larger segment (`a**b`) is two ordinary stars to path.Match,
// not a globstar, so it does not count.
func hasGlobstarSegment(segs []string) bool {
	for _, s := range segs {
		if s == "**" {
			return true
		}
	}
	return false
}

// patternHasGlobstarSegment is hasGlobstarSegment without the split, so the
// hot path can decide whether the globstar walk is needed at all without
// allocating. The four shapes are exhaustive: a bare `**` segment is either
// the whole pattern, its first segment, its last, or bounded by slashes on
// both sides.
func patternHasGlobstarSegment(pattern string) bool {
	return pattern == "**" ||
		strings.HasPrefix(pattern, "**/") ||
		strings.HasSuffix(pattern, "/**") ||
		strings.Contains(pattern, "/**/")
}

// Bounds on a user-supplied glob, applied before anything scans files. The
// walk is linear in pattern segments times path segments, so an unbounded
// pattern is unbounded work per candidate. A repo-relative path glob does
// not legitimately reach these; they exist to stop one request from turning
// a file scan into a CPU sink.
const (
	maxGlobBytes    = 1024
	maxGlobSegments = 64
)

// matchGlobstarSegments matches a segment-split pattern against a
// segment-split path, giving `**` its usual meaning: zero or more whole
// segments, wherever it appears. Every other segment is matched with
// path.Match, so a single `*` stays inside its own segment.
//
// Zero segments is deliberate — `internal/**/*_test.go` has to match
// `internal/foo_test.go` as well as `internal/a/b/c_test.go`, or the
// pattern means something different at each depth.
//
// Recursion is not an option here. A plain walk re-derives the same
// (pattern suffix, path suffix) pair once per way of reaching it, so each
// extra `**` multiplies the work: a 42-byte pattern with twelve of them
// against a 27-segment non-match ran for over a hundred seconds. The glob
// is user input and find_files runs this against every candidate file
// before applying the result limit, so that was a way to pin a daemon core
// from a single request.
//
// This is the same dynamic program run bottom-up over one row instead of a
// dense (pattern x path) memo. row[j] answers "does the pattern suffix
// under consideration match rel[j:]", and each pattern segment rewrites it
// once, so the working memory is O(len(rel)) rather than
// O(len(pattern) * len(rel)) — the matrix was itself a per-candidate
// allocation an oversized pattern could inflate.
func matchGlobstarSegments(pattern, rel []string) bool {
	n := len(rel)

	// Past the end of the pattern only an exhausted path matches.
	row := make([]bool, n+1)
	row[n] = true

	for i := len(pattern) - 1; i >= 0; i-- {
		if pattern[i] == "**" {
			// Zero or more whole segments: rel[j:] matches when rel[t:]
			// does for any t >= j. A suffix OR, in place, from the back.
			for j := n - 1; j >= 0; j-- {
				row[j] = row[j] || row[j+1]
			}
			continue
		}
		// One segment consumed. Forward is safe: writing row[j] reads
		// row[j+1], which this pass has not touched yet.
		for j := 0; j < n; j++ {
			ok, _ := path.Match(pattern[i], rel[j])
			row[j] = ok && row[j+1]
		}
		// The pattern still has this segment to place, so an exhausted
		// path can no longer match.
		row[n] = false
	}
	return row[0]
}

// matchSegmentGlob applies the single-segment glob semantics shared
// with matchPathPattern: a glob match against the full path and the
// basename, plus a bare directory-prefix shortcut.
//
// path.Match, not filepath.Match. Both callers hand this function a
// forward-slash path, and filepath.Match's separator is the platform's:
// on Windows '/' is an ordinary character there, so `*` crosses it and
// `internal/*.go` matches `internal/sub/x.go`. path.Match's separator is
// always '/', which is the semantics this file's `**` handling — and its
// own doc-comment — already assume.
//
// The two prefix shortcuts below are deliberately not glob matching, and
// they are what makes `internal/*` recursive: `dir/*` and a bare `dir`
// both match the directory and everything beneath it, the same reach as
// `dir/**`. That predates the path.Match change and callers depend on
// it, so it stays — but it means "a single `*` never crosses `/`"
// describes the glob rule, not this function's whole verdict.
// TestMatchFidelityGlob_DirStarStaysRecursive pins it.
func matchSegmentGlob(pattern, rel string) bool {
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	if ok, _ := path.Match(pattern, path.Base(rel)); ok {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	if strings.HasPrefix(rel, pattern+"/") {
		return true
	}
	return false
}
