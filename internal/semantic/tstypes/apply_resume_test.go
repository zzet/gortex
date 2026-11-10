package tstypes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

const resumeSHA = "0123456789abcdef0123456789abcdef01234567"

// resumeFixture spans several fact pages per phase and makes the later
// phases depend on the earlier ones across files: D<i> extends B<i> (supers),
// B<i>.run declares its return type (metas), and calls on D<i> resolve only
// through the inherited method and its return type (calls).
func resumeFixture(n int) map[string]string {
	files := make(map[string]string, 3*n)
	for i := 0; i < n; i++ {
		id := strings.NewReplacer("#", fmt.Sprintf("%03d", i))
		files[id.Replace("src/b#.ts")] = id.Replace(`export class R# { done(): void {} }
export class B# { run(): R# { return new R#(); } }
`)
		files[id.Replace("src/d#.ts")] = id.Replace(`import { B# } from "./b#";
export class D# extends B# { go(): void { this.run().done(); } }
`)
		files[id.Replace("src/u#.ts")] = id.Replace(`import { D# } from "./d#";
export function use#(d: D#): void { d.run().done(); d.go(); }
`)
	}
	return files
}

// resumeCut says where a run is interrupted: after afterPages apply pages,
// inside a page after afterWrites edge writes, or before parsing when
// beforeApply is set. The zero value runs uncut. spec replaces the default
// TypeScript engine. fault fails a page after it applied; a run with a fault
// may end without a result. unidentified runs a build that cannot name
// itself.
type resumeCut struct {
	afterPages   int
	afterWrites  int
	beforeApply  bool
	spec         *LangSpec
	fault        func(factPageStats) error
	unidentified bool
	reapplied    *int // counts pages applied again behind the cursor
	// gateCancelled parks the apply phase behind a gate that never opens
	// and cancels the pass while it waits there, after parsing finished.
	gateCancelled bool
}

const resumeTestVersion = "tstypes/test"

// cuttingStore interrupts the pass after a number of edge writes, so the cut
// lands inside a page rather than between two.
type cuttingStore struct {
	*store_sqlite.Store
	writes, cutAt int
	cancel        context.CancelFunc
}

func (s *cuttingStore) wrote() {
	s.writes++
	if s.writes == s.cutAt {
		s.cancel()
	}
}

func (s *cuttingStore) AddEdge(e *graph.Edge) { s.Store.AddEdge(e); s.wrote() }

func (s *cuttingStore) ReindexEdge(e *graph.Edge, oldTo string) {
	s.Store.ReindexEdge(e, oldTo)
	s.wrote()
}

func (s *cuttingStore) SetEdgeProvenance(e *graph.Edge, origin string) bool {
	changed := s.Store.SetEdgeProvenance(e, origin)
	s.wrote()
	return changed
}

// gateCancelStore cancels the pass when the apply phase asks for the resolve
// mutex, which it does right before it waits on the apply gate.
type gateCancelStore struct {
	*store_sqlite.Store
	cancel context.CancelFunc
}

func (s *gateCancelStore) ResolveMutex() *sync.Mutex {
	s.cancel()
	return s.Store.ResolveMutex()
}

// resumeRun is one daemon run against a persistent store: a fresh manager
// and provider, the store reopened from disk, and the pass cut after
// cutAfter apply pages (0 runs uncut). It returns the tstypes result and the
// number of pages the pass applied.
func resumeRun(t *testing.T, dbPath, root string, rs semantic.RepoEnrichState, cutAfter int) (*semantic.EnrichResult, int) {
	t.Helper()
	return resumeRunCut(t, dbPath, root, rs, resumeCut{afterPages: cutAfter})
}

func resumeRunCut(t *testing.T, dbPath, root string, rs semantic.RepoEnrichState, cut resumeCut) (*semantic.EnrichResult, int) {
	t.Helper()
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var g graph.Store = store
	if cut.afterWrites > 0 {
		g = &cuttingStore{Store: store, cutAt: cut.afterWrites, cancel: cancel}
	}
	if cut.gateCancelled {
		g = &gateCancelStore{Store: store, cancel: cancel}
	}
	return resumeRunOn(t, ctx, cancel, g, root, rs, cut)
}

