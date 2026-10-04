package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
)

// The one-time correction of generations an older derivation wrote.
//
// At startup, for every stamped pass, the catalogued base generations (commit
// layers and dedicated bases) whose stamp is older than the running pass are
// re-derived in place through the store's derived-row correction
// (BeginDerivedCorrection → ReplaceSourceEdges → Finish): only the pass's rows
// change, in bounded chunks, and the stamp moves last, so an interrupted
// correction runs again from the start. Working-tree generations are not
// corrected: the next edit replaces them, and each build stamps its own rows.
//
// The correction is background work and gives way to interactive work with
// the retirement sweep's predicate (interactiveWriteWanted): it waits before
// each generation while an edit cycle holds the build lane or a writer waits,
// and a chunk in flight when one appears is cancelled (its transaction rolls
// back; the chunk is replaced by source, so running it again is idempotent).

// derivedCorrectionGenerationKinds are the generation kinds corrected: the
// long-lived bases every delta composes over.
var derivedCorrectionGenerationKinds = map[string]bool{
	CommitLayerGenerationKind:   true,
	DedicatedBaseGenerationKind: true,
}

// derivedCorrectionPoll is how often a waiting correction looks again.
const derivedCorrectionPoll = 25 * time.Millisecond

// DerivedCorrectionReport is one startup correction run.
type DerivedCorrectionReport struct {
	Stale      int // generations found stale, over every pass
	Corrected  int // generations whose stamp moved to the running version
	Skipped    int // stale generations not corrected (kind, state, no view)
	Preempted  int // chunks given up to interactive work (and run again)
	Sources    int
	EdgesMoved int64 // edges deleted plus inserted
	Nodes      int64 // node rows rewritten
	// Unmatched counts file rows whose recorded content could not be read
	// back as the rows' own parse (their fingerprints stay unset).
	Unmatched int
	// Unowned counts sources whose derived rows the generation does not
	// speak for (left to the generation that does).
	Unowned int
	// RowsChanged / NoOps split the corrected generation-passes into those
	// that wrote rows and those that only moved the stamp.
	RowsChanged int
	NoOps       int
	Elapsed     time.Duration
}

// derivationEnv is what correcting one generation needs: its composed view
// and a parser configured as its builds were.
type derivationEnv struct {
	// view opens the generation's composed view (the generation on top of
	// everything beneath it).
	view func(ctx context.Context) (graph.Reader, func(), error)
	// parser returns an Indexer that parses as the generation's builds did,
	// with its repository prefix and root; nil when none is available.
	parser func() *Indexer
	// content reads a file's bytes as the generation recorded it.
	content func(ctx context.Context, rel string) ([]byte, bool)
	// batch, when set, opens one reader of the recorded content for a whole
	// generation (git cat-file --batch) and is preferred over content.
	batch func(ctx context.Context) (func(ctx context.Context, rel string) ([]byte, bool), func(), error)
}

