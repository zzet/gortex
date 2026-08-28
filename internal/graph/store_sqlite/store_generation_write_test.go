package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Generation isolation for the mutating surface.
//
// Every case below seeds ONE database twice — once as the base corpus and once
// through a handle pinned to generation 1 — with byte-identical identities, then
// mutates through the derived handle only. A statement that forgot its
// generation predicate cannot show up as a missing feature here: it shows up as
// a disturbed generation-0 row, which is what these assertions read.
//
// The named tests below pin the writers whose scoping has a shape worth
// spelling out — a VALUES join, a temp-table rebind, the evict split. Every
// remaining scoped writer is one row of generationWriteCases, which drives the
// same discipline generically: seed, snapshot generation 0, mutate through the
// derived handle, require the snapshot back byte-identical.

const (
	genWriteType    = "repo::pkg/types.go::Server"
	genWriteMethod  = "repo::pkg/methods.go::Server.Run"
	genWritePhantom = "repo::pkg/methods.go::Server"
	genWriteCaller  = "repo::pkg/caller.go::Caller"

	genWriteMethodFile = "repo::pkg/methods.go"
	genWriteCallerFile = "repo::pkg/caller.go"

	// Extra material seeded per case, in the kinds the contract, config and
	// pub/sub writers select on.
	genWriteContract  = "repo::pkg/contract.go::Contract"
	genWriteConfigKey = "repo::pkg/app.yaml::server.port"
	genWriteBridge    = "repo::pkg/bridge.go::Bridge"
	genWriteTopic     = "repo::pkg/topic.go::Topic"
	genWriteDoc       = "repo::pkg/caller.go::Doc"
	genWriteSecondDoc = "repo::pkg/caller.go::Doc2"
	genWriteMissing   = "unresolved::Missing"

	genWriteRepo = "repo"
)

func generationWriteNodes() []*graph.Node {
	return []*graph.Node{
		{ID: genWriteType, Kind: graph.KindType, Name: "Server", FilePath: "repo::pkg/types.go", RepoPrefix: genWriteRepo, Language: "go"},
		{ID: genWriteMethod, Kind: graph.KindMethod, Name: "Run", FilePath: genWriteMethodFile, RepoPrefix: genWriteRepo, Language: "go"},
		{ID: genWriteCaller, Kind: graph.KindFunction, Name: "Caller", FilePath: genWriteCallerFile, RepoPrefix: genWriteRepo, Language: "go"},
	}
}

// The member_of edge points at a phantom receiver so the Go receiver rebind has
// something to repair; the calls edge carries mutable attributes so the
// attribute writers have something to rewrite.
func generationWriteEdges() []*graph.Edge {
	return []*graph.Edge{
		{From: genWriteMethod, To: genWritePhantom, Kind: graph.EdgeMemberOf, FilePath: genWriteMethodFile, Line: 10},
		{
			From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
			FilePath: genWriteCallerFile, Line: 3, Origin: "seed", Tier: "syntactic", Confidence: 0.5,
		},
	}
}

func openGenerationWritePair(t *testing.T) (base, derived *Store) {
	t.Helper()
	base, err := Open(filepath.Join(t.TempDir(), "generation_write.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := base.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	derived = base.AtGeneration(1)
	if derived == nil {
		t.Fatal("AtGeneration(1) returned nil")
	}
	base.AddBatch(generationWriteNodes(), generationWriteEdges())
	derived.AddBatch(generationWriteNodes(), generationWriteEdges())
	for _, gen := range []int64{baseViewGeneration, 1} {
		if got := edgeCountAtGeneration(t, base, gen); got != len(generationWriteEdges()) {
			t.Fatalf("seed left generation %d with %d edges, want %d", gen, got, len(generationWriteEdges()))
		}
	}
	return base, derived
}

func edgeCountAtGeneration(t *testing.T, s *Store, viewGen int64) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE view_gen = ?`, viewGen).Scan(&count); err != nil {
		t.Fatalf("count edges at generation %d: %v", viewGen, err)
	}
	return count
}

func nodeCountInFileAtGeneration(t *testing.T, s *Store, viewGen int64, filePath string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND file_path = ?`, viewGen, filePath,
	).Scan(&count); err != nil {
		t.Fatalf("count nodes in %q at generation %d: %v", filePath, viewGen, err)
	}
	return count
}

type generationEdgeAttrs struct {
	origin     string
	tier       string
	confidence float64
	found      bool
}

func edgeAttrsAtGeneration(t *testing.T, s *Store, viewGen int64, from, to string) generationEdgeAttrs {
	t.Helper()
	var attrs generationEdgeAttrs
	rows, err := s.db.Query(
		`SELECT origin, tier, confidence FROM edges WHERE view_gen = ? AND from_id = ? AND to_id = ?`,
		viewGen, from, to,
	)
	if err != nil {
		t.Fatalf("read edge attrs at generation %d: %v", viewGen, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&attrs.origin, &attrs.tier, &attrs.confidence); err != nil {
			t.Fatalf("scan edge attrs: %v", err)
		}
		attrs.found = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate edge attrs: %v", err)
	}
	return attrs
}

