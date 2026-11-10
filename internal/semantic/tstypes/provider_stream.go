package tstypes

import (
	"context"
	"runtime"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// stageRepoFacts is the only SQLite writer. Parser workers never transact;
// their results are encoded and committed in bounded file/byte batches by the
// receiver goroutine, then released before the next batch grows.
func (p *Provider) stageRepoFacts(ctx context.Context, files []fileRef, spool *factSpool) error {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers > len(files) {
		workers = len(files)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan fileRef)
	factsCh := make(chan *fileFacts, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case ref, ok := <-jobs:
					if !ok {
						return
					}
					facts, err := analyzeFile(p.spec, ref)
					if err != nil {
						if workCtx.Err() != nil {
							return
						}
						p.logger.Debug("tstypes: file analysis failed",
							zap.String("provider", p.Name()),
							zap.String("file", ref.node.FilePath),
							zap.Error(err))
						continue
					}
					if facts != nil {
						select {
						case factsCh <- facts:
						case <-workCtx.Done():
							return
						}
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, ref := range files {
			select {
			case jobs <- ref:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(factsCh)
	}()

	batch := make([]stagedFileFacts, 0, tstypesFactPageFiles)
	batchBytes := 0
	var stageErr error
	flush := func() {
		if stageErr != nil || len(batch) == 0 {
			return
		}
		if err := spool.appendFiles(batch); err != nil {
			stageErr = err
			cancel()
		}
		for i := range batch {
			batch[i] = stagedFileFacts{}
		}
		batch = batch[:0]
		batchBytes = 0
	}
	for facts := range factsCh {
		if stageErr != nil || workCtx.Err() != nil {
			continue
		}
		record, err := stageFileFacts(facts)
		if err != nil {
			stageErr = err
			cancel()
			continue
		}
		if len(batch) > 0 && (len(batch) >= tstypesFactPageFiles ||
			batchBytes+record.bytes > tstypesFactPageBytes) {
			flush()
		}
		if stageErr == nil {
			batch = append(batch, record)
			batchBytes += record.bytes
		}
	}
	if stageErr == nil && ctx.Err() == nil {
		flush()
	}
	if stageErr != nil {
		return stageErr
	}
	return ctx.Err()
}

type stagedFactPhase int

const (
	stagedSupers stagedFactPhase = iota
	stagedMetas
	stagedAliases
	stagedCalls
)

func (p *Provider) applyStagedFacts(ctx context.Context, g graph.Store, repoPrefix string, spool *factSpool, resume *applyResume, res *semantic.EnrichResult) error {
	if counter, ok := g.(graph.RepoLanguageSymbolCounter); ok {
		res.SymbolsTotal = counter.CountRepoLanguageSymbols(repoPrefix, p.spec.Languages)
	} else {
		// Compatibility-only stores may lack the count projection. Production
		// Graph and SQLite stores implement it without row materialization.
		langs := make(map[string]bool, len(p.spec.Languages))
		for _, language := range p.spec.Languages {
			langs[language] = true
		}
		for _, node := range g.GetRepoNodes(repoPrefix) {
			if node != nil && langs[node.Language] && node.Kind != graph.KindFile && node.Kind != graph.KindImport {
				res.SymbolsTotal++
			}
		}
	}

	// One pass-scoped read-through cache for every page applier below: the
	// per-page hydration of the repo's shared type universe dominated whole-
	// process CPU (48–62% measured) because each of the 4 phases re-fetched
	// near-identical name groups and inheritance frontiers per 32-file page.
	hot := newApplyHotCache(applyHotCacheBudget())

	// Coverage walk: every staged file, no payload decode. The supers phase
	// no longer sees files without inheritance facts, so coverage counting
	// cannot ride on it; this walk also pre-warms the per-file node groups
	// for all four phases. It is the first work under the pass deadline and
	// flushes no edges, so an expiry during it yields a zero-edge partial —
	// accepted because the walk is decode-free: a budget too small for it
	// could not have completed any apply phase either.
	//
	// A resumed pass skips the walk: the count it would produce is the one
	// the interrupted pass recorded with its point, and later phases hydrate
	// the file groups they need on demand.
	// Files re-parsed since the point was recorded lost the enrichment their
	// committed pages applied, and so did the facts of their direct
	// importers that bound into them. Both are applied again in every phase
	// the cursor has passed, while the cursor itself is kept — unless the
	// set is over half the staged files, when replaying it costs about what
	// a pass from the first page does and that pass is taken instead.
	if resume.reapplying() {
		resume.beginReapply()
		reparsed := resume.reapplyFiles()
		importers, err := spool.importersOf(ctx, repoPrefix, reparsed)
		if err != nil {
			return err
		}
		staged, err := spool.fileCount(ctx)
		if err != nil {
			return err
		}
		if set := len(reparsed) + len(importers); 2*set > staged {
			p.logger.Info("tstypes: re-parsed files and their importers exceed half the staged files; applying from the first page",
				zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix),
				zap.Int("reapply_files", set), zap.Int("staged_files", staged))
			resume.restart()
		} else {
			if err := spool.markReapply(append(append([]string(nil), reparsed...), importers...)); err != nil {
				return err
			}
			p.logger.Info("tstypes: resumed apply re-applies re-parsed files",
				zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix),
				zap.Int("reparsed_files", len(reparsed)), zap.Int("importers", len(importers)))
		}
	}
	reapply := resume.reapplying()
	startPhase, startAfter, covered, resumed := resume.start()
	if resumed {
		res.SymbolsCovered = covered
	} else {
		after := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, last, err := spool.pageFiles(ctx, after)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			ap := newApplier(g, p.spec, p.Name()).withHotCache(hot)
			res.SymbolsCovered += ap.coverFiles(page)
			after = last
		}
		resume.commit(stagedSupers, "", res.SymbolsCovered)
	}

	classForPhase := map[stagedFactPhase]factClass{
		stagedSupers:  classSupers,
		stagedMetas:   classMetas,
		stagedAliases: classAliases,
		stagedCalls:   classCalls,
	}
	for phase := stagedSupers; phase <= stagedCalls; phase++ {
		// A phase before the resume point was committed by an earlier pass
		// at this revision; its edges and stamps are read back from the
		// store. Aliases resolve into this pass's spool only, so that phase
		// always runs before calls.
		var ranges []factPageRange
		switch {
		case phase < startPhase && phase != stagedAliases:
			if reapply {
				ranges = []factPageRange{{reapply: true}}
			}
		case phase == startPhase:
			if reapply && startAfter != "" {
				ranges = append(ranges, factPageRange{through: startAfter, reapply: true})
			}
			ranges = append(ranges, factPageRange{after: startAfter})
		default:
			ranges = []factPageRange{{}}
		}
		if len(ranges) == 0 {
			continue
		}
		// Adjacency is only valid within one phase: the supers phase
		// synthesizes inheritance edges that later phases' frontier walks
		// must observe. Nodes and name groups survive phase boundaries —
		// apply never creates nodes, and the shared pointers carry every
		// same-pass Meta stamp.
		hot.flushAdjacency()
		for _, rng := range ranges {
			after := rng.after
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				page, last, stats, err := spool.pageClassRange(ctx, classForPhase[phase], after, rng)
				if err != nil {
					return err
				}
				if len(page) == 0 {
					break
				}
				ap := newApplier(g, p.spec, p.Name()).withHotCache(hot)
				switch phase {
				case stagedSupers:
					err = ap.applySupersPage(ctx, page, res)
				case stagedMetas:
					err = ap.applyMetasPage(ctx, page, res)
					ap.flush()
				case stagedAliases:
					var aliases []stagedResolvedAlias
					aliases, err = ap.resolveAliasesPage(ctx, page)
					if err == nil {
						err = spool.appendAliases(aliases)
					}
				case stagedCalls:
					// Prepare once so the alias query is driven by this page's exact
					// receiver/type frontier, never by every alias in the repository.
					ap.preload(page)
					var aliases []stagedResolvedAlias
					aliases, err = spool.aliasesForTypeIDs(ctx, ap.typeNodeIDs())
					if err == nil {
						err = ap.applyCallsPage(ctx, page, aliases, res)
					}
				}
				if err == nil && p.pageFault != nil {
					err = p.pageFault(stats)
				}
				if p.observePage != nil {
					p.observePage(ap.pageStats(stats))
				}
				for i := range page {
					page[i] = nil
				}
				if err != nil {
					return err
				}
				after = last
				// The page's writes have returned (metas flushed above), so
				// the next pass may start after it. Re-applied pages sit
				// behind the cursor and do not move it.
				if phase != stagedAliases && !rng.reapply {
					resume.commit(phase, after, res.SymbolsCovered)
				}
			}
		}
	}
	stats := hot.statsSnapshot()
	p.logger.Info("tstypes: apply hot cache",
		zap.String("provider", p.Name()),
		zap.String("repo_prefix", repoPrefix),
		zap.Int64("node_hits", stats.NodeHits), zap.Int64("node_misses", stats.NodeMisses),
		zap.Int64("name_hits", stats.NameHits), zap.Int64("name_misses", stats.NameMisses),
		zap.Int64("adj_hits", stats.AdjHits), zap.Int64("adj_misses", stats.AdjMisses),
		zap.Int64("file_hits", stats.FileHits), zap.Int64("file_misses", stats.FileMisses))
	if res.SymbolsTotal > 0 {
		res.CoveragePercent = float64(res.SymbolsCovered) / float64(res.SymbolsTotal) * 100
	}
	return nil
}
