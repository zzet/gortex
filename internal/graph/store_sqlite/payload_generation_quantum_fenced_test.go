package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// Once a burst has fenced a generation through one full quantum, every
// further quantum takes the writer once: the reference check, the fence
// transaction and the writer drain run once per generation, not per quantum.
func TestFencedRetirementQuantaRunOnePreamble(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	buildMinimalAnalysisGeneration(t, s.AtGeneration(generationID), "fenced", 12, true)
	preambles, holds := s.RetirementPreambles(), s.writeMu.holds.Load()
	progress, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	quanta := progress.ChunksCommitted
	removed := false
	for attempt := 0; attempt < 512; attempt++ {
		progress, err := s.RetirePayloadGenerationQuantumFenced(t.Context(), generationID, nil)
		quanta += progress.ChunksCommitted
		if progress.CatalogRemoved {
			require.NoError(t, err)
			removed = true
			break
		}
		require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	}
	require.True(t, removed, "fenced quanta did not converge")
	require.Greater(t, quanta, int64(3))
	require.EqualValues(t, 1, s.RetirementPreambles()-preambles, "a fenced continuation re-ran the preamble")
	// One fence transaction, one drain, one transaction per quantum, and the
	// final catalog delete.
	require.LessOrEqual(t, s.writeMu.holds.Load()-holds, quanta+3)
	for _, table := range payloadGenerationTables() {
		require.Zero(t, countAtGeneration(t, s, table, generationID), table)
	}
}

// A fenced continuation still refuses a generation a reader or builder took
// ownership of after the fence, and a generation this process never fenced
// takes the full quantum.
func TestFencedRetirementQuantumKeepsOwnershipChecks(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	preambles := s.RetirementPreambles()
	progress, err := s.RetirePayloadGenerationQuantumFenced(t.Context(), generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	require.EqualValues(t, 1, progress.ChunksCommitted)
	require.EqualValues(t, 1, s.RetirementPreambles()-preambles, "an unfenced generation must take the full quantum")
	before := countAtGeneration(t, s, "nodes", generationID)
	progress, err = s.RetirePayloadGenerationQuantumFenced(t.Context(), generationID, func(int64) bool { return true })
	require.ErrorIs(t, err, ErrPayloadGenerationInUse)
	require.Equal(t, PayloadRetirementProgress{}, progress)
	require.Equal(t, before, countAtGeneration(t, s, "nodes", generationID))
}

// A retiring generation refuses a new contract attachment with the typed
// admission error the other reference writers return. The attachment never
// landed even before the admission check — the guarded building -> ready
// UPDATE in the same transaction rolled it back — so the row assertion pins
// that guard and the error assertion pins the admission check.
func TestContractAttachmentRefusesARetiringPayloadGeneration(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	core := reservedGeneration(t, s, "retiring-attachment-core")
	h := s.AtGeneration(core)
	state := attachmentState("retiring")
	state.RepoPrefix = ""
	state.CheckoutID = ""
	require.NoError(t, h.SetContractInputStateWithWorkContext(ctx, nil, state, nil))
	publishContractCoreForTest(t, s, core)
	payload := attachmentPayload(t, s)
	require.NoError(t, s.Catalog().BeginViewGenerationRetirement(ctx, payload))
	err := h.PublishContractAttachmentContext(ctx, state, attachmentFor(state, payload), nil, 1)
	require.ErrorIs(t, err, ErrCatalogGenerationRetiring)
	got, err := h.GetContractAttachmentContext(ctx, attachmentKey(state))
	require.NoError(t, err)
	require.Nil(t, got, "a retiring generation was given an attachment")
}

// A fenced generation referenced again after its fence — as the contract-input
// term of the reference predicate can make it without a write to the
// generation — is refused by the final catalog delete. That refusal names its
// holder like the fast check does and is not an error. Once the reference
// goes, the removal reports itself and the base it released.
func TestFencedRetirementReferencedAgainIsARefusalNotAnError(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	var mu sync.Mutex
	var releases []GenerationReferenceRelease
	OnGenerationReferencesReleased(func(release GenerationReferenceRelease) {
		mu.Lock()
		releases = append(releases, release)
		mu.Unlock()
	})
	t.Cleanup(func() { OnGenerationReferencesReleased(nil) })
	_, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	// The fence has committed. A layer now names the retiring generation as
	// its base, written past the catalog's admission checks.
	child := reservedGeneration(t, s, "referenced-again")
	write := func(query string, args ...any) {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_, err := s.writerDB.ExecContext(t.Context(), query, args...)
		require.NoError(t, err)
	}
	write(`UPDATE view_generations SET base_generation_id = ? WHERE generation_id = ?`, generationID, child)
	before := viewmetrics.Read()
	refused := false
	for attempt := 0; attempt < 512 && !refused; attempt++ {
		progress, err := s.RetirePayloadGenerationQuantumFenced(t.Context(), generationID, nil)
		require.False(t, progress.CatalogRemoved)
		if errors.Is(err, ErrCatalogGenerationReferenced) {
			// The refusal names what held it, as the fast check's does.
			var referenced *GenerationReferencedError
			require.ErrorAs(t, err, &referenced)
			require.True(t, referenced.Refs.OnlyBased())
			refused = true
			break
		}
		require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	}
	require.True(t, refused, "the swept generation was never refused")
	after := viewmetrics.Read()
	require.Zero(t, retireRefusalDelta(before, after, viewmetrics.RefusedError), "a reference refusal was counted as an error")
	require.EqualValues(t, 1, retireRefusalDelta(before, after, viewmetrics.RefusedBased))

	write(`UPDATE view_generations SET base_generation_id = NULL WHERE generation_id = ?`, child)
	row, found, err := s.Catalog().GetViewGeneration(t.Context(), generationID)
	require.NoError(t, err)
	require.True(t, found)
	progress, err := s.RetirePayloadGenerationQuantumFenced(t.Context(), generationID, nil)
	require.NoError(t, err)
	require.True(t, progress.CatalogRemoved)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, releases)
	last := releases[len(releases)-1]
	require.Equal(t, generationID, last.Removed)
	require.Equal(t, positiveIDs(row.BaseGenerationID), last.Released)
}