func resumeRunOn(t *testing.T, ctx context.Context, cancel context.CancelFunc, g graph.Store, root string, rs semantic.RepoEnrichState, cut resumeCut) (*semantic.EnrichResult, int) {
	t.Helper()
	spec := cut.spec
	if spec == nil {
		spec = TypeScriptSpec()
	}
	p := NewProvider(spec, zap.NewNop())
	// A test binary carries no VCS stamp, so it is unidentified by default.
	p.resumeVersion = resumeTestVersion
	if cut.unidentified {
		p.resumeVersion = ""
	}
	p.pageFault = cut.fault
	// pages counts the apply pages a point can skip; alias pages resolve
	// into the pass's spool and re-run on every pass that reaches calls.
	seen, pages := 0, 0
	p.observePage = func(stats factPageStats) {
		seen++
		if stats.Class != classAliases {
			pages++
		}
		if stats.Reapply && cut.reapplied != nil {
			*cut.reapplied++
		}
		if cut.afterPages > 0 && seen == cut.afterPages {
			cancel() // the pass deadline expiring right after this page
		}
	}
	if cut.beforeApply {
		p.observeSpool = func(string) { cancel() }
	}
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	defer mgr.Close()
	mgr.RegisterProvider(p)
	var gate chan struct{}
	if cut.gateCancelled {
		gate = make(chan struct{}) // never opens
	}
	results, _, err := mgr.EnrichAll(g, map[string]string{"": root}, semantic.EnrichOptions{
		RepoState: map[string]semantic.RepoEnrichState{"": rs},
		Context:   ctx,
		ApplyGate: gate,
	})
	require.NoError(t, err)
	for _, r := range results {
		if r.Provider == p.Name() {
			return r, pages
		}
	}
	if cut.fault == nil {
		t.Fatalf("no %s result", p.Name())
	}
	return nil, pages
}

// memoryRun is resumeRunOn for a graph that is not reopened from disk.
func memoryRun(t *testing.T, g graph.Store, root string, rs semantic.RepoEnrichState, cut resumeCut) (*semantic.EnrichResult, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return resumeRunOn(t, ctx, cancel, g, root, rs, cut)
}

func resumeSnapshot(t *testing.T, dbPath string) []string {
	t.Helper()
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	return semanticSnapshot(store)
}

func newResumeStore(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "store.sqlite")
	store, root := buildSQLiteFixture(t, dbPath, files)
	require.NoError(t, store.Close())
	return dbPath, root
}

// A pass whose budget covers only part of the apply must still converge
// across runs: every run is cut after the same number of pages, as a
// deadline sized to the repository cuts it, and the runs together must
// reach the graph a single uncut pass produces, with the same coverage.
func TestStreamedApplyCutEveryRunConvergesAcrossRuns(t *testing.T) {
	files := resumeFixture(200)
	rs := semantic.RepoEnrichState{SHA: resumeSHA}

	wantDB, wantRoot := newResumeStore(t, files)
	want, totalPages := resumeRun(t, wantDB, wantRoot, rs, 0)
	require.False(t, want.Partial)
	require.Greater(t, want.EdgesAdded+want.EdgesConfirmed+want.NodesEnriched, 0, "fixture must exercise the engine")
	const cutAfter = 2
	require.Greater(t, totalPages, 3*cutAfter, "fixture must span many cut-sized slices")
	wantSnapshot := resumeSnapshot(t, wantDB)

	dbPath, root := newResumeStore(t, files)
	maxRuns := totalPages/cutAfter + 2
	var got *semantic.EnrichResult
	runs, applied := 0, 0
	for runs < maxRuns {
		runs++
		var pages int
		got, pages = resumeRun(t, dbPath, root, rs, cutAfter)
		applied += pages
		if !got.Partial {
			break
		}
	}
	require.False(t, got.Partial,
		"still partial after %d runs cut every %d of %d pages: cut runs make no forward progress", runs, cutAfter, totalPages)
	// Alias pages are not counted: they re-run on every pass reaching calls.
	assert.Equal(t, totalPages, applied, "every page is applied exactly once across the runs")
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath), "resumed runs must reach the uncut graph")
	assert.Equal(t, want.SymbolsTotal, got.SymbolsTotal)
	assert.Equal(t, want.SymbolsCovered, got.SymbolsCovered)
	assert.InDelta(t, want.CoveragePercent, got.CoveragePercent, 1e-9)

	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	marker, found, err := store.GetEnrichmentState("", TypeScriptSpec().ProviderName)
	require.NoError(t, err)
	require.True(t, found, "the completing run must write the completion marker")
	assert.Equal(t, resumeSHA, marker.IndexedSHA)
	assert.InDelta(t, want.CoveragePercent, marker.Coverage, 1e-9)
	_, found, err = store.GetEnrichmentResume("", TypeScriptSpec().ProviderName)
	require.NoError(t, err)
	assert.False(t, found, "a completed pass must remove its resume point")
}

// markReparsePending stamps the per-file ledger marker the indexer leaves on
// a file it re-parsed without enriching.
func markReparsePending(t *testing.T, s *store_sqlite.Store, path string) {
	t.Helper()
	var file *graph.Node
	for _, n := range s.GetFileNodes(path) {
		if n.Kind == graph.KindFile {
			file = n
		}
	}
	require.NotNil(t, file, path)
	if file.Meta == nil {
		file.Meta = map[string]any{}
	}
	file.Meta[graph.MetaReparsePendingEnrichment] = true
	s.AddBatch([]*graph.Node{file}, nil)
}

