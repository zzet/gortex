package pi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// globalEnv returns an env whose user-level Pi footprint has been
// installed, ready for RemoveGlobal to take back.
func globalEnv(t *testing.T) agents.Env {
	t.Helper()
	env := defaultPostureEnv(t)
	env.Mode = agents.ModeGlobal
	env.HookMode = "enrich"
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return env
}

func TestPiRemoveGlobalTakesBackSidecarAndLegacyExtension(t *testing.T) {
	env := globalEnv(t)
	legacy := writeLegacyExtension(t, filepath.Join(AgentDir(env.Home), "extensions"), legacyBody(t))
	sidecar := GlobalSidecarPath(env.Home)

	// Preview and removal must agree on the set.
	artifacts := GlobalArtifacts(env.Home)
	if len(artifacts) != 2 {
		t.Fatalf("GlobalArtifacts = %v, want the sidecar and the legacy extension", artifacts)
	}

	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for _, path := range []string{sidecar, legacy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, stat err = %v", path, err)
		}
	}
	if GlobalArtifacts(env.Home) != nil {
		t.Errorf("nothing should be left to report after a clean removal")
	}
	if dir := filepath.Join(AgentDir(env.Home), "extensions"); !isGone(dir) {
		t.Errorf("expected the emptied %s pruned", dir)
	}
}

// An override set after an install leaves the user-level copy under the
// default agent dir. Preview and removal both have to reach it.
func TestPiRemoveGlobalTakesBackLegacyExtensionUnderTheDefaultAgentDir(t *testing.T) {
	env := globalEnv(t)
	stale := writeLegacyExtension(t,
		filepath.Join(env.Home, ".pi", "agent", "extensions"), legacyBody(t))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(env.Home, "custom-agent-dir"))

	artifacts := GlobalArtifacts(env.Home)
	if len(artifacts) != 1 || artifacts[0] != stale {
		t.Fatalf("GlobalArtifacts = %v, want [%s]", artifacts, stale)
	}
	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if !isGone(stale) {
		t.Errorf("expected %s removed", stale)
	}
}

func isGone(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// The settings file is the user's: we only ever appended one string to
// it, and `pi remove npm:pi-gortex` is the command that takes that back.
func TestPiRemoveGlobalLeavesSettingsAlone(t *testing.T) {
	env := globalEnv(t)
	settings := filepath.Join(AgentDir(env.Home), "settings.json")

	if _, failures := New().RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	list := packagesOf(t, settings)
	if len(list) != 1 || list[0] != PackageSpec {
		t.Errorf("packages = %v, want the entry left in place", list)
	}
}

// A sidecar carrying keys this adapter never writes is somebody's own
// configuration, and the never-clobber posture keeps it.
func TestPiRemoveGlobalKeepsCustomisedSidecar(t *testing.T) {
	env := globalEnv(t)
	sidecar := GlobalSidecarPath(env.Home)
	agentstest.WriteJSON(t, sidecar, map[string]any{
		"hook_mode":  "enrich",
		"custom_key": "mine",
	})

	if artifacts := GlobalArtifacts(env.Home); len(artifacts) != 0 {
		t.Errorf("a customised sidecar must not be previewed for deletion, got %v", artifacts)
	}
	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("customised sidecar must survive: %v", err)
	}
}

func TestPiRemoveGlobalDryRun(t *testing.T) {
	env := globalEnv(t)
	sidecar := GlobalSidecarPath(env.Home)

	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{DryRun: true})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 reported under dry-run", removed)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("dry-run must not delete %s: %v", sidecar, err)
	}
}

func TestPiRemoveGlobalNeedsHome(t *testing.T) {
	_, failures := New().RemoveGlobal(agents.Env{}, agents.ApplyOpts{})
	if len(failures) != 1 {
		t.Fatalf("expected one failure for a missing home, got %v", failures)
	}
	if GlobalArtifacts("") != nil {
		t.Errorf("GlobalArtifacts must report nothing without a home")
	}
}
