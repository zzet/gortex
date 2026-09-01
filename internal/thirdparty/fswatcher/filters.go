package fswatcher

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// PathFilter defines the interface for path filtering logic
type PathFilter interface {
	ShouldInclude(path string) bool
}

// patternFilter implements PathFilter using include/exclude regex patterns
type patternFilter struct {
	includePatterns []*regexp.Regexp
	excludePatterns []*regexp.Regexp
	mu              sync.RWMutex
}

// newPatternFilter creates a new filter with the given regex patterns
func newPatternFilter(include, exclude []*regexp.Regexp) *patternFilter {
	return &patternFilter{
		includePatterns: include,
		excludePatterns: exclude,
	}
}

// ShouldInclude determines if a path should be processed, excluded patterns take precedence over included
func (f *patternFilter) ShouldInclude(path string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	base := filepath.Base(path)

	for _, re := range f.excludePatterns {
		if re.MatchString(path) || re.MatchString(base) {
			return false
		}
	}

	if len(f.includePatterns) == 0 {
		return true
	}

	for _, re := range f.includePatterns {
		if re.MatchString(path) || re.MatchString(base) {
			return true
		}
	}

	return false
}

// isSubpath reports whether the child path is inside the parent directory
func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !startsWithDotDot(rel)
}

// startsWithDotDot checks if a relative path string starts with "../"
func startsWithDotDot(rel string) bool {
	return len(rel) >= 2 && rel[:2] == ".."
}

// isSystemFile checks if a path is likely a temporary or system-generated file
func isSystemFile(path string) bool {
	base := filepath.Base(path)

	for _, prefix := range osPrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	for _, suffix := range osSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}
