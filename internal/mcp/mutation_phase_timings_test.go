package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestMutationStatusWithoutTimelineKeepsMissingTimingNull(t *testing.T) {
	s := newMutationServer()
	record := s.beginMutationCommit(context.Background(), "edit_file", "", "fp", "edit.go", filepath.Join(t.TempDir(), "edit.go"))
	payload := s.mutationStatusPayload(record)
	require.NotContains(t, payload, "publication_phases", "do not fabricate an old timeline for a canonical/in-flight receipt")
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	phases := wire["phases_ms"].(map[string]any)
	states := wire["phase_states"].(map[string]any)
	require.Len(t, phases, 19) // original15 plus two admissions, leaf control and caller-result finalization
	require.Contains(t, phases, "cohort_admission")
	require.Contains(t, phases, "batch_admission")
	require.Contains(t, phases, "edit_control")
	require.Contains(t, phases, "caller_result")
	for name, ms := range phases {
		require.Nil(t, ms, name)
		require.Equal(t, "not_observed", states[name])
	}
	window := wire["phase_timing"].(map[string]any)
	require.Equal(t, "unavailable", window["clock"])
	require.Equal(t, "partial", window["coverage"])
	require.Nil(t, window["ack_wall_ms"])
	require.Nil(t, window["publication_wall_ms"])
}

func TestMutationStatusTimingAndLegacyTimelineShareSnapshot(t *testing.T) {
	s := newMutationServer()
	record := s.beginMutationCommit(context.Background(), "write_file", "", "fp", "edit.go", filepath.Join(t.TempDir(), "edit.go"))
	record.markCommitted("sha", 10)
	id := "timed-" + record.id
	ticket := &mutationReceipt{id: id, done: make(chan struct{}), generation: 7}
	s.mutationReceipts.Store(id, ticket)
	record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: id, Generation: 7})
	origin := time.Now()
	timeline := indexer.DefaultPublicationPhases().Begin("timing-"+record.id, id, indexer.PublicationSourceMCPEdit, origin)
	timeline.MarkAt(indexer.PublicationDiskWriteStarted, origin.Add(time.Millisecond))
	timeline.MarkAt(indexer.PublicationReceiptCommitted, origin.Add(2*time.Millisecond))
	timeline.MarkAt(indexer.PublicationTicketEnqueued, origin.Add(4*time.Millisecond))
	before := timeline.Snapshot()
	payload := s.mutationStatusPayload(record)
	require.Equal(t, before, payload["publication_phases"], "keep the old snapshot type and contents")
	phases := payload["phases_ms"].(map[string]*float64)
	require.Equal(t, 1.0, *phases["disk_write"])
	require.Equal(t, 2.0, *phases["publish"])
	require.Nil(t, payload["phase_timing"].(indexer.EditTimingWindow).PublicationWallMS)
	timeline.MarkAt(indexer.PublicationTicketCompleted, origin.Add(10*time.Millisecond))
	final := s.mutationStatusPayload(record)
	require.Equal(t, "partial", final["phase_timing"].(indexer.EditTimingWindow).Coverage)
	require.Equal(t, 10.0, *final["phase_timing"].(indexer.EditTimingWindow).PublicationWallMS)
	require.Nil(t, payload["phase_timing"].(indexer.EditTimingWindow).PublicationWallMS, "an earlier response owns an immutable snapshot")
}

func TestGraphOnlyReceiptTimingDoesNotInventAcknowledgement(t *testing.T) {
	s := newMutationServer()
	ticket := &mutationReceipt{id: "refresh-" + t.Name(), done: make(chan struct{})}
	s.mutationReceipts.Store(ticket.id, ticket)
	payload, found := s.graphRefreshReceiptPayload(context.Background(), ticket.id)
	require.True(t, found)
	states := payload["phase_states"].(map[string]string)
	phases := payload["phases_ms"].(map[string]*float64)
	for _, name := range []string{"dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait"} {
		require.Equal(t, "not_applicable", states[name])
		require.Nil(t, phases[name])
	}
	require.Equal(t, "not_observed", states["patch_parse"])
	require.NotContains(t, payload, "publication_phases")
}
