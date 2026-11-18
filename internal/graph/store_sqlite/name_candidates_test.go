package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

func TestNameCandidateIndexMigrationAndBulkLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.AddNode(&graph.Node{ID: "repo/a.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
	if _, err := s.writerDB.Exec(`DROP INDEX nodes_name_candidates`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`PRAGMA user_version = 29`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("migrated version=%d,%v", version, err)
	}
	s.BeginBulkLoad()
	got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("candidate index missing during bulk window=%v,%v", got, err)
	}
	if err := s.FlushBulk(); err != nil {
		t.Fatal(err)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("candidate index missing after bulk rebuild=%v,%v", got, err)
	}
}

func TestNameCandidateProjectionIsCoveringForEveryRepoScope(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	for _, allow := range []map[string]bool{nil, {"repo": true}, {"repo": true, "other": true}, {"repo": false}} {
		q, args := nameCandidateSQL(0, "needle", graph.NameSearchFilter{RepoAllow: allow})
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, "USING COVERING INDEX nodes_name_candidates (view_gen=?") {
			t.Fatalf("scope %v does not use the covering generation range:\n%s", allow, plan)
		}
		if len(allow) > 0 && !strings.Contains(plan, "repo_prefix=?") {
			t.Fatalf("missing repo seek:\n%s", plan)
		}
		// Every scope seeks each repository's range past names too short to
		// match; an unscoped read steps from repository to repository.
		if !strings.Contains(plan, "nodes_name_candidates (view_gen=? AND repo_prefix=? AND <expr>>?)") {
			t.Fatalf("scope %v does not seek by name length:\n%s", allow, plan)
		}
		if allow == nil && !strings.Contains(plan, "nodes_name_candidates (view_gen=? AND repo_prefix>?)") {
			t.Fatalf("unscoped read does not seek from repository to repository:\n%s", plan)
		}
	}
}

