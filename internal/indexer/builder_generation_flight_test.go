package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// sparseBuildFlightFixture builds a base corpus plus an A -> B target tree and
// the change set between them, so a caller can drive one sparse generation
// build against it. The physical-flight coalescing tests that once lived here
// were removed with the model-free rebuild: they observed the physical pass
// through a Walk-counting seam that the kept "walk sparse sources directly"
// refactor made unreachable, and depended on the skipped observation/adoption
// seams. The fixture and its request/result helpers survive because the
// generation-retirement suites build on them.
type sparseBuildFlightFixture struct {
	store   *store_sqlite.Store
	builder *SparseGenerationBuilder
	request BuildRequest
}

func newSparseBuildFlightFixture(t testing.TB) sparseBuildFlightFixture {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "flight-repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	builderWriteTree(t, repoDir, builderTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	baseDir := builderTempDir(t, "flight-base")
	builderWriteTree(t, baseDir, builderTreeA())

	builderWriteTree(t, repoDir, builderTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "flight-base")
	builderIndex(t, store, baseDir)
	changes, err := diffTreeChanges(context.Background(), repoDir, treeA, treeB)
	if err != nil {
		t.Fatalf("diff trees: %v", err)
	}
	target, err := source.NewGitTreeSource(context.Background(), repoDir, treeB)
	if err != nil {
		t.Fatalf("open target tree: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	return sparseBuildFlightFixture{
		store:   store,
		builder: builderNewBuilder(store),
		request: BuildRequest{
			Identity: GenerationIdentity{
				OwnerKind:           "dedicated_graph",
				GraphID:             "graph-flight",
				LayerID:             "layer-" + treeB,
				CheckoutID:          "checkout-flight",
				GenerationKind:      CommitLayerGenerationKind,
				TreeOID:             treeB,
				ProvenanceCommitOID: commitB,
				CreatedAt:           time.Now().Unix(),
			},
			Base:        store,
			Target:      target,
			Changes:     changes,
			RootPath:    baseDir,
			RepoPrefix:  builderRepoPrefix,
			WorkspaceID: builderRepoPrefix,
			ProjectID:   builderRepoPrefix,
		},
	}
}

type sparseBuildFlightResult struct {
	generationID int64
	report       BuildReport
	err          error
}

func payloadRequestForBuild(req BuildRequest) store_sqlite.PayloadGenerationRequest {
	return store_sqlite.PayloadGenerationRequest{
		OwnerKind:            req.Identity.OwnerKind,
		GraphID:              req.Identity.GraphID,
		LayerID:              req.Identity.LayerID,
		CheckoutID:           req.Identity.CheckoutID,
		GenerationKind:       req.Identity.GenerationKind,
		BaseGenerationID:     req.Identity.BaseGenerationID,
		LowerViewFingerprint: req.Identity.LowerViewFingerprint,
		TreeOID:              req.Identity.TreeOID,
		ProvenanceCommitOID:  req.Identity.ProvenanceCommitOID,
		ConfigHash:           req.Identity.ConfigHash,
		ExtractorVersions:    req.Identity.ExtractorVersions,
		ResolverVersion:      req.Identity.ResolverVersion,
		CreatedAt:            req.Identity.CreatedAt,
	}
}
