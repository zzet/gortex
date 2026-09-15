package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func newIdentityMaskStore(t testing.TB) (*Store, *Store) {
	t.Helper()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "mask.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	id, err := s.Catalog().CreateViewGeneration(context.Background(), ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: "mask-fixture", GenerationKind: "dedicated",
		TreeOID: "tree", ConfigHash: "policy", State: ViewGenerationBuilding, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, s.AtGeneration(id)
}

func TestNodeIdentityMaskMigrationPreservesLegacyAndIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE generation_node_tombstones (
view_gen INTEGER NOT NULL, node_id TEXT NOT NULL,
PRIMARY KEY(view_gen,node_id)) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO generation_node_tombstones VALUES (7,'alpha/a::Old')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := addNodeIdentityMaskKinds(tx); err != nil {
			t.Fatal(err)
		}
	}
	var kind string
	if err := tx.QueryRow(`SELECT claim_kind FROM generation_node_tombstones WHERE view_gen=7 AND node_id='alpha/a::Old'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != string(NodeIdentityMaskLegacy) {
		t.Fatalf("migration weakened legacy to %q", kind)
	}
	if _, err := tx.Exec(`INSERT INTO generation_node_tombstones(view_gen,node_id) VALUES(8,'alpha/a::Old')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM generation_node_tombstones WHERE claim_kind='legacy_tombstone'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("default/PK drift n=%d err=%v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeIdentityMaskUpsertsAreMonotoneInBothOrders(t *testing.T) {
	for _, legacyFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_first_%t", legacyFirst), func(t *testing.T) {
			_, h := newIdentityMaskStore(t)
			id := "alpha/a::Node"
			first, second := h.SetNodeIdentityReplacements, h.SetNodeTombstones
			if legacyFirst {
				first, second = second, first
			}
			for _, write := range []func([]string) error{first, second, h.SetNodeIdentityReplacements} {
				if err := write([]string{id, id}); err != nil {
					t.Fatal(err)
				}
			}
			masks, err := h.NodeIdentityMasks()
			if err != nil || !reflect.DeepEqual(masks, []NodeIdentityMask{{NodeID: id, Kind: NodeIdentityMaskLegacy}}) {
				t.Fatalf("mask=%v err=%v", masks, err)
			}
			old, err := h.NodeTombstones()
			if err != nil || !reflect.DeepEqual(old, []string{id}) {
				t.Fatalf("legacy reader=%v err=%v", old, err)
			}
			if err := h.ValidateGenerationMasks(); err != nil {
				t.Fatalf("legacy missing-row contract changed: %v", err)
			}
		})
	}
}

