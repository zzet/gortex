package indexer

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractPublishedCohortAbsentDiffersFromInvalidRetainedProof(t *testing.T) {
	actor := NewContractActor("", []string{"repo"})
	c := &ContractAnalysisCoordinator{actors: map[ContractActor]*contractActor{}}
	_, _, err := c.AcquireLastPublishedCohort(t.Context(), actor)
	if !errors.Is(err, ErrContractPublishedCohortAbsent) || !errors.Is(err, graph.ErrContractProjectionIncomplete) {
		t.Fatalf("missing metadata boundary lost incomplete compatibility: %v", err)
	}
	c.actors[actor] = &contractActor{lastCohort: &ContractPublishedCohort{}}
	_, _, err = c.AcquireLastPublishedCohort(t.Context(), actor)
	if errors.Is(err, ErrContractPublishedCohortAbsent) || !errors.Is(err, graph.ErrContractProjectionIncomplete) {
		t.Fatalf("invalid retained proof must not trigger discovery: %v", err)
	}
}
