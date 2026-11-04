package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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

type negativeQueryChannel struct {
	Present        bool `json:"present"`
	Null           bool `json:"null"`
	Valid          bool `json:"valid_results_shape"`
	Rows           int  `json:"rows"`
	Total          any  `json:"total"`
	Truncated      any  `json:"truncated"`
	BudgetCut      any  `json:"budget_cut"`
	LimitCut       any  `json:"limit_cut"`
	HasCursor      bool `json:"has_cursor"`
	FoundExact     bool `json:"found_exact_old_id_and_name"`
	HasOldIdentity bool `json:"has_old_id_or_name"`
	Complete       bool `json:"complete"`
	body           map[string]any
}

func negativeQueryScalar(v any) any {
	switch value := v.(type) {
	case nil, bool, float64:
		return value
	case string:
		if len(value) <= 128 {
			return value
		}
		return "oversize string field"
	default:
		return fmt.Sprintf("unexpected scalar type %T", v)
	}
}

func negativeQuerySummarize(raw json.RawMessage, oldID, oldName string) negativeQueryChannel {
	out := negativeQueryChannel{Present: len(raw) > 0, Null: bytes.Equal(raw, []byte("null"))}
	if !out.Present || out.Null || json.Unmarshal(raw, &out.body) != nil || out.body == nil {
		return out
	}
	rows, valid := out.body["results"].([]any)
	if !valid {
		return out
	}
	out.Valid = true
	out.Rows = len(rows)
	out.Total = negativeQueryScalar(out.body["total"])
	out.Truncated = negativeQueryScalar(out.body["truncated"])
	out.BudgetCut = negativeQueryScalar(out.body["_truncated_by_budget"])
	out.LimitCut = negativeQueryScalar(out.body["_truncated_by_limit"])
	cursor := out.body["next_cursor"]
	out.HasCursor = cursor != nil && cursor != ""
	total, countValid := out.Total.(float64)
	out.Complete = countValid && total == float64(len(rows)) && out.Truncated == false && !out.HasCursor
	for _, key := range []string{"_truncated_by_budget", "_truncated_by_limit"} {
		if v, present := out.body[key]; present && v != false {
			out.Complete = false
		}
	}
	if v, present := out.body["count_is_exact"]; present && v != true {
		out.Complete = false
	}
	for _, rawRow := range rows {
		row, ok := rawRow.(map[string]any)
		if !ok {
			out.Valid = false
			out.Complete = false
			continue
		}
		id, idOK := row["id"].(string)
		name, nameOK := row["name"].(string)
		if !idOK || id == "" || !nameOK || name == "" {
			out.Valid = false
			out.Complete = false
		}
		out.FoundExact = out.FoundExact || id == oldID && name == oldName
		out.HasOldIdentity = out.HasOldIdentity || id == oldID || name == oldName
	}
	return out
}

// The byte witnesses are registered in-process SDK JSON, not a reconstructed
// native socket frame. Diagnostics bound the printed prefix only; semantic
// checks inspect both actual returned channels in memory without trimming.
type negativeQueryObservation struct {
	Request           json.RawMessage      `json:"registered_request_json"`
	ReplyBytes        int                  `json:"registered_reply_json_bytes"`
	ReplySHA          string               `json:"registered_reply_json_sha256"`
	ReplyPrefixCut    bool                 `json:"diagnostic_prefix_cut"`
	ProtocolError     string               `json:"protocol_error"`
	IsError           json.RawMessage      `json:"actual_isError_field,omitempty"`
	IsErrorPresent    bool                 `json:"isError_present"`
	IsErrorValueValid bool                 `json:"isError_value_valid"`
	TextBlocks        int                  `json:"text_blocks"`
	ExpectedID        string               `json:"expected_target_id"`
	ExpectedName      string               `json:"expected_target_name"`
	Text              negativeQueryChannel `json:"content_json"`
	Structured        negativeQueryChannel `json:"structuredContent"`
	ChannelsAgree     bool                 `json:"channels_agree"`
	FullValueChecked  bool                 `json:"full_returned_value_checked_in_memory"`
	ReplyPrefix       string               `json:"registered_reply_prefix_base64"`
	result            *mcplib.CallToolResult
}

