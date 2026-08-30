package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/pathkey"
	"gopkg.in/yaml.v3"
)

// ErrConfigRevisionChanged means a relocation plan was made from an older
// manager snapshot. The caller must re-read durable prepared state and retry;
// applying the old ownership map could move a newly edited config entry.
var ErrConfigRevisionChanged = errors.New("config: repository relocation revision changed")

// RepoRelocation is one exact, journal-authorized configuration move. ConfigRoot
// is the last acknowledged YAML address and CurrentRoot is the new destination.
// Post-save/pre-ack recovery is decided by the prepared raw-file hash, never by
// accepting a target entry through its repo name or prefix.
type RepoRelocation struct {
	ID          string
	ConfigRoot  string
	CurrentRoot string
	Prefix      string
	Sources     RepoRelocationSources
}

// RepoRelocationResolution reports the exact address currently present in all
// authorized config collections. A single atomic YAML replacement means every
// collection must resolve to the same address; mixed state fails closed.
type RepoRelocationResolution struct {
	ID   string
	Root string
}

// PreparedRepoRelocationBatch is an immutable candidate plus the exact disk
// fingerprints bracketing its atomic replacement. The fields stay private so
// callers cannot alter the bytes after durable journal preparation.
type PreparedRepoRelocationBatch struct {
	managerRevision uint64
	global          *GlobalConfig
	candidate       GlobalConfig
	data            []byte
	beforeHash      string
	afterHash       string
	changed         bool
}

func (b *PreparedRepoRelocationBatch) BeforeHash() string {
	if b == nil {
		return ""
	}
	return b.beforeHash
}

func (b *PreparedRepoRelocationBatch) AfterHash() string {
	if b == nil {
		return ""
	}
	return b.afterHash
}

type PreparedRepoRelocationState string

const (
	PreparedRepoRelocationBefore PreparedRepoRelocationState = "before"
	PreparedRepoRelocationAfter  PreparedRepoRelocationState = "after"
)

type normalizedRepoRelocation struct {
	RepoRelocation
	configRoot  string
	currentRoot string
}

type repoRelocationBatchPlan struct {
	candidate   GlobalConfig
	resolutions map[string]RepoRelocationResolution
	changed     bool
}

// ResolveRepoRelocations verifies exact ownership without writing. The result
// is paired with ConfigManager.Revision by the manager wrapper so a later batch
// save can reject intervening Reload/manual lifecycle publication.
func (gc *GlobalConfig) ResolveRepoRelocations(
	relocations []RepoRelocation,
) (map[string]RepoRelocationResolution, error) {
	if gc == nil {
		return nil, nil
	}
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	plan, err := planRepoRelocationBatch(gc, relocations)
	if err != nil {
		return nil, err
	}
	return plan.resolutions, nil
}

// RelocateReposAndSaveIfPresent applies a family/report-level ownership map in
// one YAML replacement. Because all source indices are selected before any path
// is transformed, valid A<->B swaps/cycles converge without treating another
// moved checkout as an unrelated target occupant.
func (gc *GlobalConfig) RelocateReposAndSaveIfPresent(
	relocations []RepoRelocation,
) (map[string]RepoRelocationResolution, bool, error) {
	if gc == nil {
		return nil, false, nil
	}
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	plan, err := planRepoRelocationBatch(gc, relocations)
	if err != nil {
		return nil, false, err
	}
	if !plan.changed {
		return plan.resolutions, false, nil
	}
	if err := saveGlobalConfigLocked(&plan.candidate); err != nil {
		return nil, false, err
	}
	gc.Repos = plan.candidate.Repos
	gc.Projects = plan.candidate.Projects
	return plan.resolutions, true, nil
}

