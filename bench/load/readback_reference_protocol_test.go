package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadbackReferenceProtocolDiagnosticCannotBecomeAcceptance(t *testing.T) {
	p := readbackReferencePlan{Seed: 1, EditMeanNS: int64(editProbeMean), ProductDeadlineSeconds: 120}
	cells, err := readbackReferenceSchedules(p)
	require.NoError(t, err)
	original := cells[0]
	firstDue := original.Arrival[0].DueNS
	diagnostic := readbackReferenceProtocolSchedule(original, 120*time.Second)
	require.Len(t, diagnostic.Arrival, 1, "one diagnostic offer, not a campaign")
	require.Zero(t, diagnostic.Arrival[0].DueNS)
	require.Equal(t, int64(240*time.Second), diagnostic.PhaseBoundNS)
	require.Equal(t, 100, diagnostic.Cell.Offers, "required acceptance denominator stays frozen")
	require.False(t, summarizeReadbackReference(diagnostic.Cell.Offers, []bool{true}).Pass)
	require.Len(t, original.Arrival, 100)
	require.Equal(t, firstDue, original.Arrival[0].DueNS, "diagnostic must not rebase the acceptance schedule")
}

type referenceACKModeCaller struct {
	*referenceOrderedCaller
	ack map[string]any
}

func (c referenceACKModeCaller) callWithIdentity(ctx context.Context, method string, args any) (json.RawMessage, wireCallIdentity, error) {
	raw, id, err := c.referenceOrderedCaller.callWithIdentity(ctx, method, args)
	if args.(map[string]any)["name"] == "edit" {
		return readbackReferenceWire(c.ack), id, err
	}
	return raw, id, err
}

func TestReadbackReferenceUnknownACKPolicyCannotPassOrdinaryReadback(t *testing.T) {
	for _, mode := range []any{nil, "", "other", 1} {
		t.Run(fmt.Sprintf("mode_%v", mode), func(t *testing.T) {
			image, bodies := readbackReferenceOracleFixture(t)
			ack := map[string]any{"disk_status": "committed", "content_sha256": readbackReferenceSHA(image.Bytes)}
			if mode != nil {
				ack["ack_mode"] = mode
			}
			caller := referenceACKModeCaller{&referenceOrderedCaller{bodies: bodies, image: image}, ack}
			trial := readbackReferenceTrial{ProbeFile: image.File, Target: "Target", After: map[string]readbackReferenceImage{image.File: image}, Request: request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file"}}}
			row := observeReadbackReference(t.Context(), caller, privateRepo{Path: "/owned/primary", Prefix: "owned"}, trial, "under_edit", time.Second)
			require.False(t, row.Correct, "exact read bytes cannot invent the mutation's ACK policy")
			require.NotEmpty(t, row.Errors)
		})
	}
}

func TestReadbackReferenceRenameUsesPhysicalACKAndMoveKeepsMissingPolicyUnavailable(t *testing.T) {
	rename := readbackReferenceTrial{Request: request{Tool: "refactor", Operation: "rename"}}
	mode, evidence, err := readbackReferenceACKPolicy(map[string]any{"files": []any{map[string]any{"ack_mode": "local"}}}, rename)
	require.NoError(t, err)
	require.Equal(t, "local", mode)
	require.Equal(t, "direct original ACK", evidence)
	_, _, err = readbackReferenceACKPolicy(map[string]any{"ack_mode": "full", "files": []any{map[string]any{}}}, rename)
	require.Error(t, err, "aggregate policy cannot replace missing physical-row evidence")
	move := readbackReferenceTrial{Request: request{Tool: "refactor", Operation: "move"}}
	mode, evidence, err = readbackReferenceACKPolicy(map[string]any{}, move)
	require.NoError(t, err)
	require.Empty(t, mode)
	require.Contains(t, evidence, "unavailable")
	_, _, err = readbackReferenceACKPolicy(map[string]any{"ack_mode": "other"}, move)
	require.Error(t, err, "the known missing field does not license contradictory evidence")
}