func TestNodeIdentityMaskConcurrentUpgradesNeverReviveOutgoingOwnership(t *testing.T) {
	_, h := newIdentityMaskStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(legacy bool) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				var err error
				if legacy {
					err = h.SetNodeTombstones([]string{"alpha/a::N"})
				} else {
					err = h.SetNodeIdentityReplacements([]string{"alpha/a::N"})
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	masks, err := h.NodeIdentityMasks()
	if err != nil || len(masks) != 1 || masks[0].Kind != NodeIdentityMaskLegacy {
		t.Fatalf("mask=%v err=%v", masks, err)
	}
}

func TestNodeIdentityMaskPublishRequiresSameGenerationRow(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	id := "alpha/a::N"
	s.AddNode(&graph.Node{ID: id, Name: "base poison"})
	if err := h.SetNodeIdentityReplacements([]string{id}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishPayloadGeneration(context.Background(), h.viewGen, 2); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("missing positive row published: %v", err)
	}
	row, found, err := s.Catalog().GetViewGeneration(context.Background(), h.viewGen)
	if err != nil || !found || row.State != ViewGenerationBuilding {
		t.Fatalf("failed publish state=%v found=%t err=%v", row.State, found, err)
	}
	h.AddNode(&graph.Node{ID: id, Name: "upper", RepoPrefix: "alpha", FilePath: "alpha/a"})
	if err := s.PublishPayloadGeneration(context.Background(), h.viewGen, 3); err != nil {
		t.Fatal(err)
	}
	if err := h.SetNodeTombstones([]string{id}); err == nil {
		t.Fatal("ready generation accepted claim upgrade")
	}
	if s.GetNode(id).Name != "base poison" {
		t.Fatal("generation zero mutated")
	}
}

func TestNodeIdentityMaskCheckedSummariesScopeAndMissingPolicy(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	id := "alpha/a::N"
	s.AddNode(&graph.Node{ID: id, Name: "poison"})
	masks := []NodeIdentityMask{{NodeID: id, Kind: NodeIdentityMaskReplace}, {NodeID: "alpha/a::Deleted", Kind: NodeIdentityMaskLegacy}}
	if rows, err := h.NodeIdentityMaskSummariesContext(context.Background(), masks); !errors.Is(err, ErrGenerationMaskIntegrity) || rows != nil {
		t.Fatalf("lower fallback/partial rows=%v err=%v", rows, err)
	}
	h.AddNode(&graph.Node{ID: id, Name: "upper", FilePath: "alpha/a", RepoPrefix: "alpha", Meta: map[string]interface{}{"heavy": "not loaded"}})
	rows, err := h.NodeIdentityMaskSummariesContext(context.Background(), masks)
	if err != nil || len(rows) != 1 || rows[0].Name != "upper" || rows[0].Meta != nil {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := h.NodeIdentityMaskSummariesContext(ctx, masks); !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("canceled rows=%v err=%v", rows, err)
	}
	if rows, err := h.NodeIdentityMaskSummariesContext(context.Background(), nil); err != nil || len(rows) != 0 {
		t.Fatalf("empty rows=%v err=%v", rows, err)
	}
}

func TestNodeIdentityMaskUnknownKindFailsClosedAndRollsBackWholeBatch(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	ids := make([]string, generationMaskChunk+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("alpha/a::N%04d", i)
	}
	// The public read pool is intentionally query-only. Inject the invalid
	// format through the same writer admission used by the real mask setter.
	if err := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		tx, err := s.beginWrite()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO generation_node_tombstones(view_gen,node_id,claim_kind) VALUES(?,?,'future_kind')`, h.viewGen, ids[len(ids)-1]); err != nil {
			return err
		}
		return tx.Commit()
	}(); err != nil {
		t.Fatal(err)
	}
	if masks, err := h.NodeIdentityMasks(); !errors.Is(err, ErrGenerationMaskInvalidValue) || masks != nil {
		t.Fatalf("unknown accepted: %v %v", masks, err)
	}
	if err := h.SetNodeIdentityReplacements(ids); err == nil {
		t.Fatal("unknown stored kind downgraded")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM generation_node_tombstones WHERE view_gen=?`, h.viewGen).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial transaction count=%d err=%v", count, err)
	}
	if err := h.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("unknown published: %v", err)
	}
}

func TestNodeIdentityMaskBaseRefusalAndClaimIndependence(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	if err := s.SetNodeIdentityReplacements(nil); !errors.Is(err, ErrMasksAtBaseGeneration) {
		t.Fatalf("base accepted: %v", err)
	}
	if err := h.SetNodeIdentityReplacements([]string{"alpha/a::N"}); err != nil {
		t.Fatal(err)
	}
	if old, err := h.NodeTombstones(); err != nil || len(old) != 0 {
		t.Fatalf("identity-only misclassified as legacy: %v %v", old, err)
	}
	if edges, err := h.EdgeSourceMasks(); err != nil || len(edges) != 0 {
		t.Fatalf("identity-only gained outgoing ownership: %v %v", edges, err)
	}
	if files, err := h.FileMasks(); err != nil || len(files) != 0 {
		t.Fatalf("identity-only gained file ownership: %v %v", files, err)
	}
}

func TestNodeIdentityMaskRetirementUsesExistingMaskSweep(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	h.AddNode(&graph.Node{ID: "alpha/a::N", RepoPrefix: "alpha", FilePath: "alpha/a"})
	if err := h.SetNodeIdentityReplacements([]string{"alpha/a::N"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishPayloadGeneration(context.Background(), h.viewGen, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.RetirePayloadGeneration(context.Background(), h.viewGen, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM generation_node_tombstones WHERE view_gen=?`, h.viewGen).Scan(&count); err != nil || count != 0 {
		t.Fatalf("mask leak=%d err=%v", count, err)
	}
}

func TestNodeIdentityMaskSummaryProjectionUsesIdentitySeeks(t *testing.T) {
	_, h := newIdentityMaskStore(t)
	h.AddNode(&graph.Node{ID: "alpha/a::N", RepoPrefix: "alpha", FilePath: "alpha/a"})
	rows, err := h.db.Query("EXPLAIN QUERY PLAN "+nodeIdentitySummaryQuery(2), h.viewGen, "alpha/a::N", "alpha/a::Absent")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundSeek := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN nodes") {
			t.Fatalf("upper-payload scan: %s", detail)
		}
		if strings.Contains(detail, "SEARCH nodes") {
			foundSeek = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundSeek {
		t.Fatal("no indexed node identity seek in projection plan")
	}
}
