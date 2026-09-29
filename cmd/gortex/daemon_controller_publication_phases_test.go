package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// Publication records are keyed by checkout and generation, so they ride on
// daemon status as their own top-level block, never inside the views census.
func TestStatusRendersPublicationPhasesBesideTheViewsBlock(t *testing.T) {
	recorder := indexer.NewPublicationPhaseRecorder(0, 0)
	origin := time.Now()
	record := recorder.Begin("checkout-status", "mutation-status-1", indexer.PublicationSourceMCPEdit, origin)
	record.BindTicket(5)
	record.MarkAt(indexer.PublicationReceiptCommitted, origin.Add(2*time.Millisecond))
	record.MarkAt(indexer.PublicationTicketEnqueued, origin.Add(3*time.Millisecond))
	record.SetGeneration(17)
	record.MarkAt(indexer.PublicationTicketCompleted, origin.Add(40*time.Millisecond))

	rendered := publicationPhasesStatus(recorder)
	require.Len(t, rendered, 1)
	got := rendered[0]
	require.Equal(t, "mutation-status-1", got.Key)
	require.Equal(t, "checkout-status", got.CheckoutID)
	require.Equal(t, indexer.PublicationSourceMCPEdit, got.Source)
	require.EqualValues(t, 5, got.Ticket)
	require.EqualValues(t, 17, got.DirtyGenerationID)
	require.True(t, got.Terminal)
	require.Equal(t, "monotonic", got.Clock)
	phases := make([]string, 0, len(got.Phases))
	for _, phase := range got.Phases {
		phases = append(phases, phase.Phase)
	}
	require.Equal(t, []string{"received", "receipt_committed", "ticket_enqueued", "ticket_completed"}, phases)
	require.EqualValues(t, (40 * time.Millisecond).Nanoseconds(), got.Phases[3].OffsetNS)

	raw, err := json.Marshal(daemon.StatusResponse{Views: &daemon.ViewsStatus{Families: 1}, PublicationPhases: rendered})
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Contains(t, decoded, "publication_phases", "the status payload has no top-level publication_phases")
	views, _ := decoded["views"].(map[string]any)
	require.NotContains(t, views, "publication_phases", "publication records leaked into the identity-free views block")

	empty, err := json.Marshal(daemon.StatusResponse{PublicationPhases: publicationPhasesStatus(indexer.NewPublicationPhaseRecorder(0, 0))})
	require.NoError(t, err)
	require.NotContains(t, string(empty), "publication_phases", "an empty recorder renders a block")
}
