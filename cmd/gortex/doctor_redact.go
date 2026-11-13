package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// doctor_redact.go makes --redact mean something. The report is built to be
// pasted into an issue, and almost everything identifying in it is a path:
// the repo name in every planned-file line, the account name in every home
// path. Hashing the two roots keeps the structure legible — `<repo>/.mcp.json`
// still reads as a config path — while removing the parts a user would
// otherwise blur out by hand before sharing.

// doctorPath rewrites one path for display. Replaced for the duration of a
// redacted run; the identity default keeps the normal report verbatim.
var doctorPath = func(p string) string { return p }

// doctorLabel rewrites a non-path identifier for display: a family remote or a
// repo prefix.
//
// Redacting only the root left `display_remote` and `repo_prefix` verbatim in
// the JSON, and the human rendering printed the raw prefix. A --redact report
// could therefore still name a private remote and the local family on disk,
// which is the one thing the flag exists to prevent. Paths were hashed while
// identity was not.
var doctorLabel = func(s string) string { return s }

// newDoctorLabelRedactor replaces identifiers with stable per-run pseudonyms.
//
// Stability within a report is what matters: two findings about the same family
// must still read as the same family, or the report stops being a diagnosis. The
// mapping is therefore per-run and ordinal (<family-1>, <family-2>, ...), which
// leaks nothing about the identifier -- not even its length or shape -- and does
// not let two reports be correlated by a shared pseudonym. The trade is that the
// same repo is <family-1> in one report and <family-2> in the next; a reader who
// needs cross-report linkage is asking for less redaction than this flag offers.
func newDoctorLabelRedactor() func(string) string {
	seen := make(map[string]string)
	next := 0
	return func(s string) string {
		if strings.TrimSpace(s) == "" {
			return s
		}
		if p, ok := seen[s]; ok {
			return p
		}
		next++
		p := fmt.Sprintf("<family-%d>", next)
		seen[s] = p
		return p
	}
}

// newDoctorPathRedactor rewrites paths under the repo root to <repo>/… and
// paths under the home directory to ~/…. Repo first: the repo usually lives
// under home, and its name is the more sensitive of the two.
func newDoctorPathRedactor(home, root string) func(string) string {
	return func(p string) string {
		if strings.TrimSpace(p) == "" {
			return p
		}
		clean := filepath.Clean(p)
		if rel, ok := relativeTo(root, clean); ok {
			return filepath.Join("<repo>", rel)
		}
		if rel, ok := relativeTo(home, clean); ok {
			return filepath.Join("~", rel)
		}
		// Outside both roots: no name of ours to leak, so leave it readable.
		return clean
	}
}

// relativeTo reports base-relative form when path is inside base. "." maps to
// the empty string so the caller's Join yields the bare placeholder.
func relativeTo(base, path string) (string, bool) {
	if strings.TrimSpace(base) == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(base), path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}