func negativeQueryRegisteredCall(s *Server, ctx context.Context, id int, name string, args map[string]any, oldID, oldName string) negativeQueryObservation {
	out := negativeQueryObservation{ExpectedID: oldID, ExpectedName: oldName}
	if !s.IsToolEnabledForSession(ctx, name) {
		out.ProtocolError = "disabled registered tool"
		return out
	}
	s.EnsureToolPromotedForSession(ctx, name)
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		out.ProtocolError = err.Error()
		return out
	}
	out.Request = frame
	reply := s.MCPServer().HandleMessage(WithAuthorizedToolCall(ctx, name), frame)
	raw, err := json.Marshal(reply)
	if err != nil {
		out.ProtocolError = err.Error()
		return out
	}
	out.ReplyBytes = len(raw)
	out.ReplySHA = fmt.Sprintf("%x", sha256.Sum256(raw))
	bound := min(len(raw), 64<<10)
	out.ReplyPrefix = base64.StdEncoding.EncodeToString(raw[:bound])
	out.ReplyPrefixCut = bound < len(raw)
	var env struct {
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil {
		out.ProtocolError = "invalid registered reply JSON"
		return out
	}
	if len(env.Error) > 0 && !bytes.Equal(env.Error, []byte("null")) {
		out.ProtocolError = "registered protocol error; retained in bounded reply prefix"
		return out
	}
	if len(env.Result) == 0 {
		out.ProtocolError = "missing registered result"
		return out
	}
	if json.Unmarshal(env.Result, &out.result) != nil || out.result == nil {
		out.ProtocolError = "invalid registered result"
		return out
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(env.Result, &fields) != nil {
		out.ProtocolError = "invalid result fields"
		return out
	}
	out.IsError = fields["isError"]
	out.IsErrorPresent = len(out.IsError) > 0
	out.IsErrorValueValid = !out.IsErrorPresent || bytes.Equal(out.IsError, []byte("true")) || bytes.Equal(out.IsError, []byte("false"))
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(fields["content"], &content) == nil {
		for _, block := range content {
			if block.Type == "text" {
				out.TextBlocks++
			}
		}
	}
	// Match the payload/known-note contract in bench/load/edit_output_decode.go.
	// The registered tool reply must use its declared JSON channel. Do not
	// choose structuredContent because a text payload looks inconvenient.
	payload, payloadErr := negativeQueryDeclaredPayload(env.Result, false)
	if payloadErr != nil {
		out.ProtocolError = "declared JSON payload contract: " + payloadErr.Error()
	} else {
		out.Text = negativeQuerySummarize(payload, oldID, oldName)
	}
	out.Structured = negativeQuerySummarize(fields["structuredContent"], oldID, oldName)
	out.ChannelsAgree = !out.Structured.Present || out.Text.Valid && out.Structured.Valid && reflect.DeepEqual(out.Text.body, out.Structured.body)
	out.FullValueChecked = true
	return out
}

func negativeQueryDeclaredPayload(raw json.RawMessage, allowEmpty bool) ([]byte, error) {
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.IsError || len(envelope.Content) < 1 || len(envelope.Content) > 3 || envelope.Content[0].Type != "text" || envelope.Content[0].Text == nil || *envelope.Content[0].Text == "" && !allowEmpty {
		return nil, errors.New("output requires one successful payload and only known auxiliary notes")
	}
	lastNoteKind := 0
	for _, item := range envelope.Content[1:] {
		kind := 0
		if item.Text != nil {
			kind = negativeQuerySessionNoteKind(*item.Text)
		}
		if item.Type != "text" || kind <= lastNoteKind {
			return nil, errors.New("unknown or duplicate auxiliary output content")
		}
		lastNoteKind = kind
	}
	return []byte(*envelope.Content[0].Text), nil
}

func negativeQuerySessionNoteKind(text string) int {
	const nudge = ". Every location returned so far is real and citeable — if you are localizing or answering a question, consider writing your conclusion from what you already hold and fetching only what it still lacks. This note appears once.)"
	const escalation = " granular reads in a row. You likely have enough — answer NOW with your current best candidate and its evidence: name the file and the symbol, citing the locations already returned. A confident partial answer beats an exhausted turn budget with no answer."
	const alternative = " If one more look is genuinely needed, a single explore call with the request in plain words returns the ranked neighborhood — symbols, source, call paths — in one response."
	for _, form := range []struct {
		prefix, suffix string
		minimum, kind  int
	}{
		{"(Session note: graph read #", nudge, 6, 1},
		{"(Session note: ", escalation + " This note will not repeat.)", 10, 2},
		{"(Session note: ", escalation + alternative + " This note will not repeat.)", 10, 2},
	} {
		if !strings.HasPrefix(text, form.prefix) || !strings.HasSuffix(text, form.suffix) {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimPrefix(text, form.prefix), form.suffix)
		n, err := strconv.Atoi(digits)
		if err == nil && n >= form.minimum && strconv.Itoa(n) == digits {
			return form.kind
		}
	}
	return 0
}

func (o negativeQueryObservation) present() bool {
	return o.ProtocolError == "" && o.result != nil && !o.result.IsError && o.IsErrorValueValid && o.Text.Valid && o.Text.FoundExact && o.ChannelsAgree
}

func (o negativeQueryObservation) afterState() string {
	if o.ProtocolError != "" || o.result == nil || o.result.IsError || !o.IsErrorValueValid || !o.Text.Valid || !o.ChannelsAgree {
		return "UNKNOWN"
	}
	if o.Text.HasOldIdentity {
		return "OLD_ID_OR_NAME_RETURNED"
	}
	if o.Text.Complete {
		return "COMPLETE_ABSENCE"
	}
	return "UNKNOWN"
}

func TestRegisteredNameQueryKeepsRealPresenceControls(t *testing.T) {
	t.Run("native_backend", func(t *testing.T) { runRegisteredNameQueryPresenceControls(t, true) })
	t.Run("reader_fallback", func(t *testing.T) { runRegisteredNameQueryPresenceControls(t, false) })
}

func runRegisteredNameQueryPresenceControls(t *testing.T, native bool) {
	t.Helper()
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	const prefix = "query-fixture"
	const rel = "queries.go"
	const oldName = "QueryMarker014Before"
	const newName = "QueryMarker014"
	oldID := prefix + "/" + rel + "::" + oldName
	newID := prefix + "/" + rel + "::" + newName
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	var source strings.Builder
	source.WriteString("package probe\n\nfunc QueryFixtureRoot() int { return 0 }\n\n")
	for ordinal := range 1000 {
		fmt.Fprintf(&source, "func QueryMarker%03dBefore() int { return %d }\n\n", ordinal, ordinal)
	}
	initialPositions := token.NewFileSet()
	initialAST, err := parser.ParseFile(initialPositions, path, source.String(), 0)
	require.NoError(t, err)
	expectedLine := 0
	for _, decl := range initialAST.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == oldName {
			expectedLine = initialPositions.Position(fn.Pos()).Line
		}
	}
	require.Positive(t, expectedLine)
	require.NoError(t, os.WriteFile(path, []byte(source.String()), 0o600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "query-contract.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newProjectionTestIndexer(t, store)
	idx.SetRepoPrefix(prefix)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.NotNil(t, store.GetNode(oldID), "real indexed old identity must exist before any query")
	nativeSymbols, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Positive(t, nativeSymbols, "the registered control must use the populated native SQLite symbol corpus")
	engine := query.NewEngine(store)
	if native {
		engine.SetSearchProvider(idx.Search) // Same provider wiring as the normal daemon.
	}
	t.Logf("native_search_enabled=%t available_backend=%T native_symbol_FTS_count=%d", native, idx.Search(), nativeSymbols)
	s := NewServer(engine, store, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	t.Cleanup(func() { close(pending); s.DrainBackground() })
	const session = "name-query-registered"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	forms := []struct{ name, query string }{{"plain", oldName}, {"name_clause_only", "name:" + oldName}}
	var observations [3][2]negativeQueryObservation
	queryArgs := func(q string) map[string]any {
		return map[string]any{"operation": "symbols", "query": q, "options": map[string]any{"repo": prefix, "path": rel, "query_class": "symbol", "expand": "off", "limit": 10000, "max_per_file": 0, "max_bytes": 0}, "output": map[string]any{"format": "json", "max_bytes": 0}}
	}
	queryPair := func(stage int) {
		for i, form := range forms {
			args := queryArgs(form.query)
			observations[stage][i] = negativeQueryRegisteredCall(s, ctx, 20+stage*10+i, "search", args, oldID, oldName)
			encoded, marshalErr := json.Marshal(observations[stage][i])
			require.NoError(t, marshalErr)
			t.Logf("stage=%d form=%s observation=%s", stage, form.name, encoded)
		}
	}
	queryPair(0)
	for i, form := range forms {
		require.True(t, observations[0][i].present(), "native backend before-present control failed for %s; empty absence cannot earn credit", form.name)
	}
	retained := negativeQueryRegisteredCall(s, ctx, 25, "edit", snapshotEditArgs(path, "QueryMarker015Before", "QueryMarker015"), oldID, oldName)
	queryPair(1) // No physical read/SQL check/poll between LOCAL ACK and this burst.
	firstBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	ackFields := func(o negativeQueryObservation) map[string]any {
		encoded, marshalErr := json.Marshal(o)
		require.NoError(t, marshalErr)
		t.Logf("ack_observation=%s", encoded)
		require.Empty(t, o.ProtocolError)
		require.NotNil(t, o.result)
		require.False(t, o.result.IsError, resultText(o.result))
		fields := extractTextResult(t, o.result)
		require.Equal(t, "local", fields["ack_mode"])
		require.Equal(t, true, fields["readback_exact"])
		return fields
	}
	firstACK := ackFields(retained)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(firstBytes)), firstACK["content_sha256"])
	require.Contains(t, string(firstBytes), "func "+oldName+"(", "unrelated committed edit must retain the old function physically")
	retainedOwner := liveEntry(pendingFileProjections, path)
	require.NotNil(t, retainedOwner)
	require.Positive(t, retainedOwner.sourceCommitSeq)
	t.Logf("retained_owner commit_seq=%d source_commit_seq=%d", retainedOwner.commitSeq, retainedOwner.sourceCommitSeq)
	renamed := negativeQueryRegisteredCall(s, ctx, 35, "edit", snapshotEditArgs(path, oldName, newName), oldID, oldName)
	positiveArgs := queryArgs("name:" + newName)
	delete(positiveArgs["options"].(map[string]any), "path") // The positive lookup is repo-scoped, without path narrowing.
	newPositive := negativeQueryRegisteredCall(s, ctx, 36, "search", positiveArgs, newID, newName)
	positiveJSON, err := json.Marshal(newPositive)
	require.NoError(t, err)
	t.Logf("immediate_new_target_after_rename observation=%s", positiveJSON)
	queryPair(2) // Physical afterimage observation follows the immediate burst.
	afterBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	secondACK := ackFields(renamed)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(afterBytes)), secondACK["content_sha256"])
	afterPositions := token.NewFileSet()
	parsed, err := parser.ParseFile(afterPositions, path, afterBytes, 0)
	require.NoError(t, err)
	oldCount, newCount := 0, 0
	for _, decl := range parsed.Decls {
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
	require.Zero(t, oldCount)
	require.Equal(t, 1, newCount)
	require.NotNil(t, store.GetNode(oldID), "canonical old row stays held: the real projection must mask it")
	finalOwner := liveEntry(pendingFileProjections, path)
	require.NotNil(t, finalOwner)
	require.Positive(t, finalOwner.sourceCommitSeq)
	t.Logf("renamed_owner commit_seq=%d source_commit_seq=%d", finalOwner.commitSeq, finalOwner.sourceCommitSeq)
	require.True(t, newPositive.present(), "the real immediate name-only new-target response must contain the exact new ID and name")
	require.True(t, newPositive.ChannelsAgree, "both returned data channels must agree when present")
	checkPositive := func(channel negativeQueryChannel) {
		found := 0
		rows, valid := channel.body["results"].([]any)
		require.True(t, valid)
		for _, rawRow := range rows {
			row, valid := rawRow.(map[string]any)
			require.True(t, valid)
			if row["id"] == newID || row["name"] == newName {
				found++
				require.Equal(t, newID, row["id"])
				require.Equal(t, newName, row["name"])
				require.Equal(t, prefix+"/"+rel, row["file_path"])
				require.Equal(t, float64(expectedLine), row["start_line"])
			}
		}
		require.Equal(t, 1, found)
	}
	checkPositive(newPositive.Text)
	if newPositive.Structured.Present {
		checkPositive(newPositive.Structured)
	}
	t.Logf("new_target_name_only_positive=true exact_id=%s exact_path=%s exact_line=%d structured_present=%t channels_agree=%t", newID, prefix+"/"+rel, expectedLine, newPositive.Structured.Present, newPositive.ChannelsAgree)
	for i, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			before, retained, after := observations[0][i], observations[1][i], observations[2][i]
			t.Logf("before_present=%t retained_after_local_ACK=%t after_rename=%s channels_agree=%t physical_after_sha256=%x", before.present(), retained.present(), after.afterState(), after.ChannelsAgree, sha256.Sum256(afterBytes))
			require.True(t, before.present(), "existing query form must find the real indexed target, not manufacture empty absence")
			require.True(t, retained.present(), "an actual unrelated LOCAL ACK must not make a retained old target disappear")
			require.NotEqual(t, "OLD_ID_OR_NAME_RETURNED", after.afterState(), "a physical rename with LOCAL ACK must hide the canonical old identity")
			require.Contains(t, []string{"COMPLETE_ABSENCE", "UNKNOWN"}, after.afterState(), "only complete absence earns absence credit; incomplete/error/ambiguous channels remain UNKNOWN")
			if form.name == "name_clause_only" {
				require.Equal(t, "COMPLETE_ABSENCE", after.afterState(), "driver binding requires the unchanged strict complete-negative contract")
			}
		})
	}
}
