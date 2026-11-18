package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMeasuredParseEqualClockTickIsNotMissingOrReversed(t *testing.T) {
	at := time.Now()
	for _, kind := range []string{"completed_equal", "missing", "reversed"} {
		t.Run(kind, func(t *testing.T) {
			ack := &editAcknowledgement{entered: at}
			if kind != "missing" {
				begin := at
				if kind == "reversed" {
					begin = at.Add(time.Millisecond)
				}
				lap := ack.beginLapAt(editAckParseGate, begin)
				lap.finishAt(at)
			}
			ack.finishActualAt(at)
			ack.finishReplyAt(at, "result")
			snapshot := ack.snapshot()
			payload := buildAcknowledgementPayload(&snapshot, "observed")
			parse := payload.ObservedExecutionMS["parse"]
			if kind == "completed_equal" {
				require.Equal(t, "measured", payload.PhaseStates["parse"])
				require.NotNil(t, parse)
				require.Zero(t, *parse)
				require.EqualValues(t, 1, payload.Counts["parse"].Started)
				require.EqualValues(t, 1, payload.Counts["parse"].Completed)
				require.Zero(t, payload.Counts["parse"].NotApplicable)
				require.True(t, payload.Disjoint)
			} else {
				require.Nil(t, parse, "no observed parse or a reversed interval cannot acquire zero-duration credit")
				require.NotEqual(t, "measured", payload.PhaseStates["parse"])
			}
		})
	}
}
