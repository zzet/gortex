package mcp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

func TestScaffoldAdmitsOneFinalFileBeforeUnlockedPublicationWait(t *testing.T) {
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	publications := make(chan indexer.MutationResult, 1)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Minute
	waiting := make(chan error, 1)
	s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, release, err := acquireMutationPath(ctx, path)
		if err == nil {
			release()
		}
		waiting <- err
		return true
	}
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "main.go::helper", "new_name": "ClonedHelper", "dry_run": false, "format": "json"}
	ctx, note := withMutationCommitNote(t.Context())
	ctx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	complete := sync.OnceFunc(func() {
		publications <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
		close(publications)
	})
	var result *mcplib.CallToolResult
	var resultErr error
	parses := parser.ParseCount()
	go func() { defer close(finished); result, resultErr = s.handleScaffold(ctx, request) }()
	defer func() {
		cancel()
		complete()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("owned scaffold did not join")
		}
	}()
	select {
	case admitted := <-watcher.announced:
		require.Equal(t, []string{path}, admitted)
	case <-finished:
		t.Fatalf("scaffold returned without admitting its physical source: %v %v", resultErr, result.Content)
	case <-time.After(3 * time.Second):
		t.Fatal("scaffold never admitted its physical source")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "func ClonedHelper() {}")
	require.EqualValues(t, 1, watcher.points.Load(), "multiple scaffold edits to one file have one final publication")
	require.Len(t, note.verdict().Snapshots, 1, "same-file insertions are one physical candidate/write")
	select {
	case err := <-waiting:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("scaffold did not return its path lock before waiting")
	}
	complete()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("published scaffold did not return")
	}
	require.NoError(t, resultErr)
	require.False(t, result.IsError, "%v", result.Content)
	require.EqualValues(t, 1, parser.ParseCount()-parses, "one full clean candidate parse per physical file; queued publisher is a no-parse fixture")
}

func TestScaffoldRefusesExampleRemovedWhileWaitingForSourceLock(t *testing.T) {
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	writerCtx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "main.go::helper", "new_name": "ClonedHelper", "dry_run": false, "format": "json"}
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var result *mcplib.CallToolResult
	var resultErr error
	go func() { defer close(finished); result, resultErr = s.handleScaffold(ctx, request) }()
	defer func() {
		cancel()
		unlock()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("owned queued scaffold did not join")
		}
	}()
	waitForMutationRefs(t, path, 2)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	current := []byte(strings.ReplaceAll(string(before), "func helper() {}", "func RemovedAlternative() {}"))
	_, err = s.commitFileMutation(writerCtx, "write_file", "", "", "main.go", path, current, 0o644)
	require.NoError(t, err)
	unlock()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("queued scaffold did not return")
	}
	require.NoError(t, resultErr)
	require.True(t, result.IsError, "a stale outside-lock template cannot resurrect a removed example")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, current, after)
}

func TestScaffoldUsesTheRequestedReaderInsteadOfCanonicalGraph(t *testing.T) {
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw = append(raw, []byte("\nfunc SelectedOnly() int { return 7 }\n")...)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	projection, err := s.indexer.ProjectSource(t.Context(), path, raw)
	require.NoError(t, err)
	selected := graph.New()
	selected.AddBatch(projection.Nodes, projection.Edges)
	require.NotNil(t, selected.GetNode("main.go::SelectedOnly"))
	require.Nil(t, s.graph.GetNode("main.go::SelectedOnly"))
	ctx := withRequestView(t.Context(), &requestView{reader: selected, viewRoot: root})
	s.SetWatcher(mutationTestWatcher{})
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "main.go::SelectedOnly", "new_name": "ClonedSelected", "dry_run": false, "format": "json"}
	result, err := s.handleScaffold(ctx, request)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(after), "func ClonedSelected() int { return 7 }")
	require.Nil(t, s.graph.GetNode("main.go::ClonedSelected"), "the selected reader cannot become a canonical graph write")
}

func TestOverlayDiskMergeAdmitsWholeFrontierBeforeUnlockedWait(t *testing.T) {
	s, session, _, first, second, ctx := branchBootstrap(t)
	_, _ = invokeTool(t, s, ctx, "overlay_fork", map[string]any{"name": "queued-writer", "activate": true})
	cohort := map[string]string{first: "package main\nfunc Target() {}\nfunc AddedTarget() {}\n", second: "package main\nfunc Caller() { Target() }\nfunc AddedCaller() {}\n"}
	for path, body := range cohort {
		require.NoError(t, s.OverlayManager().Push(session, daemon.OverlayFile{Path: path, Content: body}, nil))
	}
	publications := make(chan indexer.MutationResult, 2)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Minute
	paths := []string{first, second}
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
	request.Params.Arguments = map[string]any{"from": "queued-writer", "to_disk": true, "format": "json"}
	ctx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	complete := sync.OnceFunc(func() {
		for range 2 {
			publications <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
		}
		close(publications)
	})
	var result *mcplib.CallToolResult
	var resultErr error
	go func() { defer close(finished); result, resultErr = s.handleOverlayMerge(ctx, request) }()
	defer func() {
		cancel()
		complete()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("owned overlay merge did not join")
		}
	}()
	select {
	case admitted := <-watcher.announced:
		require.ElementsMatch(t, paths, admitted)
	case <-finished:
		t.Fatalf("overlay merge returned without watcher frontier admission: %v %v", resultErr, result.Content)
	case <-time.After(3 * time.Second):
		t.Fatal("overlay merge never admitted its source frontier")
	}
	for path, body := range cohort {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
	}
	select {
	case err := <-waiting:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("overlay merge kept source locks during passive wait")
	}
	managerRead := make(chan error, 1)
	go func() { _, err := s.OverlayManager().Branches(session); managerRead <- err }()
	select {
	case err := <-managerRead:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("overlay manager mutex was retained across publication wait")
	}
	require.EqualValues(t, 1, watcher.sets.Load())
	require.Zero(t, watcher.points.Load())
	complete()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("published overlay merge did not return")
	}
	require.NoError(t, resultErr)
	require.False(t, result.IsError, "%v", result.Content)
}

