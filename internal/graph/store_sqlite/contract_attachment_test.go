package store_sqlite

import (
	"context"
	"errors"
	"github.com/zzet/gortex/internal/graph"
	"path/filepath"
	"reflect"
	"testing"
)

func attachmentState(fingerprint string) graph.ContractInputState {
	return graph.ContractInputState{RepoPrefix: "repo", CheckoutID: "wt", InputVersion: "boundary-v1", InputFingerprint: fingerprint, Accepted: true}
}
func attachmentPayload(t *testing.T, s *Store) int64 {
	t.Helper()
	id := reservedGeneration(t, s, "contract-attachment")
	if err := s.AtGeneration(id).SetProducerState(ProducerCompleteness{Producer: "graph.contracts", State: ProducerStateComplete}); err != nil {
		t.Fatal(err)
	}
	return id
}
func attachmentFor(state graph.ContractInputState, payload int64, work ...graph.ContractWork) graph.ContractAttachment {
	a := graph.ContractAttachment{RepoPrefix: state.RepoPrefix, CheckoutID: state.CheckoutID, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint, PayloadGeneration: payload}
	for _, w := range work {
		a.CompletedTokens = append(a.CompletedTokens, w.Token)
	}
	return a
}
func attachmentKey(state graph.ContractInputState) graph.ContractAttachmentKey {
	return graph.ContractAttachmentKey{RepoPrefix: state.RepoPrefix, CheckoutID: state.CheckoutID, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint}
}

func TestContractInputPrimaryPendingCASAndCancellation(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	old := attachmentState("old")
	old.CheckoutID = ""
	w := contractWorkFixture("removed-owner")
	w.CheckoutID = ""
	if err := s.BeginContractInputMutationContext(ctx, nil, old, []graph.ContractWork{w}); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.ContractInputStateContext(ctx, "repo", "")
	if err != nil || !found || got.Accepted {
		t.Fatalf("pending=%#v %v %v", got, found, err)
	}
	if err := s.PublishContractAttachmentContext(ctx, old, attachmentFor(old, attachmentPayload(t, s), w), []graph.ContractWork{w}, 1); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("pending publication=%v", err)
	}
	if err := s.AcceptContractInputMutationContext(ctx, old); err != nil {
		t.Fatal(err)
	}
	got, found, err = s.ContractInputStateContext(ctx, "repo", "")
	if err != nil || !found || !got.Accepted {
		t.Fatalf("accepted=%#v %v %v", got, found, err)
	}
	next := attachmentState("new")
	next.CheckoutID = ""
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.BeginContractInputMutationContext(canceled, &got, next, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
	if err := s.BeginContractInputMutationContext(ctx, &next, next, nil); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale begin=%v", err)
	}
	still, _, _ := s.ContractInputStateContext(ctx, "repo", "")
	if still != got {
		t.Fatal("failed begin modified input")
	}
}

func TestContractAttachmentAtomicExactWorkAndHistoricalIsolation(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	core := reservedGeneration(t, s, "core-input")
	h := s.AtGeneration(core)
	old := attachmentState("old")
	w := contractWorkFixture("deleted-shared-owner")
	w.OriginGeneration = core
	if err := h.SetContractInputStateWithWorkContext(ctx, nil, old, []graph.ContractWork{w}); err != nil {
		t.Fatal(err)
	}
	payload := attachmentPayload(t, s)
	a := attachmentFor(old, payload, w)
	wrong := w
	wrong.Scope.Deleted = false
	if err := h.PublishContractAttachmentContext(ctx, old, a, []graph.ContractWork{wrong}, 1); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("wrong captured scope=%v", err)
	}
	if got, err := h.GetContractAttachmentContext(ctx, attachmentKey(old)); err != nil || got != nil {
		t.Fatalf("partial attachment=%#v %v", got, err)
	}
	rows, err := h.ContractWorkContext(ctx)
	if err != nil || len(rows) != 1 || rows[0].State != graph.ContractWorkPending {
		t.Fatalf("partial ack=%#v %v", rows, err)
	}
	if err := h.PublishContractAttachmentContext(ctx, old, a, []graph.ContractWork{w}, 1); err != nil {
		t.Fatal(err)
	}
	got, err := h.GetContractAttachmentContext(ctx, attachmentKey(old))
	if err != nil || !reflect.DeepEqual(got, &a) {
		t.Fatalf("attachment=%#v %v", got, err)
	}
	wrongKey := attachmentKey(old)
	wrongKey.CheckoutID = "sibling"
	if got, err := h.GetContractAttachmentContext(ctx, wrongKey); err != nil || got != nil {
		t.Fatalf("sibling leaked=%#v %v", got, err)
	}
	wrongKey = attachmentKey(old)
	wrongKey.InputFingerprint = "new"
	if got, err := h.GetContractAttachmentContext(ctx, wrongKey); err != nil || got != nil {
		t.Fatalf("latest fallback=%#v %v", got, err)
	}
	rows, err = h.PendingContractWorkForScopeContext(ctx, "repo", "wt")
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending ack=%#v %v", rows, err)
	}
	all, err := h.ContractWorkContext(ctx)
	if err != nil || all[0].State != graph.ContractWorkComplete || !all[0].Scope.Deleted {
		t.Fatalf("ack lost old frontier=%#v %v", all, err)
	}
	refs, err := s.Catalog().ViewGenerationReferences(ctx, payload)
	if err != nil || !refs.ContractAttached {
		t.Fatalf("payload refs=%#v %v", refs, err)
	}
	if err := s.Catalog().DeleteViewGeneration(ctx, payload); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("referenced delete=%v", err)
	}
}