// derivationEnvFor assembles the environment a generation is corrected in.
// A checkout with a running coordinator lends its builder's parser and its
// view; any other — a lazily activated checkout whose coordinator has not
// started, a commit row with no checkout — gets a temporary read-only view of
// the generation from a materializer of the lifecycle's own and a parser from
// the repository's indexer and configuration. reason names why a generation
// cannot be corrected here when ok is false.
func (l *CheckoutLifecycle) derivationEnvFor(ctx context.Context, row store_sqlite.ViewGeneration) (env derivationEnv, reason string, ok bool) {
	if l.derivationEnvHook != nil {
		env, ok = l.derivationEnvHook(row)
		if !ok {
			reason = "no_environment"
		}
		return env, reason, ok
	}
	l.coordMu.Lock()
	c := l.coordinators[row.CheckoutID]
	l.coordMu.Unlock()
	if c != nil && c.builder != nil && c.builder.Store != nil {
		generationID := row.GenerationID
		store := c.builder.Store
		env = derivationEnv{
			view: func(ctx context.Context) (graph.Reader, func(), error) {
				return c.generationLayerReader(ctx, generationID)
			},
			parser: func() *Indexer {
				return derivationParser(store, generationID, c.builder.Registry, c.builder.Config, c.logger, c.repoPrefix, c.root)
			},
		}
		if row.ProvenanceCommitOID != "" && c.root != "" {
			env.content = gitBlobContent(c.root, row.ProvenanceCommitOID)
			env.batch = gitBatchContent(c.root, row.ProvenanceCommitOID)
		}
		return env, "", true
	}
	if row.GraphID == "" {
		return derivationEnv{}, "no_graph", false
	}
	if l.leases == nil {
		return derivationEnv{}, "no_lease_manager", false
	}
	dedicated, found, err := l.catalog.GetDedicatedGraph(ctx, row.GraphID)
	if err != nil {
		return derivationEnv{}, "graph_read_failed", false
	}
	if !found || dedicated.RepoPrefix == "" {
		return derivationEnv{}, "graph_not_catalogued", false
	}
	materializer := &graphview.Materializer{Store: l.store, Catalog: l.catalog, Leases: l.leases, Logger: l.logger}
	graphID, generationID := row.GraphID, row.GenerationID
	env.view = func(ctx context.Context) (graph.Reader, func(), error) {
		view, err := materializer.MaterializeRefView(ctx, graphID, generationID)
		if err != nil {
			return nil, nil, err
		}
		return view.Reader, view.Close, nil
	}
	// The committed content, read from any working copy of the repository:
	// the generation's own checkout, its graph's owner, or the repository's
	// indexed root (every worktree shares the object store).
	root := ""
	for _, checkoutID := range []string{row.CheckoutID, dedicated.OwnerCheckoutID} {
		if checkoutID == "" || root != "" {
			continue
		}
		if checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID); err == nil && found {
			root = checkout.RootPath
		}
	}
	var primary *Indexer
	if l.mi != nil {
		primary = l.mi.GetIndexer(dedicated.RepoPrefix)
	}
	if root == "" && primary != nil {
		root = primary.RootPath()
	}
	if l.mi != nil && l.mi.registry != nil {
		cfg := config.Default()
		if l.cfgMgr != nil {
			cfg = l.cfgMgr.GetRepoConfig(dedicated.RepoPrefix)
		}
		registry, prefix, index := l.mi.registry, dedicated.RepoPrefix, cfg.Index
		store := l.store
		env.parser = func() *Indexer {
			return derivationParser(store, generationID, registry, index, l.logger, prefix, root)
		}
	}
	if row.ProvenanceCommitOID != "" && root != "" {
		env.content = gitBlobContent(root, row.ProvenanceCommitOID)
		env.batch = gitBatchContent(root, row.ProvenanceCommitOID)
	}
	return env, "", true
}

// derivationParser is the parse-only Indexer a correction re-derives a
// generation's fingerprints with. It extracts file content and never touches
// its graph, so it is given the durable store pinned to the generation being
// corrected — a published, sealed generation — rather than an in-memory Store:
// that one is the indexer's cold-index staging buffer only (see the graph
// package's New fence), and anything held in it would die with the process.
func derivationParser(
	store *store_sqlite.Store, generationID int64, registry *parser.Registry, cfg config.IndexConfig,
	logger *zap.Logger, repoPrefix, root string,
) *Indexer {
	idx := New(store.AtGeneration(generationID), registry, cfg, logger)
	idx.SetRepoPrefix(repoPrefix)
	idx.rootPath = root
	return idx
}

// gitBlobContent reads a path at a commit.
func gitBlobContent(root, commit string) func(ctx context.Context, rel string) ([]byte, bool) {
	return func(ctx context.Context, rel string) ([]byte, bool) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "blob", commit+":"+rel).Output()
		return out, err == nil
	}
}

