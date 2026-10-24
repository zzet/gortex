package excludes

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/zzet/gortex/internal/pathkey"
)

// Matcher tests whether a path should be excluded from indexing/watching.
// It is safe for concurrent reads after construction.
type Matcher struct {
	includes        *Matcher
	literalIncludes []string
	ign             *ignore.GitIgnore
	patterns        []string
	// Required literals cheaply rule out simple positive patterns. Positive
	// patterns with uncertain syntax retain their original compiled matcher.
	requiredLiterals []string
	unfiltered       *ignore.GitIgnore
	// Directories are matched without a trailing slash, against patterns
	// with their directory-only marker removed. This prevents child globs
	// such as a/b/* from matching the parent a/b/ with an empty child name.
	dirs        *ignore.GitIgnore
	hasNegation bool
	// A non-empty list rules out negations missing all required literals.
	// Any uncertain negation keeps the parent check unfiltered.
	negationLiterals []string
	// Some dependency regexps do not propagate an ancestor match to a
	// full file path. Those lists cannot skip unmatched parent queries.
	checkUnmatchedParents bool

	// The same list without Builtin's agent-config root lines, compiled on
	// first use; see withoutBuiltinAgentRoots.
	agentRootsLifted     *Matcher
	agentRootsLiftedOnce sync.Once
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
	directories := make([]string, len(cleaned))
	for i, p := range cleaned {
		// The dependency's terminal /** also matches the parent. For
		// directory queries, /* has the same descendant coverage without
		// matching the parent itself. File queries keep their old behavior.
		dirPattern := strings.TrimSuffix(p, "/")
		// Repeated trailing slashes must stay non-matching, as in the file form.
		if strings.HasSuffix(dirPattern, "/") {
			dirPattern = p
		}
		if strings.HasSuffix(dirPattern, "/**") {
			dirPattern = strings.TrimSuffix(dirPattern, "**") + "*"
		}
		directories[i] = literalizePattern(dirPattern)
	}
	m := &Matcher{
		ign:      ignore.CompileIgnoreLines(compiled...),
		dirs:     ignore.CompileIgnoreLines(directories...),
		patterns: cleaned,
	}
	var unfiltered []string
	var unfilteredNegation bool
	seen := make(map[string]bool)
	for i, p := range cleaned {
		if strings.HasPrefix(p, "!") {
			m.hasNegation = true
			// After the negation "!", the compiler strips one more leading
			// "!" or "#", so "!!x" and "!#x" both negate "x". Their match
			// does not need that character, so they get no prefilter literal.
			if literal := requiredLiteral(p[1:]); literal != "" && !strings.HasPrefix(p[1:], "!") && !strings.HasPrefix(p[1:], "#") {
				m.negationLiterals = append(m.negationLiterals, literal)
			} else {
				unfilteredNegation = true
			}
			continue
		}
		if p == "/**" || strings.HasSuffix(p, "**/") || strings.Contains(p, `\`) {
			m.checkUnmatchedParents = true
		}
		literal := requiredLiteral(p)
		if literal == "" {
			unfiltered = append(unfiltered, compiled[i])
		} else if !seen[literal] {
			seen[literal] = true
			m.requiredLiterals = append(m.requiredLiterals, literal)
		}
	}
	if unfilteredNegation {
		m.negationLiterals = nil
	}
	if len(m.requiredLiterals) > 0 && len(unfiltered) > 0 {
		m.unfiltered = ignore.CompileIgnoreLines(unfiltered...)
	}
	return m
}

// requiredLiteral returns a substring every match of a simple glob must
// contain. Split at slashes too, since ** can consume zero path segments.
// Uncertain syntax stays unfiltered.
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

// couldNegate is conservative: folder negations can match descendants too,
// so test the whole path rather than only its final basename.
func (m *Matcher) couldNegate(rel string) bool {
	if len(m.negationLiterals) == 0 {
		return true
	}
	for _, literal := range m.negationLiterals {
		if strings.Contains(rel, literal) {
			return true
		}
	}
	return false
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

// ExcludedNegationParent returns the first excluded literal parent of a
// negation and the rule that excludes it. It is a compile-time diagnostic,
// not a matching decision. Basename and wildcard-leading negations have no
// fixed parent. Stop at wildcard or escape syntax rather than guessing a
// parent; those patterns can still apply elsewhere.
func (m *Matcher) ExcludedNegationParent(pattern string) (string, string) {
	if m == nil || m.dirs == nil || !strings.HasPrefix(pattern, "!") {
		return "", ""
	}
	rooted := strings.HasPrefix(pattern[1:], "/")
	body := strings.TrimSuffix(strings.TrimPrefix(pattern[1:], "/"), "/")
	// These exact MCP paths have an explicit Builtin exception in the
	// indexer. Do not diagnose the lifted Builtin root as a blocked parent;
	// a user's own copy of that exclusion still applies.
	if IsAgentConfigMCPDir(body) || IsAgentConfigMCPFile(body) {
		m = m.withoutBuiltinAgentRoots()
	}
	parent := ""
	for {
		segment, rest, slash := strings.Cut(body, "/")
		// The final component is the target, not one of its parents:
		// build/ then !build/ must not warn about build itself.
		if !slash || segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, `*?[\`) {
			return "", ""
		}
		if parent != "" {
			parent += "/"
		}
		parent += segment
		if excluded, ip := m.dirs.MatchesPathHow(pathkey.Normalize(parent)); excluded && ip != nil {
			// The dependency can match an unrooted negation at other
			// depths. A root-only exclude cannot prove all those matches
			// blocked. Its generated regexp uses ^(|/) for rooted rules
			// and ^(|.*/) for unrooted rules; require the latter here.
			if rooted || strings.HasPrefix(ip.Pattern.String(), "^(|.*/)") {
				_, rule := m.explainPattern(excluded, ip)
				return parent, rule
			}
		}
		body = rest
	}
}

// cleanedBuiltin is Builtin as New cleans it.
var cleanedBuiltin = sync.OnceValue(func() []string { return New(Builtin).patterns })

// withoutBuiltinAgentRoots returns m without the Builtin lines that exclude
// the agent-config roots wholesale, compiled once. Effective exclude lists
// start with Builtin; a list that does not holds only the user's lines and
// is returned as is, and a copy of the line after Builtin stays.
func (m *Matcher) withoutBuiltinAgentRoots() *Matcher {
	m.agentRootsLiftedOnce.Do(func() {
		builtin := cleanedBuiltin()
		if len(m.patterns) < len(builtin) || !slices.Equal(m.patterns[:len(builtin)], builtin) {
			m.agentRootsLifted = m
			return
		}
		patterns := make([]string, 0, len(m.patterns))
		for i, p := range m.patterns {
			if i < len(builtin) && isAgentConfigRootPattern(p) {
				continue
			}
			patterns = append(patterns, p)
		}
		m.agentRootsLifted = New(patterns)
		m.agentRootsLifted.includes = m.includes
		m.agentRootsLifted.literalIncludes = m.literalIncludes
	})
	return m.agentRootsLifted
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
	if (!m.hasNegation || !m.checkUnmatchedParents) && !m.couldMatch(rel) {
		return false
	}
	matched, _ := m.explainRel(rel, false)
	return matched
}

// MatchRelEither reports exclusion for both a file and a directory query.
// Watcher backends do not know the entry type. Check parents once, then
// evaluate the final segment against each pattern form.
func (m *Matcher) MatchRelEither(relPath string) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel := strings.TrimPrefix(pathkey.Normalize(filepath.ToSlash(relPath)), "./")
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" || rel == "." {
		return false
	}
	if (!m.hasNegation || !m.checkUnmatchedParents) && !m.couldMatch(rel) {
		return false
	}
	if (m.includes != nil || len(m.literalIncludes) > 0) && (m.IncludesRel(rel) || m.IncludesRel(rel+"/")) {
		return false
	}
	matched, ip := m.ign.MatchesPathHow(rel)
	couldNegate := m.hasNegation && m.couldNegate(rel)
	if m.hasNegation && ((couldNegate && ip != nil) || m.checkUnmatchedParents) && m.excludesParent(rel) {
		return true
	}
	if !matched {
		return false
	}
	if !couldNegate && ip != nil {
		// A matched file already crossed a directory-only marker. Only a
		// terminal /** (or repeated slash) can change this positive verdict
		// in the directory form; ordinary default rules need no second pass.
		line := strings.TrimSuffix(m.patterns[ip.LineNo-1], "/")
		if !strings.HasSuffix(line, "/") && !strings.HasSuffix(line, "/**") {
			return true
		}
	}
	return m.dirs.MatchesPath(rel)
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
	return m.explainRel(rel, true)
}

// explainRel tests the final path first, then enforces the parent rule.
// Without a positive match (even one later negated), ordinary patterns
// cannot exclude a parent. Uncertain compiler forms retain the slow check.
//
// When the final path matched and no negation line can match it, the last
// matching line is positive and the parent pass cannot change the verdict;
// it only picks which line explains it. withPattern keeps that pass for
// Explain, and MatchRel skips it.
func (m *Matcher) explainRel(rel string, withPattern bool) (bool, string) {
	if m.IncludesRel(rel) {
		return false, ""
	}
	isDir := strings.HasSuffix(rel, "/")
	rel = strings.TrimSuffix(rel, "/")
	ign := m.ign
	if isDir {
		ign = m.dirs
	}
	matched, ip := ign.MatchesPathHow(rel)
	if m.hasNegation && ((ip != nil && (withPattern || m.couldNegate(rel))) || m.checkUnmatchedParents) {
		for i := 0; i < len(rel); i++ {
			if rel[i] == '/' {
				if matched, ip := m.dirs.MatchesPathHow(rel[:i]); matched && ((m.includes == nil && len(m.literalIncludes) == 0) || !m.IncludesRel(rel[:i]+"/")) {
					return m.explainPattern(matched, ip)
				}
			}
		}
	}
	return m.explainPattern(matched, ip)
}

func (m *Matcher) explainPattern(matched bool, ip *ignore.IgnorePattern) (bool, string) {
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

// ExplainAbsDirInOpenDir tests only the final entry. A pruned filesystem
// walk must already have proved every parent open. Explicit paths and
// content-source walks must use ExplainAbsDir instead.
func (m *Matcher) ExplainAbsDirInOpenDir(absPath, root string, isDir bool) (bool, string) {
	if m == nil || m.ign == nil {
		return false, ""
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false, ""
	}
	rel = pathkey.Normalize(filepath.ToSlash(rel))
	if isDir {
		rel += "/"
	}
	if !m.couldMatch(rel) {
		return false, ""
	}
	if m.IncludesRel(rel) {
		return false, ""
	}
	ign := m.ign
	if isDir {
		ign = m.dirs
	}
	matched, ip := ign.MatchesPathHow(strings.TrimSuffix(rel, "/"))
	return m.explainPattern(matched, ip)
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
