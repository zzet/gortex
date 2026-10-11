//go:build windows

package gitstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPathEvidenceBindsDirectoryObject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	before := SamplePathEvidence(root)
	if before.VolumeKind != VolumeKindWindowsFileID || before.RootIdentity == "" || before.VolumeToken == "" {
		t.Fatalf("Windows directory must expose physical identity: %+v", before)
	}
	if again := SamplePathEvidence(root); again.RootIdentity != before.RootIdentity {
		t.Fatalf("unchanged directory identity changed: %+v / %+v", before, again)
	}
	// A case alias names the same Windows object, not a new checkout root.
	alias := filepath.Join(filepath.Dir(root), strings.ToUpper(filepath.Base(root)))
	if aliased := SamplePathEvidence(alias); aliased.RootIdentity != before.RootIdentity {
		t.Fatalf("case alias changed directory identity: %+v / %+v", before, aliased)
	}
	// Keep the original directory alive at another name so its file index
	// cannot be recycled while a replacement is created at the admitted path.
	moved := root + "-original"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if renamed := SamplePathEvidence(moved); renamed.RootIdentity != before.RootIdentity {
		t.Fatalf("rename changed object identity: %+v / %+v", before, renamed)
	}
	if missing := SamplePathEvidence(root); missing.RootExists || missing.RootIdentity != "" {
		t.Fatalf("missing admitted path retained identity: %+v", missing)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	replaced := SamplePathEvidence(root)
	if replaced.RootIdentity == "" || replaced.RootIdentity == before.RootIdentity {
		t.Fatalf("replacement reused admitted root identity: %+v / %+v", before, replaced)
	}
	if replaced.VolumeToken != before.VolumeToken {
		t.Fatalf("same-volume replacement changed volume token: %+v / %+v", before, replaced)
	}
}

func TestWindowsPathEvidenceLongDirectory(t *testing.T) {
	root := t.TempDir()
	for len(root) < 320 {
		root = filepath.Join(root, "long-directory-segment")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatal(err)
	}
	evidence := SamplePathEvidence(root)
	if evidence.VolumeKind != VolumeKindWindowsFileID || evidence.RootIdentity == "" {
		t.Fatalf("long root accepted by Lstat must expose identity: %+v", evidence)
	}
	extended := SamplePathEvidence(windowsIdentityPath(root))
	if extended.RootIdentity != evidence.RootIdentity {
		t.Fatalf("extended spelling changed physical identity: %+v / %+v", evidence, extended)
	}
}

func TestWindowsIdentityPathPreservesNamespace(t *testing.T) {
	long := strings.Repeat("segment", 40)
	for _, tc := range []struct{ path, want string }{
		{`C:\checkout`, `C:\checkout`},
		{`C:\` + long, `\\?\C:\` + long},
		{`\\server\share\` + long, `\\?\UNC\server\share\` + long},
		{`\\?\C:\` + long, `\\?\C:\` + long},
		{`\\?\UNC\server\share\` + long, `\\?\UNC\server\share\` + long},
		{`\??\C:\` + long, `\??\C:\` + long},
		{`\\.\` + long, `\\.\` + long},
	} {
		if got := windowsIdentityPath(tc.path); got != tc.want {
			t.Errorf("windowsIdentityPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
