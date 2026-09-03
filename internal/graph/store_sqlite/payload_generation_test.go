package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The payload-generation lifecycle end to end.
//
// One database carries a base corpus at generation 0 and a divergent overlay at
// a generation the catalog minted. The overlay is written through the ordinary
// Store surface — AddBatch, SetFileMetas, the FTS writer, the mask setters — so
// a lifecycle that needed its own write path could not pass these cases.

const (
	payloadRepo = "repo"

	payloadKeptFile     = payloadRepo + "::pkg/kept.go"
	payloadReplacedFile = payloadRepo + "::pkg/replaced.go"
	payloadRemovedFile  = payloadRepo + "::pkg/removed.go"
	payloadAddedFile    = payloadRepo + "::pkg/added.go"

	payloadKept     = payloadKeptFile + "::Kept"
	payloadReplaced = payloadReplacedFile + "::Replaced"
	payloadRemoved  = payloadRemovedFile + "::Removed"
	payloadAdded    = payloadAddedFile + "::Added"

	payloadGraphID    = "graph-1"
	payloadCheckoutID = "wt"
	payloadFamilyID   = "fam"
	payloadLayerID    = "layer-dirty"
)

func openPayloadStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "payload_generation.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedPayloadBase writes the base corpus: three files, one symbol each, one
// call edge between two of them, plus their file metadata and FTS documents.
func seedPayloadBase(t *testing.T, store *Store) {
	t.Helper()
	store.AddBatch([]*graph.Node{
		{ID: payloadKept, Kind: graph.KindFunction, Name: "Kept", FilePath: payloadKeptFile, RepoPrefix: payloadRepo, Language: "go"},
		{ID: payloadReplaced, Kind: graph.KindFunction, Name: "Replaced", FilePath: payloadReplacedFile, RepoPrefix: payloadRepo, Language: "go", QualName: "pkg.Replaced"},
		{ID: payloadRemoved, Kind: graph.KindFunction, Name: "Removed", FilePath: payloadRemovedFile, RepoPrefix: payloadRepo, Language: "go"},
	}, []*graph.Edge{
		{From: payloadKept, To: payloadReplaced, Kind: graph.EdgeCalls, FilePath: payloadKeptFile, Line: 7},
	})
	if err := store.SetFileMetas(payloadRepo, []graph.FileMetaRow{
		{FilePath: payloadKeptFile, ContentHash: "hash-kept", Size: 100, NodeCount: 1},
		{FilePath: payloadReplacedFile, ContentHash: "hash-replaced", Size: 200, NodeCount: 1},
		{FilePath: payloadRemovedFile, ContentHash: "hash-removed", Size: 300, NodeCount: 1},
	}); err != nil {
		t.Fatalf("SetFileMetas base: %v", err)
	}
	if err := store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: payloadKept, Tokens: "kept"},
		{NodeID: payloadReplaced, Tokens: "replaced"},
		{NodeID: payloadRemoved, Tokens: "removed"},
	}); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS base: %v", err)
	}
}

// seedPayloadControlPlane installs the family, checkout and route the
// lifecycle addresses.
func seedPayloadControlPlane(t *testing.T, store *Store) {
	t.Helper()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, payloadFamilyID, payloadCheckoutID, "inc-1")
	if err := catalog.UpsertCheckoutRoute(context.Background(), CheckoutRoute{
		CheckoutID: payloadCheckoutID,
		GraphID:    payloadGraphID,
		State:      RoutePending,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}
}

func payloadRequest() PayloadGenerationRequest {
	return PayloadGenerationRequest{
		OwnerKind:         "dedicated_graph",
		GraphID:           payloadGraphID,
		LayerID:           payloadLayerID,
		CheckoutID:        payloadCheckoutID,
		GenerationKind:    "dirty",
		TreeOID:           "tree-dirty",
		ConfigHash:        "config-1",
		ExtractorVersions: `{"go":"1"}`,
		ResolverVersion:   "r1",
		CreatedAt:         1000,
	}
}

