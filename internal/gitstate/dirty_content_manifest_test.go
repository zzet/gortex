package gitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dirtyManifestFixture lays out one of every per-path content shape the
// sampler distinguishes and returns the porcelain stream describing it.
// Without the opaque directory nothing in it depends on timestamps, so the
// fingerprint it yields is a stable golden value; an opaque directory's
// identity carries its stat evidence and is excluded from the golden.
func dirtyManifestFixture(t *testing.T, withOpaque bool) (string, []byte) {
	t.Helper()
	root := t.TempDir()
	blob := func(contents string) string {
		t.Helper()
		sha1, _, err := hashDirtyReader(context.Background(), strings.NewReader(contents), int64(len(contents)))
		if err != nil {
			t.Fatal(err)
		}
		return sha1
	}
	zero := strings.Repeat("0", 40)
	headOld := blob("committed body\n")
	dirtyContentWrite(t, root, "mod.txt", "edited body\n")
	dirtyContentWrite(t, root, "new.txt", "untracked body\n")
	exec := dirtyContentWrite(t, root, "exec.sh", "#!/bin/sh\n")
	if err := os.Chmod(exec, 0o755); err != nil {
		t.Fatal(err)
	}
	dirtyContentWrite(t, root, "same.txt", "restored body\n")
	records := []string{
		"1 .M N... 100644 100644 100644 " + headOld + " " + headOld + " mod.txt",
		"1 .D N... 100644 100644 000000 " + headOld + " " + headOld + " gone.txt",
		"? new.txt",
		"1 .M N... 100644 100644 100755 " + blob("#!/bin/sh\n") + " " + blob("#!/bin/sh\n") + " exec.sh",
		"1 M. N... 100644 100644 100644 " + blob("restored body\n") + " " + headOld + " same.txt",
		"1 AD N... 000000 100644 000000 " + zero + " " + headOld + " ghost.txt",
	}
	if withOpaque {
		if err := os.Mkdir(filepath.Join(root, "module"), 0o755); err != nil {
			t.Fatal(err)
		}
		records = append(records, "1 .M S.M. 160000 160000 160000 "+headOld+" "+headOld+" module")
	}
	return root, dirtyContentStatus(records...)
}

// dirtyManifestGoldenFingerprint is the fingerprint the sampler produced for
// dirtyManifestFixture before per-path contents were exposed. Exposing the
// contents must never move it.
const dirtyManifestGoldenFingerprint = "fa408adde46df66c14dc942b3f95f876971fd8854e9c8bb631a3dee66bb689ac"

func TestSampleDirtyFingerprintUnchangedByContents(t *testing.T) {
	root, status := dirtyManifestFixture(t, false)
	calls := 0
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return status, nil
	})
	got := dirtyContentSample(t, s)
	if got.Fingerprint != dirtyManifestGoldenFingerprint {
		t.Fatalf("fingerprint moved: got %s want %s", got.Fingerprint, dirtyManifestGoldenFingerprint)
	}
	if calls != 2 {
		t.Fatalf("git commands=%d want 2", calls)
	}
	// A second sample over the same bytes must agree with itself too.
	again := dirtyContentSample(t, s)
	if again.Fingerprint != got.Fingerprint || !slices.Equal(again.Contents, got.Contents) {
		t.Fatalf("resample disagreed: %+v vs %+v", again.Contents, got.Contents)
	}
}

func TestSampleDirtyExposesContentPerPath(t *testing.T) {
	root, status := dirtyManifestFixture(t, true)
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) { return status, nil })
	got := dirtyContentSample(t, s)
	sha := func(contents string) string {
		t.Helper()
		_, sum, err := hashDirtyReader(context.Background(), strings.NewReader(contents), int64(len(contents)))
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	byPath := make(map[string]DirtyContent, len(got.Contents))
	for _, c := range got.Contents {
		byPath[c.Path] = c
	}
	if !slices.IsSortedFunc(got.Contents, func(a, b DirtyContent) int { return strings.Compare(a.Path, b.Path) }) {
		t.Fatalf("contents not sorted: %+v", got.Contents)
	}
	want := map[string]DirtyContent{
		"mod.txt":   {Path: "mod.txt", State: DirtyContentPresent, Mode: "100644", SHA256: sha("edited body\n")},
		"gone.txt":  {Path: "gone.txt", State: DirtyContentAbsent},
		"new.txt":   {Path: "new.txt", State: DirtyContentPresent, Mode: "100644", SHA256: sha("untracked body\n")},
		"exec.sh":   {Path: "exec.sh", State: DirtyContentPresent, Mode: "100755", SHA256: sha("#!/bin/sh\n")},
		"same.txt":  {Path: "same.txt", State: DirtyContentPresent, Mode: "100644", SHA256: sha("restored body\n"), HeadEqual: true},
		"ghost.txt": {Path: "ghost.txt", State: DirtyContentAbsent, HeadEqual: true},
	}
	if len(got.Contents) != len(want)+1 {
		t.Fatalf("contents=%d want %d: %+v", len(got.Contents), len(want)+1, got.Contents)
	}
	for path, w := range want {
		if byPath[path] != w {
			t.Errorf("%s: got %+v want %+v", path, byPath[path], w)
		}
	}
	module := byPath["module"]
	if module.State != DirtyContentOpaque || module.SHA256 == "" || module.HeadEqual {
		t.Errorf("opaque directory entry: %+v", module)
	}

	// The contents are exactly the fingerprint's inputs: rebuilding the
	// canonical stream from the entries that are not HEAD-equal yields the
	// same fingerprint.
	var canonical dirtyCanonical
	canonical.str("gortex.gitstate.dirty.content.v2")
	canonical.str(got.HeadTree)
	for _, c := range got.Contents {
		if c.HeadEqual {
			continue
		}
		canonical.str(c.Path)
		switch c.State {
		case DirtyContentAbsent:
			canonical.str(absentMode)
		case DirtyContentOpaque:
			canonical.str("opaque-directory")
			canonical.str(c.SHA256)
		default:
			canonical.str(c.Mode)
			canonical.str(c.SHA256)
		}
	}
	sum := sha256.Sum256(canonical.buf)
	if rebuilt := hex.EncodeToString(sum[:]); rebuilt != got.Fingerprint {
		t.Fatalf("contents disagree with fingerprint inputs: rebuilt %s fingerprint %s", rebuilt, got.Fingerprint)
	}
}

