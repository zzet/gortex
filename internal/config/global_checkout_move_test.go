package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/pathkey"
)

func TestGlobalConfigRelocatesTopLevelAndProjectMemberships(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	aliasParent := filepath.Join(root, "alias")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	oldReal := filepath.Join(realParent, "old-worktree")
	oldAlias := filepath.Join(aliasParent, "old-worktree")
	current := filepath.Join(realParent, "renamed-worktree")
	if err := os.MkdirAll(oldReal, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(root, "config.yaml")
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: oldAlias, Ref: "refs/heads/feature", Workspace: "ws", Exclude: []string{"vendor/"}},
		},
		Projects: map[string]ProjectConfig{
			"alpha":        {Repos: []RepoEntry{{Path: oldAlias, Project: "alpha"}}},
			"unauthorized": {Repos: []RepoEntry{{Path: oldAlias, Name: "stable-prefix"}}},
			// Prefix equality is not ownership: an unrelated stale entry may
			// legitimately reuse the same explicit name.
			"other": {Repos: []RepoEntry{{Path: filepath.Join(root, "unrelated"), Name: "stable-prefix"}}},
		},
	}
	gc.SetConfigPath(configPath)
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldReal, current); err != nil {
		t.Fatal(err)
	}

	moved, err := gc.RelocateRepoAndSaveIfPresent(
		[]string{oldReal}, current, "stable-prefix",
		RepoRelocationSources{
			TopLevel: true,
			Projects: map[string]struct{}{"alpha": {}},
		},
	)
	if err != nil {
		t.Fatalf("RelocateRepoAndSaveIfPresent: %v", err)
	}
	if !moved {
		t.Fatal("move was not persisted")
	}
	if len(gc.Repos) != 1 {
		t.Fatalf("top-level entries = %d, want one deduplicated entry", len(gc.Repos))
	}
	entry := gc.Repos[0]
	if !pathkey.EqualPaths(entry.Path, canonicalConfiguredPath(current)) ||
		entry.Name != "stable-prefix" || entry.Ref != "refs/heads/feature" ||
		entry.Workspace != "ws" || len(entry.Exclude) != 1 || entry.Exclude[0] != "vendor/" {
		t.Fatalf("relocated top-level entry lost fields: %+v", entry)
	}
	project := gc.Projects["alpha"].Repos
	if len(project) != 1 ||
		!pathkey.EqualPaths(project[0].Path, canonicalConfiguredPath(current)) ||
		project[0].Name != "stable-prefix" ||
		project[0].Project != "alpha" {
		t.Fatalf("relocated project membership = %+v", project)
	}
	if got := gc.Projects["other"].Repos[0].Path; got != filepath.Join(root, "unrelated") {
		t.Fatalf("unrelated project path changed to %q", got)
	}
	if got := gc.Projects["unauthorized"].Repos[0].Path; got != oldAlias {
		t.Fatalf("unauthorized project membership changed to %q", got)
	}

	reloaded, err := LoadGlobal(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Repos) != 1 ||
		!pathkey.EqualPaths(reloaded.Repos[0].Path, canonicalConfiguredPath(current)) ||
		!pathkey.EqualPaths(
			reloaded.Projects["alpha"].Repos[0].Path, canonicalConfiguredPath(current),
		) {
		t.Fatalf("durable relocation = repos %+v projects %+v", reloaded.Repos, reloaded.Projects)
	}
}

func TestGlobalConfigRelocationRefusesDistinctOccupiedTarget(t *testing.T) {
	root := t.TempDir()
	oldRoot := filepath.Join(root, "old")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := &GlobalConfig{Repos: []RepoEntry{
		{Path: oldRoot, Name: "moving"},
		{Path: target, Name: "unrelated"},
	}}
	gc.SetConfigPath(filepath.Join(root, "config.yaml"))
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}

	moved, err := gc.RelocateRepoAndSaveIfPresent(
		[]string{oldRoot}, target, "moving",
		RepoRelocationSources{TopLevel: true},
	)
	if moved || !errors.Is(err, ErrRepoRelocationTargetOccupied) {
		t.Fatalf("occupied relocation = moved %t, err %v", moved, err)
	}
	if len(gc.Repos) != 2 || gc.Repos[0].Path != oldRoot || gc.Repos[1].Path != target {
		t.Fatalf("occupied relocation mutated entries: %+v", gc.Repos)
	}
}