// writePayloadOverlay populates a building generation with a divergent view of
// the base corpus: one file replaced, one file deleted, one file added.
func writePayloadOverlay(t *testing.T, handle *Store) {
	t.Helper()
	handle.AddBatch([]*graph.Node{
		{ID: payloadReplaced, Kind: graph.KindFunction, Name: "Replaced", FilePath: payloadReplacedFile, RepoPrefix: payloadRepo, Language: "go", QualName: "pkg.Replaced(ctx)"},
		{ID: payloadAdded, Kind: graph.KindFunction, Name: "Added", FilePath: payloadAddedFile, RepoPrefix: payloadRepo, Language: "go"},
	}, []*graph.Edge{
		{From: payloadAdded, To: payloadReplaced, Kind: graph.EdgeCalls, FilePath: payloadAddedFile, Line: 3},
	})
	if err := handle.SetFileMetas(payloadRepo, []graph.FileMetaRow{
		{FilePath: payloadReplacedFile, ContentHash: "hash-replaced-2", Size: 210, NodeCount: 1},
		{FilePath: payloadAddedFile, ContentHash: "hash-added", Size: 40, NodeCount: 1},
	}); err != nil {
		t.Fatalf("SetFileMetas overlay: %v", err)
	}
	if err := handle.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: payloadReplaced, Tokens: "replaced ctx"},
		{NodeID: payloadAdded, Tokens: "added"},
	}); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS overlay: %v", err)
	}
	if err := handle.SetFileMasks([]FileMask{
		{RepoPrefix: payloadRepo, FilePath: payloadReplacedFile, Mode: OwnershipReplace},
		{RepoPrefix: payloadRepo, FilePath: payloadAddedFile, Mode: OwnershipReplace},
		{RepoPrefix: payloadRepo, FilePath: payloadRemovedFile, Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := handle.SetNodeTombstones([]string{payloadRemoved}); err != nil {
		t.Fatalf("SetNodeTombstones: %v", err)
	}
	if err := handle.SetEdgeSourceMasks([]EdgeSourceMask{{SourceID: payloadKept, Mode: OwnershipReplace}}); err != nil {
		t.Fatalf("SetEdgeSourceMasks: %v", err)
	}
	if err := handle.SetProducerState(ProducerCompleteness{Producer: "extractor", State: ProducerStateComplete}); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
}

// payloadGenerationTables is every table whose rows carry a view_gen, plus the
// two core tables the registries do not name.
func payloadGenerationTables() []string {
	return append([]string{"nodes", "edges"}, payloadSweepTables()...)
}

func countAtGeneration(t *testing.T, store *Store, table string, generationID int64) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE view_gen = ?`, generationID).Scan(&count); err != nil {
		t.Fatalf("count %s at generation %d: %v", table, generationID, err)
	}
	return count
}

// countTableRows counts a whole table, which is how the FTS virtual tables
// have to be read: they carry no generation column, so a row nothing maps any
// more is invisible to every generation-scoped count and shows up only here.
func countTableRows(t *testing.T, store *Store, table string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// baseSnapshot renders every base-generation row of the tables an overlay
// touches, so a writer that leaked out of its generation shows up as a
// changed snapshot rather than as a missing feature.
func baseSnapshot(t *testing.T, store *Store) string {
	t.Helper()
	var b strings.Builder
	queries := []string{
		`SELECT 'node', id, name, qual_name, file_path FROM nodes WHERE view_gen = 0 ORDER BY id`,
		`SELECT 'edge', from_id, to_id, kind, file_path FROM edges WHERE view_gen = 0 ORDER BY from_id, to_id`,
		`SELECT 'file', repo_prefix, file_path, content_hash, size FROM files WHERE view_gen = 0 ORDER BY file_path`,
		`SELECT 'fts', node_id, repo_prefix, fts_rowid, '' FROM symbol_fts_rowid WHERE view_gen = 0 ORDER BY node_id`,
	}
	for _, query := range queries {
		rows, err := store.db.Query(query)
		if err != nil {
			t.Fatalf("snapshot query: %v", err)
		}
		for rows.Next() {
			var kind, a, c, d string
			var bcol any
			if err := rows.Scan(&kind, &a, &bcol, &c, &d); err != nil {
				rows.Close()
				t.Fatalf("snapshot scan: %v", err)
			}
			fmt.Fprintf(&b, "%s|%s|%v|%s|%s\n", kind, a, bcol, c, d)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("snapshot iterate: %v", err)
		}
		rows.Close()
	}
	return b.String()
}

func ftsRowidsAtGeneration(t *testing.T, store *Store, table string, generationID int64) []int64 {
	t.Helper()
	rows, err := store.db.Query(`SELECT fts_rowid FROM `+table+` WHERE view_gen = ?`, generationID)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, rowid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func countFTSRows(t *testing.T, store *Store, table string, rowids []int64) int {
	t.Helper()
	if len(rowids) == 0 {
		return 0
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rowids)), ",")
	args := make([]any, 0, len(rowids))
	for _, rowid := range rowids {
		args = append(args, rowid)
	}
	var count int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE rowid IN (`+placeholders+`)`, args...).Scan(&count); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return count
}

