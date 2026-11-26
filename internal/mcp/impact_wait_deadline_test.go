package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
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

	// A wait_deadline that passes while git diffs the tree still bounds the
	// walk: detect_changes reads the deadline before it runs git, so a slow
	// diff does not turn the caller's deadline into the walk's 3 s budget.
	if os.PathSeparator == '\\' {
		t.Log("skipping the slow-diff step: the stand-in git is a POSIX shell script")
	} else {
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		const diffDelay = 400 * time.Millisecond
		stubDir := t.TempDir()
		stub := "#!/bin/sh\nfor arg in \"$@\"; do\n\tif [ \"$arg\" = diff ]; then sleep 0.4; break; fi\ndone\nexec '" + realGit + "' \"$@\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(stubDir, "git"), []byte(stub), 0o755))
		t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		started := time.Now()
		payload := detect(map[string]any{"wait_deadline": within(wait)})
		elapsed := time.Since(started)
		t.Logf("detect_changes answered in %v with a %v diff and the topology gate held", elapsed, diffDelay)
		require.GreaterOrEqual(t, elapsed, diffDelay, "the stand-in git did not hold the diff past wait_deadline")
		assert.Less(t, elapsed, diffDelay+slack, "a wait_deadline that passed during the diff stopped bounding the walk")
		assert.Equal(t, false, payload["complete"], "%v", payload)
		assert.Equal(t, "pending", payload["graph_freshness"], "%v", payload)
		assert.Equal(t, "publication_in_flight", payload["reason"], "%v", payload)
	}

	// Freed before the deadline: the walk waits for it and answers in full.
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	require.Equal(t, free, impact(map[string]any{"wait_deadline": within(2 * time.Second)}))
	<-released
}
