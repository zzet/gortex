package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestPromotedColumns_RoundTrip verifies the promoted keys land in their
// columns, are stripped from the JSON blob, and restore into Meta with
// exact types — while non-promoted keys stay in the blob.
func TestPromotedColumns_RoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddNode(&graph.Node{
		ID: "f.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "f.go",
		Meta: map[string]any{
			"signature":  "func F()",
			"visibility": "public",
			"doc":        "F docs",
			"external":   true,
			"complexity": 5, // non-promoted — must stay in the blob
		},
	})

	n := s.GetNode("f.go::F")
	if n == nil {
		t.Fatal("GetNode returned nil")
	}
	assertType[string](t, n.Meta, "signature", "func F()")
	assertType[string](t, n.Meta, "visibility", "public")
	assertType[string](t, n.Meta, "doc", "F docs")
	assertType[bool](t, n.Meta, "external", true)
	assertType[int](t, n.Meta, "complexity", 5)

	var sig, vis, doc sql.NullString
	var ext sql.NullBool
	var blob []byte
	row := s.db.QueryRow(`SELECT signature, visibility, doc, external, meta FROM nodes WHERE id=?`, "f.go::F")
	if err := row.Scan(&sig, &vis, &doc, &ext, &blob); err != nil {
		t.Fatal(err)
	}
	if !sig.Valid || sig.String != "func F()" {
		t.Errorf("signature column = %+v", sig)
	}
	if !ext.Valid || !ext.Bool {
		t.Errorf("external column = %+v", ext)
	}
	blobStr := string(blob)
	for _, k := range []string{"signature", "visibility", "external"} {
		if strings.Contains(blobStr, k) {
			t.Errorf("blob still contains promoted key %q: %s", k, blobStr)
		}
	}
	if !strings.Contains(blobStr, "complexity") {
		t.Errorf("blob missing non-promoted key complexity: %s", blobStr)
	}
}

// TestPromotedColumns_NewColumns verifies the added promoted meta columns
// (is_async / is_static / is_abstract / is_exported / return_type / updated_at)
// and the struct-field columns (start_column / end_column) round-trip through
// their typed columns, and that a SQL filter on is_async resolves WITHOUT
// decoding the meta blob — the indexable-column acceptance.
func TestPromotedColumns_NewColumns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddNode(&graph.Node{
		ID: "f.go::Async", Kind: graph.KindFunction, Name: "Async", FilePath: "f.go",
		StartLine: 10, EndLine: 20, StartColumn: 4, EndColumn: 1,
		Meta: map[string]any{
			"is_async":    true,
			"is_static":   false,
			"is_abstract": true,
			"is_exported": true,
			"return_type": "error",
			"updated_at":  int64(1700000000),
			"complexity":  3, // non-promoted — stays in the blob
		},
	})
	// A second, non-async node to prove the filter is selective.
	s.AddNode(&graph.Node{
		ID: "f.go::Sync", Kind: graph.KindFunction, Name: "Sync", FilePath: "f.go",
		Meta: map[string]any{"is_async": false},
	})

	// Read-back restores every promoted key into Meta with its exact type,
	// and the struct columns into the Node fields.
	n := s.GetNode("f.go::Async")
	if n == nil {
		t.Fatal("GetNode returned nil")
	}
	assertType[bool](t, n.Meta, "is_async", true)
	assertType[bool](t, n.Meta, "is_static", false)
	assertType[bool](t, n.Meta, "is_abstract", true)
	assertType[bool](t, n.Meta, "is_exported", true)
	assertType[string](t, n.Meta, "return_type", "error")
	assertType[int64](t, n.Meta, "updated_at", int64(1700000000))
	assertType[int](t, n.Meta, "complexity", 3)
	if n.StartColumn != 4 || n.EndColumn != 1 {
		t.Errorf("column offsets = (%d,%d), want (4,1)", n.StartColumn, n.EndColumn)
	}

	// The promoted keys are stripped from the JSON blob; complexity is not.
	var blob []byte
	if err := s.db.QueryRow(`SELECT meta FROM nodes WHERE id=?`, "f.go::Async").Scan(&blob); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"is_async", "is_abstract", "return_type", "updated_at"} {
		if strings.Contains(string(blob), k) {
			t.Errorf("blob still contains promoted key %q: %s", k, blob)
		}
	}
	if !strings.Contains(string(blob), "complexity") {
		t.Errorf("blob missing non-promoted key complexity: %s", blob)
	}

	// Acceptance: a SQL filter on the typed column resolves the node without
	// touching the meta blob (only id is selected).
	var id string
	var startCol, endCol int
	if err := s.db.QueryRow(
		`SELECT id, start_column, end_column FROM nodes WHERE is_async = 1`,
	).Scan(&id, &startCol, &endCol); err != nil {
		t.Fatalf("is_async column filter failed: %v", err)
	}
	if id != "f.go::Async" || startCol != 4 || endCol != 1 {
		t.Errorf("filter result = (%q,%d,%d), want (f.go::Async,4,1)", id, startCol, endCol)
	}
}

