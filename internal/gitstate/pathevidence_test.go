package gitstate

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The path argument is unused by the Unix implementation and would look
// removable to a cleanup pass, so the seam's shape is pinned here: a
// platform whose identity lives behind an open handle can only reopen the
// object by path, and dropping the argument would make such an
// implementation impossible to write.
var _ func(string, fs.FileInfo) (string, string, string) = pathIdentity

func TestSamplePathEvidenceExistingRoot(t *testing.T) {
	root := realPath(t, t.TempDir())
	child := filepath.Join(root, "checkout")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ev := SamplePathEvidence(child)
	if !ev.RootExists {
		t.Fatalf("expected RootExists for %q", child)
	}
	if ev.AncestorPath != root {
		t.Errorf("AncestorPath = %q, want %q", ev.AncestorPath, root)
	}
	if ev.VolumeKind == VolumeKindUnsupported {
		if ev.VolumeToken != "" || ev.RootIdentity != "" {
			t.Errorf("unsupported platform must report empty tokens: %+v", ev)
		}
		return
	}
	if ev.VolumeKind != VolumeKindUnixDev {
		t.Fatalf("VolumeKind = %q, want %q", ev.VolumeKind, VolumeKindUnixDev)
	}
	if ev.VolumeToken == "" || ev.RootIdentity == "" {
		t.Fatalf("expected non-empty tokens: %+v", ev)
	}
	if ev.AncestorVolumeKind != VolumeKindUnixDev || ev.AncestorVolumeToken != ev.VolumeToken {
		t.Errorf("ancestor on the same volume should share the token: %+v", ev)
	}

	// Identity is stable across repeated samples and distinct between
	// two different directories on the same volume.
	again := SamplePathEvidence(child)
	if again.RootIdentity != ev.RootIdentity {
		t.Errorf("identity changed between samples: %q vs %q", again.RootIdentity, ev.RootIdentity)
	}
	sibling := filepath.Join(root, "other")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sib := SamplePathEvidence(sibling)
	if sib.RootIdentity == ev.RootIdentity {
		t.Error("two directories share one identity")
	}
	if sib.VolumeToken != ev.VolumeToken {
		t.Errorf("siblings should share a volume token: %q vs %q", sib.VolumeToken, ev.VolumeToken)
	}
}

func TestSamplePathEvidenceMissingRoot(t *testing.T) {
	root := realPath(t, t.TempDir())
	missing := filepath.Join(root, "deep", "deeper", "gone")

	ev := SamplePathEvidence(missing)
	if ev.RootExists {
		t.Fatalf("expected RootExists=false for %q", missing)
	}
	if ev.RootIdentity != "" || ev.VolumeKind != "" || ev.VolumeToken != "" {
		t.Errorf("a missing root must carry no identity: %+v", ev)
	}
	if ev.AncestorPath != root {
		t.Errorf("AncestorPath = %q, want the nearest existing ancestor %q", ev.AncestorPath, root)
	}
	if ev.AncestorVolumeKind == VolumeKindUnsupported {
		return
	}
	if ev.AncestorVolumeToken == "" {
		t.Error("ancestor volume token should prove the volume is still mounted")
	}
	// The ancestor's volume token is the same one the root reported
	// while it still existed, which is what makes the absence provable.
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	present := SamplePathEvidence(missing)
	if present.VolumeToken != ev.AncestorVolumeToken {
		t.Errorf("volume token %q does not match the recorded ancestor token %q",
			present.VolumeToken, ev.AncestorVolumeToken)
	}
}

func TestSamplePathEvidenceIndependentOfSymlinkedTempDir(t *testing.T) {
	// t.TempDir() may hand back a path whose parents are symlinks
	// (macOS routes /var through /private/var). The identity of the
	// directory must not depend on which spelling is sampled.
	raw := t.TempDir()
	resolved := realPath(t, raw)

	rawEv := SamplePathEvidence(raw)
	resolvedEv := SamplePathEvidence(resolved)
	if !rawEv.RootExists || !resolvedEv.RootExists {
		t.Fatalf("both spellings should exist: %+v / %+v", rawEv, resolvedEv)
	}
	if rawEv.RootIdentity != resolvedEv.RootIdentity {
		t.Errorf("identity differs by spelling: %q (%s) vs %q (%s)",
			rawEv.RootIdentity, raw, resolvedEv.RootIdentity, resolved)
	}
}

func TestSamplePathEvidenceDoesNotFollowRootSymlink(t *testing.T) {
	root := realPath(t, t.TempDir())
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	targetEv := SamplePathEvidence(target)
	linkEv := SamplePathEvidence(link)
	if !linkEv.RootExists {
		t.Fatal("the symlink itself should be reported as existing")
	}
	if linkEv.VolumeKind == VolumeKindUnsupported {
		t.Skip("platform reports no path identity")
	}
	if linkEv.RootIdentity == targetEv.RootIdentity {
		t.Error("the symlink was followed; identity must belong to the link itself")
	}
}

func TestSamplePathEvidenceOnBlankPath(t *testing.T) {
	ev := SamplePathEvidence("")
	if ev.RootExists || ev.AncestorPath != "" || ev.RootIdentity != "" {
		t.Errorf("blank path should yield empty evidence: %+v", ev)
	}
}

func TestSamplePathEvidenceOnFilesystemRoot(t *testing.T) {
	// The ancestor walk must terminate at the filesystem root rather
	// than looping forever. A bare separator is the root only once it
	// carries a volume, which is what the sampler absolutizes it to
	// before walking (a no-op on POSIX, "C:\" on Windows).
	fsRoot, err := filepath.Abs(string(filepath.Separator))
	if err != nil {
		t.Fatalf("resolve the filesystem root: %v", err)
	}

	ev := SamplePathEvidence(string(filepath.Separator))
	if !ev.RootExists {
		t.Fatal("the filesystem root should exist")
	}
	if ev.AncestorPath != fsRoot {
		t.Errorf("AncestorPath = %q, want the filesystem root %q", ev.AncestorPath, fsRoot)
	}
}
