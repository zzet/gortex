package gitstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Only test-owned Git repositories are invoked; global attributes and hooks
// must not enter these content-identity fixtures.
func dirtyContentGit(tb testing.TB, repo string, args ...string) string {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := []string{"-c", "user.name=Dirty Content Test", "-c", "user.email=dirty-content@example.invalid", "-c", "core.hooksPath=" + os.DevNull, "-C", repo}
	cmd := exec.CommandContext(ctx, "git", append(argv, args...)...)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func dirtyContentWrite(tb testing.TB, repo, path, contents string) string {
	tb.Helper()
	full := filepath.Join(repo, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		tb.Fatal(err)
	}
	return full
}
func dirtyContentRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	dirtyContentGit(t, repo, "init", "-b", "main")
	dirtyContentWrite(t, repo, "seed.txt", "original bytes\n")
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	dirtyContentGit(t, repo, "commit", "-m", "initial")
	return repo
}
func dirtyContentSampler(t *testing.T, repo string) *DirtySampler {
	t.Helper()
	s, err := NewDirtySampler(repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func dirtyContentSample(t *testing.T, s *DirtySampler) DirtySnapshot {
	t.Helper()
	snap, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
func TestDirtyContentIgnoresMetadataAndStageOnlyChanges(t *testing.T) {
	repo := dirtyContentRepo(t)
	path := dirtyContentWrite(t, repo, "seed.txt", "dirty bytes\n")
	s := dirtyContentSampler(t, repo)
	before := dirtyContentSample(t, s)
	check := func(label string) {
		t.Helper()
		if got := dirtyContentSample(t, s); got.Fingerprint != before.Fingerprint {
			t.Fatalf("%s changed content identity", label)
		}
	}
	stamp := time.Unix(1_600_000_000, 123_456_789)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	check("timestamp only")
	dirtyContentWrite(t, repo, "seed.txt", "dirty bytes\n")
	check("same-byte save")
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	check("stage")
	dirtyContentGit(t, repo, "reset", "HEAD", "--", "seed.txt")
	check("unstage")
	dirtyContentGit(t, repo, "commit", "--amend", "-m", "different commit message")
	after := dirtyContentSample(t, s)
	if after.HeadCommit == before.HeadCommit || after.HeadTree != before.HeadTree {
		t.Fatalf("not a same-tree amendment: before=%+v after=%+v", before, after)
	}
	if after.Fingerprint != before.Fingerprint {
		t.Fatal("same-tree amendment changed identity")
	}
}
func TestDirtyContentDetectsRestoredSizeAndMtime(t *testing.T) {
	repo := dirtyContentRepo(t)
	path := dirtyContentWrite(t, repo, "seed.txt", "first bytes\n")
	s := dirtyContentSampler(t, repo)
	before := dirtyContentSample(t, s)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirtyContentWrite(t, repo, "seed.txt", "other bytes\n")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil || afterInfo.Size() != info.Size() || !afterInfo.ModTime().Equal(info.ModTime()) {
		t.Fatalf("metadata not restored: %v", err)
	}
	if after := dirtyContentSample(t, s); after.Fingerprint == before.Fingerprint {
		t.Fatal("different bytes with restored size/mtime reused identity")
	}
}
func TestDirtyContentStagedResidueAndDeleteRestore(t *testing.T) {
	repo := dirtyContentRepo(t)
	s := dirtyContentSampler(t, repo)
	clean := dirtyContentSample(t, s)
	dirtyContentWrite(t, repo, "seed.txt", "staged bytes\n")
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	dirtyContentWrite(t, repo, "seed.txt", "original bytes\n")
	if got := dirtyContentSample(t, s); len(got.Entries) == 0 || got.Fingerprint != clean.Fingerprint {
		t.Fatalf("staged residue changed raw HEAD identity: %+v", got)
	}
	dirtyContentGit(t, repo, "reset", "HEAD", "--", "seed.txt")
	dirtyContentGit(t, repo, "rm", "--", "seed.txt")
	if got := dirtyContentSample(t, s); got.Fingerprint == clean.Fingerprint {
		t.Fatal("deletion did not change identity")
	}
	dirtyContentWrite(t, repo, "seed.txt", "original bytes\n")
	if got := dirtyContentSample(t, s); len(got.Entries) == 0 || got.Fingerprint != clean.Fingerprint {
		t.Fatalf("restored file behind staged deletion changed raw HEAD identity: %+v", got)
	}
}
func TestDirtyContentRenameAndUntrackedStaging(t *testing.T) {
	repo := dirtyContentRepo(t)
	s := dirtyContentSampler(t, repo)
	dirtyContentWrite(t, repo, "new.txt", "new file bytes\n")
	untracked := dirtyContentSample(t, s)
	dirtyContentGit(t, repo, "add", "--", "new.txt")
	if got := dirtyContentSample(t, s); got.Fingerprint != untracked.Fingerprint {
		t.Fatal("staging untracked bytes changed identity")
	}
	dirtyContentGit(t, repo, "commit", "-m", "add new")
	if err := os.Rename(filepath.Join(repo, "new.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	unstaged := dirtyContentSample(t, s)
	dirtyContentGit(t, repo, "add", "-A")
	if got := dirtyContentSample(t, s); got.Fingerprint != unstaged.Fingerprint {
		t.Fatalf("staging rename changed identity: before=%+v after=%+v", unstaged, got)
	}
}
func TestDirtyContentModeAndSymlinkTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable bits and symlink fixture")
	}
	repo := dirtyContentRepo(t)
	s := dirtyContentSampler(t, repo)
	clean := dirtyContentSample(t, s)
	if err := os.Chmod(filepath.Join(repo, "seed.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	mode := dirtyContentSample(t, s)
	if mode.Fingerprint == clean.Fingerprint {
		t.Fatal("executable mode not represented")
	}
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	if got := dirtyContentSample(t, s); got.Fingerprint != mode.Fingerprint {
		t.Fatal("staging executable bit changed identity")
	}
	link := filepath.Join(repo, "link")
	if err := os.Symlink("first-target", link); err != nil {
		t.Fatal(err)
	}
	first := dirtyContentSample(t, s)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other-target", link); err != nil {
		t.Fatal(err)
	}
	if got := dirtyContentSample(t, s); got.Fingerprint == first.Fingerprint {
		t.Fatal("different symlink text reused identity")
	}
}
func TestDirtyContentRawBytesDoNotUseCleanFilterEquivalence(t *testing.T) {
	repo := dirtyContentRepo(t)
	dirtyContentWrite(t, repo, ".gitattributes", "seed.txt text eol=lf\n")
	dirtyContentGit(t, repo, "add", "--", ".gitattributes")
	dirtyContentGit(t, repo, "commit", "-m", "attributes")
	s := dirtyContentSampler(t, repo)
	dirtyContentWrite(t, repo, "seed.txt", "different dirty bytes\n")
	before := dirtyContentSample(t, s)
	dirtyContentWrite(t, repo, "seed.txt", "different dirty bytes\r\n")
	after := dirtyContentSample(t, s)
	if len(before.Entries) == 0 || len(after.Entries) == 0 || before.Fingerprint == after.Fingerprint {
		t.Fatal("distinct reported raw bytes collapsed by text normalization")
	}
}
func TestDirtyContentDocumentsGitHiddenCleanFilterBoundary(t *testing.T) {
	repo := dirtyContentRepo(t)
	dirtyContentWrite(t, repo, ".gitattributes", "seed.txt text eol=lf\n")
	dirtyContentGit(t, repo, "add", "--", ".gitattributes")
	dirtyContentGit(t, repo, "commit", "-m", "attributes")
	s := dirtyContentSampler(t, repo)
	before := dirtyContentSample(t, s)
	path := dirtyContentWrite(t, repo, "seed.txt", "original bytes\r\n")
	bytesOnDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(bytesOnDisk, []byte("original bytes\r\n")) {
		t.Fatalf("fixture did not change raw bytes: %q, %v", bytesOnDisk, err)
	}
	reported := dirtyContentSample(t, s)
	if len(reported.Entries) != 1 || reported.Fingerprint == before.Fingerprint {
		t.Fatalf("fixture did not first report the raw conversion: %+v", reported)
	}
	// Refresh the index with the same normalized HEAD blob; raw CRLF remains
	// on disk. A write alone can still be reported modified by Git.
	dirtyContentGit(t, repo, "add", "--", "seed.txt")
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, bytesOnDisk) {
		t.Fatalf("Git altered raw fixture bytes: %q, %v", got, err)
	}
	dirtyContentGit(t, repo, "diff", "--cached", "--exit-code", "--", "seed.txt")
	after := dirtyContentSample(t, s)
	// Git's configured text normalization hides this raw change from status.
	// The sampler deliberately does not expand admission into a full-tree scan;
	// this inherited boundary must remain explicit in exact-view E2E claims.
	if len(before.Entries) != 0 || len(after.Entries) != 0 || after.Fingerprint != before.Fingerprint || after.Fingerprint == reported.Fingerprint {
		t.Fatalf("Git-hidden normalization boundary changed: before=%+v after=%+v", before, after)
	}
}

func dirtyContentStatus(records ...string) []byte {
	all := []string{"# branch.oid " + strings.Repeat("a", 40), "# branch.head main"}
	all = append(all, records...)
	return []byte(strings.Join(all, "\x00") + "\x00")
}
func dirtyContentFake(t *testing.T, root string, run dirtyCommandFunc) *DirtySampler {
	t.Helper()
	return newDirtySampler(root, strings.Repeat("a", 40), strings.Repeat("b", 40), run)
}
func TestDirtyContentCopyAndRestoredRenameNormalizeToEffectiveFiles(t *testing.T) {
	root := t.TempDir()
	contents := "original bytes\n"
	dirtyContentWrite(t, root, "original.txt", contents)
	dirtyContentWrite(t, root, "copied.txt", contents)
	sha1, _, err := hashDirtyReader(context.Background(), strings.NewReader(contents), int64(len(contents)))
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, tc := range []struct {
		name    string
		records []string
	}{
		{"untracked_copy", []string{"? copied.txt"}},
		{"staged_copy", []string{fmt.Sprintf("2 C. N... 100644 100644 100644 %s %s C100 copied.txt", sha1, sha1), "original.txt"}},
		{"staged_rename_restored_source", []string{fmt.Sprintf("2 R. N... 100644 100644 100644 %s %s R100 copied.txt", sha1, sha1), "original.txt", "? original.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := dirtyContentStatus(tc.records...)
			s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) { return status, nil })
			got := dirtyContentSample(t, s)
			if want == "" {
				want = got.Fingerprint
			} else if got.Fingerprint != want {
				t.Fatal("copy/rename index representation changed identical effective files")
			}
			if tc.name == "staged_copy" && len(got.Entries) != 1 {
				t.Fatalf("copy invented source deletion: %+v", got.Entries)
			}
		})
	}
}

func TestDirtyContentConflictDoesNotInferHeadFromMergeStages(t *testing.T) {
	root := t.TempDir()
	contents := "our stage bytes\n"
	dirtyContentWrite(t, root, "conflicted.txt", contents)
	sha1, _, err := hashDirtyReader(context.Background(), strings.NewReader(contents), int64(len(contents)))
	if err != nil {
		t.Fatal(err)
	}
	status := dirtyContentStatus()
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) { return status, nil })
	clean := dirtyContentSample(t, s)
	status = dirtyContentStatus(fmt.Sprintf("u UU N... 100644 100644 100644 100644 %s %s %s conflicted.txt", sha1, sha1, sha1))
	before := dirtyContentSample(t, s)
	if len(before.Entries) != 1 || before.Fingerprint == clean.Fingerprint {
		t.Fatal("conflict stages incorrectly certified raw HEAD equivalence")
	}
	dirtyContentWrite(t, root, "conflicted.txt", "new merge bytes\n")
	if after := dirtyContentSample(t, s); after.Fingerprint == before.Fingerprint {
		t.Fatal("conflicted-file bytes did not affect content identity")
	}
}

