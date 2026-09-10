package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Build the complete immediately-prior canonical schema in a disposable file.
// Only the two NEW column declarations are removed; no production source or
// database is copied/modified. Metadata rows are a valid dedicated ownership chain.
func dependencyRevisionV22File(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v22.sqlite")
	legacy := schemaSQL
	for _, column := range []string{
		"    dependency_revision    TEXT NOT NULL DEFAULT '',\n",
		"\tdependency_revision TEXT NOT NULL DEFAULT '',\n",
	} {
		if strings.Count(legacy, column) != 1 {
			t.Fatalf("canonical new column must occur exactly once: %q", column)
		}
		legacy = strings.Replace(legacy, column, "", 1)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO repository_families(family_id,common_dir_identity,state) VALUES('family',?,'active')`, []any{filepath.Join(filepath.Dir(path), "git")}},
		{`INSERT INTO checkouts(checkout_id,incarnation,family_id,root_path,git_dir,admin_name,state,desired_mode,effective_mode) VALUES('owner','incarnation','family',?,?,'main','checkout_ready','dedicated','dedicated')`, []any{filepath.Dir(path), filepath.Join(filepath.Dir(path), "git")}},
		{`INSERT INTO dedicated_graphs(graph_id,owner_checkout_id,repo_prefix,family_id,is_primary_base,active_generation_id,state) VALUES('graph','owner','repo','family',1,1,'ready')`, nil},
		{`INSERT INTO view_generations(generation_id,owner_kind,graph_id,checkout_id,generation_kind,tree_oid,config_hash,extractor_versions,resolver_version,state) VALUES(1,'dedicated_graph','graph','owner','dedicated','tree','config','extractors','resolver','ready')`, nil},
		{`INSERT INTO dedicated_base_publications(graph_id,owner_checkout_id,owner_incarnation,owner_generation_floor,repo_prefix,family_id,authority_epoch,authority_token,desired_epoch,tree_oid,config_hash,extractor_versions,resolver_version,attempt_token,attempt_state,generation_id) VALUES('graph','owner','incarnation',0,'repo','family',1,'authority',1,'tree','config','extractors','resolver','attempt','adopted',1)`, nil},
		{`PRAGMA user_version=22`, nil},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDependencyRevisionFullSchemaV22UpgradePreservesIdentity(t *testing.T) {
	path := dependencyRevisionV22File(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	row, found, err := s.Catalog().GetViewGeneration(context.Background(), 1)
	if err != nil || !found || row.State != ViewGenerationReady || row.TreeOID != "tree" || row.ConfigHash != "config" || row.DependencyRevision != "" {
		t.Fatalf("upgraded generation=%+v found=%v err=%v", row, found, err)
	}
	publication, found, err := s.Catalog().DedicatedBasePublication(context.Background(), "graph")
	if err != nil || !found || publication.Desire.Identity.DependencyRevision != "" || publication.Claim.GenerationID != 1 || publication.AttemptState != "adopted" {
		t.Fatalf("upgraded publication=%+v found=%v err=%v", publication, found, err)
	}
	graph, found, err := s.Catalog().GetDedicatedGraph(context.Background(), "graph")
	if err != nil || !found || graph.ActiveGenerationID != 1 {
		t.Fatalf("active graph=%+v found=%v err=%v", graph, found, err)
	}
}

func TestDependencyRevisionOpenFailureDoesNotStampAndRetries(t *testing.T) {
	path := dependencyRevisionV22File(t)
	want := errors.New("injected dependency migration failure")
	steps := make([]schemaMigration, 0, len(schemaMigrations))
	for _, step := range schemaMigrations {
		if step.version < 23 {
			steps = append(steps, step)
		}
	}
	steps = append(steps, schemaMigration{version: 23, name: "injected dependency migration", inPlace: func(tx *sql.Tx) error {
		if err := addDependencyRevisionColumns(tx); err != nil {
			return err
		}
		return want
	}})
	opened, err := openWithObserver(path, 23, steps, false, nil)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("failed migration returned a store")
	}
	if !errors.Is(err, want) {
		t.Fatalf("migration failure lost: %v", err)
	}
	probe, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := probe.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 22 {
		_ = probe.Close()
		t.Fatalf("failure stamped version=%d err=%v", version, err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM pragma_table_xinfo('view_generations') WHERE name='dependency_revision'`,
		`SELECT COUNT(*) FROM pragma_table_xinfo('dedicated_base_publications') WHERE name='dependency_revision'`,
	} {
		var count int
		if err := probe.QueryRow(query).Scan(&count); err != nil || count != 0 {
			_ = probe.Close()
			t.Fatalf("partial migration columns=%d err=%v", count, err)
		}
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	retried, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retried.Close() })
	row, found, err := retried.Catalog().GetViewGeneration(context.Background(), 1)
	if err != nil || !found || row.DependencyRevision != "" || row.State != ViewGenerationReady {
		t.Fatalf("retry changed prior row=%+v found=%v err=%v", row, found, err)
	}
}
