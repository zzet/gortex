package tstypes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

func resumeIndexer(store graph.Store) *indexer.Indexer {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return indexer.New(store, reg, config.Default().Index, zap.NewNop())
}

// indexedResumeStore indexes files into a new store the way a track does,
// resolver included, and closes it.
func indexedResumeStore(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	}
	dbPath := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	_, err = resumeIndexer(store).Index(root)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	return dbPath, root
}

// reparseThroughIndexer saves content to one file and re-indexes it the
// way a save does: incoming references are parked on stubs, the file is
// evicted and re-extracted, the parked references are re-resolved, and —
// with enrichment deferred — the file goes on the ledger.
func reparseThroughIndexer(t *testing.T, dbPath, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	withResumeIndexer(t, dbPath, root, semantic.Config{Enabled: true}, nil, func(idx *indexer.Indexer) {
		_, err := idx.IncrementalReindexPaths(root, []string{abs})
		require.NoError(t, err)
	})
}

// withResumeIndexer runs fn with a fresh indexer over the store at dbPath,
// as one daemon run does, with the default type engines registered. wrap,
// when set, replaces the store the indexer writes through.
func withResumeIndexer(t *testing.T, dbPath, root string, cfg semantic.Config, wrap func(*store_sqlite.Store) graph.Store, fn func(*indexer.Indexer)) {
	t.Helper()
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	var g graph.Store = store
	if wrap != nil {
		g = wrap(store)
	}
	runResumeIndexer(g, root, cfg, fn)
}

// runResumeIndexer is withResumeIndexer on an open store.
func runResumeIndexer(g graph.Store, root string, cfg semantic.Config, fn func(*indexer.Indexer)) {
	idx := resumeIndexer(g)
	idx.SetRootPath(root)
	mgr := semantic.NewManager(cfg, zap.NewNop())
	defer mgr.Close()
	for _, p := range DefaultProviders(zap.NewNop()) {
		mgr.RegisterProvider(p)
	}
	idx.SetSemanticManager(mgr)
	fn(idx)
}

// resumeClearFailStore fails the repository-wide discard of resume points
// and lets every other write through.
type resumeClearFailStore struct {
	*store_sqlite.Store
	failed int
}

func (s *resumeClearFailStore) ClearRepoEnrichmentResume(string) error {
	s.failed++
	return errors.New("injected: discard resume points failed")
}

// cutUntilPast runs cut passes until the stored point is in the calls phase
// past file, so every phase behind the cursor has applied file's page.
func cutUntilPast(t *testing.T, dbPath, root string, rs semantic.RepoEnrichState, file string) graph.EnrichmentResumePoint {
	t.Helper()
	for runs := 0; runs < 64; runs++ {
		got, _ := resumeRun(t, dbPath, root, rs, 2)
		require.True(t, got.Partial, "the fixture must not complete before the cursor passes %s", file)
		if point := resumePoint(t, dbPath); stagedFactPhase(point.Phase) == stagedCalls && point.After > file {
			return point
		}
	}
	t.Fatalf("the cursor never passed %s in the calls phase", file)
	return graph.EnrichmentResumePoint{}
}

// finishCutRuns runs passes cut every two pages until one completes.
func finishCutRuns(t *testing.T, dbPath, root string, rs semantic.RepoEnrichState) {
	t.Helper()
	for runs := 0; runs < 256; runs++ {
		if got, _ := resumeRun(t, dbPath, root, rs, 2); !got.Partial {
			return
		}
	}
	t.Fatal("cut runs never completed")
}

// semanticLinesFor keeps the snapshot lines naming file.
func semanticLinesFor(snapshot []string, file string) []string {
	var lines []string
	for _, line := range snapshot {
		if strings.Contains(line, file) {
			lines = append(lines, line)
		}
	}
	return lines
}

