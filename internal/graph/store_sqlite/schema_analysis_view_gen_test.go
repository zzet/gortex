package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The whole-graph analysis cache's payload-view-generation axis.
//
// Before v25 the cache was addressed by one global active slot plus
// build_revision, and build_revision is analysisMutationRevision — one
// process-local atomic on the storeCore every generation handle shares, bumped
// only when a graph mutation commits. Two analyses computed over two payload
// view generations with no intervening mutation therefore carried the
// identical revision, the second overwrote the first's active pointer, and
// either handle read whichever one landed last. These cases pin the column
// that replaces that guess.

const analysisViewGenFormatVersion = uint32(77)

// legacyAnalysisActiveGenerationBody is the v24 shape of the active pointer:
// one global slot, no generation axis. Kept here rather than derived so the
// fixture is what the shipped v24 build actually wrote.
const legacyAnalysisActiveGenerationBody = ` (
    slot          INTEGER PRIMARY KEY CHECK (slot = 1),
    generation_id INTEGER NOT NULL UNIQUE
        REFERENCES analysis_generations(generation_id) ON DELETE RESTRICT
);`

func openAnalysisViewGenStore(t *testing.T) *Store {
	t.Helper()
	store, err := openPristine(t, filepath.Join(t.TempDir(), "analysis_view_gen.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// buildAnalysisAt builds and activates one minimal analysis generation through
// a handle pinned to viewGen, and returns its id.
func buildAnalysisAt(t *testing.T, store *Store, viewGen int64, prefix string) (*Store, int64) {
	t.Helper()
	handle := store.AtGeneration(viewGen)
	if handle == nil {
		t.Fatalf("AtGeneration(%d) = nil", viewGen)
	}
	if handle.ViewGeneration() != viewGen {
		t.Fatalf("handle generation = %d, want %d", handle.ViewGeneration(), viewGen)
	}
	return handle, buildMinimalAnalysisGeneration(t, handle, prefix, 2, true)
}

func analysisGenerationViewGen(t *testing.T, store *Store, generationID int64) int64 {
	t.Helper()
	var viewGen int64
	if err := store.db.QueryRow(
		`SELECT view_gen FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&viewGen); err != nil {
		t.Fatalf("read analysis %d view_gen: %v", generationID, err)
	}
	return viewGen
}

// TestAnalysisGenerationsDoNotCollideAcrossViewGenerations is the case that
// forced the analysis cache onto its own view-generation axis: two analyses
// over two payload view generations of the same store, built with no graph
// mutation between them so their build_revision is byte-identical, stay
// separately addressable. Each handle reads its own and refuses the other's.
//
// Revert-red: drop the `a.view_gen = ?` predicate from
// ensureAnalysisGenerationReadableLocked, or the `view_gen = ?` predicate from
// LoadActiveAnalysisHeader, and the cross-generation reads below become hits.
func TestAnalysisGenerationsDoNotCollideAcrossViewGenerations(t *testing.T) {
	store := openAnalysisViewGenStore(t)

	baseRevision := store.AnalysisMutationRevision()
	baseHandle, baseAnalysis := buildAnalysisAt(t, store, baseViewGeneration, "base")
	overlayHandle, overlayAnalysis := buildAnalysisAt(t, store, 7, "overlay")

	if baseAnalysis == overlayAnalysis {
		t.Fatalf("analysis ids collided: %d", baseAnalysis)
	}
	// The premise: no graph mutation ran, so the coarse revision the old CAS
	// keyed on is the same for both. The column is the only thing telling them
	// apart.
	if got := store.AnalysisMutationRevision(); got != baseRevision {
		t.Fatalf("analysis mutation revision moved to %d; the collision premise needs it unchanged", got)
	}
	if got := analysisGenerationViewGen(t, store, baseAnalysis); got != baseViewGeneration {
		t.Fatalf("base analysis stamped view_gen %d, want %d", got, baseViewGeneration)
	}
	if got := analysisGenerationViewGen(t, store, overlayAnalysis); got != 7 {
		t.Fatalf("overlay analysis stamped view_gen %d, want 7", got)
	}

	// Both pointers survive: activating the overlay's analysis did not displace
	// the base's.
	baseHeader, found, err := baseHandle.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
	if err != nil || !found {
		t.Fatalf("base LoadActiveAnalysisHeader = %v, %v", found, err)
	}
	if baseHeader.GenerationID != baseAnalysis {
		t.Fatalf("base handle served analysis %d, want %d", baseHeader.GenerationID, baseAnalysis)
	}
	overlayHeader, found, err := overlayHandle.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
	if err != nil || !found {
		t.Fatalf("overlay LoadActiveAnalysisHeader = %v, %v", found, err)
	}
	if overlayHeader.GenerationID != overlayAnalysis {
		t.Fatalf("overlay handle served analysis %d, want %d", overlayHeader.GenerationID, overlayAnalysis)
	}

	// Neither handle can read the other's rows through the bounded query gate.
	if _, err := baseHandle.AnalysisNodeMetrics(overlayAnalysis, []string{"overlay-node"}); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("base handle read the overlay's analysis: err = %v, want %v", err, graph.ErrAnalysisGenerationInactive)
	}
	if _, err := overlayHandle.AnalysisNodeMetrics(baseAnalysis, []string{"base-node"}); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("overlay handle read the base's analysis: err = %v, want %v", err, graph.ErrAnalysisGenerationInactive)
	}

	// And each handle still reads its own.
	metrics, err := overlayHandle.AnalysisNodeMetrics(overlayAnalysis, []string{"overlay-node"})
	if err != nil || len(metrics) != 1 || metrics[0].NodeID != "overlay-node" {
		t.Fatalf("overlay handle lost its own analysis: %v, %v", metrics, err)
	}
	metrics, err = baseHandle.AnalysisNodeMetrics(baseAnalysis, []string{"base-node"})
	if err != nil || len(metrics) != 1 || metrics[0].NodeID != "base-node" {
		t.Fatalf("base handle lost its own analysis: %v, %v", metrics, err)
	}
}

// TestAnalysisGenerationWritesRefuseAnotherViewsGeneration pins the write half
// of the axis: a handle cannot append to, seal or activate an analysis
// generation another payload view is building.
func TestAnalysisGenerationWritesRefuseAnotherViewsGeneration(t *testing.T) {
	store := openAnalysisViewGenStore(t)
	overlay := store.AtGeneration(9)
	revision := store.AnalysisMutationRevision()

	header := graph.AnalysisGenerationHeader{
		FormatVersion: analysisViewGenFormatVersion, NodeCount: 1, CommunityCount: 1,
		PageRankMax: 1, AuthorityMax: 1, HubMax: 1,
	}
	generationID, accepted, err := overlay.BeginAnalysisGeneration(revision, header)
	if err != nil || !accepted {
		t.Fatalf("BeginAnalysisGeneration = %d, %v, %v", generationID, accepted, err)
	}

	_, err = store.AppendAnalysisCommunities(revision, generationID, []graph.AnalysisCommunitySummary{{ID: "c", Size: 1}})
	if err == nil {
		t.Fatal("the base handle appended to an overlay generation's analysis")
	}
	if _, err := store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentCommunities, 1); err == nil {
		t.Fatal("the base handle sealed an overlay generation's analysis component")
	}
	if _, err := store.ActivateAnalysisGeneration(revision, generationID); err == nil {
		t.Fatal("the base handle activated an overlay generation's analysis")
	}
	if err := store.AbortAnalysisGeneration(generationID); err == nil {
		t.Fatal("the base handle aborted an overlay generation's analysis")
	}

	// The owning handle still works.
	if _, err := overlay.AppendAnalysisCommunities(revision, generationID, []graph.AnalysisCommunitySummary{{ID: "c", Size: 1}}); err != nil {
		t.Fatalf("owning handle append: %v", err)
	}
}

// TestAnalysisGenerationWriteSurvivesPublishedGenerationSeal pins the seam the
// axis exposed: an analysis computed over a PUBLISHED payload generation must
// still be cacheable. Payload writes through such a handle are refused by its
// seal (refuseSealedPayloadWrite), and the analysis cache is a derived plane
// that references no payload row, so its transactions run on the base handle.
//
// Revert-red: point beginAnalysisWrite at s.beginWrite() and this fails with
// ErrPayloadGenerationSealed.
func TestAnalysisGenerationWriteSurvivesPublishedGenerationSeal(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}

	// The generation is now sealed for payload writes: the gate every payload
	// transaction passes through refuses it.
	published := store.AtGeneration(generationID)
	if err := published.refuseSealedPayloadWrite(); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("published generation write gate = %v, want %v; the seal premise is gone", err, ErrPayloadGenerationSealed)
	}

	analysisID := buildMinimalAnalysisGeneration(t, published, "published", 1, true)
	header, found, err := published.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
	if err != nil || !found || header.GenerationID != analysisID {
		t.Fatalf("published generation could not cache its analysis: %v, %v, %+v", found, err, header)
	}
}

// TestRetirePayloadGenerationSweepsItsAnalysisRows proves the retirement
// fan-out: retiring a payload generation removes exactly that generation's
// analysis rows — active pointer, manifest and every child row — and leaves
// every other view's analysis standing.
func TestRetirePayloadGenerationSweepsItsAnalysisRows(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	// Every analysis is built after the last graph mutation: durable
	// invalidation is deliberately global (one coarse mutation revision serves
	// every generation), so a mutation between these builds would clear both
	// views' pointers and the case would prove nothing about the sweep.
	baseAnalysis := buildMinimalAnalysisGeneration(t, store, "base", 3, true)
	retiredAnalysis := buildMinimalAnalysisGeneration(t, handle, "retired", 4, true)
	// A second, non-active analysis in the same view, so the sweep is proven to
	// collect history and not just the pointer's target.
	staleAnalysis := buildMinimalAnalysisGeneration(t, handle, "stale", 2, false)
	if err := handle.AbortAnalysisGeneration(staleAnalysis); err != nil {
		t.Fatalf("AbortAnalysisGeneration: %v", err)
	}

	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotDirty,
		ExpectedRouteEpoch: 1,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("un-route: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}

	// Nothing of the retired view's analysis survives.
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = ?`, generationID); got != 0 {
		t.Fatalf("%d analysis manifest rows survived retirement", got)
	}
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_active_generation WHERE view_gen = ?`, generationID); got != 0 {
		t.Fatalf("%d analysis pointers survived retirement", got)
	}
	for _, table := range []string{
		"analysis_process_steps", "analysis_process_files", "analysis_processes",
		"analysis_concept_relations", "analysis_concepts",
		"analysis_community_files", "analysis_nodes", "analysis_communities",
		"analysis_blobs", "analysis_generation_components",
	} {
		for _, analysisID := range []int64{retiredAnalysis, staleAnalysis} {
			if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM `+table+` WHERE generation_id = ?`, analysisID); got != 0 {
				t.Fatalf("%s kept %d rows of retired analysis %d", table, got, analysisID)
			}
		}
	}

	// And nothing else was touched: the base view's analysis is still active
	// and still readable.
	header, found, err := store.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
	if err != nil || !found || header.GenerationID != baseAnalysis {
		t.Fatalf("base analysis lost to another view's retirement: %v, %v, %+v", found, err, header)
	}
	metrics, err := store.AnalysisNodeMetrics(baseAnalysis, []string{"base-node"})
	if err != nil || len(metrics) != 1 {
		t.Fatalf("base analysis rows lost to another view's retirement: %v, %v", metrics, err)
	}
}

