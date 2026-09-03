package gitstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeIn writes content at a path relative to a checkout root,
// creating the parent directories, and returns the absolute path.
func writeIn(t *testing.T, root, rel, content string) string {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("create parent of %q: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
	return abs
}

// commitAll stages everything in the checkout and commits it.
func commitAll(t *testing.T, repo, message string) {
	t.Helper()
	git(t, repo, "add", "-A", "--", ".")
	git(t, repo, "commit", "-q", "-m", message)
}

// sampleDirtyOK samples dir and fails the test if the sample is
// unavailable.
func sampleDirtyOK(t *testing.T, dir string) DirtySnapshot {
	t.Helper()
	snap, err := SampleDirty(context.Background(), dir)
	if err != nil {
		t.Fatalf("SampleDirty: %v", err)
	}
	return snap
}

// dirtyEntryFor returns the single entry for path.
func dirtyEntryFor(t *testing.T, snap DirtySnapshot, path string) DirtyEntry {
	t.Helper()
	for _, e := range snap.Entries {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no entry for %q; snapshot has %s", path, quotedPaths(snap))
	return DirtyEntry{}
}

// hasDirtyEntry reports whether the snapshot carries an entry for path.
func hasDirtyEntry(snap DirtySnapshot, path string) bool {
	for _, e := range snap.Entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

// quotedPaths renders a snapshot's paths for a failure message.
func quotedPaths(snap DirtySnapshot) string {
	var paths []string
	for _, e := range snap.Entries {
		paths = append(paths, strconv.Quote(e.Path)+"/"+string(e.Kind))
	}
	if len(paths) == 0 {
		return "no entries"
	}
	return strings.Join(paths, ", ")
}

// requirePOSIXCheckout skips a test whose subject is a file mode or a
// symlink, neither of which a Windows checkout carries.
func requirePOSIXCheckout(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes and symlinks are not part of a Windows checkout")
	}
}

func TestSampleDirtyStagedOnly(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "seed.txt", "changed\n")
	git(t, repo, "add", "--", "seed.txt")

	snap := sampleDirtyOK(t, repo)
	if snap.HeadRef != "refs/heads/main" {
		t.Errorf("HeadRef = %q, want refs/heads/main", snap.HeadRef)
	}
	if !isOID(snap.HeadCommit) || !isOID(snap.HeadTree) {
		t.Errorf("HEAD objects = %q / %q, want object ids", snap.HeadCommit, snap.HeadTree)
	}
	if len(snap.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %s", quotedPaths(snap))
	}
	e := snap.Entries[0]
	if e.Kind != DirtyModified || !e.Staged || e.Unstaged {
		t.Errorf("entry = %+v, want a staged-only modification", e)
	}
	if e.Submodule || e.OldPath != "" {
		t.Errorf("unexpected fields on a plain edit: %+v", e)
	}
}

func TestSampleDirtyUnstagedOnly(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "seed.txt", "changed\n")

	e := dirtyEntryFor(t, sampleDirtyOK(t, repo), "seed.txt")
	if e.Kind != DirtyModified || e.Staged || !e.Unstaged {
		t.Errorf("entry = %+v, want an unstaged-only modification", e)
	}
}

func TestSampleDirtyStagedAndUnstagedSameFile(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "seed.txt", "staged\n")
	git(t, repo, "add", "--", "seed.txt")
	writeIn(t, repo, "seed.txt", "and then unstaged\n")

	snap := sampleDirtyOK(t, repo)
	if len(snap.Entries) != 1 {
		t.Fatalf("a file dirty on both sides is one entry, got %s", quotedPaths(snap))
	}
	e := snap.Entries[0]
	if e.Kind != DirtyModified || !e.Staged || !e.Unstaged {
		t.Errorf("entry = %+v, want both sides flagged", e)
	}
}

