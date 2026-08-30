package indexer

import (
	"context"
	"fmt"
	"io"
	"io/fs"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// promotionShellSource gives TrackRepoCtx an authoritative empty snapshot. It
// creates the configuration/indexer shell required by the generation builder
// without walking, opening, or parsing a checkout file.
type promotionShellSource struct{}

func (promotionShellSource) Open(string) (io.ReadCloser, source.FileMeta, error) {
	return nil, source.FileMeta{}, fs.ErrNotExist
}

func (promotionShellSource) Stat(string) (source.FileMeta, error) {
	return source.FileMeta{}, fs.ErrNotExist
}

func (promotionShellSource) Walk(ctx context.Context, _ func(source.FileMeta) error) error {
	return ctx.Err()
}

func (promotionShellSource) Identity() string { return "promotion-shell:empty" }
func (promotionShellSource) Close() error     { return nil }

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
	// This is the one physical full-tree pass a promotion requires. It is
	// background work: interactive ref/checkout requests retain their bounded
	// burst ahead of it in the shared gate, while the mode-transition scheduler
	// prevents a restart journal from filling that background queue with one
	// waiter per row.
	release := func() {}
	if gate := l.buildGate(); gate != nil {
		var err error
		release, err = gate.Acquire(ctx, ViewBuildBackground)
		if err != nil {
			return nil, 0, checkoutSample{}, 0, fmt.Errorf(
				"indexer: wait for dedicated base build admission: %w", err)
		}
	}
	defer release()

	if err := l.ensurePromotedRepoShell(ctx, checkout, prefix); err != nil {
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
		index, generationID, err := l.buildDedicatedCorpusSnapshot(
			ctx, graphID, checkout, prefix, coordinator, before,
		)
		if err != nil {
			return nil, 0, checkoutSample{}, resampled, err
		}

		after, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			coordinator.abandonBuild(ctx, generationID, true)
			return nil, 0, checkoutSample{}, resampled, err
		}
		if after.tree == before.tree && after.commit == before.commit {
			return index, generationID, before, resampled, nil
		}

		resampled++
		coordinator.abandonBuild(ctx, generationID, true)
	}
	return nil, 0, checkoutSample{}, resampled, fmt.Errorf(
		"%w: %s moved under two full generation builds", ErrCheckoutMoved, checkout.RootPath,
	)
}

// buildDedicatedCorpusSnapshot performs one full immutable-tree pass. Both
// initial promotion and pipeline refresh use it so the generation identity and
// parser configuration cannot drift between the two paths.
func (l *CheckoutLifecycle) buildDedicatedCorpusSnapshot(
	ctx context.Context,
	graphID string,
	checkout store_sqlite.Checkout,
	prefix string,
	coordinator *CheckoutCoordinator,
	snapshot checkoutSample,
) (*IndexResult, int64, error) {
	content, err := source.NewGitTreeSource(ctx, checkout.RootPath, snapshot.tree)
	if err != nil {
		return nil, 0, err
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
		return nil, 0, walkErr
	}

	generationID, report, buildErr := coordinator.builder.Build(ctx, BuildRequest{
		Identity: GenerationIdentity{
			OwnerKind:            dedicatedBaseGenerationKind,
			GraphID:              graphID,
			LayerID:              graphID + ":base",
			CheckoutID:           checkout.CheckoutID,
			GenerationKind:       dedicatedBaseGenerationKind,
			LowerViewFingerprint: snapshot.tree,
			TreeOID:              snapshot.tree,
			ProvenanceCommitOID:  snapshot.commit,
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
		return nil, 0, buildErr
	}
	if closeErr != nil {
		coordinator.abandonBuild(ctx, generationID, true)
		return nil, 0, closeErr
	}
	return &IndexResult{
		NodeCount:  report.NodeCount,
		EdgeCount:  report.EdgeCount,
		FileCount:  len(report.IndexedPaths),
		DurationMs: report.Duration.Milliseconds(),
		RepoPrefix: prefix,
	}, generationID, nil
}

// ensurePromotedRepoShell installs the process-local configuration/indexer
// shell required by both the generation builder and the published route. Its
// authoritative empty source makes this an O(1) topology admission, never a
// live-filesystem index. The only full-tree pass remains the exact Git tree
// generation built above.
func (l *CheckoutLifecycle) ensurePromotedRepoShell(
	ctx context.Context, checkout store_sqlite.Checkout, prefix string,
) error {
	if metadata := l.mi.GetMetadata(prefix); metadata != nil {
		if metadata.FileCount == 0 && metadata.NodeCount == 0 && metadata.EdgeCount == 0 {
			return nil
		}
		// A mutable pre-promotion registration may already occupy this prefix.
		// Retire that generation-zero payload before replacing it with the
		// payload-free shell used by the dedicated route.
		if _, _, err := l.mi.UntrackRepoChecked(ctx, prefix); err != nil {
			return fmt.Errorf("indexer: retire mutable pre-promotion repository: %w", err)
		}
	}
	// Before publication an explicit empty source is what prevents a mutable
	// filesystem index. After publication, however, a cold process must use the
	// route-owned restore arm: indexing even an empty source takes the cold
	// shadow path, whose authoritative prefix eviction spans every generation
	// and would erase the immutable corpus the route already owns.
	content := source.ContentSource(promotionShellSource{})
	owned, err := l.mi.routeOwnsDedicatedCorpus(ctx, prefix)
	if err != nil {
		return fmt.Errorf("indexer: inspect dedicated repository route: %w", err)
	}
	if owned {
		content = nil
	}
	_, err = l.mi.trackRepoSourceTransientCtx(ctx,
		config.RepoEntry{Path: checkout.RootPath, Name: prefix}, content)
	if err != nil {
		return fmt.Errorf("indexer: install transient dedicated repository shell: %w", err)
	}
	if l.mi.GetMetadata(prefix) == nil {
		return fmt.Errorf("indexer: dedicated repository shell %s was not installed", prefix)
	}
	return nil
}

// persistPromotedRepoConfig makes a successfully published dedicated route part
// of the next configured-repository replay. It must never run while promotion
// still owns only an off-route shell and an unfinished transition.
func (l *CheckoutLifecycle) persistPromotedRepoConfig(
	checkout store_sqlite.Checkout, prefix string,
) error {
	if l.cfgMgr == nil {
		return nil
	}
	if err := l.cfgMgr.Global().AddRepo(config.RepoEntry{
		Path: checkout.RootPath,
		Name: prefix,
	}); err != nil {
		return fmt.Errorf("indexer: add dedicated repository config: %w", err)
	}
	if err := l.cfgMgr.Global().Save(); err != nil {
		return fmt.Errorf("indexer: flush dedicated repository config: %w", err)
	}
	return nil
}
