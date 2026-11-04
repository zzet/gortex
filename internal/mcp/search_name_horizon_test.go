package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

func TestRegisteredNameOnlySearchPredicatePrecedesNativeHorizon(t *testing.T) {
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	const prefix = "name-search-fixture"
	const rel = "victim.go"
	const oldName = "QueryMarker001Before"
	const newName = "QueryMarker001"
	const requestLimit = 10000
	const rawLaneThreshold = (requestLimit + 5) * 2
	oldID, newID := prefix+"/"+rel+"::"+oldName, prefix+"/"+rel+"::"+newName
	root := t.TempDir()
	path := filepath.Join(root, rel)
	var victim strings.Builder
	victim.WriteString("package probe\n\nfunc QueryFixtureRoot() int { return 0 }\n\n")
	var victimCorpus strings.Builder
	victimCorpus.WriteString("package probe\n\n")
	for ordinal := range 1000 {
		destination := &victimCorpus
		if ordinal == 1 || ordinal == 2 {
			destination = &victim
		}
		fmt.Fprintf(destination, "func QueryMarker%03dBefore() int { return %d }\n\n", ordinal, ordinal)
	}
	require.NoError(t, os.WriteFile(path, []byte(victim.String()), 0o600))
	// Keep the real native corpus width without making fixture admission
	// depend on projecting 1000 untouched declarations inside the build cap.
	require.NoError(t, os.WriteFile(filepath.Join(root, "victim_corpus.go"), []byte(victimCorpus.String()), 0o600))
	neighborDir := filepath.Join(root, "neighbors")
	require.NoError(t, os.Mkdir(neighborDir, 0o700))
	for file := range 21 {
		var neighbor strings.Builder
		neighbor.WriteString("package neighbors\n\n")
		var neighborCorpus strings.Builder
		neighborCorpus.WriteString("package neighbors\n\n")
		for ordinal := range 1000 {
			// Native FTS shares the CamelCase Query token, while the actual
			// literal name predicate must reject every one of these names.
			destination := &neighbor
			if file == 0 && ordinal != 0 {
				destination = &neighborCorpus
			}
			fmt.Fprintf(destination, "func QueryMarkerReplica%02d%03dBefore() int { return %d }\n", file, ordinal, ordinal)
		}
		require.NoError(t, os.WriteFile(filepath.Join(neighborDir, fmt.Sprintf("names%02d.go", file)), []byte(neighbor.String()), 0o600))
		if file == 0 {
			require.NoError(t, os.WriteFile(filepath.Join(neighborDir, "names00_corpus.go"), []byte(neighborCorpus.String()), 0o600))
		}
	}
	positions := token.NewFileSet()
	parsed, err := parser.ParseFile(positions, path, victim.String(), 0)
	require.NoError(t, err)
	expectedLine := 0
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == oldName {
			expectedLine = positions.Position(fn.Pos()).Line
		}
	}
	require.Positive(t, expectedLine)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "name-horizon.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newProjectionTestIndexer(t, store)
	idx.SetRepoPrefix(prefix)
	_, err = idx.Index(root)
	require.NoError(t, err)
	ftsCount, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, 22001, ftsCount, "splitting fixture files must preserve the actual native corpus")
	require.Greater(t, ftsCount, rawLaneThreshold)
	t.Logf("actual_native_FTS_count=%d raw_lane_threshold=%d request_limit=%d", ftsCount, rawLaneThreshold, requestLimit)
	require.NotNil(t, store.GetNode(oldID))
	engine := query.NewEngine(store)
	engine.SetSearchProvider(idx.Search)
	s := NewServer(engine, store, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	t.Cleanup(func() { close(pending); s.DrainBackground() })
	const session = "name-horizon-registered"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	type selectedRead struct {
		query      string
		sealed     bool
		paths      []string
		namesReady bool
	}
	var selectedMu sync.Mutex
	var selections []selectedRead
	legacy, ok := s.facades.legacy("search_symbols")
	require.True(t, ok)
	s.facades.capture(legacy.tool, func(callCtx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		result, callErr := legacy.handler(callCtx, req)
		observed := selectedRead{query: req.GetString("query", ""), namesReady: true}
		if capture, _ := callCtx.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture); capture != nil {
			capture.mu.RLock()
			observed.sealed = !capture.disabled && capture.facts != nil && len(capture.entries) > 0
			for _, entry := range capture.entries {
				observed.paths = append(observed.paths, entry.graphPath)
				observed.namesReady = observed.namesReady && entry.names != nil && entry.names.Count() > 0
			}
			capture.mu.RUnlock()
		}
		selectedMu.Lock()
		selections = append(selections, observed)
		selectedMu.Unlock()
		return result, callErr
	})
	args := func(name string, pathScoped bool) map[string]any {
		options := map[string]any{"repo": prefix, "query_class": "symbol", "expand": "off", "limit": requestLimit, "max_per_file": 0, "max_bytes": 0}
		if pathScoped {
			options["path"] = rel
		}
		return map[string]any{"operation": "symbols", "query": "name:" + name, "options": options, "output": map[string]any{"format": "json", "max_bytes": 0}}
	}
	logObservation := func(label string, o negativeQueryObservation) {
		data, marshalErr := json.Marshal(o)
		require.NoError(t, marshalErr)
		t.Logf("%s observation=%s", label, data)
	}
	before := negativeQueryRegisteredCall(s, ctx, 10, "search", args(oldName, true), oldID, oldName)
	logObservation("before_present", before)
	require.True(t, before.present(), "the endpoint must see the real indexed target before any mutation")
	retainedACK := negativeQueryRegisteredCall(s, ctx, 11, "edit", snapshotEditArgs(path, "QueryMarker002Before", "QueryMarker002"), oldID, oldName)
	retained := negativeQueryRegisteredCall(s, ctx, 12, "search", args(oldName, true), oldID, oldName)
	logObservation("retained_after_local_ACK", retained)
	logObservation("retained_LOCAL_ACK", retainedACK)
	renamedACK := negativeQueryRegisteredCall(s, ctx, 13, "edit", snapshotEditArgs(path, oldName, newName), oldID, oldName)
	logObservation("rename_LOCAL_ACK", renamedACK)
	oldOwner := liveEntry(pendingFileProjections, path)
	require.NotNil(t, oldOwner)
	oldFacts, err := s.bindPendingSearch(ctx)
	require.NoError(t, err)
	require.NoError(t, validateRequestProjections(oldFacts))
	retirement := s.canonicalProjectionRetirement(path)
	require.NotNil(t, retirement)
	require.NoError(t, idx.IndexFile(path))
	retirement.run()
	require.True(t, oldOwner.checkedRetired.Load())
	require.Nil(t, liveEntry(pendingFileProjections, path))
	require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
	require.Nil(t, store.GetNode(oldID), "the native exact-name tier must really be absent after canonical publication")
	require.NotNil(t, store.GetNode(newID))
	// Establish the real broad pool after retirement and before the final
	// ACK. No SQL or filesystem observation is inserted into its read burst.
	broad, err := store.SearchSymbolsRepoScopedContext(t.Context(), oldName, []string{prefix}, rawLaneThreshold+1)
	require.NoError(t, err)
	t.Logf("canonical_old_absent=true canonical_new_present=true checked_retired=true old_facts_invalid=true actual_full_needle_FTS_hits=%d raw_lane_threshold=%d", len(broad), rawLaneThreshold)
	require.Greater(t, len(broad), rawLaneThreshold, "real native FTS token neighbors must actually exceed the unchanged request horizon after the old exact-name tier is gone")
	neighborPath := filepath.Join(neighborDir, "names00.go")
	layeredACK := negativeQueryRegisteredCall(s, ctx, 14, "edit", snapshotEditArgs(neighborPath, "QueryMarkerReplica00000Before", "QueryMarkerReplica00000"), newID, newName)
	positive := negativeQueryRegisteredCall(s, ctx, 15, "search", args(newName, false), newID, newName)
	absent := negativeQueryRegisteredCall(s, ctx, 16, "search", args(oldName, true), oldID, oldName)
	logObservation("new_positive_after_rename_ACK", positive)
	logObservation("old_negative_after_rename_ACK", absent)
	logObservation("unrelated_layer_LOCAL_ACK", layeredACK)
	// Physical observations follow the immediate burst and cannot give the
	// watcher time to manufacture a successful immediate response.
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	afterPositions := token.NewFileSet()
	afterAST, err := parser.ParseFile(afterPositions, path, after, 0)
	require.NoError(t, err)
	oldCount, newCount := 0, 0
	for _, decl := range afterAST.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Name.Name == oldName {
				oldCount++
			}
			if fn.Name.Name == newName {
				newCount++
				require.Equal(t, expectedLine, afterPositions.Position(fn.Pos()).Line)
			}
		}
	}
	for _, ack := range []negativeQueryObservation{retainedACK, renamedACK, layeredACK} {
		require.Empty(t, ack.ProtocolError)
		require.NotNil(t, ack.result)
		require.False(t, ack.result.IsError, resultText(ack.result))
		require.Equal(t, "local", ack.Text.body["ack_mode"])
		require.Equal(t, true, ack.Text.body["readback_exact"])
	}
	afterSHA := fmt.Sprintf("%x", sha256.Sum256(after))
	require.Equal(t, afterSHA, renamedACK.Text.body["content_sha256"])
	require.Zero(t, oldCount)
	require.Equal(t, 1, newCount)
	require.Nil(t, store.GetNode(oldID))
	require.NotNil(t, store.GetNode(newID))
	require.Nil(t, liveEntry(pendingFileProjections, path))
	neighborBytes, err := os.ReadFile(neighborPath)
	require.NoError(t, err)
	neighborSHA := fmt.Sprintf("%x", sha256.Sum256(neighborBytes))
	require.Equal(t, neighborSHA, layeredACK.Text.body["content_sha256"])
	owner := liveEntry(pendingFileProjections, neighborPath)
	require.NotNil(t, owner)
	require.Positive(t, owner.sourceCommitSeq)
	postBurst, err := s.bindPendingSearch(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, requestProjectionLayers(postBurst), "the unrelated actual ACK must keep the selected view layered")
	selectedMu.Lock()
	actualSelections := append([]selectedRead(nil), selections...)
	selectedMu.Unlock()
	require.Len(t, actualSelections, 4)
	for _, actual := range actualSelections[2:] {
		require.True(t, actual.sealed, "the actual registered handler must seal its selected neighbor layer")
		require.True(t, actual.namesReady)
		require.Equal(t, []string{prefix + "/neighbors/names00.go"}, actual.paths)
		t.Logf("actual_registered_query=%s capture_sealed=%t selected_pending_paths=%v names_ready=%t path_fastlane_ineligible=true", actual.query, actual.sealed, actual.paths, actual.namesReady)
	}
	t.Logf("physical_after_sha256=%s neighbor_after_sha256=%s old_AST_count=%d new_AST_count=%d canonical_old_absent=true victim_retired=true unrelated_layer_present=true source_commit_seq=%d", afterSHA, neighborSHA, oldCount, newCount, owner.sourceCommitSeq)
	require.True(t, retained.present(), "an unrelated real LOCAL ACK cannot make the still-present declaration disappear")
	require.True(t, positive.present(), "the actual immediate no-path new-name lookup must return the committed declaration")
	checkPositive := func(channel negativeQueryChannel) {
		count := 0
		for _, raw := range channel.body["results"].([]any) {
			row := raw.(map[string]any)
			if row["id"] == newID || row["name"] == newName {
				count++
				require.Equal(t, newID, row["id"])
				require.Equal(t, newName, row["name"])
				require.Equal(t, prefix+"/"+rel, row["file_path"])
				require.Equal(t, float64(expectedLine), row["start_line"])
			}
		}
		require.Equal(t, 1, count)
	}
	checkPositive(positive.Text)
	if positive.Structured.Present {
		checkPositive(positive.Structured)
	}
	t.Logf("strict_absence_state=%s declared_rows=%d total=%v truncated=%v cursor=%t channels_agree=%t", absent.afterState(), absent.Text.Rows, absent.Text.Total, absent.Text.Truncated, absent.Text.HasCursor, absent.ChannelsAgree)
	require.Equal(t, "COMPLETE_ABSENCE", absent.afterState(), "zero rows with an unexhausted candidate horizon or cursor never prove old-name absence")
}
