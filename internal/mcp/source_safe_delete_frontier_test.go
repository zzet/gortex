package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

func TestPropagateUnknownReferenceNeverInventsForce(t *testing.T) {
	for _, explicitForce := range []bool{false, true} {
		t.Run(map[bool]string{false: "no explicit force", true: "explicit force retained"}[explicitForce], func(t *testing.T) {
			s, root := newSafeDeleteTestServer(t)
			path := filepath.Join(root, "foo.go")
			request := mcplib.CallToolRequest{}
			request.Params.Arguments = map[string]any{"id": path + "::Live", "dry_run": false, "propagate": true, "force": explicitForce, "format": "json"}
			result, err := s.handleSafeDeleteSymbol(t.Context(), request)
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			if explicitForce {
				require.NotContains(t, string(raw), "func Live()")
			} else {
				require.Contains(t, string(raw), "func Live()", "an unlocated reference cannot disappear merely because the propagation plan omitted it")
			}
		})
	}
}

func TestPropagateMustNotRemoveAnotherStatementOnTheSameLine(t *testing.T) {
	s, root := setupMoveInlineRepo(t, map[string]string{
		"del.go":    "package p\nfunc Target() {}\n",
		"caller.go": "package p\nfunc Caller() {\n Target(); Keep()\n}\nfunc Keep() {}\n",
	})
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "del.go::Target", "dry_run": false, "propagate": true, "format": "json"}
	result, err := s.handleSafeDeleteSymbol(t.Context(), request)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := os.ReadFile(filepath.Join(root, "caller.go"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "Target(); Keep()", "a source line with another statement requires manual patching, not whole-line deletion")
	raw, err = os.ReadFile(filepath.Join(root, "del.go"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "func Target()")
}

func TestPropagateLaterFailureRetainsAppliedPatchAndPublication(t *testing.T) {
	s, root := setupMoveInlineRepo(t, map[string]string{
		"del.go": "package p\nfunc Target() {}\n",
		"a.go":   "package p\nfunc First() {\n Target()\n}\n",
		"z.go":   "package p\nfunc Last() {\n Target()\n}\n",
	})
	publications := make(chan indexer.MutationResult)
	defer close(publications)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Nanosecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.mutationPreCommitHook = func(record *mutationCommitRecord) {
		if filepath.Base(record.absPath) == "z.go" {
			cancel()
		}
	}
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "del.go::Target", "dry_run": false, "propagate": true, "format": "json"}
	result, err := s.handleSafeDeleteSymbol(ctx, request)
	require.NoError(t, err)
	first := filepath.Join(root, "a.go")
	raw, err := os.ReadFile(first)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "Target()", "prove the first physical caller patch committed before the second refused")
	require.Contains(t, toolResultText(result), "patches_applied", "the error path must retain actual completed source effects")
	require.Contains(t, toolResultText(result), "a.go")
	select {
	case admitted := <-watcher.announced:
		require.Equal(t, []string{first}, admitted)
	default:
		t.Fatal("a later failure abandoned publication of the first physical caller patch")
	}
	for _, name := range []string{"z.go", "del.go"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		require.Contains(t, string(raw), "Target()")
	}
}

func TestPropagateKnownAndUnknownReferenceKeepsUnknownAuthority(t *testing.T) {
	s, g, root := setupDeleteServer(t)
	g.AddEdge(&graph.Edge{From: "caller.go::Caller", To: "del.go::Target", Kind: graph.EdgeReferences, FilePath: "caller.go"})
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "del.go::Target", "dry_run": false, "propagate": true, "format": "json"}
	result, err := s.handleSafeDeleteSymbol(t.Context(), request)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := os.ReadFile(filepath.Join(root, "del.go"))
	require.NoError(t, err)
	require.True(t, strings.Contains(string(raw), "func Target()"), "removing one exact call cannot certify an unknown reference as removed")
}

