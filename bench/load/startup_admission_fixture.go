package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// startupAdmissionFixture builds one synthetic Go repository whose committed
// tree carries a unique searchable marker. Family A is small so its committed
// base publishes quickly; family B is large so its base publication keeps the
// startup join (and the global view-build gate) closed well past A's t0. The
// marker name is unique per family so an exact worktree search response can be
// attributed to the right checkout.
type startupAdmissionFixture struct {
	Corpus Corpus
	Marker string
}

func buildStartupAdmissionFixture(ctx context.Context, sourceRoot, label string, files, functions, calls int) (startupAdmissionFixture, error) {
	if files < 1 || functions < 1 || calls < 0 {
		return startupAdmissionFixture{}, fmt.Errorf("fixture %s: files/functions must be >=1 and calls >=0", label)
	}
	dir := filepath.Join(sourceRoot, label)
	// A re-run into an existing trial dir must start from a clean corpus: the fixture
	// is deterministic, so a stale committed tree would leave `git commit` with
	// nothing to commit and fail. The harness clones this dir, so removing a prior
	// fixture is safe.
	if err := os.RemoveAll(dir); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return startupAdmissionFixture{}, err
	}
	marker := "AdmissionMarker" + strings.ToUpper(label[:1]) + label[1:]
	if err := fixtureGit(ctx, dir, "init", "-q"); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := fixtureGit(ctx, dir, "config", "user.email", "fixture@example.invalid"); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := fixtureGit(ctx, dir, "config", "user.name", "fixture"); err != nil {
		return startupAdmissionFixture{}, err
	}
	for i := 0; i < files; i++ {
		var b strings.Builder
		b.WriteString("package fixture\n\n")
		if i == 0 {
			fmt.Fprintf(&b, "func %s() int { return %d }\n\n", marker, i)
		}
		for f := 0; f < functions; f++ {
			fmt.Fprintf(&b, "func F%d_%d() int {\n", i, f)
			if calls == 0 {
				fmt.Fprintf(&b, "\treturn %d\n", f)
			} else {
				b.WriteString("\tx := 0\n")
				for c := 0; c < calls; c++ {
					fmt.Fprintf(&b, "\tx += G%d_%d_%d()\n", i, f, c)
				}
				b.WriteString("\treturn x\n")
			}
			b.WriteString("}\n\n")
			if calls > 0 {
				for c := 0; c < calls; c++ {
					fmt.Fprintf(&b, "func G%d_%d_%d() int { return %d }\n\n", i, f, c, c)
				}
			}
		}
		name := fmt.Sprintf("file_%d.go", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
			return startupAdmissionFixture{}, err
		}
	}
	if err := fixtureGit(ctx, dir, "add", "-A"); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := fixtureGit(ctx, dir, "commit", "-q", "-m", "fixture"); err != nil {
		return startupAdmissionFixture{}, err
	}
	// Pack objects so the harness's --no-hardlinks clone copies a packfile
	// rather than thousands of loose objects (which races on large fixtures).
	if err := fixtureGit(ctx, dir, "gc", "-q"); err != nil {
		return startupAdmissionFixture{}, err
	}
	sha, err := fixtureGitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return startupAdmissionFixture{}, err
	}
	return startupAdmissionFixture{
		Marker: marker,
		Corpus: Corpus{
			Label:             label,
			Source:            dir,
			Commit:            strings.TrimSpace(sha),
			File:              "file_0.go",
			Symbol:            "file_0.go::" + marker,
			Query:             marker,
			ProposedSignature: "func " + marker + "() int",
		},
	}, nil
}

func fixtureGit(ctx context.Context, dir string, args ...string) error {
	_, err := fixtureGitOutput(ctx, dir, args...)
	return err
}

func fixtureGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s in %s: %w; stderr: %s", args[0], dir, err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s in %s: %w", args[0], dir, err)
	}
	return string(out), nil
}
