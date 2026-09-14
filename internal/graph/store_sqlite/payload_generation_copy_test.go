package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The row-level generation copy: what it moves, what it refuses, and what it
// costs against the two shapes it replaces.
//
// The copy exists because a first committed base over a clean checkout is a
// re-parse of bytes the store has ALREADY parsed into generation zero. Moving
// the rows is the same payload without the parse, and — inside the generation
// bulk window — without the incremental write shape either.

// copyFixtureRepo is the repository the tests below copy. A second prefix is
// seeded beside it in the scoping test so "repository-scoped" is a measured
// fact and not a reading of the SQL.
const copyFixtureRepo = "gortex"

// reservedGeneration opens a real reserved generation. The copy writes through
// the managed-generation seam, so its destination must be a generation the
// catalog is actually holding open — an ad-hoc view_gen integer is refused,
// which is the point.
func reservedGeneration(t *testing.T, store *Store, layer string) int64 {
	t.Helper()
	generationID, _, err := store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-copy", LayerID: layer,
		GenerationKind: "commit", TreeOID: "tree-copy-" + layer, CreatedAt: 10,
	})
	if err != nil {
		t.Fatalf("reserve a destination generation: %v", err)
	}
	return generationID
}

// seedGenerationZero writes a payload across every family of generation-keyed
// table the copy has to move: the two graph tables, a repo_prefix-scoped
// sidecar (file metas), the freshness provenance row, and both FTS
// projections with their docid maps.
func seedGenerationZero(t *testing.T, store *Store, repoPrefix string, nNodes, nEdges int) {
	t.Helper()
	nodes, edges := bulkFixture(nNodes, nEdges)
	for _, node := range nodes {
		node.ID = repoPrefix + "/" + node.ID
		node.FilePath = repoPrefix + "/" + node.FilePath
		node.RepoPrefix = repoPrefix
	}
	for _, edge := range edges {
		edge.From = repoPrefix + "/" + edge.From
		edge.To = repoPrefix + "/" + edge.To
		edge.FilePath = repoPrefix + "/" + edge.FilePath
	}
	base := store.AtGeneration(baseViewGeneration)
	if err := base.AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("seed generation zero: %v", err)
	}
	metas := make([]graph.FileMetaRow, 0, 16)
	for i := range 16 {
		metas = append(metas, graph.FileMetaRow{
			FilePath:  fmt.Sprintf("%s/pkg/f%d.go", repoPrefix, i),
			Size:      100 + i,
			NodeCount: i,
			// A non-empty content hash keeps the row distinguishable per file,
			// so a copy that collapsed rows would be visible.
			ContentHash: fmt.Sprintf("hash-%s-%d", repoPrefix, i),
		})
	}
	if err := base.SetFileMetas(repoPrefix, metas); err != nil {
		t.Fatalf("seed file metas: %v", err)
	}
	if err := base.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: repoPrefix, IndexedSHA: "sha-" + repoPrefix, IndexedAt: 1234,
		NodeCount: nNodes, EdgeCount: nEdges, ExtractorVersions: `{"go":7}`,
	}); err != nil {
		t.Fatalf("seed index state: %v", err)
	}
	items := make([]graph.SymbolFTSItem, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, graph.SymbolFTSItem{NodeID: node.ID, Tokens: node.Name + " " + node.QualName})
	}
	if err := base.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatalf("seed symbol fts: %v", err)
	}
	content := make([]graph.ContentFTSItem, 0, 16)
	for i := range 16 {
		content = append(content, graph.ContentFTSItem{
			NodeID:   fmt.Sprintf("%s/pkg/f%d.go", repoPrefix, i),
			FilePath: fmt.Sprintf("%s/pkg/f%d.go", repoPrefix, i),
			Ordinal:  i,
			Body:     fmt.Sprintf("section %d of %s", i, repoPrefix),
		})
	}
	if err := base.AppendContent(repoPrefix, content); err != nil {
		t.Fatalf("seed content fts: %v", err)
	}
}

