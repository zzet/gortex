package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/pathkey"
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

func TestConfigManagerRepoRegistrationsRetainsCanonicalAliasProvenance(t *testing.T) {
	manager, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(realRoot, 0o755))
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	globalConfigMu.Lock()
	global := manager.Global()
	global.Repos = []RepoEntry{{Path: aliasRoot, Name: "global-wins", Exclude: []string{"global/**"}}}
	global.Projects = map[string]ProjectConfig{
		"zeta":  {Repos: []RepoEntry{{Path: realRoot, Name: "zeta-loses"}}},
		"alpha": {Repos: []RepoEntry{{Path: filepath.Join(realRoot, "."), Name: "alpha-loses"}}},
	}
	globalConfigMu.Unlock()

	registrations := manager.RepoRegistrations()
	require.Len(t, registrations, 1, "aliases must schedule one physical corpus")
	registration := registrations[0]
	require.Equal(t, aliasRoot, registration.Entry.Path, "top-level entry keeps physical precedence")
	require.Equal(t, "global-wins", registration.Entry.Name)
	require.Equal(t, pathkey.CanonicalExistingRoot(realRoot), registration.CanonicalPath)
	require.Equal(t, []RepoEntrySource{
		{Kind: RepoEntrySourceGlobal, Locator: pathkey.CanonicalExistingRoot(realRoot)},
		{Kind: RepoEntrySourceProject, Locator: "project:alpha"},
		{Kind: RepoEntrySourceProject, Locator: "project:zeta"},
	}, registration.Sources, "each independent reference survives physical deduplication")

	registrations[0].Entry.Exclude[0] = "caller/**"
	registrations[0].CanonicalPath = "caller"
	registrations[0].Sources[0].Locator = "caller"
	fresh := manager.RepoRegistrations()
	require.Equal(t, "global/**", fresh[0].Entry.Exclude[0])
	require.Equal(t, pathkey.CanonicalExistingRoot(realRoot), fresh[0].CanonicalPath)
	require.Equal(t, pathkey.CanonicalExistingRoot(realRoot), fresh[0].Sources[0].Locator)
}

func TestConfigManagerRepoRegistrationsDeduplicatesRepeatedProjectSource(t *testing.T) {
	manager, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	root := t.TempDir()

	globalConfigMu.Lock()
	global := manager.Global()
	global.Projects = map[string]ProjectConfig{
		"same": {Repos: []RepoEntry{{Path: root}, {Path: filepath.Join(root, ".")}}},
	}
	globalConfigMu.Unlock()

	registrations := manager.RepoRegistrations()
	require.Len(t, registrations, 1)
	require.Equal(t, []RepoEntrySource{{Kind: RepoEntrySourceProject, Locator: "project:same"}}, registrations[0].Sources,
		"one project membership is one provenance source even when its path is repeated")
}

func BenchmarkConfigManagerRepoRegistrations(b *testing.B) {
	for _, physical := range []int{256, 1000} {
		b.Run(fmt.Sprintf("physical_%d_sources_%d", physical, physical*3), func(b *testing.B) {
			benchmarkConfigManagerRepoRegistrations(b, physical)
		})
	}
}

func benchmarkConfigManagerRepoRegistrations(b *testing.B, physical int) {
	manager, err := NewConfigManager(filepath.Join(b.TempDir(), "config.yaml"))
	if err != nil {
		b.Fatalf("NewConfigManager: %v", err)
	}
	base := b.TempDir()
	global := manager.Global()
	globalConfigMu.Lock()
	global.Repos = make([]RepoEntry, 0, physical)
	global.Projects = map[string]ProjectConfig{
		"alpha": {Repos: make([]RepoEntry, 0, physical)},
		"zeta":  {Repos: make([]RepoEntry, 0, physical)},
	}
	for i := 0; i < physical; i++ {
		name := fmt.Sprintf("repo-%04d", i)
		entry := RepoEntry{Path: filepath.Join(base, name), Name: name, Exclude: []string{"generated/**"}}
		global.Repos = append(global.Repos, entry)
		alpha := global.Projects["alpha"]
		alpha.Repos = append(alpha.Repos, RepoEntry{Path: entry.Path, Name: name})
		global.Projects["alpha"] = alpha
		zeta := global.Projects["zeta"]
		zeta.Repos = append(zeta.Repos, RepoEntry{Path: entry.Path, Name: name})
		global.Projects["zeta"] = zeta
	}
	globalConfigMu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registrations := manager.RepoRegistrations()
		if len(registrations) != physical {
			b.Fatalf("RepoRegistrations length = %d, want %d", len(registrations), physical)
		}
		if len(registrations[physical-1].Sources) != 3 {
			b.Fatalf("last registration sources = %d, want 3", len(registrations[physical-1].Sources))
		}
	}
}