func memberTargetAtGeneration(t *testing.T, s *Store, viewGen int64, from string) string {
	t.Helper()
	var target string
	rows, err := s.db.Query(
		`SELECT to_id FROM edges WHERE view_gen = ? AND from_id = ? AND kind = ?`,
		viewGen, from, string(graph.EdgeMemberOf),
	)
	if err != nil {
		t.Fatalf("read member target at generation %d: %v", viewGen, err)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		if err := rows.Scan(&target); err != nil {
			t.Fatalf("scan member target: %v", err)
		}
		found++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate member targets: %v", err)
	}
	if found != 1 {
		t.Fatalf("generation %d holds %d member_of edges from %q, want exactly 1", viewGen, found, from)
	}
	return target
}

// TestGenerationScopedEdgeAttributeUpdate covers both attribute writers: the
// single-edge UPDATE and the batched VALUES join.
func TestGenerationScopedEdgeAttributeUpdate(t *testing.T) {
	base, derived := openGenerationWritePair(t)

	promoted := &graph.Edge{
		From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
		FilePath: genWriteCallerFile, Line: 3, Origin: "derived-single", Tier: "semantic", Confidence: 0.91,
	}
	derived.PersistEdgeAttributes(promoted)

	if got := edgeAttrsAtGeneration(t, base, 1, genWriteCaller, genWriteMethod); got.origin != "derived-single" || got.confidence != 0.91 {
		t.Fatalf("generation-1 edge attrs = %+v, want the derived handle's write", got)
	}
	if got := edgeAttrsAtGeneration(t, base, baseViewGeneration, genWriteCaller, genWriteMethod); got.origin != "seed" || got.confidence != 0.5 {
		t.Fatalf("a derived-handle attribute write disturbed the base corpus: %+v", got)
	}

	batched := &graph.Edge{
		From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
		FilePath: genWriteCallerFile, Line: 3, Origin: "derived-batch", Tier: "semantic", Confidence: 0.77,
	}
	derived.PersistEdgeAttributesBatch([]*graph.Edge{batched})

	if got := edgeAttrsAtGeneration(t, base, 1, genWriteCaller, genWriteMethod); got.origin != "derived-batch" || got.confidence != 0.77 {
		t.Fatalf("generation-1 edge attrs after the batched write = %+v", got)
	}
	if got := edgeAttrsAtGeneration(t, base, baseViewGeneration, genWriteCaller, genWriteMethod); got.origin != "seed" || got.confidence != 0.5 {
		t.Fatalf("a derived-handle batched attribute write disturbed the base corpus: %+v", got)
	}

	// The reverse direction holds too: a base-handle write leaves the derived
	// generation alone, so neither is merely the default the other falls back to.
	base.PersistEdgeAttributes(&graph.Edge{
		From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
		FilePath: genWriteCallerFile, Line: 3, Origin: "base-write", Tier: "semantic", Confidence: 0.25,
	})
	if got := edgeAttrsAtGeneration(t, base, 1, genWriteCaller, genWriteMethod); got.origin != "derived-batch" {
		t.Fatalf("a base-handle attribute write disturbed generation 1: %+v", got)
	}
}

// TestGenerationScopedEdgeProvenanceUpdate covers the origin-only writers,
// whose stored-origin preflight decides whether a promotion happens at all.
func TestGenerationScopedEdgeProvenanceUpdate(t *testing.T) {
	base, derived := openGenerationWritePair(t)

	edge := &graph.Edge{
		From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
		FilePath: genWriteCallerFile, Line: 3, Origin: "seed", Tier: "syntactic",
	}
	if !derived.SetEdgeProvenance(edge, "go-types") {
		t.Fatal("SetEdgeProvenance through the derived handle reported no change")
	}
	if got := edgeAttrsAtGeneration(t, base, 1, genWriteCaller, genWriteMethod); got.origin != "go-types" {
		t.Fatalf("generation-1 origin = %q, want the promoted value", got.origin)
	}
	if got := edgeAttrsAtGeneration(t, base, baseViewGeneration, genWriteCaller, genWriteMethod); got.origin != "seed" {
		t.Fatalf("a derived-handle provenance promotion disturbed the base corpus: %+v", got)
	}

	batchEdge := &graph.Edge{
		From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
		FilePath: genWriteCallerFile, Line: 3, Origin: "go-types", Tier: "syntactic",
	}
	if changed := derived.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{
		{Edge: batchEdge, NewOrigin: "lsp"},
	}); changed != 1 {
		t.Fatalf("SetEdgeProvenanceBatch changed = %d, want 1", changed)
	}
	if got := edgeAttrsAtGeneration(t, base, 1, genWriteCaller, genWriteMethod); got.origin != "lsp" {
		t.Fatalf("generation-1 origin after the batch = %q", got.origin)
	}
	if got := edgeAttrsAtGeneration(t, base, baseViewGeneration, genWriteCaller, genWriteMethod); got.origin != "seed" {
		t.Fatalf("a derived-handle batched promotion disturbed the base corpus: %+v", got)
	}
}