// reparseResumeFile does to the store what a scoped re-parse of one file
// does: every node anchored to it and every edge touching them is evicted,
// the file's extraction is written again, and the file is left on the
// ledger because it was not enriched.
func reparseResumeFile(t *testing.T, dbPath, root, path string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	require.NoError(t, err)
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	lang, ok := reg.DetectLanguage(path)
	require.True(t, ok)
	ext, ok := reg.GetByLanguage(lang)
	require.True(t, ok)
	res, err := ext.Extract(path, content)
	require.NoError(t, err)
	if res.Tree != nil {
		res.Tree.Close()
	}
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		s.EvictFile(path)
		s.AddBatch(res.Nodes, res.Edges)
		markReparsePending(t, s, path)
	})
	touchResumeFile(t, root, path)
}

// touchResumeFile moves a file's modification time forward, as the save
// that precedes a re-parse does.
func touchResumeFile(t *testing.T, root, path string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(path))
	info, err := os.Stat(abs)
	require.NoError(t, err)
	next := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(abs, next, next))
}

func resumePoint(t *testing.T, dbPath string) graph.EnrichmentResumePoint {
	t.Helper()
	var point graph.EnrichmentResumePoint
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		var found bool
		var err error
		point, found, err = s.GetEnrichmentResume("", TypeScriptSpec().ProviderName)
		require.NoError(t, err)
		require.True(t, found, "a cut run must leave a point")
	})
	return point
}

func pointAfter(later, earlier graph.EnrichmentResumePoint) bool {
	return later.Phase > earlier.Phase || (later.Phase == earlier.Phase && later.After > earlier.After)
}

func reparsePendingFiles(t *testing.T, dbPath string) []string {
	t.Helper()
	var pending []string
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		for _, n := range s.AllNodes() {
			if n != nil && n.Kind == graph.KindFile {
				if marked, _ := n.Meta[graph.MetaReparsePendingEnrichment].(bool); marked {
					pending = append(pending, n.FilePath)
				}
			}
		}
	})
	return pending
}

func editResumeStore(t *testing.T, dbPath string, edit func(*store_sqlite.Store)) {
	t.Helper()
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	edit(store)
}

// A recorded point is honoured only when the next pass is the same work over
// the same committed graph. Every other run must apply from the first page
// and still reach the uncut graph.
func TestStreamedApplyResumesOnlyUnderTrustedScope(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	const cutAfter = 2

	wantDB, wantRoot := newResumeStore(t, files)
	_, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	wantSnapshot := resumeSnapshot(t, wantDB)
	require.Greater(t, totalPages, 2*cutAfter)

	provider := TypeScriptSpec().ProviderName
	cases := []struct {
		name         string
		rs           semantic.RepoEnrichState
		env          string
		unidentified bool
		prepare      func(*testing.T, *store_sqlite.Store)
		resumes      bool
		reapplied    int // pages applied again behind the cursor
	}{
		{name: "same revision on a clean tree", rs: clean, resumes: true},
		{name: "revision moved", rs: semantic.RepoEnrichState{SHA: "fedcba9876543210fedcba9876543210fedcba98"}},
		{name: "dirty tree", rs: semantic.RepoEnrichState{SHA: resumeSHA, Dirty: true}},
		{name: "whole repo re-parsed this run", rs: semantic.RepoEnrichState{SHA: resumeSHA, Force: true}},
		{name: "files re-parsed this run are on the ledger", rs: semantic.RepoEnrichState{SHA: resumeSHA, Force: true, ReparsedFilesMarked: true}, resumes: true},
		{name: "unidentified build", rs: clean, unidentified: true},
		{name: "forced re-enrich", rs: clean, env: "1"},
		{name: "no revision", rs: semantic.RepoEnrichState{}},
		{name: "checkout-scoped pass", rs: semantic.RepoEnrichState{SHA: resumeSHA, CheckoutID: "checkout-1"}},
		{name: "engine version changed", rs: clean, prepare: func(t *testing.T, s *store_sqlite.Store) {
			point, found, err := s.GetEnrichmentResume("", provider)
			require.NoError(t, err)
			require.True(t, found)
			point.Version = "tstypes/0/older-build"
			require.NoError(t, s.SetEnrichmentResume(point))
		}},
		// The cut lands in the supers phase, past d000.ts: its supers facts
		// are applied again as one page, everything else resumes.
		{name: "re-parse awaiting enrichment", rs: clean, resumes: true, reapplied: 1, prepare: func(t *testing.T, s *store_sqlite.Store) {
			markReparsePending(t, s, "src/d000.ts")
		}},
		// A marked file still ahead of the cursor is applied once, by the
		// forward pages.
		{name: "re-parse ahead of the cursor", rs: clean, resumes: true, prepare: func(t *testing.T, s *store_sqlite.Store) {
			markReparsePending(t, s, "src/d099.ts")
		}},
		{name: "point recorded without a build identity", rs: clean, unidentified: true, prepare: func(t *testing.T, s *store_sqlite.Store) {
			point, found, err := s.GetEnrichmentResume("", provider)
			require.NoError(t, err)
			require.True(t, found)
			point.Version = ""
			require.NoError(t, s.SetEnrichmentResume(point))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, root := newResumeStore(t, files)
			cut, _ := resumeRun(t, dbPath, root, clean, cutAfter)
			require.True(t, cut.Partial)
			if tc.prepare != nil {
				editResumeStore(t, dbPath, func(s *store_sqlite.Store) { tc.prepare(t, s) })
			}
			if tc.env != "" {
				t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", tc.env)
			}
			got, pages := resumeRunCut(t, dbPath, root, tc.rs, resumeCut{unidentified: tc.unidentified})
			require.False(t, got.Partial)
			if tc.resumes {
				assert.Equal(t, totalPages-cutAfter+tc.reapplied, pages, "the committed pages must be skipped")
			} else {
				assert.Equal(t, totalPages, pages, "the pass must apply from the first page")
			}
			assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath))
		})
	}
}