func planRepoRelocationBatch(
	gc *GlobalConfig,
	relocations []RepoRelocation,
) (repoRelocationBatchPlan, error) {
	plan := repoRelocationBatchPlan{
		candidate:   *gc,
		resolutions: make(map[string]RepoRelocationResolution, len(relocations)),
	}
	if len(relocations) == 0 {
		plan.candidate.Repos = cloneRepoEntries(gc.Repos)
		plan.candidate.Projects = cloneProjects(gc.Projects)
		return plan, nil
	}
	normalized := make([]normalizedRepoRelocation, 0, len(relocations))
	seenIDs := make(map[string]struct{}, len(relocations))
	for _, relocation := range relocations {
		if relocation.ID == "" || relocation.ConfigRoot == "" ||
			relocation.CurrentRoot == "" || relocation.Prefix == "" {
			return plan, fmt.Errorf("config: incomplete repository relocation %+v", relocation)
		}
		if _, duplicate := seenIDs[relocation.ID]; duplicate {
			return plan, fmt.Errorf("config: duplicate repository relocation %s", relocation.ID)
		}
		seenIDs[relocation.ID] = struct{}{}
		if !relocation.Sources.TopLevel && len(relocation.Sources.Projects) == 0 {
			return plan, fmt.Errorf("%w: relocation %s has no authorized collection",
				ErrRepoRelocationSourceMissing, relocation.ID)
		}
		n := normalizedRepoRelocation{
			RepoRelocation: relocation,
			configRoot:     canonicalConfiguredPath(relocation.ConfigRoot),
			currentRoot:    canonicalConfiguredPath(relocation.CurrentRoot),
		}
		normalized = append(normalized, n)
	}

	topLevel := selectRelocations(normalized, func(r normalizedRepoRelocation) bool {
		return r.Sources.TopLevel
	})
	var err error
	if len(topLevel) != 0 {
		plan.candidate.Repos, plan.changed, err = relocateRepoCollectionBatch(
			gc.Repos, topLevel, "top-level repository", plan.resolutions,
		)
		if err != nil {
			return plan, err
		}
	} else {
		plan.candidate.Repos = cloneRepoEntries(gc.Repos)
	}

	plan.candidate.Projects = make(map[string]ProjectConfig, len(gc.Projects))
	for name, project := range gc.Projects {
		members := selectRelocations(normalized, func(r normalizedRepoRelocation) bool {
			_, ok := r.Sources.Projects[name]
			return ok
		})
		if len(members) == 0 {
			project.Repos = cloneRepoEntries(project.Repos)
			plan.candidate.Projects[name] = project
			continue
		}
		var changed bool
		project.Repos, changed, err = relocateRepoCollectionBatch(
			project.Repos, members, "project "+name, plan.resolutions,
		)
		if err != nil {
			return plan, err
		}
		plan.changed = plan.changed || changed
		plan.candidate.Projects[name] = project
	}
	for _, relocation := range normalized {
		for name := range relocation.Sources.Projects {
			if _, exists := gc.Projects[name]; !exists {
				return plan, fmt.Errorf("%w: project %s", ErrRepoRelocationSourceMissing, name)
			}
		}
		if _, resolved := plan.resolutions[relocation.ID]; !resolved {
			return plan, fmt.Errorf("%w: relocation %s", ErrRepoRelocationSourceMissing, relocation.ID)
		}
	}
	return plan, nil
}

func cloneProjects(projects map[string]ProjectConfig) map[string]ProjectConfig {
	if projects == nil {
		return nil
	}
	out := make(map[string]ProjectConfig, len(projects))
	for name, project := range projects {
		project.Repos = cloneRepoEntries(project.Repos)
		out[name] = project
	}
	return out
}

func selectRelocations(
	all []normalizedRepoRelocation,
	include func(normalizedRepoRelocation) bool,
) []normalizedRepoRelocation {
	out := make([]normalizedRepoRelocation, 0, len(all))
	for _, relocation := range all {
		if include(relocation) {
			out = append(out, relocation)
		}
	}
	return out
}

