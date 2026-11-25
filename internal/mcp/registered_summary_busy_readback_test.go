package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

func TestRegisteredFullAckSummaryReturnsOwnDefinitionOnStoreDeadline(t *testing.T) {
	for _, scenario := range []string{"unchanged", "changed_bytes_same_stamp", "changed_owner_same_bytes", "second_editor_commit"} {
		t.Run(scenario, func(t *testing.T) {
			testRegisteredFullAckSummaryStoreDeadline(t, scenario)
		})
	}
}

func testRegisteredFullAckSummaryStoreDeadline(t *testing.T, scenario string) {
	t.Helper()
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(root, "main.go")
	const before = "package probe\n\nfunc BeforeSummary() int { return 1 }\n"
	const own = "func OwnSummaryEdit() int { return 2 }"
	const ownID = "main.go::OwnSummaryEdit"
	require.NoError(t, os.WriteFile(path, []byte(before), 0600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "summary.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	idx := newProjectionTestIndexer(t, sql)
	_, err = idx.Index(root)
	require.NoError(t, err)
	backend := &canonicalContextDeadlineStore{Store: sql}
	s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckFull
	t.Cleanup(func() {
		s.DrainBackground()
		s.queryLog.Close()
	})
	const session = "full-ack-summary-store-deadline"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	base := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"summary-store-deadline","version":"1"}}}`)))
	s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	// Embedded full ACK waits for actual SQLite publication. This orders the
	// same committed/retired state without depending on watcher scheduling.
	ack := snapshotRegisteredCall(t, s, base, 2, "edit", snapshotEditArgs(path, "func BeforeSummary() int { return 1 }", own))
	require.False(t, ack.IsError, resultText(ack))
	commit := extractTextResult(t, ack)
	require.Equal(t, "full", commit["ack_mode"])
	require.Equal(t, mutationDiskCommitted, commit["disk_status"])
	require.Equal(t, mutationGraphFresh, commit["graph_status"])
	require.Equal(t, true, commit["reindexed"])
	require.NotEmpty(t, commit["mutation_receipt"])
	require.Positive(t, commit["commit_seq"])
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), commit["content_sha256"])
	require.NotNil(t, sql.GetNode(ownID))
	require.Nil(t, liveEntry(pendingFileProjections, path))
	require.Positive(t, pendingFileProjections.retired.Load(), "retirement must follow the real full-ACK publication")
	matches, err := idx.IndexedContentMatchesContext(t.Context(), "main.go", raw)
	require.NoError(t, err)
	require.True(t, matches)

	output := map[string]any{"format": "json", "max_bytes": 0}
	read := func(id int, op string, target map[string]any) map[string]any {
		t.Helper()
		result := snapshotRegisteredCall(t, s, base, id, "read", map[string]any{"operation": op, "target": target, "output": output})
		require.False(t, result.IsError, resultText(result))
		return extractTextResult(t, result)
	}
	fileTarget := map[string]any{"file": path}
	require.Contains(t, read(3, "source", map[string]any{"symbol": ownID})["source"], own)
	require.Equal(t, string(raw), read(4, "file", fileTarget)["content"])
	firstStamp, err := statDiskStamp(path)
	require.NoError(t, err)
	const second = "func SecondEditorSummary() int { return 3 }"
	const secondID = "main.go::SecondEditorSummary"
	var secondOwner *projectionEntry
	var secondAck *mcplib.CallToolResult
	var secondErr error
	releaseSecond, joinSecond := func() {}, func() {}
	var alterSelection func()
	switch scenario {
	case "changed_bytes_same_stamp":
		changed := bytes.Replace(raw, []byte("OwnSummaryEdit"), []byte("NewSummaryEdit"), 1)
		require.NotEqual(t, raw, changed)
		require.Len(t, changed, len(raw))
		alterSelection = func() {
			require.NoError(t, os.WriteFile(path, changed, 0600))
			require.NoError(t, os.Chtimes(path, firstStamp.info.ModTime(), firstStamp.info.ModTime()))
			changedStamp, stampErr := statDiskStamp(path)
			require.NoError(t, stampErr)
			require.True(t, firstStamp.matches(changedStamp), "content changed without a disk stamp change")
			require.Nil(t, pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)])
		}
	case "changed_owner_same_bytes":
		// Prepare a real parse outside the controlled SQL budget. Only its
		// new registry owner is published at the checked reader boundary.
		next, buildErr := projectFile(t.Context(), idx, path, raw, 100, firstStamp)
		require.NoError(t, buildErr)
		alterSelection = func() {
			require.NoError(t, pendingFileProjections.publish(next))
			require.Same(t, next, pendingFileProjections.snap.Load().entries[next.key])
		}
	case "second_editor_commit":
		// A second editor's registered full-ACK edit keeps the own definition
		// and commits after this summary selected its canonical bytes, while
		// its store read is open. Its publication is held, so its committed
		// owner stays live as the file's pending version.
		published := make(chan indexer.MutationResult)
		s.SetWatcher(mutationTestWatcher{done: published})
		const editorB = "full-ack-summary-second-editor"
		require.NoError(t, s.ConnectSession(editorB, func([]byte) error { return nil }))
		t.Cleanup(func() { s.DisconnectSession(editorB) })
		secondCtx := s.WithClientSession(WithSessionID(t.Context(), editorB), editorB)
		s.NoteSessionToolPolicy(editorB, FacadeSurfaceVersion, "hide")
		require.NotNil(t, s.MCPServer().HandleMessage(secondCtx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"summary-second-editor","version":"1"}}}`)))
		done := make(chan struct{})
		releaseSecond = sync.OnceFunc(func() { close(published) })
		joinSecond = sync.OnceFunc(func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("the second editor's held full ACK did not join")
			}
		})
		t.Cleanup(func() {
			releaseSecond()
			joinSecond()
		})
		ownSeq := uint64(commit["commit_seq"].(float64))
		alterSelection = func() {
			go func() {
				defer close(done)
				secondAck, secondErr = externalVersionRegisteredCall(s, secondCtx, 30, "edit", snapshotEditArgs(path, own, own+"\n\n"+second))
			}()
			for waited := time.Now(); time.Since(waited) < 5*time.Second; time.Sleep(time.Millisecond) {
				if entry := liveEntry(pendingFileProjections, path); entry != nil && entry.commitSeq > ownSeq {
					secondOwner = entry
					return
				}
			}
			t.Error("the second editor did not commit while the summary was in flight")
		}
	}

	// Activate actual SQLite reader holders only in the checked summary bundle,
	// after the request has independently selected its canonical file view.
	var holders sync.WaitGroup
	count := min(runtime.NumCPU(), 4)
	release := make(chan struct{})
	backend.release = sync.OnceFunc(func() { close(release) })
	backend.join = holders.Wait
	t.Cleanup(func() {
		backend.release()
		backend.join()
	})
	holderErrors := make(chan error, count)
	var selected context.Context
	var projectionRemainingAtStore time.Duration
	var holdErr error
	started := 0
	backend.hold = func(ctx context.Context) error {
		selected = ctx
		if deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok {
			projectionRemainingAtStore = time.Until(deadline)
		}
		if alterSelection != nil {
			alterSelection()
		}
		if scenario == "second_editor_commit" {
			// The second editor's real commit runs inside the private store
			// scope and may outlast it. The store stays busy for the rest of
			// the scope, so the read ends on the same store deadline however
			// long the commit took.
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Error("the store scope has no deadline")
			}
			holdErr = ctx.Err()
			return holdErr
		}
		started = count
		entered := make(chan struct{}, count)
		for range count {
			holders.Go(func() {
				holderErrors <- sql.WithFileEditingContextReader(t.Context(), func(context.Context, graph.FileEditingContextRows) error {
					entered <- struct{}{}
					<-release
					return nil
				})
			})
		}
		for range count {
			select {
			case <-entered:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	require.Nil(t, liveEntry(pendingFileProjections, path))
	backend.armed.Store(true)
	allowance := &projectedSourceAllowance{tool: "get_file_summary"}
	budget := projectedReadBudget
	var first map[string]any
	if scenario == "second_editor_commit" {
		// The second editor's real commit runs inside this request in the
		// fixture, so the shared allowance is widened as other lifecycle
		// tests do. Production keeps the original allowance.
		budget = canonicalLifecycleFixtureBudget
		ctx := context.WithValue(base, projectedSourceDeadlineKey{}, time.Now().Add(budget))
		ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
		result := snapshotRegisteredCall(t, s, ctx, 5, "read", map[string]any{"operation": "summary", "target": fileTarget, "output": output})
		require.False(t, result.IsError, resultText(result))
		first = extractTextResult(t, result)
	} else {
		first = read(5, "summary", fileTarget)
	}
	if scenario == "second_editor_commit" {
		require.ErrorIs(t, holdErr, context.DeadlineExceeded)
	} else {
		require.ErrorIs(t, backend.observedErr, context.DeadlineExceeded)
		require.Positive(t, backend.waits, "summary must actually wait for the real SQLite reader pool")
	}
	require.Positive(t, backend.budget)
	require.LessOrEqual(t, backend.budget, 30*time.Millisecond)
	require.Positive(t, projectionRemainingAtStore)
	require.LessOrEqual(t, projectionRemainingAtStore, budget)
	for range started {
		require.NoError(t, <-holderErrors)
	}
	require.NotNil(t, selected)
	require.Empty(t, requestDeferredProjectionFiles(selected))
	require.Empty(t, requestProjectionLayers(selected))
	require.Contains(t, requestStoreBusyFiles(selected), "main.go")
	proof, ok := selected.Value(canonicalSummarySourceKey{}).(*canonicalSummarySource)
	require.True(t, ok)
	require.NotNil(t, proof.registry)
	require.Same(t, idx, proof.indexer)
	require.Equal(t, raw, proof.bytes)
	require.True(t, firstStamp.matches(proof.stamp))
	require.Nil(t, proof.registry.entries[proof.key])
	freshness, valid := first["freshness"].(map[string]any)
	require.True(t, valid)
	if scenario == "second_editor_commit" {
		// The newer owner is a recorded tool commit whose bytes are on disk.
		// The summary answers from it and stays pending. The earlier
		// canonical bytes never answer after that commit.
		require.NotNil(t, secondOwner)
		require.Same(t, secondOwner, liveEntry(pendingFileProjections, path), "the second editor's publication stays held")
		require.Nil(t, sql.GetNode(secondID))
		t.Logf("second editor commit: attempts=%d store_waits=%d hold_err=%v response=%v", allowance.attempts.Load(), backend.waits, holdErr, first)
		require.Equal(t, "pending", freshness["graph_freshness"])
		require.Equal(t, false, freshness["complete"])
		ids := map[string]bool{}
		for _, row := range first["nodes"].([]any) {
			ids[row.(map[string]any)["id"].(string)] = true
		}
		require.True(t, ids[ownID], "the first summary must contain the own acknowledged definition")
		require.True(t, ids[secondID], "the summary answers from the newer committed version")
		require.EqualValues(t, 2, allowance.attempts.Load())
		releaseSecond()
		joinSecond()
		require.NoError(t, secondErr)
		require.NotNil(t, secondAck)
		require.Equal(t, mutationDiskCommitted, extractTextResult(t, secondAck)["disk_status"])
		return
	}
	require.Equal(t, "pending", freshness["graph_freshness"])
	require.Equal(t, "store_busy", freshness["reason"])
	require.Equal(t, false, freshness["complete"])
	require.NotEqual(t, true, freshness["readback_exact"])
	require.EqualValues(t, 0, first["total_edges"])
	require.Empty(t, first["edges"])
	require.Empty(t, first["dependents"])
	rows, valid := first["nodes"].([]any)
	require.True(t, valid)
	if scenario != "unchanged" {
		require.Empty(t, rows, "a changed file version or owner cannot answer from the earlier canonical bytes")
		t.Logf("canonical proof refused after %s; SQLite reader wait and pending/store_busy retained", scenario)
		return
	}
	found := 0
	for _, rawRow := range rows {
		row := rawRow.(map[string]any)
		if row["id"] == ownID {
			require.Equal(t, "main.go", row["file_path"])
			require.EqualValues(t, 3, row["start_line"])
			found++
		}
	}
	t.Logf("own full ACK verified, SQL pool waits=%d, store scope=%s, projection time left at store=%s, no projection deferred, original immediate summary rows=%d", backend.waits, backend.budget, projectionRemainingAtStore, len(rows))
	require.Equal(t, 1, found, "the first registered summary must contain the own acknowledged definition")
}
