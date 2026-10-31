package indexer

import "strings"

// gitBlobObjectNameValid reports whether rel is safe to embed as the path
// half of a `git cat-file` object name (`<sha>:<rel>`). git reads the path
// verbatim after the first colon and refuses `../` traversal, so this does
// not add correctness — but rel is a repository-controlled name, and control
// characters make error messages and diagnostics ambiguous, so reject them
// up front rather than embedding a filename that could confuse the next
// consumer of the object name.
func gitBlobObjectNameValid(rel string) bool {
	return !strings.ContainsAny(rel, "\x00\n\r\t\v\f")
}
