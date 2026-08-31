package pathkey

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CaseInsensitivePaths reports whether path identity comparisons should
// ignore letter case. The default is chosen for the host filesystem:
// true on Windows and macOS (whose default NTFS / APFS volumes are
// case-insensitive), false on Linux.
//
// The environment override GORTEX_CASE_SENSITIVE_PATHS lets an operator
// correct the default for an unusual mount:
//
//   - =1 (or true/yes) forces case-SENSITIVE comparisons — for a
//     case-sensitive APFS or NTFS volume on macOS / Windows.
//   - =0 (or false/no) forces case-INSENSITIVE comparisons — for a
//     case-insensitive mount (e.g. an exFAT / SMB share) on Linux.
//
// It is a settable package variable rather than a constant so that
// higher-layer tests can flip it deterministically on any CI platform.
// A test that flips it MUST restore it via t.Cleanup and MUST NOT call
// t.Parallel — the variable is process-global.
var CaseInsensitivePaths bool

func init() {
	CaseInsensitivePaths = defaultCaseInsensitive()
}

// defaultCaseInsensitive resolves the initial value of CaseInsensitivePaths
// from the environment override, falling back to the host GOOS default.
func defaultCaseInsensitive() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_CASE_SENSITIVE_PATHS"))) {
	case "1", "true", "yes", "on":
		return false
	case "0", "false", "no", "off":
		return true
	}
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// foldPath reduces p to a canonical identity key: filepath.Clean removes
// redundant separators and dot segments, Normalize folds Unicode to NFC,
// the Windows-style volume component (drive letter or UNC root) is
// upper-cased so "c:" and "C:" agree, and — when ci is true — the
// remainder is lower-cased so a case-insensitive filesystem treats
// "Documents" and "documents" as one path.
//
// It is a pure function of (string, bool): the volume is detected with
// host-independent semantics (see volumeNameLen) so Windows and macOS
// folding can be table-tested on a Linux CI runner. It never touches the
// filesystem and never resolves symlinks — identity is lexical.
func foldPath(p string, ci bool) string {
	p = filepath.Clean(p)
	p = Normalize(p)
	vl := volumeNameLen(p)
	vol := strings.ToUpper(p[:vl])
	rest := p[vl:]
	if ci {
		rest = strings.ToLower(rest)
	}
	return vol + rest
}

// EqualPaths reports whether two absolute paths identify the same
// location once folded. A byte-equal fast path mirrors Equal so the
// common case allocates nothing.
func EqualPaths(a, b string) bool {
	if a == b {
		return true
	}
	return foldPath(a, CaseInsensitivePaths) == foldPath(b, CaseInsensitivePaths)
}

// HasPathPrefix reports whether path is root itself or lies beneath it,
// comparing on folded identity and respecting path-component boundaries:
// "/Users/me/Doc" is not under "/Users/me/Documents". The volume root
// ("/" on POSIX, "C:\" on Windows) already ends in a separator after
// filepath.Clean, which the trailing-separator branch handles.
func HasPathPrefix(path, root string) bool {
	fp := foldPath(path, CaseInsensitivePaths)
	fr := foldPath(root, CaseInsensitivePaths)
	if fp == fr {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasSuffix(fr, sep) {
		return strings.HasPrefix(fp, fr)
	}
	return strings.HasPrefix(fp, fr+sep)
}

// NormalizeVolume returns p with its Windows-style volume component
// (drive letter or UNC root) upper-cased, e.g. "c:\x" -> "C:\x". It is a
// no-op when there is no volume component (every POSIX path) and when the
// volume is already upper-cased, so it never allocates on the common path.
//
// It is applied at store time to NEW tracked-repo entries only, to
// converge cosmetically with os.Getwd's convention (which upper-cases the
// drive letter on Windows). The volume is never part of a repo basename,
// so re-casing it cannot rotate a repo prefix or node IDs. It must never
// be applied to an already-stored path.
func NormalizeVolume(p string) string {
	vl := volumeNameLen(p)
	if vl == 0 {
		return p
	}
	up := strings.ToUpper(p[:vl])
	if up == p[:vl] {
		return p
	}
	return up + p[vl:]
}

// SamePathIdentity is the store-time dedup predicate. It treats a and b
// as the same repo when they fold equal AND, when they differ byte-wise
// but both stat cleanly, os.SameFile confirms they resolve to the same
// directory — so two genuinely distinct directories on a case-sensitive
// volume are never merged. If either path fails to stat, the fold is
// trusted: a path that cannot be stat'd cannot be a distinct live repo.
//
// It stats the filesystem, so it must never be called on a per-request
// hot path — only when adding, removing, or deduping a tracked entry.
func SamePathIdentity(a, b string) bool {
	if a == b {
		return true
	}
	if !EqualPaths(a, b) {
		return false
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr != nil || berr != nil {
		return true
	}
	return os.SameFile(ai, bi)
}

// CanonicalExistingRoot returns a stable absolute identity for an existing
// filesystem root. It resolves aliases such as macOS's /tmp -> /private/tmp,
// while retaining the clean absolute spelling when symlink resolution is not
// possible (for example, when a previously configured root is offline).
func CanonicalExistingRoot(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	abs = NormalizeVolume(filepath.Clean(abs))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return NormalizeVolume(filepath.Clean(resolved))
}

// CanonicalHasPathPrefix reports lexical containment first, then retries after
// canonicalizing both operands. The lexical fast path is not only cheaper for
// the ordinary same-spelling case: it preserves containment when a configured
// child has gone offline and therefore cannot be symlink-resolved while its
// still-existing parent can. The canonical fallback keeps routing across
// aliases such as macOS's /tmp -> /private/tmp.
func CanonicalHasPathPrefix(path, prefix string) bool {
	if HasPathPrefix(path, prefix) {
		return true
	}
	return HasPathPrefix(CanonicalExistingRoot(path), CanonicalExistingRoot(prefix))
}

// volumeNameLen returns the length of the leading Windows-style volume
// component of p — a drive letter ("c:") or a UNC root ("\\host\share").
// It uses Windows semantics regardless of the host OS so that path
// folding is deterministic and testable on every platform; a POSIX path
// (which begins with "/") has no volume and yields 0. Both '\\' and '/'
// are accepted as separators because a Windows path may arrive with
// either.
func volumeNameLen(p string) int {
	if len(p) < 2 {
		return 0
	}
	// Drive letter, e.g. "c:".
	if p[1] == ':' && isDriveLetter(p[0]) {
		return 2
	}
	// UNC root, e.g. "\\host\share" or "//host/share".
	if isAnySeparator(p[0]) && isAnySeparator(p[1]) {
		n := len(p)
		i := 2
		host := i
		for i < n && !isAnySeparator(p[i]) {
			i++
		}
		if i == host {
			return 0 // "\\" with no host is not a UNC volume
		}
		if i < n {
			i++ // consume the separator before the share name
		}
		for i < n && !isAnySeparator(p[i]) {
			i++
		}
		return i
	}
	return 0
}

func isDriveLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isAnySeparator(c byte) bool {
	return c == '\\' || c == '/'
}