// TestPruneAnalysisGenerationsRetainsPerViewGeneration pins the GC's retention
// window to the view axis: a busy view's history must not evict a quiet view's.
//
// Each analysis is activated, so the newest per view is the pointer's target
// (never collectable) and the rest are ready-but-superseded history — the
// population the retention window ranks. The quiet view is built first so its
// generation ids are the LOWEST: under a store-wide "newest keep, order by
// generation_id DESC, OFFSET keep" window, the busy view's churn pushes the
// quiet view's history off the end.
//
// Revert-red: restore the flat `ORDER BY generation_id DESC LIMIT -1 OFFSET ?`
// candidate query and the quiet view drops to one surviving analysis.
func TestPruneAnalysisGenerationsRetainsPerViewGeneration(t *testing.T) {
	store := openAnalysisViewGenStore(t)

	quiet := store.AtGeneration(3)
	for i := range 2 {
		buildMinimalAnalysisGeneration(t, quiet, fmt.Sprintf("quiet-%d", i), 1, true)
	}
	busy := store.AtGeneration(4)
	for i := range 5 {
		buildMinimalAnalysisGeneration(t, busy, fmt.Sprintf("busy-%d", i), 1, true)
	}

	if err := store.PruneAnalysisGenerations(context.Background(), 2, 100); err != nil {
		t.Fatalf("PruneAnalysisGenerations: %v", err)
	}

	// Quiet view: one active plus one superseded, and the window keeps two.
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = 3`); got != 2 {
		t.Fatalf("quiet view kept %d analyses, want 2 — the busy view's churn evicted its history", got)
	}
	// Busy view: one active plus the two the window keeps; two collected.
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = 4`); got != 3 {
		t.Fatalf("busy view kept %d analyses, want 1 active + the retention window's 2", got)
	}
}

