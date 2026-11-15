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
	"go.uber.org/zap/zaptest/observer"

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
	logger       *zap.Logger // defaults to a no-op logger
	// gateCancelled parks the apply phase behind a gate that never opens
	// and cancels the pass while it waits there, after parsing finished.
	gateCancelled bool
	// duringStaging runs on the pass's store after the pass decided where
	// to start and before it parses.
	duringStaging func(graph.Store)
	// afterFileScan runs on the pass's store once the pass listed its files.
	afterFileScan func(graph.Store)
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
	logger := cut.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	p := NewProvider(spec, logger)
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
		if cut.afterPages > 0 && seen == cut.afterPages {
			cancel() // the pass deadline expiring right after this page
		}
	}
	if cut.beforeApply {
		p.observeSpool = func(string) { cancel() }
	}
	if cut.duringStaging != nil {
		p.observeSpool = func(string) { cut.duringStaging(g) }
	}
	if cut.afterFileScan != nil {
		p.observeFiles = func() { cut.afterFileScan(g) }
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
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) { reparseResumeFileIn(t, s, root, path) })
}

// reparseResumeFileIn is reparseResumeFile on an open store.
func reparseResumeFileIn(t *testing.T, s *store_sqlite.Store, root, path string) {
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
	s.EvictFile(path)
	s.AddBatch(res.Nodes, res.Edges)
	markReparsePending(t, s, path)
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
	const (
		mayNotResume  = "this pass may not resume (dirty tree, forced re-enrich, or a re-parse this run the ledger does not cover)"
		noIdentity    = "this build has no identity"
		reparsedSince = "a file was re-parsed, added or removed since it was recorded"
	)
	cases := []struct {
		name         string
		rs           semantic.RepoEnrichState
		env          string
		unidentified bool
		prepare      func(*testing.T, *store_sqlite.Store)
		resumes      bool
		refusal      string // the logged reason a stored point was discarded
	}{
		{name: "same revision on a clean tree", rs: clean, resumes: true},
		{name: "revision moved", rs: semantic.RepoEnrichState{SHA: "fedcba9876543210fedcba9876543210fedcba98"}, refusal: "recorded at another revision"},
		{name: "dirty tree", rs: semantic.RepoEnrichState{SHA: resumeSHA, Dirty: true}, refusal: mayNotResume},
		{name: "whole repo re-parsed this run", rs: semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, refusal: mayNotResume},
		{name: "files re-parsed this run are on the ledger", rs: semantic.RepoEnrichState{SHA: resumeSHA, Force: true, ReparsedFilesMarked: true}, resumes: true},
		{name: "unidentified build", rs: clean, unidentified: true, refusal: noIdentity},
		{name: "forced re-enrich", rs: clean, env: "1", refusal: mayNotResume},
		{name: "no revision", rs: semantic.RepoEnrichState{}},
		{name: "checkout-scoped pass", rs: semantic.RepoEnrichState{SHA: resumeSHA, CheckoutID: "checkout-1"}},
		{name: "engine version changed", rs: clean, refusal: "recorded by another build", prepare: func(t *testing.T, s *store_sqlite.Store) {
			point, found, err := s.GetEnrichmentResume("", provider)
			require.NoError(t, err)
			require.True(t, found)
			point.Version = "tstypes/0/older-build"
			require.NoError(t, s.SetEnrichmentResume(point))
		}},
		// A file re-parsed since the point was recorded, behind the cursor
		// or ahead of it: no bounded set names every file whose facts bound
		// into it, so the pass applies from the first page.
		{name: "re-parse behind the cursor", rs: clean, refusal: reparsedSince, prepare: func(t *testing.T, s *store_sqlite.Store) {
			markReparsePending(t, s, "src/d000.ts")
		}},
		{name: "re-parse ahead of the cursor", rs: clean, refusal: reparsedSince, prepare: func(t *testing.T, s *store_sqlite.Store) {
			markReparsePending(t, s, "src/d099.ts")
		}},
		{name: "point recorded without a build identity", rs: clean, unidentified: true, refusal: noIdentity, prepare: func(t *testing.T, s *store_sqlite.Store) {
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
			core, logs := observer.New(zap.InfoLevel)
			got, pages := resumeRunCut(t, dbPath, root, tc.rs, resumeCut{unidentified: tc.unidentified, logger: zap.New(core)})
			require.False(t, got.Partial)
			var reasons []any
			for _, e := range logs.FilterMessage(resumeRefusedMsg).All() {
				reasons = append(reasons, e.ContextMap()["reason"])
			}
			if tc.refusal == "" {
				assert.Empty(t, reasons, "nothing was refused")
			} else {
				assert.Equal(t, []any{tc.refusal}, reasons, "a refused point is logged once with its cause")
			}
			if tc.resumes {
				assert.Equal(t, totalPages-cutAfter, pages, "the committed pages must be skipped")
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
// applied, and so did the files whose facts bound into it. The next run
// does not resume over the changed ledger: it applies from the first page
// and records points over the new ledger, which the run after it resumes.
// The marker stays until a pass completes; the indexer discharges it then
// (see TestRunDeferredEnrich_FullPassDischargesLedger).
func TestStreamedApplyRestartsAfterReparse(t *testing.T) {
	files := resumeFixture(100)
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
	const cutAfter = 4
	cut, _ := resumeRun(t, dbPath, root, clean, cutAfter)
	require.True(t, cut.Partial)
	cut, _ = resumeRun(t, dbPath, root, clean, cutAfter)
	require.True(t, cut.Partial)
	before := resumePoint(t, dbPath)
	reparseResumeFile(t, dbPath, root, reparsed)
	require.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath))

	cut, _ = resumeRun(t, dbPath, root, clean, cutAfter)
	require.True(t, cut.Partial)
	fresh := resumePoint(t, dbPath)
	assert.True(t, pointAfter(before, fresh), "the run after the re-parse starts from the first page")
	assert.NotEqual(t, before.LedgerDigest, fresh.LedgerDigest, "its points cover the new ledger")

	got, pages := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	assert.Less(t, pages, totalPages, "the run over an unchanged ledger resumes")
	assert.Equal(t, wantSnapshot, resumeSnapshot(t, dbPath), "the runs must restore the re-parsed file")
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

// A run that ends before its apply phase starts has applied nothing, so the
// point it resumed still describes the store and must survive: a restart or
// a parse that uses up a small budget would otherwise throw away every
// committed page. The next run resumes the same point and the runs converge
// to the uncut graph.
func TestStreamedApplyExitBeforeApplyKeepsThePoint(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	wantDB, wantRoot := newResumeStore(t, files)
	want, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, want.Partial)

	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	before := resumePoint(t, dbPath)

	for _, exit := range []struct {
		name string
		cut  resumeCut
	}{
		{name: "cut while parsing", cut: resumeCut{beforeApply: true}},
		{name: "cut waiting on the apply gate", cut: resumeCut{gateCancelled: true}},
	} {
		cut, pages := resumeRunCut(t, dbPath, root, clean, exit.cut)
		require.True(t, cut.Partial, exit.name)
		require.Zero(t, pages, "%s: the run applies nothing", exit.name)
		assert.Equal(t, before, resumePoint(t, dbPath), "%s: the point survives", exit.name)
	}

	cut, _ = resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)
	assert.True(t, pointAfter(resumePoint(t, dbPath), before), "the next run resumes and moves the cursor")

	got, _ := resumeRun(t, dbPath, root, clean, 0)
	require.False(t, got.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath))
}

// The C# and Kotlin engines bind cross-file types by repo-unique name, so
// removing a file can change what other files' facts bind to although no
// edge pointed into it and nothing was re-parsed. A pass must not resume a
// point recorded before the removal.
func TestStreamedApplyRefusesResumeAfterFileRemoved(t *testing.T) {
	files := resumeFixture(100)
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	dbPath, root := newResumeStore(t, files)
	cut, _ := resumeRun(t, dbPath, root, clean, 2)
	require.True(t, cut.Partial)

	const removed = "src/u050.ts" // nothing imports it
	require.NoError(t, os.Remove(filepath.Join(root, filepath.FromSlash(removed))))
	editResumeStore(t, dbPath, func(s *store_sqlite.Store) { s.EvictFile(removed) })
	require.Empty(t, reparsePendingFiles(t, dbPath))

	core, logs := observer.New(zap.InfoLevel)
	got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core)})
	require.False(t, got.Partial)
	assert.Equal(t, []any{"a file was re-parsed, added or removed since it was recorded"}, refusalReasons(logs))
}

