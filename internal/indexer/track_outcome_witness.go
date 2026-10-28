package indexer

import "github.com/zzet/gortex/internal/config"

// TrackOutcomeWitness carries only memory identity for a completed outcome.
// A cache cannot reuse it across owner replacement or configuration revision.
type TrackOutcomeWitness struct {
	prefix string
	owner  *RepoMetadata
	root   string
	config *config.RepoTrackSnapshot
	loads  *repoLoadSnapshot
	valid  bool
}

func (mi *MultiIndexer) CaptureTrackOutcome(prefix, collisionOwner string) TrackOutcomeWitness {
	mi.mu.RLock()
	owner := mi.repos[prefix]
	witness := TrackOutcomeWitness{prefix: prefix, owner: owner, valid: true}
	witness.loads = mi.repoLoads.snap.Load()
	if owner != nil {
		witness.root = owner.RootPath
	}
	mi.mu.RUnlock()
	if collisionOwner != "" && owner != nil && witness.root != collisionOwner {
		witness.valid = false
	}
	if mi.configMgr != nil {
		witness.config = mi.configMgr.RepoTrackSnapshot()
	}
	return witness
}

func (mi *MultiIndexer) TrackOutcomeCurrent(witness TrackOutcomeWitness) bool {
	if !witness.valid {
		return false
	}
	if mi.repoLoads.snap.Load() != witness.loads {
		return false // completed alias discovery makes a pending retry actionable
	}
	if mi.configMgr != nil && mi.configMgr.RepoTrackSnapshot() != witness.config {
		return false
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	owner := mi.repos[witness.prefix]
	return owner == witness.owner && (owner == nil || owner.RootPath == witness.root)
}
