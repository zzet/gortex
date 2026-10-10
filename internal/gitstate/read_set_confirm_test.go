package gitstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readSetRepo is a committed repository with pkg/a.txt, pkg/b.txt, pkg/s.txt,
// other/c.txt and other/d.txt, a working-tree edit of pkg/a.txt and an
// untracked pkg/u.txt. The read set (readSetFiles, readSetDirs) reads pkg
// whole and other/c.txt alone; other/d.txt is not read.
// It returns the checkout root (the linked worktree when linked is set)
// after waiting out the change-stamp margin, so the setup reads as settled.
func readSetRepo(t *testing.T, linked bool) string {
	t.Helper()
	repo := initRepo(t, filepath.Join(tempRoot(t), "repo"))
	write := func(root, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"pkg/a.txt": "a\n", "pkg/b.txt": "b\n", "pkg/s.txt": "s\n", "other/c.txt": "c\n", "other/d.txt": "d\n"} {
		write(repo, name, body)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "files")
	root := repo
	if linked {
		root = addWorktree(t, repo, filepath.Join(filepath.Dir(repo), "linked"), "-b", "linked")
	}
	write(root, "pkg/a.txt", "a edited\n")
	write(root, "pkg/u.txt", "untracked\n")
	time.Sleep(readSetChangeMargin + 20*time.Millisecond)
	return root
}

var readSetFiles = []string{"pkg/a.txt", "pkg/b.txt", "pkg/u.txt", "pkg/go.mod", "go.mod", "other/c.txt"}
var readSetDirs = []string{"pkg"}

func sampleForReadSet(t *testing.T, root string) (*DirtySampler, DirtySnapshot) {
	t.Helper()
	s, err := NewDirtySampler(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s, before
}

func TestConfirmReadSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		linked    bool
		change    func(t *testing.T, root string, s *DirtySampler)
		confirmed bool
		rehashed  int
		reason    string
	}{
		{name: "nothing_changed", confirmed: true},
		{name: "nothing_changed_linked_worktree", linked: true, confirmed: true},
		{name: "unrelated_file_appears", confirmed: true, change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "other/new.txt", "new\n")
		}},
		{name: "unrelated_file_changes", confirmed: true, change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "other/d.txt", "d edited\n")
		}},
		{name: "unrelated_file_removed_beside_a_context_file", confirmed: true, change: func(t *testing.T, root string, _ *DirtySampler) {
			// A file read alone does not list its directory: removing an
			// unread neighbour moves the directory's stamp, which only
			// matters for a read path that went missing.
			if err := os.Remove(filepath.Join(root, "other/d.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "context_file_removed", reason: "may have been removed", change: func(t *testing.T, root string, _ *DirtySampler) {
			if err := os.Remove(filepath.Join(root, "other/c.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unread_sibling_in_a_read_directory_changes", reason: "pkg/s.txt", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/s.txt", "s edited\n")
		}},
		{name: "dirty_read_file_rewritten_with_same_bytes", confirmed: true, rehashed: 1, change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/a.txt", "a edited\n")
		}},
		{name: "clean_read_file_changes", reason: "changed since the sample: pkg/b.txt", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/b.txt", "b edited\n")
		}},
		{name: "clean_read_file_rewritten_with_same_bytes", reason: "pkg/b.txt", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/b.txt", "b\n")
		}},
		{name: "dirty_read_file_changes", reason: "content changed since the sample: pkg/a.txt", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/a.txt", "a edited twice\n")
		}},
		{name: "untracked_read_file_changes", reason: "pkg/u.txt", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/u.txt", "untracked edited\n")
		}},
		{name: "read_file_removed", reason: "read directory changed", change: func(t *testing.T, root string, _ *DirtySampler) {
			if err := os.Remove(filepath.Join(root, "pkg/b.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file_added_to_a_read_directory", reason: "read directory changed", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "pkg/new.txt", "new\n")
		}},
		{name: "missing_manifest_appears_at_an_ancestor", reason: "go.mod", change: func(t *testing.T, root string, _ *DirtySampler) {
			writeReadSetFile(t, root, "go.mod", "module x\n")
		}},
		{name: "head_moves", reason: "HEAD", change: func(t *testing.T, root string, _ *DirtySampler) {
			git(t, root, "commit", "-q", "--allow-empty", "-m", "moved")
		}},
		{name: "head_moves_linked_worktree", linked: true, reason: "HEAD", change: func(t *testing.T, root string, _ *DirtySampler) {
			git(t, root, "commit", "-q", "--allow-empty", "-m", "moved")
		}},
		{name: "a_later_sample_disagrees", reason: "latest sample", change: func(t *testing.T, root string, s *DirtySampler) {
			writeReadSetFile(t, root, "other/new.txt", "new\n")
			if _, err := s.Sample(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := readSetRepo(t, tc.linked)
			s, before := sampleForReadSet(t, root)
			if len(before.Contents) == 0 {
				t.Fatal("fixture sampled clean")
			}
			if tc.change != nil {
				tc.change(t, root, s)
			}
			got, err := s.ConfirmReadSet(context.Background(), before, readSetFiles, readSetDirs)
			if err != nil {
				t.Fatal(err)
			}
			// Windows supplies no trusted change stamps. Confirmation must
			// conservatively refuse, even when the content did not change.
			// A mismatched latest fingerprint is checked before stamp support.
			if !s.changeStampsTrusted() && tc.name != "a_later_sample_disagrees" {
				if got.Confirmed || !strings.Contains(got.Reason, "no change stamps") || got.Rehashed != 0 || got.Files != 0 || got.Dirs != 0 {
					t.Fatalf("unsupported change stamps must refuse without claiming checked paths: %+v", got)
				}
				return
			}
			if got.Confirmed != tc.confirmed {
				t.Fatalf("confirmed = %v (reason %q), want %v", got.Confirmed, got.Reason, tc.confirmed)
			}
			if !tc.confirmed && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason %q does not name %q", got.Reason, tc.reason)
			}
			if tc.confirmed && got.Rehashed != tc.rehashed {
				t.Fatalf("rehashed = %d, want %d", got.Rehashed, tc.rehashed)
			}
			if tc.confirmed && (got.Dirs != 1 || got.Files < 7) {
				t.Fatalf("checked %d files in %d dirs, want the read set", got.Files, got.Dirs)
			}
		})
	}
}