func TestGlobalConfigBatchRelocatesCrossFamilyWorktreeSwapAtomically(t *testing.T) {
	root := t.TempDir()
	rootA := filepath.Join(root, "a")
	rootB := filepath.Join(root, "b")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := &GlobalConfig{Repos: []RepoEntry{
		{Path: rootA, Name: "one", Ref: "refs/heads/one"},
		{Path: rootB, Name: "two", Ref: "refs/heads/two"},
	}}
	gc.SetConfigPath(filepath.Join(root, "config.yaml"))
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}
	relocations := []RepoRelocation{
		{ID: "family-a/checkout", ConfigRoot: rootA, CurrentRoot: rootB, Prefix: "one", Sources: RepoRelocationSources{TopLevel: true}},
		{ID: "family-b/checkout", ConfigRoot: rootB, CurrentRoot: rootA, Prefix: "two", Sources: RepoRelocationSources{TopLevel: true}},
	}
	resolved, moved, err := gc.RelocateReposAndSaveIfPresent(relocations)
	if err != nil || !moved {
		t.Fatalf("swap relocation = moved %t, err %v", moved, err)
	}
	if !pathkey.EqualPaths(resolved["family-a/checkout"].Root, canonicalConfiguredPath(rootA)) ||
		!pathkey.EqualPaths(resolved["family-b/checkout"].Root, canonicalConfiguredPath(rootB)) {
		t.Fatalf("swap resolution = %+v", resolved)
	}
	if len(gc.Repos) != 2 || gc.Repos[0].Name != "one" || gc.Repos[0].Path != canonicalConfiguredPath(rootB) ||
		gc.Repos[1].Name != "two" || gc.Repos[1].Path != canonicalConfiguredPath(rootA) {
		t.Fatalf("swap candidate = %+v", gc.Repos)
	}
	reloaded, err := LoadGlobal(gc.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Repos) != 2 || reloaded.Repos[0].Path != canonicalConfiguredPath(rootB) ||
		reloaded.Repos[1].Path != canonicalConfiguredPath(rootA) {
		t.Fatalf("durable swap = %+v", reloaded.Repos)
	}
}

func TestGlobalConfigCurrentTargetIsNotAcceptedAsMoveOwnership(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: target, Name: "stable"}}}
	gc.SetConfigPath(filepath.Join(root, "config.yaml"))
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}

	relocation := RepoRelocation{
		ID: "checkout", ConfigRoot: filepath.Join(root, "gone"),
		CurrentRoot: target, Prefix: "stable",
		Sources: RepoRelocationSources{TopLevel: true},
	}
	if _, err := gc.ResolveRepoRelocations([]RepoRelocation{relocation}); !errors.Is(err, ErrRepoRelocationSourceMissing) {
		t.Fatalf("current-only resolution = %v, want source missing", err)
	}

	// Neither the configured name nor a blank name proves ownership. Recovery
	// of a post-save/pre-ack move is authorized only by the journaled exact
	// before/after file hashes, outside the path-selection planner.
	gc.Repos[0].Name = ""
	if _, err := gc.ResolveRepoRelocations([]RepoRelocation{relocation}); !errors.Is(err, ErrRepoRelocationSourceMissing) {
		t.Fatalf("blank current-only resolution = %v, want source missing", err)
	}
}

