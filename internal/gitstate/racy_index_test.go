package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeRacyIndexRepo commits files files whose modification time is one past
// instant, records that stat in the index, then stamps the index file with
// that same instant: every entry's recorded mtime now equals the index's own,
// which is exactly the racily clean state a fresh `git worktree add` leaves.
func makeRacyIndexRepo(t *testing.T, files int) string {
	t.Helper()
	repo := dirtyContentRepo(t)
	body := strings.Repeat("racily clean content line\n", 600)
	for i := 0; i < files; i++ {
		dirtyContentWrite(t, repo, fmt.Sprintf("pkg/f%04d.txt", i), body)
	}
	dirtyContentGit(t, repo, "add", "-A")
	dirtyContentGit(t, repo, "commit", "-q", "-m", "files")
	// Racy refresh is a per-path stat/content check. Keep every regular file
	// and its full 600-line payload, but share one blob: creating hundreds of
	// unrelated loose objects is not part of the refresh contract.
	staged := dirtyContentGit(t, repo, "ls-files", "--stage", "-z", "--", "pkg")
	entries := strings.Split(strings.TrimSuffix(staged, "\x00"), "\x00")
	if len(entries) != files {
		t.Fatalf("racy fixture has %d index entries, want %d", len(entries), files)
	}
	objects := make(map[string]bool)
	for i, entry := range entries {
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid fixture index entry %q", entry)
		}
		metadata := strings.Fields(parts[0])
		wantPath := fmt.Sprintf("pkg/f%04d.txt", i)
		if len(metadata) != 3 || metadata[0] != "100644" || !isOID(metadata[1]) || metadata[2] != "0" || parts[1] != wantPath {
			t.Fatalf("unexpected fixture index entry %q for %s", entry, wantPath)
		}
		objects[metadata[1]] = true
		info, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(parts[1])))
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(body)) {
			t.Fatalf("fixture path %s is not a complete regular payload: info=%v err=%v", parts[1], info, err)
		}
	}
	if len(objects) != 1 {
		t.Fatalf("racy fixture has %d content objects, want one shared blob", len(objects))
	}
	t.Logf("racy fixture: %d independent regular paths, one content object, %d bytes/path", files, len(body))
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	err := filepath.WalkDir(repo, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(path, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			return err
		}
		return os.Chtimes(path, past, past)
	})
	if err != nil {
		t.Fatal(err)
	}
	dirtyContentGit(t, repo, "update-index", "-q", "--refresh")
	if err := os.Chtimes(filepath.Join(repo, ".git", "index"), past, past); err != nil {
		t.Fatal(err)
	}
	return repo
}

var refreshIndexTrace = regexp.MustCompile(`performance: ([0-9.]+) s:\s+refresh index`)

// statusRefreshSeconds runs the daemon's own status (no optional locks, so
// nothing is written back) and returns git's "refresh index" time.
func statusRefreshSeconds(t *testing.T, repo string) float64 {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "--no-optional-locks", "status", "--porcelain=v2", "-z", "--untracked-files=all")
	cmd.Env = append(os.Environ(), "GIT_TRACE_PERFORMANCE=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	match := refreshIndexTrace.FindSubmatch(out)
	if match == nil {
		t.Fatalf("no refresh-index trace in:\n%s", out)
	}
	seconds, err := strconv.ParseFloat(string(match[1]), 64)
	if err != nil {
		t.Fatal(err)
	}
	return seconds
}

// A racily clean index makes every status hash every tracked file, and the
// daemon's statuses never write the refreshed index back, so it stays slow.
// RefreshRacyIndex detects it from the index file and heals it with one
// locked refresh: the next status is fast.
func TestRefreshRacyIndexHealsARacilyCleanCheckout(t *testing.T) {
	repo := makeRacyIndexRepo(t, 800)
	s := dirtyContentSampler(t, repo)
	report := s.RacyIndex()
	if !report.Known || report.Racy < 800 || !report.NeedsRefresh() {
		t.Fatalf("racy fixture not detected: %+v", report)
	}
	slow := min(statusRefreshSeconds(t, repo), statusRefreshSeconds(t, repo))
	// The daemon's own status does not heal it.
	if again := s.RacyIndex(); !again.NeedsRefresh() {
		t.Fatalf("a no-optional-locks status healed the index: %+v", again)
	}

	before, after, ran, err := s.RefreshRacyIndex(context.Background())
	if err != nil || !ran {
		t.Fatalf("refresh: ran=%v err=%v", ran, err)
	}
	if before.Racy < 800 || after.NeedsRefresh() {
		t.Fatalf("refresh did not heal the index: before=%+v after=%+v", before, after)
	}
	fast := max(statusRefreshSeconds(t, repo), statusRefreshSeconds(t, repo))
	t.Logf("refresh index: %.4fs racily clean, %.4fs after the refresh", slow, fast)
	if fast*3 > slow {
		t.Fatalf("status refresh-index time %.4fs after the refresh, %.4fs before: not healed", fast, slow)
	}
	if _, _, ranAgain, err := s.RefreshRacyIndex(context.Background()); ranAgain || err != nil {
		t.Fatalf("a healthy index was refreshed again: ran=%v err=%v", ranAgain, err)
	}
}

// A healthy index is left alone, and a locked one is not waited for.
func TestRefreshRacyIndexSkipsHealthyAndLockedIndexes(t *testing.T) {
	healthy := dirtyContentRepo(t)
	time.Sleep(1100 * time.Millisecond) // past the index's own second
	dirtyContentGit(t, healthy, "update-index", "-q", "--refresh")
	if _, _, ran, err := dirtyContentSampler(t, healthy).RefreshRacyIndex(context.Background()); ran || err != nil {
		t.Fatalf("healthy index: ran=%v err=%v", ran, err)
	}

	racy := makeRacyIndexRepo(t, 20)
	lock := filepath.Join(racy, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := dirtyContentSampler(t, racy)
	if _, _, _, err := s.RefreshRacyIndex(context.Background()); !errors.Is(err, ErrIndexLocked) {
		t.Fatalf("locked index: %v, want ErrIndexLocked", err)
	}
	if !s.RacyIndex().NeedsRefresh() {
		t.Fatal("a locked index was written")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, after, ran, err := s.RefreshRacyIndex(context.Background()); !ran || err != nil || after.NeedsRefresh() {
		t.Fatalf("retry after the lock: ran=%v err=%v after=%+v", ran, err, after)
	}
}

// The refresh never makes a sample wait and never runs beside an edit's
// sample: it takes no sampling lease (a background sample holding it does not
// stop it), and it does not start while an urgent sample holds the lease.
func TestRefreshRacyIndexYieldsToAnEditsSampleOnly(t *testing.T) {
	repo := makeRacyIndexRepo(t, 20)
	s := dirtyContentSampler(t, repo)

	urgent, err := s.acquire(WithUrgentSample(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ran, err := s.RefreshRacyIndex(context.Background()); ran || !errors.Is(err, ErrRefreshYielded) {
		urgent()
		t.Fatalf("refresh beside an edit's sample: ran=%v err=%v", ran, err)
	}
	urgent()

	background, err := s.Hold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer background()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := s.RefreshRacyIndex(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh waited for a background sample's lease")
	}
	if s.RacyIndex().NeedsRefresh() {
		t.Fatal("the refresh did not heal the index")
	}
}