// generationRows renders one table's rows for one generation and repository as
// sorted strings, with the columns a copy is NOT expected to carry verbatim
// left out: the generation stamp itself, the physical edge rowid, and the FTS
// docids (globally unique, so the destination mints its own).
func generationRows(t *testing.T, store *Store, table string, generationID int64, repoPrefix string) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin a read transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	columns, err := generationCopyColumns(ctx, tx, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	projection := make([]string, 0, len(columns))
	scoped := false
	for _, column := range columns {
		switch column {
		case viewGenColumnName:
		case "fts_rowid":
		default:
			if table == "edges" && column == "id" {
				continue
			}
			if column == "repo_prefix" {
				scoped = true
			}
			projection = append(projection, column)
		}
	}
	query := `SELECT ` + strings.Join(projection, ", ") + ` FROM ` + table + ` WHERE ` + viewGenColumnName + ` = ?`
	args := []any{generationID}
	if scoped {
		query += ` AND repo_prefix = ?`
		args = append(args, repoPrefix)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		values := make([]any, len(projection))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		fields := make([]string, 0, len(values))
		for _, value := range values {
			if blob, isBlob := value.([]byte); isBlob {
				value = string(blob)
			}
			fields = append(fields, fmt.Sprintf("%v", value))
		}
		out = append(out, strings.Join(fields, "\x00"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %s: %v", table, err)
	}
	sort.Strings(out)
	return out
}

// ftsDocuments renders one generation's FTS documents through its docid map,
// which is the only address a generation has for them.
func ftsDocuments(t *testing.T, store *Store, docidMap ftsDocidMap, generationID int64, repoPrefix string) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin a read transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	columns, err := generationCopyColumns(ctx, tx, docidMap.fts)
	if err != nil {
		t.Fatalf("columns of %s: %v", docidMap.fts, err)
	}
	projection := make([]string, 0, len(columns))
	for _, column := range columns {
		projection = append(projection, "f."+column)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+strings.Join(projection, ", ")+`
  FROM `+docidMap.ids+` m JOIN `+docidMap.fts+` f ON f.rowid = m.fts_rowid
 WHERE m.`+viewGenColumnName+` = ? AND m.repo_prefix = ?`, generationID, repoPrefix)
	if err != nil {
		t.Fatalf("read %s: %v", docidMap.fts, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		values := make([]any, len(projection))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan %s: %v", docidMap.fts, err)
		}
		fields := make([]string, 0, len(values))
		for _, value := range values {
			if blob, isBlob := value.([]byte); isBlob {
				value = string(blob)
			}
			fields = append(fields, fmt.Sprintf("%v", value))
		}
		out = append(out, strings.Join(fields, "\x00"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %s: %v", docidMap.fts, err)
	}
	sort.Strings(out)
	return out
}

// A copied generation carries what its source carried, table for table.
//
// The oracle is not a row COUNT: a copy that dropped a column, collapsed rows
// onto one key, or carried a stale generation stamp would keep every count
// right. Every writable column of every generation-keyed table is compared,
// with only the three values a copy must NOT carry verbatim excluded — the
// generation stamp, the physical edge rowid, and the FTS docids.
func TestCopyPayloadGenerationCarriesEveryGenerationKeyedTable(t *testing.T) {
	store := openCatalogStore(t)
	seedGenerationZero(t, store, copyFixtureRepo, 256, 512)
	destination := reservedGeneration(t, store, "carries")

	counts, err := store.CopyPayloadGeneration(context.Background(), baseViewGeneration, destination, copyFixtureRepo)
	if err != nil {
		t.Fatalf("CopyPayloadGeneration: %v", err)
	}
	if counts.Nodes != 256 || counts.Edges == 0 {
		t.Fatalf("counts = %+v, want the seeded 256 nodes and some edges", counts)
	}
	if counts.Rows <= counts.Nodes+counts.Edges {
		t.Fatalf("counts.Rows = %d, want more than the %d graph rows: the sidecars and FTS projections "+
			"are part of what a generation carries", counts.Rows, counts.Nodes+counts.Edges)
	}

	tables := append([]string{"nodes", "edges"}, payloadSweepTables()...)
	moved := 0
	for _, table := range tables {
		want := generationRows(t, store, table, baseViewGeneration, copyFixtureRepo)
		got := generationRows(t, store, table, destination, copyFixtureRepo)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s: the copied generation holds %d rows, the source %d, and they differ:\ngot=%v\nwant=%v",
				table, len(got), len(want), got, want)
		}
		if len(want) > 0 {
			moved++
		}
	}
	if moved < 5 {
		t.Fatalf("only %d generation-keyed tables carried rows in the source, so this comparison is "+
			"mostly comparing empty sets", moved)
	}

	for _, docidMap := range generationFTSDocidMaps {
		want := ftsDocuments(t, store, docidMap, baseViewGeneration, copyFixtureRepo)
		got := ftsDocuments(t, store, docidMap, destination, copyFixtureRepo)
		if len(want) == 0 {
			t.Fatalf("%s: the fixture seeded no documents, so the copy is not being tested", docidMap.fts)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s: copied documents differ:\ngot=%v\nwant=%v", docidMap.fts, got, want)
		}
		// The docids themselves must NOT be shared: one docid names one row of
		// one shared virtual table, so two generations claiming the same one
		// is corruption, and symbol_fts_rowid_by_rowid is UNIQUE for exactly
		// that reason.
		var shared int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+docidMap.ids+` a JOIN `+docidMap.ids+` b
  ON a.fts_rowid = b.fts_rowid AND a.`+viewGenColumnName+` = ? AND b.`+viewGenColumnName+` = ?`,
			baseViewGeneration, destination).Scan(&shared); err != nil {
			t.Fatalf("probe shared docids in %s: %v", docidMap.ids, err)
		}
		if shared != 0 {
			t.Fatalf("%s: %d docids are claimed by both generations", docidMap.ids, shared)
		}
	}
	integrityOK(t, store.db)
}