func TestConfirmReadSetRefusesWithoutEvidence(t *testing.T) {
	root := readSetRepo(t, false)
	s, before := sampleForReadSet(t, root)
	other := before
	other.Fingerprint = "not-the-sample"
	if got, _ := s.ConfirmReadSet(context.Background(), other, readSetFiles, readSetDirs); got.Confirmed {
		t.Fatal("confirmed a fingerprint no sample carried")
	}
	fresh, err := NewDirtySampler(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.ConfirmReadSet(context.Background(), before, readSetFiles, readSetDirs); got.Confirmed {
		t.Fatal("a sampler with no sample confirmed")
	}
	if got, _ := s.ConfirmReadSet(context.Background(), before, []string{"../outside"}, []string{"../x"}); got.Confirmed {
		t.Fatal("a read directory outside the checkout confirmed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ConfirmReadSet(ctx, before, readSetFiles, readSetDirs); err == nil {
		t.Fatal("a canceled confirmation returned no error")
	}
}

func TestConfirmReadSetRefusesWithoutTrustedChangeStamps(t *testing.T) {
	root := readSetRepo(t, false)
	s, before := sampleForReadSet(t, root)
	// Exercise the conservative filesystem fallback on every test platform.
	s.stampsChecked, s.stampsTrusted = true, false
	got, err := s.ConfirmReadSet(context.Background(), before, readSetFiles, readSetDirs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confirmed || !strings.Contains(got.Reason, "no change stamps") || got.Rehashed != 0 || got.Files != 0 || got.Dirs != 0 {
		t.Fatalf("unsupported change stamps must refuse without claiming checked paths: %+v", got)
	}
}

func writeReadSetFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ConfirmReadSetContent confirms a read file that moved after the sample by
// the bytes the build parsed from it, against the sample it names even when a
// later sample replaced it as the latest; everything it did not parse, the
// directories it read and HEAD stay confirmed by their stamps.
func TestConfirmReadSetContent(t *testing.T) {
	sampled := BlobSHA256([]byte("a edited\n"))
	for _, tc := range []struct {
		name      string
		parsed    map[string]string
		change    func(t *testing.T, root string, s *DirtySampler)
		confirmed bool
		proven    int
		reason    string
	}{
		{name: "nothing_changed", parsed: map[string]string{"pkg/a.txt": sampled}, confirmed: true},
		{name: "parsed_file_saved_again", parsed: map[string]string{"pkg/a.txt": sampled}, confirmed: true, proven: 1,
			change: func(t *testing.T, root string, _ *DirtySampler) {
				writeReadSetFile(t, root, "pkg/a.txt", "a edited twice\n")
			}},
		{name: "parsed_file_saved_again_with_the_same_bytes", parsed: map[string]string{"pkg/a.txt": sampled}, confirmed: true, proven: 1,
			change: func(t *testing.T, root string, _ *DirtySampler) {
				writeReadSetFile(t, root, "pkg/a.txt", "a edited\n")
			}},
		{name: "parsed_file_saved_again_and_a_later_sample_taken", parsed: map[string]string{"pkg/a.txt": sampled}, confirmed: true, proven: 1,
			change: func(t *testing.T, root string, s *DirtySampler) {
				writeReadSetFile(t, root, "pkg/a.txt", "a edited twice\n")
				if _, err := s.Sample(context.Background()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "parsed_bytes_are_not_the_sample", parsed: map[string]string{"pkg/a.txt": BlobSHA256([]byte("a edited twice\n"))},
			reason: "parsed from bytes other than the sample's: pkg/a.txt"},
		{name: "unparsed_dirty_file_changes", parsed: map[string]string{}, reason: "holds no bytes for changed since the sample: pkg/a.txt",
			change: func(t *testing.T, root string, _ *DirtySampler) {
				writeReadSetFile(t, root, "pkg/a.txt", "a edited twice\n")
			}},
		// A dirty file the build holds no bytes for may have been read
		// unrecorded while it moved: a re-hash equal to the sample's (here
		// it never changed bytes at all) does not confirm it, as it does
		// for a build without a proof.
		{name: "unparsed_dirty_file_rewritten_with_the_same_bytes", parsed: map[string]string{}, reason: "holds no bytes for changed since the sample: pkg/a.txt",
			change: func(t *testing.T, root string, _ *DirtySampler) {
				writeReadSetFile(t, root, "pkg/a.txt", "a edited\n")
			}},
		{name: "unparsed_sibling_in_a_read_directory_changes", parsed: map[string]string{"pkg/a.txt": sampled}, reason: "pkg/s.txt",
			change: func(t *testing.T, root string, _ *DirtySampler) {
				writeReadSetFile(t, root, "pkg/s.txt", "s edited\n")
			}},
		{name: "file_added_to_a_read_directory", parsed: map[string]string{"pkg/a.txt": sampled}, reason: "read directory changed",
			change: func(t *testing.T, root string, _ *DirtySampler) {
				writeReadSetFile(t, root, "pkg/new.txt", "new\n")
			}},
		{name: "parsed_file_removed", parsed: map[string]string{"pkg/a.txt": sampled}, reason: "read directory changed",
			change: func(t *testing.T, root string, _ *DirtySampler) {
				if err := os.Remove(filepath.Join(root, "pkg/a.txt")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "head_moves", parsed: map[string]string{"pkg/a.txt": sampled}, reason: "HEAD",
			change: func(t *testing.T, root string, _ *DirtySampler) {
				git(t, root, "commit", "-q", "--allow-empty", "-m", "moved")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := readSetRepo(t, true)
			s, before := sampleForReadSet(t, root)
			var held bool
			for _, content := range before.Contents {
				if content.Path == "pkg/a.txt" {
					held = content.SHA256 == sampled
				}
			}
			if !held {
				t.Fatalf("the sample's content identity of pkg/a.txt is not BlobSHA256 of its bytes: %+v", before.Contents)
			}
			if tc.change != nil {
				tc.change(t, root, s)
			}
			got, err := s.ConfirmReadSetContent(context.Background(), before, readSetFiles, readSetDirs, tc.parsed)
			if err != nil {
				t.Fatal(err)
			}
			if !s.changeStampsTrusted() {
				if got.Confirmed {
					t.Fatalf("confirmed without trusted change stamps: %+v", got)
				}
				return
			}
			if got.Confirmed != tc.confirmed {
				t.Fatalf("confirmed = %v (reason %q), want %v", got.Confirmed, got.Reason, tc.confirmed)
			}
			if !tc.confirmed && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason %q does not name %q", got.Reason, tc.reason)
			}
			if got.ContentProven != tc.proven {
				t.Fatalf("content proven = %d, want %d (%+v)", got.ContentProven, tc.proven, got)
			}
		})
	}
}

// The content confirmation names the build's own sample, which the build
// holds: a snapshot this sampler did not take, or one whose fingerprint was
// rewritten, is refused; the build's own sample still confirms after twenty
// samples of other states (freshness proofs, ticket captures, fences) were
// taken while it ran — more than the recent-sample cache holds.
func TestConfirmReadSetContentHoldsTheBuildsOwnSample(t *testing.T) {
	root := readSetRepo(t, false)
	s, before := sampleForReadSet(t, root)
	other := before
	other.Fingerprint = "not-the-sample"
	if got, _ := s.ConfirmReadSetContent(context.Background(), other, readSetFiles, readSetDirs, nil); got.Confirmed {
		t.Fatal("confirmed a fingerprint no sample carried")
	}
	foreign, err := SampleDirty(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ConfirmReadSetContent(context.Background(), foreign, readSetFiles, readSetDirs, nil); got.Confirmed {
		t.Fatal("confirmed a sample this sampler did not take")
	}
	for i := 0; i < 20; i++ {
		writeReadSetFile(t, root, "other/new.txt", strings.Repeat("n", i+1)+"\n")
		if _, err := s.Sample(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := s.LatestSampleOf(before.Fingerprint, time.Time{}); ok {
		t.Fatal("the build's sample is still in the recent-sample cache: the test does not exercise eviction")
	}
	got, err := s.ConfirmReadSetContent(context.Background(), before, readSetFiles, readSetDirs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.changeStampsTrusted() {
		if got.Confirmed {
			t.Fatalf("confirmed without trusted change stamps: %+v", got)
		}
		return
	}
	if !got.Confirmed {
		t.Fatalf("the build's own sample did not confirm after other samples were taken: %+v", got)
	}
}

// A file read while it held other bytes and restored before a later sample of
// the same state was taken is refused: the build's own sample began before
// the read, and the restore stamped the file after it. A later sample of the
// same fingerprint, which began after the restore, never stands in for it —
// for the content confirmation or the stamp-only one.
func TestConfirmReadSetRefusesAFileRestoredBeforeALaterSampleOfTheSameState(t *testing.T) {
	sampled := BlobSHA256([]byte("a edited\n"))
	for _, tc := range []struct {
		name    string
		confirm func(s *DirtySampler, before DirtySnapshot) (ReadSetConfirmation, error)
	}{
		{"content", func(s *DirtySampler, before DirtySnapshot) (ReadSetConfirmation, error) {
			return s.ConfirmReadSetContent(context.Background(), before, readSetFiles, readSetDirs, map[string]string{"pkg/a.txt": sampled})
		}},
		{"stamps", func(s *DirtySampler, before DirtySnapshot) (ReadSetConfirmation, error) {
			return s.ConfirmReadSet(context.Background(), before, readSetFiles, readSetDirs)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := readSetRepo(t, false)
			s, before := sampleForReadSet(t, root)
			// other/c.txt, clean and read alone, holds other bytes while the
			// build reads it, then is restored.
			writeReadSetFile(t, root, "other/c.txt", "c moved\n")
			writeReadSetFile(t, root, "other/c.txt", "c\n")
			time.Sleep(readSetChangeMargin + 20*time.Millisecond)
			later, err := s.Sample(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if later.Fingerprint != before.Fingerprint {
				t.Fatal("the restore did not bring the checkout back to the build's sample")
			}
			got, err := tc.confirm(s, before)
			if err != nil {
				t.Fatal(err)
			}
			if got.Confirmed || (s.changeStampsTrusted() && !strings.Contains(got.Reason, "other/c.txt")) {
				t.Fatalf("a read file restored before a later sample of the same state = %+v, want refused at other/c.txt", got)
			}
		})
	}
}

// ConfirmReadSetSettled holds every read-set path the caller cannot judge by
// recorded bytes to its change stamp: one moved since the build's own sample
// began is reported even when the working copy is back in the sampled state
// (same bytes, an absent manifest created and removed, a file added to a read
// directory and removed), while judged paths, paths outside the read set and
// — unless entries is set — an unread entry of a read directory are not.
func TestConfirmReadSetSettled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		judged  map[string]bool
		entries bool
		change  func(t *testing.T, root string)
		moved   string
	}{
		{name: "nothing_changed"},
		{name: "unread_file_changes", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "other/d.txt", "d edited\n")
		}},
		{name: "read_file_rewritten_with_its_bytes", moved: "other/c.txt", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "other/c.txt", "c moved\n")
			writeReadSetFile(t, root, "other/c.txt", "c\n")
		}},
		{name: "judged_read_file_rewritten", judged: map[string]bool{"other/c.txt": true}, change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "other/c.txt", "c moved\n")
		}},
		{name: "dirty_read_file_rewritten_with_its_bytes", moved: "pkg/a.txt", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "pkg/a.txt", "a moved\n")
			writeReadSetFile(t, root, "pkg/a.txt", "a edited\n")
		}},
		{name: "absent_manifest_created_and_removed", moved: "go.mod", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "go.mod", "module x\n")
			if err := os.Remove(filepath.Join(root, "go.mod")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file_added_to_a_read_directory_and_removed", moved: "pkg", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "pkg/new.txt", "new\n")
			if err := os.Remove(filepath.Join(root, "pkg/new.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unread_entry_of_a_read_directory_rewritten", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "pkg/s.txt", "s\n")
		}},
		{name: "unread_entry_of_a_read_directory_rewritten_entries", entries: true, moved: "pkg/s.txt", change: func(t *testing.T, root string) {
			writeReadSetFile(t, root, "pkg/s.txt", "s\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := readSetRepo(t, false)
			s, before := sampleForReadSet(t, root)
			if tc.change != nil {
				tc.change(t, root)
			}
			moved, proven, err := s.ConfirmReadSetSettled(context.Background(), before, readSetFiles, readSetDirs, tc.entries, tc.judged)
			if err != nil {
				t.Fatal(err)
			}
			if !s.changeStampsTrusted() {
				if proven {
					t.Fatalf("proven without trusted change stamps (moved %q)", moved)
				}
				return
			}
			if !proven || moved != tc.moved {
				t.Fatalf("ConfirmReadSetSettled = %q (proven %v), want %q", moved, proven, tc.moved)
			}
		})
	}
	t.Run("no_evidence", func(t *testing.T) {
		root := readSetRepo(t, false)
		s, before := sampleForReadSet(t, root)
		other := before
		other.Fingerprint = "not-the-sample"
		if _, proven, _ := s.ConfirmReadSetSettled(context.Background(), other, readSetFiles, readSetDirs, false, nil); proven {
			t.Fatal("proven against a fingerprint the sample does not carry")
		}
		foreign, err := SampleDirty(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if _, proven, _ := s.ConfirmReadSetSettled(context.Background(), foreign, readSetFiles, readSetDirs, false, nil); proven {
			t.Fatal("proven against a sample this sampler did not take")
		}
		s.stampsChecked, s.stampsTrusted = true, false
		if _, proven, _ := s.ConfirmReadSetSettled(context.Background(), before, readSetFiles, readSetDirs, false, nil); proven {
			t.Fatal("proven without trusted change stamps")
		}
	})
}

// LatestSampleOf finds a recent sample of a state after a sample of another
// state replaced it as the latest, but only one that began at or after the
// instant asked for, and never one pushed out of the recent samples.
func TestLatestSampleOf(t *testing.T) {
	root := readSetRepo(t, false)
	admitted := time.Now()
	s, before := sampleForReadSet(t, root)
	writeReadSetFile(t, root, "pkg/a.txt", "a saved again\n")
	moved, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if moved.Fingerprint == before.Fingerprint {
		t.Fatal("the save did not move the sample")
	}
	if latest, _, _ := s.LatestSampleSince(admitted); latest.Fingerprint != moved.Fingerprint {
		t.Fatal("the latest sample is not the moved one")
	}
	got, started, ok := s.LatestSampleOf(before.Fingerprint, admitted)
	if !ok || got.Fingerprint != before.Fingerprint || started.Before(admitted) || got.HeadCommit != before.HeadCommit {
		t.Fatalf("LatestSampleOf(before) = %v %v %v, want the earlier sample begun after %v", got.Fingerprint, started, ok, admitted)
	}
	if _, _, ok := s.LatestSampleOf(before.Fingerprint, time.Now()); ok {
		t.Fatal("a sample begun before the instant asked for was returned")
	}
	if _, _, ok := s.LatestSampleOf("not-a-sample", admitted); ok {
		t.Fatal("a fingerprint no sample carried was found")
	}
	for i := 0; i < sampleEvidenceHistory; i++ {
		writeReadSetFile(t, root, "other/new.txt", strings.Repeat("n", i+1)+"\n")
		if _, err := s.Sample(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := s.LatestSampleOf(before.Fingerprint, admitted); ok {
		t.Fatal("a sample pushed out of the recent ones was found")
	}
}