// CorrectStaleDerivations re-derives, once, every stale base generation's
// derived rows. It returns when every stale generation it can correct is
// corrected, or ctx ends.
func (l *CheckoutLifecycle) CorrectStaleDerivations(ctx context.Context) (DerivedCorrectionReport, error) {
	var report DerivedCorrectionReport
	if l == nil || l.store == nil || l.catalog == nil {
		return report, nil
	}
	started := time.Now()
	memo := newDerivationMemo()
	// Nothing of the correction — not even its catalog reads — runs while an
	// edit cycle holds the lane or a writer waits.
	if err := l.waitForInteractiveQuiet(ctx, l.coordinatorSnapshot()); err != nil {
		return report, err
	}
	// The fingerprints first: they read nothing the capability pass writes.
	for _, pass := range []struct {
		name    string
		version int
	}{
		{derivationPassFileFingerprints, fileFingerprintDerivationVersion},
		{derivationPassCapability, capabilityDerivationVersion},
	} {
		stale, err := l.store.GenerationsWithStaleDerivation(ctx, pass.name, pass.version)
		if err != nil {
			return report, err
		}
		report.Stale += len(stale)
		for _, g := range stale {
			row, found, err := l.catalog.GetViewGeneration(ctx, g.GenerationID)
			if err != nil {
				return report, err
			}
			switch {
			case !found:
				report.Skipped++
				l.logCorrectionSkip(store_sqlite.ViewGeneration{GenerationID: g.GenerationID}, pass.name, "no_catalog_row")
				continue
			case row.State != store_sqlite.ViewGenerationReady:
				report.Skipped++
				l.logCorrectionSkip(row, pass.name, "state_"+string(row.State))
				continue
			case !derivedCorrectionGenerationKinds[row.GenerationKind]:
				// Working-tree layers are replaced by the next edit; not a
				// skip worth a line each.
				report.Skipped++
				continue
			}
			env, reason, ok := l.derivationEnvFor(ctx, row)
			if !ok {
				report.Skipped++
				l.logCorrectionSkip(row, pass.name, reason)
				continue
			}
			corrected, err := l.correctGenerationYielding(ctx, pass.name, g.Version, pass.version, row, env, memo, &report)
			if err != nil {
				if ctx.Err() != nil {
					return report, ctx.Err()
				}
				report.Skipped++
				if l.logger != nil {
					l.logger.Warn("indexer: derived-row correction left a generation stale",
						zap.Int64("generation", row.GenerationID), zap.String("pass", pass.name), zap.Error(err))
				}
				continue
			}
			if corrected {
				report.Corrected++
			}
		}
	}
	// Set before the summary is logged (it was set in a defer after the log
	// line, so the line always said 0).
	report.Elapsed = time.Since(started)
	if l.logger != nil && report.Stale > 0 {
		l.logger.Info("indexer: stale derivations corrected",
			zap.Int("stale", report.Stale), zap.Int("corrected", report.Corrected), zap.Int("skipped", report.Skipped),
			zap.Int("rows_changed", report.RowsChanged), zap.Int("no_ops", report.NoOps),
			zap.Int("preempted", report.Preempted), zap.Int("sources", report.Sources),
			zap.Int64("edges_moved", report.EdgesMoved), zap.Int64("nodes", report.Nodes),
			zap.Int("unmatched_files", report.Unmatched), zap.Int("unowned_sources", report.Unowned), zap.Duration("elapsed", report.Elapsed))
	}
	return report, nil
}

// logCorrectionSkip names why a stale generation was left stale.
func (l *CheckoutLifecycle) logCorrectionSkip(row store_sqlite.ViewGeneration, pass, reason string) {
	if l.logger == nil {
		return
	}
	l.logger.Info("indexer: stale derivation not corrected",
		zap.Int64("generation", row.GenerationID), zap.String("pass", pass),
		zap.String("kind", row.GenerationKind), zap.String("checkout", row.CheckoutID),
		zap.String("graph", row.GraphID), zap.String("reason", reason))
}

