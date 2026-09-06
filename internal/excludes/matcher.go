package excludes

import (
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/zzet/gortex/internal/pathkey"
)

// Matcher tests whether a path should be excluded from indexing/watching.
// It is safe for concurrent reads after construction.
type Matcher struct {
	ign      *ignore.GitIgnore
	patterns []string
	// Required literals cheaply rule out simple positive patterns. Positive
	// patterns with uncertain syntax retain their original compiled matcher.
	requiredLiterals []string
	unfiltered       *ignore.GitIgnore
}

// New compiles the given patterns into a Matcher. A nil/empty list is
// valid and will match nothing.
//
// Patterns are folded to Unicode NFC so a pattern naming a non-ASCII
// directory matches paths regardless of which Unicode form the
// filesystem walk produced — MatchRel folds the candidate path to the
// same form before testing it.
//
// Each pattern is also run through literalizePattern before it reaches
// go-gitignore, whose compiler splices pattern text straight into a Go
// regexp and escapes only "." and "?". Without that step a line as
// ordinary as "*$" compiles to a regexp that matches every path and
// excludes the entire repository (#624). The Matcher keeps the original
// text in patterns — the rewrite is a compiler detail and must never
// reach a user-visible surface.
func New(patterns []string) *Matcher {
	cleaned := make([]string, 0, len(patterns))
	compiled := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		p = pathkey.Normalize(p)
		cleaned = append(cleaned, p)
		compiled = append(compiled, literalizePattern(p))
	}
	m := &Matcher{
		ign:      ignore.CompileIgnoreLines(compiled...),
		patterns: cleaned,
	}
	var unfiltered []string
	seen := make(map[string]bool)
	for i, p := range cleaned {
		if strings.HasPrefix(p, "!") {
			continue
		}
		literal := requiredLiteral(p)
		if literal == "" {
			unfiltered = append(unfiltered, compiled[i])
		} else if !seen[literal] {
			seen[literal] = true
			m.requiredLiterals = append(m.requiredLiterals, literal)
		}
	}
	if len(m.requiredLiterals) > 0 && len(unfiltered) > 0 {
		m.unfiltered = ignore.CompileIgnoreLines(unfiltered...)
	}
	return m
}

// requiredLiteral returns a substring every match of a simple glob must
// contain. Split at slashes too: a/b/* can match the directory a/b itself,
// and ** can consume zero path segments. Uncertain syntax stays unfiltered.
func requiredLiteral(pattern string) string {
	if strings.ContainsAny(pattern, "\\?[]") {
		return ""
	}
	var longest string
	for _, part := range strings.FieldsFunc(pattern, func(r rune) bool { return r == '*' || r == '/' }) {
		if len(part) > len(longest) {
			longest = part
		}
	}
	return longest
}

func (m *Matcher) couldMatch(rel string) bool {
	if len(m.requiredLiterals) == 0 {
		return true
	}
	for _, literal := range m.requiredLiterals {
		if strings.Contains(rel, literal) {
			return true
		}
	}
	return m.unfiltered != nil && m.unfiltered.MatchesPath(rel)
}

// Patterns returns the cleaned pattern list (empties and comments removed).
func (m *Matcher) Patterns() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.patterns))
	copy(out, m.patterns)
	return out
}

// MatchRel reports whether a repo-root-relative path is excluded.
// Path separators are normalised to forward slashes and the path is
// folded to Unicode NFC — matching how New normalised the patterns —
// before matching, so a non-ASCII path component compares equal to its
// pattern whether the OS supplied it decomposed (macOS NFD) or
// precomposed (Linux / git NFC).
func (m *Matcher) MatchRel(relPath string) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel := pathkey.Normalize(filepath.ToSlash(relPath))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return false
	}
	if !m.couldMatch(rel) {
		return false
	}
	return m.ign.MatchesPath(rel)
}

// Explain reports whether a repo-root-relative path is excluded and, when
// it is, which pattern excluded it — in the caller's original wording, not
// the rewritten form New compiles.
//
// This is what makes an over-broad ignore self-diagnosing: a repo that
// indexes zero files can name the one line responsible instead of leaving
// the operator to bisect a .gitignore by hand.
func (m *Matcher) Explain(relPath string) (bool, string) {
	if m == nil || m.ign == nil {
		return false, ""
	}
	rel := pathkey.Normalize(filepath.ToSlash(relPath))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return false, ""
	}
	matched, ip := m.ign.MatchesPathHow(rel)
	if !matched || ip == nil {
		return matched, ""
	}
	// LineNo is 1-based into the slice New handed the compiler, which is
	// index-aligned with m.patterns.
	if ip.LineNo >= 1 && ip.LineNo <= len(m.patterns) {
		return true, m.patterns[ip.LineNo-1]
	}
	return true, ip.Line
}

