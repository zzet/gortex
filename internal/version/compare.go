package version

import (
	"strconv"
	"strings"
)

// Compare orders two Versions by SemVer 2.0.0 precedence:
// major.minor.patch numerically, then pre-release (absent outranks
// present; identifiers compared per comparePrereleaseIdent). Build
// metadata is ignored — per the spec it never affects precedence.
// Returns -1, 0, or +1.
func Compare(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return compareInt(a.Major, b.Major)
	case a.Minor != b.Minor:
		return compareInt(a.Minor, b.Minor)
	case a.Patch != b.Patch:
		return compareInt(a.Patch, b.Patch)
	case a.Prerelease == b.Prerelease:
		return 0
	case a.Prerelease == "":
		return 1 // the release outranks any of its pre-releases
	case b.Prerelease == "":
		return -1
	default:
		return comparePrerelease(a.Prerelease, b.Prerelease)
	}
}

// comparePrerelease compares two dot-separated pre-release identifier
// lists per SemVer 2.0.0: identifier by identifier, and once every
// shared identifier is equal, the shorter list ranks below the longer.
func comparePrerelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := comparePrereleaseIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return compareInt(len(as), len(bs))
}

// comparePrereleaseIdent compares one pre-release identifier pair:
// numeric identifiers compare numerically and rank below alphanumeric
// ones; alphanumeric identifiers compare in ASCII sort order.
func comparePrereleaseIdent(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	switch {
	case aErr == nil && bErr == nil:
		return compareInt(an, bn)
	case aErr == nil:
		return -1 // numeric identifiers rank below alphanumeric
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// compareInt is the three-way integer compare the semver helpers use.
func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