// correctGenerationYielding corrects one generation, standing down for
// interactive work: it waits while such work wants the writer, and runs the
// correction under a context cancelled the moment it appears; a preempted
// correction waits and starts over (it never finished, so the stamp has not
// moved and every chunk it wrote is re-derived and replaced the same way).
func (l *CheckoutLifecycle) correctGenerationYielding(
	ctx context.Context, pass string, from, to int, row store_sqlite.ViewGeneration, env derivationEnv, memo *derivationMemo, report *DerivedCorrectionReport,
) (bool, error) {
	coordinators := l.coordinatorSnapshot()
	for {
		if err := l.waitForInteractiveQuiet(ctx, coordinators); err != nil {
			return false, err
		}
		runCtx, stop := l.preemptOnInteractiveWrite(ctx, coordinators)
		err := l.correctGeneration(runCtx, pass, from, to, row, env, memo, report)
		preempted := retirementPreempted(ctx, runCtx)
		stop()
		if preempted {
			report.Preempted++
			l.derivedCorrectionPreempted.Add(1)
			continue
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
}

// coordinatorSnapshot is the registered coordinators, for the interactive
// predicate.
func (l *CheckoutLifecycle) coordinatorSnapshot() []*CheckoutCoordinator {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	out := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, c := range l.coordinators {
		out = append(out, c)
	}
	return out
}

// correctionPreemptions reports the correction runs given up to interactive
// work so far.
func (l *CheckoutLifecycle) correctionPreemptions() int64 { return l.derivedCorrectionPreempted.Load() }

// waitForInteractiveQuiet returns once no interactive work wants the writer.
func (l *CheckoutLifecycle) waitForInteractiveQuiet(ctx context.Context, coordinators []*CheckoutCoordinator) error {
	for l.interactiveWriteWanted(coordinators) {
		timer := time.NewTimer(derivedCorrectionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

// derivedCorrectionChunkRows bounds the rows one correction chunk writes:
// one chunk holds the writer, and at the cold store's ≈0.5 ms per row a
// 4,096-row chunk measured up to 7.8 s. 512 rows keeps one hold near a
// quarter of a second there.
const derivedCorrectionChunkRows = 512

// derivationMemo keeps, for one correction run, what one generation's
// correction derived for the next generation with the same content (the
// same tree over the same base under the same configuration — commit layers
// several checkouts built for one commit), and each file's parsed
// fingerprints per tree.
type derivationMemo struct {
	capability   map[string]*capabilityDerivation
	fingerprints map[string]fileFingerprintOutcome
}

// capabilityDerivation is one generation content's derived capability rows.
type capabilityDerivation struct {
	// computed is every source the rows were read for; rows the capability
	// rows of each (absent: none); touched the sources the pass touched.
	computed map[string]struct{}
	rows     map[string][]*graph.Edge
	touched  []string
}

// fileFingerprintOutcome is one file's content parsed at a tree.
type fileFingerprintOutcome struct {
	derived derivedFingerprints
	shape   string
	ok      bool
}

func newDerivationMemo() *derivationMemo {
	return &derivationMemo{
		capability:   make(map[string]*capabilityDerivation),
		fingerprints: make(map[string]fileFingerprintOutcome),
	}
}

// derivationContentKey names a generation's content for the memo: two
// generations with the same key hold the same rows over the same view. The
// empty key (no tree or no base) is never shared.
func derivationContentKey(row store_sqlite.ViewGeneration) string {
	if row.TreeOID == "" || row.BaseGenerationID <= 0 {
		return ""
	}
	return strings.Join([]string{
		row.GenerationKind, strconv.FormatInt(row.BaseGenerationID, 10), row.TreeOID, row.LowerViewFingerprint,
		row.ConfigHash, row.ExtractorVersions, row.ResolverVersion, row.DependencyRevision,
	}, "\x00")
}

// derivationGenerationStats is one generation-pass correction, logged per
// generation.
type derivationGenerationStats struct {
	candidates, changed, unchanged, unowned int
	rows                                    int64
	reused                                  bool
	view, derive, diff, write               time.Duration
}

// correctGeneration re-derives one pass's rows of one generation and moves
// its stamp.
func (l *CheckoutLifecycle) correctGeneration(
	ctx context.Context, pass string, from, to int, row store_sqlite.ViewGeneration, env derivationEnv, memo *derivationMemo, report *DerivedCorrectionReport,
) error {
	started := time.Now()
	kinds := capabilityDerivedEdgeKinds
	if pass == derivationPassFileFingerprints {
		kinds = []graph.EdgeKind{fileFingerprintCorrectionKind}
	}
	correction, err := l.store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{
		GenerationID: row.GenerationID, Pass: pass, FromVersion: from, ToVersion: to, EdgeKinds: kinds,
	})
	if err != nil {
		return err
	}
	own := l.store.AtGeneration(row.GenerationID)
	var stats derivationGenerationStats
	switch pass {
	case derivationPassFileFingerprints:
		err = correctFileFingerprints(ctx, correction, own, env, row, memo, report, &stats)
	case derivationPassCapability:
		err = correctCapabilityRows(ctx, correction, own, env, row, memo, report, &stats)
	default:
		err = fmt.Errorf("indexer: no correction for pass %q", pass)
	}
	if err != nil {
		return err
	}
	finished, err := correction.Finish(ctx)
	if err != nil {
		return err
	}
	if finished.Chunks > 0 {
		report.RowsChanged++
	} else {
		report.NoOps++
	}
	if l.logger != nil {
		l.logger.Info("indexer: derivation corrected",
			zap.Int64("generation", row.GenerationID), zap.String("pass", pass), zap.String("kind", row.GenerationKind),
			zap.Int("candidates", stats.candidates), zap.Int("changed", stats.changed), zap.Int("unchanged", stats.unchanged),
			zap.Int("unowned", stats.unowned), zap.Int64("rows_written", stats.rows), zap.Int("chunks", finished.Chunks),
			zap.Bool("rows_changed", finished.Chunks > 0), zap.Bool("reused_derivation", stats.reused),
			zap.Duration("view", stats.view), zap.Duration("derive", stats.derive), zap.Duration("diff", stats.diff),
			zap.Duration("write", stats.write), zap.Duration("max_chunk_hold", finished.MaxChunkHold),
			zap.Duration("elapsed", time.Since(started)))
	}
	return nil
}

// correctFileFingerprints stamps the derived fingerprints on the
// generation's file rows that carry none, from the content the generation
// recorded, when that content parses to the rows' own declarations. The
// content is read through one git cat-file --batch process per generation,
// and a file parsed at a tree is not parsed again for another generation of
// the same tree.
func correctFileFingerprints(
	ctx context.Context, correction *store_sqlite.DerivedCorrection, own *store_sqlite.Store, env derivationEnv,
	row store_sqlite.ViewGeneration, memo *derivationMemo, report *DerivedCorrectionReport, stats *derivationGenerationStats,
) error {
	var files []*graph.Node
	for node := range own.NodesByKind(graph.KindFile) {
		if node != nil && node.FilePath != "" && !storedDerivedFingerprints([]*graph.Node{node}).complete() {
			files = append(files, node)
		}
	}
	stats.candidates = len(files)
	content := env.content
	if env.batch != nil {
		reader, closeReader, err := env.batch(ctx)
		if err == nil {
			defer closeReader()
			content = reader
		}
	}
	if len(files) == 0 || env.parser == nil || content == nil {
		report.Unmatched += len(files)
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].FilePath < files[j].FilePath })
	idx := env.parser()
	defer idx.Close()
	var chunk []*graph.Node
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		writeStarted := time.Now()
		if err := correction.ReplaceSourceEdges(ctx, nil, nil, chunk); err != nil {
			return err
		}
		stats.write += time.Since(writeStarted)
		report.Nodes += int64(len(chunk))
		stats.rows += int64(len(chunk))
		chunk = chunk[:0]
		return nil
	}
	deriveStarted := time.Now()
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := graphPathRel(idx, file.FilePath)
		key := ""
		if row.TreeOID != "" {
			key = row.TreeOID + "\x00" + rel
		}
		outcome, known := memo.fingerprints[key]
		if !known || key == "" {
			src, ok := content(ctx, rel)
			if ok {
				_, derived, parsed, parsedOK := idx.extractionFingerprintsOfContent(filepath.Join(idx.rootPath, filepath.FromSlash(rel)), src)
				outcome = fileFingerprintOutcome{derived: derived, shape: nodeShape(parsed), ok: parsedOK && derived.complete()}
			}
			if key != "" {
				memo.fingerprints[key] = outcome
			}
		}
		if !outcome.ok || outcome.shape != nodeShape(own.GetFileNodes(file.FilePath)) {
			report.Unmatched++
			stats.unchanged++
			continue
		}
		stamped := *file
		stamped.Meta = make(map[string]any, len(file.Meta)+5)
		for k, v := range file.Meta {
			stamped.Meta[k] = v
		}
		setDerivedFingerprintMeta(stamped.Meta, outcome.derived)
		chunk = append(chunk, &stamped)
		stats.changed++
		if len(chunk) >= derivedCorrectionChunkRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	stats.derive = time.Since(deriveStarted) - stats.write
	return flush()
}

// graphPathRel is a graph file path relative to the repository root (the
// repository prefix removed).
func graphPathRel(idx *Indexer, path string) string {
	if prefix := idx.repoPrefix; prefix != "" && len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/' {
		return path[len(prefix)+1:]
	}
	return path
}