func TestOverlayDiskMergeRefusesChangedBranchAfterPathWait(t *testing.T) {
	for _, change := range []string{"content", "membership"} {
		t.Run(change, func(t *testing.T) {
			s, session, _, first, second, ctx := branchBootstrap(t)
			_, _ = invokeTool(t, s, ctx, "overlay_fork", map[string]any{"name": "changing", "activate": true})
			require.NoError(t, s.OverlayManager().Push(session, daemon.OverlayFile{Path: first, Content: "package main\nfunc Replacement() {}\n"}, nil))
			before, err := os.ReadFile(first)
			require.NoError(t, err)
			_, release, err := acquireMutationPath(t.Context(), first)
			require.NoError(t, err)
			unlock := sync.OnceFunc(release)
			defer unlock()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			request := mcplib.CallToolRequest{}
			request.Params.Arguments = map[string]any{"from": "changing", "to_disk": true}
			type response struct {
				result *mcplib.CallToolResult
				err    error
			}
			done := make(chan response, 1)
			go func() { result, err := s.handleOverlayMerge(ctx, request); done <- response{result, err} }()
			defer func() {
				cancel()
				unlock()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("owned branch merge did not join")
				}
			}()
			waitForMutationRefs(t, first, 2)
			path := first
			if change == "membership" {
				path = second
			}
			changed := make(chan error, 1)
			go func() {
				changed <- s.OverlayManager().Push(session, daemon.OverlayFile{Path: path, Content: "package main\nfunc NewerOverlay() {}\n"}, nil)
			}()
			select {
			case err := <-changed:
				require.NoError(t, err)
			case <-time.After(time.Second):
				unlock()
				require.NoError(t, <-changed)
				t.Fatal("branch mutex held while acquiring disk paths")
			}
			unlock()
			select {
			case result := <-done:
				done <- result // leave the owned join receipt for cleanup
				require.NoError(t, result.err)
				require.True(t, result.result.IsError, "%v", result.result.Content)
			case <-time.After(3 * time.Second):
				t.Fatal("changed branch merge did not refuse")
			}
			after, err := os.ReadFile(first)
			require.NoError(t, err)
			require.Equal(t, before, after, "cohort mismatch must refuse before the first physical write")
			files, err := s.OverlayManager().Files(session)
			require.NoError(t, err)
			require.Contains(t, files, path, "refusal must retain the newer source branch")
		})
	}
}

func TestOverlayDiskMergeLaterCancellationStillAdmitsCommittedSubset(t *testing.T) {
	s, session, _, first, second, ctx := branchBootstrap(t)
	_, _ = invokeTool(t, s, ctx, "overlay_fork", map[string]any{"name": "partial", "activate": true})
	paths := []string{first, second}
	slices.Sort(paths)
	originalSecond, err := os.ReadFile(paths[1])
	require.NoError(t, err)
	body := "package main\nfunc PartialReplacement() {}\n"
	for _, path := range paths {
		require.NoError(t, s.OverlayManager().Push(session, daemon.OverlayFile{Path: path, Content: body}, nil))
	}
	publications := make(chan indexer.MutationResult)
	defer close(publications)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	ctx, note := withMutationCommitNote(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mutationPreCommitHook = func(record *mutationCommitRecord) {
		if record.absPath == paths[1] {
			cancel()
		}
	}
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"from": "partial", "to_disk": true}
	result, err := s.handleOverlayMerge(ctx, request)
	require.NoError(t, err)
	require.True(t, result.IsError, "%v", result.Content)
	actualFirst, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	require.Equal(t, body, string(actualFirst))
	actualSecond, err := os.ReadFile(paths[1])
	require.NoError(t, err)
	require.Equal(t, originalSecond, actualSecond)
	select {
	case admitted := <-watcher.announced:
		require.Equal(t, []string{paths[0]}, admitted)
	default:
		t.Fatal("later cancellation lost the first physical file's publication admission")
	}
	var committed int
	for _, snapshot := range note.verdict().Snapshots {
		if snapshot.DiskStatus == mutationDiskCommitted {
			committed++
			require.True(t, snapshot.GraphRecorded, "the committed subset retains its publication outcome")
		}
	}
	require.Equal(t, 1, committed)
}
