package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture only builds into a directory it creates: an existing one is
// refused, never cleared, so a reused or user-supplied scratch path is safe.
func TestStartupAdmissionFixtureRefusesExistingDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := buildStartupAdmissionFixture(ctx, root, "alpha", 2, 1, 1); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if _, err := buildStartupAdmissionFixture(ctx, root, "alpha", 2, 1, 1); err == nil {
		t.Fatal("a second build into an existing fixture dir must be refused")
	}
}

// Inherited GIT_DIR / GIT_WORK_TREE must not redirect fixture git into another
// repository.
func TestStartupAdmissionFixtureIgnoresInheritedGitEnv(t *testing.T) {
	ctx := context.Background()
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init other: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	fixture, err := buildStartupAdmissionFixture(ctx, t.TempDir(), "alpha", 2, 1, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if fixture.Corpus.Commit == "" {
		t.Fatal("fixture produced no commit")
	}
	out, _ := exec.Command("git", "--git-dir", filepath.Join(other, ".git"), "rev-list", "--all").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("fixture git wrote into the inherited GIT_DIR: %s", out)
	}
}

// A first exact answer is ready only while the unrelated family still holds
// startup, and only with a witness of that hold.
func TestStartupAdmissionVerdictRequiresObservedHold(t *testing.T) {
	o := startupAdmissionOptions{Budget: 10 * time.Second, HoldMin: 60 * time.Second}
	boot := time.Unix(1000, 0)
	exact := boot.Add(time.Second)
	settled := 1.0
	verdict := func(bBase, gate time.Time) string {
		trial := startupAdmissionTrial{BootStart: boot, SettledExactMS: &settled}
		finalizeStartupAdmissionTrial(&trial, o, boot, bBase, exact, gate)
		return trial.Outcome
	}
	if got := verdict(boot.Add(90*time.Second), boot.Add(91*time.Second)); got != "ready" {
		t.Fatalf("held trial: %s", got)
	}
	if got := verdict(time.Time{}, boot.Add(91*time.Second)); got != "no_hold_witness" {
		t.Fatalf("missing B witness: %s", got)
	}
	if got := verdict(boot.Add(90*time.Second), boot); got != "not_during_hold" {
		t.Fatalf("exact after gate open: %s", got)
	}
}

// The exact predicate binds the measured checkout's identity.
func TestStartupAdmissionExactBindsCheckout(t *testing.T) {
	raw := []byte(`{"view":{"exact":true,"kind":"worktree","repo_prefix":"alpha","checkout_id":"ck-1"},"hits":[{"name":"M"}]}`)
	if !startupAdmissionExact(raw, "alpha", "ck-1", "M") {
		t.Fatal("the measured checkout's exact answer was rejected")
	}
	if startupAdmissionExact(raw, "alpha", "ck-2", "M") {
		t.Fatal("another checkout's exact answer was accepted")
	}
}
