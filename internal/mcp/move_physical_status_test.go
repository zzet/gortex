package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMovePhysicalStatusMatchesExistingGuardedRecord(t *testing.T) {
	s, move, ctx, ack, _, target := moveObservationExecutionFixture(t)
	ack.finishActual()
	ack.finishReply(nil, nil)
	timing := move.records[target]
	physical := timing.moveFile.physical.Value()
	require.NotNil(t, physical)
	before := s.mutationStatusPayload(physical)
	status, handled := s.moveFileObservationPayload(ctx, timing.id)
	require.True(t, handled)
	require.Equal(t, "observed", status["physical_commit_state"])
	require.Equal(t, before, status["physical_commit"], "render the same owned physical status, including unavailable/null publication evidence")
	require.Equal(t, true, status["physical_commit_shared_acknowledgement"])
	require.Equal(t, false, status["physical_commit_additive_with_acknowledgement"])
	require.Equal(t, "move_file_observation", status["receipt_kind"])
	require.NotContains(t, status, "mutation_id")
	require.NotContains(t, status, "graph_status_terminal", "the timing row does not become physical authority")
	child := status["physical_commit"].(map[string]any)
	require.Equal(t, physical.id, child["receipt"])
	child["graph_status"] = "fabricated-client-value"
	again, _ := s.moveFileObservationPayload(ctx, timing.id)
	require.NotEqual(t, "fabricated-client-value", again["physical_commit"].(map[string]any)["graph_status"])
}