// TestPayloadGenerationLifecycle walks begin -> write -> publish -> route ->
// un-route -> retire and asserts the overlay is visible only through its own
// handle, the base is untouched throughout, and retirement leaves nothing
// behind in any generation-keyed table or in the catalog.
func TestPayloadGenerationLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	before := baseSnapshot(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	if generationID <= 0 || handle == nil || handle.ViewGeneration() != generationID {
		t.Fatalf("BeginPayloadGeneration = %d, %v", generationID, handle)
	}
	writePayloadOverlay(t, handle)

	symbolDocs := ftsRowidsAtGeneration(t, store, "symbol_fts_rowid", generationID)
	if len(symbolDocs) != 2 {
		t.Fatalf("overlay symbol docids = %v, want 2", symbolDocs)
	}

	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}

	generation, found, err := store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil || !found {
		t.Fatalf("GetViewGeneration = %v, %v, %v", generation, found, err)
	}
	if generation.State != ViewGenerationReady {
		t.Fatalf("published state = %q, want %q", generation.State, ViewGenerationReady)
	}
	if generation.PublishedAt <= 0 {
		t.Fatalf("published_at = %d, want a stamped clock reading", generation.PublishedAt)
	}
	// Two files carried, three files claimed, 210 + 40 bytes behind them.
	if generation.CoveredFiles != 2 || generation.AffectedFiles != 3 || generation.StorageBytes != 250 {
		t.Fatalf("rollup = covered %d, affected %d, bytes %d; want 2, 3, 250",
			generation.CoveredFiles, generation.AffectedFiles, generation.StorageBytes)
	}

	route, found, err := store.Catalog().GetCheckoutRoute(ctx, payloadCheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckoutRoute = %v, %v, %v", route, found, err)
	}
	if route.DirtyGenerationID != generationID || route.CommitGenerationID != 0 {
		t.Fatalf("route generations = commit %d, dirty %d; want 0, %d",
			route.CommitGenerationID, route.DirtyGenerationID, generationID)
	}
	if route.RouteEpoch != 1 || route.State != RouteActive {
		t.Fatalf("route = epoch %d, state %q; want 1, %q", route.RouteEpoch, route.State, RouteActive)
	}

	// The overlay is what the derived handle serves.
	derived := store.AtGeneration(generationID)
	replaced := derived.GetNode(payloadReplaced)
	if replaced == nil || replaced.QualName != "pkg.Replaced(ctx)" {
		t.Fatalf("derived Replaced = %+v", replaced)
	}
	if derived.GetNode(payloadAdded) == nil {
		t.Fatalf("derived handle lost the added node")
	}
	if derived.GetNode(payloadKept) != nil {
		t.Fatalf("derived handle served an inherited node it never wrote")
	}
	masks, err := derived.FileMasks()
	if err != nil || len(masks) != 3 {
		t.Fatalf("FileMasks = %v, %v", masks, err)
	}
	tombstones, err := derived.NodeTombstones()
	if err != nil || len(tombstones) != 1 || tombstones[0] != payloadRemoved {
		t.Fatalf("NodeTombstones = %v, %v", tombstones, err)
	}

	// The base is what it always was.
	baseReplaced := store.GetNode(payloadReplaced)
	if baseReplaced == nil || baseReplaced.QualName != "pkg.Replaced" {
		t.Fatalf("base Replaced = %+v", baseReplaced)
	}
	if store.GetNode(payloadRemoved) == nil {
		t.Fatalf("base lost the removed node")
	}
	if store.GetNode(payloadAdded) != nil {
		t.Fatalf("base served a node only the overlay wrote")
	}
	if got := baseSnapshot(t, store); got != before {
		t.Fatalf("base changed while the overlay was built:\nbefore:\n%s\nafter:\n%s", before, got)
	}

	// A routed generation cannot be retired.
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("RetirePayloadGeneration while routed = %v, want %v", err, ErrCatalogGenerationReferenced)
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

	for _, table := range payloadGenerationTables() {
		if got := countAtGeneration(t, store, table, generationID); got != 0 {
			t.Fatalf("%s still holds %d rows at generation %d", table, got, generationID)
		}
	}
	if got := countFTSRows(t, store, "symbol_fts", symbolDocs); got != 0 {
		t.Fatalf("symbol_fts still holds %d retired documents", got)
	}
	if _, found, err := store.Catalog().GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("catalog row after retire = %v, %v", found, err)
	}
	if got := baseSnapshot(t, store); got != before {
		t.Fatalf("retirement disturbed the base:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestPayloadGenerationPublishRefusesInconsistentMasks proves the publish gate
// runs the mask integrity check: a generation that claims to have deleted a
// file it also carries cannot reach ready.
func TestPayloadGenerationPublishRefusesInconsistentMasks(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	handle.AddBatch([]*graph.Node{
		{ID: payloadAdded, Kind: graph.KindFunction, Name: "Added", FilePath: payloadAddedFile, RepoPrefix: payloadRepo, Language: "go"},
	}, nil)
	if err := handle.SetFileMasks([]FileMask{
		{RepoPrefix: payloadRepo, FilePath: payloadAddedFile, Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}

	if err := store.PublishPayloadGeneration(ctx, generationID, 2000); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("publish with contradictory masks = %v, want %v", err, ErrGenerationMaskIntegrity)
	}
	generation, _, err := store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration: %v", err)
	}
	if generation.State != ViewGenerationBuilding {
		t.Fatalf("state after refused publish = %q, want %q", generation.State, ViewGenerationBuilding)
	}
	// A refused publish leaves the generation writable, so the caller can fix
	// the contradiction and try again.
	if err := handle.SetFileMasks([]FileMask{
		{RepoPrefix: payloadRepo, FilePath: payloadAddedFile, Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks after refused publish: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 2000); err != nil {
		t.Fatalf("publish after repair: %v", err)
	}
}

// TestPayloadGenerationPublishRefusesUnsettledProducer proves the second half
// of the publish gate: a producer that has not finished blocks the transition.
func TestPayloadGenerationPublishRefusesUnsettledProducer(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	if err := handle.SetProducerState(ProducerCompleteness{Producer: "resolver", State: ProducerStateBuilding}); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 2000); !errors.Is(err, ErrPayloadGenerationIncomplete) {
		t.Fatalf("publish with a building producer = %v, want %v", err, ErrPayloadGenerationIncomplete)
	}
	if err := handle.SetProducerState(ProducerCompleteness{Producer: "resolver", State: ProducerStateComplete}); err != nil {
		t.Fatalf("SetProducerState complete: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 2000); err != nil {
		t.Fatalf("publish after the producer settled: %v", err)
	}
}

// TestPayloadGenerationWritesRefusedAfterReady proves the write gate seals a
// published generation across every write door — the batch writer, the sidecar
// setters and the mask writers — while the base handle keeps writing.
func TestPayloadGenerationWritesRefusedAfterReady(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishPayloadGeneration(ctx, generationID, 3000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	// The handle the writer already holds is refused, and so is a handle
	// derived after the publish: both share one flag.
	for name, write := range map[string]func(*Store) error{
		"add_batch": func(s *Store) error {
			_, err := s.addBatchSetOriented([]*graph.Node{
				{ID: payloadAdded + "2", Kind: graph.KindFunction, Name: "Late", FilePath: payloadAddedFile, RepoPrefix: payloadRepo},
			}, nil)
			return err
		},
		"file_metas": func(s *Store) error {
			return s.SetFileMetas(payloadRepo, []graph.FileMetaRow{{FilePath: payloadAddedFile, ContentHash: "late"}})
		},
		"file_masks": func(s *Store) error {
			return s.SetFileMasks([]FileMask{{RepoPrefix: payloadRepo, FilePath: payloadAddedFile, Mode: OwnershipReplace}})
		},
		"producer_state": func(s *Store) error {
			return s.SetProducerState(ProducerCompleteness{Producer: "late", State: ProducerStateComplete})
		},
	} {
		for _, target := range []*Store{handle, store.AtGeneration(generationID)} {
			if err := write(target); !errors.Is(err, ErrPayloadGenerationSealed) {
				t.Fatalf("%s through a published generation = %v, want %v", name, err, ErrPayloadGenerationSealed)
			}
		}
	}

	// The base corpus is untouched by the seal.
	store.AddBatch([]*graph.Node{
		{ID: payloadKept, Kind: graph.KindFunction, Name: "Kept", FilePath: payloadKeptFile, RepoPrefix: payloadRepo, Language: "go", QualName: "pkg.Kept"},
	}, nil)
	if node := store.GetNode(payloadKept); node == nil || node.QualName != "pkg.Kept" {
		t.Fatalf("base write after the overlay was sealed = %+v", node)
	}
}

// TestPayloadGenerationRouteFlipCASLeavesReadyUnrouted proves the composed
// operation is not atomic across its two halves by design: a flip that loses
// its compare-and-set leaves the generation ready and unrouted.
func TestPayloadGenerationRouteFlipCASLeavesReadyUnrouted(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	// Another reconciler moves the route first, so the epoch the caller
	// captured is stale.
	if err := store.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         payloadCheckoutID,
		Slot:               RouteSlotCommit,
		ExpectedRouteEpoch: 0,
		State:              RoutePending,
	}); err != nil {
		t.Fatalf("competing flip: %v", err)
	}

	err = store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty)
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("PublishAndRoute with a stale epoch = %v, want %v", err, ErrCatalogStaleGuard)
	}

	generation, _, err := store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration: %v", err)
	}
	if generation.State != ViewGenerationReady {
		t.Fatalf("state after a lost flip = %q, want %q", generation.State, ViewGenerationReady)
	}
	route, _, err := store.Catalog().GetCheckoutRoute(ctx, payloadCheckoutID)
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if route.DirtyGenerationID != 0 {
		t.Fatalf("dirty slot after a lost flip = %d, want 0", route.DirtyGenerationID)
	}

	// The caller decides what a ready-but-unrouted generation becomes.
	if err := store.MarkPayloadGenerationSuperseded(ctx, generationID); err != nil {
		t.Fatalf("MarkPayloadGenerationSuperseded: %v", err)
	}
	generation, _, err = store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration: %v", err)
	}
	if generation.State != ViewGenerationSuperseded {
		t.Fatalf("state after supersede = %q, want %q", generation.State, ViewGenerationSuperseded)
	}
	// Superseded is still sealed.
	if err := handle.SetProducerState(ProducerCompleteness{Producer: "late", State: ProducerStateComplete}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("write to a superseded generation = %v, want %v", err, ErrPayloadGenerationSealed)
	}
}