func TestConfigManagerPreparedRelocationFingerprintsExactAtomicBytes(t *testing.T) {
	root := t.TempDir()
	oldRoot := filepath.Join(root, "old")
	newRoot := filepath.Join(root, "new")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: oldRoot, Name: "stable"}}}
	configPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(configPath)
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	cm, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := cm.PrepareRepoRelocationBatch([]RepoRelocation{{
		ID: "checkout", ConfigRoot: oldRoot, CurrentRoot: newRoot, Prefix: "stable",
		Sources: RepoRelocationSources{TopLevel: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if batch.BeforeHash() == "" || batch.AfterHash() == "" ||
		batch.BeforeHash() == batch.AfterHash() {
		t.Fatalf("prepared hashes = before %q after %q", batch.BeforeHash(), batch.AfterHash())
	}
	state, err := cm.PreparedRepoRelocationState(batch.BeforeHash(), batch.AfterHash())
	if err != nil || state != PreparedRepoRelocationBefore {
		t.Fatalf("before state = %q, err %v", state, err)
	}
	moved, err := cm.CommitRepoRelocationBatch(batch)
	if err != nil || !moved {
		t.Fatalf("commit = moved %t, err %v", moved, err)
	}
	state, err = cm.PreparedRepoRelocationState(batch.BeforeHash(), batch.AfterHash())
	if err != nil || state != PreparedRepoRelocationAfter {
		t.Fatalf("after state = %q, err %v", state, err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := configBytesHash(data); got != batch.AfterHash() {
		t.Fatalf("committed raw hash = %q, want %q", got, batch.AfterHash())
	}
}

func TestConfigManagerPreparedRelocationRefusesExternalDiskEdit(t *testing.T) {
	root := t.TempDir()
	oldRoot := filepath.Join(root, "old")
	newRoot := filepath.Join(root, "new")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: oldRoot, Name: "stable"}}}
	configPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(configPath)
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	cm, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := cm.PrepareRepoRelocationBatch([]RepoRelocation{{
		ID: "checkout", ConfigRoot: oldRoot, CurrentRoot: newRoot, Prefix: "stable",
		Sources: RepoRelocationSources{TopLevel: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("# concurrent manual edit\nrepos: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if moved, err := cm.CommitRepoRelocationBatch(batch); moved ||
		!errors.Is(err, ErrConfigRevisionChanged) {
		t.Fatalf("stale commit = moved %t, err %v", moved, err)
	}
	if state, err := cm.PreparedRepoRelocationState(batch.BeforeHash(), batch.AfterHash()); state != "" || !errors.Is(err, ErrConfigRevisionChanged) {
		t.Fatalf("third fingerprint state = %q, err %v", state, err)
	}
}

func TestPreparedSwapDisappearanceRemovesOnlyExactOwnerOnEitherSide(t *testing.T) {
	for _, tc := range []struct {
		name        string
		commitBatch bool
		removeRoot  func(a, b string) string
		peerRoot    func(a, b string) string
	}{
		{
			name: "before-save", removeRoot: func(a, _ string) string { return a },
			peerRoot: func(_, b string) string { return b },
		},
		{
			name: "after-save", commitBatch: true,
			removeRoot: func(_, b string) string { return b },
			peerRoot:   func(a, _ string) string { return a },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			rootA := filepath.Join(root, "a")
			rootB := filepath.Join(root, "b")
			if err := os.MkdirAll(rootA, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(rootB, 0o755); err != nil {
				t.Fatal(err)
			}
			gc := &GlobalConfig{Repos: []RepoEntry{
				{Path: rootA, Name: "one"}, {Path: rootB, Name: "two"},
			}}
			configPath := filepath.Join(root, "config.yaml")
			gc.SetConfigPath(configPath)
			if err := gc.Save(); err != nil {
				t.Fatal(err)
			}
			cm, err := NewConfigManager(configPath)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := cm.PrepareRepoRelocationBatch([]RepoRelocation{
				{ID: "one", ConfigRoot: rootA, CurrentRoot: rootB, Prefix: "one", Sources: RepoRelocationSources{TopLevel: true}},
				{ID: "two", ConfigRoot: rootB, CurrentRoot: rootA, Prefix: "two", Sources: RepoRelocationSources{TopLevel: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.commitBatch {
				if moved, err := cm.CommitRepoRelocationBatch(batch); err != nil || !moved {
					t.Fatalf("commit swap = moved %t, err %v", moved, err)
				}
			}
			removed, err := cm.RemoveRepoSourcesAndSaveIfPresent(
				tc.removeRoot(rootA, rootB), RepoRelocationSources{TopLevel: true},
			)
			if err != nil || !removed {
				t.Fatalf("remove disappeared owner = removed %t, err %v", removed, err)
			}
			entries := cm.Global().Repos
			if len(entries) != 1 || entries[0].Name != "two" ||
				!pathkey.EqualPaths(
					canonicalConfiguredPath(entries[0].Path),
					canonicalConfiguredPath(tc.peerRoot(rootA, rootB)),
				) {
				t.Fatalf("swap peer was removed or moved: %+v", entries)
			}
		})
	}
}

func TestGlobalConfigRelocationFailureKeepsLiveAndDurableOldPaths(t *testing.T) {
	root := t.TempDir()
	oldRoot := filepath.Join(root, "old")
	newRoot := filepath.Join(root, "new")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: oldRoot}}}
	configPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(configPath)
	if err := gc.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}

	originalWrite := globalConfigWriteFile
	globalConfigWriteFile = func(string, []byte, os.FileMode) error {
		return errors.New("injected atomic replacement failure")
	}
	t.Cleanup(func() { globalConfigWriteFile = originalWrite })
	moved, err := gc.RelocateRepoAndSaveIfPresent(
		[]string{oldRoot}, newRoot, "old",
		RepoRelocationSources{TopLevel: true},
	)
	if err == nil || moved {
		t.Fatalf("failed relocation = moved %t, err %v", moved, err)
	}
	if len(gc.Repos) != 1 || gc.Repos[0].Path != oldRoot || gc.Repos[0].Name != "" {
		t.Fatalf("failed write published live candidate: %+v", gc.Repos)
	}
	reloaded, err := LoadGlobal(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Repos) != 1 || reloaded.Repos[0].Path != oldRoot {
		t.Fatalf("failed write changed durable config: %+v", reloaded.Repos)
	}
}

func BenchmarkRelocateRepoEntries256(b *testing.B) {
	root := b.TempDir()
	current := filepath.Join(root, "repo-moved")
	if err := os.MkdirAll(current, 0o755); err != nil {
		b.Fatal(err)
	}
	entries := make([]RepoEntry, 256)
	for i := range entries {
		entries[i] = RepoEntry{
			Path: filepath.Join(root, "repo-"+benchmarkDecimal(i)),
			Name: "repo-" + benchmarkDecimal(i),
		}
	}
	previous := canonicalConfiguredPath(entries[128].Path)
	current = canonicalConfiguredPath(current)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		moved, changed := relocateRepoEntries(entries, []string{previous}, current, "repo-128")
		if !changed || len(moved) != len(entries) {
			b.Fatalf("relocate result = changed %t, len %d", changed, len(moved))
		}
	}
}

func BenchmarkPlanRepoRelocationBatch256With16Cycle(b *testing.B) {
	root := b.TempDir()
	entries := make([]RepoEntry, 256)
	for i := range entries {
		entries[i] = RepoEntry{
			Path: filepath.Join(root, "repo-"+benchmarkDecimal(i)),
			Name: "repo-" + benchmarkDecimal(i),
		}
	}
	gc := &GlobalConfig{Repos: entries}
	relocations := make([]RepoRelocation, 16)
	for i := range relocations {
		next := (i + 1) % len(relocations)
		relocations[i] = RepoRelocation{
			ID:          "checkout-" + benchmarkDecimal(i),
			ConfigRoot:  entries[i].Path,
			CurrentRoot: entries[next].Path,
			Prefix:      entries[i].Name,
			Sources:     RepoRelocationSources{TopLevel: true},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plan, err := planRepoRelocationBatch(gc, relocations)
		if err != nil || !plan.changed || len(plan.resolutions) != len(relocations) {
			b.Fatalf("batch plan = changed %t, resolutions %d, err %v",
				plan.changed, len(plan.resolutions), err)
		}
	}
}

func benchmarkDecimal(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