// TestAnalysisGenerationConcurrentViewSelection runs the write and read paths
// of two payload view generations against each other. Under -race it proves the
// axis carries no shared mutable state; functionally it proves neither view's
// active pointer is ever displaced by the other's.
func TestAnalysisGenerationConcurrentViewSelection(t *testing.T) {
	store := openAnalysisViewGenStore(t)

	const rounds = 6
	views := []int64{baseViewGeneration, 11, 12}
	var wg sync.WaitGroup
	failures := make(chan string, len(views)*rounds)
	for _, viewGen := range views {
		wg.Add(1)
		go func(viewGen int64) {
			defer wg.Done()
			handle := store.AtGeneration(viewGen)
			for round := range rounds {
				analysisID, err := buildAnalysisWithoutFatal(handle, fmt.Sprintf("view-%d-%d", viewGen, round))
				if err != nil {
					failures <- fmt.Sprintf("view %d round %d: build: %v", viewGen, round, err)
					return
				}
				header, found, err := handle.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
				if err != nil || !found {
					failures <- fmt.Sprintf("view %d round %d: header %v, %v", viewGen, round, found, err)
					return
				}
				if header.GenerationID != analysisID {
					failures <- fmt.Sprintf("view %d round %d: served analysis %d, want %d", viewGen, round, header.GenerationID, analysisID)
					return
				}
				var stamped int64
				if err := store.db.QueryRow(
					`SELECT view_gen FROM analysis_generations WHERE generation_id = ?`, analysisID).Scan(&stamped); err != nil {
					failures <- fmt.Sprintf("view %d round %d: read stamp: %v", viewGen, round, err)
					return
				}
				if stamped != viewGen {
					failures <- fmt.Sprintf("view %d round %d: analysis stamped %d", viewGen, round, stamped)
					return
				}
			}
		}(viewGen)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

// buildAnalysisWithoutFatal is buildMinimalAnalysisGeneration for goroutines:
// it reports through an error instead of t.Fatal, which may only be called on
// the test's own goroutine.
func buildAnalysisWithoutFatal(store *Store, prefix string) (int64, error) {
	revision := store.AnalysisMutationRevision()
	header := graph.AnalysisGenerationHeader{
		FormatVersion: analysisViewGenFormatVersion, NodeCount: 1, CommunityCount: 1, ConceptCount: 1,
		PageRankMax: 1, AuthorityMax: 1, HubMax: 1, Modularity: 0.5,
	}
	generationID, accepted, err := store.BeginAnalysisGeneration(revision, header)
	if err != nil {
		return 0, err
	}
	if !accepted {
		return 0, fmt.Errorf("begin rejected")
	}
	steps := []struct {
		name string
		run  func() (bool, error)
	}{
		{"communities", func() (bool, error) {
			return store.AppendAnalysisCommunities(revision, generationID, []graph.AnalysisCommunitySummary{{ID: prefix + "-community", Label: prefix, Size: 1}})
		}},
		{"nodes", func() (bool, error) {
			return store.AppendAnalysisNodes(revision, generationID, []graph.AnalysisNodeMetric{{NodeID: prefix + "-node", CommunityID: prefix + "-community", PageRank: 1, Authority: 1, Hub: 1}})
		}},
		{"concepts", func() (bool, error) {
			return store.AppendAnalysisConcepts(revision, generationID, []graph.AnalysisConcept{{Token: prefix + "-token", InVocabulary: true}}, nil)
		}},
		{"adjacency blob", func() (bool, error) {
			return store.PutAnalysisBlob(revision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: []byte("adjacency-" + prefix)})
		}},
		{"leiden blob", func() (bool, error) {
			return store.PutAnalysisBlob(revision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobLeiden, Payload: []byte("leiden-" + prefix)})
		}},
		{"seal nodes", func() (bool, error) {
			return store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentNodes, 1)
		}},
		{"seal communities", func() (bool, error) {
			return store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentCommunities, 1)
		}},
		{"seal processes", func() (bool, error) {
			return store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentProcesses, 0)
		}},
		{"seal concepts", func() (bool, error) {
			return store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentConcepts, 1)
		}},
		{"seal adjacency", func() (bool, error) {
			return store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentAdjacency, 1)
		}},
		{"seal leiden", func() (bool, error) {
			return store.SealAnalysisComponent(revision, generationID, graph.AnalysisComponentLeiden, 1)
		}},
		{"activate", func() (bool, error) {
			return store.ActivateAnalysisGeneration(revision, generationID)
		}},
	}
	for _, step := range steps {
		accepted, err := step.run()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", step.name, err)
		}
		if !accepted {
			return 0, fmt.Errorf("%s rejected", step.name)
		}
	}
	return generationID, nil
}

// downgradeAnalysisTablesToV24 rewrites the analysis cache back to its v24
// shape — no view_gen anywhere, one global active slot — keeping only the base
// generation's rows, and stamps the file at user_version 24. The result is a
// store the shipped v24 build could have written.
func downgradeAnalysisTablesToV24(t *testing.T, db *sql.DB) {
	t.Helper()
	execDDL(t, db, `ALTER TABLE analysis_generations DROP COLUMN view_gen`)

	const legacy = "analysis_active_generation_v24"
	execDDL(t, db, `CREATE TABLE `+legacy+legacyAnalysisActiveGenerationBody)
	execDDL(t, db, `INSERT INTO `+legacy+`(slot, generation_id)
SELECT slot, generation_id FROM analysis_active_generation`)
	execDDL(t, db, `DROP TABLE analysis_active_generation`)
	execDDL(t, db, `ALTER TABLE `+legacy+` RENAME TO analysis_active_generation`)
	execDDL(t, db, `PRAGMA user_version = 24`)
}