// TestPayloadGenerationSecondBeginCoalesces pins the coalescing decision: a
// repeat begin for the same layer and the same inputs adopts the build already
// in flight, while a begin whose inputs differ gets its own generation.
func TestPayloadGenerationSecondBeginCoalesces(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadControlPlane(t, store)

	first, _, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("first BeginPayloadGeneration: %v", err)
	}
	second, secondHandle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("second BeginPayloadGeneration: %v", err)
	}
	if second != first {
		t.Fatalf("repeat begin = generation %d, want the in-flight %d", second, first)
	}
	if secondHandle.ViewGeneration() != first {
		t.Fatalf("adopted handle generation = %d, want %d", secondHandle.ViewGeneration(), first)
	}

	movedTree := payloadRequest()
	movedTree.TreeOID = "tree-dirty-2"
	third, _, err := store.BeginPayloadGeneration(ctx, movedTree)
	if err != nil {
		t.Fatalf("BeginPayloadGeneration with moved inputs: %v", err)
	}
	if third == first {
		t.Fatalf("begin with different inputs adopted generation %d", third)
	}

	// A generation with no layer is outside the rule and always gets its own.
	unnamed := payloadRequest()
	unnamed.LayerID = ""
	fourth, _, err := store.BeginPayloadGeneration(ctx, unnamed)
	if err != nil {
		t.Fatalf("BeginPayloadGeneration without a layer: %v", err)
	}
	fifth, _, err := store.BeginPayloadGeneration(ctx, unnamed)
	if err != nil {
		t.Fatalf("second BeginPayloadGeneration without a layer: %v", err)
	}
	if fourth == fifth {
		t.Fatalf("layerless begins coalesced onto generation %d", fourth)
	}

	// Publishing the adopted build closes it for adoption.
	if err := store.PublishPayloadGeneration(ctx, first, 4000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	sixth, _, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration after publish: %v", err)
	}
	if sixth == first {
		t.Fatalf("begin adopted the published generation %d", first)
	}
}