func TestContractInputFoldAndPreviousSnapshot(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	bottom := reservedGeneration(t, s, "input-bottom")
	upper := reservedGeneration(t, s, "input-upper")
	folded := reservedGeneration(t, s, "input-folded")
	old := attachmentState("old")
	w := contractWorkFixture("old-deleted")
	w.OriginGeneration = bottom
	if err := s.AtGeneration(bottom).SetContractInputStateWithWorkContext(ctx, nil, old, []graph.ContractWork{w}); err != nil {
		t.Fatal(err)
	}
	if err := s.AtGeneration(bottom).PublishContractAttachmentContext(ctx, old, attachmentFor(old, attachmentPayload(t, s), w), []graph.ContractWork{w}, 1); err != nil {
		t.Fatal(err)
	}
	// Explicit carry is permitted only from real catalog ancestry. This fixture
	// uses a physical accepted row to exercise fold precedence independently.
	if err := s.AtGeneration(upper).SetContractInputStateWithWorkContext(ctx, nil, old, nil); err != nil {
		t.Fatal(err)
	}
	next := attachmentState("new")
	if err := s.AtGeneration(upper).SetContractInputStateWithWorkContext(ctx, &old, next, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FlattenGenerationChain(ctx, []int64{bottom, upper}, folded); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.AtGeneration(folded).ContractInputStateContext(ctx, "repo", "wt")
	if err != nil || !found || got.InputFingerprint != "new" || got.PreviousInputFingerprint != "old" {
		t.Fatalf("fold state=%#v %v %v", got, found, err)
	}
	debt, err := s.AtGeneration(folded).ContractWorkContext(ctx)
	if err != nil || len(debt) != 0 {
		t.Fatalf("fold retained completed history=%#v %v", debt, err)
	}
	copy := reservedGeneration(t, s, "input-copy")
	if _, err := s.CopyGenerationPayloadWhole(ctx, folded, copy); err != nil {
		t.Fatal(err)
	}
	copied, _, err := s.AtGeneration(copy).ContractInputStateContext(ctx, "repo", "wt")
	if err != nil || copied != got {
		t.Fatalf("copied=%#v %v", copied, err)
	}
}

func TestContractInputRestartPendingPreservesRemovedFrontierAndPrevious(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "contract-restart.sqlite")
	s, err := openPristine(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	state := attachmentState("first")
	state.CheckoutID = ""
	w := contractWorkFixture("removed-first")
	w.CheckoutID = ""
	if err := s.BeginContractInputMutationContext(ctx, nil, state, []graph.ContractWork{w}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractInputMutationContext(ctx, state); err != nil {
		t.Fatal(err)
	}
	payload := attachmentPayload(t, s)
	a := attachmentFor(state, payload, w)
	if err := s.PublishContractAttachmentContext(ctx, state, a, []graph.ContractWork{w}, 1); err != nil {
		t.Fatal(err)
	}
	next := attachmentState("pending-second")
	next.CheckoutID = ""
	debt := w
	debt.Token = "renamed-second"
	debt.Scope.Causes = []string{"renamed_owner"}
	if err := s.BeginContractInputMutationContext(ctx, &state, next, []graph.ContractWork{debt}); err != nil {
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
	pending, found, err := s.ContractInputStateContext(ctx, "repo", "")
	if err != nil || !found || pending.Accepted || pending.PreviousInputFingerprint != "first" {
		t.Fatalf("restarted pending=%#v %v %v", pending, found, err)
	}
	got, err := s.GetContractAttachmentContext(ctx, attachmentKey(state))
	if err != nil || got == nil || got.PayloadGeneration != payload {
		t.Fatalf("previous unavailable=%#v %v", got, err)
	}
	captured, err := s.PendingContractWorkForScopeContext(ctx, "repo", "")
	if err != nil || len(captured) != 1 || !captured[0].Scope.Deleted {
		t.Fatalf("restart deletion frontier=%#v %v", captured, err)
	}
	replacement := next
	replacement.InputFingerprint = "pending-third"
	newDebt := debt
	newDebt.Token = "removed-third"
	if err := s.BeginContractInputMutationContext(ctx, &pending, replacement, []graph.ContractWork{newDebt}); err != nil {
		t.Fatal(err)
	}
	captured, err = s.PendingContractWorkForScopeContext(ctx, "repo", "")
	if err != nil || len(captured) != 2 {
		t.Fatalf("superseded debt lost=%#v %v", captured, err)
	}
	if err := s.AcceptContractInputMutationContext(ctx, next); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old acceptance=%v", err)
	}
}

func TestContractAttachmentEmptyNamespaceCancellationAndSeal(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	core := reservedGeneration(t, s, "empty-input")
	h := s.AtGeneration(core)
	state := attachmentState("empty")
	state.RepoPrefix = ""
	state.CheckoutID = ""
	if err := h.SetContractInputStateWithWorkContext(ctx, nil, state, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.ContractInputStateContext(ctx, "repo", ""); err != nil || found {
		t.Fatalf("empty namespace broadened %v %v", found, err)
	}
	payload := attachmentPayload(t, s)
	a := attachmentFor(state, payload)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.PublishContractAttachmentContext(canceled, state, a, nil, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel publish=%v", err)
	}
	if got, err := h.GetContractAttachmentContext(ctx, attachmentKey(state)); err != nil || got != nil {
		t.Fatalf("cancel wrote header=%#v %v", got, err)
	}
	if err := h.PublishContractAttachmentContext(ctx, state, a, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AtManagedGenerationMustForContractTest(t, payload).SetContractWork(ctx, []graph.ContractWork{contractWorkFixture("sealed-write")}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("sealed payload write=%v", err)
	}
}
func (s *Store) AtManagedGenerationMustForContractTest(t *testing.T, id int64) *Store {
	t.Helper()
	h, err := s.AtManagedGeneration(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestContractAttachmentNonemptyOwnerShapeAndMaskRefusal(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	core := reservedGeneration(t, s, "nonempty-core")
	h := s.AtGeneration(core)
	state := attachmentState("shaped")
	if err := h.SetContractInputStateWithWorkContext(ctx, nil, state, nil); err != nil {
		t.Fatal(err)
	}
	payload := attachmentPayload(t, s)
	layer, err := s.AtManagedGeneration(payload)
	if err != nil {
		t.Fatal(err)
	}
	source := &graph.Node{ID: "repo/provider.go::serve", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/provider.go", Name: "serve"}
	shape := &graph.Node{ID: "repo/shared.go::Reply", Kind: graph.KindType, RepoPrefix: "repo", FilePath: "repo/shared.go", Meta: map[string]any{"shape": map[string]any{"fields": []any{map[string]any{"name": "new_wire", "type": "string"}}}}}
	canonical := &graph.Node{ID: "http::GET::/new", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: source.FilePath, Meta: map[string]any{"type": "http", "role": "provider", "symbol_id": source.ID, "contract_owner_record": true, "contract_meta": map[string]any{"method": "GET", "path": "/new", "response_type": shape.ID}}}
	owner := &graph.Edge{From: source.ID, To: canonical.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath, Line: 7, Meta: map[string]any{"contract_owner_repo_prefix": "repo", "contract_owner_role": "provider", "contract_owner_symbol_id": source.ID, "contract_owner_meta": map[string]any{"method": "GET", "path": "/new", "response_type": shape.ID}}}
	if err := layer.AddBatchChecked([]*graph.Node{source, canonical, shape}, []*graph.Edge{owner}); err != nil {
		t.Fatal(err)
	}
	a := attachmentFor(state, payload)
	if err := h.PublishContractAttachmentContext(ctx, state, a, nil, 1); err != nil {
		t.Fatal(err)
	}
	projection, err := layer.LoadContractRepoProjectionContext(ctx, "repo")
	if err != nil || len(projection.OwnerRows) != 1 || projection.Targets[canonical.ID] == nil {
		t.Fatalf("owner projection=%#v %v", projection, err)
	}
	if projection.OwnerRows[0].Edge.Meta["contract_owner_meta"].(map[string]any)["response_type"] != shape.ID {
		t.Fatal("owner payload lost type")
	}
	typed, err := layer.LayerContractIDProjectionContext(ctx, []string{shape.ID})
	if err != nil || typed.SourceNodes[shape.ID] == nil {
		t.Fatalf("shape projection=%#v %v", typed, err)
	}
	if !reflect.DeepEqual(typed.SourceNodes[shape.ID].Meta, shape.Meta) {
		t.Fatalf("shape bytes changed=%#v", typed.SourceNodes[shape.ID].Meta)
	}
	if s.GetNode(canonical.ID) != nil || s.GetNode(shape.ID) != nil {
		t.Fatal("isolated payload leaked into ordinary primary")
	}
	masked := attachmentPayload(t, s)
	if err := s.AtGeneration(masked).SetFileMasks([]FileMask{{RepoPrefix: "repo", FilePath: source.FilePath, Mode: OwnershipReplace}}); err != nil {
		t.Fatal(err)
	}
	changed := state
	changed.InputFingerprint = "mask-input"
	other := reservedGeneration(t, s, "mask-core")
	if err := s.AtGeneration(other).SetContractInputStateWithWorkContext(ctx, nil, changed, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AtGeneration(other).PublishContractAttachmentContext(ctx, changed, attachmentFor(changed, masked), nil, 1); err == nil {
		t.Fatal("core file masks accepted")
	}
}

func TestContractAttachmentSelectedVectorCASAbsenceAndUnrelatedPrimary(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	primary := attachmentState("primary-a")
	primary.CheckoutID = ""
	if err := s.BeginContractInputMutationContext(ctx, nil, primary, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractInputMutationContext(ctx, primary); err != nil {
		t.Fatal(err)
	}
	core := reservedGeneration(t, s, "selected-vector")
	linked := attachmentState("linked-b")
	if err := s.AtGeneration(core).SetContractInputStateWithWorkContext(ctx, nil, linked, nil); err != nil {
		t.Fatal(err)
	}
	zero, _, err := s.ContractInputStateContext(ctx, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	physical, _, err := s.AtGeneration(core).ContractInputStateContext(ctx, "repo", "wt")
	if err != nil {
		t.Fatal(err)
	}
	witnesses := []graph.ContractInputWitness{{GenerationID: 0, State: zero, Found: true}, {GenerationID: core, State: physical, Found: true}, {GenerationID: core, State: graph.ContractInputState{RepoPrefix: "unresolved", CheckoutID: "wt"}, Found: false}}
	logical, err := graph.ComposeContractInputState("repo", "wt", witnesses)
	if err != nil {
		t.Fatal(err)
	}
	missing := attachmentState("new-negative-input")
	missing.RepoPrefix = "unresolved"
	if err := s.AtGeneration(core).SetContractInputStateWithWorkContext(ctx, nil, missing, nil); err != nil {
		t.Fatal(err)
	}
	payload := attachmentPayload(t, s)
	if err := s.AtGeneration(core).PublishContractAttachmentWithInputsContext(ctx, logical, witnesses, attachmentFor(logical, payload), nil, 1); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("missing witness changed=%v", err)
	}
	witnesses = witnesses[:2]
	if err := s.AtGeneration(core).PublishContractAttachmentWithInputsContext(ctx, logical, witnesses, attachmentFor(logical, payload), nil, 1); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Catalog().ViewGenerationReferences(ctx, payload)
	if err != nil || !refs.ContractAttached {
		t.Fatalf("compound ref=%#v %v", refs, err)
	}
	next := primary
	next.InputFingerprint = "primary-new"
	if err := s.BeginContractInputMutationContext(ctx, &zero, next, nil); err != nil {
		t.Fatal(err)
	}
	stalePayload := attachmentPayload(t, s)
	if err := s.AtGeneration(core).PublishContractAttachmentWithInputsContext(ctx, logical, witnesses, attachmentFor(logical, stalePayload), nil, 1); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("changed inherited0=%v", err)
	}
	// A full root contains no physical0 witness, so the same primary change
	// does not tax or invalidate its independently accepted input.
	own := []graph.ContractInputWitness{{GenerationID: core, State: physical, Found: true}}
	ownLogical, err := graph.ComposeContractInputState("repo", "wt", own)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AtGeneration(core).PublishContractAttachmentWithInputsContext(ctx, ownLogical, own, attachmentFor(ownLogical, stalePayload), nil, 1); err != nil {
		t.Fatal(err)
	}
	copied := reservedGeneration(t, s, "selected-vector-copy")
	if _, err := s.CopyGenerationPayloadWhole(ctx, core, copied); err != nil {
		t.Fatal(err)
	}
	if err := s.Catalog().DeleteViewGeneration(ctx, core); err != nil {
		t.Fatal(err)
	}
	refs, err = s.Catalog().ViewGenerationReferences(ctx, stalePayload)
	if err != nil || !refs.ContractAttached {
		t.Fatalf("copy lost input reference=%#v %v", refs, err)
	}
}

func TestContractAttachmentCumulativePositiveFoldPreservesInheritedIdentity(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	base := attachmentState("live0")
	base.CheckoutID = ""
	if err := s.BeginContractInputMutationContext(ctx, nil, base, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractInputMutationContext(ctx, base); err != nil {
		t.Fatal(err)
	}
	lower := reservedGeneration(t, s, "cumulative-lower")
	upper := reservedGeneration(t, s, "cumulative-upper")
	folded := reservedGeneration(t, s, "cumulative-fold")
	a, b := attachmentState("positive-a"), attachmentState("positive-b-includes-a")
	if err := s.AtGeneration(lower).SetContractInputStateWithWorkContext(ctx, nil, a, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AtGeneration(upper).SetContractInputStateWithWorkContext(ctx, nil, b, nil); err != nil {
		t.Fatal(err)
	}
	zero, _, _ := s.ContractInputStateContext(ctx, "repo", "")
	lo, _, _ := s.AtGeneration(lower).ContractInputStateContext(ctx, "repo", "wt")
	hi, _, _ := s.AtGeneration(upper).ContractInputStateContext(ctx, "repo", "wt")
	before := []graph.ContractInputWitness{{GenerationID: 0, State: zero, Found: true}, {GenerationID: lower, State: lo, Found: true}, {GenerationID: upper, State: hi, Found: true}}
	key, err := graph.ComposeContractInputState("repo", "wt", before)
	if err != nil {
		t.Fatal(err)
	}
	payload := attachmentPayload(t, s)
	if err := s.AtGeneration(upper).PublishContractAttachmentWithInputsContext(ctx, key, before, attachmentFor(key, payload), nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FlattenGenerationChain(ctx, []int64{lower, upper}, folded); err != nil {
		t.Fatal(err)
	}
	current, found, err := s.AtGeneration(folded).ContractInputStateContext(ctx, "repo", "wt")
	if err != nil || !found {
		t.Fatalf("fold input=%#v %v %v", current, found, err)
	}
	after, err := graph.ComposeContractInputState("repo", "wt", []graph.ContractInputWitness{{GenerationID: 0, State: zero, Found: true}, {GenerationID: folded, State: current, Found: true}})
	if err != nil || after != key {
		t.Fatalf("fold identity changed %#v -> %#v %v", key, after, err)
	}
	// Retiring old physical witnesses cannot unpin the logical payload when
	// cumulative top authority survives in the folded positive generation.
	if _, err := s.writerDB.Exec(`DELETE FROM generation_contract_input_state WHERE view_gen IN (?,?)`, lower, upper); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Catalog().ViewGenerationReferences(ctx, payload)
	if err != nil || !refs.ContractAttached {
		t.Fatalf("fold logical ref=%#v %v", refs, err)
	}
	got, err := s.GetContractAttachmentContext(ctx, attachmentKey(after))
	if err != nil || got == nil || got.PayloadGeneration != payload {
		t.Fatalf("historical binding lost %#v %v", got, err)
	}
}