func TestSafeDeleteRevalidatesAuthorityAfterWholeFrontierLock(t *testing.T) {
	for _, mode := range []string{"published reference", "unpublished reference", "cascade member gains caller"} {
		t.Run(mode, func(t *testing.T) {
			rootBody := "package p\nfunc Root() {}\n"
			if mode == "cascade member gains caller" {
				rootBody = "package p\nfunc Root() { Child() }\n"
			}
			s, root := setupMoveInlineRepo(t, map[string]string{
				"root.go": rootBody, "child.go": "package p\nfunc Child() {}\n", "outside.go": "package p\nfunc Outside() {}\n",
			})
			locked := filepath.Join(root, "root.go")
			if mode == "cascade member gains caller" {
				locked = filepath.Join(root, "child.go")
			}
			_, release, err := acquireMutationPath(t.Context(), locked)
			require.NoError(t, err)
			var once sync.Once
			unlock := func() { once.Do(release) }
			defer unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := mcplib.CallToolRequest{}
			request.Params.Arguments = map[string]any{"id": "root.go::Root", "dry_run": false, "format": "json"}
			if mode == "cascade member gains caller" {
				request.Params.Arguments.(map[string]any)["cascade"] = "apply"
			}
			var publication chan indexer.MutationResult
			if mode == "unpublished reference" {
				publication = make(chan indexer.MutationResult, 1)
				s.SetWatcher(mutationTestWatcher{done: publication})
			}
			type response struct {
				result *mcplib.CallToolResult
				err    error
			}
			finished := make(chan response, 1)
			go func() { result, err := s.handleSafeDeleteSymbol(ctx, request); finished <- response{result, err} }()
			joined := false
			defer func() {
				cancel()
				unlock()
				if !joined {
					select {
					case <-finished:
					case <-time.After(3 * time.Second):
						t.Error("owned delete did not join during cleanup")
					}
				}
			}()
			waitForMutationRefs(t, locked, 2)
			callee := "Root"
			if mode == "cascade member gains caller" {
				callee = "Child"
			}
			outside := filepath.Join(root, "outside.go")
			writeCtx, releaseWriter, err := acquireMutationPath(t.Context(), outside)
			require.NoError(t, err)
			defer releaseWriter()
			_, err = s.commitFileMutation(writeCtx, "write_file", "", "", "outside.go", outside, []byte("package p\nfunc Outside() { "+callee+"() }\n"), 0o644)
			require.NoError(t, err, "a real independent source commit lands while the delete is queued")
			var receipt *mutationReceipt
			if mode == "unpublished reference" {
				admission := s.admitSourceMutations(writeCtx, outside)
				receipt = admission.receipt
				require.NotNil(t, receipt)
			} else {
				s.graph.AddEdge(&graph.Edge{From: "outside.go::Outside", To: strings.ToLower(callee) + ".go::" + callee, Kind: graph.EdgeCalls, FilePath: "outside.go", Line: 2})
			}
			releaseWriter()
			unlock()
			var actual response
			select {
			case actual = <-finished:
				joined = true
			case <-time.After(3 * time.Second):
				t.Fatal("delete did not return after the path frontier was released")
			}
			if receipt != nil {
				publication <- indexer.MutationResult{RequestedGeneration: receipt.generation, AppliedGeneration: receipt.generation, Reindexed: true}
				close(publication)
				<-receipt.done
			}
			require.NoError(t, actual.err)
			require.False(t, actual.result.IsError, "%v", actual.result.Content)
			raw, err := os.ReadFile(filepath.Join(root, "root.go"))
			require.NoError(t, err)
			require.Contains(t, string(raw), "func Root()", "new reference/closure authority must be checked after the final union admission, before any delete")
			if mode == "cascade member gains caller" {
				raw, err = os.ReadFile(filepath.Join(root, "child.go"))
				require.NoError(t, err)
				require.Contains(t, string(raw), "func Child()", "a newly referenced cascade member must not be deleted under an obsolete closure")
			}
			require.Contains(t, toolResultText(actual.result), "source_authority_changed")
		})
	}
}