func TestDirtyContentStatusFenceDoesNotPublishFailedCache(t *testing.T) {
	for _, scenario := range []string{"bytes", "status", "cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := dirtyContentWrite(t, root, "note.txt", "first bytes\n")
			status := dirtyContentStatus("? note.txt")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s := dirtyContentFake(t, root, func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				calls++
				if calls == 2 {
					switch scenario {
					case "bytes":
						info, err := os.Stat(path)
						if err != nil {
							return nil, err
						}
						if err := os.WriteFile(path, []byte("other bytes\n"), 0o644); err != nil {
							return nil, err
						}
						if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
							return nil, err
						}
					case "status":
						return dirtyContentStatus("? note.txt", "? appeared.txt"), nil
					case "cancellation":
						cancel()
					}
				}
				return status, nil
			})
			previous := map[string]dirtyContentMemo{"previous.txt": {sha256: "previous-sample"}}
			s.contentCache = previous
			got, err := s.Sample(ctx)
			if !errors.Is(err, ErrDirtyUnavailable) || !reflect.DeepEqual(got, DirtySnapshot{}) {
				t.Fatalf("incoherent sample not unavailable+empty: got=%+v err=%v", got, err)
			}
			if scenario == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if !reflect.DeepEqual(s.contentCache, previous) {
				t.Fatal("failed sample published partial cache")
			}
		})
	}
}
func TestDirtyContentCanceledCallerDoesNotWaitBehindSampler(t *testing.T) {
	root := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, enterOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) {
		enterOnce.Do(func() { close(entered) })
		<-release
		return dirtyContentStatus(), nil
	})
	first := make(chan error, 1)
	go func() { _, err := s.Sample(context.Background()); first <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan error, 1)
	go func() {
		snap, err := s.Sample(ctx)
		if !reflect.DeepEqual(snap, DirtySnapshot{}) {
			err = fmt.Errorf("canceled waiter returned %+v", snap)
		}
		second <- err
	}()
	secondFinished := false
	select {
	case err := <-second:
		secondFinished = true
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDirtyUnavailable) {
			t.Errorf("canceled waiter cause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("canceled caller waited for another sample")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !secondFinished {
		<-second
	}
}
func TestDirtyContentOpaqueDirectoryKeepsNonrecursiveContract(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "module"), 0o755); err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("c", 40)
	status := dirtyContentStatus("1 .M S.M. 160000 160000 160000 " + oid + " " + oid + " module")
	calls := 0
	s := dirtyContentFake(t, root, func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		calls++
		if dir != root || len(args) == 0 || args[0] != "--no-optional-locks" {
			return nil, fmt.Errorf("unexpected recursive Git probe: %q %v", dir, args)
		}
		return status, nil
	})
	got := dirtyContentSample(t, s)
	if len(got.Entries) != 1 || !got.Entries[0].Submodule || calls != 2 {
		t.Fatalf("opaque contract changed: entries=%+v calls=%d", got.Entries, calls)
	}
	if len(s.contentCache) != 0 {
		t.Fatal("opaque directory cached as file body")
	}
}
func TestDirtyContentCacheBoundAndCleanFastPath(t *testing.T) {
	root := t.TempDir()
	records := make([]string, dirtyContentCacheLimit+1)
	for i := range records {
		name := fmt.Sprintf("file-%04d.txt", i)
		dirtyContentWrite(t, root, name, "x")
		records[i] = "? " + name
	}
	status := dirtyContentStatus(records...)
	calls := 0
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) { calls++; return status, nil })
	dirtyContentSample(t, s)
	if got := len(s.contentCache); got != dirtyContentCacheLimit {
		t.Fatalf("cache cardinality=%d want%d", got, dirtyContentCacheLimit)
	}
	if calls != 2 {
		t.Fatalf("dirty commands=%d want2", calls)
	}
	status = dirtyContentStatus()
	calls = 0
	dirtyContentSample(t, s)
	if calls != 1 || len(s.contentCache) != 0 {
		t.Fatalf("clean fast path commands=%d cache=%d", calls, len(s.contentCache))
	}
}

