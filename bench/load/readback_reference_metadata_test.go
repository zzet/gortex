package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadbackReferenceFullMetadataRejectsSourceAndBriefShapes(t *testing.T) {
	image, bodies := readbackReferenceOracleFixture(t)
	require.False(t, readbackReferenceCorrect("symbol", readbackReferenceWire(bodies["source"]), image, "Target", false), "source spans cannot prove full metadata")
	node := bodies["summary"]["nodes"].([]any)[0].(map[string]any)
	require.False(t, readbackReferenceCorrect("symbol", readbackReferenceWire(node), image, "Target", false), "brief node cannot prove full metadata")
	full := map[string]any{"node": node, "in_edges": []any{}, "out_edges": nil}
	require.True(t, readbackReferenceCorrect("symbol", readbackReferenceWire(full), image, "Target", false))
	delete(full, "in_edges")
	require.False(t, readbackReferenceCorrect("symbol", readbackReferenceWire(full), image, "Target", false))
}

func TestReadbackReferenceMetadataSurfaceIsExplicitlyUnavailableBesideExactQuartet(t *testing.T) {
	image, bodies := readbackReferenceOracleFixture(t)
	trial := readbackReferenceTrial{ProbeFile: image.File, Target: "Target", After: map[string]readbackReferenceImage{image.File: image}, Request: request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file"}}}
	requests := readbackReferenceProbeRequests(trial)
	require.Equal(t, "get_symbol", requests["symbol"].Tool, "metadata has no public read.symbol route")
	require.Equal(t, "full", requests["symbol"].Arguments["detail"])
	caller := &referenceOrderedCaller{bodies: bodies, image: image}
	row := observeReadbackReference(t.Context(), caller, privateRepo{Path: "/owned/primary", Prefix: "owned"}, trial, "under_edit", time.Second)
	require.True(t, row.Correct, "the required quartet stays independent")
	require.Len(t, caller.calls, 5, "one ACK and four real read endpoints")
	require.False(t, row.Probes["symbol"].Correct)
	require.Nil(t, row.Probes["symbol"].Latency.Value)
	require.Contains(t, row.Probes["symbol"].Error, "compatibility")
}