// TestPayloadGenerationRetireRespectsLease proves the lease hook: retirement is
// refused while a reader holds the generation, and the payload survives the
// refusal intact.
func TestPayloadGenerationRetireRespectsLease(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishPayloadGeneration(ctx, generationID, 5000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	leased := true
	inUse := func(candidate int64) bool { return leased && candidate == generationID }
	if err := store.RetirePayloadGeneration(ctx, generationID, inUse); !errors.Is(err, ErrPayloadGenerationInUse) {
		t.Fatalf("retire while leased = %v, want %v", err, ErrPayloadGenerationInUse)
	}
	if got := countAtGeneration(t, store, "nodes", generationID); got != 2 {
		t.Fatalf("nodes at generation %d after a refused retire = %d, want 2", generationID, got)
	}
	generation, _, err := store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration: %v", err)
	}
	if generation.State != ViewGenerationReady {
		t.Fatalf("state after a refused retire = %q, want %q", generation.State, ViewGenerationReady)
	}

	leased = false
	if err := store.RetirePayloadGeneration(ctx, generationID, inUse); err != nil {
		t.Fatalf("retire after the lease dropped: %v", err)
	}
	for _, table := range payloadGenerationTables() {
		if got := countAtGeneration(t, store, table, generationID); got != 0 {
			t.Fatalf("%s still holds %d rows at generation %d", table, got, generationID)
		}
	}
}