// TestGenerationScopedExactEdgeDelete covers the exact-identity delete, whose
// VALUES join carries the full logical key and now the generation with it.
func TestGenerationScopedExactEdgeDelete(t *testing.T) {
	base, derived := openGenerationWritePair(t)

	doomed := &graph.Edge{
		From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
		FilePath: genWriteCallerFile, Line: 3,
	}
	if removed := derived.RemoveEdgesExact([]*graph.Edge{doomed}); removed != 1 {
		t.Fatalf("RemoveEdgesExact removed %d rows, want 1", removed)
	}
	if got := edgeAttrsAtGeneration(t, base, 1, genWriteCaller, genWriteMethod); got.found {
		t.Fatalf("generation-1 edge survived its own delete: %+v", got)
	}
	if got := edgeAttrsAtGeneration(t, base, baseViewGeneration, genWriteCaller, genWriteMethod); !got.found || got.origin != "seed" {
		t.Fatalf("a derived-handle exact delete removed the base corpus's edge: %+v", got)
	}

	// RemoveEdge deletes by the (from, to, kind) prefix instead, and binds the
	// generation the same way.
	if !derived.RemoveEdge(genWriteMethod, genWritePhantom, graph.EdgeMemberOf) {
		t.Fatal("RemoveEdge through the derived handle reported no deletion")
	}
	if got := edgeCountAtGeneration(t, base, 1); got != 0 {
		t.Fatalf("generation 1 holds %d edges after both deletes, want 0", got)
	}
	if got := edgeCountAtGeneration(t, base, baseViewGeneration); got != len(generationWriteEdges()) {
		t.Fatalf("base corpus holds %d edges after the derived deletes, want %d", got, len(generationWriteEdges()))
	}
}

// TestGenerationScopedFileEvict pins both halves of the eviction split: ordinary
// file and repository replacement is generation-scoped, while the explicit
// all-generation repository capability is reserved for administration.
func TestGenerationScopedFileEvict(t *testing.T) {
	base, derived := openGenerationWritePair(t)

	nodes, edges := derived.EvictFile(genWriteCallerFile)
	if nodes != 1 || edges != 1 {
		t.Fatalf("EvictFile removed (%d nodes, %d edges), want (1, 1)", nodes, edges)
	}
	if got := nodeCountInFileAtGeneration(t, base, 1, genWriteCallerFile); got != 0 {
		t.Fatalf("generation 1 kept %d nodes of the evicted file", got)
	}
	if got := nodeCountInFileAtGeneration(t, base, baseViewGeneration, genWriteCallerFile); got != 1 {
		t.Fatalf("a derived-handle file eviction removed the base corpus's node: %d rows left", got)
	}
	if got := edgeAttrsAtGeneration(t, base, baseViewGeneration, genWriteCaller, genWriteMethod); !got.found {
		t.Fatal("a derived-handle file eviction removed the base corpus's incident edge")
	}
	if got := edgeCountAtGeneration(t, base, 1); got != len(generationWriteEdges())-1 {
		t.Fatalf("generation 1 holds %d edges after the file eviction, want %d", got, len(generationWriteEdges())-1)
	}

	// The batch form takes the same scope.
	if nodes, _ := derived.EvictFiles([]string{genWriteMethodFile}); nodes != 1 {
		t.Fatalf("EvictFiles removed %d nodes, want 1", nodes)
	}
	if got := nodeCountInFileAtGeneration(t, base, baseViewGeneration, genWriteMethodFile); got != 1 {
		t.Fatalf("a derived-handle batch eviction removed the base corpus's node: %d rows left", got)
	}

	// Ordinary repository replacement follows the calling handle exactly like
	// file eviction. The base corpus is removed while the derived generation's
	// one untouched type node survives.
	nodes, edges = base.EvictRepo(genWriteRepo)
	if nodes != len(generationWriteNodes()) || edges != len(generationWriteEdges()) {
		t.Fatalf("EvictRepo removed (%d nodes, %d edges), want (%d, %d)",
			nodes, edges, len(generationWriteNodes()), len(generationWriteEdges()))
	}
	for gen, want := range map[int64]int{baseViewGeneration: 0, 1: 1} {
		var count int
		if err := base.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen = ?`, gen).Scan(&count); err != nil {
			t.Fatalf("count nodes at generation %d: %v", gen, err)
		}
		if count != want {
			t.Fatalf("EvictRepo left %d nodes at generation %d, want %d", count, gen, want)
		}
	}

	// Authoritative repository removal opts into the destructive capability and
	// clears the surviving payload from every generation.
	nodes, edges = base.EvictRepoAllGenerations(genWriteRepo)
	if nodes != 1 || edges != 0 {
		t.Fatalf("EvictRepoAllGenerations removed (%d nodes, %d edges), want (1, 0)", nodes, edges)
	}
	for _, gen := range []int64{baseViewGeneration, 1} {
		var count int
		if err := base.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen = ?`, gen).Scan(&count); err != nil {
			t.Fatalf("count nodes after all-generation eviction at generation %d: %v", gen, err)
		}
		if count != 0 {
			t.Fatalf("EvictRepoAllGenerations left %d nodes at generation %d", count, gen)
		}
	}
}

