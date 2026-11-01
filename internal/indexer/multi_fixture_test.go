package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reach"
)

// These fixtures retain the atomic cold-cohort shape used by lane, topology,
// output-generation and resolver tests. They are deliberately test-only:
// production startup registers roots through TrackRepoCtx and coordinates its
// deferred stages in the daemon. External fixtures use that production path.
// The cohort fixture names the existing topology output entry, not a separate
// production mutation door.
// finishFixtureDeferredPasses completes the cold-index deferred tail without
// repeating the whole-graph cross-repository resolver. The receipt-guided tail
// already performs either exact cross-repository catch-up or a fail-closed full
// pass; only contract-bridge reconciliation remains afterward.
func (mi *MultiIndexer) finishFixtureDeferredPasses(ctx context.Context, completedIndexers []*Indexer) DeferredPassesResult {
	result := mi.beginDeferredPasses(ctx, nil, completedIndexers, true).FinishTailResult()
	mi.ReconcileContractEdges()
	return result
}

// IndexAll indexes all active repos concurrently. Each repo gets its own
// Indexer instance with repo-specific config. Returns per-repo IndexResults.
func (mi *MultiIndexer) IndexAll() (map[string]*IndexResult, error) {
	return mi.indexFixtureScope("", "")
}

// indexFixtureScope selects fixture roots with the same workspace/project
// precedence as registered repositories: entry override, workspace config,
// then prefix. Empty filters retain every active fixture root; a typo refuses
// an empty test graph. This is not a production startup selection API.
func (mi *MultiIndexer) indexFixtureScope(workspaceSlug, projectSlug string) (map[string]*IndexResult, error) {
	repos := mi.configMgr.ActiveRepos()
	if len(repos) == 0 {
		return nil, nil
	}
	if workspaceSlug != "" || projectSlug != "" {
		filtered := mi.filterFixtureReposByScope(repos, workspaceSlug, projectSlug)
		if len(filtered) == 0 {
			return nil, fmt.Errorf("scope filter matched zero of %d active repos (workspace=%q project=%q)", len(repos), workspaceSlug, projectSlug)
		}
		repos = filtered
	}

	// indexFixtureRepos owns the complete coordinated pipeline, including the
	// one contract reconciliation required before graph-wide derivation. Do
	// not repeat it here: reconciliation evicts and rebuilds a generation and
	// a second identical pass is pure global work.
	//
	// A lone repo takes the same path as any other — it is simply the first
	// tracked repo, and its nodes carry its prefix like everyone else's.
	return mi.indexFixtureRepos(repos)
}

// filterFixtureReposByScope returns the subset of repos whose resolved
// workspace and project slugs match the supplied filters. Empty
// filters disable that axis. Loads each repo's `.gortex.yaml` first so
// resolution sees the workspace/project declared there — matching only
// against `RepoEntry.Workspace` would miss repos that declare their
// slug in their own config file (the typical case for first-party
// repos).
func (mi *MultiIndexer) filterFixtureReposByScope(repos []config.RepoEntry, workspaceSlug, projectSlug string) []config.RepoEntry {
	if workspaceSlug == "" && projectSlug == "" {
		return repos
	}
	out := make([]config.RepoEntry, 0, len(repos))
	for _, e := range repos {
		absPath, err := filepath.Abs(e.Path)
		if err != nil {
			continue
		}
		prefix := config.ResolvePrefix(e)
		mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
		cfg := mi.configMgr.GetRepoConfig(prefix)
		entryCopy := e
		if workspaceSlug != "" && resolveWorkspaceID(&entryCopy, cfg, prefix) != workspaceSlug {
			continue
		}
		if projectSlug != "" && resolveProjectID(&entryCopy, cfg, prefix) != projectSlug {
			continue
		}
		out = append(out, e)
	}
	return out
}

// rebuildFixtureRefFacts seeds the durable resolved-reference sidecar once for
// the complete successful repository set. The backend implementation is a
// single set-oriented transaction; keeping the capability probe here avoids a
// per-repository persistence loop and leaves in-memory stores as a no-op.
func (mi *MultiIndexer) rebuildFixtureRefFacts(ctx context.Context, repoPrefixes []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rebuilder, ok := mi.graph.(graph.RefFactsRebuilder)
	if !ok {
		return nil
	}
	if err := rebuilder.RebuildRefFactsForRepos(repoPrefixes); err != nil {
		return err
	}
	// The current backend contract is atomic but not interruptible in flight.
	// Observing cancellation here prevents completion/global consumers from
	// publishing a generation whose ref-fact boundary the caller abandoned.
	return ctx.Err()
}