// The copy is repository-scoped, and the scope is the one the ordinary readers
// use: a node's own repo_prefix, and for an edge the repo_prefix of its SOURCE
// node. A second repository seeded beside the first must not ride along.
func TestCopyPayloadGenerationScopesToOneRepository(t *testing.T) {
	store := openCatalogStore(t)
	seedGenerationZero(t, store, copyFixtureRepo, 64, 128)
	seedGenerationZero(t, store, "other-repo", 64, 128)
	destination := reservedGeneration(t, store, "scoped")

	counts, err := store.CopyPayloadGeneration(context.Background(), baseViewGeneration, destination, copyFixtureRepo)
	if err != nil {
		t.Fatalf("CopyPayloadGeneration: %v", err)
	}

	handle := store.AtGeneration(destination)
	if got := len(handle.GetRepoNodes(copyFixtureRepo)); got == 0 {
		t.Fatal("the copy carried none of the repository it named")
	}
	if got := len(handle.GetRepoNodes("other-repo")); got != 0 {
		t.Fatalf("the copy carried %d nodes of a repository it was not asked for", got)
	}
	if got := len(handle.GetRepoEdges("other-repo")); got != 0 {
		t.Fatalf("the copy carried %d edges of a repository it was not asked for", got)
	}
	var foreign int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM files WHERE view_gen = ? AND repo_prefix = ?`,
		destination, "other-repo").Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if foreign != 0 {
		t.Fatalf("the copy carried %d foreign sidecar rows", foreign)
	}
	// The edge scope is the join, so the source repository's own edges must be
	// whole rather than merely non-empty.
	if got, want := len(handle.GetRepoEdges(copyFixtureRepo)), len(store.AtGeneration(baseViewGeneration).GetRepoEdges(copyFixtureRepo)); got != want {
		t.Fatalf("the copy carried %d of %d edges", got, want)
	}

	// Everything above reads the destination through the ordinary readers, and
	// for EDGES that is blind by construction: GetRepoEdges re-applies the same
	// edges→nodes join inside the destination generation, and since the nodes
	// copy IS scoped a foreign repository has no nodes there — so its leaked
	// edges are invisible to the very query hunting for them. The raw row count
	// of the destination generation is the only oracle that sees them, and it is
	// the number that matters downstream: counts.Edges is written into
	// repo_index_state.edge_count and into the build report.
	ownedNodes := int64(len(store.AtGeneration(baseViewGeneration).GetRepoNodes(copyFixtureRepo)))
	ownedEdges := int64(len(store.AtGeneration(baseViewGeneration).GetRepoEdges(copyFixtureRepo)))
	if ownedNodes == 0 || ownedEdges == 0 {
		t.Fatalf("the fixture gave generation zero %d nodes / %d edges for %q, so the scope oracle below "+
			"compares empty sets", ownedNodes, ownedEdges, copyFixtureRepo)
	}
	if got := rawGenerationRowCount(t, store, "nodes", destination); got != ownedNodes {
		t.Fatalf("the destination generation holds %d node rows in total, want the %d generation zero owns "+
			"for %q: the surplus is another repository riding along", got, ownedNodes, copyFixtureRepo)
	}
	if got := rawGenerationRowCount(t, store, "edges", destination); got != ownedEdges {
		t.Fatalf("the destination generation holds %d edge rows in total, want the %d generation zero owns "+
			"for %q: the surplus is another repository's edges riding along, and GetRepoEdges cannot see them "+
			"because the destination has no foreign nodes for its join to land on", got, ownedEdges, copyFixtureRepo)
	}
	if counts.Nodes != ownedNodes || counts.Edges != ownedEdges {
		t.Fatalf("CopyPayloadGeneration reported %+v, want %d nodes / %d edges: these counts are written to "+
			"repo_index_state and to the build report, so an inflated one misreports the base", counts, ownedNodes, ownedEdges)
	}
	// No edge may land at the destination without the source node that scoped
	// it: an orphan is an edge whose repository was never copied.
	var orphans int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM edges e WHERE e.`+viewGenColumnName+` = ?
 AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.from_id AND n.`+viewGenColumnName+` = e.`+viewGenColumnName+`)`,
		destination).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d edges at the destination have no source node there: they belong to a repository the copy "+
			"was not asked for", orphans)
	}
	// Same raw oracle for every repo_prefix-scoped sidecar and both FTS docid
	// maps, so the `files` probe above is not the only scoped table checked.
	scopedTables := append([]string{}, payloadSweepTables()...)
	for _, docidMap := range generationFTSDocidMaps {
		scopedTables = append(scopedTables, docidMap.ids)
	}
	checked := 0
	for _, table := range scopedTables {
		if !generationTableHasRepoPrefix(t, store, table) {
			continue
		}
		var rode int64
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+viewGenColumnName+` = ? AND repo_prefix <> ?`,
			destination, copyFixtureRepo).Scan(&rode); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if rode != 0 {
			t.Fatalf("%s: %d rows of a repository the copy was not asked for", table, rode)
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("only %d repository-scoped sidecars were probed, so this is not a scope oracle", checked)
	}
}