// Saves through the indexer's real re-parse path between cut runs: the
// re-parsed file's importers hold calls and an extends the type engine
// bound into it, and each save changes the targets' signatures so the
// indexer parks those references and re-resolves them instead of carrying
// them over. Each save lands once the cursor has passed the importers; the
// runs that follow must reach what an uncut pass reaches over the same
// history.
func TestStreamedApplyResumeMatchesUncutPassAcrossSaves(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/a.ts"
	// The file declares run and done twice, so a reference parked on its
	// bare name is ambiguous to the structural resolver; only the receiver
	// type binds it.
	saves := [3]string{`export class Svc { run(): void {} }
export class Base { done(): void {} }
export class Other { run(): void {} done(): void {} }
`, `export class Svc { run(n?: number): void {} }
export class Base { done(n?: number): void {} }
export class Other { run(): void {} done(): void {} }
`}
	saves[2] = saves[0]
	files[reparsed] = saves[0]
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); }
`
	files["src/c.ts"] = `import { Base } from "./a";
export class C extends Base { finish(): void { this.done(); } }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}

	dbPath, root := indexedResumeStore(t, files)
	for _, save := range saves[1:] {
		cutUntilPast(t, dbPath, root, clean, "src/c.ts")
		reparseThroughIndexer(t, dbPath, root, reparsed, save)
		require.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath), "the re-parse must leave the file on the ledger")
	}
	finishCutRuns(t, dbPath, root, clean)

	// The oracle: the same history — enriched, the same saves — then one
	// pass from the first page.
	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	for _, save := range saves[1:] {
		reparseThroughIndexer(t, wantDB, wantRoot, reparsed, save)
	}
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "resumed runs must reach the uncut graph")
}

// A save re-enriched in place must discard the repository's resume points,
// since in-place enrichment does not restore the edges its importers' type
// facts bound into the file. When that discard fails and the later marker
// writes succeed, a restart at the same clean revision must still not
// resume past those dependents on the old cursor, including one no import
// names: the runs that follow, each a fresh process, must reach the graph
// an uncut pass reaches over the same history.
func TestStreamedApplyFailedResumeDiscardMatchesUncutPass(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/a.ts"
	// The save adds the method b.ts and c.ts already call. Only a pass that
	// applies their calls again binds it; the in-place re-enrich of a.ts
	// does not. c.ts reaches it as a member inherited through n.ts, so no
	// import of c.ts names a.ts.
	saves := [2]string{`export class Svc { run(): void {} }
`, `export class Svc { run(): void {} go(): void {} }
`}
	files[reparsed] = saves[0]
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); s.go(); }
`
	files["src/n.ts"] = `import { Svc } from "./a";
export class Mid extends Svc {}
`
	files["src/c.ts"] = `import { Mid } from "./n";
export function callC(m: Mid): void { m.go(); }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	onWatch := semantic.Config{Enabled: true, EnrichOnWatch: true}
	saveInPlace := func(dbPath, root string, wrap func(*store_sqlite.Store) graph.Store) {
		abs := filepath.Join(root, filepath.FromSlash(reparsed))
		require.NoError(t, os.WriteFile(abs, []byte(saves[1]), 0o644))
		withResumeIndexer(t, dbPath, root, onWatch, wrap, func(idx *indexer.Indexer) {
			require.NoError(t, idx.IndexFile(abs))
		})
	}

	dbPath, root := indexedResumeStore(t, files)
	stale := cutUntilPast(t, dbPath, root, clean, "src/c.ts")
	var failing *resumeClearFailStore
	saveInPlace(dbPath, root, func(s *store_sqlite.Store) graph.Store {
		failing = &resumeClearFailStore{Store: s}
		return failing
	})
	require.Positive(t, failing.failed, "the in-place re-enrich must try to discard the points")
	require.Equal(t, stale, resumePoint(t, dbPath), "the failed discard leaves the old point behind")
	require.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath), "the failed discard keeps the file on the ledger")
	// Restart: a fresh process runs the next pass uncut.
	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)

	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	saveInPlace(wantDB, wantRoot, nil)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "runs after the failed discard must reach the uncut graph")
}