// indexFixtureRepos indexes multiple repos concurrently with repo prefixes.
func (mi *MultiIndexer) indexFixtureRepos(repos []config.RepoEntry) (map[string]*IndexResult, error) {
	type repoResult struct {
		prefix string
		result *IndexResult
		idx    *Indexer
		meta   *RepoMetadata
		err    error
	}

	// Resolve each repo's prefix serially first: git-worktree instancing,
	// the derived-name persistence, and the cross-batch collision guard
	// must be deterministic, not raced across the parallel index
	// goroutines below. The same loop builds the tracked-module map used
	// for cross-repo dependency detection.
	type resolvedRepo struct {
		entry    config.RepoEntry
		prefix   string
		cfg      *config.Config
		absPath  string
		identity *RepoIdentity
	}
	var configLookup config.RepoPathLookup
	if mi.configMgr != nil {
		configLookup = mi.configMgr.Global().SnapshotRepoPaths()
	}
	moduleRoots := make(map[string]string)
	resolved := make([]resolvedRepo, 0, len(repos))
	seenPrefix := make(map[string]string, len(repos)) // prefix → absPath
	var resolveErrors []string
	for _, entry := range repos {
		absPath, err := filepath.Abs(entry.Path)
		if err != nil {
			resolveErrors = append(resolveErrors, fmt.Sprintf("resolving path %s: %v", entry.Path, err))
			continue
		}
		info, err := os.Stat(absPath)
		if err != nil {
			resolveErrors = append(resolveErrors, fmt.Sprintf("opening repository %s: %v", entry.Path, err))
			continue
		}
		if !info.IsDir() {
			resolveErrors = append(resolveErrors, fmt.Sprintf("opening repository %s: not a directory", entry.Path))
			continue
		}
		identity, _ := DetectIdentity(absPath)
		e := entry
		prefix, cfg := mi.resolveTrackPrefix(&e, absPath, identity, configLookup, mi.configuredPrefixCheck())
		// Two distinct checkouts can still land on the same derived prefix
		// (e.g. two worktrees declaring the same workspace). resolveTrackPrefix
		// only sees already-tracked repos, so guard against collisions within
		// this batch too.
		if prev, ok := seenPrefix[prefix]; ok {
			if pathkey.SamePathIdentity(prev, absPath) {
				// Duplicate config entries for one repository must not launch
				// concurrent raw workers behind a single held repository lane.
				continue
			}
			prefix += "-" + shortPathHash(absPath)
			e.Name = prefix
			// This prefix is minted for THIS batch and is not the one the
			// lifecycle registered an owner under, so the receipt opened for
			// it below finds no owner and the repository is left unwitnessed
			// (openSourceWitness). That is the honest direction — the nodes
			// this pass writes carry the disambiguated prefix too, so no pin
			// taken on the colliding prefix is reading them — but a request
			// against the disambiguated prefix gets ErrBaseCorpusUnwitnessed
			// rather than an exactness label until an owner is registered for
			// the name this batch chose.
		}
		seenPrefix[prefix] = absPath
		resolved = append(resolved, resolvedRepo{entry: e, prefix: prefix, cfg: cfg, absPath: absPath, identity: identity})
		moduleRoots[prefix] = absPath
	}
	trackedModules := readTrackedGoModules(moduleRoots)
	for prefix, mod := range trackedModules {
		mi.logger.Debug("tracked repo module", zap.String("repo", prefix), zap.String("module", mod))
	}

	if len(resolved) == 0 {
		if len(resolveErrors) == 0 {
			return map[string]*IndexResult{}, nil
		}
		sort.Strings(resolveErrors)
		return nil, fmt.Errorf("all repos failed to index: %s", strings.Join(resolveErrors, "; "))
	}

	prefixes := make([]string, 0, len(resolved))
	for _, repo := range resolved {
		prefixes = append(prefixes, repo.prefix)
	}
	// Freeze one batch-mode generation before acquiring any stable repository
	// lane. This global gate -> sorted lanes order prevents a queued transition
	// writer from owning the gate while waiting for a lane held by this batch.
	mi.batchMutationGate.RLock()
	defer mi.batchMutationGate.RUnlock()

	// One receipt per OUTPUT OWNER this batch writes, opened after every lane
	// is held and before any payload moves. The whole batch is one decision: if
	// a newer mutation takes over any of these owners while it runs, the batch
	// cannot fulfil that owner's generation.
	//
	// Owners are deduplicated, and not by prefix. Two tracked entries can name
	// one repository root (two config entries with different explicit Names and
	// one Path: resolveTrackPrefix honours an explicit Name verbatim, so the
	// prefix collision guard above never fires). legacyOutputTargetFor keys the
	// owner on the root, so both entries name ONE generation-zero output.
	// Issuing two receipts for it would make the second supersede the first and
	// refuse a cold index that fully succeeded.
	batchTargets := make([]OutputMutationTarget, 0, len(resolved))
	seenOutputOwner := make(map[string]bool, len(resolved))
	for _, repo := range resolved {
		target := legacyOutputTargetFor(outputStoreIdentity(mi.graph), repo.prefix, repo.absPath, "prefix:"+repo.prefix)
		if seenOutputOwner[target.OwnerKey] {
			continue
		}
		seenOutputOwner[target.OwnerKey] = true
		batchTargets = append(batchTargets, target)
	}

	var finalResults map[string]*IndexResult
	laneErr := mi.withRepositoryMutationLanes(context.Background(), prefixes, func() error {
		receipts, receiptErr := mi.outputGenerationAuthority().BeginAll(
			context.Background(), OutputEntryRepositoryTopology, batchTargets)
		if receiptErr != nil {
			return receiptErr
		}
		settled := false
		defer func() {
			if !settled {
				receipts.Abandon()
			}
		}()
		// Preventive half: refuse before the cold batch writes anything if any
		// owner was already taken over while admission blocked on its gates.
		if err := receipts.Current(); err != nil {
			return err
		}
		batchErr := func() error {
			// The one topology writer covers parse, publication, deferred/cross-repo
			// tails, ref-facts, and global derivation.
			batchMode := mi.currentBatchMode()
			finishTopologyMutation := reach.BeginTopologyMutation(mi.graph)
			defer finishTopologyMutation(true)

			pipelineResults, pipelineErr := func() (map[string]*IndexResult, error) {
				resultCh := make(chan repoResult, len(resolved))
				var wg sync.WaitGroup
				coordinatedBulk, _ := mi.graph.(graph.CoordinatedBulkLoader)
				coordinatedBulkActive := coordinatedBulk != nil && coordinatedBulk.BeginCoordinatedBulkLoad()
				defer func() {
					if coordinatedBulkActive {
						if err := coordinatedBulk.EndCoordinatedBulkLoad(); err != nil {
							mi.logger.Error("multi-repo bulk-load cleanup failed", zap.Error(err))
						}
					}
				}()

				for _, rr := range resolved {
					wg.Add(1)
					go func(r resolvedRepo) {
						defer wg.Done()

						idx := mi.newPerRepoIndexerGuardedWithMode(r.cfg.Index, batchMode)
						idx.SetRepoPrefix(r.prefix)
						entryCopy := r.entry
						idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, r.cfg, r.prefix))
						idx.SetProjectID(resolveProjectID(&entryCopy, r.cfg, r.prefix))
						idx.SetTrackedRepoModules(trackedModules)
						// Defer the per-repo cross-cutting passes (ResolveAll,
						// semantic enrich, contract extract+commit) so they don't
						// race against each other across goroutines on the shared
						// graph. They run serially below via RunDeferredPasses after
						// wg.Wait(). The graph-wide derivation passes run once after
						// the loop via the shared global-pass pipeline.
						idx.SetDeferResolve(true)

						result, err := idx.indexCtxRaw(WithIndexOrigin(context.Background(), "index_all"), r.absPath)
						if err != nil {
							idx.Close()
							resultCh <- repoResult{prefix: r.prefix, err: fmt.Errorf("indexing %s: %w", r.absPath, err)}
							return
						}
						if result == nil {
							idx.Close()
							resultCh <- repoResult{prefix: r.prefix, err: fmt.Errorf("indexing %s returned a nil result", r.absPath)}
							return
						}
						result.RepoPrefix = r.prefix

						meta := &RepoMetadata{
							RepoPrefix:    r.prefix,
							RootPath:      r.absPath,
							Identity:      r.identity,
							LastIndexTime: time.Now(),
							FileCount:     result.FileCount,
							NodeCount:     result.NodeCount,
							EdgeCount:     result.EdgeCount,
							ParseErrors:   result.Errors,
							FileMtimes:    idx.publishFileMtimes(),

							IsWorktree: ResolveWorktree(r.absPath).IsWorktree,
						}

						resultCh <- repoResult{prefix: r.prefix, result: result, idx: idx, meta: meta}
					}(rr)
				}

				go func() {
					wg.Wait()
					close(resultCh)
				}()

				results := make(map[string]*IndexResult)
				indexErrors := resolveErrors
				completed := make([]repoResult, 0, len(resolved))

				// Drain workers without holding the registry lock. Constructors and future
				// worker tails may need read access to MultiIndexer state; pinning mi.mu
				// across the channel range turns any such read into a producer/consumer
				// deadlock and blocks unrelated registry readers for the whole warmup.
				for rr := range resultCh {
					if rr.err != nil {
						mi.logger.Error("failed to index repo", zap.String("prefix", rr.prefix), zap.Error(rr.err))
						indexErrors = append(indexErrors, rr.err.Error())
						continue
					}
					completed = append(completed, rr)
					results[rr.prefix] = rr.result
				}
				oldIndexers := make([]*Indexer, 0, len(completed))
				mi.mu.Lock()
				for _, rr := range completed {
					if old := mi.indexers[rr.prefix]; old != nil && old != rr.idx {
						oldIndexers = append(oldIndexers, old)
					}
					mi.repos[rr.prefix] = rr.meta
					mi.indexers[rr.prefix] = rr.idx
				}
				mi.mu.Unlock()
				for _, old := range oldIndexers {
					old.Close()
				}
				if coordinatedBulkActive {
					if err := coordinatedBulk.EndCoordinatedBulkLoad(); err != nil {
						return nil, fmt.Errorf("multi-repo bulk-load finalize: %w", err)
					}
					coordinatedBulkActive = false
				}
				for _, rr := range completed {
					if err := mi.postPublishGoModules(context.Background(), rr.idx, true); err != nil {
						mi.logger.Warn("post-publish Go module follow-up", zap.Error(err))
					}
				}

				// Do not publish a completed pipeline when no repository reached the
				// deferred stages. Besides making the failure deterministic, this prevents
				// global passes from deriving edges from a partially-drained failed batch.
				if len(indexErrors) > 0 && len(results) == 0 {
					sort.Strings(indexErrors)
					return nil, fmt.Errorf("all repos failed to index: %s", strings.Join(indexErrors, "; "))
				}

				// Complete cold multi-repo indexing through the same coordinated pipeline
				// used by daemon warmup:
				//   1. materialise go.mod contracts once, then run one shared base resolve;
				//   2. enrich repositories in bounded language-aware batches (large Go
				//      repositories remain exclusive), committing contracts only after each
				//      batch drains;
				//   3. use the mutation receipt to perform only the exact catch-up needed for
				//      semantic/contract mutations.
				//
				// The old loop called idx.RunDeferredPasses with
				// skipResolveInDeferred=false, so every repository performed ResolveAll over
				// the entire shared graph. At R repositories and E edges that was O(R*E) and
				// was the dominant cold-index regression. runDeferredGoMod is generation-
				// idempotent, so RunDeferredPassesAll does not repeat the pre-resolve work.
				deferCtx := context.Background()
				if err := mi.RunPreEnrichResolve(deferCtx, nil, nil); err != nil {
					return results, fmt.Errorf("multi-repo pre-enrichment resolve: %w", err)
				}
				completedIndexers := make([]*Indexer, 0, len(completed))
				for _, rr := range completed {
					completedIndexers = append(completedIndexers, rr.idx)
				}
				deferredResult := mi.finishFixtureDeferredPasses(deferCtx, completedIndexers)
				mi.logger.Info("multi-repo coordinated deferred passes complete",
					zap.Int("repos_indexed", len(results)),
					zap.Int("repos_failed", len(indexErrors)),
					zap.Int("enrich_scheduled", deferredResult.EnrichScheduled),
					zap.Bool("exact_cross_repo_complete", deferredResult.ExactCrossRepoComplete))

				// ResolveAll normally seeds ref_facts after a full resolve. The coordinated
				// cold path intentionally bypasses per-repository ResolveAll, so seed the
				// successful repository set once after every base, semantic catch-up, and
				// cross-repository mutation has settled. Sorting makes the boundary stable
				// for tracing/tests; SQLite consumes the whole slice in one transaction.
				successfulPrefixes := make([]string, 0, len(results))
				for prefix := range results {
					successfulPrefixes = append(successfulPrefixes, prefix)
				}
				sort.Strings(successfulPrefixes)
				if err := mi.rebuildFixtureRefFacts(deferCtx, successfulPrefixes); err != nil {
					return results, fmt.Errorf("multi-repo reference-fact rebuild: %w", err)
				}

				// Graph-wide derivation passes run exactly once after every repo
				// has been parsed, every per-repo and cross-repo resolver has lifted
				// placeholder edges, and contract bridges are in place. RunDeferredPasses
				// intentionally skips these so we don't pay an O(global) walk per
				// repo (was the dominant cost at R≈100+).
				mi.runGlobalGraphPassesTopologyHeld(context.Background(), nil, false)

				return results, nil
			}()
			finalResults = pipelineResults
			return pipelineErr
		}()
		if batchErr != nil {
			return batchErr
		}
		settled = true
		// Fulfilment is refused when a newer mutation took over any of these
		// owners while the batch ran; the batch then reports that refusal
		// instead of announcing a completed cold index.
		if err := receipts.Complete(); err != nil {
			return err
		}
		// Every completed repository's journal is accepted, failed files
		// included: the cold pass staged a failure envelope for each of them
		// (indexCtxRaw), so the accepted namespace excludes exactly that set.
		for prefix, result := range finalResults {
			if result != nil {
				if err := mi.acceptInstalledContractCoreInputs(context.Background(), prefix); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return finalResults, laneErr
}