// TestAnalysisViewGenMigrationStepPreservesLegacyRowsAtBaseGeneration runs the
// v25 step itself against a legacy fixture, so the copy is observed before
// Open's own schema-transition invalidation runs (store.go: a version change
// stales the active generation and clears the pointer — an intentional,
// pre-existing contract, not a migration defect).
func TestAnalysisViewGenMigrationStepPreservesLegacyRowsAtBaseGeneration(t *testing.T) {
	path, legacyAnalysis := seedLegacyAnalysisStore(t)

	withRawDB(t, path, func(db *sql.DB) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin migration tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		if err := addAnalysisViewGenerationKeys(tx); err != nil {
			t.Fatalf("addAnalysisViewGenerationKeys: %v", err)
		}
		// Idempotent: a second run finds both halves already done.
		if err := addAnalysisViewGenerationKeys(tx); err != nil {
			t.Fatalf("addAnalysisViewGenerationKeys twice: %v", err)
		}

		var manifestGen, manifestViewGen int64
		if err := tx.QueryRow(
			`SELECT generation_id, view_gen FROM analysis_generations`).Scan(&manifestGen, &manifestViewGen); err != nil {
			t.Fatalf("read migrated manifest: %v", err)
		}
		if manifestGen != legacyAnalysis || manifestViewGen != baseViewGeneration {
			t.Fatalf("migrated manifest = generation %d at view_gen %d, want %d at %d",
				manifestGen, manifestViewGen, legacyAnalysis, baseViewGeneration)
		}

		var pointerGen, pointerViewGen, pointerSlot int64
		if err := tx.QueryRow(
			`SELECT generation_id, view_gen, slot FROM analysis_active_generation`).Scan(&pointerGen, &pointerViewGen, &pointerSlot); err != nil {
			t.Fatalf("read migrated pointer: %v", err)
		}
		if pointerGen != legacyAnalysis || pointerViewGen != baseViewGeneration || pointerSlot != 1 {
			t.Fatalf("migrated pointer = generation %d at view_gen %d slot %d, want %d at %d slot 1",
				pointerGen, pointerViewGen, pointerSlot, legacyAnalysis, baseViewGeneration)
		}

		// Every child row the legacy generation owned came through untouched.
		for table, want := range map[string]int{
			"analysis_nodes": 1, "analysis_communities": 1, "analysis_concepts": 3,
			"analysis_blobs": 2, "analysis_generation_components": 6,
		} {
			var count int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE generation_id = ?`, legacyAnalysis).Scan(&count); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			if count != want {
				t.Fatalf("%s holds %d rows for the legacy generation, want %d", table, count, want)
			}
		}
	})
}

// seedLegacyAnalysisStore writes a real analysis cache through the current
// build, then rewrites the two analysis tables back to their v24 shape and
// stamps the file at user_version 24. It returns the store path and the
// analysis generation the fixture holds.
func seedLegacyAnalysisStore(t *testing.T) (string, int64) {
	t.Helper()
	if currentSchemaVersion < 25 {
		t.Fatalf("currentSchemaVersion = %d, want >= 25 for the analysis view generation axis", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 25 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v25 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-analysis-view-gen.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	legacyAnalysis := buildMinimalAnalysisGeneration(t, seed, "legacy", 3, true)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) { downgradeAnalysisTablesToV24(t, db) })
	return path, legacyAnalysis
}

// TestSchemaV24StoreOpensForwardOntoTheAnalysisViewAxis is the
// backward-compatibility proof for v25 at the Open boundary: a store whose
// analysis cache predates the view axis migrates in place — no rebuild, no
// reindex, every surviving row at generation 0 — and comes back able to hold
// one analysis per payload view generation.
//
// Open clears the active pointer on any schema transition (store.go: "A schema
// transition invalidates any generation produced against the old graph shape"),
// which is why this case asserts the manifest and child rows survive rather
// than that the legacy pointer still serves. The migration step's own copy is
// pinned by TestAnalysisViewGenMigrationStepPreservesLegacyRowsAtBaseGeneration.
func TestSchemaV24StoreOpensForwardOntoTheAnalysisViewAxis(t *testing.T) {
	path, legacyAnalysis := seedLegacyAnalysisStore(t)

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v24 store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("re-keying the analysis cache in place must not signal a wipe/reindex")
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}

	// The legacy manifest row survived and reads as the base corpus.
	if got := scalarInt(t, migrated.writerDB, `SELECT COUNT(*) FROM analysis_generations`); got != 1 {
		t.Fatalf("migrated store holds %d analysis generations, want the 1 the fixture wrote", got)
	}
	for _, gen := range rowViewGens(t, migrated.writerDB, "analysis_generations") {
		if gen != baseViewGeneration {
			t.Fatalf("legacy manifest row landed at view_gen %d, want %d", gen, baseViewGeneration)
		}
	}
	if got := scalarInt(t, migrated.writerDB,
		`SELECT COUNT(*) FROM analysis_nodes WHERE generation_id = ?`, legacyAnalysis); got != 1 {
		t.Fatalf("legacy analysis node rows = %d, want 1", got)
	}

	// And the migrated store is a fully-fledged v25 one: the base corpus and a
	// second payload view each hold their own active analysis.
	baseAnalysis := buildMinimalAnalysisGeneration(t, migrated, "post-migration-base", 1, true)
	overlay := migrated.AtGeneration(6)
	overlayAnalysis := buildMinimalAnalysisGeneration(t, overlay, "post-migration-overlay", 1, true)

	baseHeader, found, err := migrated.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
	if err != nil || !found || baseHeader.GenerationID != baseAnalysis {
		t.Fatalf("post-migration base analysis = %+v, %v, %v; want generation %d", baseHeader, found, err, baseAnalysis)
	}
	overlayHeader, found, err := overlay.LoadActiveAnalysisHeader(analysisViewGenFormatVersion)
	if err != nil || !found || overlayHeader.GenerationID != overlayAnalysis {
		t.Fatalf("post-migration overlay analysis = %+v, %v, %v; want generation %d", overlayHeader, found, err, overlayAnalysis)
	}
	for _, gen := range rowViewGens(t, migrated.writerDB, "analysis_active_generation") {
		if gen != baseViewGeneration && gen != 6 {
			t.Fatalf("unexpected pointer at view_gen %d", gen)
		}
	}
	if got := scalarInt(t, migrated.writerDB, `SELECT COUNT(*) FROM analysis_active_generation`); got != 2 {
		t.Fatalf("migrated store holds %d active pointers, want one per view", got)
	}
}

// TestSchemaV25StoreIsRefusedByThePreviousOpener is the newer-schema refusal
// half of the additive-migration contract: a store this build stamps at v25
// must be refused — not rebuilt, not silently opened — by a binary whose
// currentSchemaVersion is still 24.
func TestSchemaV25StoreIsRefusedByThePreviousOpener(t *testing.T) {
	if currentSchemaVersion != 25 {
		t.Fatalf("currentSchemaVersion = %d; this case pins the v24 -> v25 boundary", currentSchemaVersion)
	}
	previous := make([]schemaMigration, 0, len(schemaMigrations))
	for _, migration := range schemaMigrations {
		if migration.version <= 24 {
			previous = append(previous, migration)
		}
	}
	if err := validateSchemaMigrations(24, previous); err != nil {
		t.Fatalf("the reconstructed v24 registry is not well formed: %v", err)
	}
	plan := planSchemaMigrationWith(currentSchemaVersion, 24, previous)
	if plan.err == nil {
		t.Fatal("a v25 store was accepted by the v24 opener")
	}
	var tooNew *SchemaTooNewError
	if !errors.As(plan.err, &tooNew) || tooNew.Stored != 25 || tooNew.Supported != 24 {
		t.Fatalf("refusal = %v (%+v), want a typed SchemaTooNewError stored=25 supported=24", plan.err, tooNew)
	}
	if plan.wipe || plan.stamp || len(plan.inPlace) != 0 {
		t.Fatalf("refusal carried work: %+v", plan)
	}
}

// TestCorruptHeaderKeepsTheLatchWhileAnotherViewHoldsAnAnalysis pins the
// shared-latch half of the axis.
//
// analysisGenerationPresent lives on the storeCore every generation handle
// shares (store.go) and short-circuits invalidateAnalysisGenerationLocked: a
// false latch means the next graph mutation skips durable invalidation
// entirely. Before the view axis there was only ever one active pointer, so
// LoadActiveAnalysisHeader's corrupt path could clear the latch outright.
// With two views each holding one, clearing it on one view's corruption would
// leave the other view's pointer standing while the hot path believed no
// analysis existed — the one way a restart can resurrect stale analysis.
//
// Revert-red: set s.analysisGenerationPresent = false in the corrupt branch of
// LoadActiveAnalysisHeader (its pre-axis form) and the overlay's analysis
// survives the mutation below.
func TestCorruptHeaderKeepsTheLatchWhileAnotherViewHoldsAnAnalysis(t *testing.T) {
	store := openAnalysisViewGenStore(t)

	baseHandle, baseAnalysis := buildAnalysisAt(t, store, baseViewGeneration, "base")
	overlayHandle, overlayAnalysis := buildAnalysisAt(t, store, 11, "overlay")
	if !store.analysisGenerationPresent {
		t.Fatal("the mutation latch is not set after two analyses were activated")
	}

	// Corrupt the BASE view's analysis the way the package's existing
	// corruption case does: drop one sealed component census row, which makes
	// validateAnalysisGenerationTx fail.
	if _, err := store.writerDB.Exec(
		`DELETE FROM analysis_generation_components WHERE generation_id = ? AND component = ?`,
		baseAnalysis, string(graph.AnalysisComponentNodes),
	); err != nil {
		t.Fatalf("corrupt the base analysis: %v", err)
	}
	if _, found, err := baseHandle.LoadActiveAnalysisHeader(analysisViewGenFormatVersion); found || !errors.Is(err, graph.ErrAnalysisGenerationCorrupt) {
		t.Fatalf("corrupt load found=%v err=%v, want %v", found, err, graph.ErrAnalysisGenerationCorrupt)
	}

	// The corrupt view's pointer is gone; the other view's is not.
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_active_generation WHERE view_gen = ?`, baseViewGeneration); got != 0 {
		t.Fatalf("the corrupt view kept %d pointers", got)
	}
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_active_generation WHERE view_gen = 11`); got != 1 {
		t.Fatalf("the overlay view has %d pointers, want 1; the corrupt path reached another view's row", got)
	}
	// Reported, not fatal: the consequence below is the assertion that
	// matters, and it has to run even when the latch is already wrong.
	if !store.analysisGenerationPresent {
		t.Error("the shared mutation latch was cleared while view 11 still held an active analysis")
	}

	// The decisive assertion: the next graph mutation must still run durable
	// invalidation, so the surviving pointer is cleared and its generation
	// marked stale rather than left to be re-read after a restart.
	overlayHandle.AddNode(&graph.Node{ID: "live-after-corruption", Kind: graph.KindFunction, Name: "Live", FilePath: "live.go"})

	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_active_generation`); got != 0 {
		t.Fatalf("%d analysis pointers survived a graph mutation", got)
	}
	if got := scalarInt(t, store.db, `SELECT state FROM analysis_generations WHERE generation_id = ?`, overlayAnalysis); got != analysisGenerationStale {
		t.Fatalf("overlay analysis state = %d after a graph mutation, want stale (%d)", got, analysisGenerationStale)
	}
	if _, err := overlayHandle.AnalysisNodeMetrics(overlayAnalysis, []string{"overlay-node"}); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("the overlay analysis is still readable after a graph mutation: err = %v, want %v", err, graph.ErrAnalysisGenerationInactive)
	}
}

