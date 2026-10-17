package store_sqlite

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// privateBaselineEdgeCandidatesEndpointQuery freezes the old expression for
// attribution and parity after the production builder changes.
func privateBaselineEdgeCandidatesEndpointQuery(pairs int) string {
	return `WITH wanted(from_id, to_id) AS (VALUES ` + edgeCandidatesValues(pairs, "(?, ?)") + `)
		SELECT ` + lookupQualifiedEdgeCols + `
		  FROM wanted AS w
		  JOIN edges AS e ON e.from_id = w.from_id AND e.to_id = w.to_id
		 WHERE e.view_gen = ?`
}

func TestPrivateEdgeCandidatesEndpointGenerationQualifierUsesLogicalUniqueIndex(t *testing.T) {
	db := privateOpenEdgeEndpointPlanDB(t)
	pairs := make([]privateEdgeEndpoint, 0, 65)
	for i := 0; i < 64; i++ {
		pairs = append(pairs, privateEdgeEndpoint{from: fmt.Sprintf("source-%04d", i), to: "hot-target"})
	}
	pairs = append(pairs, privateEdgeEndpoint{from: "rare-source", to: "rare-target"})
	args := privateEdgeEndpointArgs(pairs, int64(7))

	current := privateEdgeEndpointQueryPlan(t, db, privateBaselineEdgeCandidatesEndpointQuery(len(pairs)), args...)
	candidate := privateEdgeEndpointQueryPlan(t, db, edgeCandidatesEndpointQuery(len(pairs)), args...)
	t.Logf("baseline plan:\n%s\ncandidate plan:\n%s", current, candidate)
	if strings.Contains(current, "sqlite_autoindex_edges_1") ||
		(!strings.Contains(current, "edges_by_to") && !strings.Contains(current, "edges_by_from")) {
		t.Fatalf("current fixture no longer demonstrates a one-endpoint secondary-index plan:\n%s", current)
	}
	if !strings.Contains(candidate, "sqlite_autoindex_edges_1") || !strings.Contains(candidate, "from_id=? AND to_id=?") {
		t.Fatalf("candidate does not use the logical unique endpoint prefix:\n%s", candidate)
	}
	if strings.Contains(candidate, "edges_by_to") {
		t.Fatalf("candidate still uses the high-fan-in target index:\n%s", candidate)
	}
}

