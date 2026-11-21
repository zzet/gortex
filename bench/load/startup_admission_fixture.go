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
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		return startupAdmissionFixture{}, err
	}
	dir := filepath.Join(sourceRoot, label)
	// Only ever build into a directory this call creates: the caller hands in a
	// fresh trial root, and an existing dir is refused rather than cleared.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return startupAdmissionFixture{}, err
	}
	// The same isolation the private run uses: no inherited GIT_* (GIT_DIR,
	// GIT_WORK_TREE, GIT_INDEX_FILE, ...) can point fixture git at another repo.
	env := privateEnv(sourceRoot, "", os.DevNull)
	marker := "AdmissionMarker" + strings.ToUpper(label[:1]) + label[1:]
	if err := fixtureGit(ctx, env, dir, "init", "-q"); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := fixtureGit(ctx, env, dir, "config", "user.email", "fixture@example.invalid"); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := fixtureGit(ctx, env, dir, "config", "user.name", "fixture"); err != nil {
		return startupAdmissionFixture{}, err
	}
	for i := 0; i < files; i++ {
		if err := ctx.Err(); err != nil {
			return startupAdmissionFixture{}, err
		}
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
	if err := fixtureGit(ctx, env, dir, "add", "-A"); err != nil {
		return startupAdmissionFixture{}, err
	}
	if err := fixtureGit(ctx, env, dir, "commit", "-q", "-m", "fixture"); err != nil {
		return startupAdmissionFixture{}, err
	}
	// Pack objects so the harness's --no-hardlinks clone copies a packfile
	// rather than thousands of loose objects (which races on large fixtures).
	if err := fixtureGit(ctx, env, dir, "gc", "-q"); err != nil {
		return startupAdmissionFixture{}, err
	}
	sha, err := fixtureGitOutput(ctx, env, dir, "rev-parse", "HEAD")
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

func fixtureGit(ctx context.Context, env []string, dir string, args ...string) error {
	_, err := fixtureGitOutput(ctx, env, dir, args...)
	return err
}

func fixtureGitOutput(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s in %s: %w; stderr: %s", args[0], dir, err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s in %s: %w", args[0], dir, err)
	}
	return string(out), nil
}
