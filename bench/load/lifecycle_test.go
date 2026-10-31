package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitFixture(t *testing.T) Corpus {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		cmd.Dir = dir
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package sample\nfunc Example() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "sample.go")
	git("commit", "--quiet", "-m", "fixture")
	c := sampleCorpus()
	c.Source, c.Commit = dir, git("rev-parse", "HEAD")
	// A newer source commit must not silently replace the manifest pin.
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "new.go")
	git("commit", "--quiet", "-m", "later fixture")
	return c
}

func TestPrivateClonePinWorktreesAndMutationOwnership(t *testing.T) {
	c := gitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := newPrivateRun(ctx, t.TempDir(), "/unused/binary", []Corpus{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
	if _, err := os.Stat(filepath.Join(p.Repos[0].Path, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("clone ignored commit pin: %v", err)
	}
	if err := p.addWorktrees(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for _, repo := range p.Repos {
		if err := p.writeFile(repo, []byte("package sample\n// owned edit\n")); err != nil {
			t.Fatal(err)
		}
	}
	original, err := os.ReadFile(filepath.Join(c.Source, c.File))
	if err != nil || string(original) != "package sample\nfunc Example() {}\n" {
		t.Fatalf("manifest source was modified: %s %v", original, err)
	}
	if err := p.writeFile(privateRepo{Path: c.Source, Corpus: c}, []byte("forbidden")); err == nil {
		t.Fatal("manifest-root edit accepted")
	}
	if err := p.writeFile(privateRepo{Path: p.Root, Corpus: c}, []byte("forbidden")); err == nil {
		t.Fatal("non-clone edit accepted")
	}
	for _, root := range []string{c.Source, filepath.Join(c.Source, "nested", "new")} {
		if _, err := newPrivateRun(ctx, root, "/unused", []Corpus{c}); err == nil {
			t.Fatalf("scratch under source accepted: %s", root)
		}
	}
}

func TestPrivateOwnershipRejectsSymlinkEscapes(t *testing.T) {
	c := gitFixture(t)
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(c.Source, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := newPrivateRun(context.Background(), filepath.Join(alias, "new", "scratch"), "/unused", []Corpus{c}); err == nil {
		t.Fatal("symlinked scratch escaped source protection")
	}
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(c.Source, c.File), filepath.Join(root, "escape.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedFile(root, "escape.go"); err == nil {
		t.Fatal("source file symlink escaped clone ownership")
	}
}

func TestPrivateEnvironmentAndManifestValidation(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_SOCKET", "/other/session.sock")
	t.Setenv("GORTEX_LLM_PROVIDER", "external")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	env := privateEnv("/owned", "/owned/socket", "/owned/gitconfig")
	values := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["GORTEX_DAEMON_SOCKET"] != "/owned/socket" || values["GORTEX_LLM_PROVIDER"] != "" || values["GIT_CONFIG_COUNT"] != "" || values["HOME"] != filepath.Join("/owned", "home") {
		t.Fatalf("inherited daemon/git environment: %v", values)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	for _, data := range []string{`[]`, `[{}]`, `[{"unknown":true}]`, `[] []`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadManifest(path); err == nil {
			t.Fatalf("invalid manifest accepted: %s", data)
		}
	}
}

func TestCancelledClonePreparationDoesNotRetainState(t *testing.T) {
	corpus := gitFixture(t)
	scratch := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newPrivateRun(ctx, scratch, "unused", []Corpus{corpus}); err == nil {
		t.Fatal("cancelled preparation succeeded")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(scratch, entry.Name(), "repos")); !os.IsNotExist(err) {
			t.Fatalf("failed clone retained state: %v", err)
		}
	}
}