// Deleting a file re-derives its surviving importers from source with no
// write to them (deletion_importers.go). An importer already on the ledger,
// and covered by the stored point's ledger digest, loses the type facts its
// pages applied while its path, mtime and size stay the same. Its new epoch
// must still keep the runs that follow from resuming behind the old cursor,
// so they reach the graph an uncut pass reaches over the same history.
func TestStreamedApplyImporterForcedThroughReparseMatchesUncutPass(t *testing.T) {
	files := resumeFixture(100)
	const importer = "src/b.ts"
	// run is declared twice, so only the receiver type binds s.run().
	files["src/a.ts"] = `export class Svc { run(): void {} }
export class Other { run(): void {} }
`
	files["src/gone.ts"] = `export function helper(): number { return 1; }
`
	files[importer] = `import { Svc } from "./a";
import { helper } from "./gone";
export function callB(s: Svc): number { s.run(); return helper(); }
`
	saved := files[importer] + "// saved\n"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	deleteGone := func(dbPath, root string) {
		abs := filepath.Join(root, "src", "gone.ts")
		info, err := os.Stat(abs)
		require.NoError(t, err)
		require.NoError(t, os.Remove(abs))
		withResumeIndexer(t, dbPath, root, semantic.Config{Enabled: true}, nil, func(idx *indexer.Indexer) {
			// The running daemon's mtime ledger names the file it deletes.
			idx.SetFileMtimes(map[string]int64{"src/gone.ts": info.ModTime().UnixNano()})
			_, err := idx.IncrementalReindexPaths(root, []string{abs})
			require.NoError(t, err)
		})
	}

	dbPath, root := indexedResumeStore(t, files)
	// A save puts the importer on the ledger; the first pass then covers
	// it, and the cursor moves past it in every phase.
	reparseThroughIndexer(t, dbPath, root, importer, saved)
	require.Equal(t, []string{importer}, reparsePendingFiles(t, dbPath))
	cutUntilPast(t, dbPath, root, clean, importer)
	before := semanticLinesFor(resumeSnapshot(t, dbPath), importer)
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(importer)))
	require.NoError(t, err)

	deleteGone(dbPath, root)
	after, err := os.Stat(filepath.Join(root, filepath.FromSlash(importer)))
	require.NoError(t, err)
	require.Equal(t, info.ModTime(), after.ModTime(), "the importer is not written")
	require.Equal(t, info.Size(), after.Size())
	require.Equal(t, []string{importer}, reparsePendingFiles(t, dbPath))
	require.NotEqual(t, before, semanticLinesFor(resumeSnapshot(t, dbPath), importer),
		"the deletion must re-derive the importer and drop facts its pages applied")
	finishCutRuns(t, dbPath, root, clean)

	wantDB, wantRoot := indexedResumeStore(t, files)
	reparseThroughIndexer(t, wantDB, wantRoot, importer, saved)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	deleteGone(wantDB, wantRoot)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "resumed runs must reach the uncut graph")
}

// markerWriteLostStore drops the indexer's marker-only node writes, as a
// crash after a re-parse's fact write and before its marker write would.
type markerWriteLostStore struct {
	*store_sqlite.Store
	dropped int
}

func (s *markerWriteLostStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if len(edges) == 0 && len(nodes) > 0 {
		markerOnly := true
		for _, n := range nodes {
			if n == nil || n.Kind != graph.KindFile {
				markerOnly = false
				break
			}
		}
		if markerOnly {
			s.dropped++
			return
		}
	}
	s.Store.AddBatch(nodes, edges)
}

// A crash after a re-parse replaced a file's facts must not leave the file
// off the ledger while the old point survives: the restarted runs would
// resume past its importers at the same clean revision. The re-parse here
// runs to the end with every marker-only write lost; the runs that follow,
// each a fresh process, must reach the graph an uncut pass reaches over the
// same history.
func TestStreamedApplyResumeAfterLostMarkerWriteMatchesUncutPass(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/a.ts"
	// As in TestStreamedApplyResumeMatchesUncutPassAcrossSaves: only the
	// receiver type binds the references, and the save changes the targets'
	// signatures so the indexer parks them.
	saved := `export class Svc { run(n?: number): void {} }
export class Base { done(n?: number): void {} }
export class Other { run(): void {} done(): void {} }
`
	files[reparsed] = `export class Svc { run(): void {} }
export class Base { done(): void {} }
export class Other { run(): void {} done(): void {} }
`
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); }
`
	files["src/c.ts"] = `import { Base } from "./a";