func TestPrivateEdgeCandidatesEndpointGenerationQualifierPreservesRowsAndMetadata(t *testing.T) {
	store, _ := openTempStore(t)
	const deepGeneration int64 = (1 << 40) + 7

	base := []*graph.Edge{
		privateEndpointEdge("source-a", "hot-target", graph.EdgeCalls, "base/a.go", 11, "base-a"),
		privateEndpointEdge("source-b", "hot-target", graph.EdgeReferences, "base/b.go", 12, "base-b"),
	}
	generationSeven := []*graph.Edge{
		privateEndpointEdge("source-a", "hot-target", graph.EdgeCalls, "g7/a.go", 21, "g7-a"),
		privateEndpointEdge("rare-source", "rare-target", graph.EdgeImports, "g7/rare.go", 22, "g7-rare"),
	}
	deep := []*graph.Edge{
		privateEndpointEdge("source-a", "hot-target", graph.EdgeCalls, "deep/a.go", 31, "deep-a"),
		privateEndpointEdge("source-b", "hot-target", graph.EdgeReferences, "deep/b.go", 32, "deep-b"),
		privateEndpointEdge("rare-source", "rare-target", graph.EdgeImports, "deep/rare.go", 33, "deep-rare"),
		// Reverse lexical order relative to insertion/row-ID order. A bare
		// index switch would return aaa.go first and change first-match APIs.
		privateEndpointEdge("order-source", "order-target", graph.EdgeCalls, "zzz.go", 90, "order-first"),
		privateEndpointEdge("order-source", "order-target", graph.EdgeCalls, "aaa.go", 10, "order-second"),
		privateEndpointEdge("order-source", "order-target", graph.EdgeReferences, "ref.go", 50, "order-reference"),
	}
	if err := store.AddBatchChecked(nil, base); err != nil {
		t.Fatalf("add base edges: %v", err)
	}
	if err := store.AtGeneration(7).AddBatchChecked(nil, generationSeven); err != nil {
		t.Fatalf("add generation-seven edges: %v", err)
	}
	if err := store.AtGeneration(deepGeneration).AddBatchChecked(nil, deep); err != nil {
		t.Fatalf("add deep-generation edges: %v", err)
	}
	// Exercise generation isolation with many retained generations as well as
	// a numerically deep generation. Each generation repeats both a hot target
	// and a rare target while keeping the queried identities distinct.
	for generation := int64(8); generation < 32; generation++ {
		edges := []*graph.Edge{
			privateEndpointEdge(fmt.Sprintf("noise-hot-%02d", generation), "hot-target", graph.EdgeCalls, fmt.Sprintf("noise/%02d.go", generation), int(generation), fmt.Sprintf("noise-%02d", generation)),
			privateEndpointEdge(fmt.Sprintf("noise-rare-%02d", generation), fmt.Sprintf("rare-target-%02d", generation), graph.EdgeImports, fmt.Sprintf("rare/%02d.go", generation), int(generation), fmt.Sprintf("rare-%02d", generation)),
		}
		if err := store.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatalf("add retained generation %d: %v", generation, err)
		}
	}

	pairs := []privateEdgeEndpoint{
		{from: "source-b", to: "hot-target"},
		{from: "rare-source", to: "rare-target"},
		{from: "source-a", to: "hot-target"},
		{from: "missing-source", to: "hot-target"},
		{from: "order-source", to: "order-target"},
	}
	for _, generation := range []int64{0, 7, deepGeneration, deepGeneration + 1} {
		t.Run(fmt.Sprintf("generation-%d", generation), func(t *testing.T) {
			args := privateEdgeEndpointArgs(pairs, generation)
			current, err := store.queryEdgeCandidatesSQL(privateBaselineEdgeCandidatesEndpointQuery(len(pairs)), args...)
			if err != nil {
				t.Fatalf("query current rows: %v", err)
			}
			candidate, err := store.queryEdgeCandidatesSQL(edgeCandidatesEndpointQuery(len(pairs)), args...)
			if err != nil {
				t.Fatalf("query candidate rows: %v", err)
			}
			if got, want := privateCanonicalEdges(candidate), privateCanonicalEdges(current); !reflect.DeepEqual(got, want) {
				t.Fatalf("candidate rows differ from current rows\n candidate=%#v\n current=%#v", got, want)
			}
			if got, want := privateEndpointSequences(candidate), privateEndpointSequences(current); !reflect.DeepEqual(got, want) {
				t.Fatalf("candidate within-endpoint order differs from current order\n candidate=%#v\n current=%#v", got, want)
			}
		})
	}
}

func TestPrivateGetEdgeCandidatesPreservesFirstMatchAndSitePointer(t *testing.T) {
	store, _ := openTempStore(t)
	const generation int64 = (1 << 40) + 7
	first := privateEndpointEdge("order-source", "order-target", graph.EdgeCalls, "zzz.go", 90, "order-first")
	second := privateEndpointEdge("order-source", "order-target", graph.EdgeCalls, "aaa.go", 10, "order-second")
	otherKind := privateEndpointEdge("order-source", "order-target", graph.EdgeReferences, "ref.go", 50, "order-reference")
	if err := store.AtGeneration(generation).AddBatchChecked(nil, []*graph.Edge{first, second, otherKind}); err != nil {
		t.Fatalf("add ordered candidates: %v", err)
	}

	pair := []privateEdgeEndpoint{{from: first.From, to: first.To}}
	baseline, err := store.queryEdgeCandidatesSQL(privateBaselineEdgeCandidatesEndpointQuery(1), privateEdgeEndpointArgs(pair, generation)...)
	if err != nil {
		t.Fatalf("query baseline candidates: %v", err)
	}
	if len(baseline) != 3 || baseline[0].FilePath != first.FilePath {
		t.Fatalf("baseline first-match order = %#v, want inserted calls edge first", privateEndpointSequences(baseline))
	}

	view := store.AtGeneration(generation)
	set := view.GetEdgeCandidates(
		[]graph.EdgeEndpoint{{From: first.From, To: first.To}},
		[]graph.EdgeSite{{From: first.From, Line: first.Line, Kind: first.Kind}},
	)
	gotEndpoint := set.Endpoint(first.From, first.To)
	gotKind := set.EndpointKind(first.From, first.To, first.Kind)
	if privateEdgeIdentity(gotEndpoint) != privateEdgeIdentity(baseline[0]) {
		t.Fatalf("Endpoint first = %s, baseline first = %s", privateEdgeIdentity(gotEndpoint), privateEdgeIdentity(baseline[0]))
	}
	if privateEdgeIdentity(gotKind) != privateEdgeIdentity(baseline[0]) {
		t.Fatalf("EndpointKind first = %s, baseline first = %s", privateEdgeIdentity(gotKind), privateEdgeIdentity(baseline[0]))
	}
	site := set.Site(first.From, first.Line, first.Kind)
	if len(site) != 1 || site[0] != gotKind {
		t.Fatalf("site bucket = %#v; want one canonical pointer shared with endpoint bucket", privateCanonicalEdges(site))
	}
	if got := set.Endpoint("missing", "missing"); got != nil {
		t.Fatalf("missing endpoint = %#v, want nil", got)
	}
}