// setDerivedFingerprintMeta is stampDerivedFingerprints for one file row.
func setDerivedFingerprintMeta(meta map[string]any, fingerprints derivedFingerprints) {
	meta[sourceDerivedDeclFingerprintMeta] = fingerprints.declarations
	meta[sourceDerivedImportFingerprintMeta] = fingerprints.imports
	meta[sourceDerivedRuntimeFingerprintMeta] = fingerprints.runtime
	meta[sourceDerivedArtifactFingerprintMeta] = fingerprints.artifacts
	if fingerprints.hierarchy != "" {
		meta[sourceDerivedHierarchyFingerprintMeta] = fingerprints.hierarchy
	}
}

// capabilityRowKey renders one capability row for the comparison of a
// source's derived rows with the rows the generation holds.
func capabilityRowKey(e *graph.Edge) string {
	meta, _ := json.Marshal(e.Meta)
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%g\x00%s", e.Kind, e.To, e.FilePath, e.Line, e.Origin, e.Confidence, meta)
}

// sameCapabilityRows reports whether two row sets are equal as multisets.
func sameCapabilityRows(a, b []*graph.Edge) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, e := range a {
		count[capabilityRowKey(e)]++
	}
	for _, e := range b {
		key := capabilityRowKey(e)
		if count[key] == 0 {
			return false
		}
		count[key]--
	}
	return true
}

