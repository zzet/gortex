package store_sqlite

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Finite production fold-rebase discriminator. All public attachment inputs
// are genuine immutable rows. No source/cap/deadline/ownership policy changed.
func TestContractPublicationSourceKeepsCheckedAncestryAcrossRebase(t *testing.T) {
	ctx := context.Background()
	s := openCatalogStore(t)
	lower := reservedGeneration(t, s, "old-prefix")
	old := attachmentState("old-prefix")
	require.NoError(t, s.AtGeneration(lower).SetContractInputStateWithWorkContext(ctx, nil, old, nil))
	require.NoError(t, s.AtGeneration(lower).AddBatchChecked([]*graph.Node{{ID: "repo/base.go::Helper", Kind: graph.KindFunction, Name: "Helper", Language: "go", RepoPrefix: "repo", FilePath: "repo/base.go"}}, nil))
	publishContractCoreForTest(t, s, lower)
	folded := reservedGeneration(t, s, "folded-prefix")
	counts, err := s.CopyPayloadGeneration(ctx, lower, folded, "repo")
	require.NoError(t, err)
	require.Positive(t, counts.Nodes)
	publishContractCoreForTest(t, s, folded)
	child := reservedGeneration(t, s, "selected-child")
	_, err = s.writerDB.Exec(`UPDATE view_generations SET base_generation_id=? WHERE generation_id=?`, lower, child)
	require.NoError(t, err)
	current := attachmentState("child-current")
	require.NoError(t, s.AtGeneration(child).SetContractInputStateWithWorkContext(ctx, &old, current, nil))
	publishContractCoreForTest(t, s, child)
	physical := s.AtGeneration(child)
	witnesses := []graph.ContractInputWitness{{GenerationID: lower, State: old, Found: true}, {GenerationID: child, State: current, Found: true}}
	logical, err := graph.ComposeContractInputState("repo", "wt", witnesses)
	require.NoError(t, err)
	payload := attachmentPayload(t, s)
	target := ContractAttachmentPublishTarget{Store: physical, Expected: logical, Witnesses: witnesses, Attachment: attachmentFor(logical, payload)}
	// Prove every ordinary publication guard succeeds BEFORE the actual rebase,
	// without committing an attachment that would mask this missing publication.
	tx, err := s.writerDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var result ContractAttachmentPublication
	require.NoError(t, physical.validateContractAttachmentPublicationTx(ctx, tx, target.Expected, target.Witnesses, target.Attachment, target.Work, &result))
	require.NoError(t, tx.Rollback())
	captured, err := physical.CaptureContractPublicationSource(ctx, logical, witnesses)
	require.NoError(t, err)
	// The production API verifies ready/cycle/from guards and mutates only the
	// child's catalog parent. Folded graph and input rows are exact copies.
	require.NoError(t, s.Catalog().RebaseViewGeneration(ctx, RebaseViewGenerationRequest{GenerationID: child, FromBase: lower, ToBase: folded}))
	copied, found, err := s.AtGeneration(folded).ContractInputStateContext(ctx, "repo", "wt")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, old, copied)
	require.True(t, reflect.DeepEqual(s.AtGeneration(lower).GetRepoNodes("repo"), s.AtGeneration(folded).GetRepoNodes("repo")))
	newVector := []graph.ContractInputWitness{{GenerationID: folded, State: copied, Found: true}, {GenerationID: child, State: current, Found: true}}
	after, err := graph.ComposeContractInputState("repo", "wt", newVector)
	require.NoError(t, err)
	require.Equal(t, logical, after)
	// Original captured physical inputs are unchanged; current code instead
	// rejects their old, still-valid ancestry after the background metadata move.
	_, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1)
	require.ErrorIs(t, err, ErrCatalogStaleGuard, "ordinary unproved callers retain current ancestry guard")
	derived := target
	derived.Store = captured.AtGeneration(child)
	_, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{derived}, 1)
	require.ErrorIs(t, err, ErrCatalogStaleGuard, "generation derivation cannot carry capture authority")
	target.Store = captured
	_, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1)
	require.NoError(t, err, "checked captured ancestry remains valid while exact physical inputs stay unchanged")
	winner, err := s.GetContractAttachmentContext(ctx, attachmentKey(logical))
	require.NoError(t, err)
	require.NotNil(t, winner)
	require.Equal(t, payload, winner.PayloadGeneration)
}

