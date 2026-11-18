package gitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
)

// countingDirtySampler samples repo with the real git, counting status runs.
func countingDirtySampler(t *testing.T, repo string) (*DirtySampler, *int) {
	t.Helper()
	statuses := 0
	run := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "status" {
				statuses++
				break
			}
		}
		return gitcmd.Run(ctx, dir, args...)
	}
	return newDirtySampler(repo, "", "", run), &statuses
}

// settleDirtyChanges waits until every change made so far lies before the
// cutoff a sample started now would use.
func settleDirtyChanges() { time.Sleep(readSetChangeMargin + 30*time.Millisecond) }

// A dirty tree whose changes all predate the sample is proven by the change
// stamps: one status, no fence, and the same fingerprint the fence proves.
func TestDirtyStampProofTakesOneStatusForASettledTree(t *testing.T) {
	repo := dirtyContentRepo(t)
	dirtyContentWrite(t, repo, "seed.txt", "edited bytes\n")
	dirtyContentWrite(t, repo, "untracked/new.txt", "new file\n")
	if err := os.Remove(filepath.Join(repo, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	dirtyContentWrite(t, repo, "seed.txt", "edited again\n")
	settleDirtyChanges()

	s, statuses := countingDirtySampler(t, repo)
	if !s.changeStampsTrusted() {
		t.Skip("the test directory's filesystem gives no trusted change stamps")
	}
	snap, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *statuses != 1 {
		t.Fatalf("a settled dirty sample ran status %d times, want 1", *statuses)
	}
	if stamped, fenced := s.DirtyContentProofs(); stamped != 1 || fenced != 0 {
		t.Fatalf("proofs stamped=%d fenced=%d, want 1/0", stamped, fenced)
	}

	// The fence, forced by an untrusted-stamp sampler, proves the same state.
	fencedSampler, _ := countingDirtySampler(t, repo)
	fencedSampler.stampsChecked, fencedSampler.stampsTrusted = true, false
	fencedSnap, err := fencedSampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, fenced := fencedSampler.DirtyContentProofs(); fenced != 1 {
		t.Fatalf("an untrusted-stamp sampler was not fenced")
	}
	if snap.Fingerprint != fencedSnap.Fingerprint {
		t.Fatalf("stamp-proven fingerprint %s differs from the fenced one %s", snap.Fingerprint, fencedSnap.Fingerprint)
	}
}

// A dirty file changed after it was hashed and before its evidence is proven
// is detected: the stamp proof does not accept it, and the fence behind it
// refuses the sample. The change keeps the size and the modification time, so
// only the inode change stamp can tell. A HEAD moved in the same window is
// refused the same way.
func TestDirtyStampProofDetectsAChangeBetweenTheHashAndTheStamp(t *testing.T) {
	for _, scenario := range []string{"content", "head"} {
		t.Run(scenario, func(t *testing.T) {
			repo := dirtyContentRepo(t)
			path := dirtyContentWrite(t, repo, "seed.txt", "hashed bytes\n")
			settleDirtyChanges()
			s, _ := countingDirtySampler(t, repo)
			if !s.changeStampsTrusted() {
				t.Skip("the test directory's filesystem gives no trusted change stamps")
			}
			fired := false
			previous := dirtyContentHashed
			dirtyContentHashed = func() {
				if fired {
					return
				}
				fired = true
				switch scenario {
				case "content":
					info, err := os.Stat(path)
					if err != nil {
						t.Error(err)
						return
					}
					if err := os.WriteFile(path, []byte("hashed BYTES\n"), 0o644); err != nil {
						t.Error(err)
						return
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Error(err)
					}
				case "head":
					dirtyContentGit(t, repo, "switch", "-c", "moved-while-sampling")
				}
			}
			t.Cleanup(func() { dirtyContentHashed = previous })

			snap, err := s.Sample(context.Background())
			if !fired {
				t.Fatal("the hash-to-proof window was never reached")
			}
			if err == nil {
				t.Fatalf("a %s change between the hash and the proof was accepted: fingerprint %s", scenario, snap.Fingerprint)
			}
			// A caller tells a tree that moved under the sample from one that
			// cannot be sampled by the cause.
			if !errors.Is(err, ErrDirtyUnavailable) || !errors.Is(err, ErrDirtyMoved) {
				t.Fatalf("a %s change while sampling = %v, want ErrDirtyUnavailable caused by ErrDirtyMoved", scenario, err)
			}
			if stamped, fenced := s.DirtyContentProofs(); stamped != 0 || fenced != 1 {
				t.Fatalf("proofs stamped=%d fenced=%d, want 0/1 (the stamps must not prove a changed %s)", stamped, fenced, scenario)
			}
		})
	}
}

// A path changed inside the coarse-clock margin before the sample is not
// proven by its stamp: the sample falls back to the fence (and still
// succeeds, the tree being quiet).
func TestDirtyStampProofFencesAYoungChange(t *testing.T) {
	repo := dirtyContentRepo(t)
	dirtyContentWrite(t, repo, "seed.txt", "just written\n")
	s, statuses := countingDirtySampler(t, repo)
	if _, err := s.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stamped, fenced := s.DirtyContentProofs(); stamped != 0 || fenced != 1 || *statuses != 2 {
		t.Fatalf("young change: stamped=%d fenced=%d statuses=%d, want 0/1/2", stamped, fenced, *statuses)
	}
}

func rawSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// A sample taken right after the caller's own write, where that file is the
// only young one, is proven without the fence when the caller names the write
// (WithKnownWrite): one status. A second young file, or a named write whose
// bytes are not what is on disk, falls back to the fence.
func TestKnownWriteLetsTheCaptureSampleSkipTheFence(t *testing.T) {
	for _, scenario := range []string{"only_the_write_is_young", "another_file_is_young", "written_bytes_differ"} {
		t.Run(scenario, func(t *testing.T) {
			repo := dirtyContentRepo(t)
			dirtyContentWrite(t, repo, "settled.txt", "settled untracked bytes\n")
			settleDirtyChanges()
			written := dirtyContentWrite(t, repo, "seed.txt", "the edit's bytes\n")
			sha := rawSHA256(t, written)
			if scenario == "another_file_is_young" {
				dirtyContentWrite(t, repo, "other.txt", "someone else's bytes\n")
			}
			if scenario == "written_bytes_differ" {
				sha = strings.Repeat("0", 64)
			}
			s, statuses := countingDirtySampler(t, repo)
			if !s.changeStampsTrusted() {
				t.Skip("the test directory's filesystem gives no trusted change stamps")
			}
			if _, err := s.Sample(WithKnownWrite(context.Background(), "seed.txt", sha)); err != nil {
				t.Fatal(err)
			}
			stamped, fenced := s.DirtyContentProofs()
			if scenario == "only_the_write_is_young" {
				if *statuses != 1 || stamped != 1 || fenced != 0 {
					t.Fatalf("statuses=%d stamped=%d fenced=%d, want 1/1/0", *statuses, stamped, fenced)
				}
				return
			}
			if *statuses != 2 || stamped != 0 || fenced != 1 {
				t.Fatalf("statuses=%d stamped=%d fenced=%d, want the fence (2/0/1)", *statuses, stamped, fenced)
			}
		})
	}
}

// The known write proves the file only as the bytes written: a rewrite of it
// between the hash and the proof is refused (and the fence behind refuses the
// sample).
func TestKnownWriteDoesNotProveAFileChangedAfterItsHash(t *testing.T) {
	repo := dirtyContentRepo(t)
	written := dirtyContentWrite(t, repo, "seed.txt", "the edit's bytes\n")
	sha := rawSHA256(t, written)
	s, _ := countingDirtySampler(t, repo)
	if !s.changeStampsTrusted() {
		t.Skip("the test directory's filesystem gives no trusted change stamps")
	}
	fired := false
	previous := dirtyContentHashed
	dirtyContentHashed = func() {
		if !fired {
			fired = true
			if err := os.WriteFile(written, []byte("the edit's BYTES\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { dirtyContentHashed = previous })
	if _, err := s.Sample(WithKnownWrite(context.Background(), "seed.txt", sha)); err == nil {
		t.Fatal("a file rewritten after its hash was proven by the known write")
	}
	if stamped, _ := s.DirtyContentProofs(); stamped != 0 {
		t.Fatalf("stamped=%d, want 0", stamped)
	}
}
