package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// buildPromotedCorpus writes the captured HEAD tree into the immutable base
// generation that the dedicated route will publish. The temporary coordinator
// supplies the exact same index configuration as the live checkout coordinator,
// but remains unsignaled so it cannot race the promotion transaction.
func (l *CheckoutLifecycle) buildPromotedCorpus(
	ctx context.Context,
	graphID string,
	checkout store_sqlite.Checkout,
	prefix string,
) (*IndexResult, int64, checkoutSample, int, error) {
	if l.mi.GetMetadata(prefix) != nil {
		l.mi.UntrackRepo(prefix)
	}
	if _, err := l.mi.trackRepoSourceCtx(ctx, config.RepoEntry{Path: checkout.RootPath, Name: prefix}, nil); err != nil {
		return nil, 0, checkoutSample{}, 0, err
	}
	coordinator, err := l.buildCoordinatorWithPoll(ctx, graphID, checkout, -1)
	if err != nil {
		return nil, 0, checkoutSample{}, 0, err
	}
	if coordinator == nil {
		return nil, 0, checkoutSample{}, 0, fmt.Errorf("indexer: build dedicated coordinator for checkout %s", checkout.CheckoutID)
	}
	defer coordinator.Close()

	resampled := 0
	for attempt := 0; attempt < 2; attempt++ {
		before, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			return nil, 0, checkoutSample{}, resampled, err
		}
		content, err := source.NewGitTreeSource(ctx, checkout.RootPath, before.tree)
		if err != nil {
			return nil, 0, checkoutSample{}, resampled, err
		}
		if l.indexBarrier != nil {
			l.indexBarrier()
		}

		changes := make([]LayerPathChange, 0)
		walkErr := content.Walk(ctx, func(file source.FileMeta) error {
			changes = append(changes, LayerPathChange{Path: file.Path, Kind: LayerPathAdded})
			return nil
		})
		if walkErr != nil {
			_ = content.Close()
			return nil, 0, checkoutSample{}, resampled, walkErr
		}

		generationID, report, buildErr := coordinator.builder.Build(ctx, BuildRequest{
			Identity: GenerationIdentity{
				OwnerKind:            dedicatedBaseGenerationKind,
				GraphID:              graphID,
				LayerID:              graphID + ":base",
				CheckoutID:           checkout.CheckoutID,
				GenerationKind:       dedicatedBaseGenerationKind,
				LowerViewFingerprint: before.tree,
				TreeOID:              before.tree,
				ProvenanceCommitOID:  before.commit,
				ConfigHash:           coordinator.configHash,
				ExtractorVersions:    coordinator.extractors,
				ResolverVersion:      checkoutResolverVersion,
				CreatedAt:            l.now().Unix(),
			},
			Base:        graph.New(),
			Target:      content,
			Changes:     changes,
			RootPath:    coordinator.root,
			RepoPrefix:  coordinator.repoPrefix,
			WorkspaceID: coordinator.workspaceID,
			ProjectID:   coordinator.projectID,
		})
		closeErr := content.Close()
		if buildErr != nil {
			return nil, 0, checkoutSample{}, resampled, buildErr
		}
		if closeErr != nil {
			coordinator.abandonBuild(ctx, generationID, true)
			return nil, 0, checkoutSample{}, resampled, closeErr
		}

		after, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			coordinator.abandonBuild(ctx, generationID, true)
			return nil, 0, checkoutSample{}, resampled, err
		}
		if after.tree == before.tree && after.commit == before.commit {
			return &IndexResult{
				NodeCount:  report.NodeCount,
				EdgeCount:  report.EdgeCount,
				FileCount:  len(report.IndexedPaths),
				DurationMs: report.Duration.Milliseconds(),
				RepoPrefix: prefix,
			}, generationID, before, resampled, nil
		}

		resampled++
		coordinator.abandonBuild(ctx, generationID, true)
	}
	return nil, 0, checkoutSample{}, resampled, fmt.Errorf(
		"%w: %s moved under two full generation builds", ErrCheckoutMoved, checkout.RootPath,
	)
}