// A run that must not resume discards the point before doing anything else,
// so a pass cut before it applies a page cannot leave a point the next run
// would honour over enrichment this run invalidated.
func TestStreamedApplyDiscardsUntrustedPointBeforeApplying(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	_, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	dbPath, root := newResumeStore(t, files)

	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	forced, pages := resumeRunCut(t, dbPath, root, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, resumeCut{beforeApply: true})
	require.True(t, forced.Partial)
	require.Zero(t, pages)
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		_, found, err := s.GetEnrichmentResume("", TypeScriptSpec().ProviderName)
		require.NoError(t, err)
		assert.False(t, found, "the re-parsed run must discard the earlier point")
	})
	got, pages := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	assert.Equal(t, totalPages, pages)
}

// The in-memory graph persists nothing, so every pass starts from the first
// page and nothing is recorded.
func TestStreamedApplyNeverResumesOnMemoryBackend(t *testing.T) {
	g, root := buildFixture(t, resumeFixture(100))
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	_, ok := any(g).(graph.EnrichmentResumeStore)
	require.False(t, ok, "the memory graph must not persist resume points")
	cut, cutPages := memoryRun(t, g, root, clean, resumeCut{afterPages: 2})
	require.True(t, cut.Partial)
	require.Equal(t, 2, cutPages)
	got, pages := memoryRun(t, g, root, clean, resumeCut{})
	require.False(t, got.Partial)
	fresh, freshRoot := buildFixture(t, resumeFixture(100))
	_, totalPages := memoryRun(t, fresh, freshRoot, clean, resumeCut{})
	assert.Equal(t, totalPages, pages)
}

// A page cut part-way through is not recorded: the next run applies it again
// from its first fact, so nothing it had not finished is ever skipped.
func TestStreamedApplyCutInsidePageReplaysThatPage(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	_, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	wantSnapshot := resumeSnapshot(t, wantDB)

	dbPath, root := newResumeStore(t, files)
	const cutAfterWrites = 45 // not a multiple of a page's facts
	var got *semantic.EnrichResult
	runs, cuts := 0, 0
	for runs < 2*totalPages {
		runs++
		got, _ = resumeRunCut(t, dbPath, root, clean, resumeCut{afterWrites: cutAfterWrites})
		if !got.Partial {
			break
		}
		cuts++
	}
	require.False(t, got.Partial, "still partial after %d runs", runs)
	require.Greater(t, cuts, 1, "the fixture must be cut inside pages more than once")
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath))
}

// Trait aliases resolve into the pass's temp spool, not the store, so a run
// resuming in the calls phase must resolve them again before applying calls.
func TestStreamedApplyResumedCallsSeeTraitAliases(t *testing.T) {
	var traits strings.Builder
	traits.WriteString("<?php\n")
	files := map[string]string{}
	for i := 0; i < 8; i++ {
		id := strings.NewReplacer("#", fmt.Sprintf("%03d", i))
		traits.WriteString(id.Replace("trait T# { public function fn(): void {} }\nclass C# { use T# { T#::fn as renamed; } }\n"))
	}
	files["a/traits.php"] = traits.String()
	for i := 0; i < 160; i++ {
		id := strings.NewReplacer("#", fmt.Sprintf("%03d", i), "%", fmt.Sprintf("%03d", i%8))
		files[id.Replace("u/u#.php")] = id.Replace("<?php\nclass App# { public function run(C% $c): void { $c->renamed(); } }\n")
	}
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	php := resumeCut{spec: PHPSpec()}

	wantDB, wantRoot := newResumeStore(t, files)
	want, totalPages := resumeRunCut(t, wantDB, wantRoot, clean, php)
	require.False(t, want.Partial)
	require.NotZero(t, want.EdgesAdded+want.EdgesConfirmed, "aliased calls must resolve")
	wantSnapshot := resumeSnapshot(t, wantDB)

	dbPath, root := newResumeStore(t, files)
	cut := php
	cut.afterPages = 2
	var got *semantic.EnrichResult
	for runs := 0; runs < totalPages+2; runs++ {
		got, _ = resumeRunCut(t, dbPath, root, clean, cut)
		if !got.Partial {
			break
		}
	}
	require.False(t, got.Partial)
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath))
}