export class C extends Base { finish(): void { this.done(); } }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}

	dbPath, root := indexedResumeStore(t, files)
	stale := cutUntilPast(t, dbPath, root, clean, "src/c.ts")
	abs := filepath.Join(root, filepath.FromSlash(reparsed))
	require.NoError(t, os.WriteFile(abs, []byte(saved), 0o644))
	withResumeIndexer(t, dbPath, root, semantic.Config{Enabled: true},
		func(s *store_sqlite.Store) graph.Store { return &markerWriteLostStore{Store: s} },
		func(idx *indexer.Indexer) {
			_, err := idx.IncrementalReindexPaths(root, []string{abs})
			require.NoError(t, err)
		})
	require.Equal(t, stale, resumePoint(t, dbPath), "a deferred save leaves the point behind")
	assert.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath),
		"the re-parse's own fact write must put the file on the ledger")
	finishCutRuns(t, dbPath, root, clean)

	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	reparseThroughIndexer(t, wantDB, wantRoot, reparsed, saved)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "runs after the lost marker write must reach the uncut graph")
}

// discardCrashStore fails the discard of resume points and drops the
// indexer's marker-only node writes after it, as a crash right after the
// failed delete would.
type discardCrashStore struct {
	markerWriteLostStore
	crashed bool
}

func (s *discardCrashStore) ClearRepoEnrichmentResume(string) error {
	s.crashed = true
	return errors.New("injected: discard resume points failed")
}

func (s *discardCrashStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if s.crashed {
		s.markerWriteLostStore.AddBatch(nodes, edges)
		return
	}
	s.Store.AddBatch(nodes, edges)
}

// As TestStreamedApplyFailedResumeDiscardMatchesUncutPass, but the process
// dies right after the failed discard, before any later marker write. The
// in-place re-enriched file must already be on the ledger, so the restarted
// runs do not resume past c.ts, which no import of a.ts names.
func TestStreamedApplyCrashAfterFailedResumeDiscardMatchesUncutPass(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/a.ts"
	saves := [2]string{`export class Svc { run(): void {} }
`, `export class Svc { run(): void {} go(): void {} }
`}
	files[reparsed] = saves[0]
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); s.go(); }
`
	files["src/n.ts"] = `import { Svc } from "./a";
export class Mid extends Svc {}
`
	files["src/c.ts"] = `import { Mid } from "./n";
export function callC(m: Mid): void { m.go(); }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	onWatch := semantic.Config{Enabled: true, EnrichOnWatch: true}
	saveInPlace := func(dbPath, root string, wrap func(*store_sqlite.Store) graph.Store) {
		abs := filepath.Join(root, filepath.FromSlash(reparsed))
		require.NoError(t, os.WriteFile(abs, []byte(saves[1]), 0o644))
		withResumeIndexer(t, dbPath, root, onWatch, wrap, func(idx *indexer.Indexer) {
			require.NoError(t, idx.IndexFile(abs))
		})
	}

	dbPath, root := indexedResumeStore(t, files)
	stale := cutUntilPast(t, dbPath, root, clean, "src/c.ts")
	var crashing *discardCrashStore
	saveInPlace(dbPath, root, func(s *store_sqlite.Store) graph.Store {
		crashing = &discardCrashStore{markerWriteLostStore: markerWriteLostStore{Store: s}}
		return crashing
	})
	require.True(t, crashing.crashed, "the in-place re-enrich must try to discard the points")
	require.Equal(t, stale, resumePoint(t, dbPath), "the failed discard leaves the old point behind")
	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)

	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	saveInPlace(wantDB, wantRoot, nil)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "runs after the crash must reach the uncut graph")
}

// factWriteCrashStore drops the node write that re-adds path's file node
// and every node and mtime write after it, as a crash between a re-parse's
// eviction and its fact write would.
type factWriteCrashStore struct {
	*store_sqlite.Store
	path    string
	crashed bool
}

