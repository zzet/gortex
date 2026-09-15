package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The two historical public parent-refusal tests below retain their original
// bodies. They now serve as acceptance oracles against the actual implementation.
// No old implementation overlay or prototype parent helper is included.

func retirementGapPayload(t *testing.T, f *dedicatedPublicationFixture, id int64) *graph.Node {
	t.Helper()
	node := &graph.Node{ID: "repo/claimed.go::Claimed", Name: "Claimed", Kind: graph.KindFunction,
		FilePath: "repo/claimed.go", RepoPrefix: "repo", Language: "go", StartLine: 7}
	f.store.AtGeneration(id).AddBatch([]*graph.Node{node}, nil)
	if got := f.store.AtGeneration(id).GetNode(node.ID); got == nil {
		t.Fatal("fixture failed to persist generation payload")
	}
	return node
}

func retirementGapReady(t *testing.T, f *dedicatedPublicationFixture, id int64) {
	t.Helper()
	if err := f.store.PublishPayloadGeneration(context.Background(), id, 2); err != nil {
		t.Fatal(err)
	}
}

func retirementGapMustSurvive(t *testing.T, f *dedicatedPublicationFixture, id int64, node *graph.Node, before ViewGenerationState) {
	t.Helper()
	row, found, err := f.c.GetViewGeneration(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Errorf("referenced generation %d was deleted", id)
	} else if row.State != before {
		t.Errorf("referenced generation %d changed state: want %s, got %s", id, before, row.State)
	}
	// Read the actual generation payload, not just the surviving catalog row.
	if got := f.store.AtGeneration(id).GetNode(node.ID); got == nil {
		t.Errorf("referenced generation %d payload was deleted", id)
	} else if got.Name != node.Name || got.StartLine != node.StartLine {
		t.Errorf("referenced generation %d payload changed: %+v", id, got)
	}
}

func retirementFenceUnclaimed(t *testing.T, f *dedicatedPublicationFixture) (int64, *graph.Node) {
	t.Helper()
	i := f.desire.Identity
	id, _, err := f.store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: f.graph.GraphID, CheckoutID: f.owner.CheckoutID,
		GenerationKind: "dedicated", TreeOID: i.TreeOID, ConfigHash: i.ConfigHash,
		ExtractorVersions: i.ExtractorVersions, ResolverVersion: i.ResolverVersion, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	node := retirementGapPayload(t, f, id)
	retirementGapReady(t, f, id)
	return id, node
}

