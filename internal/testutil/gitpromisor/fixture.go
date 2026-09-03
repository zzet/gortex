// Package gitpromisor builds deterministic local partial-clone fixtures for tests.
package gitpromisor

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fixture owns a local bare origin and a complete no-checkout clone.
type Fixture struct {
	OriginDir      string
	CompleteDir    string
	BaseCommitOID  string
	CommitOID      string
	OtherCommitOID string
	EmptyTreeOID   string
	BaseTreeOID    string
	RootTreeOID    string
	NestedTreeOID  string
	BaseBlobOID    string
	RootBlobOID    string
	NestedBlobOID  string
}

// Client is an isolated filtered clone whose upload-pack invocations are counted.
type Client struct {
	Dir         string
	counterPath string
}

// New creates a two-file tree entirely through local Git plumbing.
func New(t testing.TB) *Fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local upload-pack counter fixture requires a POSIX wrapper")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("find git: %v", err)
	}
	if _, err := exec.LookPath("git-upload-pack"); err != nil {
		t.Fatalf("find git-upload-pack: %v", err)
	}

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	runGit(t, "", "init", "--bare", "--quiet", origin)

	baseBlob := runGit(t, "package renamed\n\nfunc Alpha() int { return 1 }\nfunc Shared() string { return \"same\" }\n", "--git-dir="+origin, "hash-object", "-w", "--stdin")
	nestedBlob := runGit(t, "package renamed\n\nfunc Alpha() int { return 2 }\nfunc Shared() string { return \"same\" }\n", "--git-dir="+origin, "hash-object", "-w", "--stdin")
	rootBlob := runGit(t, "package root\n", "--git-dir="+origin, "hash-object", "-w", "--stdin")
	emptyTree := runGit(t, "", "--git-dir="+origin, "mktree")
	baseTree := runGit(t,
		fmt.Sprintf("100644 blob %s\told.go\n", baseBlob),
		"--git-dir="+origin, "mktree",
	)
	nestedTree := runGit(t,
		fmt.Sprintf("100644 blob %s\tmissing.go\n", nestedBlob),
		"--git-dir="+origin, "mktree",
	)
	rootTree := runGit(t,
		fmt.Sprintf("100644 blob %s\troot.go\n040000 tree %s\tnested\n", rootBlob, nestedTree),
		"--git-dir="+origin, "mktree",
	)
	baseCommit := runGit(t, "base fixture\n",
		"-c", "user.name=Gortex Test",
		"-c", "user.email=gortex@example.invalid",
		"--git-dir="+origin, "commit-tree", baseTree,
	)
	commit := runGit(t, "fixture\n",
		"-c", "user.name=Gortex Test",
		"-c", "user.email=gortex@example.invalid",
		"--git-dir="+origin, "commit-tree", rootTree, "-p", baseCommit,
	)
	otherCommit := runGit(t, "unadvertised fixture\n",
		"-c", "user.name=Gortex Test",
		"-c", "user.email=gortex@example.invalid",
		"--git-dir="+origin, "commit-tree", rootTree,
	)
	runGit(t, "", "--git-dir="+origin, "update-ref", "refs/heads/main", commit)
	runGit(t, "", "--git-dir="+origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, "", "--git-dir="+origin, "config", "uploadpack.allowFilter", "true")
	runGit(t, "", "--git-dir="+origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	complete := filepath.Join(root, "complete")
	runGit(t, "", "clone", "--quiet", "--no-checkout", fileURL(origin), complete)
	return &Fixture{
		OriginDir:      origin,
		CompleteDir:    complete,
		BaseCommitOID:  baseCommit,
		CommitOID:      commit,
		OtherCommitOID: otherCommit,
		EmptyTreeOID:   emptyTree,
		BaseTreeOID:    baseTree,
		RootTreeOID:    rootTree,
		NestedTreeOID:  nestedTree,
		BaseBlobOID:    baseBlob,
		RootBlobOID:    rootBlob,
		NestedBlobOID:  nestedBlob,
	}
}

// Clone creates an isolated partial clone and installs a local upload-pack counter.
func (f *Fixture) Clone(t testing.TB, filter string) *Client {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "client")
	runGit(t, "", "clone", "--quiet", "--no-checkout", "--filter="+filter, fileURL(f.OriginDir), dir)

	uploadPack, err := exec.LookPath("git-upload-pack")
	if err != nil {
		t.Fatalf("find git-upload-pack: %v", err)
	}
	counter := filepath.Join(root, "upload-pack.requests")
	wrapper := filepath.Join(root, "counting-upload-pack")
	script := fmt.Sprintf("#!/bin/sh\nprintf '1\\n' >> %s\nexec %s \"$@\"\n", shellQuote(counter), shellQuote(uploadPack))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write upload-pack counter: %v", err)
	}
	runGit(t, "", "-C", dir, "config", "remote.origin.uploadpack", wrapper)
	return &Client{Dir: dir, counterPath: counter}
}

// WriteRef installs a loose ref without requiring the target object to exist.
func (c *Client) WriteRef(t testing.TB, ref, oid string) {
	t.Helper()
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		t.Fatalf("unsafe test ref %q", ref)
	}
	path := filepath.Join(c.Dir, ".git", filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create loose ref directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(oid+"\n"), 0o644); err != nil {
		t.Fatalf("write loose ref: %v", err)
	}
}

// ObjectPresent reports whether oid is available without allowing a lazy fetch.
func (c *Client) ObjectPresent(t testing.TB, oid string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", c.Dir, "cat-file", "-e", oid)
	cmd.Env = commandEnv(true)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false
		}
		t.Fatalf("probe local object %s: %v: %s", oid, err, strings.TrimSpace(stderr.String()))
	}
	return true
}

// FetchAndRequireRequest materializes oid with lazy fetching enabled and proves
// that the configured upload-pack counter observed the request.
func (c *Client) FetchAndRequireRequest(t testing.TB, oid string) {
	t.Helper()
	c.ResetRequests(t)
	runGit(t, "", "-C", c.Dir, "cat-file", "-p", oid)
	if got := c.RequestCount(t); got == 0 {
		t.Fatalf("positive control materialized %s without an observed upload-pack request", oid)
	}
	if !c.ObjectPresent(t, oid) {
		t.Fatalf("positive control did not materialize %s", oid)
	}
}

// RequestCount returns the number of upload-pack wrapper invocations.
func (c *Client) RequestCount(t testing.TB) int {
	t.Helper()
	data, err := os.ReadFile(c.counterPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read upload-pack counter: %v", err)
	}
	return bytes.Count(data, []byte{'\n'})
}

// ResetRequests zeros the upload-pack request counter.
func (c *Client) ResetRequests(t testing.TB) {
	t.Helper()
	if err := os.Remove(c.counterPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset upload-pack counter: %v", err)
	}
}

func runGit(t testing.TB, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = commandEnv(false)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String())
}

func commandEnv(noLazyFetch bool) []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+3)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (key == "GIT_NO_LAZY_FETCH" || key == "GIT_TERMINAL_PROMPT" || key == "GIT_OPTIONAL_LOCKS") {
			continue
		}
		env = append(env, entry)
	}
	if noLazyFetch {
		env = append(env, "GIT_NO_LAZY_FETCH=1")
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