func TestContractPublicationSourceRejectsSiblingMissingAndForgedVectors(t *testing.T) {
	ctx := context.Background()
	s := openCatalogStore(t)
	core := reservedGeneration(t, s, "capture-core")
	state := attachmentState("current")
	require.NoError(t, s.AtGeneration(core).SetContractInputStateWithWorkContext(ctx, nil, state, nil))
	publishContractCoreForTest(t, s, core)
	sibling := reservedGeneration(t, s, "capture-sibling")
	require.NoError(t, s.AtGeneration(sibling).SetContractInputStateWithWorkContext(ctx, nil, state, nil))
	publishContractCoreForTest(t, s, sibling)
	valid := []graph.ContractInputWitness{{GenerationID: core, State: state, Found: true}}
	for _, mode := range []string{"sibling", "absence", "duplicate", "wrong_header", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			rows := append([]graph.ContractInputWitness(nil), valid...)
			switch mode {
			case "sibling":
				rows[0].GenerationID = sibling
			case "absence":
				rows[0].Found = false
			case "duplicate":
				rows = append(rows, rows[0])
			case "wrong_header":
				rows[0].State.InputFingerprint = "forged"
			case "omitted":
				rows = nil
			}
			_, err := s.AtGeneration(core).CaptureContractPublicationSource(ctx, state, rows)
			require.Error(t, err)
		})
	}
	captured, err := s.AtGeneration(core).CaptureContractPublicationSource(ctx, state, valid)
	require.NoError(t, err)
	// Mutation of the caller's vector cannot mutate the captured authority.
	valid[0].State.InputFingerprint = "forged"
	target := ContractAttachmentPublishTarget{Store: captured, Expected: state, Witnesses: valid, Attachment: attachmentFor(state, attachmentPayload(t, s))}
	_, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1)
	require.ErrorIs(t, err, ErrCatalogStaleGuard)
	valid[0].State = state
	target.Witnesses = valid
	_, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1)
	require.NoError(t, err)
	// Captured receiver has no teardown authority over the actual store.
	require.NoError(t, captured.Close())
	require.NotNil(t, s.AtGeneration(core))
	_, found, err := s.AtGeneration(core).ContractInputStateContext(ctx, "repo", "wt")
	require.NoError(t, err)
	require.True(t, found)
}

func TestContractPublicationSourceStillRefusesChangedPrimaryAndCancellation(t *testing.T) {
	ctx := context.Background()
	s := openCatalogStore(t)
	state := attachmentState("primary-current")
	state.CheckoutID = ""
	require.NoError(t, s.BeginContractInputMutationContext(ctx, nil, state, nil))
	require.NoError(t, s.AcceptContractInputMutationContext(ctx, state))
	rows := []graph.ContractInputWitness{{GenerationID: 0, State: state, Found: true}}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := s.CaptureContractPublicationSource(canceled, state, rows)
	require.ErrorIs(t, err, context.Canceled)
	captured, err := s.CaptureContractPublicationSource(ctx, state, rows)
	require.NoError(t, err)
	payload := attachmentPayload(t, s)
	next := state
	next.InputFingerprint = "primary-pending"
	require.NoError(t, s.BeginContractInputMutationContext(ctx, &state, next, nil))
	target := ContractAttachmentPublishTarget{Store: captured, Expected: state, Witnesses: rows, Attachment: attachmentFor(state, payload)}
	_, err = s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1)
	require.ErrorIs(t, err, ErrCatalogStaleGuard)
	got, err := s.GetContractAttachmentContext(ctx, attachmentKey(state))
	require.NoError(t, err)
	require.Nil(t, got)
	generation, found, err := s.Catalog().GetViewGeneration(ctx, payload)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ViewGenerationBuilding, generation.State, "refusal must not seal even one payload")
}
