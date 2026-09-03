package gitstate

import (
	"path/filepath"
	"strings"
	"testing"
)

// entries joins porcelain attribute entries into the NUL-delimited
// stream git emits: every attribute is NUL-terminated and an empty
// entry closes each record.
func entries(records ...[]string) []byte {
	var b strings.Builder
	for _, rec := range records {
		for _, attr := range rec {
			b.WriteString(attr)
			b.WriteByte(0)
		}
		b.WriteByte(0)
	}
	return []byte(b.String())
}

func TestParsePorcelainZKeepsNewlinesInValues(t *testing.T) {
	out := entries(
		[]string{
			"worktree /repos/main",
			"HEAD 1111111111111111111111111111111111111111",
			"branch refs/heads/main",
		},
		[]string{
			"worktree /repos/wt\nnewline and space",
			"HEAD 2222222222222222222222222222222222222222",
			"detached",
			"locked because\nthe reason spans lines",
		},
		[]string{
			"worktree /repos/stale",
			"HEAD 3333333333333333333333333333333333333333",
			"branch refs/heads/release/2.0",
			"prunable gitdir file points to non-existent location",
		},
	)

	got := parsePorcelainZ(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d: %+v", len(got), got)
	}

	if got[0].Path != "/repos/main" || got[0].Branch != "main" {
		t.Errorf("first record = %+v", got[0])
	}

	second := got[1]
	if second.Path != "/repos/wt\nnewline and space" {
		t.Errorf("second path = %q, newline or space lost", second.Path)
	}
	if !second.Detached || second.Branch != "" {
		t.Errorf("second record should be detached: %+v", second)
	}
	if !second.Locked || second.LockReason != "because\nthe reason spans lines" {
		t.Errorf("second lock = %v / %q", second.Locked, second.LockReason)
	}

	third := got[2]
	if third.Branch != "release/2.0" || third.HEADRef != "refs/heads/release/2.0" {
		t.Errorf("slash in branch name mangled: %q / %q", third.Branch, third.HEADRef)
	}
	if !third.Prunable || third.PruneReason != "gitdir file points to non-existent location" {
		t.Errorf("third prune = %v / %q", third.Prunable, third.PruneReason)
	}
}

func TestParsePorcelainZNormalizesHEAD(t *testing.T) {
	out := entries(
		[]string{
			"worktree /repos/fresh",
			"HEAD 0000000000000000000000000000000000000000",
			"branch refs/heads/main",
		},
		[]string{
			"worktree /repos/bare",
			"bare",
		},
		[]string{
			"worktree /repos/garbage",
			"HEAD fatal: something went wrong",
			"branch refs/heads/main",
		},
	)

	got := parsePorcelainZ(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	if got[0].HEADOID != "" || !got[0].Unborn {
		t.Errorf("all-zero HEAD should read as unborn with no OID: %+v", got[0])
	}
	if !got[1].Bare || got[1].Unborn {
		t.Errorf("a bare entry is bare and not unborn: %+v", got[1])
	}
	if got[2].HEADOID != "" {
		t.Errorf("non-OID HEAD value leaked through: %q", got[2].HEADOID)
	}
}

func TestParsePorcelainZIgnoresUnknownAttributes(t *testing.T) {
	out := entries([]string{
		"worktree /repos/main",
		"HEAD 4444444444444444444444444444444444444444",
		"branch refs/heads/main",
		"somethingnew with a value",
	})

	got := parsePorcelainZ(out)
	if len(got) != 1 {
		t.Fatalf("an unknown attribute split the record: %+v", got)
	}
	if got[0].Branch != "main" {
		t.Errorf("record = %+v", got[0])
	}
}

func TestParsePorcelainZOnEmptyOutput(t *testing.T) {
	if got := parsePorcelainZ(nil); len(got) != 0 {
		t.Errorf("expected no records, got %+v", got)
	}
	if got := parsePorcelainZ([]byte{0, 0}); len(got) != 0 {
		t.Errorf("expected no records from a lone separator, got %+v", got)
	}
}

func TestSplitAttribute(t *testing.T) {
	cases := []struct{ in, key, value string }{
		{"locked", "locked", ""},
		{"locked because", "locked", "because"},
		{"worktree /a b/c", "worktree", "/a b/c"},
		{"branch refs/heads/x", "branch", "refs/heads/x"},
	}
	for _, c := range cases {
		key, value := splitAttribute(c.in)
		if key != c.key || value != c.value {
			t.Errorf("splitAttribute(%q) = %q / %q, want %q / %q", c.in, key, value, c.key, c.value)
		}
	}
}

func TestObjectIDValidation(t *testing.T) {
	valid := []string{
		strings.Repeat("a", 40),
		strings.Repeat("0", 40),
		strings.Repeat("f", 64),
	}
	for _, s := range valid {
		if !isOID(s) {
			t.Errorf("isOID(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", 39),
		strings.Repeat("a", 65),
		strings.Repeat("A", 40),
		"fatal: not a git repository",
		strings.Repeat("a", 39) + "z",
	}
	for _, s := range invalid {
		if isOID(s) {
			t.Errorf("isOID(%q) = true, want false", s)
		}
	}

	if !isZeroOID(strings.Repeat("0", 40)) {
		t.Error("all-zero OID not recognized")
	}
	if isZeroOID("") || isZeroOID(strings.Repeat("a", 40)) {
		t.Error("isZeroOID matched a non-zero value")
	}
}

func TestTwoPathsAndResolveAgainst(t *testing.T) {
	first, second, ok := twoPaths("/a/.git\n/b/.git\n")
	if !ok || first != "/a/.git" || second != "/b/.git" {
		t.Errorf("twoPaths = %q / %q / %v", first, second, ok)
	}
	if _, _, ok := twoPaths("/a/.git\n"); ok {
		t.Error("twoPaths accepted a single line")
	}
	if _, _, ok := twoPaths("/a\n/b\n/c\n"); ok {
		t.Error("twoPaths accepted three lines")
	}
	if _, _, ok := twoPaths(""); ok {
		t.Error("twoPaths accepted empty output")
	}

	// The anchor and the already-absolute path are built from a real
	// temporary directory: a "/repo" literal carries no volume, so on
	// Windows it is not an absolute path at all and resolveAgainst would
	// join it onto the anchor instead of returning it.
	repo := filepath.Join(t.TempDir(), "repo")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere", ".git")

	if got := resolveAgainst(repo, ".git"); got != filepath.Join(repo, ".git") {
		t.Errorf("resolveAgainst relative = %q", got)
	}
	if got := resolveAgainst(repo, "."); got != repo {
		t.Errorf("resolveAgainst dot = %q", got)
	}
	if got := resolveAgainst(repo, elsewhere); got != elsewhere {
		t.Errorf("resolveAgainst absolute = %q", got)
	}
	if got := resolveAgainst(repo, ""); got != "" {
		t.Errorf("resolveAgainst empty = %q", got)
	}
}

func TestAbsDirRejectsBlank(t *testing.T) {
	if _, err := absDir(""); err == nil {
		t.Error("absDir accepted an empty path")
	}
	if _, err := absDir("   "); err == nil {
		t.Error("absDir accepted a blank path")
	}
	got, err := absDir(".")
	if err != nil {
		t.Fatalf("absDir: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("absDir(%q) = %q, want an absolute path", ".", got)
	}
}