func relocateRepoCollectionBatch(
	entries []RepoEntry,
	relocations []normalizedRepoRelocation,
	collection string,
	resolutions map[string]RepoRelocationResolution,
) ([]RepoEntry, bool, error) {
	// Canonicalization resolves filesystem aliases and may stat missing-path
	// ancestors. Do it once per configured entry: a naive relocation-by-entry
	// loop turns a modest batch of worktree moves into thousands of syscalls.
	entryRoots := make([]string, len(entries))
	for i, entry := range entries {
		entryRoots[i] = canonicalConfiguredPath(entry.Path)
	}
	owners := make(map[int]int)
	selected := make([][]int, len(relocations))
	selectedRoot := make([]string, len(relocations))
	for relocationIndex, relocation := range relocations {
		var configMatches []int
		for entryIndex, root := range entryRoots {
			if pathkey.EqualPaths(root, relocation.configRoot) {
				configMatches = append(configMatches, entryIndex)
			}
		}
		source := configMatches
		selectedRoot[relocationIndex] = relocation.configRoot
		if len(source) == 0 {
			return nil, false, fmt.Errorf("%w: %s relocation %s",
				ErrRepoRelocationSourceMissing, collection, relocation.ID)
		}
		for _, entryIndex := range source {
			if owner, exists := owners[entryIndex]; exists && owner != relocationIndex {
				return nil, false, fmt.Errorf(
					"%w: %s source entry belongs to both %s and %s",
					ErrRepoRelocationTargetOccupied, collection,
					relocations[owner].ID, relocation.ID,
				)
			}
			owners[entryIndex] = relocationIndex
		}
		selected[relocationIndex] = source
		resolution := RepoRelocationResolution{
			ID: relocation.ID, Root: selectedRoot[relocationIndex],
		}
		if prior, exists := resolutions[relocation.ID]; exists &&
			!pathkey.EqualPaths(prior.Root, resolution.Root) {
			return nil, false, fmt.Errorf(
				"%w: config collections disagree for relocation %s",
				ErrRepoRelocationSourceMissing, relocation.ID,
			)
		}
		resolutions[relocation.ID] = resolution
	}

	// A target occupied by another selected relocation is a valid swap/cycle.
	// Any unselected target entry belongs to someone outside the batch.
	for relocationIndex, relocation := range relocations {
		for previous := 0; previous < relocationIndex; previous++ {
			if pathkey.EqualPaths(relocations[previous].currentRoot, relocation.currentRoot) {
				return nil, false, fmt.Errorf("%w: %s relocations %s and %s share target",
					ErrRepoRelocationTargetOccupied, collection,
					relocations[previous].ID, relocation.ID)
			}
		}
		for entryIndex, root := range entryRoots {
			if !pathkey.EqualPaths(root, relocation.currentRoot) {
				continue
			}
			if _, owned := owners[entryIndex]; !owned {
				return nil, false, fmt.Errorf("%w: %s target for %s",
					ErrRepoRelocationTargetOccupied, collection, relocation.ID)
			}
		}
	}

	first := make(map[int]int, len(relocations))
	for relocationIndex, indices := range selected {
		first[indices[0]] = relocationIndex
	}
	out := make([]RepoEntry, 0, len(entries))
	changed := false
	for entryIndex, entry := range entries {
		owner, owned := owners[entryIndex]
		if !owned {
			entry.Exclude = append([]string(nil), entry.Exclude...)
			out = append(out, entry)
			continue
		}
		if firstOwner, keep := first[entryIndex]; !keep || firstOwner != owner {
			changed = true
			continue
		}
		relocation := relocations[owner]
		entry.Exclude = append([]string(nil), entry.Exclude...)
		canonicalTarget := relocation.currentRoot
		if entry.Path != canonicalTarget {
			entry.Path = canonicalTarget
			changed = true
		}
		if entry.Name == "" {
			entry.Name = relocation.Prefix
			changed = true
		}
		out = append(out, entry)
	}
	return out, changed, nil
}

// ResolveRepoRelocations returns an immutable ownership snapshot and its
// manager revision.
func (cm *ConfigManager) ResolveRepoRelocations(
	relocations []RepoRelocation,
) (uint64, map[string]RepoRelocationResolution, error) {
	if cm == nil {
		return 0, nil, nil
	}
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if cm.global == nil {
		return cm.revision.Load(), nil, nil
	}
	resolved, err := cm.global.ResolveRepoRelocations(relocations)
	return cm.revision.Load(), resolved, err
}

