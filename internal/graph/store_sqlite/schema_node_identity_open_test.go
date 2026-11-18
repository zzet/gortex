package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A complete actual v23 schema: keep both dependency-revision columns, remove
// ONLY the proposed v24 discriminator, then stamp v23. New schema additions
// must keep this historical fixture honest before changing the version label.
func nodeIdentityMaskV23File(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v23.sqlite")
	const column = "    claim_kind TEXT NOT NULL DEFAULT 'legacy_tombstone',\n"
	if strings.Count(schemaSQL, column) != 1 {
		t.Fatal("v24 canonical declaration must occur exactly once")
	}
	legacy := strings.Replace(schemaSQL, column, "", 1)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO generation_node_tombstones(view_gen,node_id) VALUES(7,'alpha/a::legacy'); PRAGMA user_version=23`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_xinfo('generation_node_tombstones') WHERE name='claim_kind'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fixture mislabeled v23: %d %v", count, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNodeIdentityMaskFullV23OpenAndReopenPreserveBothKinds(t *testing.T) {
	path := nodeIdentityMaskV23File(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version int
	// The assertion is "Open brought the v23 fixture fully forward", not "the
	// registry stops at v24" — a later additive step (v25's analysis view axis)
	// stamps its own version and must not read as a regression here. The
	// sibling probe at :135 still pins the fixture's own stored version.
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	legacy, err := s.AtGeneration(7).NodeIdentityMasks()
	if err != nil || len(legacy) != 1 || legacy[0].Kind != NodeIdentityMaskLegacy {
		t.Fatalf("legacy changed: %v %v", legacy, err)
	}
	id, err := s.Catalog().CreateViewGeneration(context.Background(), ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "roundtrip", GenerationKind: "dedicated", TreeOID: "tree", ConfigHash: "policy", State: ViewGenerationBuilding, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	h := s.AtGeneration(id)
	h.AddBatch([]*graph.Node{{ID: "alpha/a::new", Name: "new", RepoPrefix: "alpha", FilePath: "alpha/a"}, {ID: "alpha/a::full", Name: "full", RepoPrefix: "alpha", FilePath: "alpha/a"}}, nil)
	if err := h.SetNodeIdentityReplacements([]string{"alpha/a::new", "alpha/a::full"}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetNodeTombstones([]string{"alpha/a::full"}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetNodeIdentityReplacements([]string{"alpha/a::full"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishPayloadGeneration(context.Background(), id, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for reopen := 0; reopen < 2; reopen++ {
		next, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		masks, err := next.AtGeneration(id).NodeIdentityMasks()
		if err != nil || len(masks) != 2 {
			_ = next.Close()
			t.Fatalf("roundtrip: %v %v", masks, err)
		}
		got := map[string]NodeIdentityMaskKind{}
		for _, m := range masks {
			got[m.NodeID] = m.Kind
		}
		if got["alpha/a::new"] != NodeIdentityMaskReplace || got["alpha/a::full"] != NodeIdentityMaskLegacy {
			_ = next.Close()
			t.Fatalf("claim downgrade on reopen: %v", got)
		}
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodeIdentityMaskOpenFailureDoesNotStampAndRetries(t *testing.T) {
	path := nodeIdentityMaskV23File(t)
	want := errors.New("injected mask migration failure")
	steps := make([]schemaMigration, 0, len(schemaMigrations))
	for _, step := range schemaMigrations {
		if step.version < 24 {
			steps = append(steps, step)
		}
	}
	steps = append(steps, schemaMigration{version: 24, name: "injected mask migration", inPlace: func(tx *sql.Tx) error {
		if err := addNodeIdentityMaskKinds(tx); err != nil {
			return err
		}
		return want
	}})
	opened, err := openWithObserver(path, 24, steps, false, nil)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("failed migration returned store")
	}
	if !errors.Is(err, want) {
		t.Fatalf("failure lost: %v", err)
	}
	probe, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version, count int
	if err := probe.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 23 {
		_ = probe.Close()
		t.Fatalf("failure stamped %d %v", version, err)
	}
	if err := probe.QueryRow(`SELECT COUNT(*) FROM pragma_table_xinfo('generation_node_tombstones') WHERE name='claim_kind'`).Scan(&count); err != nil || count != 0 {
		_ = probe.Close()
		t.Fatalf("partial discriminator %d %v", count, err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	retried, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer retried.Close()
	masks, err := retried.AtGeneration(7).NodeIdentityMasks()
	if err != nil || len(masks) != 1 || masks[0].Kind != NodeIdentityMaskLegacy {
		t.Fatalf("retry altered legacy: %v %v", masks, err)
	}
}

// A binary that cannot distinguish identity-only from legacy masks must not
// open this newer format and silently suppress lower outgoing adjacency.
// This is a release gate on the existing version opener, not a passing claim.
func TestNodeIdentityMaskOlderVersionCannotOpenNewFormat(t *testing.T) {
	path := nodeIdentityMaskV23File(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	older := make([]schemaMigration, 0, len(schemaMigrations))
	for _, step := range schemaMigrations {
		if step.version < 24 {
			older = append(older, step)
		}
	}
	opened, err := openWithObserver(path, 23, older, false, nil)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("older reader opened identity-only format")
	}
	if err == nil {
		t.Fatal("older reader lacked an explicit refusal")
	}
}
