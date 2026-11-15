package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// newDoctorRootRedactor replaces root paths with stable per-run aliases.
//
// The path redactor (newDoctorPathRedactor) rewrites paths under the repo root
// to <repo>/... and paths under home to ~/.... That is right for paths that
// live inside the two roots we know about. It is wrong for the roots the
// legacy-state census reports: a checkout root in a sibling home, or outside
// home entirely, is returned verbatim -- "no name of ours to leak" is true for
// the report's own paths, but not for a path that identifies ANOTHER
// repository.
//
// So the census gets its own redactor: every root it reports, wherever it
// lives, becomes a stable per-run alias. Correlations survive (the same root
// always gets the same alias) and the original path does not.
func newDoctorRootRedactor() func(string) string {
	seen := make(map[string]string)
	next := 0
	return func(p string) string {
		if strings.TrimSpace(p) == "" {
			return p
		}
		clean := filepath.Clean(p)
		if alias, ok := seen[clean]; ok {
			return alias
		}
		next++
		alias := fmt.Sprintf("<root-%d>", next)
		seen[clean] = alias
		return alias
	}
}