// TestPayloadGenerationRetireIsResumable proves the sweep is idempotent: a
// retire interrupted after its payload deletes leaves a retiring row whose
// next run finishes the job.
func TestPayloadGenerationRetireIsResumable(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishPayloadGeneration(ctx, generationID, 6000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	// Stand where a killed retire leaves the database: the row is retiring and
	// the payload sweep has already run.
	if err := store.Catalog().SetViewGenerationState(ctx, generationID, ViewGenerationRetiring); err != nil {
		t.Fatalf("SetViewGenerationState: %v", err)
	}
	if err := store.sweepPayloadGeneration(ctx, generationID); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("resumed retire: %v", err)
	}
	if _, found, err := store.Catalog().GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("catalog row after the resumed retire = %v, %v", found, err)
	}
}

// TestPayloadGenerationSweepClearsFTSPastOneChunk proves the FTS sweep is
// self-limiting. The virtual tables carry no generation column, so their rows
// are reachable only through the docid maps: a chunk that deleted documents
// without taking the map rows it read would find the same docids again, remove
// nothing the second time, and strand every document past the first batch.
func TestPayloadGenerationSweepClearsFTSPastOneChunk(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	baseSymbolDocs := countTableRows(t, store, "symbol_fts")
	baseContentDocs := countTableRows(t, store, "content_fts")

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}

	// More documents than one sweep chunk removes, in both FTS corpora.
	const documents = payloadGenerationSweepBatch + 250
	nodes := make([]*graph.Node, 0, documents)
	symbols := make([]graph.SymbolFTSItem, 0, documents)
	sections := make([]graph.ContentFTSItem, 0, documents)
	for i := 0; i < documents; i++ {
		file := fmt.Sprintf("%s::pkg/bulk%04d.go", payloadRepo, i)
		nodeID := file + "::Bulk"
		nodes = append(nodes, &graph.Node{
			ID: nodeID, Kind: graph.KindFunction, Name: "Bulk",
			FilePath: file, RepoPrefix: payloadRepo, Language: "go",
		})
		symbols = append(symbols, graph.SymbolFTSItem{NodeID: nodeID, Tokens: "bulk"})
		sections = append(sections, graph.ContentFTSItem{NodeID: nodeID, FilePath: file, Body: "bulk body"})
	}
	handle.AddBatch(nodes, nil)
	if err := handle.BatchUpsertSymbolFTS(symbols); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS: %v", err)
	}
	if err := handle.AppendContent(payloadRepo, sections); err != nil {
		t.Fatalf("AppendContent: %v", err)
	}
	if got := countAtGeneration(t, store, "symbol_fts_rowid", generationID); got != documents {
		t.Fatalf("overlay symbol docids = %d, want %d", got, documents)
	}

	if err := store.PublishPayloadGeneration(ctx, generationID, 7000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}

	if got := countTableRows(t, store, "symbol_fts"); got != baseSymbolDocs {
		t.Fatalf("symbol_fts holds %d documents after retire, want the %d the base carries", got, baseSymbolDocs)
	}
	if got := countTableRows(t, store, "content_fts"); got != baseContentDocs {
		t.Fatalf("content_fts holds %d documents after retire, want the %d the base carries", got, baseContentDocs)
	}
	for _, table := range payloadGenerationTables() {
		if got := countAtGeneration(t, store, table, generationID); got != 0 {
			t.Fatalf("%s still holds %d rows at generation %d", table, got, generationID)
		}
	}
}