// A file re-parsed between cut runs lost the enrichment its committed pages
// applied. Resumed runs keep their cursor and apply that file again in every
// phase behind it, so the runs still reach the uncut graph. Re-applying
// comes before the cursor in each phase, so a run makes progress only when
// its budget covers the re-parsed files' pages (one per phase here) plus a
// page. The marker stays until a pass completes; the indexer discharges it
// then (see TestRunDeferredEnrich_FullPassDischargesLedger).
func TestStreamedApplyResumeReappliesReparsedFiles(t *testing.T) {
	files := resumeFixture(100)
	// Sorts into the first page of every phase and has supers, metas and
	// calls facts. No other file references it, so re-parsing it loses
	// nothing a pass over its own facts cannot restore.
	const reparsed = "src/b000l.ts"
	files[reparsed] = `import { B000, R000 } from "./b000";
class L000 extends B000 { own(): R000 { return this.run(); } }
export function leaf(): void { new L000().own().done(); new L000().run().done(); }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	want, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, want.Partial)
	wantSnapshot := resumeSnapshot(t, wantDB)

	dbPath, root := newResumeStore(t, files)
	const reappliedPhases = 3 // supers, metas, calls
	const cutAfter = reappliedPhases + 1
	var got *semantic.EnrichResult
	runs, applied := 0, 0
	for runs < totalPages+2 {
		runs++
		var pages int
		got, pages = resumeRun(t, dbPath, root, clean, cutAfter)
		applied += pages
		if !got.Partial {
			break
		}
		reparseResumeFile(t, dbPath, root, reparsed)
		require.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath))
	}
	require.False(t, got.Partial, "still partial after %d runs", runs)
	require.Greater(t, runs, reappliedPhases, "the cursor must pass the re-parsed file in several phases")
	assert.Greater(t, applied, totalPages, "the re-parsed file's pages are applied again")
	assert.LessOrEqual(t, applied, totalPages+(runs-1)*reappliedPhases, "only the re-parsed file is applied again")
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath), "resumed runs must restore the re-parsed file")
	assert.Equal(t, want.SymbolsCovered, got.SymbolsCovered)
	assert.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath),
		"the provider leaves the ledger to the indexer")
}

// A build that carries neither a VCS revision nor a released version cannot
// tell itself from another such build, so it records no point.
func TestStreamedApplyUnidentifiedBuildRecordsNothing(t *testing.T) {
	dbPath, root := newResumeStore(t, resumeFixture(100))
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	cut, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, unidentified: true})
	require.True(t, cut.Partial)
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		_, found, err := s.GetEnrichmentResume("", TypeScriptSpec().ProviderName)
		require.NoError(t, err)
		assert.False(t, found)
	})
}

func TestResumeVersionNeedsABuildIdentity(t *testing.T) {
	setting := func(k, v string) debug.BuildSetting { return debug.BuildSetting{Key: k, Value: v} }
	cases := []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{name: "development build without VCS stamp", info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}},
		{name: "no version at all", info: debug.BuildInfo{}},
		{name: "development build at a revision",
			info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{setting("vcs.revision", "abc"), setting("vcs.modified", "true")}},
			want: "tstypes/1/(devel)/abc/true"},
		{name: "released module", info: debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, want: "tstypes/1/v1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resumeVersionFor(&tc.info))
		})
	}
}

// A page that fails for a reason other than the deadline fails the pass;
// the pages committed before it stay committed and the next run resumes
// after the last of them.
func TestStreamedApplyFailedPageResumesAfterLastGoodPage(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	_, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	wantSnapshot := resumeSnapshot(t, wantDB)

	dbPath, root := newResumeStore(t, files)
	const failAt = 3
	seen := 0
	failed, pages := resumeRunCut(t, dbPath, root, clean, resumeCut{fault: func(factPageStats) error {
		seen++
		if seen == failAt {
			return errors.New("injected page failure")
		}
		return nil
	}})
	require.Nil(t, failed, "a failed pass reports no result")
	require.Equal(t, failAt, pages)
	got, pages := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	assert.Equal(t, totalPages-(failAt-1), pages, "the failed page is applied again, the ones before it are not")
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath))
}

// failingResumeStore fails every point write from the failAt-th on.
type failingResumeStore struct {
	*store_sqlite.Store
	sets, failAt int
}

func (s *failingResumeStore) SetEnrichmentResume(p graph.EnrichmentResumePoint) error {
	s.sets++
	if s.sets >= s.failAt {
		return errors.New("injected point write failure")
	}
	return s.Store.SetEnrichmentResume(p)
}

// A point write that fails stops recording for the rest of the pass, which
// carries on applying; the next run resumes from the last point written.
func TestStreamedApplyStopsRecordingAfterPointWriteFails(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	_, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	wantSnapshot := resumeSnapshot(t, wantDB)

	dbPath, root := newResumeStore(t, files)
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	// Write 1 records the coverage walk, write 2 the first page.
	g := &failingResumeStore{Store: store, failAt: 3}
	ctx, cancel := context.WithCancel(context.Background())
	const cutAfter = 5
	cut, pages := resumeRunOn(t, ctx, cancel, g, root, clean, resumeCut{afterPages: cutAfter})
	cancel()
	require.NoError(t, store.Close())
	require.True(t, cut.Partial)
	require.Equal(t, cutAfter, pages)
	assert.Equal(t, 3, g.sets, "no point write is attempted after one fails")

	got, pages := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	assert.Equal(t, totalPages-1, pages, "only the page the last written point covers is skipped")
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath))
}

// A re-parse that drops the references into a file (eviction removes every
// edge touching the file's nodes; the indexer re-parks only some of them)
// takes with it what the type engine bound there from the file's importers.
// Resumed runs apply those importers again behind the cursor too.
func TestStreamedApplyResumeReappliesImportersOfEvictedFiles(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/a.ts"
	files[reparsed] = `export class Svc { run(): void {} }
export class Base { done(): void {} }
`
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); }
`
	files["src/c.ts"] = `import { Base } from "./a";
export class C extends Base { finish(): void { this.done(); } }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}

	dbPath, root := newResumeStore(t, files)
	const reappliedPhases = 3 // supers, metas, calls; the three files share a page
	const cutAfter = reappliedPhases + 1
	var got *semantic.EnrichResult
	runs := 0
	for runs < 64 {
		runs++
		got, _ = resumeRun(t, dbPath, root, clean, cutAfter)
		if !got.Partial {
			break
		}
		reparseResumeFile(t, dbPath, root, reparsed)
	}
	require.False(t, got.Partial, "still partial after %d runs", runs)
	require.Greater(t, runs, reappliedPhases, "the cursor must pass the importers in several phases")

	wantDB, wantRoot := newResumeStore(t, files)
	reparseResumeFile(t, wantDB, wantRoot, reparsed)
	want, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "resumed runs must restore what importers bound into the re-parsed file")
}

// An import that binds no single local name still names the module the file
// depends on. A file reaching a re-parsed file only through a namespace
// import, `import x = require()` or a CommonJS require binds its types by
// name, so resumed runs must find it among the importers and apply it again.
func TestStreamedApplyResumeReappliesPathOnlyImporters(t *testing.T) {
	for _, tc := range []struct {
		name, file, src string
	}{
		{name: "namespace import", file: "src/b.ts", src: `import * as a from "./a";
export function callB(s: a.Svc): void { s.run(); }
`},
		{name: "import require", file: "src/b.ts", src: `import a = require("./a");
export function callB(s: a.Svc): void { s.run(); }
`},
		{name: "await dynamic import", file: "src/b.ts", src: `export async function callB(): Promise<void> {
  const { Svc } = await import("./a");
  const s = new Svc();
  s.run();
}
`},
		{name: "dynamic import then", file: "src/b.ts", src: `export function callB(): void {
  import("./a").then(({ Svc }) => {
    const s = new Svc();
    s.run();
  });
}
`},
		{name: "CommonJS require", file: "src/b.js", src: `const { Svc } = require("./a");
function callB() { const s = new Svc(); s.run(); }
module.exports = { callB };
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := resumeFixture(100)
			const reparsed = "src/a.ts"
			files[reparsed] = `export class Svc { run(): void {} }
`
			files[tc.file] = tc.src
			clean := semantic.RepoEnrichState{SHA: resumeSHA}

			wantDB, wantRoot := newResumeStore(t, files)
			reparseResumeFile(t, wantDB, wantRoot, reparsed)
			want, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
			require.False(t, want.Partial)
			require.Positive(t, semanticEdgesBetween(t, wantDB, tc.file, reparsed),
				"the engine must bind the dependent into the re-parsed file")

			dbPath, root := newResumeStore(t, files)
			const reappliedPhases = 3 // supers, metas, calls; the files share a page
			var got *semantic.EnrichResult
			runs := 0
			for runs < 64 {
				runs++
				got, _ = resumeRun(t, dbPath, root, clean, reappliedPhases+1)
				if !got.Partial {
					break
				}
				reparseResumeFile(t, dbPath, root, reparsed)
			}
			require.False(t, got.Partial, "still partial after %d runs", runs)
			require.Greater(t, runs, reappliedPhases, "the cursor must pass the dependent in several phases")
			assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath),
				"resumed runs must restore what the dependent bound into the re-parsed file")
		})
	}
}