// MatchAbs reports whether an absolute path under root is excluded.
// Returns false if path is not under root.
func (m *Matcher) MatchAbs(absPath, root string) bool {
	return m.MatchAbsDir(absPath, root, false)
}

// MatchAbsDir reports whether an absolute path under root is excluded.
// When isDir is true the path is treated as a directory, so a pattern
// written with a trailing slash (e.g. "build/") matches the directory
// itself — letting the caller prune the whole subtree instead of
// descending it and re-testing every file. Returns false if path is
// not under root.
func (m *Matcher) MatchAbsDir(absPath, root string, isDir bool) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false
	}
	if isDir {
		rel += "/"
	}
	return m.MatchRel(rel)
}

// ExplainAbsDir is MatchAbsDir plus the pattern that excluded the path,
// in the caller's original wording. The pattern is "" when nothing
// matched.
func (m *Matcher) ExplainAbsDir(absPath, root string, isDir bool) (bool, string) {
	if m == nil || m.ign == nil {
		return false, ""
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false, ""
	}
	if isDir {
		rel += "/"
	}
	return m.Explain(rel)
}

// HasNegatedDescendant reports whether any re-include ("!") pattern in
// the matcher could match a path strictly beneath relDir.
//
// The index walk prunes an excluded directory with filepath.SkipDir so
// it never descends a subtree it would only throw away. But go-gitignore
// treats "*" as matching across "/", so a blanket like "a/b/*" reports
// the directory "a/b" itself as excluded — pruning it would skip a later
// "!a/b/keep/" re-include before the walk ever reaches the child. This
// lets the walk ask "could a negation resurrect something under here?"
// and keep descending when the answer is yes, mirroring git, which never
// prunes a directory a negation could re-include a child from.
//
// relDir is a repo-root-relative, forward-slash directory path (a
// trailing slash and a leading "./" are tolerated). The check is
// deliberately conservative: an unanchored or wildcard-leading negation
// can match at varying depths, so it is treated as "could be under
// anything" and the directory is kept rather than pruned.
func (m *Matcher) HasNegatedDescendant(relDir string) bool {
	if m == nil {
		return false
	}
	relDir = pathkey.Normalize(filepath.ToSlash(relDir))
	relDir = strings.TrimPrefix(relDir, "./")
	relDir = strings.TrimSuffix(relDir, "/")
	if relDir == "." {
		relDir = ""
	}
	for _, p := range m.patterns {
		if !strings.HasPrefix(p, "!") {
			continue
		}
		np := strings.TrimSpace(p[1:])
		np = strings.TrimPrefix(np, "/")
		np = strings.TrimSuffix(np, "/")
		if np == "" {
			continue
		}
		// A negation with no internal slash is unanchored: gitignore
		// matches it at any depth, so it can re-include something under
		// any directory. Keep descending.
		if !strings.Contains(np, "/") {
			return true
		}
		anchor := literalAnchor(np)
		if anchor == "" {
			// First segment is itself a wildcard ("*/...", "**/..."): it
			// can match at varying depths, so stay conservative.
			return true
		}
		// At the root, every anchored negation lives somewhere beneath us.
		if relDir == "" {
			return true
		}
		// The negation's match-set intersects relDir's subtree when its
		// literal anchor sits at or under relDir, or relDir sits under the
		// anchor (a wildcard tail can then still reach into relDir).
		if anchor == relDir ||
			strings.HasPrefix(anchor, relDir+"/") ||
			strings.HasPrefix(relDir, anchor+"/") {
			return true
		}
	}
	return false
}

// literalAnchor returns the leading path segments of a slash-bearing
// gitignore pattern up to (but excluding) the first segment that holds a
// wildcard meta-character. It returns "" when the first segment is
// itself a wildcard ("*", "**", "?foo", ...).
func literalAnchor(pattern string) string {
	segs := strings.Split(pattern, "/")
	lit := make([]string, 0, len(segs))
	for _, s := range segs {
		if strings.ContainsAny(s, "*?[") {
			break
		}
		lit = append(lit, s)
	}
	return strings.Join(lit, "/")
}
