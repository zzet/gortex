package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Ordinary repo provenance records extractor versions, not the builder's
// installed-producer marker. A current complete primary census can certify
// that separate policy: the copy carries its actual receipts and actor.
// Existing ready generations still require the exact full policy identity.
func (b *SparseGenerationBuilder) claimedContractCopyPolicy(ctx context.Context, state graph.RepoIndexState, desired string) bool {
	if b.contractCoreRuntime == nil || desired != b.extractorVersionsFingerprint() || state.ExtractorVersions != extractorVersionsFingerprint() {
		return false
	}
	return claimedContractCopyBaseline(ctx, b.Store.AtGeneration(generationZero), state.RepoPrefix) == nil
}

func claimedContractCopyBaseline(ctx context.Context, handle *store_sqlite.Store, repo string) error {
	state, found, err := handle.ContractInputStateContext(ctx, repo, "")
	if err != nil {
		return err
	}
	if !found || !state.Accepted || state.InputVersion != contractCoreInputVersion || state.InputFingerprint == "" {
		return fmt.Errorf("indexer: copied primary contract input is not accepted")
	}
	baseline, err := handle.ContractBoundaryReceiptBaselineContext(ctx, repo, "")
	if err != nil {
		return err
	}
	if baseline == nil || baseline.Version != contractBoundaryReceiptVersion || baseline.Fingerprint == "" {
		return fmt.Errorf("indexer: copied primary contract census is not certified")
	}
	return nil
}