func (s *factWriteCrashStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if !s.crashed {
		for _, n := range nodes {
			if n != nil && n.Kind == graph.KindFile && n.FilePath == s.path {
				s.crashed = true
				break
			}
		}
	}
	if s.crashed {
		return
	}
	s.Store.AddBatch(nodes, edges)
}

func (s *factWriteCrashStore) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if s.crashed {
		return nil
	}
	return s.Store.BulkSetFileMtimes(repoPrefix, mtimes)
}

func (s *factWriteCrashStore) ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if s.crashed {
		return nil
	}
	return s.Store.ReplaceFileMtimes(repoPrefix, mtimes)
}

// warmRestart reconciles the store the way a restarted daemon does: it
// trusts the persisted mtimes and re-indexes what the census reports.
func warmRestart(t *testing.T, dbPath, root string) []string {
	t.Helper()
	var changed []string
	withResumeIndexer(t, dbPath, root, semantic.Config{Enabled: true}, nil, func(idx *indexer.Indexer) {
		idx.SetFileMtimes(idx.Graph().(graph.FileMtimeReader).LoadFileMtimes(""))
		var deleted []string
		var err error
		changed, deleted, err = idx.ChangedSinceMtimes(root)
		require.NoError(t, err)
		var paths []string
		for _, rel := range append(append([]string(nil), changed...), deleted...) {
			paths = append(paths, filepath.Join(root, filepath.FromSlash(rel)))
		}
		if len(paths) > 0 {
			_, err = idx.IncrementalReindexPaths(root, paths)
			require.NoError(t, err)
		}
	})
	return changed
}

// The importer of a deleted file is evicted and re-derived with no write to
// it. A crash between the eviction and the fact write leaves it with no
// nodes, and so off the ledger, while the old point and its mtime receipt
// survive. The restart must re-derive it, and the runs that follow must
// reach the graph an uncut pass reaches over the same history.
func TestStreamedApplyResumeAfterCrashInForcedImporterReparseMatchesUncutPass(t *testing.T) {
	files := resumeFixture(100)
	const importer = "src/b.ts"
	files["src/a.ts"] = `export class Svc { run(): void {} }
export class Other { run(): void {} }
`
	files["src/gone.ts"] = `export function helper(): number { return 1; }
`
	files[importer] = `import { Svc } from "./a";
import { helper } from "./gone";
export function callB(s: Svc): number { s.run(); return helper(); }
`
	saved := files[importer] + "// saved\n"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	deleteGone := func(dbPath, root string, wrap func(*store_sqlite.Store) graph.Store) {
		abs := filepath.Join(root, "src", "gone.ts")
		require.NoError(t, os.Remove(abs))
		withResumeIndexer(t, dbPath, root, semantic.Config{Enabled: true}, wrap, func(idx *indexer.Indexer) {
			// The running daemon's mtime ledger names every indexed file.
			mtimes := idx.Graph().(graph.FileMtimeReader).LoadFileMtimes("")
			require.Contains(t, mtimes, importer)
			idx.SetFileMtimes(mtimes)
			_, err := idx.IncrementalReindexPaths(root, []string{abs})
			require.NoError(t, err)
		})
	}

	dbPath, root := indexedResumeStore(t, files)
	reparseThroughIndexer(t, dbPath, root, importer, saved)
	cutUntilPast(t, dbPath, root, clean, importer)
	var crashing *factWriteCrashStore
	deleteGone(dbPath, root, func(s *store_sqlite.Store) graph.Store {
		crashing = &factWriteCrashStore{Store: s, path: importer}
		return crashing
	})
	require.True(t, crashing.crashed, "the deletion must re-derive the importer")
	assert.Contains(t, warmRestart(t, dbPath, root), importer, "the restart must re-derive the importer")
	finishCutRuns(t, dbPath, root, clean)

	wantDB, wantRoot := indexedResumeStore(t, files)
	reparseThroughIndexer(t, wantDB, wantRoot, importer, saved)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	deleteGone(wantDB, wantRoot, nil)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "runs after the crash must reach the uncut graph")
}

