package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/reach"
)

// A graph publication holding the topology gate must not keep a change answer
// past the caller's wait_deadline. The answer arrives by then as a lower bound
// that says why it is partial; a gate that is free, or frees in time, leaves
// the answer as it was.
func TestChangeToolsAnswerByWaitDeadlineWhileTopologyGateHeld(t *testing.T) {
	s, root := newRelationFixture(t)
	// A diff for detect_changes to map. The graph is not told, so no receipt
	// is pending and the relation wait itself has nothing to wait for.
	writeUnindexed(t, root, "caller.go", "package p\n\nfunc Caller() {\n\tTarget()\n\tTarget()\n}\n")
	id := targetID(t, s)
	within := func(d time.Duration) string { return time.Now().Add(d).Format(time.RFC3339Nano) }
	impact := func(args map[string]any) map[string]any {
		args["ids"], args["format"] = id, "json"
		payload := lifecycleResultPayload(t, callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact", args))
		delete(payload, "barrier_wait_ms")
		return payload
	}
	detect := func(args map[string]any) map[string]any {
		args["format"] = "json"
		payload := lifecycleResultPayload(t, callChangeTool(t, s.handleDetectChanges, "detect_changes", args))
		delete(payload, "barrier_wait_ms")
		return payload
	}

	// Gate free: wait_deadline changes nothing.
	free := impact(map[string]any{})
	require.Equal(t, true, free["complete"], "%v", free)
	require.Equal(t, float64(1), free["total_affected"], "%v", free)
	require.Equal(t, free, impact(map[string]any{"wait_deadline": within(2 * time.Second)}))
	freeDetect := detect(map[string]any{})
	require.NotContains(t, freeDetect, "graph_freshness")
	require.Equal(t, freeDetect, detect(map[string]any{"wait_deadline": within(2 * time.Second)}))

	finish := reach.BeginTopologyMutation(s.graph)
	released := make(chan struct{})
	release := sync.OnceFunc(func() {
		finish(false)
		close(released)
	})
	t.Cleanup(release)

	const wait, slack = 200 * time.Millisecond, 750 * time.Millisecond
	for name, call := range map[string]func(map[string]any) map[string]any{"explain_change_impact": impact, "detect_changes": detect} {
		started := time.Now()
		payload := call(map[string]any{"wait_deadline": within(wait)})
		elapsed := time.Since(started)
		t.Logf("%s answered in %v with the topology gate held", name, elapsed)
		assert.Less(t, elapsed, wait+slack, "%s waited on the topology gate past wait_deadline", name)
		assert.Equal(t, false, payload["complete"], "%s: %v", name, payload)
		assert.Equal(t, "pending", payload["graph_freshness"], "%s: %v", name, payload)
		assert.Equal(t, "publication_in_flight", payload["reason"], "%s: %v", name, payload)
	}

	// Freed before the deadline: the walk waits for it and answers in full.
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	require.Equal(t, free, impact(map[string]any{"wait_deadline": within(2 * time.Second)}))
	<-released
}