// TestAnalysisWritesRefusedOnceTheirPayloadGenerationRetires closes the leak
// the base-handle bypass opened.
//
// beginAnalysisWrite runs on the base handle so a PUBLISHED generation can
// still cache an analysis, which means analysis transactions never reach
// refuseSealedPayloadWrite. The seal was also the only thing refusing a write
// into a generation being deleted: sweepAnalysisGenerations snapshots the
// analysis ids it will remove and then releases writeMu between chunks, so a
// write landing after that snapshot leaves manifest, child and pointer rows
// for a corpus that no longer exists — and generation ids are never reissued,
// so nothing ever collects them.
//
// Revert-red: drop the refuseRetiredAnalysisWrite call from
// BeginAnalysisGeneration and the begin below succeeds, leaving a manifest row
// stamped with a retired view generation.
func TestAnalysisWritesRefusedOnceTheirPayloadGenerationRetires(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	// A published generation must still be able to cache an analysis: that is
	// the premise the retirement refusal has to leave intact.
	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	publishedAnalysis := buildMinimalAnalysisGeneration(t, handle, "published", 1, true)

	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotDirty,
		ExpectedRouteEpoch: 1,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("un-route: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = ?`, generationID); got != 0 {
		t.Fatalf("the sweep left %d analysis manifest rows; this case needs it to have run", got)
	}

	// A begin through the retired generation's handle is refused, and leaves
	// nothing behind.
	revision := handle.AnalysisMutationRevision()
	header := graph.AnalysisGenerationHeader{
		FormatVersion: analysisViewGenFormatVersion,
		NodeCount:     1, CommunityCount: 1, ConceptCount: 1,
		PageRankMax: 1, AuthorityMax: 1, HubMax: 1, Modularity: 0.5,
	}
	id, accepted, err := handle.BeginAnalysisGeneration(revision, header)
	if !errors.Is(err, ErrPayloadGenerationRetired) {
		t.Fatalf("BeginAnalysisGeneration on a retired generation = (%d, %v, %v), want %v", id, accepted, err, ErrPayloadGenerationRetired)
	}
	if accepted {
		t.Fatal("a retired generation accepted a new analysis")
	}
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = ?`, generationID); got != 0 {
		t.Fatalf("%d analysis manifest rows landed on a retired generation", got)
	}

	// The bounded write gate refuses too, which is what covers an analysis
	// begun before the retirement started.
	if _, accepted, err := handle.beginAnalysisGenerationWrite(revision, publishedAnalysis); !errors.Is(err, ErrPayloadGenerationRetired) || accepted {
		t.Fatalf("append gate on a retired generation = (%v, %v), want %v", accepted, err, ErrPayloadGenerationRetired)
	}

	// And nothing else closed: the base corpus still caches analyses.
	baseAnalysis := buildMinimalAnalysisGeneration(t, store, "base-after-retire", 1, true)
	if _, found, err := store.LoadActiveAnalysisHeader(analysisViewGenFormatVersion); err != nil || !found {
		t.Fatalf("the base corpus lost its analysis cache to another view's retirement: %v, %v", found, err)
	}
	if got := analysisGenerationViewGen(t, store, baseAnalysis); got != baseViewGeneration {
		t.Fatalf("base analysis stamped view_gen %d, want %d", got, baseViewGeneration)
	}
}

// TestAnalysisWritesRefusedThroughAHandleDerivedAfterRetirement closes the
// residual the in-memory seal left open.
//
// RetirePayloadGeneration ends by deleting the catalog row and then the shared
// payloadSeal (payload_generation.go). A handle derived at that id afterwards
// therefore starts unknown and finds no catalog row, which the seal resolver
// has always called "open" — so the admission check that refuses an analysis
// write into a retiring generation used to admit one into a generation that is
// already gone. The catalog's AUTOINCREMENT high-water mark is the tombstone
// that survives both the row and the seal.
//
// Revert-red: drop the !found / payloadGenerationTombstoned branch from
// refuseRetiredAnalysisWrite and the begin below succeeds, leaving a manifest
// row stamped with a generation nothing will ever collect.
func TestAnalysisWritesRefusedThroughAHandleDerivedAfterRetirement(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotDirty,
		ExpectedRouteEpoch: 1,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("un-route: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}

	// The premise: retirement disposed of the shared flag, so a handle derived
	// now cannot inherit the retired verdict.
	if _, cached := store.payloadSeals.Load(generationID); cached {
		t.Fatal("retirement left the shared seal in place; this case needs it gone")
	}
	fresh := store.AtGeneration(generationID)
	if fresh == nil || fresh.seal == nil {
		t.Fatalf("AtGeneration(%d) gave no seal", generationID)
	}
	if got := fresh.seal.state.Load(); got != payloadSealUnknown {
		t.Fatalf("a handle derived after retirement starts at seal state %d, want unknown (%d)", got, payloadSealUnknown)
	}
	if _, found, err := store.Catalog().GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("retirement left a catalog row: found=%v err=%v", found, err)
	}

	revision := fresh.AnalysisMutationRevision()
	header := graph.AnalysisGenerationHeader{
		FormatVersion: analysisViewGenFormatVersion,
		NodeCount:     1, CommunityCount: 1, ConceptCount: 1,
		PageRankMax: 1, AuthorityMax: 1, HubMax: 1, Modularity: 0.5,
	}
	id, accepted, err := fresh.BeginAnalysisGeneration(revision, header)
	if !errors.Is(err, ErrPayloadGenerationRetired) || accepted {
		t.Fatalf("BeginAnalysisGeneration through a handle derived after retirement = (%d, %v, %v), want %v",
			id, accepted, err, ErrPayloadGenerationRetired)
	}
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = ?`, generationID); got != 0 {
		t.Fatalf("%d analysis manifest rows landed on a swept generation", got)
	}
	// The bounded append/seal/activate gate refuses the same way.
	if _, accepted, err := fresh.beginAnalysisGenerationWrite(revision, 1); !errors.Is(err, ErrPayloadGenerationRetired) || accepted {
		t.Fatalf("append gate through a handle derived after retirement = (%v, %v), want %v",
			accepted, err, ErrPayloadGenerationRetired)
	}
	// A payload-plane resolve must not be able to cache "open" underneath the
	// analysis check and reopen the hole on the fast path.
	if err := fresh.refuseSealedPayloadWrite(); err != nil {
		t.Fatalf("payload write gate on a swept generation = %v, want the pre-existing nil", err)
	}
	if err := fresh.refuseRetiredAnalysisWrite(); !errors.Is(err, ErrPayloadGenerationRetired) {
		t.Fatalf("analysis admission after a payload resolve = %v, want %v", err, ErrPayloadGenerationRetired)
	}

	// The boundary the tombstone must not cross: an id the catalog never
	// minted has no row for an entirely different reason and stays open, which
	// is what keeps every caller that manages generations itself working.
	unminted := store.AtGeneration(generationID + 1000)
	if err := unminted.refuseRetiredAnalysisWrite(); err != nil {
		t.Fatalf("an unminted generation was refused as retired: %v", err)
	}
	unmintedAnalysis := buildMinimalAnalysisGeneration(t, unminted, "unminted", 1, true)
	if got := analysisGenerationViewGen(t, store, unmintedAnalysis); got != generationID+1000 {
		t.Fatalf("unminted analysis stamped view_gen %d, want %d", got, generationID+1000)
	}
}