// rawGenerationRowCount counts one table's rows at one generation with NO
// repository predicate at all. That is deliberate: every repository-aware read
// path re-applies the copy's own scope, so a scope that widened would be
// invisible to all of them.
func rawGenerationRowCount(t *testing.T, store *Store, table string, generationID int64) int64 {
	t.Helper()
	var rows int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+viewGenColumnName+` = ?`, generationID).Scan(&rows); err != nil {
		t.Fatalf("count %s at generation %d: %v", table, generationID, err)
	}
	return rows
}

// generationTableHasRepoPrefix reports whether a generation-keyed table carries
// a repository column of its own. The four ownership masks do not, which is why
// the scope probe skips rather than fails on them.
func generationTableHasRepoPrefix(t *testing.T, store *Store, table string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin a read transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	columns, err := generationCopyColumns(ctx, tx, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	for _, column := range columns {
		if column == "repo_prefix" {
			return true
		}
	}
	return false
}

// Generation zero is read, never written. It is the mutable working-copy view
// the whole store composes over, and a copy that touched it would be editing
// live rows underneath every reader.
func TestCopyPayloadGenerationLeavesTheSourceAlone(t *testing.T) {
	store := openCatalogStore(t)
	seedGenerationZero(t, store, copyFixtureRepo, 128, 256)
	destination := reservedGeneration(t, store, "source-alone")

	before := map[string][]string{}
	tables := append([]string{"nodes", "edges"}, payloadSweepTables()...)
	for _, table := range tables {
		before[table] = generationRows(t, store, table, baseViewGeneration, copyFixtureRepo)
	}
	if _, err := store.CopyPayloadGeneration(context.Background(), baseViewGeneration, destination, copyFixtureRepo); err != nil {
		t.Fatalf("CopyPayloadGeneration: %v", err)
	}
	for _, table := range tables {
		after := generationRows(t, store, table, baseViewGeneration, copyFixtureRepo)
		if strings.Join(after, "\n") != strings.Join(before[table], "\n") {
			t.Fatalf("%s: the copy modified generation zero's own rows", table)
		}
	}
	var stamp int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM view_generations WHERE generation_id = ?`,
		baseViewGeneration).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp != 0 {
		t.Fatal("the copy minted a catalog row for generation zero, which would relabel the base corpus")
	}
}

// Every precondition is a refusal, not a silent no-op. Each of these would
// otherwise write rows somebody is already reading, or write them where no
// reader can address them.
func TestCopyPayloadGenerationRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("the base corpus as a destination", func(t *testing.T) {
		store := openCatalogStore(t)
		seedGenerationZero(t, store, copyFixtureRepo, 8, 8)
		source := reservedGeneration(t, store, "into-base")
		_, err := store.CopyPayloadGeneration(ctx, source, baseViewGeneration, copyFixtureRepo)
		if !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("copy into generation zero = %v, want a refusal: the base corpus is never a copy's destination", err)
		}
	})

	t.Run("onto itself", func(t *testing.T) {
		store := openCatalogStore(t)
		destination := reservedGeneration(t, store, "self")
		_, err := store.CopyPayloadGeneration(ctx, destination, destination, copyFixtureRepo)
		if !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("copy onto itself = %v, want a refusal", err)
		}
	})

	t.Run("no repository prefix", func(t *testing.T) {
		store := openCatalogStore(t)
		destination := reservedGeneration(t, store, "no-prefix")
		_, err := store.CopyPayloadGeneration(ctx, baseViewGeneration, destination, "")
		if !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("copy with no prefix = %v, want a refusal: the empty prefix names shared global externals", err)
		}
	})

	t.Run("a second copy into a generation that already holds rows", func(t *testing.T) {
		store := openCatalogStore(t)
		seedGenerationZero(t, store, copyFixtureRepo, 32, 64)
		destination := reservedGeneration(t, store, "seal")
		if _, err := store.CopyPayloadGeneration(ctx, baseViewGeneration, destination, copyFixtureRepo); err != nil {
			t.Fatalf("first copy: %v", err)
		}
		nodes := len(store.AtGeneration(destination).GetRepoNodes(copyFixtureRepo))

		_, err := store.CopyPayloadGeneration(ctx, baseViewGeneration, destination, copyFixtureRepo)
		if !errors.Is(err, ErrGenerationBulkLoadPopulated) {
			t.Fatalf("second copy = %v, want ErrGenerationBulkLoadPopulated: a whole-generation copy over "+
				"existing rows publishes a generation that is neither the source nor a re-parse", err)
		}
		if got := len(store.AtGeneration(destination).GetRepoNodes(copyFixtureRepo)); got != nodes {
			t.Fatalf("the refused second copy still wrote: %d nodes, was %d", got, nodes)
		}
	})

	t.Run("a published generation", func(t *testing.T) {
		store := openCatalogStore(t)
		seedGenerationZero(t, store, copyFixtureRepo, 8, 8)
		generationID, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
			OwnerKind: "ref_view", GraphID: "graph-copy", LayerID: "sealed",
			GenerationKind: "commit", TreeOID: "tree-sealed", CreatedAt: 10,
		})
		if err != nil {
			t.Fatalf("BeginPayloadGeneration: %v", err)
		}
		for _, row := range []ProducerCompleteness{
			{Producer: "source.snapshot", State: ProducerStateComplete},
			{Producer: "graph.syntax", State: ProducerStateComplete},
		} {
			if err := handle.SetProducerState(row); err != nil {
				t.Fatalf("SetProducerState %s: %v", row.Producer, err)
			}
		}
		if err := store.PublishPayloadGeneration(ctx, generationID, 20); err != nil {
			t.Fatalf("PublishPayloadGeneration: %v", err)
		}

		_, err = store.CopyPayloadGeneration(ctx, baseViewGeneration, generationID, copyFixtureRepo)
		if !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("copy into a published generation = %v, want ErrPayloadGenerationSealed", err)
		}
	})

	t.Run("a generation the catalog is not holding open", func(t *testing.T) {
		store := openCatalogStore(t)
		seedGenerationZero(t, store, copyFixtureRepo, 8, 8)
		_, err := store.CopyPayloadGeneration(ctx, baseViewGeneration, 4242, copyFixtureRepo)
		if !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("copy into an unreserved generation = %v, want the managed-admission refusal", err)
		}
	})
}

// The copy runs inside the generation bulk window when one is open, which is
// what gives it the bulk write shape rather than the incremental one.
//
// The window pins ONE writer connection, and beginWriteContext routes the
// copy's transaction onto it. A copy that opened its own connection would
// write at the pooled cache size with automatic checkpoints live — the shape
// the window exists to replace — and would also deadlock nothing while quietly
// costing everything, which is why this is asserted rather than assumed.
func TestCopyPayloadGenerationRidesTheGenerationBulkWindow(t *testing.T) {
	store := openCatalogStore(t)
	seedGenerationZero(t, store, copyFixtureRepo, 128, 256)
	destination := reservedGeneration(t, store, "windowed")

	opened, err := store.BeginGenerationBulkLoad(destination)
	if err != nil {
		t.Fatalf("BeginGenerationBulkLoad: %v", err)
	}
	if !opened {
		t.Fatal("the fixture store refused a window over an empty reserved generation")
	}
	pinned := store.bulkConn
	counts, err := store.CopyPayloadGeneration(context.Background(), baseViewGeneration, destination, copyFixtureRepo)
	if err != nil {
		t.Fatalf("CopyPayloadGeneration inside the window: %v", err)
	}
	if counts.Nodes == 0 {
		t.Fatal("the copy inside the window moved nothing")
	}
	if store.bulkConn != pinned {
		t.Fatal("the copy replaced the window's pinned writer connection")
	}
	if generation, held := store.InGenerationBulkLoad(); !held || generation != destination {
		t.Fatalf("after the copy the store holds window %d (held=%v), want the caller's %d still open",
			generation, held, destination)
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("EndGenerationBulkLoad: %v", err)
	}
	if got := len(store.AtGeneration(destination).GetRepoNodes(copyFixtureRepo)); got != 128 {
		t.Fatalf("the windowed copy landed %d nodes, want 128", got)
	}
	integrityOK(t, store.db)
}

// copyArmMeasurement is one arm of the write-shape comparison.
type copyArmMeasurement struct {
	name      string
	walBytes  int64
	walFrames int
	nodes     int
	edges     int
}

// The three shapes a first committed base can be written in, measured on the
// same payload in the same store.
//
//   - incremental: the shape before the generation bulk window existed — rows
//     written row-by-row through the ordinary path, with automatic checkpoints
//     draining the log every time it crosses the line.
//   - re-parse in the window: the same row-by-row write with the window's
//     shape, which is what the re-parse route now gets.
//   - copy in the window: the rows moved by INSERT … SELECT inside the window,
//     which is what a clean checkout now gets.
//
// The numbers are WAL bytes and frames, which is what the daemon actually
// carries; the assertions are relations between the arms rather than absolute
// figures, because the absolute figures are a property of this fixture's size.
func TestCopyPayloadGenerationWriteShapeAgainstTheTwoWriteRoutes(t *testing.T) {
	const line = 200
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", strconv.Itoa(line))
	// 1,500 files' worth of symbols at the corpus's measured ~4 symbols and
	// ~8 edges per file. Large enough that the incremental arm crosses the
	// auto-checkpoint line many times, which is the cost being compared.
	const nodes, edges = 6000, 12000

	arms := []copyArmMeasurement{
		measureCopyArm(t, "incremental", nodes, edges, false, false),
		measureCopyArm(t, "reparse_in_window", nodes, edges, true, false),
		measureCopyArm(t, "copy_in_window", nodes, edges, true, true),
	}
	for _, arm := range arms {
		t.Logf("generation write shape: arm=%s wal_bytes=%d wal_frames=%d nodes=%d edges=%d",
			arm.name, arm.walBytes, arm.walFrames, arm.nodes, arm.edges)
	}
	incremental, reparse, copied := arms[0], arms[1], arms[2]

	// Every arm must have produced the same payload, or the byte counts are
	// measuring different things.
	for _, arm := range arms[1:] {
		if arm.nodes != incremental.nodes || arm.edges != incremental.edges {
			t.Fatalf("arm %s landed %d/%d rows against the incremental arm's %d/%d",
				arm.name, arm.nodes, arm.edges, incremental.nodes, incremental.edges)
		}
	}
	t.Logf("generation write shape: copy/reparse WAL bytes = %d/%d (%.2fx), reparse/incremental log "+
		"high-water = %d/%d (%.2fx)",
		copied.walBytes, reparse.walBytes, float64(copied.walBytes)/float64(reparse.walBytes),
		reparse.walFrames, incremental.walFrames, float64(reparse.walFrames)/float64(incremental.walFrames))

	// The window arms accumulate their whole payload; the incremental arm is
	// drained every time it crosses the line, so the log it is left holding is
	// a fraction of what it actually wrote. (That the incremental arm really
	// is being drained mid-payload is pinned separately and directly by
	// TestGenerationBulkLoadDefersTheAutomaticDrainToOneAtItsEnd; here it is
	// the baseline the two window arms are read against.)
	if reparse.walFrames <= incremental.walFrames {
		t.Fatalf("the re-parse arm's log high-water is %d against the incremental arm's %d: its payload "+
			"was drained mid-flight, so the window is not covering it", reparse.walFrames, incremental.walFrames)
	}
	if copied.walFrames <= incremental.walFrames {
		t.Fatalf("the copy arm's log high-water is %d against the incremental arm's %d: its payload was "+
			"drained mid-flight", copied.walFrames, incremental.walFrames)
	}
	// The headline this route exists for: the same payload, in the same
	// window, costs strictly less log moved as rows than re-derived row by row
	// — and that is before the parse the copy does not do.
	if copied.walBytes >= reparse.walBytes {
		t.Fatalf("the copy wrote %d WAL bytes against the row-by-row arm's %d in the same window, so "+
			"moving the rows bought nothing", copied.walBytes, reparse.walBytes)
	}
}

// measureCopyArm writes one arm's payload into a freshly reserved generation of
// an equally seeded store and reports the log the payload itself left, measured
// before anything finalizes.
func measureCopyArm(t *testing.T, name string, nNodes, nEdges int, window, copyRoute bool) copyArmMeasurement {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = store.Close() }()
	pageSize := pragmaIntDB(t, store.db, "page_size")

	// Generation zero has to carry the payload in the copy arm and has to
	// exist at all in the others, or the cold fast path would engage on the
	// destination write and the arms would not be the same measurement.
	seedGenerationZero(t, store, copyFixtureRepo, nNodes, nEdges)
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("drain the seed WAL: %v", err)
	}
	if got := walFileBytes(t, path); got != 0 {
		t.Fatalf("arm %s starts with a %d-byte WAL, so its growth is not its own", name, got)
	}
	destination := reservedGeneration(t, store, name)
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("drain the reservation's WAL: %v", err)
	}

	if window {
		opened, err := store.BeginGenerationBulkLoad(destination)
		if err != nil {
			t.Fatalf("BeginGenerationBulkLoad in arm %s: %v", name, err)
		}
		if !opened {
			t.Fatalf("arm %s was refused a window over an empty reserved generation", name)
		}
	}
	if copyRoute {
		if _, err := store.CopyPayloadGeneration(context.Background(), baseViewGeneration, destination, copyFixtureRepo); err != nil {
			t.Fatalf("copy in arm %s: %v", name, err)
		}
	} else {
		handle, err := store.AtManagedGeneration(destination)
		if err != nil {
			t.Fatal(err)
		}
		nodes, edges := bulkFixture(nNodes, nEdges)
		for _, node := range nodes {
			node.ID = copyFixtureRepo + "/" + node.ID
			node.FilePath = copyFixtureRepo + "/" + node.FilePath
			node.RepoPrefix = copyFixtureRepo
		}
		for _, edge := range edges {
			edge.From = copyFixtureRepo + "/" + edge.From
			edge.To = copyFixtureRepo + "/" + edge.To
			edge.FilePath = copyFixtureRepo + "/" + edge.FilePath
		}
		const nodeChunk, edgeChunk = 1000, 2000
		for i := 0; i < len(nodes); i += nodeChunk {
			nodeEnd := min(i+nodeChunk, len(nodes))
			edgeStart := min(i/nodeChunk*edgeChunk, len(edges))
			edgeEnd := min(edgeStart+edgeChunk, len(edges))
			if err := handle.AddBatchChecked(nodes[i:nodeEnd], edges[edgeStart:edgeEnd]); err != nil {
				t.Fatalf("AddBatchChecked in arm %s: %v", name, err)
			}
		}
	}

	arm := copyArmMeasurement{
		name:      name,
		walBytes:  walFileBytes(t, path),
		walFrames: walFileFrames(t, path, pageSize),
	}
	if window {
		if err := store.EndGenerationBulkLoad(); err != nil {
			t.Fatalf("EndGenerationBulkLoad in arm %s: %v", name, err)
		}
	}
	handle := store.AtGeneration(destination)
	arm.nodes = len(handle.GetRepoNodes(copyFixtureRepo))
	arm.edges = len(handle.GetRepoEdges(copyFixtureRepo))
	return arm
}

// A copied generation retires without residue.
//
// This is the invariant that keeps the copy and the sweep in agreement: the
// copy writes into exactly the tables payloadSweepTables walks plus the two FTS
// projections addressed through their docid maps, which is the same
// enumeration RetirePayloadGeneration deletes through. A copy that wrote
// anywhere else would leave rows no later call could reach — the residue the
// sweep exists to prevent — and nothing about the copy itself would look wrong.
func TestACopiedGenerationRetiresWithoutResidue(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	seedGenerationZero(t, store, copyFixtureRepo, 128, 256)
	destination := reservedGeneration(t, store, "retire")
	if _, err := store.CopyPayloadGeneration(ctx, baseViewGeneration, destination, copyFixtureRepo); err != nil {
		t.Fatalf("CopyPayloadGeneration: %v", err)
	}

	tables := append([]string{"nodes", "edges"}, payloadSweepTables()...)
	carried := map[string]int{}
	for _, table := range tables {
		carried[table] = len(generationRows(t, store, table, destination, copyFixtureRepo))
	}
	if err := store.RetirePayloadGeneration(ctx, destination, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}

	for _, table := range tables {
		if left := len(generationRows(t, store, table, destination, copyFixtureRepo)); left != 0 {
			t.Fatalf("%s: %d of the %d copied rows survived the retirement, so the copy wrote outside "+
				"the set the sweep walks", table, left, carried[table])
		}
	}
	for _, docidMap := range generationFTSDocidMaps {
		var left int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+docidMap.ids+` WHERE `+viewGenColumnName+` = ?`,
			destination).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("%s: %d docid rows survived the retirement", docidMap.ids, left)
		}
		// The source's own documents are untouched: the sweep addressed the
		// copy's docids and only those.
		if got := len(ftsDocuments(t, store, docidMap, baseViewGeneration, copyFixtureRepo)); got == 0 {
			t.Fatalf("%s: retiring the copy took generation zero's documents with it", docidMap.fts)
		}
	}
	if got := len(store.AtGeneration(baseViewGeneration).GetRepoNodes(copyFixtureRepo)); got == 0 {
		t.Fatal("retiring the copy emptied generation zero")
	}
	integrityOK(t, store.db)
}
