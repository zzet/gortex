package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestConfigManagerRepoEntriesReturnsDefensiveSnapshot(t *testing.T) {
	manager, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := manager.Global().AddRepo(RepoEntry{
		Path: repoRoot, Name: "repo", Exclude: []string{"generated/**"},
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	first := manager.RepoEntries()
	if len(first) != 1 {
		t.Fatalf("RepoEntries length = %d, want 1", len(first))
	}
	first[0].Path = "mutated"
	first[0].Exclude[0] = "mutated/**"

	second := manager.RepoEntries()
	if len(second) != 1 || second[0].Path == "mutated" {
		t.Fatalf("RepoEntries returned aliased entry: %+v", second)
	}
	if len(second[0].Exclude) != 1 || second[0].Exclude[0] != "generated/**" {
		t.Fatalf("RepoEntries returned aliased exclusions: %+v", second[0].Exclude)
	}
}

func TestConfigManagerRepoEntriesConcurrentMutationSnapshot(t *testing.T) {
	manager, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	global := manager.Global()
	base := t.TempDir()

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			root := filepath.Join(base, fmt.Sprintf("repo-%03d", i))
			if err := global.AddRepo(RepoEntry{Path: root, Name: fmt.Sprintf("repo-%03d", i), Exclude: []string{"tmp/**"}}); err != nil {
				t.Errorf("AddRepo(%d): %v", i, err)
				return
			}
			if err := global.RemoveRepo(root); err != nil {
				t.Errorf("RemoveRepo(%d): %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			entries := manager.RepoEntries()
			for j := range entries {
				entries[j].Path = "caller-owned"
				if len(entries[j].Exclude) != 0 {
					entries[j].Exclude[0] = "caller-owned/**"
				}
			}
		}
	}()
	workers.Wait()
}

func TestConfigManagerRepoEntriesIncludesProjectsDeterministically(t *testing.T) {
	manager, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	base := t.TempDir()
	top := filepath.Join(base, "top")
	alpha := filepath.Join(base, "alpha")
	zeta := filepath.Join(base, "zeta")

	globalConfigMu.Lock()
	global := manager.Global()
	global.Repos = []RepoEntry{{Path: top, Name: "top", Exclude: []string{"top/**"}}}
	global.Projects = map[string]ProjectConfig{
		"z-project": {Repos: []RepoEntry{{Path: zeta, Name: "zeta", Exclude: []string{"zeta/**"}}}},
		"a-project": {Repos: []RepoEntry{
			{Path: alpha, Name: "alpha", Exclude: []string{"alpha/**"}},
			{Path: top, Name: "duplicate-must-not-win", Exclude: []string{"wrong/**"}},
		}},
	}
	globalConfigMu.Unlock()

	first := manager.RepoEntries()
	second := manager.RepoEntries()
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("RepoEntries lengths = %d/%d, want 3/3", len(first), len(second))
	}
	wantPaths := []string{top, alpha, zeta}
	for i, want := range wantPaths {
		if first[i].Path != want || second[i].Path != want {
			t.Fatalf("RepoEntries[%d] paths = %q/%q, want %q", i, first[i].Path, second[i].Path, want)
		}
	}
	if first[0].Name != "top" || first[0].Exclude[0] != "top/**" {
		t.Fatalf("top-level duplicate did not retain precedence: %+v", first[0])
	}
	first[1].Exclude[0] = "mutated/**"
	if got := manager.RepoEntries()[1].Exclude[0]; got != "alpha/**" {
		t.Fatalf("project exclusion alias leaked into config: %q", got)
	}
}

func BenchmarkConfigManagerRepoEntriesProjects(b *testing.B) {
	manager, err := NewConfigManager(filepath.Join(b.TempDir(), "config.yaml"))
	if err != nil {
		b.Fatalf("NewConfigManager: %v", err)
	}
	global := manager.Global()
	globalConfigMu.Lock()
	global.Projects = make(map[string]ProjectConfig, 64)
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("project-%03d", i)
		global.Projects[name] = ProjectConfig{Repos: []RepoEntry{{
			Path: filepath.Join(b.TempDir(), name), Name: name, Exclude: []string{"generated/**"},
		}}}
	}
	globalConfigMu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if entries := manager.RepoEntries(); len(entries) != 64 {
			b.Fatalf("RepoEntries length = %d, want 64", len(entries))
		}
	}
}