// TestPayloadGenerationLosingPublisherKeepsTheSeal proves a publish that does
// not reach ready never hands the generation back to writers once it has left
// building. The first case is the losing publisher's half staged without the
// race: the generation is already published, so the second publish fails on
// the guarded rollup with the seal freshly closed, and reopening it there
// would admit writes to a payload readers are already being served. The loop
// then runs the real race.
func TestPayloadGenerationLosingPublisherKeepsTheSeal(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishPayloadGeneration(ctx, generationID, 8000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 8001); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("second publish of a ready generation = %v, want %v", err, ErrCatalogStaleGuard)
	}
	if err := handle.SetProducerState(ProducerCompleteness{Producer: "late", State: ProducerStateComplete}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("write after the refused publish = %v, want %v", err, ErrPayloadGenerationSealed)
	}

	// The same thing under a real race: exactly one publisher reaches ready,
	// and the loser's error leaves every writer refused.
	for attempt := 0; attempt < 8; attempt++ {
		request := payloadRequest()
		request.TreeOID = fmt.Sprintf("tree-publish-race-%d", attempt)
		racedID, racedHandle, err := store.BeginPayloadGeneration(ctx, request)
		if err != nil {
			t.Fatalf("BeginPayloadGeneration: %v", err)
		}
		writePayloadOverlay(t, racedHandle)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			errs  = make([]error, 2)
		)
		for i := range errs {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				<-start
				errs[slot] = store.PublishPayloadGeneration(ctx, racedID, 8100)
			}(i)
		}
		close(start)
		wg.Wait()

		published := 0
		for _, err := range errs {
			switch {
			case err == nil:
				published++
			case errors.Is(err, ErrCatalogStaleGuard):
			default:
				t.Fatalf("racing publish = %v, want nil or %v", err, ErrCatalogStaleGuard)
			}
		}
		if published != 1 {
			t.Fatalf("publishers that reached ready = %d, want 1", published)
		}
		generation, _, err := store.Catalog().GetViewGeneration(ctx, racedID)
		if err != nil {
			t.Fatalf("GetViewGeneration: %v", err)
		}
		if generation.State != ViewGenerationReady {
			t.Fatalf("state after the race = %q, want %q", generation.State, ViewGenerationReady)
		}
		if err := racedHandle.SetProducerState(ProducerCompleteness{Producer: "late", State: ProducerStateComplete}); !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("write after the losing publish = %v, want %v", err, ErrPayloadGenerationSealed)
		}
	}
}