// The repo-unique name lookup reads every file node of the pass's
// languages, including those the pass does not analyze: vendored files,
// files over the size cap and files missing on disk. A re-parse of one of
// them must refuse a resume like a re-parse of any other.
func TestStreamedApplyRefusesResumeAfterUnanalyzedFileReparsed(t *testing.T) {
	const vendored = "src/vendor/v.ts"
	clean := semantic.RepoEnrichState{SHA: resumeSHA}
	for _, tc := range []struct {
		name, path string
		prepare    func(t *testing.T, root string)
	}{
		{name: "vendored", path: vendored},
		{name: "over the size cap", path: "src/u050.ts", prepare: func(t *testing.T, root string) {
			require.NoError(t, os.Truncate(filepath.Join(root, "src", "u050.ts"), maxFileBytes+1))
		}},
		{name: "missing on disk", path: "src/u050.ts", prepare: func(t *testing.T, root string) {
			require.NoError(t, os.Remove(filepath.Join(root, "src", "u050.ts")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := resumeFixture(100)
			files[vendored] = "export class V { run(): void {} }\n"
			dbPath, root := newResumeStore(t, files)
			if tc.prepare != nil {
				tc.prepare(t, root)
			}
			cut, _ := resumeRun(t, dbPath, root, clean, 2)
			require.True(t, cut.Partial)

			editResumeStore(t, dbPath, func(s *store_sqlite.Store) {
				markReparsePending(t, s, tc.path)
				for _, n := range s.GetFileNodes(tc.path) {
					if n.Kind == graph.KindFile {
						n.Meta[graph.MetaReparseEpoch] = "e1"
						s.AddBatch([]*graph.Node{n}, nil)
					}
				}
			})

			core, logs := observer.New(zap.InfoLevel)
			got, _ := resumeRunCut(t, dbPath, root, clean, resumeCut{logger: zap.New(core)})
			require.False(t, got.Partial)
			assert.Equal(t, []any{"a file was re-parsed, added or removed since it was recorded"}, refusalReasons(logs))
		})
	}
}