// TestGenerationScopedReceiverRebind covers the temp-table rebind: the
// candidate collection joins four relations, and all four must come from the
// writing handle's generation or the repair reads one corpus and writes another.
func TestGenerationScopedReceiverRebind(t *testing.T) {
	base, derived := openGenerationWritePair(t)

	changed, err := derived.RebindGoMethodReceivers("")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if changed != 1 {
		t.Fatalf("rebind changed %d candidates, want 1", changed)
	}
	if got := memberTargetAtGeneration(t, base, 1, genWriteMethod); got != genWriteType {
		t.Fatalf("generation-1 receiver = %q, want the canonical type %q", got, genWriteType)
	}
	if got := memberTargetAtGeneration(t, base, baseViewGeneration, genWriteMethod); got != genWritePhantom {
		t.Fatalf("a derived-handle rebind rewrote the base corpus's receiver: %q", got)
	}

	// The base handle still has its own phantom to repair, which proves the
	// derived pass consumed only its own candidates.
	changed, err = base.RebindGoMethodReceiversForFiles([]string{genWriteMethodFile})
	if err != nil {
		t.Fatalf("batch rebind: %v", err)
	}
	if changed != 1 {
		t.Fatalf("batch rebind changed %d candidates, want 1", changed)
	}
	if got := memberTargetAtGeneration(t, base, baseViewGeneration, genWriteMethod); got != genWriteType {
		t.Fatalf("base-corpus receiver = %q, want the canonical type %q", got, genWriteType)
	}
	if got := memberTargetAtGeneration(t, base, 1, genWriteMethod); got != genWriteType {
		t.Fatalf("the base-handle batch rebind disturbed generation 1: %q", got)
	}
}

// TestGenerationScopedEvictPlansStayIndexed keeps the residual generation
// conjunct a filter on an indexed seek. The file eviction runs once per changed
// file during incremental indexing; a scan of nodes or edges here is the
// regression this fence exists to catch.
func TestGenerationScopedEvictPlansStayIndexed(t *testing.T) {
	s := newPlanLockFixture(t)
	cases := []struct {
		name   string
		query  string
		args   int
		want   string
		forbid []string
	}{
		{
			name:  "evict_file_nodes",
			query: `DELETE FROM nodes WHERE ` + evictFilePredicate + ` AND view_gen = ?`,
			args:  2,
			want:  "nodes_by_file (file_path=?)",
		},
		{
			name: "evict_file_edges",
			query: `DELETE FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE ` +
				evictFilePredicate + ` AND view_gen = ?) AND view_gen = ?`,
			args:   3,
			want:   "nodes_by_file (file_path=?)",
			forbid: []string{"SCAN edges"},
		},
		{
			name:  "evict_repo_nodes_unscoped",
			query: `DELETE FROM nodes WHERE ` + evictNonEmptyRepoPredicate,
			args:  1,
			want:  "nodes_by_repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, s, tc.query, tc.args)
			joined := strings.Join(plan, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("plan missing %q:\n%s", tc.want, joined)
			}
			for _, forbidden := range tc.forbid {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("plan contains forbidden %q:\n%s", forbidden, joined)
				}
			}
			if strings.Contains(joined, "SCAN nodes") {
				t.Fatalf("generation scoping demoted an indexed seek to a node scan:\n%s", joined)
			}
		})
	}
}

// --- the remaining writer families ----------------------------------------

// generationWriteCase is one scoped writer driven through the derived handle
// only. seed adds whatever material the writer selects on; tables names the
// sidecars it touches beyond nodes and edges.
type generationWriteCase struct {
	name    string
	tables  []string
	seed    func(t *testing.T, base, derived *Store)
	disturb func(t *testing.T, s *Store)
	// verifyBase reaches the state a row snapshot cannot: the FTS5 virtual
	// tables carry no generation column, so a document lost from one is
	// invisible in its ownership sidecar.
	verifyBase func(t *testing.T, base *Store)
}