// inheritanceFixture adds a re-parsed file whose save adds a method that
// c.ts reaches only as a member inherited through n.ts: no import of c.ts
// names a.ts. b.ts calls it through a direct import.
func inheritanceFixture() (map[string]string, [2]string) {
	files := resumeFixture(100)
	saves := [2]string{`export class Svc { run(): void {} }
`, `export class Svc { run(): void {} go(): void {} }
`}
	files["src/a.ts"] = saves[0]
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); s.go(); }
`
	files["src/n.ts"] = `import { Svc } from "./a";
export class Mid extends Svc {}
`
	files["src/c.ts"] = `import { Mid } from "./n";
export function callC(m: Mid): void { m.go(); }
`
	return files, saves
}

// inheritedCall is the edge only a pass that applies c.ts after the save
// binds.
const inheritedCall = "e|src/c.ts::callC|src/a.ts::Svc.go|calls|2|ast_resolved|0.9500|EXTRACTED|typescript-types|<nil>"

// A deferred save with no fault: the re-parsed file goes on the ledger and
// the next run, uncut, starts behind a cursor that has passed c.ts, which
// depends on the file only through inheritance. It must reach the graph an
// uncut pass reaches over the same history.
func TestStreamedApplyResumeAfterSaveMatchesUncutPassThroughInheritance(t *testing.T) {
	files, saves := inheritanceFixture()
	const reparsed = "src/a.ts"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}

	dbPath, root := indexedResumeStore(t, files)
	cutUntilPast(t, dbPath, root, clean, "src/c.ts")
	reparseThroughIndexer(t, dbPath, root, reparsed, saves[1])
	require.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath))
	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)

	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	reparseThroughIndexer(t, wantDB, wantRoot, reparsed, saves[1])
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	require.Contains(t, resumeSnapshot(t, wantDB), inheritedCall, "the uncut pass binds the inherited call")
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "resumed runs must reach the uncut graph")
}

// enrichCrashStore stops persisting anything once the indexer first acts
// on the resume points after an in-place re-enrich (a marker-only node
// write or a discard of the points), as a crash right after the in-place
// enrichment wrote its facts would.
type enrichCrashStore struct {
	markerWriteLostStore
	crashed bool
}

func (s *enrichCrashStore) ClearRepoEnrichmentResume(string) error {
	s.crashed = true
	return nil
}

func (s *enrichCrashStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if !s.crashed {
		before := s.dropped
		s.markerWriteLostStore.AddBatch(nodes, edges)
		s.crashed = s.dropped > before
	}
}

// A crash after a save's in-place enrichment wrote its facts, before any
// write that records what the re-enrich means for the resume points. The
// restarted runs must not resume past c.ts on the old cursor.
func TestStreamedApplyCrashAfterInPlaceEnrichMatchesUncutPass(t *testing.T) {
	files, saves := inheritanceFixture()
	const reparsed = "src/a.ts"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	onWatch := semantic.Config{Enabled: true, EnrichOnWatch: true}
	saveInPlace := func(dbPath, root string, wrap func(*store_sqlite.Store) graph.Store) {
		abs := filepath.Join(root, filepath.FromSlash(reparsed))
		require.NoError(t, os.WriteFile(abs, []byte(saves[1]), 0o644))
		withResumeIndexer(t, dbPath, root, onWatch, wrap, func(idx *indexer.Indexer) {
			require.NoError(t, idx.IndexFile(abs))
		})
	}

	dbPath, root := indexedResumeStore(t, files)
	stale := cutUntilPast(t, dbPath, root, clean, "src/c.ts")
	var crashing *enrichCrashStore
	saveInPlace(dbPath, root, func(s *store_sqlite.Store) graph.Store {
		crashing = &enrichCrashStore{markerWriteLostStore: markerWriteLostStore{Store: s}}
		return crashing
	})
	require.True(t, crashing.crashed, "the in-place re-enrich must reach the resume points")
	require.Equal(t, stale, resumePoint(t, dbPath), "the crash leaves the old point behind")
	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)

	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	saveInPlace(wantDB, wantRoot, nil)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "runs after the crash must reach the uncut graph")
}

func refusalReasons(logs *observer.ObservedLogs) []any {
	var reasons []any
	for _, e := range logs.FilterMessage(resumeRefusedMsg).All() {
		reasons = append(reasons, e.ContextMap()["reason"])
	}
	return reasons
}

// A covered file re-parsed after a pass decided to resume, while it staged,
// loses facts the pages behind the cursor applied. The pass must recheck the
// ledger before it skips those pages and apply from the first page instead,
// reaching the graph an uncut pass reaches over the same history.
func TestStreamedApplyRechecksResumeAfterStaging(t *testing.T) {
	files := resumeFixture(100)
	// u000.ts calls into it through d000.ts; both sort before the cursor.
	const reparsed = "src/b000.ts"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}

	dbPath, root := newResumeStore(t, files)
	cutUntilPast(t, dbPath, root, clean, "src/u000.ts")
	core, logs := observer.New(zap.InfoLevel)
	got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core), duringStaging: func(g graph.Store) {
		s, ok := g.(*store_sqlite.Store)
		require.True(t, ok)
		reparseResumeFileIn(t, s, root, reparsed)
	}})
	require.False(t, got.Partial)
	require.Len(t, logs.FilterMessage("tstypes: apply resuming after committed pages").All(), 1,
		"the pass decides to resume before the re-parse")
	assert.Equal(t, []any{"a file was re-parsed while the pass staged"}, refusalReasons(logs))

	wantDB, wantRoot := newResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	reparseResumeFile(t, wantDB, wantRoot, reparsed)
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "the rechecked pass must reach the uncut graph")
}

// A watch save enriched in place while a type pass stages: the indexer
// discards the resume points and takes the file off the ledger before the
// pass, cut later, records a point of its own. A restart at the same clean
// revision must not resume from that point.
func TestStreamedApplyPointRecordedAcrossInPlaceSaveNotResumedAfterRestart(t *testing.T) {
	files, saves := inheritanceFixture()
	const reparsed = "src/a.ts"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	onWatch := semantic.Config{Enabled: true, EnrichOnWatch: true}
	saveInPlace := func(g graph.Store, root string) {
		abs := filepath.Join(root, filepath.FromSlash(reparsed))
		require.NoError(t, os.WriteFile(abs, []byte(saves[1]), 0o644))
		runResumeIndexer(g, root, onWatch, func(idx *indexer.Indexer) {
			require.NoError(t, idx.IndexFile(abs))
		})
	}

	dbPath, root := indexedResumeStore(t, files)
	stale := cutUntilPast(t, dbPath, root, clean, "src/c.ts")
	cut, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, duringStaging: func(g graph.Store) {
		saveInPlace(g, root)
		_, found, err := g.(graph.EnrichmentResumeStore).GetEnrichmentResume("", TypeScriptSpec().ProviderName)
		require.NoError(t, err)
		require.False(t, found, "the in-place save discards the points")
	}})
	require.True(t, cut.Partial)
	require.NotEqual(t, stale, resumePoint(t, dbPath), "the cut pass records a point after the discard")
	require.Empty(t, reparsePendingFiles(t, dbPath), "the in-place save takes the file off the ledger")
	// Restart: a fresh process runs the next pass uncut.
	core, logs := observer.New(zap.InfoLevel)
	got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core)})
	require.False(t, got.Partial)
	assert.Equal(t, []any{"a file was re-parsed, added or removed since it was recorded"}, refusalReasons(logs))

	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	withResumeIndexer(t, wantDB, wantRoot, onWatch, nil, func(idx *indexer.Indexer) {
		abs := filepath.Join(wantRoot, filepath.FromSlash(reparsed))
		require.NoError(t, os.WriteFile(abs, []byte(saves[1]), 0o644))
		require.NoError(t, idx.IndexFile(abs))
	})
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	require.Contains(t, resumeSnapshot(t, wantDB), inheritedCall, "the uncut pass binds the inherited call")
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "runs after the restart must reach the uncut graph")
}

// A skip that replaces a covered file's facts with a synthetic node (here a
// binary payload) while a type pass stages: the pass, cut later, records a
// point over the ledger it opened with. A restart at the same clean
// revision must not resume from that point.
func TestStreamedApplyPointRecordedAcrossSkipEvictionNotResumedAfterRestart(t *testing.T) {
	const skipped = "src/b000.ts"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	dbPath, root := indexedResumeStore(t, resumeFixture(100))
	cut, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, duringStaging: func(g graph.Store) {
		abs := filepath.Join(root, filepath.FromSlash(skipped))
		require.NoError(t, os.WriteFile(abs, []byte("export class B000 {}\x00\n"), 0o644))
		runResumeIndexer(g, root, semantic.Config{Enabled: true}, func(idx *indexer.Indexer) {
			require.NoError(t, idx.IndexFile(abs))
		})
	}})
	require.True(t, cut.Partial)
	resumePoint(t, dbPath) // the cut pass records a point after the skip

	core, logs := observer.New(zap.InfoLevel)
	got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core)})
	require.False(t, got.Partial)
	assert.Equal(t, []any{"a file was re-parsed, added or removed since it was recorded"}, refusalReasons(logs))
}

// A file removed while a type pass stages: the pass, cut later, records a
// point over the ledger it opened with, which still names the file. A
// restart at the same clean revision must not resume from that point.
func TestStreamedApplyPointRecordedAcrossFileRemovalNotResumedAfterRestart(t *testing.T) {
	const removed = "src/u050.ts" // nothing imports it
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	dbPath, root := indexedResumeStore(t, resumeFixture(100))
	evicted := 0
	cut, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, duringStaging: func(g graph.Store) {
		abs := filepath.Join(root, filepath.FromSlash(removed))
		if os.Remove(abs) != nil {
			return
		}
		runResumeIndexer(g, root, semantic.Config{Enabled: true}, func(idx *indexer.Indexer) {
			evicted, _ = idx.EvictFile(abs)
		})
	}})
	require.Positive(t, evicted, "the removal evicts the file")
	require.True(t, cut.Partial)
	resumePoint(t, dbPath) // the cut pass records a point after the removal

	core, logs := observer.New(zap.InfoLevel)
	got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core)})
	require.False(t, got.Partial)
	assert.Equal(t, []any{"a file was re-parsed, added or removed since it was recorded"}, refusalReasons(logs))
}

// A file indexed while a pass lists its files, once the listing has passed
// it, is neither staged nor applied by that pass. The points the pass
// records must not cover it: the next pass refuses them and applies from
// the first page, reaching the graph an uncut pass reaches.
func TestStreamedApplyFileAddedDuringFileScanRefusesResume(t *testing.T) {
	const added = "src/a.ts" // sorts before every fixture file
	const content = `import { B000 } from "./b000";
export class A extends B000 { go(): void { this.run().done(); } }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	add := func(g graph.Store, root string) {
		abs := filepath.Join(root, filepath.FromSlash(added))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
		runResumeIndexer(g, root, semantic.Config{Enabled: true}, func(idx *indexer.Indexer) {
			require.NoError(t, idx.IndexFile(abs))
		})
	}

	dbPath, root := indexedResumeStore(t, resumeFixture(100))
	cut, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, afterFileScan: func(g graph.Store) { add(g, root) }})
	require.True(t, cut.Partial)
	require.Equal(t, []string{added}, reparsePendingFiles(t, dbPath), "the added file goes on the ledger")
	resumePoint(t, dbPath) // the cut pass records a point after the add

	core, logs := observer.New(zap.InfoLevel)
	got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core)})
	require.False(t, got.Partial)
	assert.Equal(t, []any{"a file was re-parsed, added or removed since it was recorded"}, refusalReasons(logs))

	wantDB, wantRoot := indexedResumeStore(t, resumeFixture(100))
	editResumeStore(t, wantDB, func(s *store_sqlite.Store) { add(s, wantRoot) })
	want, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "the refused pass must reach the uncut graph")
}
