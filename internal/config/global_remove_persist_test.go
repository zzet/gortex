package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveRepoAndSaveIfPresentRetainsIntentUntilAtomicWriteSucceeds(t *testing.T) {
	repoRoot := t.TempDir()
	otherRoot := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: repoRoot, Name: "retryable"},
			{Path: otherRoot, Name: "retained"},
		},
		Projects: map[string]ProjectConfig{
			"mixed": {Repos: []RepoEntry{
				{Path: repoRoot, Name: "project-retryable"},
				{Path: otherRoot, Name: "project-retained"},
			}},
			"target-only": {Repos: []RepoEntry{{Path: repoRoot, Name: "project-only"}}},
		},
	}
	cfg.SetConfigPath(configPath)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save initial config: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read initial config: %v", err)
	}

	writeErr := errors.New("injected atomic config write failure")
	originalWriter := globalConfigWriteFile
	globalConfigWriteFile = func(string, []byte, os.FileMode) error { return writeErr }
	removed, err := cfg.RemoveRepoAndSaveIfPresent(repoRoot)
	globalConfigWriteFile = originalWriter
	if !errors.Is(err, writeErr) {
		t.Fatalf("remove error = %v, want injected failure", err)
	}
	if removed {
		t.Fatal("failed durable removal reported success")
	}
	if len(cfg.Repos) != 2 || cfg.Repos[0].Path != repoRoot || cfg.Repos[1].Path != otherRoot {
		t.Fatalf("top-level intent changed after failed write: %+v", cfg.Repos)
	}
	if got := cfg.Projects["mixed"].Repos; len(got) != 2 || got[0].Path != repoRoot || got[1].Path != otherRoot {
		t.Fatalf("mixed project intent changed after failed write: %+v", got)
	}
	if got := cfg.Projects["target-only"].Repos; len(got) != 1 || got[0].Path != repoRoot {
		t.Fatalf("target-only project intent changed after failed write: %+v", got)
	}
	afterFailure, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after failure: %v", err)
	}
	if string(afterFailure) != string(before) {
		t.Fatalf("on-disk config changed after failed write\nbefore: %s\nafter: %s", before, afterFailure)
	}

	removed, err = cfg.RemoveRepoAndSaveIfPresent(repoRoot)
	if err != nil {
		t.Fatalf("retry durable removal: %v", err)
	}
	if !removed {
		t.Fatal("retry did not remove configured repository")
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Path != otherRoot {
		t.Fatalf("in-memory top-level removal changed unrelated intent: %+v", cfg.Repos)
	}
	if got := cfg.Projects["mixed"].Repos; len(got) != 1 || got[0].Path != otherRoot {
		t.Fatalf("in-memory project removal changed unrelated intent: %+v", got)
	}
	if got := cfg.Projects["target-only"].Repos; len(got) != 0 {
		t.Fatalf("in-memory project target remains after successful retry: %+v", got)
	}
	reloaded, err := LoadGlobal(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(reloaded.Repos) != 1 || reloaded.Repos[0].Path != otherRoot {
		t.Fatalf("on-disk top-level removal changed unrelated intent: %+v", reloaded.Repos)
	}
	if got := reloaded.Projects["mixed"].Repos; len(got) != 1 || got[0].Path != otherRoot {
		t.Fatalf("on-disk project removal changed unrelated intent: %+v", got)
	}
	if got := reloaded.Projects["target-only"].Repos; len(got) != 0 {
		t.Fatalf("on-disk project target remains after successful retry: %+v", got)
	}
}
