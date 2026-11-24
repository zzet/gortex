package reporoot

import (
	"os"
	"testing"
)

// TestPathResolvesTheCheckoutThatOwnsThisSource is the trimpath fence: the
// root has to name the real checkout, not the process's directory of the
// moment, and it has to keep doing so after a test chdirs.
func TestPathResolvesTheCheckoutThatOwnsThisSource(t *testing.T) {
	self := Path(t, "internal", "testutil", "reporoot", "reporoot.go")
	if info, err := os.Stat(self); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("resolved root does not contain this package's source (%s): %v", self, err)
	}
	if _, err := os.Stat(Path(t, "go.mod")); err != nil {
		t.Fatalf("resolved root has no go.mod: %v", err)
	}

	before := Path(t)
	t.Chdir(t.TempDir())
	if after := Path(t); after != before {
		t.Fatalf("root moved with the working directory: %q then %q", before, after)
	}
}

// TestFindReportsAnUnresolvableRoot pins the loud-failure branch: a binary
// started outside any module must say so rather than hand back a path that
// happens to exist.
func TestFindReportsAnUnresolvableRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	if got, err := find(); err == nil {
		t.Fatalf("find() returned %q outside any module, want an error", got)
	}
}