type dirtyContentReaderFunc func([]byte) (int, error)

func (f dirtyContentReaderFunc) Read(p []byte) (int, error) { return f(p) }
func TestDirtyContentHashReaderBoundsAndCancellation(t *testing.T) {
	sentinel := errors.New("reader failure")
	for _, tc := range []struct {
		name   string
		reader io.Reader
		size   int64
		want   error
	}{
		{"negative", strings.NewReader("x"), -1, nil},
		{"growth", strings.NewReader("xx"), 1, nil},
		{"shrink", strings.NewReader("x"), 2, nil},
		{"no_progress", dirtyContentReaderFunc(func([]byte) (int, error) { return 0, nil }), 1, io.ErrNoProgress},
		{"reader_error", dirtyContentReaderFunc(func([]byte) (int, error) { return 0, sentinel }), 1, sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := hashDirtyReader(context.Background(), tc.reader, tc.size)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("reader outcome: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	reader := dirtyContentReaderFunc(func(p []byte) (int, error) { calls++; p[0] = 'x'; cancel(); return 1, nil })
	if _, _, err := hashDirtyReader(ctx, reader, 2); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("chunk cancel: calls=%d err=%v", calls, err)
	}
	one, two, err := hashDirtyReader(context.Background(), strings.NewReader("hello\n"), 6)
	if err != nil || one != "ce013625030ba8dba906f756967f9e9ca394464a" || len(two) != 64 {
		t.Fatalf("Git blob hashes=%s/%s err=%v", one, two, err)
	}
}
func TestDirtyContentDigestCacheUsesTrustedChangeEvidence(t *testing.T) {
	dir := t.TempDir()
	path := dirtyContentWrite(t, dir, "file.txt", "first bytes\n")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Lstat("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	first, reused, err := dirtyContentForPath(context.Background(), root, "file.txt", info, dirtyContentMemo{}, false)
	if err != nil || reused {
		t.Fatalf("cold digest reused=%t err=%v", reused, err)
	}
	_, reused, err = dirtyContentForPath(context.Background(), root, "file.txt", info, first, true)
	if err != nil || reused != first.reusable {
		t.Fatalf("unsupported evidence reused=%t eligible=%t err=%v", reused, first.reusable, err)
	}
	if err := os.WriteFile(path, []byte("other bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := root.Lstat("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	second, reused, err := dirtyContentForPath(context.Background(), root, "file.txt", current, first, true)
	if err != nil || reused || second.sha256 == first.sha256 {
		t.Fatalf("stale digest reused=%t err=%v", reused, err)
	}
}

func BenchmarkDirtyContentDigestCache(b *testing.B) {
	dir := b.TempDir()
	sizes := []int{1024, 64 * 1024, 1024 * 1024}
	for _, size := range sizes {
		path := filepath.Join(dir, fmt.Sprintf("file-%d.txt", size))
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	// Fixture setup only: production never sleeps to age inode timestamps.
	time.Sleep(dirtyContentQuietWindow + 50*time.Millisecond)
	root, err := os.OpenRoot(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer root.Close()
	for _, size := range sizes {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			path := fmt.Sprintf("file-%d.txt", size)
			info, err := root.Lstat(path)
			if err != nil {
				b.Fatal(err)
			}
			memo, _, err := dirtyContentForPath(context.Background(), root, path, info, dirtyContentMemo{}, false)
			if err != nil {
				b.Fatal(err)
			}
			for _, cached := range []bool{false, true} {
				b.Run(fmt.Sprintf("cached_%t", cached), func(b *testing.B) {
					b.ReportAllocs()
					hashes := 0
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						current, err := root.Lstat(path)
						if err != nil {
							b.Fatal(err)
						}
						_, reused, err := dirtyContentForPath(context.Background(), root, path, current, memo, cached)
						if err != nil {
							b.Fatal(err)
						}
						if !reused {
							hashes++
						}
					}
					b.ReportMetric(float64(hashes)/float64(b.N), "hashes/op")
				})
			}
		})
	}
}
func TestDirtyContentRejectsRegularFileReplacedByFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX FIFO fixture")
	}
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	dir := t.TempDir()
	path := dirtyContentWrite(t, dir, "file.txt", "bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Lstat("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, mkfifo, path).CombinedOutput(); err != nil {
		t.Fatalf("create FIFO: %v: %s", err, out)
	}
	done := make(chan error, 1)
	go func() { _, err := readDirtyContent(context.Background(), root, "file.txt", info); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted special file as regular content")
		}
	case <-time.After(5 * time.Second):
		// Negative-control blocking-open code must not leak a goroutine.
		unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("could not unblock FIFO: %v", err)
		}
		_ = unblock.Close()
		<-done
		t.Fatal("regular-to-FIFO race blocked before type validation")
	}
}
func TestDirtyContentRejectsRootReplacementDuringFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX rename of an open root; Windows file identity is tested separately")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dirtyContentWrite(t, root, "note.txt", "same bytes\n")
	status := dirtyContentStatus("? note.txt")
	calls := 0
	s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			if err := os.Rename(root, filepath.Join(parent, "original")); err != nil {
				return nil, err
			}
			if err := os.Mkdir(root, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("same bytes\n"), 0o644); err != nil {
				return nil, err
			}
		}
		return status, nil
	})
	got, err := s.Sample(context.Background())
	if !errors.Is(err, ErrDirtyUnavailable) || !reflect.DeepEqual(got, DirtySnapshot{}) {
		t.Fatalf("root replacement accepted: got=%+v err=%v", got, err)
	}
	if len(s.contentCache) != 0 {
		t.Fatal("root-replaced sample published cache")
	}
}
func TestDirtyContentQuietWindowDoesNotPromoteYoungDigest(t *testing.T) {
	now := time.Now()
	stamp := now.Add(-time.Millisecond)
	version := dirtyFileVersion{size: 12, mtime: stamp.UnixNano(), changeSec: stamp.Unix(), changeNsec: int64(stamp.Nanosecond()), device: 1, inode: 2, mode: 0o644, cacheable: true}
	// Distinct bytes share a coarse-but-nonzero version. An ineligible digest
	// must not become eligible by aging; only a fresh old-stamp hash may do so.
	first := dirtyContentMemo{version: version, sha256: "first-bytes", hashedAt: now, reusable: false}
	other := dirtyContentMemo{version: version, sha256: "other-bytes", hashedAt: now, reusable: false}
	if first.version != other.version || first.sha256 == other.sha256 {
		t.Fatal("bad equal-metadata fixture")
	}
	later := now.Add(dirtyContentQuietWindow + time.Second)
	if dirtyStampQuiet(version, now) || !dirtyStampQuiet(version, later) {
		t.Fatal("fixture does not straddle quiet window")
	}
	if dirtyMemoReusable(first, version, now) || dirtyMemoReusable(first, version, later) {
		t.Fatal("young digest became reusable without another hash")
	}
	other.hashedAt, other.reusable = later, true
	if !dirtyMemoReusable(other, version, later.Add(time.Millisecond)) {
		t.Fatal("fresh old-stamp hash did not qualify")
	}
	unsupported := version
	unsupported.cacheable = false
	if dirtyMemoReusable(other, unsupported, later) {
		t.Fatal("unsupported evidence reused")
	}
	future := version
	future.changeSec = later.Add(time.Second).Unix()
	if dirtyStampQuiet(future, later) {
		t.Fatal("future inode timestamp reused")
	}
	if dirtyMemoReusable(other, version, later.Add(-time.Millisecond)) {
		t.Fatal("clock rollback before hash reused")
	}
}
func TestDirtyContentClockDiscontinuityDisablesReuse(t *testing.T) {
	for _, tc := range []struct {
		name            string
		wall, monotonic time.Duration
		want            bool
	}{
		{"ordinary", 15 * time.Second, 15 * time.Second, true},
		{"rollback", 14 * time.Second, 15 * time.Second, false},
		{"forward_jump", 16 * time.Second, 15 * time.Second, false},
		{"negative_wall", -time.Second, time.Second, false},
		{"negative_monotonic", time.Second, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirtyClockContinuous(tc.wall, tc.monotonic); got != tc.want {
				t.Fatalf("clock continuity=%t want%t", got, tc.want)
			}
		})
	}
}
func TestDirtyContentSamplerCacheDoesNotCrossPhysicalRoots(t *testing.T) {
	for _, mutateSamplerRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_root_%t", mutateSamplerRoot), func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "checkout")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			oldPath := dirtyContentWrite(t, root, "note.txt", "first bytes\n")
			status := dirtyContentStatus("? note.txt")
			s := dirtyContentFake(t, root, func(context.Context, string, ...string) ([]byte, error) { return status, nil })
			before := dirtyContentSample(t, s)
			oldRoot := s.contentCacheRoot
			oldInfo, err := os.Stat(oldPath)
			if err != nil {
				t.Fatal(err)
			}
			if mutateSamplerRoot {
				root = filepath.Join(parent, "other-checkout")
				s.root = root // Exercise private root-relative cache scoping.
			} else if err := os.Rename(root, filepath.Join(parent, "original")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			path := dirtyContentWrite(t, root, "note.txt", "other bytes\n")
			if err := os.Chtimes(path, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
				t.Fatal(err)
			}
			after := dirtyContentSample(t, s)
			if after.Fingerprint == before.Fingerprint || os.SameFile(oldRoot, s.contentCacheRoot) {
				t.Fatal("root-relative cache leaked across physical roots")
			}
		})
	}
}

