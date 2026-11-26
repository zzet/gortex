package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/hooks"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/testenv"
)

type pendingHookDetectRequest struct {
	cwd      string
	deadline string
}

// pendingHookDispatcher records each detect_changes call the hooks send
// through the real daemon socket, then dispatches it unchanged.
type pendingHookDispatcher struct {
	editACKRegisteredDispatcher
	detect chan<- pendingHookDetectRequest
}

func (d pendingHookDispatcher) Dispatch(ctx context.Context, session *daemon.Session, frame []byte) ([]byte, error) {
	var request struct {
		Params struct {
			Name string `json:"name"`
			Args struct {
				Deadline string `json:"wait_deadline"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &request) == nil && request.Params.Name == "detect_changes" {
		select {
		case d.detect <- pendingHookDetectRequest{cwd: session.CWD, deadline: request.Params.Args.Deadline}:
		default:
		}
	}
	return d.editACKRegisteredDispatcher.Dispatch(ctx, session, frame)
}

// A real watched edit is held after its SQLite apply and before relations
// publish. Both briefing hook entry points (the Stop enrich hook and the
// apply_patch PostToolUse enrich hook) reach the daemon over its socket during
// that same held interval. Each must return within the half-second hook budget with a
// pending relations briefing: either the labelled partial that names the file,
// or the bounded advisory when detect_changes did not answer in time.
func TestPendingWatchedEditReachesBothHookBriefings(t *testing.T) {
	selectedProjectionRegistry(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("GORTEX_HOOK_SESSION_DIR", filepath.Join(home, "sessions"))
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", filepath.Join(home, "hook.jsonl"))
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	t.Setenv(batchTransactionDirEnv, filepath.Join(home, "transactions"))
	f, store, _ := newSharedReceiptFreshContractFixture(t)
	s := f.server
	s.SetMaterializer(&graphview.Materializer{Store: store.Store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()})
	checkoutMutationGit(t, f.root, "init", "-q")
	checkoutMutationGit(t, f.root, "add", "main.go")
	checkoutMutationGit(t, f.root, "-c", "commit.gpgsign=false", "-c", "core.hooksPath="+os.DevNull, "commit", "-qm", "source fixture")

	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state.json"))
	d := daemon.New(socket, "pending-watcher-hooks", zap.NewNop())
	detect := make(chan pendingHookDetectRequest, 2)
	d.MCPDispatcher = pendingHookDispatcher{editACKRegisteredDispatcher: editACKRegisteredDispatcher{server: s}, detect: detect}
	require.NoError(t, d.Listen())
	served := make(chan error, 1)
	go func() { served <- d.Serve() }()
	t.Cleanup(func() {
		store.unblock()
		require.NoError(t, d.Shutdown())
		require.NoError(t, <-served)
	})

	// The PreToolUse hook records this session's write. Without it the Stop
	// briefing has no change of this session to report or attribute.
	const writer = "pending-watcher-hooks-writer"
	path := filepath.Join(f.root, "main.go")
	enrichHook := func() { hooks.Run(0, hooks.ModeEnrich) }
	patchHook := func() { hooks.RunCodex(0, hooks.CodexModeEnrich) }
	invokePendingWatcherHook(t, enrichHook, map[string]any{
		"hook_event_name": "PreToolUse", "session_id": writer, "cwd": f.root, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": path, "old_string": "// stream-1-tail", "new_string": "func Stream1Edit00() { Target() }\n// stream-1-tail"},
	})
	sessionState, err := os.ReadFile(filepath.Join(home, "sessions", writer+".json"))
	require.NoError(t, err)
	var attribution struct {
		WrittenPaths []string `json:"written_paths"`
	}
	require.NoError(t, json.Unmarshal(sessionState, &attribution))
	require.Contains(t, attribution.WrittenPaths, path)

	store.armed.Store(true)
	offer := &sharedReceiptOffer{agent: 1, ticket: make(chan *indexer.MutationTicket, 1)}
	f.edit(offer)
	ctx, cancel := context.WithTimeout(f.contexts[1], 5*time.Second)
	defer cancel()
	ticket, receipt := sharedReceiptFreshContractPublication(t, ctx, f, offer)
	select {
	case <-store.applied:
	case <-ctx.Done():
		t.Fatal("the watched edit did not reach the post-apply publication hold")
	}
	require.True(t, ticket.TierState().RelationsAt.IsZero())
	bytes, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(bytes), "func Stream1Edit00() { Target() }")

	for _, sample := range []struct {
		event string
		run   func()
		input map[string]any
	}{
		{"Stop", enrichHook, map[string]any{"hook_event_name": "Stop", "session_id": writer, "cwd": f.root, "stop_hook_active": false}},
		{"PostToolUse", patchHook, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "apply_patch", "cwd": f.root,
			"tool_input": map[string]any{"command": "*** Begin Patch"}, "tool_response": "Done!"}},
	} {
		before := ticket.TierState()
		require.True(t, before.RelationsAt.IsZero())
		raw, elapsed := invokePendingWatcherHook(t, sample.run, sample.input)
		after := ticket.TierState()
		require.Equal(t, before.Generation, after.Generation)
		require.True(t, after.RelationsAt.IsZero(), "the %s hook must return during the same held publication", sample.event)
		// The hook abandons its call when its budget expires, so the frame may
		// reach the daemon after the hook has returned. The held publication
		// keeps that within the same interval.
		select {
		case request := <-detect:
			require.Equal(t, f.root, request.cwd)
			_, err := time.Parse(time.RFC3339Nano, request.deadline)
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatalf("%s did not dispatch detect_changes through the daemon socket", sample.event)
		}
		require.LessOrEqual(t, elapsed, 500*time.Millisecond, "%s briefing exceeded the hook budget", sample.event)
		var reply struct {
			Output struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		require.NoError(t, json.Unmarshal(raw, &reply), "%s", raw)
		require.Equal(t, sample.event, reply.Output.Event)
		if strings.Contains(reply.Output.Context, "relations: pending (detect_changes did not answer within 300ms)") {
			t.Logf("%s hook returned the bounded pending advisory in %v", sample.event, elapsed)
		} else {
			require.Contains(t, reply.Output.Context, "relations: pending (1 files)")
			require.Contains(t, reply.Output.Context, "main.go")
			t.Logf("%s hook returned the labelled pending partial in %v", sample.event, elapsed)
		}
	}

	store.unblock()
	select {
	case <-receipt.done:
		require.False(t, ticket.TierState().RelationsAt.IsZero())
	case <-ctx.Done():
		t.Fatal("the held watched receipt did not finish after release")
	}
}

// invokePendingWatcherHook runs one in-process hook entry point with input
// on stdin and returns its stdout and wall time.
func invokePendingWatcherHook(t *testing.T, run func(), input map[string]any) ([]byte, time.Duration) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.Create(filepath.Join(dir, "stdin"))
	require.NoError(t, err)
	defer in.Close()
	require.NoError(t, json.NewEncoder(in).Encode(input))
	_, err = in.Seek(0, 0)
	require.NoError(t, err)
	out, err := os.Create(filepath.Join(dir, "stdout"))
	require.NoError(t, err)
	defer out.Close()
	originalIn, originalOut := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = originalIn, originalOut }()
	os.Stdin, os.Stdout = in, out
	start := time.Now()
	run()
	elapsed := time.Since(start)
	raw, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	return raw, elapsed
}