// semanticEdgesBetween counts the type-engine edges from nodes of one file
// to nodes of another.
func semanticEdgesBetween(t *testing.T, dbPath, fromFile, toFile string) int {
	t.Helper()
	count := 0
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		inFile := func(id, file string) bool {
			n := s.GetNode(id)
			return n != nil && n.FilePath == file
		}
		for _, e := range s.AllEdges() {
			if e == nil || e.Meta == nil {
				continue
			}
			if src, _ := e.Meta["semantic_source"].(string); src == "" {
				continue
			}
			if inFile(e.From, fromFile) && inFile(e.To, toFile) {
				count++
			}
		}
	})
	return count
}

// The re-apply is paid once per ledger: a resumed run that re-applied the
// re-parsed files records that its points cover them, and the next cut run
// over the same ledger re-applies nothing and spends its budget moving the
// cursor.
func TestStreamedApplyReappliesALedgerOnce(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/b000l.ts"
	files[reparsed] = `import { B000, R000 } from "./b000";
class L000 extends B000 { own(): R000 { return this.run(); } }
export function leaf(): void { new L000().own().done(); }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	reparseResumeFile(t, dbPath, root, reparsed)

	first := 0
	cut, _ = resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 4, reapplied: &first})
	require.True(t, cut.Partial)
	require.Positive(t, first, "the first resumed run re-applies the re-parsed file")
	covered := resumePoint(t, dbPath)

	again := 0
	cut, pages := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, reapplied: &again})
	require.True(t, cut.Partial)
	assert.Zero(t, again, "the same ledger is not re-applied twice")
	assert.Equal(t, 2, pages)
	assert.True(t, pointAfter(resumePoint(t, dbPath), covered), "the run moves the cursor")

	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	wantDB, wantRoot := newResumeStore(t, files)
	reparseResumeFile(t, wantDB, wantRoot, reparsed)
	_, _ = resumeRun(t, wantDB, wantRoot, clean, 0)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath))
}

// Re-applied pages never move the cursor, so a resumed run cut while
// re-applying would leave the point with the old ledger digest, and every
// later run would repeat the same re-apply and be cut at the same place. The
// run discards the point instead; the next run applies from the first page
// and records the new digest, and runs that are each cut after one page
// still converge to the uncut graph.
func TestStreamedApplyCutInsideReapplyStartsFresh(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/b000l.ts"
	files[reparsed] = `import { B000, R000 } from "./b000";
class L000 extends B000 { own(): R000 { return this.run(); } }
export function leaf(): void { new L000().own().done(); }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	reparseResumeFile(t, wantDB, wantRoot, reparsed)
	want, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, want.Partial)

	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	before := resumePoint(t, dbPath)
	reparseResumeFile(t, dbPath, root, reparsed)

	reapplied := 0
	cut, _ = resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 1, reapplied: &reapplied})
	require.True(t, cut.Partial)
	require.Equal(t, 1, reapplied, "the run is cut on a re-applied page")
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
		_, found, err := s.GetEnrichmentResume("", TypeScriptSpec().ProviderName)
		require.NoError(t, err)
		assert.False(t, found, "a run cut before it moved its cursor discards the point")
	})

	reapplied = 0
	cut, _ = resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, reapplied: &reapplied})
	require.True(t, cut.Partial)
	assert.Zero(t, reapplied, "the next run applies from the first page")
	fresh := resumePoint(t, dbPath)
	assert.Equal(t, before.Phase, fresh.Phase, "the next run started from the first page")
	assert.Equal(t, before.After, fresh.After, "the next run started from the first page")
	assert.NotEqual(t, before.LedgerDigest, fresh.LedgerDigest, "its points cover the new ledger")

	var got *semantic.EnrichResult
	runs := 0
	for runs < totalPages+2 {
		runs++
		got, _ = resumeRun(t, dbPath, root, clean, 1)
		if !got.Partial {
			break
		}
	}
	require.False(t, got.Partial, "still partial after %d runs cut every page", runs)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "the runs must reach the uncut graph")
	assert.Equal(t, want.SymbolsCovered, got.SymbolsCovered)
}