func BenchmarkDirtyContentSampleReal(b *testing.B) {
	for _, tc := range []struct {
		name    string
		dirty   int
		tracked bool
	}{{"clean", 0, false}, {"dirty_one", 1, false}, {"dirty_32", 32, false}, {"tracked_one", 1, true}} {
		b.Run(tc.name, func(b *testing.B) {
			repo := b.TempDir()
			dirtyContentGit(b, repo, "init", "-b", "main")
			dirtyContentWrite(b, repo, "seed.txt", "original bytes\n")
			dirtyContentGit(b, repo, "add", "--", "seed.txt")
			dirtyContentGit(b, repo, "commit", "-m", "initial")
			for i := 0; i < tc.dirty; i++ {
				path := fmt.Sprintf("dirty-%02d.txt", i)
				if tc.tracked {
					path = "seed.txt"
				}
				dirtyContentWrite(b, repo, path, strings.Repeat("x", 1024))
			}
			s, err := NewDirtySampler(repo, "", "")
			if err != nil {
				b.Fatal(err)
			}
			originalRun := s.run
			commands := 0
			s.run = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				commands++
				return originalRun(ctx, dir, args...)
			}
			snap, err := s.Sample(context.Background())
			if err != nil || len(snap.Entries) != tc.dirty {
				b.Fatalf("fixture entries=%d want%d err=%v", len(snap.Entries), tc.dirty, err)
			}
			if tc.dirty > 0 {
				// Identical baseline/fix fixture setup, outside measurements;
				// production never sleeps. This exceeds the 2s cache quiet window.
				time.Sleep(2100 * time.Millisecond)
				if _, err := s.Sample(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			commands = 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Sample(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(commands)/float64(b.N), "git_commands/op")
		})
	}
}
