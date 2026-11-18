package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestExternalAgentHintKeepsUnregisteredConfiguredFamilyPending(t *testing.T) {
	primary, linked := filepath.Join(t.TempDir(), "primary"), filepath.Join(t.TempDir(), "linked")
	if err := os.Mkdir(primary, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionGuidanceGit(t, primary, "init", "-q", "-b", "main")
	sessionGuidanceGit(t, primary, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "fixture")
	sessionGuidanceGit(t, primary, "worktree", "add", "-q", "-b", "feature", linked)
	oldStatus, oldRoots := sessionStartStatusFn, hookConfiguredRepoRoots
	t.Cleanup(func() { sessionStartStatusFn, hookConfiguredRepoRoots = oldStatus, oldRoots })
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) { return &daemon.StatusResponse{Ready: true}, nil }
	hookConfiguredRepoRoots = func() []string { return []string{primary} }
	got := buildStaleIndexHint(linked)
	if !strings.Contains(got, "checkout discovery is pending") || !strings.Contains(got, "view_building") || strings.Contains(got, "gortex track "+linked) {
		t.Fatalf("configured family was told to retrack: %s", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "no reconnect is needed", ""), "reconnect") {
		t.Fatalf("external hook gave a reconnect instruction: %s", got)
	}
	outside := t.TempDir()
	if got := buildStaleIndexHint(outside); !strings.Contains(got, "gortex track "+outside) {
		t.Fatalf("unrelated directory lost explicit track guidance: %s", got)
	}
}

func TestDirectUnloadedFamilyPrimaryHooksKeepDiscoveryGuidance(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	if err := os.Mkdir(primary, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionGuidanceGit(t, primary, "init", "-q", "-b", "main")
	oldStatus, oldRoots := sessionStartStatusFn, hookConfiguredRepoRoots
	t.Cleanup(func() { sessionStartStatusFn, hookConfiguredRepoRoots = oldStatus, oldRoots })
	hookConfiguredRepoRoots = func() []string { return []string{primary} }
	status := &daemon.StatusResponse{Ready: true, TrackedRepos: []daemon.TrackedRepoStatus{{Path: primary, Name: "primary", Unloaded: true}}}
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) { return status, nil }
	for name, text := range map[string]string{
		"session-start":      renderCwdCoverage(primary, status),
		"lean-session-start": renderLeanReadiness(primary, status),
		"external-agent":     buildStaleIndexHint(primary),
	} {
		if !strings.Contains(text, "view_building") || !strings.Contains(text, "discovery is pending") || strings.Contains(text, "gortex track "+primary) || strings.Contains(strings.ReplaceAll(text, "no reconnect is needed", ""), "reconnect") {
			t.Errorf("%s mislabeled direct configured/unloaded family: %s", name, text)
		}
	}
}

func TestSessionStartProbePreservesLoadingAndTerminalRegistrationState(t *testing.T) {
	for _, state := range []string{daemon.RepoStateLoading, daemon.RepoStateFailed, daemon.RepoStateMissing, daemon.RepoStatePrefixCollision, daemon.RepoStateReady, ""} {
		status := sessionStartStatusFromProbe(daemon.ProbeResponse{TrackedRepos: []daemon.ProbeRepo{{Path: "/fixture", State: state}}})
		row := status.TrackedRepos[0]
		if row.Loading != (state == daemon.RepoStateLoading) || row.Unloaded != (state == daemon.RepoStateLoading || state == daemon.RepoStateFailed || state == daemon.RepoStateMissing || state == daemon.RepoStatePrefixCollision) || row.Missing != (state == daemon.RepoStateMissing) {
			t.Errorf("probe state %q lost terminal/loading distinction: %+v", state, row)
		}
		covered, _ := classifyCwd("/fixture", status.TrackedRepos)
		if (covered == nil) != (row.Unloaded && !row.Loading) {
			t.Errorf("probe state %q changed healthy/active-loading coverage or admitted terminal unloaded row", state)
		}
	}
}

func TestPrefixCollisionHooksPreserveOwnerGuidance(t *testing.T) {
	root, owner := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	status := sessionStartStatusFromProbe(daemon.ProbeResponse{TrackedRepos: []daemon.ProbeRepo{{
		Path: root, Prefix: "src", State: daemon.RepoStatePrefixCollision,
		ErrorCode: daemon.ErrPrefixCollision, OwnerPath: owner, OwnerState: "present",
	}}})
	oldStatus := sessionStartStatusFn
	t.Cleanup(func() { sessionStartStatusFn = oldStatus })
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) { return status, nil }
	for name, text := range map[string]string{
		"session-start":      renderCwdCoverage(root, status),
		"lean-session-start": renderLeanReadiness(root, status),
		"external-agent":     buildStaleIndexHint(root),
	} {
		if !strings.Contains(text, "prefix_collision") || !strings.Contains(text, owner) ||
			strings.Contains(text, "untrack") || strings.Contains(text, "discovery is pending") || strings.Contains(text, "enforcement active") {
			t.Errorf("%s lost collision owner/remedy: %s", name, text)
		}
	}
}