func TestSampleDirtyDeletions(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "staged-gone.txt", "a\n")
	writeIn(t, repo, "worktree-gone.txt", "b\n")
	commitAll(t, repo, "two more files")

	git(t, repo, "rm", "-q", "--", "staged-gone.txt")
	if err := os.Remove(filepath.Join(repo, "worktree-gone.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	snap := sampleDirtyOK(t, repo)
	staged := dirtyEntryFor(t, snap, "staged-gone.txt")
	if staged.Kind != DirtyDeleted || !staged.Staged || staged.Unstaged {
		t.Errorf("staged deletion = %+v", staged)
	}
	unstaged := dirtyEntryFor(t, snap, "worktree-gone.txt")
	if unstaged.Kind != DirtyDeleted || unstaged.Staged || !unstaged.Unstaged {
		t.Errorf("unstaged deletion = %+v", unstaged)
	}
}

func TestSampleDirtyUntrackedNestedFile(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, filepath.Join("nested", "deeper", "new.txt"), "x\n")

	snap := sampleDirtyOK(t, repo)
	// --untracked-files=all expands the directory, so the entry names
	// the file itself and not the directory holding it.
	e := dirtyEntryFor(t, snap, "nested/deeper/new.txt")
	if e.Kind != DirtyUntracked || e.Staged || !e.Unstaged {
		t.Errorf("entry = %+v, want an unstaged untracked file", e)
	}
	if hasDirtyEntry(snap, "nested/") || hasDirtyEntry(snap, "nested") {
		t.Errorf("the containing directory was reported instead of the file: %s", quotedPaths(snap))
	}
}

func TestSampleDirtyUntrackedAwkwardNames(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "with space.txt", "y\n")

	// A newline in a filename is what makes the NUL-delimited format
	// necessary. Some filesystems reject it; the rest of the test still
	// runs when they do.
	newlined := filepath.Join(repo, "with\nnewline.txt")
	nlErr := os.WriteFile(newlined, []byte("z\n"), 0o644)

	snap := sampleDirtyOK(t, repo)
	spaced := dirtyEntryFor(t, snap, "with space.txt")
	if spaced.Kind != DirtyUntracked {
		t.Errorf("spaced entry = %+v", spaced)
	}

	t.Run("newline in name", func(t *testing.T) {
		if nlErr != nil {
			t.Skipf("filesystem rejects a newline in a filename: %v", nlErr)
		}
		e := dirtyEntryFor(t, snap, "with\nnewline.txt")
		if !strings.Contains(e.Path, "\n") {
			t.Fatalf("path %q lost its newline", e.Path)
		}
		if e.Kind != DirtyUntracked {
			t.Errorf("entry = %+v", e)
		}
	})
}

func TestSampleDirtyRenameDecomposes(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "old name.txt", strings.Repeat("line of content\n", 20))
	commitAll(t, repo, "add the file that gets renamed")
	git(t, repo, "mv", "--", "old name.txt", "new name.txt")

	snap := sampleDirtyOK(t, repo)
	if len(snap.Entries) != 2 {
		t.Fatalf("a rename is two path facts, got %s", quotedPaths(snap))
	}
	gone := dirtyEntryFor(t, snap, "old name.txt")
	if gone.Kind != DirtyDeleted || !gone.Staged || gone.Unstaged {
		t.Errorf("source entry = %+v, want a staged deletion", gone)
	}
	if gone.OldPath != "" {
		t.Errorf("source entry carries OldPath %q; the destination is the one that remembers", gone.OldPath)
	}
	arrived := dirtyEntryFor(t, snap, "new name.txt")
	if arrived.Kind != DirtyRenamedFrom || !arrived.Staged || arrived.Unstaged {
		t.Errorf("destination entry = %+v, want a staged rename", arrived)
	}
	if arrived.OldPath != "old name.txt" {
		t.Errorf("OldPath = %q, want %q", arrived.OldPath, "old name.txt")
	}
	// Sorting is by path, and the destination sorts after the source.
	if snap.Entries[0].Path != "new name.txt" || snap.Entries[1].Path != "old name.txt" {
		t.Errorf("entries are not path-ordered: %s", quotedPaths(snap))
	}
}

