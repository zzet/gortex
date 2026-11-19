package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type worktreeStartupFixture struct {
	Corpus               Corpus
	Initial, Final, Seed string
	Env                  []string
}

func newWorktreeStartupFixture(ctx context.Context, root string, files, changes, functions, calls int) (worktreeStartupFixture, error) {
	var fixture worktreeStartupFixture
	if files < 2 || changes < 1 || changes > files || functions < 1 || calls < 1 {
		return fixture, errors.New("startup fixture needs files >= 2, 1 <= changes <= files, functions >= 1 and calls >= 1")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fixture, err
	}
	gitConfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\nname = Startup benchmark\nemail = startup@example.invalid\n"), 0o600); err != nil {
		return fixture, err
	}
	fixture.Env = append(privateEnv(root, filepath.Join(root, "unused.sock"), gitConfig),
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	p := &privateRun{Root: root, Env: fixture.Env}
	repo := filepath.Join(root, "source")
	if err := os.Mkdir(repo, 0o700); err != nil {
		return fixture, err
	}
	fixture.Corpus = Corpus{Label: "startup", Source: repo, File: "file0000.go", Symbol: "file0000.go::StartupBaseMarker", Query: "Startup", ProposedSignature: "func StartupBaseMarker() int"}
	if _, err := p.git(ctx, repo, "init", "--quiet", "--initial-branch=main"); err != nil {
		return fixture, err
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.invalid/startup\n\ngo 1.23\n"), 0o600); err != nil {
		return fixture, err
	}
	for version := range 4 {
		count := files
		if version > 0 {
			count = changes
		}
		for file := range count {
			var source strings.Builder
			source.WriteString("package startup\n\n")
			if file == 0 {
				argument := ""
				if version > 0 {
					// A declaration change with many callers exercises reverse
					// closure fan-out, rather than only changing a leaf body.
					argument = "values ...int"
				}
				fmt.Fprintf(&source, "func StartupPivot(%s) int { return %d }\n", argument, version)
				marker := []string{"StartupBaseMarker", "StartupInitialMarker", "StartupFinalMarker", "StartupSeedMarker"}[version]
				fmt.Fprintf(&source, "func %s() int { return StartupPivot() }\n", marker)
			}
			for function := range functions {
				fmt.Fprintf(&source, "func BenchBody%04d_%03d() int {\nreturn %d", file, function, version)
				for range calls {
					source.WriteString(" +\nStartupPivot()")
				}
				source.WriteString("\n}\n")
			}
			if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("file%04d.go", file)), []byte(source.String()), 0o600); err != nil {
				return fixture, err
			}
		}
		if _, err := p.git(ctx, repo, "add", "--all"); err != nil {
			return fixture, err
		}
		if _, err := p.git(ctx, repo, "commit", "--quiet", "--message", fmt.Sprintf("startup fixture %d", version)); err != nil {
			return fixture, err
		}
		sha, err := p.git(ctx, repo, "rev-parse", "HEAD")
		if err != nil {
			return fixture, err
		}
		commit := strings.TrimSpace(string(sha))
		switch version {
		case 0:
			fixture.Corpus.Commit = commit
		case 1:
			fixture.Initial = commit
			if _, err := p.git(ctx, repo, "branch", "startup-initial"); err != nil {
				return fixture, err
			}
		case 2:
			fixture.Final = commit
			if _, err := p.git(ctx, repo, "branch", "startup-final"); err != nil {
				return fixture, err
			}
		case 3:
			fixture.Seed = commit
		}
	}
	_, err := p.git(ctx, repo, "checkout", "--quiet", "--detach", fixture.Corpus.Commit)
	return fixture, err
}
