package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Each batch destination is a distinct logical reservation; BeginPayloadGeneration
// intentionally reuses an existing building reservation for the same layer.
var attachmentBatchReservationSeq atomic.Uint64

func attachmentBatchGeneration(t *testing.T, s *Store, kind string) int64 {
	t.Helper()
	return reservedGeneration(t, s, fmt.Sprintf("batch-%s-%s-%d", kind, t.Name(), attachmentBatchReservationSeq.Add(1)))
}

func attachmentBatchPayload(t *testing.T, s *Store) int64 {
	t.Helper()
	id := attachmentBatchGeneration(t, s, "payload")
	if err := s.AtGeneration(id).SetProducerState(ProducerCompleteness{Producer: "graph.contracts", State: ProducerStateComplete}); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertAttachmentBatchDistinct(t *testing.T, targets []ContractAttachmentPublishTarget) {
	t.Helper()
	cores, payloads := map[int64]bool{}, map[int64]bool{}
	for _, target := range targets {
		core, payload := target.Store.viewGen, target.Attachment.PayloadGeneration
		if cores[core] || payloads[payload] || payload <= 0 || core == payload {
			t.Fatalf("fixture aliases core/payload reservations: core=%d payload=%d", core, payload)
		}
		cores[core], payloads[payload] = true, true
	}
}

func attachmentBatchTarget(t *testing.T, s *Store, repo string) ContractAttachmentPublishTarget {
	t.Helper()
	ctx := context.Background()
	core := attachmentBatchGeneration(t, s, "core")
	physical := attachmentState(repo)
	physical.RepoPrefix = repo
	h := s.AtGeneration(core)
	work := contractWorkFixture("batch-" + repo)
	work.RepoPrefix = repo
	work.CheckoutID = physical.CheckoutID
	work.OriginGeneration = core
	if err := h.SetContractInputStateWithWorkContext(ctx, nil, physical, []graph.ContractWork{work}); err != nil {
		t.Fatal(err)
	}
	publishContractCoreForTest(t, s, core)
	witnesses := []graph.ContractInputWitness{{GenerationID: core, State: physical, Found: true}}
	logical, err := graph.ComposeContractInputState(repo, physical.CheckoutID, witnesses)
	if err != nil {
		t.Fatal(err)
	}
	return ContractAttachmentPublishTarget{Store: h, Expected: logical, Witnesses: witnesses, Attachment: attachmentFor(logical, attachmentBatchPayload(t, s), work), Work: []graph.ContractWork{work}}
}

func assertAttachmentBatchUnpublished(t *testing.T, s *Store, targets []ContractAttachmentPublishTarget) {
	t.Helper()
	for _, target := range targets {
		got, err := s.GetContractAttachmentContext(context.Background(), attachmentKey(target.Expected))
		if err != nil || got != nil {
			t.Fatalf("partial publication: %#v %v", got, err)
		}
		var state string
		if err := s.db.QueryRow(`SELECT state FROM view_generations WHERE generation_id=?`, target.Attachment.PayloadGeneration).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != string(ViewGenerationBuilding) {
			t.Fatalf("partial seal: %s", state)
		}
	}
}

func TestContractAttachmentBatchRollsBackLaterRefusal(t *testing.T) {
	for _, refusal := range []string{"stale-witness", "bad-ack", "different-store", "missing-witness", "incomplete-payload"} {
		t.Run(refusal, func(t *testing.T) {
			s := openCatalogStore(t)
			targets := []ContractAttachmentPublishTarget{attachmentBatchTarget(t, s, "one"), attachmentBatchTarget(t, s, "two")}
			assertAttachmentBatchDistinct(t, targets)
			switch refusal {
			case "stale-witness":
				if _, err := s.writerDB.Exec(`UPDATE generation_contract_input_state SET input_fingerprint='moved' WHERE view_gen=?`, targets[1].Store.viewGen); err != nil {
					t.Fatal(err)
				}
			case "bad-ack":
				targets[1].Attachment.CompletedTokens = []string{"uncaptured"}
			case "different-store":
				targets[1].Store = openCatalogStore(t)
			case "missing-witness":
				targets[1].Witnesses = nil
			case "incomplete-payload":
				if _, err := s.writerDB.Exec(`DELETE FROM generation_producer_completeness WHERE view_gen=?`, targets[1].Attachment.PayloadGeneration); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.PublishContractAttachmentsWithInputsContext(context.Background(), targets, 1)
			wantErr := error(ErrCatalogStaleGuard)
			switch refusal {
			case "bad-ack", "different-store":
				wantErr = ErrCatalogInvalidValue
			case "missing-witness":
				wantErr = graph.ErrContractInputVector
			case "incomplete-payload":
				wantErr = sql.ErrNoRows
			}
			if !errors.Is(err, wantErr) || result != nil || (err != nil && strings.Contains(err.Error(), "shared attachment reservation")) {
				t.Fatalf("%s refusal result=%#v error=%v, want %v", refusal, result, err, wantErr)
			}
			if refusal == "bad-ack" && !strings.Contains(err.Error(), "acknowledgment batch") {
				t.Fatalf("wrong acknowledgment refusal: %v", err)
			}
			if refusal == "different-store" && !strings.Contains(err.Error(), "attachment stores differ") {
				t.Fatalf("wrong store refusal: %v", err)
			}
			assertAttachmentBatchUnpublished(t, s, targets)
		})
	}
}

func TestContractAttachmentBatchPerTargetAncestryAndDuplicateBinding(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	targets := []ContractAttachmentPublishTarget{attachmentBatchTarget(t, s, "one"), attachmentBatchTarget(t, s, "two")}
	assertAttachmentBatchDistinct(t, targets)
	// Neither selected core contains the other target's witness.
	swapped := append([]ContractAttachmentPublishTarget(nil), targets...)
	swapped[1].Store = targets[0].Store
	if _, err := s.PublishContractAttachmentsWithInputsContext(ctx, swapped, 1); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("wrong ancestry: %v", err)
	}
	assertAttachmentBatchUnpublished(t, s, targets)
	results, err := s.PublishContractAttachmentsWithInputsContext(ctx, targets, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i, target := range targets {
		if results[i].PayloadGeneration != target.Attachment.PayloadGeneration || results[i].UnusedPayloadGeneration != 0 {
			t.Fatalf("result %#v", results[i])
		}
	}
	retry := targets[0]
	retry.Attachment = attachmentFor(retry.Expected, attachmentBatchPayload(t, s), retry.Work...)
	results, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{retry}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].PayloadGeneration != targets[0].Attachment.PayloadGeneration || results[0].UnusedPayloadGeneration != retry.Attachment.PayloadGeneration {
		t.Fatalf("duplicate result %#v", results)
	}
	got, err := s.GetContractAttachmentContext(ctx, attachmentKey(retry.Expected))
	if err != nil || got == nil || got.PayloadGeneration != targets[0].Attachment.PayloadGeneration {
		t.Fatalf("replacement %#v %v", got, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM view_generations WHERE generation_id=?`, retry.Attachment.PayloadGeneration).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(ViewGenerationBuilding) {
		t.Fatalf("losing reservation sealed: %s", state)
	}
	for _, badState := range []string{string(ViewGenerationBuilding), string(ViewGenerationRetiring)} {
		if _, err := s.writerDB.Exec(`UPDATE view_generations SET state=? WHERE generation_id=?`, badState, targets[0].Attachment.PayloadGeneration); err != nil {
			t.Fatal(err)
		}
		if result, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{retry}, 4); err == nil || result != nil {
			t.Fatalf("bad existing payload %s: %#v %v", badState, result, err)
		}
	}
}

func TestContractAttachmentBatchValidatesSharedPrimaryDebtBeforeWrites(t *testing.T) {
	for _, primaryFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "primary-first", false: "linked-first"}[primaryFirst], func(t *testing.T) {
			s := openCatalogStore(t)
			ctx := context.Background()
			primary := attachmentState("primary-shared")
			primary.CheckoutID = ""
			work := contractWorkFixture("shared-primary-debt")
			work.CheckoutID = ""
			if err := s.BeginContractInputMutationContext(ctx, nil, primary, []graph.ContractWork{work}); err != nil {
				t.Fatal(err)
			}
			if err := s.AcceptContractInputMutationContext(ctx, primary); err != nil {
				t.Fatal(err)
			}
			primaryWitness := graph.ContractInputWitness{GenerationID: 0, State: primary, Found: true}
			core := attachmentBatchGeneration(t, s, "sparse-linked")
			publishContractCoreForTest(t, s, core)
			linkedWitnesses := []graph.ContractInputWitness{primaryWitness, {GenerationID: core, State: graph.ContractInputState{RepoPrefix: "repo", CheckoutID: "wt"}, Found: false}}
			logical, err := graph.ComposeContractInputState("repo", "wt", linkedWitnesses)
			if err != nil {
				t.Fatal(err)
			}
			a := ContractAttachmentPublishTarget{Store: s, Expected: primary, Witnesses: []graph.ContractInputWitness{primaryWitness}, Attachment: attachmentFor(primary, attachmentBatchPayload(t, s), work), Work: []graph.ContractWork{work}}
			b := ContractAttachmentPublishTarget{Store: s.AtGeneration(core), Expected: logical, Witnesses: linkedWitnesses, Attachment: attachmentFor(logical, attachmentBatchPayload(t, s), work), Work: []graph.ContractWork{work}}
			targets := []ContractAttachmentPublishTarget{a, b}
			assertAttachmentBatchDistinct(t, targets)
			if !primaryFirst {
				targets = []ContractAttachmentPublishTarget{b, a}
			}
			if _, err := s.PublishContractAttachmentsWithInputsContext(ctx, targets, 1); err != nil {
				t.Fatal(err)
			}
			for _, target := range targets {
				got, err := s.GetContractAttachmentContext(ctx, attachmentKey(target.Expected))
				if err != nil || got == nil || len(got.CompletedTokens) != 1 {
					t.Fatalf("missing acknowledgment %#v %v", got, err)
				}
			}
			var remaining int
			if err := s.db.QueryRow(`SELECT count(*) FROM generation_contract_work WHERE view_gen=0 AND token=?`, work.Token).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("primary debt remains: %d", remaining)
			}
			// Both duplicate requests remain valid after primary debt was consumed.
			for i := range targets {
				targets[i].Attachment.PayloadGeneration = attachmentBatchPayload(t, s)
			}
			if _, err := s.PublishContractAttachmentsWithInputsContext(ctx, targets, 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContractAttachmentDuplicateProofAndLegacyCleanupCustody(t *testing.T) {
	for _, corrupt := range []string{"legacy-singleton", "forged-file", "forged-scope", "forged-identity", "missing-ack", "missing-input", "corrupt-input", "missing-producer", "incomplete-producer"} {
		t.Run(corrupt, func(t *testing.T) {
			s := openCatalogStore(t)
			ctx := context.Background()
			target := attachmentBatchTarget(t, s, "repo")
			if _, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1); err != nil {
				t.Fatal(err)
			}
			winner := target.Attachment.PayloadGeneration
			target.Attachment.PayloadGeneration = attachmentBatchPayload(t, s)
			var err error
			switch corrupt {
			case "legacy-singleton":
				err = target.Store.PublishContractAttachmentWithInputsContext(ctx, target.Expected, target.Witnesses, target.Attachment, target.Work, 2)
			case "forged-file":
				target.Work[0].FilePath = "forged.go"
			case "forged-scope":
				target.Work[0].Scope.Deleted = !target.Work[0].Scope.Deleted
			case "forged-identity":
				target.Attachment.CheckoutID = "another-actor"
			case "missing-ack":
				_, err = s.writerDB.Exec(`DELETE FROM contract_attachment_work`)
			case "missing-input":
				_, err = s.writerDB.Exec(`DELETE FROM contract_attachment_inputs`)
			case "corrupt-input":
				_, err = s.writerDB.Exec(`UPDATE contract_attachment_inputs SET source_fingerprint='forged'`)
			case "missing-producer":
				_, err = s.writerDB.Exec(`DELETE FROM generation_producer_completeness WHERE view_gen=?`, winner)
			case "incomplete-producer":
				_, err = s.writerDB.Exec(`UPDATE generation_producer_completeness SET state='building' WHERE view_gen=?`, winner)
			}
			if corrupt == "legacy-singleton" {
				if !errors.Is(err, ErrCatalogStaleGuard) {
					t.Fatalf("legacy duplicate lost cleanup custody: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 2); err == nil || result != nil {
					t.Fatalf("forgery accepted %#v %v", result, err)
				}
			}
			var state string
			if err := s.db.QueryRow(`SELECT state FROM view_generations WHERE generation_id=?`, target.Attachment.PayloadGeneration).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != string(ViewGenerationBuilding) {
				t.Fatalf("loser sealed %s", state)
			}
		})
	}
}

func TestContractAttachmentDuplicateCopiedPhysicalProof(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	source := reservedGeneration(t, s, "copied-proof-source")
	physical := attachmentState("copied-proof")
	physical.CheckoutID = "actor-a"
	work := contractWorkFixture("copied-proof-debt")
	work.CheckoutID = physical.CheckoutID
	work.OriginGeneration = source
	if err := s.AtGeneration(source).SetContractInputStateWithWorkContext(ctx, nil, physical, []graph.ContractWork{work}); err != nil {
		t.Fatal(err)
	}
	// The consumer is actor-b, whose selected core carries actor-a's accepted
	// input and debt. This is the supported copied-actor capture shape.
	missing := graph.ContractInputState{RepoPrefix: "repo", CheckoutID: "actor-b"}
	witnesses := []graph.ContractInputWitness{{GenerationID: source, State: missing, Found: false}, {GenerationID: source, State: physical, Found: true}}
	expected, err := graph.ComposeContractInputState("repo", "actor-b", witnesses)
	if err != nil {
		t.Fatal(err)
	}
	publishContractCoreForTest(t, s, source)
	winner := attachmentBatchPayload(t, s)
	original := ContractAttachmentPublishTarget{Store: s.AtGeneration(source), Expected: expected, Witnesses: witnesses, Attachment: attachmentFor(expected, winner, work), Work: []graph.ContractWork{work}}
	if _, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{original}, 1); err != nil {
		t.Fatal(err)
	}
	seedFamilyAndCheckout(t, s.Catalog(), "copied-proof-family", "actor-b", "copied-proof-inc")
	copied, _, err := s.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "checkout", GraphID: "copied-proof-graph", CheckoutID: "actor-b", LayerID: "copied-proof-layer", GenerationKind: "commit", TreeOID: "copied-proof-tree", CreatedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyGenerationPayloadWhole(ctx, source, copied); err != nil {
		t.Fatal(err)
	}
	carried, found, err := s.AtGeneration(copied).ContractInputStateContext(ctx, "repo", "actor-a")
	if err != nil || !found {
		t.Fatalf("copied input %#v %v %v", carried, found, err)
	}
	absent, found, err := s.AtGeneration(copied).ContractInputStateContext(ctx, "repo", "actor-b")
	if err != nil || found {
		t.Fatalf("preferred actor unexpectedly found %#v %v %v", absent, found, err)
	}
	copiedWork, err := s.AtGeneration(copied).PendingContractWorkForScopeContext(ctx, "repo", "actor-a")
	if err != nil || len(copiedWork) != 1 {
		t.Fatalf("copied debt %#v %v", copiedWork, err)
	}
	currentWitnesses := []graph.ContractInputWitness{{GenerationID: copied, State: absent, Found: false}, {GenerationID: copied, State: carried, Found: true}}
	logical, err := graph.ComposeContractInputState("repo", "actor-b", currentWitnesses)
	if err != nil || !sameContractInput(logical, expected) {
		t.Fatalf("logical identity changed %#v %v", logical, err)
	}
	publishContractCoreForTest(t, s, copied)
	loser := attachmentBatchPayload(t, s)
	retry := ContractAttachmentPublishTarget{Store: s.AtGeneration(copied), Expected: logical, Witnesses: currentWitnesses, Attachment: attachmentFor(logical, loser, copiedWork...), Work: copiedWork}
	results, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{retry}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].PayloadGeneration != winner || results[0].UnusedPayloadGeneration != loser {
		t.Fatalf("copied proof winner %#v", results)
	}
	// A newly selected absence-only companion changes cohort completeness even
	// though ComposeContractInputState deliberately retains the same logical key.
	retry.Witnesses = append(retry.Witnesses, graph.ContractInputWitness{GenerationID: copied, State: graph.ContractInputState{RepoPrefix: "new-companion", CheckoutID: "actor-b"}, Found: false})
	unchanged, err := graph.ComposeContractInputState("repo", "actor-b", retry.Witnesses)
	if err != nil || !sameContractInput(unchanged, expected) {
		t.Fatalf("absence-only key changed %#v %v", unchanged, err)
	}
	if results, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{retry}, 4); !errors.Is(err, ErrCatalogStaleGuard) || results != nil {
		t.Fatalf("changed cohort accepted %#v %v", results, err)
	}
	got, err := s.GetContractAttachmentContext(ctx, attachmentKey(expected))
	if err != nil || got == nil || got.PayloadGeneration != winner {
		t.Fatalf("winner replaced %#v %v", got, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM view_generations WHERE generation_id=?`, loser).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(ViewGenerationBuilding) {
		t.Fatalf("unused reservation sealed %s", state)
	}
}