// A run that ends before its apply phase starts has re-applied nothing, so
// the point it resumed still describes the store and must survive: a restart
// or a parse that uses up a small budget would otherwise throw away every
// committed page. The next run re-applies against the same point and the
// runs converge to the uncut graph.
func TestStreamedApplyExitBeforeApplyKeepsThePoint(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/b000l.ts"
	files[reparsed] = `import { B000, R000 } from "./b000";
class L000 extends B000 { own(): R000 { return this.run(); } }
export function leaf(): void { new L000().own().done(); }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	reparseResumeFile(t, wantDB, wantRoot, reparsed)
	want, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, want.Partial)

	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	before := resumePoint(t, dbPath)
	reparseResumeFile(t, dbPath, root, reparsed)

	for _, exit := range []struct {
		name string
		cut  resumeCut
	}{
		{name: "cut while parsing", cut: resumeCut{beforeApply: true}},
		{name: "cut waiting on the apply gate", cut: resumeCut{gateCancelled: true}},
	} {
		reapplied := 0
		exit.cut.reapplied = &reapplied
		cut, pages := resumeRunCut(t, dbPath, root, clean, exit.cut)
		require.True(t, cut.Partial, exit.name)
		require.Zero(t, pages, "%s: the run applies nothing", exit.name)
		require.Zero(t, reapplied, exit.name)
		assert.Equal(t, before, resumePoint(t, dbPath), "%s: the point survives", exit.name)
	}

	reapplied := 0
	cut, _ = resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 4, reapplied: &reapplied})
	require.True(t, cut.Partial)
	assert.Positive(t, reapplied, "the next run still re-applies against the point")
	assert.True(t, pointAfter(resumePoint(t, dbPath), before), "and moves its cursor")

	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath))
}

// A re-parsed file most of the repository imports would be replayed at
// about the cost of a whole pass, so the pass starts from the first page
// instead; it covers the ledger, and the run after it resumes without
// re-applying anything.
func TestStreamedApplyHighFanInLedgerStartsFresh(t *testing.T) {
	files := resumeFixture(20)
	const hub = "src/a.ts"
	files[hub] = `export class Hub { ping(): void {} }
`
	for i := 0; i < 100; i++ {
		id := strings.NewReplacer("#", fmt.Sprintf("%03d", i))
		files[id.Replace("src/h#.ts")] = id.Replace(`import { Hub } from "./a";
export class H# extends Hub { go(): void { this.ping(); } }
`)
	}
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	reparseResumeFile(t, wantDB, wantRoot, hub)
	want, totalPages := resumeRun(t, wantDB, wantRoot, clean, 0)

	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	before := resumePoint(t, dbPath)
	reparseResumeFile(t, dbPath, root, hub)

	reapplied := 0
	cut, _ = resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, reapplied: &reapplied})
	require.True(t, cut.Partial)
	assert.Zero(t, reapplied, "the importers are not replayed")
	fresh := resumePoint(t, dbPath)
	assert.Equal(t, before.Phase, fresh.Phase, "the run started from the first page")
	assert.Equal(t, before.After, fresh.After, "the run started from the first page")
	assert.NotEqual(t, before.LedgerDigest, fresh.LedgerDigest, "its points cover the new ledger")

	reapplied = 0
	got, pages := resumeRunCut(t, dbPath, root, clean, resumeCut{reapplied: &reapplied})
	require.False(t, got.Partial)
	assert.Zero(t, reapplied)
	assert.Equal(t, totalPages-2, pages, "the next run resumes")
	assert.Equal(t, want.SymbolsCovered, got.SymbolsCovered, "the fresh run walked coverage again")
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath))
}

// An engine whose import hints do not name every dependent of a file
// (same-package or wildcard references bind with no import) cannot bound the
// re-apply to the importers it can see, so a changed ledger sends the pass
// back to the first page even though its files carry import hints.
func TestStreamedApplyLedgerWithoutAuthoritativeImportsStartsFresh(t *testing.T) {
	spec := *TypeScriptSpec()
	spec.ImportHintsAuthoritative = false
	require.NotNil(t, spec.Imports, "the engine still records import hints")
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	_, totalPages := resumeRunCut(t, wantDB, wantRoot, clean, resumeCut{spec: &spec})

	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{afterPages: 2, spec: &spec})
	require.True(t, cut.Partial)
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) { markReparsePending(t, s, "src/d000.ts") })

	reapplied := 0
	got, pages := resumeRunCut(t, dbPath, root, clean, resumeCut{spec: &spec, reapplied: &reapplied})
	require.False(t, got.Partial)
	assert.Zero(t, reapplied)
	assert.Equal(t, totalPages, pages, "the pass applies from the first page")
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath))
}

// Only TypeScript records an import for every direct cross-file reference.
// The other engines see direct references that carry no import naming the
// file — same-package or same-namespace names, wildcard imports,
// module-qualified access — so they must not bound a resumed re-apply to the
// importers.
func TestImportHintsAuthoritativeOnlyForTypeScript(t *testing.T) {
	for _, p := range DefaultProviders(zap.NewNop()) {
		want := p.spec.ProviderName == TypeScriptSpec().ProviderName
		assert.Equal(t, want, p.spec.ImportHintsAuthoritative, p.spec.ProviderName)
		if p.spec.ImportHintsAuthoritative {
			assert.NotNil(t, p.spec.Imports, "%s: authoritative hints need an import decoder", p.spec.ProviderName)
		}
	}
}
