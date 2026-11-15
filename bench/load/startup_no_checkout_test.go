package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupLinkedWorktreeNoCheckout(t *testing.T) {
	for _, noCheckout := range []bool{false, true} {
		name := "populated-default"
		if noCheckout {
			name = "no-checkout"
		}
		t.Run(name, func(t *testing.T) {
			corpus := gitFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p, err := newPrivateRun(ctx, t.TempDir(), "/unused", []Corpus{corpus})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
			linked, err := addLinkedCheckouts(ctx, p, 2, noCheckout)
			if err != nil {
				t.Fatal(err)
			}
			if len(linked) != 2 {
				t.Fatalf("linked checkouts = %d, want 2", len(linked))
			}
			git := func(dir string, args ...string) string {
				t.Helper()
				data, err := p.git(ctx, dir, args...)
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			primary := p.Repos[0]
			common := git(primary.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
			gitDirs := make(map[string]bool)
			for _, cwd := range linked {
				if got := git(cwd.Dir, "rev-parse", "HEAD"); got != corpus.Commit {
					t.Fatalf("linked HEAD = %q, want pinned %q", got, corpus.Commit)
				}
				if got := git(cwd.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != common {
					t.Fatalf("linked common directory = %q, want %q", got, common)
				}
				admin := git(cwd.Dir, "rev-parse", "--absolute-git-dir")
				if admin == common || gitDirs[admin] {
					t.Fatalf("linked administration directory is not distinct: %q", admin)
				}
				gitDirs[admin] = true
				metadata, err := os.ReadFile(filepath.Join(cwd.Dir, ".git"))
				if err != nil || !strings.HasPrefix(string(metadata), "gitdir: ") {
					t.Fatalf("linked .git file: %q, %v", metadata, err)
				}
				target := filepath.Join(cwd.Dir, corpus.File)
				if noCheckout {
					if _, err := os.Lstat(target); !os.IsNotExist(err) {
						t.Fatalf("no-checkout tracked file exists: %v", err)
					}
					entries, err := os.ReadDir(cwd.Dir)
					if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
						t.Fatalf("no-checkout directory was populated: %v, %v", entries, err)
					}
					if cwd.File != target {
						t.Fatalf("hook must retain actual absent linked target, got %q", cwd.File)
					}
				} else if data, err := os.ReadFile(target); err != nil || !strings.Contains(string(data), "func Example()") || cwd.File != target {
					t.Fatalf("default populated target changed: %q, %v, hook %q", data, err, cwd.File)
				}
				if _, err := os.Stat(primary.File); err != nil {
					t.Fatalf("primary indexed source file changed: %v", err)
				}
			}
			if got := strings.Count(git(primary.Path, "worktree", "list", "--porcelain"), "worktree "); got != 3 {
				t.Fatalf("native Git worktree inventory = %d, want primary + 2", got)
			}
		})
	}
}

func TestStartupNoCheckoutOptionIsExplicitAndScoped(t *testing.T) {
	base := []string{"--binary", "/b", "--manifest", "/m", "--scratch", "/s"}
	for _, scenario := range []string{"startup", "read", "cold", "all"} {
		options, err := parseRun(append(base, "--scenario", scenario))
		if err != nil || options.Startup.NoCheckout {
			t.Fatalf("default %s options changed: %+v, %v", scenario, options.Startup, err)
		}
		options, err = parseRun(append(base, "--scenario", scenario, "--startup-no-checkout"))
		if scenario == "startup" {
			if err != nil || !options.Startup.NoCheckout {
				t.Fatalf("explicit startup no-checkout option = %+v, %v", options.Startup, err)
			}
		} else if err == nil {
			t.Fatalf("%s accepted startup-only no-checkout fixture", scenario)
		}
	}
}
