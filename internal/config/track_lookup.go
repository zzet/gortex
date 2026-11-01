package config

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/pathkey"
)

// RepoTrackSnapshot indexes admission candidates without canonicalizing every
// configured root. Identity remains the caller's responsibility: these lexical
// buckets are a filter, never evidence that two directories are the same.
type RepoTrackSnapshot struct {
	byPrefix map[string][]RepoEntry
	byBase   map[string][]RepoEntry
}

func (s *RepoTrackSnapshot) PrefixCandidates(prefix string) []RepoEntry {
	return copyTrackEntries(s.byPrefix[prefix])
}

func (s *RepoTrackSnapshot) RootCandidates(root string) []RepoEntry {
	return copyTrackEntries(s.byBase[pathkey.IdentityKey(filepath.Base(root))])
}

func copyTrackEntries(entries []RepoEntry) []RepoEntry {
	out := append([]RepoEntry(nil), entries...)
	for i := range out {
		out[i].Exclude = append([]string(nil), out[i].Exclude...)
	}
	return out
}

// RepoTrackSnapshot is revision-cached separately from the startup path
// snapshot. A track of one new root must not stat all configured roots first.
func (cm *ConfigManager) RepoTrackSnapshot() *RepoTrackSnapshot {
	gc := cm.Global()
	cm.lookupMu.Lock()
	defer cm.lookupMu.Unlock()
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()
	if cm.trackGlobal == gc && cm.trackRevision == gc.repoRevision && cm.trackSnapshot != nil {
		return cm.trackSnapshot
	}
	s := &RepoTrackSnapshot{byPrefix: make(map[string][]RepoEntry), byBase: make(map[string][]RepoEntry)}
	// Prefix ownership follows the existing top-level admission contract.
	// Project-only entries still supply overrides through the root lookup.
	for _, entry := range gc.Repos {
		entry.Exclude = append([]string(nil), entry.Exclude...)
		prefix := ResolvePrefix(entry)
		s.byPrefix[prefix] = append(s.byPrefix[prefix], entry)
	}
	for _, entry := range gc.repoEntriesLocked() {
		entry.Exclude = append([]string(nil), entry.Exclude...)
		base := pathkey.IdentityKey(filepath.Base(entry.Path))
		s.byBase[base] = append(s.byBase[base], entry)
	}
	cm.trackGlobal, cm.trackRevision, cm.trackSnapshot = gc, gc.repoRevision, s
	return s
}

func (s *RepoTrackSnapshot) InstanceCandidates(prefix string) []RepoEntry {
	out := s.PrefixCandidates(prefix)
	// Basenames themselves may contain @. Each separator is a possible
	// instancing boundary; look up only those finite lexical keys.
	for offset := 0; offset < len(prefix); {
		index := strings.IndexByte(prefix[offset:], '@')
		if index < 0 {
			break
		}
		index += offset
		base := prefix[:index]
		for _, entry := range copyTrackEntries(s.byPrefix[base]) {
			if entry.Name == "" {
				out = append(out, entry)
			}
		}
		offset = index + 1
	}
	return out
}