func TestSampleDirtyModeFlip(t *testing.T) {
	requirePOSIXCheckout(t)
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "tool.sh", "#!/bin/sh\n")
	commitAll(t, repo, "add a non-executable script")

	if err := os.Chmod(filepath.Join(repo, "tool.sh"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// git reports a mode flip as a plain 'M' with identical blob hashes
	// on both sides, so only the octal columns tell it apart from a
	// content edit.
	unstaged := dirtyEntryFor(t, sampleDirtyOK(t, repo), "tool.sh")
	if unstaged.Kind != DirtyModeChanged || unstaged.Staged || !unstaged.Unstaged {
		t.Errorf("unstaged flip = %+v, want an unstaged mode change", unstaged)
	}

	git(t, repo, "add", "--", "tool.sh")
	staged := dirtyEntryFor(t, sampleDirtyOK(t, repo), "tool.sh")
	if staged.Kind != DirtyModeChanged || !staged.Staged || staged.Unstaged {
		t.Errorf("staged flip = %+v, want a staged mode change", staged)
	}
}

func TestSampleDirtySymlinkSwaps(t *testing.T) {
	requirePOSIXCheckout(t)
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "becomes-link.txt", "plain content\n")
	if err := os.Symlink("somewhere", filepath.Join(repo, "becomes-file")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	commitAll(t, repo, "a file and a symlink")

	if err := os.Remove(filepath.Join(repo, "becomes-link.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(repo, "becomes-link.txt")); err != nil {
		t.Fatalf("symlink over file: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "becomes-file")); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	writeIn(t, repo, "becomes-file", "a real file now\n")

	unstaged := sampleDirtyOK(t, repo)
	if e := dirtyEntryFor(t, unstaged, "becomes-link.txt"); e.Kind != DirtySymlinkChanged || !e.Unstaged {
		t.Errorf("file replaced by a symlink = %+v", e)
	}
	if e := dirtyEntryFor(t, unstaged, "becomes-file"); e.Kind != DirtySymlinkChanged || !e.Unstaged {
		t.Errorf("symlink replaced by a file = %+v", e)
	}

	git(t, repo, "add", "-A", "--", ".")
	staged := sampleDirtyOK(t, repo)
	if e := dirtyEntryFor(t, staged, "becomes-link.txt"); e.Kind != DirtySymlinkChanged || !e.Staged {
		t.Errorf("staged file-to-symlink = %+v", e)
	}
	if e := dirtyEntryFor(t, staged, "becomes-file"); e.Kind != DirtySymlinkChanged || !e.Staged {
		t.Errorf("staged symlink-to-file = %+v", e)
	}
}

func TestSampleDirtySubmoduleEntry(t *testing.T) {
	root := tempRoot(t)
	inner := initRepo(t, filepath.Join(root, "inner"))
	outer := initRepo(t, filepath.Join(root, "outer"))

	// Adding a submodule from a local path needs the file protocol
	// explicitly allowed; a git built without it cannot run this test.
	if out, err := tryGit(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--", inner, "mod"); err != nil {
		t.Skipf("git refused to add a local submodule: %v\n%s", err, out)
	}

	added := dirtyEntryFor(t, sampleDirtyOK(t, outer), "mod")
	if !added.Submodule {
		t.Errorf("freshly added submodule = %+v, want Submodule", added)
	}
	if added.Kind != DirtyAdded || !added.Staged {
		t.Errorf("freshly added submodule = %+v, want a staged add", added)
	}
	if gitmodules := dirtyEntryFor(t, sampleDirtyOK(t, outer), ".gitmodules"); gitmodules.Submodule {
		t.Errorf(".gitmodules is a plain file, not a submodule: %+v", gitmodules)
	}

	git(t, outer, "commit", "-q", "-m", "add submodule")
	writeIn(t, filepath.Join(outer, "mod"), "seed.txt", "changed inside the submodule\n")

	dirty := dirtyEntryFor(t, sampleDirtyOK(t, outer), "mod")
	if !dirty.Submodule || dirty.Kind != DirtyModified || !dirty.Unstaged {
		t.Errorf("dirty submodule = %+v, want an unstaged modification flagged as a submodule", dirty)
	}
}

func TestSampleDirtyUnmergedEntry(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "shared.txt", "base\n")
	commitAll(t, repo, "base")

	git(t, repo, "checkout", "-q", "-b", "other")
	writeIn(t, repo, "shared.txt", "their side\n")
	commitAll(t, repo, "their change")
	git(t, repo, "checkout", "-q", "main")
	writeIn(t, repo, "shared.txt", "our side\n")
	commitAll(t, repo, "our change")

	if _, err := tryGit(t, repo, "merge", "other"); err == nil {
		t.Fatal("expected the merge to conflict")
	}

	e := dirtyEntryFor(t, sampleDirtyOK(t, repo), "shared.txt")
	if e.Kind != DirtyModified {
		t.Errorf("Kind = %q, want %q for a conflicted path", e.Kind, DirtyModified)
	}
	if !e.Staged || !e.Unstaged {
		t.Errorf("entry = %+v, want a conflict flagged on both sides", e)
	}
}

func TestSampleDirtyUnbornHEAD(t *testing.T) {
	root := tempRoot(t)
	repo := initBareRepo(t, filepath.Join(root, "fresh"), false)
	writeIn(t, repo, "staged.txt", "a\n")
	writeIn(t, repo, "loose.txt", "b\n")
	git(t, repo, "add", "--", "staged.txt")

	snap := sampleDirtyOK(t, repo)
	if snap.HeadRef != "refs/heads/main" {
		t.Errorf("HeadRef = %q, want refs/heads/main", snap.HeadRef)
	}
	if snap.HeadCommit != "" || snap.HeadTree != "" {
		t.Errorf("unborn HEAD resolved to objects: %q / %q", snap.HeadCommit, snap.HeadTree)
	}
	if snap.Fingerprint == "" {
		t.Error("an unborn checkout is still fingerprintable")
	}
	// Nothing is committed yet, so every tracked path is an addition.
	if e := dirtyEntryFor(t, snap, "staged.txt"); e.Kind != DirtyAdded || !e.Staged {
		t.Errorf("entry = %+v, want a staged add", e)
	}
	if e := dirtyEntryFor(t, snap, "loose.txt"); e.Kind != DirtyUntracked {
		t.Errorf("entry = %+v, want an untracked file", e)
	}
}

func TestSampleDirtyOmitsIgnoredFiles(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, ".gitignore", "build/\nnoisy.log\n")
	commitAll(t, repo, "ignore rules")

	writeIn(t, repo, "noisy.log", "spam\n")
	writeIn(t, repo, filepath.Join("build", "artifact.bin"), "bytes\n")
	writeIn(t, repo, "watched.txt", "real change\n")

	snap := sampleDirtyOK(t, repo)
	if hasDirtyEntry(snap, "noisy.log") || hasDirtyEntry(snap, "build/artifact.bin") {
		t.Errorf("ignored paths leaked into the snapshot: %s", quotedPaths(snap))
	}
	if !hasDirtyEntry(snap, "watched.txt") {
		t.Errorf("the un-ignored file is missing: %s", quotedPaths(snap))
	}
}

func TestSampleDirtyIsDeterministic(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "renamed.txt", strings.Repeat("content\n", 20))
	writeIn(t, repo, "dropped.txt", "gone soon\n")
	commitAll(t, repo, "more files")

	writeIn(t, repo, "seed.txt", "edited\n")
	git(t, repo, "add", "--", "seed.txt")
	git(t, repo, "mv", "--", "renamed.txt", "moved.txt")
	git(t, repo, "rm", "-q", "--", "dropped.txt")
	writeIn(t, repo, "untracked.txt", "new\n")

	first := sampleDirtyOK(t, repo)
	second := sampleDirtyOK(t, repo)
	if !reflect.DeepEqual(first.Entries, second.Entries) {
		t.Errorf("entries differ between samples:\n%+v\n%+v", first.Entries, second.Entries)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Errorf("fingerprints differ with nothing changed: %q vs %q", first.Fingerprint, second.Fingerprint)
	}
	if first.Fingerprint == "" {
		t.Error("empty fingerprint")
	}

	sorted := slicesAreSorted(first.Entries)
	if !sorted {
		t.Errorf("entries are not in (path, kind) order: %s", quotedPaths(first))
	}
}

// slicesAreSorted reports whether entries are in the order SampleDirty
// promises.
func slicesAreSorted(entries []DirtyEntry) bool {
	for i := 1; i < len(entries); i++ {
		if compareDirtyEntries(entries[i-1], entries[i]) > 0 {
			return false
		}
	}
	return true
}

func TestSampleDirtyFingerprintSensitivity(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	writeIn(t, repo, "tool.sh", "#!/bin/sh\n")
	commitAll(t, repo, "add a script")

	seen := map[string]string{}
	record := func(label string) {
		t.Helper()
		fp := sampleDirtyOK(t, repo).Fingerprint
		if prev, ok := seen[fp]; ok {
			t.Fatalf("%q fingerprints the same as %q (%s)", label, prev, fp)
		}
		seen[fp] = label
	}

	record("clean")

	note := writeIn(t, repo, "note.txt", "one\n")
	record("untracked file appeared")

	writeIn(t, repo, "note.txt", "one and two\n")
	record("content edit")

	// Only the modification time moves here: same bytes, same length,
	// same status. The stat evidence is what makes it visible.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(note, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	record("mtime bump alone")

	writeIn(t, repo, "seed.txt", "edited\n")
	record("tracked file edited")

	// The file on disk does not move; only which side of the index the
	// change sits on does.
	git(t, repo, "add", "--", "seed.txt")
	record("same edit, now staged")

	t.Run("mode flip", func(t *testing.T) {
		requirePOSIXCheckout(t)
		if err := os.Chmod(filepath.Join(repo, "tool.sh"), 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		record("mode flip")
	})
}

func TestSampleDirtyUnavailable(t *testing.T) {
	isolateGit(t)
	dir := realPath(t, t.TempDir())
	if out, err := tryGit(t, dir, "rev-parse", "--git-dir"); err == nil {
		t.Skipf("temp dir is inside a git repository (%s)", out)
	}

	snap, err := SampleDirty(context.Background(), dir)
	if !errors.Is(err, ErrDirtyUnavailable) {
		t.Fatalf("err = %v, want ErrDirtyUnavailable", err)
	}
	if len(snap.Entries) != 0 || snap.Fingerprint != "" {
		t.Errorf("expected a zero snapshot alongside the error, got %+v", snap)
	}
	if _, err := SampleDirty(context.Background(), ""); !errors.Is(err, ErrDirtyUnavailable) {
		t.Fatalf("err on empty dir = %v, want ErrDirtyUnavailable", err)
	}
	missing := filepath.Join(dir, "does", "not", "exist")
	if _, err := SampleDirty(context.Background(), missing); !errors.Is(err, ErrDirtyUnavailable) {
		t.Fatalf("err on missing dir = %v, want ErrDirtyUnavailable", err)
	}
}

// statusStream joins porcelain v2 records into the NUL-delimited stream
// git emits: every record is NUL-terminated, and a rename's source path
// is a record of its own.
func statusStream(records ...string) []byte {
	var b strings.Builder
	for _, rec := range records {
		b.WriteString(rec)
		b.WriteByte(0)
	}
	return []byte(b.String())
}

func TestParseStatusZRenameAndCopy(t *testing.T) {
	out := statusStream(
		"2 R. N... 100644 100644 100644 1111111111111111111111111111111111111111 1111111111111111111111111111111111111111 R100 dst.txt",
		"src.txt",
		// A copy leaves its source in place, so it decomposes into the
		// destination alone.
		"2 C. N... 100644 100644 100644 2222222222222222222222222222222222222222 2222222222222222222222222222222222222222 C085 copied.txt",
		"origin.txt",
		"1 .M N... 100644 100644 100644 3333333333333333333333333333333333333333 3333333333333333333333333333333333333333 after.txt",
	)

	got := parseStatusZ(out)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d: %+v", len(got), got)
	}
	if got[0] != (DirtyEntry{Path: "src.txt", Kind: DirtyDeleted, Staged: true}) {
		t.Errorf("rename source = %+v", got[0])
	}
	if got[1] != (DirtyEntry{Path: "dst.txt", Kind: DirtyRenamedFrom, Staged: true, OldPath: "src.txt"}) {
		t.Errorf("rename destination = %+v", got[1])
	}
	if got[2] != (DirtyEntry{Path: "copied.txt", Kind: DirtyRenamedFrom, Staged: true, OldPath: "origin.txt"}) {
		t.Errorf("copy destination = %+v", got[2])
	}
	// The record after the rename pair must still be read as a record,
	// not swallowed as somebody's source path.
	if got[3].Path != "after.txt" || got[3].Kind != DirtyModified {
		t.Errorf("record following a rename = %+v", got[3])
	}
}

func TestParseStatusZRenameCarriesWorktreeEditOnDestination(t *testing.T) {
	out := statusStream(
		"2 RM N... 100644 100644 100644 4444444444444444444444444444444444444444 4444444444444444444444444444444444444444 R100 dst.txt",
		"src.txt",
	)

	got := parseStatusZ(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %+v", got)
	}
	if got[0].Unstaged {
		t.Errorf("the vanished source picked up the destination's worktree edit: %+v", got[0])
	}
	if !got[1].Staged || !got[1].Unstaged {
		t.Errorf("destination = %+v, want both sides flagged", got[1])
	}
}

func TestParseStatusZKeepsAwkwardPaths(t *testing.T) {
	out := statusStream(
		"1 .M N... 100644 100644 100644 5555555555555555555555555555555555555555 5555555555555555555555555555555555555555 dir with space/file\nwith newline.txt",
		"? untracked with space.txt",
	)

	got := parseStatusZ(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %+v", got)
	}
	if got[0].Path != "dir with space/file\nwith newline.txt" {
		t.Errorf("changed path = %q, spaces or newline lost", got[0].Path)
	}
	if got[1].Path != "untracked with space.txt" || got[1].Kind != DirtyUntracked {
		t.Errorf("untracked entry = %+v", got[1])
	}
}

func TestParseStatusZSkipsWhatItCannotRead(t *testing.T) {
	out := statusStream(
		"# branch.oid 6666666666666666666666666666666666666666",
		"! ignored.txt",
		"1 .M N... 100644 100644",
		"2 R. N... 100644 100644 100644 7777777777777777777777777777777777777777 7777777777777777777777777777777777777777 R100 truncated.txt",
	)

	if got := parseStatusZ(out); len(got) != 0 {
		t.Errorf("expected no entries, got %+v", got)
	}
	if got := parseStatusZ(nil); len(got) != 0 {
		t.Errorf("expected no entries from empty output, got %+v", got)
	}
	if got := parseStatusZ([]byte{0, 0}); len(got) != 0 {
		t.Errorf("expected no entries from bare separators, got %+v", got)
	}
}

func TestKindFromModes(t *testing.T) {
	cases := []struct {
		name              string
		head, index, tree string
		want              DirtyKind
	}{
		{"unchanged", "100644", "100644", "100644", ""},
		{"created", "000000", "100644", "100644", ""},
		{"deleted from index", "100644", "000000", "000000", ""},
		{"staged mode flip", "100644", "100755", "100755", DirtyModeChanged},
		{"unstaged mode flip", "100644", "100644", "100755", DirtyModeChanged},
		{"file becomes a submodule", "100644", "160000", "160000", DirtyModeChanged},
		{"staged file to symlink", "100644", "120000", "120000", DirtySymlinkChanged},
		{"staged symlink to file", "120000", "100644", "100644", DirtySymlinkChanged},
		{"unstaged symlink to file", "120000", "120000", "100644", DirtySymlinkChanged},
		// A symlink swap on one side outranks a mode flip on the other:
		// the type change is the larger fact about the path.
		{"symlink swap plus mode flip", "100755", "100644", "120000", DirtySymlinkChanged},
	}
	for _, c := range cases {
		if got := kindFromModes(c.head, c.index, c.tree); got != c.want {
			t.Errorf("%s: kindFromModes(%s, %s, %s) = %q, want %q", c.name, c.head, c.index, c.tree, got, c.want)
		}
	}
}

func TestKindFromStatus(t *testing.T) {
	cases := []struct {
		x, y byte
		want DirtyKind
	}{
		{'A', '.', DirtyAdded},
		{'.', 'A', DirtyAdded},
		{'D', '.', DirtyDeleted},
		{'.', 'D', DirtyDeleted},
		{'M', 'M', DirtyModified},
		{'.', 'M', DirtyModified},
		// The staged column decides when both are set: it says what the
		// index records.
		{'A', 'M', DirtyAdded},
		{'D', 'M', DirtyDeleted},
	}
	for _, c := range cases {
		if got := kindFromStatus(c.x, c.y); got != c.want {
			t.Errorf("kindFromStatus(%q, %q) = %q, want %q", c.x, c.y, got, c.want)
		}
	}
}

func TestFingerprintDirtyIsInjectiveAcrossFieldBoundaries(t *testing.T) {
	root := t.TempDir()
	// Two snapshots whose fields concatenate to the same characters. A
	// separator-joined encoding would collapse them; a length-prefixed
	// one cannot.
	first := []DirtyEntry{{Path: "ab", Kind: DirtyUntracked, OldPath: "c"}}
	second := []DirtyEntry{{Path: "a", Kind: DirtyUntracked, OldPath: "bc"}}
	if fingerprintDirty(root, "", first) == fingerprintDirty(root, "", second) {
		t.Error("field boundaries are not recoverable from the encoding")
	}

	// The same entries under a different HEAD are a different snapshot.
	if fingerprintDirty(root, strings.Repeat("a", 40), first) == fingerprintDirty(root, strings.Repeat("b", 40), first) {
		t.Error("HeadCommit does not reach the fingerprint")
	}
	// So is the same path staged rather than unstaged.
	staged := []DirtyEntry{{Path: "ab", Kind: DirtyUntracked, OldPath: "c", Staged: true}}
	if fingerprintDirty(root, "", first) == fingerprintDirty(root, "", staged) {
		t.Error("the staged flag does not reach the fingerprint")
	}
}

func TestStatEvidenceIgnoresDeletionsAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "present.txt"), []byte("bytes\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	size, mtime := statEvidence(root, DirtyEntry{Path: "present.txt", Kind: DirtyModified})
	if size != 6 || mtime == 0 {
		t.Errorf("present file = %d / %d, want its real size and mtime", size, mtime)
	}
	// A deletion is not statted even when something still stands at the
	// path, so the evidence cannot flap while the deletion is pending.
	if size, mtime := statEvidence(root, DirtyEntry{Path: "present.txt", Kind: DirtyDeleted}); size != 0 || mtime != 0 {
		t.Errorf("deletion = %d / %d, want no evidence", size, mtime)
	}
	if size, mtime := statEvidence(root, DirtyEntry{Path: "gone.txt", Kind: DirtyModified}); size != 0 || mtime != 0 {
		t.Errorf("missing path = %d / %d, want no evidence", size, mtime)
	}
}
