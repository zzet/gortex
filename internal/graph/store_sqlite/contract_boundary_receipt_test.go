package store_sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func boundaryStorageReceipt(file, fingerprint string) graph.ContractBoundaryReceipt {
	return graph.ContractBoundaryReceipt{RepoPrefix: "repo", FilePath: "repo/" + file, Version: "contract-boundary-v1", Fingerprint: fingerprint, SourceFingerprint: "source-" + fingerprint, Payload: []byte(`{"records":[],"produced_inputs":{"Payload":"digest"}}`), LookupKeys: []string{"*::type::Payload", "project::http::GET::/unmatched"}, ProducedKeys: []string{"repo::symbol::serve"}, Scope: graph.ContractWorkScope{Groups: []graph.ContractWorkGroup{{WorkspaceID: "workspace", ProjectID: "project", ContractID: "http::GET::/unmatched"}}, Causes: []string{"negative_membership"}}}
}

func TestContractBoundaryReceiptIndexedNegativeOwnersAndPendingUnion(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	old := boundaryStorageReceipt("consumer.go", "old")
	row, known, err := s.ContractBoundaryReceiptForVersionContext(ctx, "repo", "", "new.go", old.Version)
	if err != nil || known || row != nil {
		t.Fatalf("legacy absence=%#v %v %v", row, known, err)
	}
	if err := s.SetContractBoundaryReceiptBaselineContext(ctx, graph.ContractBoundaryReceiptBaseline{RepoPrefix: "repo", Version: old.Version, Fingerprint: "policy-v1"}); err != nil {
		t.Fatal(err)
	}
	row, known, err = s.ContractBoundaryReceiptForVersionContext(ctx, "repo", "", "new.go", old.Version)
	if err != nil || !known || row != nil {
		t.Fatalf("certified new file=%#v %v %v", row, known, err)
	}
	_, known, err = s.ContractBoundaryReceiptForVersionContext(ctx, "repo", "", "new.go", "future-version")
	if err != nil || known {
		t.Fatalf("stale baseline certified newfile %v %v", known, err)
	}
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}); err != nil {
		t.Fatal(err)
	}
	dependent := old
	dependent.RepoPrefix = "other"
	dependent.CheckoutID = "other-actor"
	dependent.FilePath = "other/provider.go"
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{dependent}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{dependent}); err != nil {
		t.Fatal(err)
	}
	matches, err := s.ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{"*::type::Payload"}, 10)
	if err != nil || len(matches) != 2 {
		t.Fatalf("negative dependency owners=%#v %v", matches, err)
	}
	foundOther := false
	for _, r := range matches {
		if r.RepoPrefix == "other" {
			foundOther = r.CheckoutID == "other-actor" && r.FilePath == "other/provider.go" && r.Accepted && r.Version == old.Version && reflect.DeepEqual(r.Scope, dependent.Scope)
		}
	}
	if !foundOther {
		t.Fatal("reverse owner namespace/source/scope lost")
	}
	mid := old
	mid.Fingerprint = "middle"
	mid.SourceFingerprint = "source-middle"
	mid.LookupKeys = []string{"*::type::Middle"}
	mid.ProducedKeys = []string{"repo::symbol::middle"}
	latest := mid
	latest.Fingerprint = "latest"
	latest.SourceFingerprint = "source-latest"
	latest.LookupKeys = []string{"*::type::Latest"}
	latest.ProducedKeys = []string{"repo::symbol::latest"}
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{mid}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{latest}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"project::http::GET::/unmatched", "*::type::Middle", "*::type::Latest"} {
		matches, err := s.ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{key}, 10)
		if err != nil || len(matches) == 0 {
			t.Fatalf("inflight key vanished=%s %v", key, err)
		}
		for _, r := range matches {
			if r.RepoPrefix == "repo" && r.Accepted {
				t.Fatal("staged key looked accepted")
			}
		}
	}
	row, known, err = s.ContractBoundaryReceiptContext(ctx, "repo", "", old.FilePath)
	if err != nil || !known || row.Previous == nil || row.Previous.Fingerprint != "old" || !row.Previous.Accepted || row.Previous.Previous != nil {
		t.Fatalf("bounded predecessor=%#v %v %v", row, known, err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{mid}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("older acceptance=%v", err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{latest}); err != nil {
		t.Fatal(err)
	}
	matches, err = s.ContractBoundaryReceiptsForProducedKeysContext(ctx, []string{"repo::symbol::middle"}, 10)
	if err != nil || len(matches) != 0 {
		t.Fatalf("old staged produced key survived=%#v %v", matches, err)
	}
	matches, err = s.ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{"*::type::Latest"}, 10)
	if err != nil || len(matches) != 1 || !matches[0].Accepted {
		t.Fatalf("latest accepted owner=%#v %v", matches, err)
	}
	// Rename is two exact owners: the old tombstone masks its membership,
	// while the new file retains negative lookup membership independently.
	deleted := latest
	deleted.Deleted = true
	deleted.Fingerprint = "rename-delete"
	deleted.SourceFingerprint = "absent"
	deleted.LookupKeys = nil
	deleted.ProducedKeys = nil
	deleted.Payload = nil
	renamed := latest
	renamed.FilePath = "repo/renamed.go"
	renamed.Fingerprint = "renamed"
	renamed.SourceFingerprint = "source-renamed"
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{deleted, renamed}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{deleted, renamed}); err != nil {
		t.Fatal(err)
	}
	matches, err = s.ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{"*::type::Latest"}, 10)
	if err != nil || len(matches) != 1 || matches[0].FilePath != renamed.FilePath {
		t.Fatalf("rename old owner survived=%#v %v", matches, err)
	}
	plan, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT file_path FROM generation_contract_boundary_keys WHERE view_gen=0 AND key_kind='lookup' AND lookup_key='*::type::Latest'`)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var details strings.Builder
	for plan.Next() {
		var a, b, c int
		var text string
		if err := plan.Scan(&a, &b, &c, &text); err != nil {
			t.Fatal(err)
		}
		details.WriteString(text)
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details.String(), "SEARCH") || strings.Contains(details.String(), "SCAN generation_contract_boundary_keys") {
		t.Fatalf("unindexed reverse plan=%s", details.String())
	}
}

func TestContractBoundaryReceiptAtomicStateAcceptanceAndLimits(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	state := attachmentState("receipt-state")
	state.CheckoutID = ""
	receipt := boundaryStorageReceipt("handler.go", "captured")
	work := contractWorkFixture("receipt-token")
	work.CheckoutID = ""
	if err := s.BeginContractInputMutationWithReceiptsContext(ctx, nil, state, []graph.ContractWork{work}, []graph.ContractBoundaryReceipt{receipt}); err != nil {
		t.Fatal(err)
	}
	input, found, err := s.ContractInputStateContext(ctx, "repo", "")
	if err != nil || !found || input.Accepted {
		t.Fatalf("partial input=%#v %v %v", input, found, err)
	}
	bad := receipt
	bad.SourceFingerprint = "other-source"
	if err := s.AcceptContractInputMutationWithReceiptsContext(ctx, state, []graph.ContractBoundaryReceipt{bad}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("wrong source accepted=%v", err)
	}
	row, _, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", receipt.FilePath)
	if err != nil || row.Accepted {
		t.Fatalf("partial receipt=%#v %v", row, err)
	}
	if err := s.AcceptContractInputMutationWithReceiptsContext(ctx, state, []graph.ContractBoundaryReceipt{receipt}); err != nil {
		t.Fatal(err)
	}
	input, _, err = s.ContractInputStateContext(ctx, "repo", "")
	if err != nil || !input.Accepted {
		t.Fatalf("input not accepted=%#v %v", input, err)
	}
	row, _, err = s.ContractBoundaryReceiptContext(ctx, "repo", "", receipt.FilePath)
	if err != nil || !row.Accepted {
		t.Fatalf("receipt not accepted=%#v %v", row, err)
	}
	other := receipt
	other.FilePath = "repo/second.go"
	other.Fingerprint = "other"
	other.SourceFingerprint = "source-other"
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{other}); err != nil {
		t.Fatal(err)
	}
	matches, err := s.ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{"*::type::Payload"}, 1)
	if !errors.Is(err, ErrContractBoundaryReceiptLimit) || matches != nil {
		t.Fatalf("partial limit=%#v %v", matches, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	matches, err = s.ContractBoundaryReceiptsForLookupKeysContext(canceled, []string{"*::type::Payload"}, 10)
	if !errors.Is(err, context.Canceled) || matches != nil {
		t.Fatalf("partial cancel=%#v %v", matches, err)
	}
	huge := receipt
	huge.FilePath = "repo/huge.go"
	huge.Scope.Causes = []string{strings.Repeat("x", 4*contractBoundaryPayloadLimit)}
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{huge}); !errors.Is(err, ErrContractBoundaryReceiptLimit) {
		t.Fatalf("encoded scope unbounded=%v", err)
	}
	row, known, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", huge.FilePath)
	if err != nil || row != nil || known {
		t.Fatalf("failed huge receipt partial=%#v %v %v", row, known, err)
	}
}

func TestContractBoundaryReceiptPendingRestartFoldAndBulk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "boundary.sqlite")
	s, err := openPristine(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Actual cold bulk owner pins its writer connection. Sidecar mutations reuse
	// that connection and do not attempt a second max-one writer checkout.
	s.BeginBulkLoad()
	if s.bulkConn == nil {
		t.Fatal("cold bulk writer was not actually pinned")
	}
	old := boundaryStorageReceipt("old.go", "old")
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}); err != nil {
		t.Fatal(err)
	}
	replacement := old
	replacement.Fingerprint = "deleted"
	replacement.SourceFingerprint = "absent"
	replacement.Deleted = true
	replacement.Payload = nil
	replacement.LookupKeys = nil
	replacement.ProducedKeys = nil
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{replacement}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushBulk(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openPristine(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	row, _, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", old.FilePath)
	if err != nil || row.Accepted || row.Previous == nil || !bytes.Equal(row.Previous.Payload, old.Payload) {
		t.Fatalf("restart old receipt lost=%#v %v", row, err)
	}
	matches, err := s.ContractBoundaryReceiptsForLookupKeysContext(ctx, old.LookupKeys, 10)
	if err != nil || len(matches) != 1 || matches[0].Accepted {
		t.Fatalf("restart pending absence uncertified=%#v %v", matches, err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{replacement}); err != nil {
		t.Fatal(err)
	}
	bottom, upper, folded := reservedGeneration(t, s, "receipt-bottom"), reservedGeneration(t, s, "receipt-upper"), reservedGeneration(t, s, "receipt-folded")
	acceptedOld := old
	acceptedOld.Accepted = true
	if err := s.AtGeneration(bottom).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{acceptedOld}); err != nil {
		t.Fatal(err)
	}
	empty := replacement
	empty.Accepted = true
	if err := s.AtGeneration(upper).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{empty}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FlattenGenerationChain(ctx, []int64{bottom, upper}, folded); err != nil {
		t.Fatal(err)
	}
	matches, err = s.AtGeneration(folded).ContractBoundaryReceiptsForLookupKeysContext(ctx, old.LookupKeys, 10)
	if err != nil || len(matches) != 0 {
		t.Fatalf("fold revived lower keys=%#v %v", matches, err)
	}
	row, _, err = s.AtGeneration(folded).ContractBoundaryReceiptContext(ctx, "repo", "", old.FilePath)
	if err != nil || !row.Deleted {
		t.Fatalf("fold deleted tombstone lost=%#v %v", row, err)
	}
	// A staged replacement keeps the complete selected predecessor membership
	// through fold. The runtime seeds the sparse target from its selected receipt.
	pending := reservedGeneration(t, s, "receipt-pending")
	if _, err := s.CopyGenerationPayloadWhole(ctx, bottom, pending); err != nil {
		t.Fatal(err)
	}
	next := old
	next.Fingerprint = "pending"
	next.SourceFingerprint = "source-pending"
	next.LookupKeys = []string{"*::type::New"}
	next.ProducedKeys = []string{"repo::symbol::new"}
	if err := s.AtGeneration(pending).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{next}); err != nil {
		t.Fatal(err)
	}
	pendingFold := reservedGeneration(t, s, "receipt-pending-fold")
	if _, err := s.FlattenGenerationChain(ctx, []int64{bottom, pending}, pendingFold); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{old.LookupKeys[0], "*::type::New"} {
		matches, err = s.AtGeneration(pendingFold).ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{key}, 10)
		if err != nil || len(matches) != 1 || matches[0].Accepted || matches[0].Previous == nil {
			t.Fatalf("fold dropped pending union %s=%#v %v", key, matches, err)
		}
	}
	copied := reservedGeneration(t, s, "receipt-copy")
	if _, err := s.CopyGenerationPayloadWhole(ctx, folded, copied); err != nil {
		t.Fatal(err)
	}
	row, _, err = s.AtGeneration(copied).ContractBoundaryReceiptContext(ctx, "repo", "", old.FilePath)
	if err != nil || !row.Deleted {
		t.Fatalf("copy receipt lost=%#v %v", row, err)
	}
}

func TestContractBoundaryReceiptCheckedSparseCarry(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	old := boundaryStorageReceipt("carry.go", "accepted")
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}); err != nil {
		t.Fatal(err)
	}
	captured, _, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", old.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	target := reservedGeneration(t, s, "receipt-carry-target")
	next := old
	next.CheckoutID = "linked-b"
	next.Fingerprint = "pending-new"
	next.SourceFingerprint = "source-new"
	next.LookupKeys = []string{"*::type::New"}
	source := graph.ContractBoundaryReceiptSource{GenerationID: 0, Receipt: *captured, TargetCheckoutID: "linked-b"}
	forged := source
	forged.Receipt.Fingerprint = "forged"
	if err := s.AtGeneration(target).SetContractBoundaryReceiptsWithSourcesContext(ctx, []graph.ContractBoundaryReceipt{next}, []graph.ContractBoundaryReceiptSource{forged}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("forged carry accepted=%v", err)
	}
	row, known, err := s.AtGeneration(target).ContractBoundaryReceiptContext(ctx, "repo", "linked-b", old.FilePath)
	if err != nil || known || row != nil {
		t.Fatalf("failed carry partial=%#v %v %v", row, known, err)
	}
	if err := s.AtGeneration(target).SetContractBoundaryReceiptsWithSourcesContext(ctx, []graph.ContractBoundaryReceipt{next}, []graph.ContractBoundaryReceiptSource{source}); err != nil {
		t.Fatal(err)
	}
	row, _, err = s.AtGeneration(target).ContractBoundaryReceiptContext(ctx, "repo", "linked-b", old.FilePath)
	if err != nil || row.Accepted || row.Previous == nil || row.Previous.CheckoutID != "linked-b" || !row.Previous.Carried || row.Previous.CarriedFromCheckoutID != "" || row.Previous.CarriedFromGeneration != 0 || row.Previous.Fingerprint != old.Fingerprint {
		t.Fatalf("carry provenance=%#v %v", row, err)
	}
	matches, err := s.AtGeneration(target).ContractBoundaryReceiptsForLookupKeysContext(ctx, old.LookupKeys, 10)
	if err != nil || len(matches) != 1 || matches[0].CheckoutID != "linked-b" || matches[0].Accepted {
		t.Fatalf("old membership lost=%#v %v", matches, err)
	}
	if err := s.AtGeneration(target).SetContractBoundaryReceiptsWithSourcesContext(ctx, nil, []graph.ContractBoundaryReceiptSource{source}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("nonempty carry target overwritten=%v", err)
	}
	// Source zero's row changed since capture: stale capture cannot seed another view.
	changed := old
	changed.Fingerprint = "changed"
	changed.SourceFingerprint = "source-changed"
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{changed}); err != nil {
		t.Fatal(err)
	}
	second := reservedGeneration(t, s, "receipt-carry-stale")
	if err := s.AtGeneration(second).SetContractBoundaryReceiptsWithSourcesContext(ctx, []graph.ContractBoundaryReceipt{next}, []graph.ContractBoundaryReceiptSource{source}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale source0 carry=%v", err)
	}
}