// TestPromotedColumns_SemanticType verifies semantic_type/semantic_source —
// promoted alongside signature/visibility/etc. so enrichment can query the
// unstamped subset by column instead of decoding every node's meta blob —
// round-trip through their columns and out of the JSON blob.
func TestPromotedColumns_SemanticType(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddNode(&graph.Node{
		ID: "f.go::T", Kind: graph.KindFunction, Name: "T", FilePath: "f.go",
		Meta: map[string]any{
			"semantic_type":   "string",
			"semantic_source": "lsp-gopls",
			"complexity":      5, // non-promoted — must stay in the blob
		},
	})

	n := s.GetNode("f.go::T")
	if n == nil {
		t.Fatal("GetNode returned nil")
	}
	assertType[string](t, n.Meta, "semantic_type", "string")
	assertType[string](t, n.Meta, "semantic_source", "lsp-gopls")
	assertType[int](t, n.Meta, "complexity", 5)

	var st, ss sql.NullString
	var blob []byte
	row := s.db.QueryRow(`SELECT semantic_type, semantic_source, meta FROM nodes WHERE id=?`, "f.go::T")
	if err := row.Scan(&st, &ss, &blob); err != nil {
		t.Fatal(err)
	}
	if !st.Valid || st.String != "string" {
		t.Errorf("semantic_type column = %+v", st)
	}
	if !ss.Valid || ss.String != "lsp-gopls" {
		t.Errorf("semantic_source column = %+v", ss)
	}
	blobStr := string(blob)
	for _, k := range []string{"semantic_type", "semantic_source"} {
		if strings.Contains(blobStr, k) {
			t.Errorf("blob still contains promoted key %q: %s", k, blobStr)
		}
	}
	if !strings.Contains(blobStr, "complexity") {
		t.Errorf("blob missing non-promoted key complexity: %s", blobStr)
	}
}

// TestPromotedColumns_ExternalFalse guards the NULL-vs-false distinction:
// a stored false must round-trip as false, not vanish.
func TestPromotedColumns_ExternalFalse(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddNode(&graph.Node{
		ID: "x", Kind: graph.KindFunction, Name: "x", FilePath: "x.go",
		Meta: map[string]any{"external": false},
	})
	n := s.GetNode("x")
	if n == nil {
		t.Fatal("nil")
	}
	v, ok := n.Meta["external"].(bool)
	if !ok || v != false {
		t.Errorf("external false: got %v (%T)", n.Meta["external"], n.Meta["external"])
	}
}

// TestPromotedColumns_Migration verifies ensureNodeColumns adds the
// promoted columns to a database created with the pre-promotion schema.
func TestPromotedColumns_Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-promotion column set on the current identity key. The key is not
	// what this test is about, but the plan below pins the migration list to
	// the v2 step, so the v16 re-key never runs here and the fixture has to
	// carry it — the node writer binds view_gen on every insert.
	_, err = raw.Exec(`CREATE TABLE nodes (
		id TEXT NOT NULL, view_gen INTEGER NOT NULL DEFAULT 0,
		kind TEXT NOT NULL, name TEXT NOT NULL,
		qual_name TEXT NOT NULL DEFAULT '', file_path TEXT NOT NULL,
		start_line INTEGER NOT NULL DEFAULT 0, end_line INTEGER NOT NULL DEFAULT 0,
		language TEXT NOT NULL DEFAULT '', repo_prefix TEXT NOT NULL DEFAULT '',
		workspace_id TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '',
		meta BLOB,
		PRIMARY KEY (id, view_gen)
	) WITHOUT ROWID`)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	// Keep this mechanical-column migration test on the historical v2 plan;
	// shipped v3 intentionally rebuilds pre-v3 topology for integrity.
	s, err := openWith(path, 2, schemaMigrations[:1], false)
	if err != nil {
		t.Fatalf("Open old-schema db: %v", err)
	}
	defer s.Close()
	s.AddNode(&graph.Node{
		ID: "m", Kind: graph.KindFunction, Name: "m", FilePath: "m.go",
		Meta: map[string]any{"signature": "sig", "external": true},
	})
	n := s.GetNode("m")
	if n == nil {
		t.Fatal("nil after migration")
	}
	assertType[string](t, n.Meta, "signature", "sig")
	assertType[bool](t, n.Meta, "external", true)
}