// TestPayloadGenerationPublishDrainsRacingWriter proves the publish window is
// closed. A second builder holding the same generation — what a coalesced
// begin hands out — streams mask writes across the whole publish, so rows
// arrive on both sides of every step it takes. Each mask is sound on its own,
// which leaves the rollup as the observable: affected_files is measured once,
// and a row admitted after that measurement but before the ready flip lands
// inside a published generation the count no longer describes. The writes are
// refused from the seal onwards, so the two must agree.
func TestPayloadGenerationPublishDrainsRacingWriter(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	for attempt := 0; attempt < 8; attempt++ {
		request := payloadRequest()
		request.TreeOID = fmt.Sprintf("tree-write-race-%d", attempt)
		generationID, handle, err := store.BeginPayloadGeneration(ctx, request)
		if err != nil {
			t.Fatalf("BeginPayloadGeneration: %v", err)
		}
		writePayloadOverlay(t, handle)
		_, second, err := store.BeginPayloadGeneration(ctx, request)
		if err != nil {
			t.Fatalf("second BeginPayloadGeneration: %v", err)
		}

		var (
			wg         sync.WaitGroup
			start      = make(chan struct{})
			writeErr   error
			publishErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// A delete mask over a path the generation carries nothing for is
			// valid, so every one of these could legitimately be published.
			for i := 0; i < 400; i++ {
				if err := second.SetFileMasks([]FileMask{{
					RepoPrefix: payloadRepo,
					FilePath:   fmt.Sprintf("%s::pkg/late%04d.go", payloadRepo, i),
					Mode:       OwnershipDelete,
				}}); err != nil {
					writeErr = err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			publishErr = store.PublishPayloadGeneration(ctx, generationID, 9000)
		}()
		close(start)
		wg.Wait()

		if publishErr != nil {
			t.Fatalf("publish racing sound mask writes = %v", publishErr)
		}
		if !errors.Is(writeErr, ErrPayloadGenerationSealed) {
			t.Fatalf("mask writes racing a completed publish ended with %v, want %v", writeErr, ErrPayloadGenerationSealed)
		}
		generation, _, err := store.Catalog().GetViewGeneration(ctx, generationID)
		if err != nil {
			t.Fatalf("GetViewGeneration: %v", err)
		}
		if masks := countAtGeneration(t, store, "generation_file_masks", generationID); int64(masks) != generation.AffectedFiles {
			t.Fatalf("published generation counts %d affected files but carries %d mask rows",
				generation.AffectedFiles, masks)
		}
		if err := store.AtGeneration(generationID).ValidateGenerationMasks(); err != nil {
			t.Fatalf("published generation fails the check its publish ran: %v", err)
		}
	}
}

// TestPayloadGenerationSweepCoversEveryGenerationTable is the drift guard: the
// sweep must name every table that carries a view_gen column, so a sidecar
// added later cannot silently outlive the generation that wrote it.
func TestPayloadGenerationSweepCoversEveryGenerationTable(t *testing.T) {
	store := openPayloadStore(t)
	rows, err := store.db.Query(`
SELECT m.name FROM sqlite_schema AS m
 WHERE m.type = 'table'
   AND EXISTS (SELECT 1 FROM pragma_table_info(m.name) AS c WHERE c.name = 'view_gen')
 ORDER BY m.name`)
	if err != nil {
		t.Fatalf("scan schema: %v", err)
	}
	defer rows.Close()
	var carriers []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		carriers = append(carriers, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema: %v", err)
	}

	swept := make(map[string]struct{}, len(carriers))
	for _, table := range payloadGenerationTables() {
		swept[table] = struct{}{}
	}
	for _, table := range carriers {
		if _, ok := swept[table]; !ok {
			t.Fatalf("table %s carries view_gen but the payload sweep does not visit it", table)
		}
	}
	if len(carriers) != len(swept) {
		t.Fatalf("sweep visits %d tables, the schema carries view_gen on %d", len(swept), len(carriers))
	}
}