func TestSampleDirtyExposesContentPerPathInRealCheckout(t *testing.T) {
	requirePOSIXCheckout(t)
	repo := dirtyContentRepo(t)
	dirtyContentWrite(t, repo, "keep.txt", "keep\n")
	dirtyContentWrite(t, repo, "drop.txt", "drop\n")
	dirtyContentWrite(t, repo, "tool.sh", "#!/bin/sh\n")
	dirtyContentGit(t, repo, "add", "--", "keep.txt", "drop.txt", "tool.sh")
	dirtyContentGit(t, repo, "commit", "-m", "more")

	dirtyContentWrite(t, repo, "seed.txt", "modified bytes\n")
	if err := os.Remove(filepath.Join(repo, "drop.txt")); err != nil {
		t.Fatal(err)
	}
	dirtyContentWrite(t, repo, "fresh.txt", "untracked\n")
	if err := os.Chmod(filepath.Join(repo, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := dirtyContentSampler(t, repo)
	got := dirtyContentSample(t, s)
	states := make(map[string]string, len(got.Contents))
	for _, c := range got.Contents {
		states[c.Path] = fmt.Sprintf("%s/%s/%t", c.State, c.Mode, c.HeadEqual)
	}
	want := map[string]string{
		"seed.txt":  "present/100644/false",
		"drop.txt":  "absent//false",
		"fresh.txt": "present/100644/false",
		"tool.sh":   "present/100755/false",
	}
	if len(states) != len(want) {
		t.Fatalf("contents=%v want %v", states, want)
	}
	for path, w := range want {
		if states[path] != w {
			t.Errorf("%s: got %s want %s", path, states[path], w)
		}
	}

	// Undo the edit by restoring committed bytes while staged: the entry
	// stays reported but is HEAD-equal and leaves the fingerprint.
	dirtyContentWrite(t, repo, "seed.txt", "original bytes\n")
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	dirtyContentWrite(t, repo, "seed.txt", "modified bytes\n")
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	dirtyContentWrite(t, repo, "seed.txt", "original bytes\n")
	undone := dirtyContentSample(t, s)
	for _, c := range undone.Contents {
		if c.Path == "seed.txt" && !c.HeadEqual {
			t.Fatalf("restored committed bytes not HEAD-equal: %+v", c)
		}
	}
}

func TestSampleDirtyContentsEmptyWhenClean(t *testing.T) {
	repo := dirtyContentRepo(t)
	s := dirtyContentSampler(t, repo)
	got := dirtyContentSample(t, s)
	if len(got.Entries) != 0 || got.Contents != nil {
		t.Fatalf("clean checkout exposed contents: entries=%+v contents=%+v", got.Entries, got.Contents)
	}
	dirtyContentWrite(t, repo, "seed.txt", "edited\n")
	if dirty := dirtyContentSample(t, s); len(dirty.Contents) != 1 {
		t.Fatalf("dirty contents=%+v", dirty.Contents)
	}
	dirtyContentWrite(t, repo, "seed.txt", "original bytes\n")
	if clean := dirtyContentSample(t, s); clean.Contents != nil {
		t.Fatalf("restored checkout exposed contents: %+v", clean.Contents)
	}
}

// A sample refused by a fence exposes no contents at all.
func TestSampleDirtyTornSampleExposesNoContents(t *testing.T) {
	root, status := dirtyManifestFixture(t, false)
	calls := 0
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			return append(append([]byte(nil), status...), []byte("? late.txt\x00")...), nil
		}
		return status, nil
	})
	got, err := s.Sample(context.Background())
	if err == nil {
		t.Fatal("torn sample accepted")
	}
	if got.Contents != nil || got.Fingerprint != "" {
		t.Fatalf("torn sample exposed state: %+v", got)
	}
}