func TestPropagateDeleteAdmitsOnlyTheFinalCompleteSourceFrontier(t *testing.T) {
	for _, sameFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "two files", true: "same physical file"}[sameFile], func(t *testing.T) {
			files := map[string]string{"del.go": "package p\nfunc Target() {}\n", "caller.go": "package p\nfunc Caller() {\n Target()\n}\n"}
			if sameFile {
				files = map[string]string{"del.go": "package p\nfunc Target() {}\nfunc Caller() {\n Target()\n}\n"}
			}
			s, root := setupMoveInlineRepo(t, files)
			publications := make(chan indexer.MutationResult, len(files))
			watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
			s.SetWatcher(watcher)
			s.mutationReindexWait = time.Minute
			paths := []string{filepath.Join(root, "del.go")}
			if !sameFile {
				paths = append(paths, filepath.Join(root, "caller.go"))
			}
			waiting := make(chan error, 1)
			s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
				check, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				_, release, err := acquireMutationPaths(check, paths)
				if err == nil {
					release()
				}
				waiting <- err
				return true
			}
			request := mcplib.CallToolRequest{}
			request.Params.Arguments = map[string]any{"id": "del.go::Target", "dry_run": false, "propagate": true, "format": "json"}
			ctx, note := withMutationCommitNote(t.Context())
			ctx, cancel := context.WithCancel(ctx)
			finished := make(chan struct{})
			complete := sync.OnceFunc(func() {
				for range len(files) {
					publications <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
				}
				close(publications)
			})
			var result *mcplib.CallToolResult
			var resultErr error
			go func() { defer close(finished); result, resultErr = s.handleSafeDeleteSymbol(ctx, request) }()
			defer func() {
				cancel()
				complete()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Error("owned propagate/delete did not join")
				}
			}()
			select {
			case admitted := <-watcher.announced:
				require.ElementsMatch(t, paths, admitted)
			case <-finished:
				t.Fatalf("propagate/delete returned before admitting its final frontier: %v %v", resultErr, result.Content)
			case <-time.After(3 * time.Second):
				t.Fatal("propagate/delete never admitted its final frontier")
			}
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "Target()", "both caller removal and definition delete precede the first admission")
			}
			select {
			case err := <-waiting:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("source locks were not released before publication wait")
			}
			complete()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("published operation did not return")
			}
			require.NoError(t, resultErr)
			require.False(t, result.IsError, "%v", result.Content)
			snapshots := note.verdict().Snapshots
			require.Len(t, snapshots, 2, "the caller patch and the definition deletion are real physical writes")
			if sameFile {
				require.EqualValues(t, 1, watcher.points.Load())
				require.Zero(t, watcher.sets.Load())
				recorded := 0
				for _, snapshot := range snapshots {
					if snapshot.GraphRecorded {
						recorded++
					}
				}
				require.Equal(t, 1, recorded, "only final bytes may receive publication success; the intermediate physical receipt stays historical")
			} else {
				require.EqualValues(t, 1, watcher.sets.Load())
				require.Zero(t, watcher.points.Load())
			}
		})
	}
}

func TestPropagateChangedSourcePreviewNeverAuthorizesApply(t *testing.T) {
	s, root := newRelationFixture(t)
	caller := writeUnindexed(t, root, "caller.go", "package p\n\nfunc Caller() {\n\tTarget()\n}\n\nfunc More() {}\n")
	pendingFreshnessReceipt(s, "preview-caller", "", caller, 1)
	target := filepath.Join(root, "target.go")
	beforeCaller, err := os.ReadFile(caller)
	require.NoError(t, err)
	beforeTarget, err := os.ReadFile(target)
	require.NoError(t, err)
	id := targetID(t, s)
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": id, "propagate": true, "format": "json"}
	ctx, note := withMutationCommitNote(t.Context())
	preview, err := s.handleSafeDeleteSymbol(ctx, request)
	require.NoError(t, err)
	require.False(t, preview.IsError, "%v", preview.Content)
	payload := extractTextResult(t, preview)
	require.Equal(t, "propagation_plan", payload["status"])
	require.EqualValues(t, 0, payload["manual"])
	require.Equal(t, true, payload["source_revalidation_required"])
	require.Contains(t, payload["hint"], "not write authorization")
	require.Empty(t, note.verdict().Snapshots)
	// A private preview row is also not an authorization token for the writer.
	plan := s.buildPropagationPreview(ctx, s.readerFor(ctx), s.readerFor(ctx).GetNode(id))
	applied, failed, err := s.applyRemoveLinePatches(ctx, plan)
	require.NoError(t, err)
	require.Empty(t, applied)
	require.Len(t, failed, 1)
	require.Equal(t, "source_unverified", failed[0].Reason)
	require.Empty(t, note.verdict().Snapshots)
	request.Params.Arguments.(map[string]any)["dry_run"] = false
	result, err := s.handleSafeDeleteSymbol(ctx, request)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.Equal(t, "propagation_blocked_manual", extractTextResult(t, result)["status"])
	require.Empty(t, note.verdict().Snapshots)
	afterCaller, err := os.ReadFile(caller)
	require.NoError(t, err)
	afterTarget, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, beforeCaller, afterCaller)
	require.Equal(t, beforeTarget, afterTarget)
}
