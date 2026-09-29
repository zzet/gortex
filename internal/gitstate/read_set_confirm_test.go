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

func writeReadSetFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