// RelocateReposAndSaveIfPresent applies only the ownership plan read at
// expectedRevision. The manager lock also serializes Reload publication.
func (cm *ConfigManager) RelocateReposAndSaveIfPresent(
	expectedRevision uint64,
	relocations []RepoRelocation,
) (map[string]RepoRelocationResolution, bool, error) {
	if cm == nil {
		return nil, false, nil
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.revision.Load() != expectedRevision {
		return nil, false, ErrConfigRevisionChanged
	}
	if cm.global == nil {
		return nil, false, nil
	}
	resolved, moved, err := cm.global.RelocateReposAndSaveIfPresent(relocations)
	if err != nil {
		return nil, false, err
	}
	if moved {
		cm.revision.Add(1)
	}
	return resolved, moved, nil
}

// PrepareRepoRelocationBatch builds the exact YAML candidate and fingerprints
// the current file without publishing either. Callers durably record both
// hashes with every participating checkout before Commit replaces the file.
func (cm *ConfigManager) PrepareRepoRelocationBatch(
	relocations []RepoRelocation,
) (*PreparedRepoRelocationBatch, error) {
	if cm == nil {
		return nil, nil
	}
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if cm.global == nil {
		return nil, nil
	}
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	plan, err := planRepoRelocationBatch(cm.global, relocations)
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(&plan.candidate)
	if err != nil {
		return nil, fmt.Errorf("marshaling relocated global config: %w", err)
	}
	beforeHash, err := globalConfigDiskHash(cm.global.ConfigPath())
	if err != nil {
		return nil, err
	}
	return &PreparedRepoRelocationBatch{
		managerRevision: cm.revision.Load(),
		global:          cm.global,
		candidate:       plan.candidate,
		data:            data,
		beforeHash:      beforeHash,
		afterHash:       configBytesHash(data),
		changed:         plan.changed,
	}, nil
}

// CommitRepoRelocationBatch replaces exactly the disk image whose fingerprint
// was prepared. A Reload or external edit makes the plan stale instead of being
// overwritten. The bool reports whether bytes changed.
func (cm *ConfigManager) CommitRepoRelocationBatch(
	batch *PreparedRepoRelocationBatch,
) (bool, error) {
	if cm == nil || batch == nil {
		return false, nil
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.global != batch.global || cm.revision.Load() != batch.managerRevision {
		return false, ErrConfigRevisionChanged
	}
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	currentHash, err := globalConfigDiskHash(cm.global.ConfigPath())
	if err != nil {
		return false, err
	}
	if currentHash != batch.beforeHash {
		return false, ErrConfigRevisionChanged
	}
	if !batch.changed {
		return false, nil
	}
	path := cm.global.ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("creating config directory %s: %w", filepath.Dir(path), err)
	}
	if err := globalConfigWriteFile(path, batch.data, 0o644); err != nil {
		return false, fmt.Errorf("writing global config to %s: %w", path, err)
	}
	cm.global.Repos = batch.candidate.Repos
	cm.global.Projects = batch.candidate.Projects
	cm.revision.Add(1)
	return true, nil
}

// PreparedRepoRelocationState identifies which side of a prepared atomic
// replacement is on disk. Any third hash is a manual/concurrent edit and fails
// closed; callers retain the durable move journal.
func (cm *ConfigManager) PreparedRepoRelocationState(
	beforeHash, afterHash string,
) (PreparedRepoRelocationState, error) {
	if cm == nil {
		return "", ErrConfigRevisionChanged
	}
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if cm.global == nil {
		return "", ErrConfigRevisionChanged
	}
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	currentHash, err := globalConfigDiskHash(cm.global.ConfigPath())
	if err != nil {
		return "", err
	}
	switch currentHash {
	case beforeHash:
		return PreparedRepoRelocationBefore, nil
	case afterHash:
		return PreparedRepoRelocationAfter, nil
	default:
		return "", fmt.Errorf("%w: prepared config fingerprint no longer matches disk",
			ErrConfigRevisionChanged)
	}
}

// RemoveRepoSourcesAndSaveIfPresent forgets one exact checkout address only
// from the config collections its durable intents authorize. Unlike the legacy
// path-wide remover, it cannot delete a swap peer that currently occupies a
// different journal-owned address in the same YAML file.
func (cm *ConfigManager) RemoveRepoSourcesAndSaveIfPresent(
	root string,
	sources RepoRelocationSources,
) (bool, error) {
	if cm == nil {
		return false, nil
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.global == nil {
		return false, nil
	}
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	canonicalRoot := canonicalConfiguredPath(root)
	remove := func(entries []RepoEntry) ([]RepoEntry, bool) {
		out := make([]RepoEntry, 0, len(entries))
		removed := false
		for _, entry := range entries {
			if pathkey.EqualPaths(canonicalConfiguredPath(entry.Path), canonicalRoot) {
				removed = true
				continue
			}
			entry.Exclude = append([]string(nil), entry.Exclude...)
			out = append(out, entry)
		}
		return out, removed
	}
	candidate := *cm.global
	changed := false
	missingCollections := 0
	authorizedCollections := 0
	if sources.TopLevel {
		authorizedCollections++
		candidate.Repos, changed = remove(cm.global.Repos)
		if !changed {
			missingCollections++
		}
	} else {
		candidate.Repos = cloneRepoEntries(cm.global.Repos)
	}
	candidate.Projects = make(map[string]ProjectConfig, len(cm.global.Projects))
	for name, project := range cm.global.Projects {
		if _, authorized := sources.Projects[name]; authorized {
			authorizedCollections++
			var removed bool
			project.Repos, removed = remove(project.Repos)
			changed = changed || removed
			if !removed {
				missingCollections++
			}
		} else {
			project.Repos = cloneRepoEntries(project.Repos)
		}
		candidate.Projects[name] = project
	}
	for name := range sources.Projects {
		if _, exists := cm.global.Projects[name]; !exists {
			return false, fmt.Errorf("%w: project %s", ErrRepoRelocationSourceMissing, name)
		}
	}
	if !sources.TopLevel && len(sources.Projects) == 0 {
		return false, fmt.Errorf("%w: no authorized config collection", ErrRepoRelocationSourceMissing)
	}
	if !changed {
		return false, nil
	}
	if missingCollections != 0 {
		return false, fmt.Errorf(
			"%w: repository appears in %d of %d authorized collections",
			ErrRepoRelocationSourceMissing,
			authorizedCollections-missingCollections, authorizedCollections,
		)
	}
	if err := saveGlobalConfigLocked(&candidate); err != nil {
		return false, err
	}
	cm.global.Repos = candidate.Repos
	cm.global.Projects = candidate.Projects
	cm.revision.Add(1)
	return true, nil
}

const missingGlobalConfigHash = "missing"

func globalConfigDiskHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return missingGlobalConfigHash, nil
		}
		return "", fmt.Errorf("reading global config %s: %w", path, err)
	}
	return configBytesHash(data), nil
}

func configBytesHash(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}
