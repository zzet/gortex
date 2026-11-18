package excludes

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/pathkey"
)

// NewWithIncludes compiles force-includes separately from gitignore rules.
// Anchored literals open their ancestors; basename literals need open parents.
func NewWithIncludes(patterns, includes []string) *Matcher {
	m := New(patterns)
	var globs []string
	for _, p := range includes {
		p = pathkey.Normalize(filepath.ToSlash(strings.TrimSpace(p)))
		p = strings.TrimPrefix(p, "!")
		p = strings.TrimPrefix(p, "./")
		if p == "" {
			continue
		}
		anchored := strings.HasPrefix(p, "/") || strings.Contains(strings.TrimSuffix(p, "/"), "/")
		if !anchored || strings.ContainsAny(p, `*?[\`) {
			if anchored && !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			globs = append(globs, p)
		} else {
			m.literalIncludes = append(m.literalIncludes, strings.TrimPrefix(p, "/"))
		}
	}
	if len(globs) > 0 {
		m.includes = New(globs)
	}
	return m
}

// ParentLimitedIncludes returns includes that cannot reopen their ancestors.
func (m *Matcher) ParentLimitedIncludes() []string {
	if m == nil {
		return nil
	}
	return m.includes.Patterns()
}

// IncludesRel reports a force-include. A wildcard or basename entry can
// reopen its target folder, but cannot reach through a closed ancestor.
// Only anchored literals open ancestors. Callers still enforce hard exclusions
// and per-directory ignore files independently.
func (m *Matcher) IncludesRel(rel string) bool {
	if m == nil || (m.includes == nil && len(m.literalIncludes) == 0) {
		return false
	}
	rel = strings.TrimPrefix(pathkey.Normalize(filepath.ToSlash(rel)), "./")
	if m.includesLiteral(rel) {
		return true
	}
	if m.includes == nil || !m.includes.couldMatch(rel) {
		return false
	}
	for i := range rel {
		if rel[i] != '/' {
			continue
		}
		// Test the target before its exclusion: includes override excludes.
		if m.includes.dirs.MatchesPath(rel[:i]) {
			return true
		}
		if !m.includesLiteral(rel[:i]+"/") && m.dirs.MatchesPath(rel[:i]) {
			return false
		}
	}
	return !strings.HasSuffix(rel, "/") && m.includes.ign.MatchesPath(rel)
}

func (m *Matcher) includesLiteral(rel string) bool {
	isDir := strings.HasSuffix(rel, "/")
	node := strings.TrimSuffix(rel, "/")
	for _, p := range m.literalIncludes {
		target := strings.TrimSuffix(p, "/")
		if node == target || strings.HasPrefix(node, target+"/") || (isDir && strings.HasPrefix(target, node+"/")) {
			return true
		}
	}
	return false
}

func (m *Matcher) excludesParent(rel string) bool {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && m.dirs.MatchesPath(rel[:i]) && ((m.includes == nil && len(m.literalIncludes) == 0) || !m.IncludesRel(rel[:i]+"/")) {
			return true
		}
	}
	return false
}
