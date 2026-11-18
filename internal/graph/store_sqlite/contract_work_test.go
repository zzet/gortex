package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func contractWorkFixture(token string) graph.ContractWork {
	return graph.ContractWork{
		Token: token, CheckoutID: "wt", RepoPrefix: "repo", FilePath: "repo/removed.go",
		InputVersion: "boundary-v1", InputFingerprint: "accepted-old-and-new", State: graph.ContractWorkPending,
		Scope: graph.ContractWorkScope{Deleted: true, Causes: []string{"removed_owner", "shared_type"},
			Groups:    []graph.ContractWorkGroup{{WorkspaceID: "ws", ProjectID: "project", ContractID: "http::/old"}},
			SymbolIDs: []string{"old-handler"}, LookupKeys: []string{"ws\x00type\x00Payload"}},
	}
}

func TestContractWorkFoldPreservesDeletionAndExactAcknowledgments(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	bottom, upper, folded, copied := reservedGeneration(t, store, "contract-bottom"), reservedGeneration(t, store, "contract-upper"),
		reservedGeneration(t, store, "contract-folded"), reservedGeneration(t, store, "contract-copied")
	old, latest := contractWorkFixture("old-input"), contractWorkFixture("latest-input")
	old.OriginGeneration, latest.OriginGeneration = bottom, bottom
	if err := store.AtGeneration(bottom).SetContractWork(ctx, []graph.ContractWork{old, latest}); err != nil {
		t.Fatal(err)
	}
	old.State = graph.ContractWorkComplete
	if err := store.AtGeneration(upper).SetContractWork(ctx, []graph.ContractWork{old}); err != nil {
		t.Fatal(err)
	}
	if err := store.AtGeneration(upper).SetFileMasks([]FileMask{{RepoPrefix: "repo", FilePath: old.FilePath, Mode: OwnershipDelete}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FlattenGenerationChain(ctx, []int64{bottom, upper}, folded); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CopyGenerationPayloadWhole(ctx, folded, copied); err != nil {
		t.Fatal(err)
	}
	want := []graph.ContractWork{latest, old}
	for _, id := range []int64{folded, copied} {
		got, err := store.AtGeneration(id).ContractWorkContext(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("generation %d work = %#v, %v; want %#v", id, got, err, want)
		}
	}
	// Origin can already have been folded out. Preserve an acknowledgment
	// rather than letting the original pending token resurface below a future
	// fold boundary. Its immutable origin is not rewritten to the copied ID.
	ack, refold := reservedGeneration(t, store, "contract-late-ack"), reservedGeneration(t, store, "contract-refold")
	latest.State = graph.ContractWorkComplete
	if err := store.AtGeneration(ack).SetContractWork(ctx, []graph.ContractWork{latest}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FlattenGenerationChain(ctx, []int64{copied, ack}, refold); err != nil {
		t.Fatal(err)
	}
	got, err := store.AtGeneration(refold).ContractWorkContext(ctx)
	if err != nil || !reflect.DeepEqual(got, []graph.ContractWork{latest, old}) {
		t.Fatalf("refolded work = %#v, %v", got, err)
	}
}

func TestContractWorkScopeAndCanceledWrites(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	primary := contractWorkFixture("primary")
	primary.CheckoutID = ""
	linked, other := contractWorkFixture("linked"), contractWorkFixture("other-repo")
	other.RepoPrefix = "other"
	if err := store.SetContractWork(ctx, []graph.ContractWork{primary, linked, other}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ContractWorkForScopeContext(ctx, "repo", "")
	if err != nil || !reflect.DeepEqual(got, []graph.ContractWork{primary}) {
		t.Fatalf("scoped work = %#v, %v", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.SetContractWork(canceled, []graph.ContractWork{contractWorkFixture("canceled")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	invalid := contractWorkFixture("invalid")
	invalid.State = "unknown"
	if err := store.SetContractWork(ctx, []graph.ContractWork{contractWorkFixture("valid"), invalid}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	got, err = store.ContractWorkContext(ctx)
	if err != nil || len(got) != 3 {
		t.Fatalf("failed batches changed work: %#v, %v", got, err)
	}
	if got, err := store.ContractWorkContext(canceled); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled read = %#v, %v", got, err)
	}
}

func TestContractWorkReopenAndSealedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := reservedGeneration(t, store, "contract-reopen")
	work := contractWorkFixture("reopen")
	if err := store.AtGeneration(id).SetContractWork(ctx, []graph.ContractWork{work}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPayloadGeneration(ctx, id, 20); err != nil {
		t.Fatal(err)
	}
	if err := store.AtGeneration(id).SetContractWork(ctx, []graph.ContractWork{work}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("sealed write = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openPristine(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.AtGeneration(id).ContractWorkContext(ctx)
	if err != nil || !reflect.DeepEqual(got, []graph.ContractWork{work}) {
		t.Fatalf("reopened work = %#v, %v", got, err)
	}
}

func TestContractWorkManagedBulkScopedCopyAndRollback(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	work, foreign := contractWorkFixture("base"), contractWorkFixture("foreign")
	foreign.RepoPrefix = "other"
	if err := store.SetContractWork(ctx, []graph.ContractWork{work, foreign}); err != nil {
		t.Fatal(err)
	}
	id := reservedGeneration(t, store, "contract-scoped-bulk")
	opened, err := store.BeginGenerationBulkLoad(id)
	if err != nil || !opened {
		t.Fatalf("bulk open = %v, %v", opened, err)
	}
	if _, err := store.CopyPayloadGeneration(ctx, 0, id, "repo"); err != nil {
		t.Fatal(err)
	}
	handle, err := store.AtManagedGeneration(id)
	if err != nil {
		t.Fatal(err)
	}
	newWork := contractWorkFixture("new")
	newWork.OriginGeneration = id
	if err := handle.SetContractWork(ctx, []graph.ContractWork{newWork}); err != nil {
		t.Fatal(err)
	}
	changed := work
	changed.InputFingerprint = "not-the-accepted-input"
	if err := handle.SetContractWork(ctx, []graph.ContractWork{contractWorkFixture("rolled-back"), changed}); err == nil {
		t.Fatal("token identity mutation accepted")
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatal(err)
	}
	got, err := handle.ContractWorkContext(ctx)
	if err != nil || !reflect.DeepEqual(got, []graph.ContractWork{work, newWork}) {
		t.Fatalf("scoped managed work = %#v, %v", got, err)
	}
}

func TestContractWorkDecodeAndLimitReturnNoPartialProof(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	work := contractWorkFixture("a-valid")
	broken := contractWorkFixture("z-broken")
	if err := store.SetContractWork(ctx, []graph.ContractWork{work, broken}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writerDB.Exec(`UPDATE generation_contract_work SET scope = '{' WHERE token = 'z-broken'`); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ContractWorkContext(ctx); err == nil || got != nil {
		t.Fatalf("partial decode certified = %#v, %v", got, err)
	}
	if _, err := store.writerDB.Exec(`DELETE FROM generation_contract_work`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writerDB.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i <= ?)
 INSERT INTO generation_contract_work
 (view_gen, token, origin_generation, checkout_id, repo_prefix, file_path, input_version, input_fingerprint, state, scope)
 SELECT 0, 'work-' || i, 0, '', 'repo', 'repo/file', 'v1', 'input', 'pending', '{}' FROM n`, contractWorkReadLimit); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ContractWorkForScopeContext(ctx, "repo", ""); !errors.Is(err, ErrContractWorkLimit) || got != nil {
		t.Fatalf("limited read certified = %d rows, %v", len(got), err)
	}
}

func TestContractStateContextDistinguishesZeroBaselineAndCancellation(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	if _, found, err := store.GetContractStateContext(ctx, "repo"); err != nil || found {
		t.Fatalf("unbuilt baseline = %v, %v", found, err)
	}
	want := graph.ContractState{RepoPrefix: "repo", IndexedSHA: "accepted-tree", CompletedAt: 123, ContractCount: 0}
	if err := store.SetContractState(want); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetContractStateContext(ctx, "repo")
	if err != nil || !found || got != want {
		t.Fatalf("completed true zero = %#v, %v, %v", got, found, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, found, err := store.GetContractStateContext(canceled, "repo"); !errors.Is(err, context.Canceled) || found {
		t.Fatalf("canceled baseline = %v, %v", found, err)
	}
}