func TestNameCandidatesFilterBeforeLimitAndDoNotDecodeDiscardedPayloads(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	nodes := []*graph.Node{
		{ID: "a/other.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "a/other.go", RepoPrefix: "a"},
		{ID: "b/outside.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "b/outside.go", RepoPrefix: "b"},
		{ID: "b/target.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "b/target.go", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "proj", Meta: map[string]any{"payload": "chosen"}},
		{ID: "external::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "external::needle"},
	}
	s.AddBatch(nodes, nil)
	// A full-row fallback would attempt to decode these deliberately broken
	// discarded payloads; compact filtering must never touch their metadata.
	if _, err := s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id IN (?,?)`, []byte("invalid metadata"), nodes[0].ID, nodes[1].ID); err != nil {
		t.Fatal(err)
	}
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"b": true}, Accept: func(n *graph.Node) bool {
		if n.Meta != nil {
			t.Fatal("predicate received hydrated metadata")
		}
		return n.FilePath == "b/target.go" && n.WorkspaceID == "ws" && n.ProjectID == "proj"
	}}
	got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), "NEEDLE", 1, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != nodes[2].ID || got[0].Meta["payload"] != "chosen" {
		t.Fatalf("selected payload = %#v", got)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 0, graph.NameSearchFilter{RepoAllow: map[string]bool{"b": false}})
	if err != nil || !reflect.DeepEqual(nodeIDs(got), []string{nodes[3].ID}) {
		t.Fatalf("false allow-set prefixless policy = %v, %v", nodeIDs(got), err)
	}
}

func TestNameCandidatesSnapshotAndBaseMutationFreshness(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	old := &graph.Node{ID: "repo/a.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo", Meta: map[string]any{"payload": "old"}}
	s.AddNode(old)
	changed := false
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"repo": true}, Accept: func(*graph.Node) bool {
		// Deliberate test-only interposition between key selection and payload
		// hydration; production predicates are pure and never perform writes.
		if !changed {
			changed = true
			s.AddNode(&graph.Node{ID: old.ID, Name: "renamed", Kind: old.Kind, FilePath: "other/a.go", RepoPrefix: "other", Meta: map[string]any{"payload": "new"}})
		}
		return true
	}}
	got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, filter)
	if err != nil || len(got) != 1 || got[0].Name != "needle" || got[0].RepoPrefix != "repo" || got[0].Meta["payload"] != "old" {
		t.Fatalf("mixed read snapshot: %#v, %v", got, err)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("renamed base hit stayed cached: %v, %v", nodeIDs(got), err)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "renamed", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 1 || got[0].Meta["payload"] != "new" {
		t.Fatalf("updated base payload: %#v, %v", got, err)
	}
	s.EvictFile("other/a.go")
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "renamed", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("deleted base hit stayed cached: %v, %v", nodeIDs(got), err)
	}
}

func TestNameCandidatesLiteralUnicodeGenerationAndCancellation(t *testing.T) {
	_, h := seedNameIndexGeneration(t, true)
	for _, handle := range []*Store{h, h.AtGeneration(0)} {
		if handle.viewGen == 0 {
			for i, name := range nameIndexNames {
				handle.AddNode(&graph.Node{ID: string(rune('a' + i)), Name: name, Kind: graph.KindFunction})
			}
		}
		for _, query := range []string{"_", "%", `\`, "ÉCOLE", "école", "Ω", "ω", "CONFIG", "no-match"} {
			for _, limit := range []int{0, 1, 3} {
				got, err := handle.FindNodesByNameContainingFilteredContext(context.Background(), query, limit, graph.NameSearchFilter{Accept: func(*graph.Node) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				want := sqlFindContaining(t, handle, query, limit)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("generation %d literal %q/%d: %v != %v", handle.viewGen, query, limit, nodeIDs(got), nodeIDs(want))
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := h.FindNodesByNameContainingFilteredContext(ctx, "config", 1, graph.NameSearchFilter{}); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled lookup = %v, %v", got, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	_, err := h.FindNodesByNameContainingFilteredContext(ctx, "config", 1, graph.NameSearchFilter{Accept: func(*graph.Node) bool { cancel(); return true }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation = %v", err)
	}
}

// nameCandidateTrickyNames mixes case, LIKE metacharacters, non-ASCII text,
// invalid UTF-8 (a lone continuation byte, a lead byte without continuation,
// Latin-1 bytes), U+FFFD and an embedded NUL across many lengths.
var nameCandidateTrickyNames = []string{
	"a", "B", "ab", "Ab", "abc", "close", "Close", "CLOSE", "closer", "enclosed", "mcpRequestRegistry",
	"MCPREQUESTREGISTRY", "with_underscore", "with%percent", `back\slash`, "École", "école", "ÉCOLE",
	"Straße", "Ω", "ω", "Ωmega", "日本語Name", "r\xff", "\xc3", "\xa9caf", "caf\xe9", "\xc3\xa9\xa9",
	"\xe2\x82", "\x80\x80", "a\x80\x80", "r\uFFFD", "ab\x00cd", "x\x00", strings.Repeat("long", 40),
}

// nameCandidateUnboundedSQL is the candidate read without the length bound or
// the candidate index: the rows the bounded read must return, in order.
func nameCandidateUnboundedSQL(generation int64, substr string, filter graph.NameSearchFilter) (string, []any) {
	q := `SELECT ` + nameCandidateCols + ` FROM nodes NOT INDEXED WHERE name <> '' AND view_gen = ? AND name LIKE ? ESCAPE '\'`
	args := []any{generation, "%" + escapeLikePattern(substr) + "%"}
	if len(filter.RepoAllow) > 0 {
		repos := []string{""}
		for repo, allowed := range filter.RepoAllow {
			if allowed && repo != "" {
				repos = append(repos, repo)
			}
		}
		encoded, _ := json.Marshal(repos)
		q += ` AND repo_prefix IN (SELECT value FROM json_each(?))`
		args = append(args, string(encoded))
	}
	return q + ` ORDER BY id`, args
}

// queryNameCandidateKeys runs a candidate read and returns its compact keys.
func queryNameCandidateKeys(t *testing.T, s *Store, q string, args []any) ([]graph.Node, error) {
	t.Helper()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []graph.Node
	for rows.Next() {
		var key graph.Node
		if err := rows.Scan(&key.ID, &key.Name, &key.Kind, &key.FilePath, &key.RepoPrefix, &key.WorkspaceID, &key.ProjectID); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// admittedKeyIDs is the capped, filtered prefix of keys a filtered lookup
// returns.
func admittedKeyIDs(keys []graph.Node, filter graph.NameSearchFilter, limit int) []string {
	ids := []string{}
	for i := range keys {
		if filter.Allows(&keys[i]) && (limit <= 0 || len(ids) < limit) {
			ids = append(ids, keys[i].ID)
		}
	}
	return ids
}

// The length bound is SQLite's own length() of the substring, so a name the
// pattern matches can never be shorter than the bound.
func TestNameCandidateLengthBoundCountsCharactersAsSQLiteDoes(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	inputs := append([]string{"", "\x00", "\x80", "\xc0", "\xc3\xa9", "\xf0\x9f\x98\x80", "\xff\x80\x80\x80\x80", "\x80\x80", "a\x80\x80", "\xc3\x80\x80", "a\xc3", "%_\\"}, nameCandidateTrickyNames...)
	for _, input := range inputs {
		var want int
		if err := s.db.QueryRow(`SELECT length(CAST(? AS TEXT))`, input).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got := likeMatchMinLength(input); got != want {
			t.Fatalf("likeMatchMinLength(%q) = %d, SQLite length() = %d", input, got, want)
		}
	}
}

// The length seek returns exactly the rows of an unbounded LIKE scan, in the
// same order, for every repository scope, generation and substring, including
// substrings no name can contain and one too long for LIKE to accept.
func TestNameCandidateLengthSeekReturnsTheUnboundedScan(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	rng := rand.New(rand.NewSource(7))
	alphabet := []string{"a", "B", "c", "_", "%", `\`, "é", "É", "Ω", "\xc3", "\xa9", "\xff", "\x80", "\x00", "z"}
	random := func(maxLen int) string {
		var b strings.Builder
		for n := 1 + rng.Intn(maxLen); n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	names := append([]string(nil), nameCandidateTrickyNames...)
	for range 200 {
		names = append(names, random(10))
	}
	repos := []string{"", "repo", "other", "zeta"}
	for _, gen := range []int64{0, 41} {
		var nodes []*graph.Node
		for i, name := range names {
			repo := repos[i%len(repos)]
			nodes = append(nodes, &graph.Node{
				ID: fmt.Sprintf("%s/f%03d.go::n%03d", repo, (i*37)%101, i), Name: name, Kind: graph.KindFunction,
				FilePath: fmt.Sprintf("%s/f%03d.go", repo, (i*37)%101), RepoPrefix: repo,
			})
		}
		nodes = append(nodes, &graph.Node{ID: "repo/empty.go", Name: "", Kind: graph.KindFile, FilePath: "repo/empty.go", RepoPrefix: "repo"})
		if err := s.AtGeneration(gen).AddBatchChecked(nodes, nil); err != nil {
			t.Fatal(err)
		}
		// The normal writer's JSON encoding repairs invalid UTF-8. Preserve
		// actual legacy TEXT bytes here or the length/LIKE equivalence check
		// never exercises the malformed names it claims to cover.
		for _, node := range nodes {
			if !utf8.ValidString(node.Name) {
				if _, err := s.writerDB.Exec(`UPDATE nodes SET name=CAST(? AS TEXT) WHERE view_gen=? AND id=?`, []byte(node.Name), gen, node.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	queries := []string{"a", "A", "close", "CLOSE", "registry", "_", "%", `\`, "é", "É", "école", "Ω", "ω", "日本語",
		"\xc3", "\xa9", "\xc3\xa9", "\xe9", "\uFFFD", "\x80", "\x80\x80", "ab\x00zz", "\x00", "long", strings.Repeat("long", 40),
		strings.Repeat("long", 41), "no-such-name", strings.Repeat("x", sqliteLikePatternLimit)}
	for _, name := range names {
		if len(name) > 2 {
			queries = append(queries, name, name[1:], strings.ToUpper(name))
		}
	}
	for range 200 {
		queries = append(queries, random(4))
	}
	scopes := []map[string]bool{nil, {"repo": true}, {"repo": true, "zeta": true}, {"other": false}}
	compared, matched := 0, 0
	for _, gen := range []int64{0, 41, 7} {
		for _, query := range queries {
			for _, allow := range scopes {
				filter := graph.NameSearchFilter{RepoAllow: allow}
				wantQ, wantArgs := nameCandidateUnboundedSQL(gen, query, filter)
				want, wantErr := queryNameCandidateKeys(t, s, wantQ, wantArgs)
				gotQ, gotArgs := nameCandidateSQL(gen, query, filter)
				got, gotErr := queryNameCandidateKeys(t, s, gotQ, gotArgs)
				if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(got, want) {
					t.Fatalf("generation %d %q scope %v: bounded %v (%v), unbounded %v (%v)", gen, query, allow, got, gotErr, want, wantErr)
				}
				compared++
				if len(want) > 0 {
					matched++
				}
			}
		}
	}
	if matched < compared/10 {
		t.Fatalf("only %d of %d comparisons found rows; the corpus no longer exercises matches", matched, compared)
	}
	// The capped, filtered read over the mutable base keeps the same order and
	// stops at the same admitted row.
	accept := func(n *graph.Node) bool { return !strings.HasSuffix(n.ID, "7") }
	for _, query := range queries[:40] {
		for _, allow := range scopes {
			for _, limit := range []int{0, 1, 3} {
				filter := graph.NameSearchFilter{RepoAllow: allow, Accept: accept}
				got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), query, limit, filter)
				wantQ, wantArgs := nameCandidateUnboundedSQL(0, query, graph.NameSearchFilter{RepoAllow: allow})
				keys, wantErr := queryNameCandidateKeys(t, s, wantQ, wantArgs)
				want := admittedKeyIDs(keys, filter, limit)
				if (err != nil) != (wantErr != nil) || err == nil && !reflect.DeepEqual(nodeIDs(got), want) {
					t.Fatalf("%q scope %v limit %d: got %v (%v), want %v (%v)", query, allow, limit, nodeIDs(got), err, want, wantErr)
				}
			}
		}
	}
}

// A store from before the length key gets its candidate index rebuilt in
// place with unchanged answers; an index already in the current shape is not
// built a second time.
func TestNameCandidateLengthMigrationRebuildsTheLegacyKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy38.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range nameCandidateTrickyNames {
		s.AddNode(&graph.Node{ID: fmt.Sprintf("repo/a.go::n%02d", i), Name: name, Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: []string{"", "repo"}[i%2]})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX nodes_name_candidates`,
		`CREATE INDEX nodes_name_candidates ON nodes(view_gen, repo_prefix, id, name, kind, file_path, workspace_id, project_id) WHERE name <> ''`,
		`PRAGMA user_version = 38`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("migrated version=%d,%v", version, err)
	}
	for _, query := range []string{"close", "é", "\xc3", "ab", "registry"} {
		wantQ, wantArgs := nameCandidateUnboundedSQL(0, query, graph.NameSearchFilter{})
		keys, err := queryNameCandidateKeys(t, s, wantQ, wantArgs)
		if err != nil {
			t.Fatal(err)
		}
		want := admittedKeyIDs(keys, graph.NameSearchFilter{}, 0)
		got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), query, 0, graph.NameSearchFilter{})
		if err != nil || !reflect.DeepEqual(nodeIDs(got), want) {
			t.Fatalf("%q after migration: %v (%v), want %v", query, nodeIDs(got), err, want)
		}
	}
	tx, err := s.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := indexColumnsMatch(tx, "nodes_name_candidates", nodesNameCandidatesIndexColumns)
	if err != nil || !current {
		t.Fatalf("migrated candidate index shape current=%v, %v", current, err)
	}
	cookie := func() int64 {
		t.Helper()
		var v int64
		if err := tx.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := cookie()
	if err := seekNameCandidatesByLength(tx); err != nil {
		t.Fatal(err)
	}
	if after := cookie(); after != before {
		t.Fatalf("current nodes_name_candidates was rebuilt: schema cookie %d -> %d", before, after)
	}
}
