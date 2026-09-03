package indexer

import (
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// evictRepoCurrentGeneration is the replacement path. An older backend that
// does not expose generations has only one logical generation, so its ordinary
// Store eviction is the compatible fallback.
func evictRepoCurrentGeneration(target graph.Store, repoPrefix string) (nodesRemoved, edgesRemoved int) {
	if scoped, ok := target.(graph.CurrentGenerationRepoEvicter); ok {
		return scoped.EvictRepoCurrentGeneration(repoPrefix)
	}
	return target.EvictRepo(repoPrefix)
}

// evictRepoAllGenerations is reserved for authoritative repository removal.
// It fails closed unless the backend declares the destructive capability: the
// ordinary Store eviction is generation-scoped and must never masquerade as a
// complete purge through a capability-hiding wrapper.
func evictRepoAllGenerations(target graph.Store, repoPrefix string) (nodesRemoved, edgesRemoved int, err error) {
	if repoPrefix == "" {
		return 0, 0, fmt.Errorf("all-generation repository eviction refuses an empty repo prefix")
	}
	if checked, ok := target.(graph.CheckedAllGenerationsRepoEvicter); ok {
		return checked.EvictRepoAllGenerationsChecked(repoPrefix)
	}
	destructive, ok := target.(graph.AllGenerationsRepoEvicter)
	if !ok {
		return 0, 0, fmt.Errorf("store %T does not expose all-generation repository eviction", target)
	}
	nodesRemoved, edgesRemoved = destructive.EvictRepoAllGenerations(repoPrefix)
	return nodesRemoved, edgesRemoved, nil
}