// generationTableSnapshot renders every row one generation owns in a table as
// sorted text. Column names come from the result set, so a new column joins the
// comparison without touching this helper.
func generationTableSnapshot(t *testing.T, s *Store, table string, viewGen int64) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT * FROM `+table+` WHERE view_gen = ?`, viewGen)
	if err != nil {
		t.Fatalf("snapshot %s at generation %d: %v", table, viewGen, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		parts := make([]string, 0, len(columns))
		for i, column := range columns {
			parts = append(parts, fmt.Sprintf("%s=%v", column, cells[i]))
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// addToBothGenerations seeds identical extra material into both generations.
// build runs once per handle so neither handle stores values AddBatch mutated
// on the other's behalf.
func addToBothGenerations(base, derived *Store, build func() ([]*graph.Node, []*graph.Edge)) {
	for _, s := range []*Store{base, derived} {
		nodes, edges := build()
		s.AddBatch(nodes, edges)
	}
}

func (tc generationWriteCase) run(t *testing.T) {
	base, derived := openGenerationWritePair(t)
	if tc.seed != nil {
		tc.seed(t, base, derived)
	}
	tables := append([]string{"nodes", "edges"}, tc.tables...)
	baseBefore := make(map[string]string, len(tables))
	derivedBefore := make(map[string]string, len(tables))
	for _, table := range tables {
		baseBefore[table] = generationTableSnapshot(t, base, table, baseViewGeneration)
		derivedBefore[table] = generationTableSnapshot(t, base, table, 1)
	}

	tc.disturb(t, derived)

	// Without this the case would also pass for a writer that did nothing at
	// all, which proves nothing about where its rows went.
	moved := false
	for _, table := range tables {
		if generationTableSnapshot(t, base, table, 1) != derivedBefore[table] {
			moved = true
		}
	}
	if !moved {
		t.Fatal("the writer changed no generation-1 row, so the case cannot tell scoping from inaction")
	}
	for _, table := range tables {
		after := generationTableSnapshot(t, base, table, baseViewGeneration)
		if after != baseBefore[table] {
			t.Fatalf("a derived-handle write disturbed the base corpus's %s rows:\nbefore:\n%s\nafter:\n%s",
				table, baseBefore[table], after)
		}
	}
	if tc.verifyBase != nil {
		tc.verifyBase(t, base)
	}
}

//nolint:gocyclo // one row per writer family; the length is the coverage.
func generationWriteCases() []generationWriteCase {
	callsEdge := func() *graph.Edge {
		return &graph.Edge{
			From: genWriteCaller, To: genWriteMethod, Kind: graph.EdgeCalls,
			FilePath: genWriteCallerFile, Line: 3,
		}
	}
	// The seeded token deliberately differs from the node's Name: an exact-name
	// query short-circuits ahead of FTS, so a search for "Run" would answer
	// from the nodes table even with the document gone.
	seedSymbolFTS := func(t *testing.T, base, derived *Store) {
		t.Helper()
		for _, s := range []*Store{base, derived} {
			if err := s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: genWriteMethod, Tokens: "RunHandler"}}); err != nil {
				t.Fatalf("seed symbol fts: %v", err)
			}
		}
	}
	verifyBaseSymbolFTS := func(t *testing.T, base *Store) {
		t.Helper()
		hits, err := base.SearchSymbols("RunHandler", 8)
		if err != nil {
			t.Fatalf("base symbol search: %v", err)
		}
		if len(hits) != 1 || hits[0].NodeID != genWriteMethod {
			t.Fatalf("a derived-handle FTS write took the base corpus's document with it: %+v", hits)
		}
	}

	return []generationWriteCase{
		{
			name: "workspace_slug_backfill",
			disturb: func(t *testing.T, s *Store) {
				changed := s.BackfillWorkspaceSlugs([]graph.WorkspaceSlug{
					{RepoPrefix: genWriteRepo, Workspace: "ws", Project: "proj"},
				})
				if changed != len(generationWriteNodes()) {
					t.Fatalf("BackfillWorkspaceSlugs changed %d rows, want %d", changed, len(generationWriteNodes()))
				}
			},
		},
		{
			name: "workspace_slug_backfill_with_impact",
			disturb: func(t *testing.T, s *Store) {
				got := s.BackfillWorkspaceSlugsWithImpact([]graph.WorkspaceSlug{
					{RepoPrefix: genWriteRepo, Workspace: "ws", Project: "proj"},
				})
				want := len(generationWriteNodes())
				if got.Changed != want || got.ResolutionAffected != want {
					t.Fatalf("BackfillWorkspaceSlugsWithImpact = %+v, want %d changed and %d resolution-affected",
						got, want, want)
				}
			},
		},
		{
			name: "semantic_node_stamps",
			disturb: func(t *testing.T, s *Store) {
				enriched := s.PersistSemanticNodeStamps([]graph.SemanticNodeStamp{
					{NodeID: genWriteMethod, SemanticType: "Server", ReturnType: "error", SemanticSource: "lsp"},
				})
				if enriched != 1 {
					t.Fatalf("PersistSemanticNodeStamps enriched %d nodes, want 1", enriched)
				}
			},
		},
		{
			name: "edge_terminal_stamps",
			disturb: func(t *testing.T, s *Store) {
				stamped := callsEdge()
				stamped.Meta = map[string]any{
					graph.EdgeMetaResolveTerminal:       true,
					graph.EdgeMetaResolveTerminalReason: "exhausted",
				}
				s.PersistEdgeTerminalStamps([]*graph.Edge{stamped})
			},
		},
		{
			name: "cross_repo_flags",
			disturb: func(t *testing.T, s *Store) {
				if changed := s.MarkEdgesCrossRepo([]*graph.Edge{callsEdge()}); changed != 1 {
					t.Fatalf("MarkEdgesCrossRepo changed %d edges, want 1", changed)
				}
			},
		},
		{
			name: "edge_kind_evict",
			disturb: func(t *testing.T, s *Store) {
				if removed := s.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeCalls}); removed != 1 {
					t.Fatalf("EvictEdgesByKinds removed %d edges, want 1", removed)
				}
			},
		},
		{
			name: "scoped_edge_kind_evict",
			disturb: func(t *testing.T, s *Store) {
				removed, err := s.EvictEdgesFromSourcesByKinds(
					context.Background(), []string{genWriteCaller}, []graph.EdgeKind{graph.EdgeCalls})
				if err != nil {
					t.Fatalf("EvictEdgesFromSourcesByKinds: %v", err)
				}
				if removed != 1 {
					t.Fatalf("EvictEdgesFromSourcesByKinds removed %d edges, want 1", removed)
				}
			},
		},
		{
			name: "reindex_edges",
			disturb: func(t *testing.T, s *Store) {
				rebound := callsEdge()
				rebound.To = genWriteType
				rebound.Origin = "seed"
				rebound.Tier = "syntactic"
				rebound.Confidence = 0.5
				s.ReindexEdges([]graph.EdgeReindex{{Edge: rebound, OldTo: genWriteMethod}})
				if !s.EdgeExists(genWriteCaller, genWriteType, graph.EdgeCalls, genWriteCallerFile, 3) {
					t.Fatal("the rebound edge is missing from the generation it was written through")
				}
			},
		},
		{
			name: "unresolved_target_reindex",
			seed: func(t *testing.T, base, derived *Store) {
				addToBothGenerations(base, derived, func() ([]*graph.Node, []*graph.Edge) {
					return nil, []*graph.Edge{{
						From: genWriteCaller, To: genWriteMissing, Kind: graph.EdgeCalls,
						FilePath: genWriteCallerFile, Line: 7, Confidence: 0.4,
					}}
				})
			},
			disturb: func(t *testing.T, s *Store) {
				s.ReindexUnresolvedEdgeTargets([]graph.UnresolvedEdgeTargetReindex{{
					Old: graph.EdgeIdentity{
						From: genWriteCaller, To: genWriteMissing, Kind: graph.EdgeCalls,
						FilePath: genWriteCallerFile, Line: 7,
					},
					NewTo: genWriteType,
				}})
				if !s.EdgeExists(genWriteCaller, genWriteType, graph.EdgeCalls, genWriteCallerFile, 7) {
					t.Fatal("the retargeted edge is missing from the generation it was written through")
				}
			},
		},
		{
			name: "config_node_evict",
			seed: func(t *testing.T, base, derived *Store) {
				addToBothGenerations(base, derived, func() ([]*graph.Node, []*graph.Edge) {
					return []*graph.Node{{
						ID: genWriteConfigKey, Kind: graph.KindConfigKey, Name: "server.port",
						FilePath: "repo::pkg/app.yaml", RepoPrefix: genWriteRepo,
					}}, []*graph.Edge{{
						From: genWriteCaller, To: genWriteConfigKey, Kind: graph.EdgeReads,
						FilePath: genWriteCallerFile, Line: 8,
					}}
				})
			},
			disturb: func(t *testing.T, s *Store) {
				nodes, edges := s.EvictConfigNodesByIDs([]string{genWriteConfigKey})
				if nodes != 1 || edges != 1 {
					t.Fatalf("EvictConfigNodesByIDs removed (%d nodes, %d edges), want (1, 1)", nodes, edges)
				}
			},
		},
		{
			name: "contract_node_evict",
			seed: func(t *testing.T, base, derived *Store) {
				addToBothGenerations(base, derived, generationWriteContractMaterial)
			},
			disturb: func(t *testing.T, s *Store) {
				nodes, edges := s.EvictContractNodesByIDs([]string{genWriteContract})
				if nodes != 1 || edges != 1 {
					t.Fatalf("EvictContractNodesByIDs removed (%d nodes, %d edges), want (1, 1)", nodes, edges)
				}
			},
		},
		{
			name: "contract_owner_replace",
			seed: func(t *testing.T, base, derived *Store) {
				addToBothGenerations(base, derived, generationWriteContractMaterial)
			},
			disturb: func(t *testing.T, s *Store) {
				result, err := s.ReplaceContractOwners(graph.ContractOwnerReplacement{
					RepoPrefix:     genWriteRepo,
					FilePaths:      []string{genWriteCallerFile},
					TouchedNodeIDs: []string{genWriteContract},
				})
				if err != nil {
					t.Fatalf("ReplaceContractOwners: %v", err)
				}
				if result.EdgesRemoved != 1 || result.NodesRemoved != 1 {
					t.Fatalf("ReplaceContractOwners = %+v, want one owner edge and its orphaned contract removed", result)
				}
			},
		},
		{
			name: "derived_contract_replace",
			seed: func(t *testing.T, base, derived *Store) {
				addToBothGenerations(base, derived, func() ([]*graph.Node, []*graph.Edge) {
					return []*graph.Node{
						{
							ID: genWriteBridge, Kind: graph.KindContractBridge, Name: "Bridge",
							FilePath: "repo::pkg/bridge.go", RepoPrefix: genWriteRepo,
						},
						{
							ID: genWriteTopic, Kind: graph.KindTopic, Name: "Topic",
							FilePath: "repo::pkg/topic.go", RepoPrefix: genWriteRepo,
						},
					}, []*graph.Edge{
						{
							From: genWriteCaller, To: genWriteBridge, Kind: graph.EdgeReferences,
							FilePath: genWriteCallerFile, Line: 6,
						},
						{
							From: genWriteCaller, To: genWriteTopic, Kind: graph.EdgeProducesTopic,
							FilePath: genWriteCallerFile, Line: 7,
						},
					}
				})
			},
			disturb: func(t *testing.T, s *Store) {
				result, err := s.ReplaceDerivedContracts(graph.DerivedContractReplacement{
					RemoveEdges: []*graph.Edge{{
						From: genWriteCaller, To: genWriteTopic, Kind: graph.EdgeProducesTopic,
						FilePath: genWriteCallerFile, Line: 7,
					}},
					RemoveBridgeNodeIDs: []string{genWriteBridge},
					TouchedTopicNodeIDs: []string{genWriteTopic},
				})
				if err != nil {
					t.Fatalf("ReplaceDerivedContracts: %v", err)
				}
				if result.EdgesRemoved != 2 || result.NodesRemoved != 2 {
					t.Fatalf("ReplaceDerivedContracts = %+v, want the bridge and the orphaned topic removed with their edges", result)
				}
			},
		},
		{
			name:   "clone_signatures",
			tables: []string{"clone_shingles"},
			seed: func(t *testing.T, base, derived *Store) {
				t.Helper()
				for _, s := range []*Store{base, derived} {
					if err := s.BulkSetCloneCorpus(genWriteRepo, []graph.CloneCorpusRow{{
						NodeID: genWriteMethod, RepoPrefix: genWriteRepo,
						Shingles: []uint64{1, 2, 3}, TokenCount: 3,
					}}); err != nil {
						t.Fatalf("seed clone corpus: %v", err)
					}
				}
			},
			disturb: func(t *testing.T, s *Store) {
				if err := s.BulkSetCloneSignatures(genWriteRepo, []graph.CloneCorpusSignatureUpdate{
					{NodeID: genWriteMethod, Signature: "derived-signature"},
				}); err != nil {
					t.Fatalf("BulkSetCloneSignatures: %v", err)
				}
			},
		},
		{
			name:   "ref_facts_rebuild",
			tables: []string{"ref_facts"},
			seed: func(t *testing.T, base, derived *Store) {
				t.Helper()
				if err := base.RebuildRefFactsForRepos([]string{genWriteRepo}); err != nil {
					t.Fatalf("seed base ref facts: %v", err)
				}
			},
			disturb: func(t *testing.T, s *Store) {
				if err := s.RebuildRefFactsForRepos([]string{genWriteRepo}); err != nil {
					t.Fatalf("RebuildRefFactsForRepos: %v", err)
				}
			},
		},
		{
			name:   "ref_facts_replace_files",
			tables: []string{"ref_facts"},
			seed: func(t *testing.T, base, derived *Store) {
				t.Helper()
				if err := base.ReplaceRefFactsForFiles(genWriteRepo, []string{genWriteCallerFile}); err != nil {
					t.Fatalf("seed base ref facts: %v", err)
				}
			},
			disturb: func(t *testing.T, s *Store) {
				if err := s.ReplaceRefFactsForFiles(genWriteRepo, []string{genWriteCallerFile}); err != nil {
					t.Fatalf("ReplaceRefFactsForFiles: %v", err)
				}
			},
		},
		{
			name:       "symbol_fts_batch_upsert",
			tables:     []string{"symbol_fts_rowid"},
			seed:       seedSymbolFTS,
			verifyBase: verifyBaseSymbolFTS,
			// Re-tokenising the seeded symbol exercises the reuse-the-old-docid
			// path; the second, unseeded symbol is what makes the sidecar move
			// even though the first keeps its docid.
			disturb: func(t *testing.T, s *Store) {
				err := s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
					{NodeID: genWriteMethod, Tokens: "RunHandlerAgain"},
					{NodeID: genWriteCaller, Tokens: "CallerTokens"},
				})
				if err != nil {
					t.Fatalf("BatchUpsertSymbolFTS: %v", err)
				}
			},
		},
		{
			name:       "symbol_fts_batch_delete",
			tables:     []string{"symbol_fts_rowid"},
			seed:       seedSymbolFTS,
			verifyBase: verifyBaseSymbolFTS,
			disturb: func(t *testing.T, s *Store) {
				if err := s.BatchDeleteSymbolFTS([]string{genWriteMethod}); err != nil {
					t.Fatalf("BatchDeleteSymbolFTS: %v", err)
				}
			},
		},
		{
			name:       "symbol_fts_repo_reset",
			tables:     []string{"symbol_fts_rowid"},
			seed:       seedSymbolFTS,
			verifyBase: verifyBaseSymbolFTS,
			disturb: func(t *testing.T, s *Store) {
				if err := s.ResetSymbolFTS(genWriteRepo); err != nil {
					t.Fatalf("ResetSymbolFTS: %v", err)
				}
			},
		},
		{
			name:       "symbol_fts_repo_replace",
			tables:     []string{"symbol_fts_rowid"},
			seed:       seedSymbolFTS,
			verifyBase: verifyBaseSymbolFTS,
			disturb: func(t *testing.T, s *Store) {
				err := s.ReplaceSymbolFTS(genWriteRepo, func(emit func([]graph.SymbolFTSItem) error) error {
					return emit([]graph.SymbolFTSItem{{NodeID: genWriteCaller, Tokens: "Caller"}})
				})
				if err != nil {
					t.Fatalf("ReplaceSymbolFTS: %v", err)
				}
			},
		},
		{
			name:   "content_fts_replace",
			tables: []string{"content_fts_rowid"},
			verifyBase: func(t *testing.T, base *Store) {
				hits, err := base.SearchContent("seeded", "", 8)
				if err != nil {
					t.Fatalf("base content search: %v", err)
				}
				if len(hits) != 1 || hits[0].NodeID != genWriteDoc {
					t.Fatalf("a derived-handle content replacement took the base corpus's section with it: %+v", hits)
				}
			},
			seed: func(t *testing.T, base, derived *Store) {
				t.Helper()
				for _, s := range []*Store{base, derived} {
					if err := s.AppendContent(genWriteRepo, []graph.ContentFTSItem{
						{NodeID: genWriteDoc, FilePath: genWriteCallerFile, Ordinal: 0, Body: "seeded body"},
					}); err != nil {
						t.Fatalf("seed content: %v", err)
					}
				}
			},
			// The replacement carries two sections where the seed had one: a
			// reallocated docid can land on the value it just freed, so the
			// section count is what makes the derived generation observably move.
			disturb: func(t *testing.T, s *Store) {
				err := s.ReplaceContentFiles(genWriteRepo, []graph.ContentFTSFileReplacement{{
					FilePath: genWriteCallerFile,
					Items: []graph.ContentFTSItem{
						{NodeID: genWriteDoc, FilePath: genWriteCallerFile, Ordinal: 0, Body: "replaced body"},
						{NodeID: genWriteSecondDoc, FilePath: genWriteCallerFile, Ordinal: 1, Body: "second section"},
					},
				}})
				if err != nil {
					t.Fatalf("ReplaceContentFiles: %v", err)
				}
			},
		},
	}
}

// generationWriteContractMaterial is the contract node plus the owner edge both
// the contract evicter and the owner replacer select on.
func generationWriteContractMaterial() ([]*graph.Node, []*graph.Edge) {
	return []*graph.Node{{
		ID: genWriteContract, Kind: graph.KindContract, Name: "Contract",
		FilePath: "repo::pkg/contract.go", RepoPrefix: genWriteRepo,
	}}, []*graph.Edge{{
		From: genWriteCaller, To: genWriteContract, Kind: graph.EdgeProvides,
		FilePath: genWriteCallerFile, Line: 5,
	}}
}

// TestGenerationScopedWriterFamilies drives every remaining scoped writer
// through the derived handle and requires the base corpus back byte-identical.
func TestGenerationScopedWriterFamilies(t *testing.T) {
	for _, tc := range generationWriteCases() {
		t.Run(tc.name, func(t *testing.T) { tc.run(t) })
	}
}

// generationWriteFences maps the name of every standalone write-fence test to
// the test itself. The read fence's capability checklist cites these names as
// the isolation proof for a mutating capability, so they have to stay bound to
// something that actually runs.
var generationWriteFences = map[string]func(t *testing.T){
	"TestGenerationScopedEdgeAttributeUpdate":  TestGenerationScopedEdgeAttributeUpdate,
	"TestGenerationScopedEdgeProvenanceUpdate": TestGenerationScopedEdgeProvenanceUpdate,
	"TestGenerationScopedExactEdgeDelete":      TestGenerationScopedExactEdgeDelete,
	"TestGenerationScopedFileEvict":            TestGenerationScopedFileEvict,
	"TestGenerationScopedReceiverRebind":       TestGenerationScopedReceiverRebind,
}

// generationWriteFenceNames is every name a capability may cite: the standalone
// tests plus one subtest per writer-family case.
func generationWriteFenceNames() map[string]struct{} {
	cases := generationWriteCases()
	out := make(map[string]struct{}, len(generationWriteFences)+len(cases))
	for name := range generationWriteFences {
		out[name] = struct{}{}
	}
	for _, tc := range cases {
		out["TestGenerationScopedWriterFamilies/"+tc.name] = struct{}{}
	}
	return out
}

// TestGenerationWriteFenceNamesAreReal keeps each cited name bound to the test
// that implements it. The map values are the functions themselves, so a rename
// that misses its key is caught here instead of leaving the checklist pointing
// at a name no test carries.
func TestGenerationWriteFenceNamesAreReal(t *testing.T) {
	for name, fn := range generationWriteFences {
		implemented := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
		if i := strings.LastIndex(implemented, "."); i >= 0 {
			implemented = implemented[i+1:]
		}
		if implemented != name {
			t.Errorf("write fence %q is implemented by %s", name, implemented)
		}
	}
}