func TestPrivateEdgeCandidatesEndpointGenerationQualifierRepeatedPairAcrossGenerations(t *testing.T) {
	store, _ := openTempStore(t)
	const generations int64 = 64
	for generation := int64(0); generation < generations; generation++ {
		edge := privateEndpointEdge("stable-source", "stable-target", graph.EdgeCalls, "stable.go", 17, fmt.Sprintf("generation-%d", generation))
		if err := store.AtGeneration(generation).AddBatchChecked(nil, []*graph.Edge{edge}); err != nil {
			t.Fatalf("add repeated pair generation %d: %v", generation, err)
		}
	}
	pairs := []privateEdgeEndpoint{{from: "stable-source", to: "stable-target"}}
	for _, generation := range []int64{0, generations / 2, generations - 1} {
		args := privateEdgeEndpointArgs(pairs, generation)
		baseline, err := store.queryEdgeCandidatesSQL(privateBaselineEdgeCandidatesEndpointQuery(1), args...)
		if err != nil {
			t.Fatalf("query baseline generation %d: %v", generation, err)
		}
		candidate, err := store.queryEdgeCandidatesSQL(edgeCandidatesEndpointQuery(1), args...)
		if err != nil {
			t.Fatalf("query candidate generation %d: %v", generation, err)
		}
		if got, want := privateCanonicalEdges(candidate), privateCanonicalEdges(baseline); !reflect.DeepEqual(got, want) {
			t.Fatalf("repeated-pair generation %d rows differ: candidate=%#v baseline=%#v", generation, got, want)
		}
	}

	args := privateEdgeEndpointArgs(pairs, generations-1)
	const iterations = 64
	start := time.Now()
	for i := 0; i < iterations; i++ {
		if _, err := store.queryEdgeCandidatesSQL(privateBaselineEdgeCandidatesEndpointQuery(1), args...); err != nil {
			t.Fatalf("warm baseline iteration %d: %v", i, err)
		}
	}
	baselineElapsed := time.Since(start)
	start = time.Now()
	for i := 0; i < iterations; i++ {
		if _, err := store.queryEdgeCandidatesSQL(edgeCandidatesEndpointQuery(1), args...); err != nil {
			t.Fatalf("warm candidate iteration %d: %v", i, err)
		}
	}
	candidateElapsed := time.Since(start)
	t.Logf("repeated-pair diagnostic generations=%d iterations=%d baseline=%s candidate=%s", generations, iterations, baselineElapsed, candidateElapsed)
}