// correctCapabilityRows re-derives the capability rows the generation speaks
// for, over its composed view: the pass runs for the generation's files on a
// delta over that view, and every source it may have changed (its own, and
// the receiver callers a changed mutation set reaches) or the generation
// already records rows for is compared with the rows the generation holds —
// when the generation speaks for that source's rows (it covers their file or
// owns the source's out-edges). Only a source whose rows differ is written; a
// generation whose rows are all right writes nothing, so its correction moves
// the stamp and nothing else. A source the generation does not speak for
// would duplicate the rows below it; it is counted, not written.
func correctCapabilityRows(
	ctx context.Context, correction *store_sqlite.DerivedCorrection, own *store_sqlite.Store, env derivationEnv,
	row store_sqlite.ViewGeneration, memo *derivationMemo, report *DerivedCorrectionReport, stats *derivationGenerationStats,
) error {
	if env.view == nil {
		return errors.New("indexer: no view of the generation to re-derive over")
	}
	sourceSet := make(map[string]struct{})
	fileSet := make(map[string]struct{})
	for _, node := range own.AllNodes() {
		if node == nil || node.ID == "" || node.FilePath == "" {
			continue
		}
		fileSet[node.FilePath] = struct{}{}
		if node.Kind != graph.KindFile {
			sourceSet[node.ID] = struct{}{}
		}
	}
	for _, kind := range capabilityDerivedEdgeKinds {
		for edge := range own.EdgesByKind(kind) {
			if edge != nil && edge.From != "" {
				sourceSet[edge.From] = struct{}{}
			}
		}
	}
	if len(sourceSet) == 0 {
		return nil
	}
	key := derivationContentKey(row)
	derivation := memo.capability[key]
	if derivation != nil {
		for id := range sourceSet {
			if _, ok := derivation.computed[id]; !ok {
				derivation = nil
				break
			}
		}
	}
	if derivation != nil {
		stats.reused = true
	} else {
		viewStarted := time.Now()
		view, release, err := env.view(ctx)
		if err != nil {
			return err
		}
		defer release()
		stats.view = time.Since(viewStarted)
		deriveStarted := time.Now()
		dw := graph.NewDeltaWriter(view, nil)
		files := make([]string, 0, len(fileSet))
		for file := range fileSet {
			files = append(files, file)
		}
		sort.Strings(files)
		synthesizeCapabilityEdgesScoped(dw, nil, files...)
		if err := ctx.Err(); err != nil {
			return err
		}
		derivation = &capabilityDerivation{computed: make(map[string]struct{}), rows: make(map[string][]*graph.Edge)}
		derivation.touched = dw.TouchedSources(capabilityDerivedEdgeKinds...)
		ids := make([]string, 0, len(sourceSet)+len(derivation.touched))
		for id := range sourceSet {
			ids = append(ids, id)
		}
		ids = append(ids, derivation.touched...)
		for id, edges := range dw.GetOutEdgesByNodeIDs(ids) {
			for _, edge := range edges {
				if edge != nil && isCapabilityEdgeKind(edge.Kind) {
					derivation.rows[id] = append(derivation.rows[id], edge)
				}
			}
		}
		for _, id := range ids {
			derivation.computed[id] = struct{}{}
		}
		stats.derive = time.Since(deriveStarted)
		if key != "" {
			memo.capability[key] = derivation
		}
	}
	for _, id := range derivation.touched {
		sourceSet[id] = struct{}{}
	}
	diffStarted := time.Now()
	layer, err := graphview.NewGenerationLayer(own)
	if err != nil {
		return err
	}
	candidates := make([]string, 0, len(sourceSet))
	for id := range sourceSet {
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	stats.candidates = len(candidates)
	placed := own.GetNodesByIDs(candidates)
	held := own.GetOutEdgesByNodeIDs(candidates)
	var sources []string
	for _, id := range candidates {
		file := ""
		if n := placed[id]; n != nil {
			file = n.FilePath
		} else if rows := derivation.rows[id]; len(rows) > 0 {
			file = rows[0].FilePath
		}
		if !layer.OwnsOutEdges(id) && (file == "" || !layer.HasFile(file)) {
			report.Unowned++
			stats.unowned++
			continue
		}
		var current []*graph.Edge
		for _, edge := range held[id] {
			if edge != nil && isCapabilityEdgeKind(edge.Kind) {
				current = append(current, edge)
			}
		}
		if sameCapabilityRows(current, derivation.rows[id]) {
			stats.unchanged++
			continue
		}
		stats.changed++
		sources = append(sources, id)
	}
	stats.diff = time.Since(diffStarted)
	var chunkSources []string
	var chunkEdges []*graph.Edge
	flush := func() error {
		if len(chunkSources) == 0 {
			return nil
		}
		writeStarted := time.Now()
		if err := correction.ReplaceSourceEdges(ctx, chunkSources, chunkEdges, nil); err != nil {
			return err
		}
		stats.write += time.Since(writeStarted)
		report.Sources += len(chunkSources)
		report.EdgesMoved += int64(len(chunkEdges))
		stats.rows += int64(len(chunkEdges))
		chunkSources, chunkEdges = nil, nil
		return nil
	}
	for _, id := range sources {
		rows := derivation.rows[id]
		if len(chunkSources) > 0 && len(chunkEdges)+len(rows) > derivedCorrectionChunkRows {
			if err := flush(); err != nil {
				return err
			}
		}
		chunkSources = append(chunkSources, id)
		chunkEdges = append(chunkEdges, rows...)
	}
	return flush()
}