// TestPayloadWriteGateHonoursTheSharedSealOnASweptGeneration pins the one
// refusal the tombstone branch must not swallow.
//
// resolvePayloadSeal's row-less path used to end in openPayloadSeal, whose CAS
// fails when a publish or a retire stored a verdict while the catalog read was
// in flight; it then re-reads the flag and refuses. Declining to cache the open
// verdict for a swept generation must not also drop that re-read, or a handle
// that loaded unknown BEFORE RetirePayloadGeneration's setPayloadSeal and
// whose catalog point read completed AFTER the same retire deleted the row is
// admitted to the payload write gate — past the point drainPayloadWriters can
// wait for it, into a generation the sweep has already walked and an id that is
// never reissued.
//
// Revert-red: replace the tombstone branch's seal re-read with a bare
// `return nil` and the first assertion below fails with err = <nil>.
func TestPayloadWriteGateHonoursTheSharedSealOnASweptGeneration(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotDirty,
		ExpectedRouteEpoch: 1,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("un-route: %v", err)
	}
	// The handle is derived BEFORE the retire, so it keeps its pointer to the
	// shared seal object that RetirePayloadGeneration flips and then drops from
	// the map (payloadSeals.Delete) — exactly the object a writer already
	// inside the gate holds.
	racer := store.AtGeneration(generationID)
	if racer == nil || racer.seal == nil {
		t.Fatalf("AtGeneration(%d) gave no seal", generationID)
	}
	shared := racer.seal
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}
	if got := shared.state.Load(); got != payloadSealRetired {
		t.Fatalf("the shared seal says %d after retirement, want retired (%d)", got, payloadSealRetired)
	}
	if _, found, err := store.Catalog().GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("retirement left a catalog row: found=%v err=%v", found, err)
	}

	// The interleaving, reconstructed: refuseSealedPayloadWrite read the flag
	// one instruction before the retire stored its verdict, so it is already
	// inside resolvePayloadSeal on a seal that now says retired. That is the
	// only way this branch is reached with a non-unknown flag, and it is not
	// reachable through the entrypoint's fast path once the flag has flipped.
	if err := racer.resolvePayloadSeal(shared); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("resolvePayloadSeal on a retired seal with no catalog row = %v, want %v",
			err, ErrPayloadGenerationSealed)
	}
	// The refusal must not have been bought by caching a verdict the payload
	// plane would then report differently, nor by reopening the flag.
	if got := shared.state.Load(); got != payloadSealRetired {
		t.Fatalf("the tombstone branch rewrote the shared seal to %d, want it left at retired (%d)", got, payloadSealRetired)
	}
	// The production entrypoint agrees, on the same handle and the same error.
	if err := racer.refuseSealedPayloadWrite(); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("refuseSealedPayloadWrite on a retired swept generation = %v, want %v",
			err, ErrPayloadGenerationSealed)
	}
	if err := racer.refuseRetiredAnalysisWrite(); !errors.Is(err, ErrPayloadGenerationRetired) {
		t.Fatalf("analysis admission on a retired swept generation = %v, want %v",
			err, ErrPayloadGenerationRetired)
	}

	// And the branch still admits what it always admitted: an unknown seal at
	// the same swept id keeps the payload plane's historical nil, so the
	// tombstone changes a payload outcome only where the shared flag itself
	// already said the generation was closed.
	fresh := store.AtGeneration(generationID)
	if got := fresh.seal.state.Load(); got != payloadSealUnknown {
		t.Fatalf("a handle derived after retirement starts at seal state %d, want unknown (%d)", got, payloadSealUnknown)
	}
	if err := fresh.refuseSealedPayloadWrite(); err != nil {
		t.Fatalf("payload write gate on a swept generation with an unknown seal = %v, want the pre-existing nil", err)
	}
}