func TestPrivateEdgeCandidatesEndpointGenerationQualifierUsesIntegerBinding(t *testing.T) {
	store, _ := openTempStore(t)
	const generation int64 = 7
	edge := privateEndpointEdge("typed-source", "typed-target", graph.EdgeCalls, "typed.go", 1, "typed")
	if err := store.AtGeneration(generation).AddBatchChecked(nil, []*graph.Edge{edge}); err != nil {
		t.Fatalf("add typed edge: %v", err)
	}
	var bindType string
	if err := store.db.QueryRow(`SELECT typeof(?)`, generation).Scan(&bindType); err != nil {
		t.Fatalf("inspect bind type: %v", err)
	}
	if bindType != "integer" {
		t.Fatalf("Store generation bind type = %q, want integer", bindType)
	}
	pairs := []privateEdgeEndpoint{{from: edge.From, to: edge.To}}
	got, err := store.queryEdgeCandidatesSQL(edgeCandidatesEndpointQuery(1), privateEdgeEndpointArgs(pairs, generation)...)
	if err != nil {
		t.Fatalf("query integer-bound candidate: %v", err)
	}
	if rows := privateCanonicalEdges(got); len(rows) != 1 || !strings.Contains(rows[0], "typed") {
		t.Fatalf("integer-bound candidate rows = %#v", rows)
	}

	// This is a contract boundary, not a supported call shape: removing column
	// affinity deliberately means a text bind is not coerced to INTEGER.
	var textCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM edges WHERE from_id=? AND to_id=? AND +view_gen=?`, edge.From, edge.To, "7").Scan(&textCount); err != nil {
		t.Fatalf("query text-bound control: %v", err)
	}
	if textCount != 0 {
		t.Fatalf("text-bound control returned %d rows, want 0", textCount)
	}
}

type privateEdgeEndpoint struct {
	from string
	to   string
}

func privateEdgeEndpointArgs(pairs []privateEdgeEndpoint, generation int64) []any {
	args := make([]any, 0, len(pairs)*2+1)
	for _, pair := range pairs {
		args = append(args, pair.from, pair.to)
	}
	return append(args, generation)
}

func privateEndpointEdge(from, to string, kind graph.EdgeKind, file string, line int, marker string) *graph.Edge {
	return &graph.Edge{
		From:            from,
		To:              to,
		Kind:            kind,
		FilePath:        file,
		Line:            line,
		Confidence:      0.75,
		ConfidenceLabel: "high",
		Origin:          "private-endpoint-regression",
		Tier:            "exact",
		CrossRepo:       strings.HasPrefix(marker, "deep"),
		Meta: map[string]any{
			"marker":             marker,
			"resolve_terminal":   true,
			"semantic_source":    "private-test",
			"extra_string_value": "kept",
		},
	}
}

func privateEdgeIdentity(edge *graph.Edge) string {
	if edge == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s|%s|%s|%s|%d", edge.From, edge.To, edge.Kind, edge.FilePath, edge.Line)
}

func privateEndpointSequences(edges []*graph.Edge) map[privateEdgeEndpoint][]string {
	out := make(map[privateEdgeEndpoint][]string)
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		key := privateEdgeEndpoint{from: edge.From, to: edge.To}
		out[key] = append(out[key], privateEdgeIdentity(edge))
	}
	return out
}

// Compare the complete rows as a sorted multiset, while the separate sequence
// assertion covers the within-endpoint first-match contract.
func privateCanonicalEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			out = append(out, "<nil>")
			continue
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d|%g|%s|%s|%s|%t|%#v",
			edge.From, edge.To, edge.Kind, edge.FilePath, edge.Line,
			edge.Confidence, edge.ConfidenceLabel, edge.Origin, edge.Tier,
			edge.CrossRepo, edge.Meta))
	}
	sort.Strings(out)
	return out
}

func privateOpenEdgeEndpointPlanDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:edge-endpoint-plan?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open planner database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	statements := []string{
		`CREATE TABLE edges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			from_id TEXT NOT NULL,
			to_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			file_path TEXT,
			line INTEGER,
			confidence REAL,
			confidence_label TEXT,
			origin TEXT,
			tier TEXT,
			cross_repo INTEGER,
			meta TEXT,
			resolve_terminal INTEGER,
			resolve_terminal_reason TEXT,
			semantic_source TEXT,
			view_gen INTEGER NOT NULL DEFAULT 0,
			UNIQUE(from_id, to_id, kind, file_path, line, view_gen)
		)`,
		`CREATE INDEX edges_by_to ON edges(to_id, view_gen, kind)`,
		`CREATE INDEX edges_by_from ON edges(from_id, view_gen, kind)`,
		`CREATE INDEX edges_by_generation ON edges(view_gen)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create planner schema: %v", err)
		}
	}
	for _, generation := range []int64{0, 7, (1 << 40) + 7} {
		if _, err := db.Exec(`
WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i < 3999)
INSERT INTO edges(from_id,to_id,kind,file_path,line,view_gen)
SELECT printf('source-%04d',i),'hot-target','calls',printf('src/%04d.go',i),i+1,? FROM seq`, generation); err != nil {
			t.Fatalf("seed hot target generation %d: %v", generation, err)
		}
		if _, err := db.Exec(`INSERT INTO edges(from_id,to_id,kind,file_path,line,view_gen) VALUES
('rare-source','rare-target','imports','rare.go',1,?),
('other-source','other-target','calls','other.go',2,?)`, generation, generation); err != nil {
			t.Fatalf("seed rare targets generation %d: %v", generation, err)
		}
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze planner database: %v", err)
	}
	return db
}

func privateEdgeEndpointQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return strings.Join(details, "\n")
}