func retirementFenceNoWrites(t *testing.T, f *dedicatedPublicationFixture) func() {
	t.Helper()
	f.exec(t, `CREATE TABLE retirement_fence_write_audit (n INTEGER NOT NULL)`)
	f.exec(t, `INSERT INTO retirement_fence_write_audit VALUES(0)`)
	f.exec(t, `CREATE TRIGGER retirement_fence_write_audit_update AFTER UPDATE ON view_generations BEGIN UPDATE retirement_fence_write_audit SET n=n+1; END`)
	before, err := os.Stat(f.path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		var writes int
		if err := f.store.db.QueryRow(`SELECT n FROM retirement_fence_write_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(f.path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		if writes != 0 || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			t.Errorf("unexpected writes=%d WAL bytes=%d→%d mtime unchanged=%v", writes, before.Size(), after.Size(), before.ModTime().Equal(after.ModTime()))
		}
	}
}

func parentGuardRequest(f *dedicatedPublicationFixture, parent int64, layer string) PayloadGenerationRequest {
	i := f.desire.Identity
	return PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: f.graph.GraphID, CheckoutID: f.owner.CheckoutID,
		GenerationKind: "dedicated", TreeOID: "child-tree", BaseGenerationID: parent, LayerID: layer,
		ConfigHash: i.ConfigHash, ExtractorVersions: i.ExtractorVersions, ResolverVersion: i.ResolverVersion, CreatedAt: 3,
	}
}

func TestParentGuardCurrentPublicStateCompatibility(t *testing.T) {
	for _, state := range []ViewGenerationState{ViewGenerationBuilding, ViewGenerationReady, ViewGenerationSuperseded, ViewGenerationFailed} {
		t.Run(string(state), func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			parent, _ := retirementFenceUnclaimed(t, f)
			ctx := context.Background()
			if err := f.c.SetViewGenerationState(ctx, parent, state); err != nil {
				t.Fatal(err)
			}
			req := parentGuardRequest(f, parent, "compat-child")
			first, handle, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, req)
			if err != nil || first <= parent || handle == nil || adopted {
				t.Fatalf("first allocation: state=%s id=%d handle=%v adopted=%v err=%v", state, first, handle != nil, adopted, err)
			}
			again, handle, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, req)
			if err != nil || again != first || handle == nil || !adopted {
				t.Fatalf("coalesced allocation: state=%s id=%d handle=%v adopted=%v err=%v", state, again, handle != nil, adopted, err)
			}
		})
	}
}

// This remains an integration acceptance oracle: unlike the new dedicated
// claim helper, the existing public allocator may not reject retiring parents.
func TestRetirementGapPublicAllocatorRejectsRetiringParent(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	parent, node := retirementFenceUnclaimed(t, f)
	ctx := context.Background()
	if err := f.c.BeginViewGenerationRetirement(ctx, parent); err != nil {
		t.Fatal(err)
	}
	i := f.desire.Identity
	child, _, err := f.store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: f.graph.GraphID, CheckoutID: f.owner.CheckoutID,
		GenerationKind: "dedicated", TreeOID: "child-tree", BaseGenerationID: parent,
		ConfigHash: i.ConfigHash, ExtractorVersions: i.ExtractorVersions, ResolverVersion: i.ResolverVersion, CreatedAt: 3,
	})
	t.Logf("public allocator after retirement fence: parent=%d child=%d err=%v", parent, child, err)
	if err == nil {
		t.Errorf("public allocator accepted retiring parent %d (child %d)", parent, child)
	}
	retirementGapMustSurvive(t, f, parent, node, ViewGenerationRetiring)
}

func TestRetirementGapPublicCoalescedAllocatorRejectsRetiringParent(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	parent, node := retirementFenceUnclaimed(t, f)
	ctx := context.Background()
	req := parentGuardRequest(f, parent, "coalesced-child")
	first, _, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, req)
	if err != nil || adopted || first <= parent {
		t.Fatalf("initial child fixture: id=%d adopted=%v err=%v", first, adopted, err)
	}
	checkID, _, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, req)
	if err != nil || !adopted || checkID != first {
		t.Fatalf("fixture did not reach building coalescence: id=%d adopted=%v err=%v", checkID, adopted, err)
	}
	// Explicit legacy/inconsistent-state fixture: the existing unguarded public
	// state API can leave a referenced parent retiring, as the earlier race did.
	// This is NOT a legitimate ordering permitted by the new atomic fence.
	if err := f.c.SetViewGenerationState(ctx, parent, ViewGenerationRetiring); err != nil {
		t.Fatal(err)
	}
	before := f.count(t)
	check := retirementFenceNoWrites(t, f)
	again, handle, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, req)
	t.Logf("public coalescence after parent retiring: parent=%d first=%d again=%d handle=%v adopted=%v err=%v", parent, first, again, handle != nil, adopted, err)
	if err == nil || handle != nil || adopted || again != 0 {
		t.Errorf("public allocator returned an existing child of retiring parent: id=%d handle=%v adopted=%v err=%v", again, handle != nil, adopted, err)
	}
	if f.count(t) != before {
		t.Error("coalesced rejection allocated another generation")
	}
	check()
	retirementGapMustSurvive(t, f, parent, node, ViewGenerationRetiring)
}

func parentTerminalRow(f *dedicatedPublicationFixture, parent int64, layer string) ViewGeneration {
	i := f.desire.Identity
	return ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: f.graph.GraphID, CheckoutID: f.owner.CheckoutID,
		GenerationKind: "dedicated", BaseGenerationID: parent, LayerID: layer, TreeOID: "parent-terminal-child",
		ConfigHash: i.ConfigHash, ExtractorVersions: i.ExtractorVersions, ResolverVersion: i.ResolverVersion,
		State: ViewGenerationBuilding, CreatedAt: 3,
	}
}

func parentTerminalCall(t *testing.T, f *dedicatedPublicationFixture, method string, row ViewGeneration) (int64, bool, *Store, error) {
	t.Helper()
	ctx := context.Background()
	switch method {
	case "create":
		id, err := f.c.CreateViewGeneration(ctx, row)
		return id, false, nil, err
	case "adopt_named", "adopt_unnamed":
		id, adopted, err := f.c.AdoptOrCreateViewGeneration(ctx, row)
		return id, adopted, nil, err
	case "begin":
		id, handle, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, PayloadGenerationRequest{
			OwnerKind: row.OwnerKind, GraphID: row.GraphID, CheckoutID: row.CheckoutID,
			GenerationKind: row.GenerationKind, BaseGenerationID: row.BaseGenerationID,
			LayerID: row.LayerID, TreeOID: row.TreeOID, ConfigHash: row.ConfigHash,
			ExtractorVersions: row.ExtractorVersions, ResolverVersion: row.ResolverVersion, CreatedAt: row.CreatedAt,
		})
		return id, adopted, handle, err
	default:
		t.Fatalf("unknown parent admission method %q", method)
		return 0, false, nil, nil
	}
}

// This audit counts committed logical row mutations, including insert/delete,
// rather than treating an attempted INSERT before rollback as a durable child.
func parentTerminalNoWrites(t *testing.T, f *dedicatedPublicationFixture) func() {
	t.Helper()
	before := f.count(t)
	f.exec(t, `CREATE TABLE parent_terminal_audit(n INTEGER NOT NULL)`)
	f.exec(t, `INSERT INTO parent_terminal_audit VALUES(0)`)
	for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
		f.exec(t, fmt.Sprintf(`CREATE TRIGGER parent_terminal_%s AFTER %s ON view_generations BEGIN UPDATE parent_terminal_audit SET n=n+1; END`, operation, operation))
	}
	return func() {
		t.Helper()
		var writes int
		if err := f.store.db.QueryRow(`SELECT n FROM parent_terminal_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		if after := f.count(t); writes != 0 || after != before {
			t.Errorf("refused/no-op admission changed rows: writes=%d count=%d want=%d", writes, after, before)
		}
	}
}

func parentTerminalSnapshot(t *testing.T, f *dedicatedPublicationFixture, id int64) ViewGeneration {
	t.Helper()
	row, found, err := f.c.GetViewGeneration(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("generation snapshot: id=%d found=%v err=%v", id, found, err)
	}
	return row
}

func parentTerminalParent(t *testing.T, f *dedicatedPublicationFixture, state string) int64 {
	t.Helper()
	if state == "root" {
		return 0
	}
	id, _ := retirementFenceUnclaimed(t, f)
	ctx := context.Background()
	switch state {
	case "retiring":
		if err := f.c.BeginViewGenerationRetirement(ctx, id); err != nil {
			t.Fatal(err)
		}
	case "missing":
		if err := f.store.RetirePayloadGeneration(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		if _, found, err := f.c.GetViewGeneration(ctx, id); err != nil || found {
			t.Fatalf("missing parent fixture remains: found=%v err=%v", found, err)
		}
	default:
		// Generic parent policy permits these construction states. This fixture
		// does not assert that Building/Failed parents are servable to readers.
		if err := f.c.SetViewGenerationState(ctx, id, ViewGenerationState(state)); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestGenerationParentAdmissionFreshBranches(t *testing.T) {
	for _, method := range []string{"create", "adopt_named", "adopt_unnamed", "begin"} {
		for _, state := range []string{"root", "building", "ready", "superseded", "failed", "retiring", "missing"} {
			t.Run(method+"/"+state, func(t *testing.T) {
				f := newDedicatedPublicationFixture(t)
				parent := parentTerminalParent(t, f, state)
				row := parentTerminalRow(f, parent, "fresh-child")
				if method == "adopt_unnamed" {
					row.LayerID = ""
				}
				beforeCount := f.count(t)
				var beforeParent ViewGeneration
				if parent > 0 && state != "missing" {
					beforeParent = parentTerminalSnapshot(t, f, parent)
				}
				var check func()
				refused := state == "retiring" || state == "missing"
				if refused {
					check = parentTerminalNoWrites(t, f)
				}
				id, adopted, handle, err := parentTerminalCall(t, f, method, row)
				if refused {
					if err == nil || id != 0 || adopted || handle != nil {
						t.Errorf("refused parent returned child: id=%d adopted=%v handle=%v err=%v", id, adopted, handle != nil, err)
					}
					if state == "retiring" && !errors.Is(err, ErrCatalogGenerationRetiring) {
						t.Errorf("retiring refusal lost its cause: %v", err)
					}
					// Missing may fail an existing SQL constraint before the new
					// lookup; do not replace its established error with a new one.
					check()
				} else {
					if err != nil || id <= 0 || adopted || (method == "begin" && handle == nil) {
						t.Fatalf("healthy fresh admission: id=%d adopted=%v handle=%v err=%v", id, adopted, handle != nil, err)
					}
					child := parentTerminalSnapshot(t, f, id)
					if child.BaseGenerationID != parent || child.State != ViewGenerationBuilding || f.count(t) != beforeCount+1 {
						t.Errorf("healthy child not persisted correctly: %+v", child)
					}
					if method == "adopt_named" || method == "begin" {
						again, reused, nextHandle, err := parentTerminalCall(t, f, method, row)
						if err != nil || again != id || !reused || (method == "begin" && nextHandle == nil) || f.count(t) != beforeCount+1 {
							t.Errorf("healthy matching admission: id=%d adopted=%v handle=%v err=%v", again, reused, nextHandle != nil, err)
						}
					}
				}
				if parent > 0 && state != "missing" && !reflect.DeepEqual(parentTerminalSnapshot(t, f, parent), beforeParent) {
					t.Error("child admission changed the parent row")
				}
			})
		}
	}
}

func TestGenerationParentAdmissionMatchedRetiring(t *testing.T) {
	for _, method := range []string{"adopt_named", "begin"} {
		t.Run(method, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			parent, node := retirementFenceUnclaimed(t, f)
			row := parentTerminalRow(f, parent, "matching-child")
			first, adopted, _, err := parentTerminalCall(t, f, method, row)
			if err != nil || adopted || first <= parent {
				t.Fatalf("healthy first child: id=%d adopted=%v err=%v", first, adopted, err)
			}
			again, adopted, _, err := parentTerminalCall(t, f, method, row)
			if err != nil || !adopted || again != first {
				t.Fatalf("healthy matching child: id=%d adopted=%v err=%v", again, adopted, err)
			}
			ctx := context.Background()
			if err := f.c.BeginViewGenerationRetirement(ctx, parent); !errors.Is(err, ErrCatalogGenerationReferenced) {
				t.Fatalf("legal fence ignored existing child: %v", err)
			}
			retirementGapMustSurvive(t, f, parent, node, ViewGenerationReady)
			// Explicit legacy/inconsistent-state setup, NOT a legal retirement
			// while a child holds the parent. The generic setter permits this.
			if err := f.c.SetViewGenerationState(ctx, parent, ViewGenerationRetiring); err != nil {
				t.Fatal(err)
			}
			beforeParent, beforeChild := parentTerminalSnapshot(t, f, parent), parentTerminalSnapshot(t, f, first)
			check := parentTerminalNoWrites(t, f)
			again, adopted, handle, err := parentTerminalCall(t, f, method, row)
			if !errors.Is(err, ErrCatalogGenerationRetiring) || again != 0 || adopted || handle != nil {
				t.Errorf("matched parent refusal: id=%d adopted=%v handle=%v err=%v", again, adopted, handle != nil, err)
			}
			check()
			if !reflect.DeepEqual(parentTerminalSnapshot(t, f, parent), beforeParent) || !reflect.DeepEqual(parentTerminalSnapshot(t, f, first), beforeChild) {
				t.Error("refused reuse changed existing parent or child")
			}
			retirementGapMustSurvive(t, f, parent, node, ViewGenerationRetiring)
		})
	}
}

func TestGenerationParentAdmissionValidationPriority(t *testing.T) {
	for _, method := range []string{"create", "adopt_named", "adopt_unnamed"} {
		t.Run(method, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			parent := parentTerminalParent(t, f, "retiring")
			row := parentTerminalRow(f, parent, "invalid-child")
			if method == "adopt_unnamed" {
				row.LayerID = ""
			}
			check := parentTerminalNoWrites(t, f)
			states := []ViewGenerationState{"invalid-child-state"}
			if method != "create" {
				states = append(states, ViewGenerationReady)
			}
			for _, state := range states {
				row.State = state
				id, adopted, handle, err := parentTerminalCall(t, f, method, row)
				if !errors.Is(err, ErrCatalogInvalidValue) || errors.Is(err, ErrCatalogGenerationRetiring) || id != 0 || adopted || handle != nil {
					t.Errorf("child validation lost priority: id=%d adopted=%v err=%v", id, adopted, err)
				}
			}
			check()
		})
	}
}

func TestGenerationParentAdmissionSQLPriority(t *testing.T) {
	for _, method := range []string{"create", "adopt_named", "adopt_unnamed", "adopt_matched"} {
		t.Run(method, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			parent, _ := retirementFenceUnclaimed(t, f)
			row := parentTerminalRow(f, parent, "sql-priority-child")
			callMethod := method
			if method == "adopt_unnamed" {
				row.LayerID = ""
			}
			if method == "adopt_matched" {
				callMethod = "adopt_named"
				id, adopted, _, err := parentTerminalCall(t, f, callMethod, row)
				if err != nil || adopted || id <= parent {
					t.Fatalf("matching SQL-priority fixture: id=%d adopted=%v err=%v", id, adopted, err)
				}
				// Only this branch deliberately creates a legacy inconsistent
				// parent state; normal new-child branches use the atomic fence.
				if err := f.c.SetViewGenerationState(context.Background(), parent, ViewGenerationRetiring); err != nil {
					t.Fatal(err)
				}
			} else if err := f.c.BeginViewGenerationRetirement(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `CREATE TRIGGER parent_priority_failure BEFORE INSERT ON view_generations BEGIN SELECT RAISE(ABORT, 'parent-priority'); END`)
			check := parentTerminalNoWrites(t, f)
			id, adopted, handle, err := parentTerminalCall(t, f, callMethod, row)
			if err == nil || id != 0 || adopted || handle != nil {
				t.Errorf("SQL-priority refusal returned child: id=%d adopted=%v err=%v", id, adopted, err)
			} else if method == "adopt_matched" {
				if !errors.Is(err, ErrCatalogGenerationRetiring) || strings.Contains(err.Error(), "parent-priority") {
					t.Errorf("matched path unexpectedly inserted or lost parent cause: %v", err)
				}
			} else if !strings.Contains(err.Error(), "parent-priority") || errors.Is(err, ErrCatalogGenerationRetiring) {
				t.Errorf("earlier INSERT error lost priority: %v", err)
			}
			check()
		})
	}
}

func parentTerminalTransition(f *dedicatedPublicationFixture, handle *Store, id int64, operation string) error {
	ctx := context.Background()
	switch operation {
	case "catalog_publish":
		return f.c.PublishViewGeneration(ctx, id, 9)
	case "store_publish_base":
		return f.store.PublishPayloadGeneration(ctx, id, 9)
	case "store_publish_managed":
		return handle.PublishPayloadGeneration(ctx, id, 9)
	case "guarded_failure":
		return f.c.SetViewGenerationState(ctx, id, ViewGenerationFailed, ViewGenerationBuilding)
	case "guarded_supersession":
		return f.c.SetViewGenerationState(ctx, id, ViewGenerationSuperseded, ViewGenerationReady)
	default:
		return fmt.Errorf("unknown terminal operation %q", operation)
	}
}

func TestGenerationRetiringRejectsLateGuardedTransitions(t *testing.T) {
	for _, operation := range []string{"catalog_publish", "store_publish_base", "store_publish_managed", "guarded_failure", "guarded_supersession"} {
		for _, fenced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fenced=%t", operation, fenced), func(t *testing.T) {
				f := newDedicatedPublicationFixture(t)
				ctx := context.Background()
				id, handle, err := f.store.BeginPayloadGeneration(ctx, parentGuardRequest(f, 0, "terminal-child"))
				if err != nil || id <= 0 || handle == nil {
					t.Fatalf("terminal fixture allocation: id=%d handle=%v err=%v", id, handle != nil, err)
				}
				node := retirementGapPayload(t, f, id)
				want := ViewGenerationReady
				if operation == "guarded_failure" {
					want = ViewGenerationFailed
				}
				if operation == "guarded_supersession" {
					retirementGapReady(t, f, id)
					want = ViewGenerationSuperseded
				}
				if fenced {
					if err := f.c.BeginViewGenerationRetirement(ctx, id); err != nil {
						t.Fatal(err)
					}
					before := parentTerminalSnapshot(t, f, id)
					check := parentTerminalNoWrites(t, f)
					err := parentTerminalTransition(f, handle, id, operation)
					if err == nil {
						t.Error("late guarded transition accepted Retiring generation")
					}
					if !strings.HasPrefix(operation, "store_publish_") && !errors.Is(err, ErrCatalogStaleGuard) {
						t.Errorf("catalog guarded transition lost stale cause: %v", err)
					}
					check()
					if !reflect.DeepEqual(parentTerminalSnapshot(t, f, id), before) {
						t.Error("late transition changed fenced row or publication metadata")
					}
					want = ViewGenerationRetiring
				} else if err := parentTerminalTransition(f, handle, id, operation); err != nil {
					t.Fatalf("healthy guarded transition failed: %v", err)
				}
				retirementGapMustSurvive(t, f, id, node, want)
			})
		}
	}
}
