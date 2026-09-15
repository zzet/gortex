// Package testdsn builds the SQLite "file:" URI a test needs when it opens a
// store it does not own the connection settings for — a read-only audit probe,
// a downgrade fixture, a direct counter read beside a running builder.
//
// It exists because the obvious spelling is wrong on Windows and right
// everywhere else, so every copy of it passed review and then failed on one
// leg of CI. `(&url.URL{Scheme: "file", Path: path}).String()` renders a POSIX
// path as file:///tmp/store.sqlite, but a Windows path as
// file://C:%5CUsers%5C... — url.URL writes the "//" that introduces an
// authority whenever a scheme and a path are both present, and a path that
// does not start with "/" then lands in the authority position. SQLite parses
// everything up to the next "/" as the authority and refuses the DSN with
// "invalid uri authority: C:%5CUsers%5C...", so the test cannot open the store
// at all.
//
// The rules here are the production builder's (store_sqlite.sqliteDSN): make
// the path absolute, fold separators to "/", prefix a "/" when the result does
// not already begin with one so a drive letter sits in the path rather than
// the authority, and let url.URL do the percent-escaping. Keeping one copy in
// one package is what stops the next fixture from re-deriving the broken one.
package testdsn

import (
	"net/url"
	"path/filepath"
	"strings"
)

// FileURI returns the SQLite URI for path with rawQuery attached.
//
// An explicit URI or the in-memory sentinel is passed through with rawQuery
// appended, matching the production builder: such a caller has already chosen
// its own spelling and may carry cache=shared or mode=memory in it.
func FileURI(path, rawQuery string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		if rawQuery == "" {
			return path
		}
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		return path + separator + rawQuery
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return fileURI(path, rawQuery)
}

// fileURI is FileURI's rendering half, split out so it can be exercised with
// an absolute path in the *other* platform's spelling. filepath.Abs and
// filepath.ToSlash both answer for the running OS, which would make a
// Windows-path test on a POSIX host prove nothing; this half folds backslashes
// on every platform instead. The cost is that a POSIX file whose name
// genuinely contains a backslash would be re-spelled — no store path a test
// hands this helper does.
func fileURI(path, rawQuery string) string {
	slashed := strings.ReplaceAll(path, `\`, "/")
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed, RawQuery: rawQuery}).String()
}