// TestViewGenerationIdAllocationIsTheRetirementTombstone asserts the schema
// shape the tombstone's authority rests on.
//
// payloadGenerationTombstoned reads sqlite_sequence, which SQLite maintains
// only for a table declared INTEGER PRIMARY KEY AUTOINCREMENT, and relies on
// the mark never being lowered by a delete. If a later migration rebuilds
// view_generations without AUTOINCREMENT the function silently degrades to
// "never tombstoned" and the retirement refusal disappears with no other test
// going red — so the dependency is asserted here rather than left implicit.
func TestViewGenerationIdAllocationIsTheRetirementTombstone(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	var ddl string
	if err := store.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'view_generations'`).Scan(&ddl); err != nil {
		t.Fatalf("read view_generations DDL: %v", err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		t.Fatalf("view_generations is no longer AUTOINCREMENT, so sqlite_sequence keeps no high-water mark "+
			"and payloadGenerationTombstoned degrades to never-tombstoned; DDL:\n%s", ddl)
	}

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	mark := func() int64 {
		var seq sql.NullInt64
		if err := store.db.QueryRow(
			`SELECT seq FROM sqlite_sequence WHERE name = 'view_generations'`).Scan(&seq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("sqlite_sequence holds no view_generations row after minting generation %d", generationID)
			}
			t.Fatalf("read sqlite_sequence: %v", err)
		}
		return seq.Int64
	}
	if got := mark(); got < generationID {
		t.Fatalf("allocation mark %d is below the minted generation %d", got, generationID)
	}

	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotDirty,
		ExpectedRouteEpoch: 1,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("un-route: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}

	// The row is gone; the mark is not. That difference is the tombstone.
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM view_generations WHERE generation_id = ?`, generationID); got != 0 {
		t.Fatalf("retirement left %d view_generations rows", got)
	}
	if got := mark(); got < generationID {
		t.Fatalf("deleting the row lowered the allocation mark to %d, below generation %d", got, generationID)
	}
	swept := store.AtGeneration(generationID)
	tombstoned, err := swept.payloadGenerationTombstoned()
	if err != nil {
		t.Fatalf("payloadGenerationTombstoned: %v", err)
	}
	if !tombstoned {
		t.Fatalf("generation %d reads as never minted after retirement", generationID)
	}
	unminted := store.AtGeneration(generationID + 1000)
	tombstoned, err = unminted.payloadGenerationTombstoned()
	if err != nil {
		t.Fatalf("payloadGenerationTombstoned(unminted): %v", err)
	}
	if tombstoned {
		t.Fatalf("generation %d was never minted but reads as tombstoned", generationID+1000)
	}
}

