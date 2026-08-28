package gitstate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDirtySamplerUnchangedFleetUsesOneCommandPerCheckout(t *testing.T) {
	for _, size := range []int{1, 64, 512} {
		t.Run(fmt.Sprintf("fleet_%d", size), func(t *testing.T) {
			var commands atomic.Int64
			run := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				commands.Add(1)
				return porcelainBranch(testCommitA, "main"), nil
			}
			samplers := make([]*DirtySampler, size)
			root := t.TempDir()
			for i := range samplers {
				samplers[i] = newDirtySampler(root, testCommitA, testTreeA, run)
			}

			start := make(chan struct{})
			errCh := make(chan error, size)
			var wg sync.WaitGroup
			for _, sampler := range samplers {
				wg.Add(1)
				go func(sampler *DirtySampler) {
					defer wg.Done()
					<-start
					_, err := sampler.Sample(context.Background())
					errCh <- err
				}(sampler)
			}
			close(start)
			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					t.Fatalf("Sample: %v", err)
				}
			}
			if got := commands.Load(); got != int64(size) {
				t.Fatalf("%d unchanged checkouts ran %d commands, want one each", size, got)
			}
		})
	}
}

func benchmarkDirtySamplerRepo(b *testing.B) (root, commit, tree string) {
	b.Helper()
	root = filepath.Join(b.TempDir(), "repo")
	run := func(args ...string) string {
		b.Helper()
		cmd := exec.Command("git", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			b.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main", root)
	if err := os.WriteFile(filepath.Join(root, "seed.go"), []byte("package seed\n"), 0o644); err != nil {
		b.Fatalf("WriteFile: %v", err)
	}
	run("-C", root, "add", "seed.go")
	run("-C", root, "-c", "user.name=Gortex", "-c", "user.email=gortex@example.invalid", "commit", "-m", "seed")
	commit = run("-C", root, "rev-parse", "HEAD")
	tree = run("-C", root, "rev-parse", "HEAD^{tree}")
	return root, commit, tree
}

func BenchmarkDirtySamplerUnchangedReal(b *testing.B) {
	root, commit, tree := benchmarkDirtySamplerRepo(b)
	for _, size := range []int{1, 64, 512} {
		b.Run(fmt.Sprintf("fleet_%d", size), func(b *testing.B) {
			samplers := make([]*DirtySampler, size)
			for i := range samplers {
				var err error
				samplers[i], err = NewDirtySampler(root, commit, tree)
				if err != nil {
					b.Fatalf("NewDirtySampler: %v", err)
				}
			}
			b.ReportAllocs()
			b.ReportMetric(1, "commands/sample")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, sampler := range samplers {
					if _, err := sampler.Sample(context.Background()); err != nil {
						b.Fatalf("Sample: %v", err)
					}
				}
			}
		})
	}
}
