package main

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

type referenceOrderedCaller struct {
	mu          sync.Mutex
	calls       []string
	next        int64
	before      bool
	ackFinished time.Time
	bodies      map[string]map[string]any
	image       readbackReferenceImage
}

func (c *referenceOrderedCaller) callWithIdentity(_ context.Context, _ string, args any) (json.RawMessage, wireCallIdentity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := args.(map[string]any)
	a := p["arguments"].(map[string]any)
	operation := a["operation"].(string)
	tool := p["name"].(string)
	c.next++
	now := time.Now()
	id := wireCallIdentity{ID: c.next, Started: now, Finished: now}
	if tool == "edit" {
		c.calls = append(c.calls, "ACK")
		c.ackFinished = now
		return readbackReferenceWire(map[string]any{"disk_status": "committed", "ack_mode": "local", "content_sha256": readbackReferenceSHA(c.image.Bytes)}), id, nil
	}
	if c.ackFinished.IsZero() {
		c.before = true
	}
	c.calls = append(c.calls, operation)
	if operation == "symbol" {
		return readbackReferenceWire(c.bodies["source"]), id, nil
	}
	return readbackReferenceWire(c.bodies[operation]), id, nil
}
func TestReadbackReferenceSameSessionImmediateBurstHasNoVisibilityRetry(t *testing.T) {
	image, bodies := readbackReferenceOracleFixture(t)
	c := &referenceOrderedCaller{bodies: bodies, image: image}
	trial := readbackReferenceTrial{Ordinal: 0, Editor: 0, ProbeFile: image.File, Target: "Target", After: map[string]readbackReferenceImage{image.File: image}, Request: request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file"}}}
	row := observeReadbackReference(t.Context(), c, privateRepo{Path: "/owned/primary", Prefix: "owned"}, trial, "under_edit", time.Second)
	require.True(t, row.Correct, "%+v", row.Errors)
	require.False(t, c.before)
	require.Len(t, c.calls, 5)
	require.Equal(t, "ACK", c.calls[0])
	require.Len(t, row.Probes, 5)
	for name, p := range row.Probes {
		if name == "symbol" {
			continue
		}
		require.Greater(t, p.ID, row.ACK.ID)
		require.False(t, p.Started.Before(row.ProbeOrigin))
	}
}
func TestReadbackReferenceImmediatePendingDoesNotPollUntilItLooksCorrect(t *testing.T) {
	image, bodies := readbackReferenceOracleFixture(t)
	bodies["source"] = map[string]any{"source_state": "pending", "complete": false}
	c := &referenceOrderedCaller{bodies: bodies, image: image}
	trial := readbackReferenceTrial{ProbeFile: image.File, Target: "Target", After: map[string]readbackReferenceImage{image.File: image}, Request: request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file"}}}
	row := observeReadbackReference(context.Background(), c, privateRepo{Path: "/owned/primary", Prefix: "owned"}, trial, "under_edit", time.Second)
	require.False(t, row.Correct)
	require.False(t, row.Probes["source"].Correct)
	require.Len(t, c.calls, 5)
}

func TestReadbackReferenceOpenLoopDoesNotWaitForPreviousEditorCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runReadbackReferenceOffers(ctx, time.Now(), []readbackReferenceTrial{{Editor: 0, Ordinal: 0}, {Editor: 0, Ordinal: 1}}, func(_ int, trial readbackReferenceTrial) {
			if trial.Ordinal == 0 {
				close(firstEntered)
				<-release
			} else {
				<-firstEntered
				close(secondEntered)
			}
		})
		close(done)
	}()
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Error("second fixed-editor offer was serialized behind previous completion")
	}
	close(release)
	<-done
}