// TestAnalysisWritesFollowRetirementNotManagedPayloadAdmission pins both
// directions of the one accept path the axis widened.
//
// beginAnalysisWrite runs on atBase(), which drops the seal AND the
// managed-payload restriction (store_generation.go), so checkManagedPayloadWriteTx
// does not apply to the analysis plane at all. That is deliberate — an analysis
// over a published or externally managed view must still be cacheable — but it
// means the analysis plane's only admission rule is refuseRetiredAnalysisWrite.
// Both halves are pinned here: a live generation is admitted even where managed
// payload admission refuses, and a retired one is refused even though managed
// payload admission reports the same ErrPayloadGenerationSealed either way.
func TestAnalysisWritesFollowRetirementNotManagedPayloadAdmission(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	// A live, building, managed generation: payload admission passes and the
	// analysis plane admits too.
	managed, err := store.AtManagedGeneration(generationID)
	if err != nil {
		t.Fatalf("AtManagedGeneration: %v", err)
	}
	managed.writeMu.Lock()
	tx, err := managed.beginWrite()
	if err != nil {
		managed.writeMu.Unlock()
		t.Fatalf("managed payload write on a building generation: %v", err)
	}
	_ = tx.Rollback()
	managed.writeMu.Unlock()
	managedAnalysis := buildMinimalAnalysisGeneration(t, managed, "managed", 1, true)
	if got := analysisGenerationViewGen(t, store, managedAnalysis); got != generationID {
		t.Fatalf("managed analysis stamped view_gen %d, want %d", got, generationID)
	}

	// The bypass, stated as a fact: a managed handle whose generation the
	// catalog never minted is refused on the payload plane and admitted on the
	// analysis plane.
	unmanagedID := generationID + 1000
	unminted, err := store.AtManagedGeneration(unmanagedID)
	if err != nil {
		t.Fatalf("AtManagedGeneration(unminted): %v", err)
	}
	if err := unminted.refuseRetiredAnalysisWrite(); err != nil {
		t.Fatalf("analysis admission on an unminted managed generation = %v, want nil", err)
	}
	// Not just the admission check — the whole write. This is the assertion
	// that fails if beginAnalysisWrite ever stops running on atBase(): the
	// managed restriction would then reach the analysis plane and refuse.
	unmintedAnalysis := buildMinimalAnalysisGeneration(t, unminted, "unminted-managed", 1, true)
	if got := analysisGenerationViewGen(t, store, unmintedAnalysis); got != unmanagedID {
		t.Fatalf("unminted managed analysis stamped view_gen %d, want %d", got, unmanagedID)
	}
	unminted.writeMu.Lock()
	unmintedTx, err := unminted.beginWrite()
	if unmintedTx != nil {
		_ = unmintedTx.Rollback()
	}
	unminted.writeMu.Unlock()
	if !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("managed payload write on an unminted generation = %v, want %v", err, ErrPayloadGenerationSealed)
	}

	// Retire, then the other direction: the analysis plane refuses through a
	// managed handle derived after the sweep.
	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotDirty,
		ExpectedRouteEpoch: 1,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("un-route: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}
	retired, err := store.AtManagedGeneration(generationID)
	if err != nil {
		t.Fatalf("AtManagedGeneration(retired): %v", err)
	}
	if err := retired.refuseRetiredAnalysisWrite(); !errors.Is(err, ErrPayloadGenerationRetired) {
		t.Fatalf("analysis admission on a retired managed generation = %v, want %v", err, ErrPayloadGenerationRetired)
	}
}

// TestPruneAnalysisGenerationsBoundsRetainedHistoryAcrossViews pins the
// aggregate bound on the retention window.
//
// Partitioning retention by view generation is what keeps a busy view from
// evicting a quiet one's history, but on its own it makes the retained
// population scale as views × keep with no ceiling, and the number of live
// payload generations is a runtime quantity. analysisRetentionViewCap bounds
// it: only the newest cap views keep history, older views keep their active
// analysis and nothing else.
//
// Revert-red: drop `OR rank_of_view > ?` from the candidate query and every
// view keeps its full window, so the total below rises by the history of the
// views past the cap.
func TestPruneAnalysisGenerationsBoundsRetainedHistoryAcrossViews(t *testing.T) {
	store := openAnalysisViewGenStore(t)

	const keep = 2
	const perView = keep + 2 // one active plus keep+1 collectable, so the window bites

	// The absolute bound, deliberately NOT derived from the constant under
	// test. Every other expectation below is written in terms of
	// analysisRetentionViewCap, so widening the constant would silently widen
	// them with it; this is the number the axis actually promises — the
	// doc-comment on analysisRetentionViewCap and the "no view_gen index on
	// analysis_generations" rationale in schema.go both cite it. Raising the
	// cap has to come here, update those two texts, and re-justify the missing
	// index.
	const documentedHistoryBound = 16
	if analysisRetentionViewCap*keep > documentedHistoryBound {
		t.Fatalf("the retention cap now admits %d history rows (cap %d × keep %d), above the documented bound of %d",
			analysisRetentionViewCap*keep, analysisRetentionViewCap, keep, documentedHistoryBound)
	}

	views := analysisRetentionViewCap + 3
	for v := range views {
		handle := store.AtGeneration(int64(v + 1))
		for i := range perView {
			buildMinimalAnalysisGeneration(t, handle, fmt.Sprintf("view%d-%d", v, i), 1, true)
		}
	}
	if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations`); got != views*perView {
		t.Fatalf("seeded %d analyses, want %d", got, views*perView)
	}

	if err := store.PruneAnalysisGenerations(context.Background(), keep, 100); err != nil {
		t.Fatalf("PruneAnalysisGenerations: %v", err)
	}

	// Views are ranked by their newest analysis, and they were built in id
	// order, so the last analysisRetentionViewCap views are the ones that keep
	// history.
	for v := range views {
		viewGen := int64(v + 1)
		want := 1 // the active analysis, which the window never ranks
		if v >= views-analysisRetentionViewCap {
			want = keep + 1
		}
		got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations WHERE view_gen = ?`, viewGen)
		if got != want {
			t.Fatalf("view %d kept %d analyses, want %d", viewGen, got, want)
		}
		if got := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_active_generation WHERE view_gen = ?`, viewGen); got != 1 {
			t.Fatalf("view %d has %d active pointers after prune, want 1 — the cap collected an active analysis", viewGen, got)
		}
	}

	// The aggregate statement: collectable history is bounded by the cap, not
	// by the number of views.
	total := scalarInt(t, store.db, `SELECT COUNT(*) FROM analysis_generations`)
	wantTotal := analysisRetentionViewCap*(keep+1) + (views - analysisRetentionViewCap)
	if total != wantTotal {
		t.Fatalf("store kept %d analyses across %d views, want %d (cap %d × keep %d, plus one active per view)",
			total, views, wantTotal, analysisRetentionViewCap, keep)
	}
	history := scalarInt(t, store.db, `
		SELECT COUNT(*) FROM analysis_generations
		WHERE generation_id NOT IN (SELECT generation_id FROM analysis_active_generation)`)
	if history > analysisRetentionViewCap*keep {
		t.Fatalf("retained history is %d rows, above the aggregate bound of %d", history, analysisRetentionViewCap*keep)
	}
	if history > documentedHistoryBound {
		t.Fatalf("retained history is %d rows, above the documented absolute bound of %d",
			history, documentedHistoryBound)
	}
}
