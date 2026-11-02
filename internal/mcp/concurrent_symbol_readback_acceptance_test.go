//go:build acceptance

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

// This verifies the #1012 right-symbol write criterion with an independent disk
// oracle. Public source outcomes are a separate two-session census, never the
// #1014 1/8/32-agent acceptance row. No latency or reference-host credit.
func TestEditSymbolTwoSessionsRepeatedFragment1000(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	const original = "package sample\n\nfunc first() int {\n\treturn 0\n}\n\nfunc second() int {\n\treturn 0\n}\n\nfunc decoyZero() int {\n\treturn 0\n}\n\nfunc decoyFirst() int {\n\treturn 11\n}\n\nfunc decoySecond() int {\n\treturn 22\n}\n"
	s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": original})
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	s.SetWatcher(watcher)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { require.NoError(t, watcher.Stop()); s.indexer.Close() })
	// Do not change the default policy to make this control complete sooner.
	require.Equal(t, config.EditAckFull, s.sourceAckPolicy(t.Context()).mode)
	edit := s.wrapToolHandler(s.handleEditSymbol)
	read := s.wrapToolHandler(s.handleGetSymbolSource)
	var auxiliaryResponses atomic.Int64
	invoke := func(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
		req := mcplib.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		var result *mcplib.CallToolResult
		var err error
		if name == "edit_symbol" {
			result, err = edit(ctx, req)
		} else {
			result, err = read(ctx, req)
		}
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, fmt.Errorf("%s returned no result", name)
		}
		if result.IsError {
			return nil, fmt.Errorf("%s refused: %s", name, resultText(result))
		}
		if len(result.Content) == 0 {
			return nil, fmt.Errorf("%s returned no primary payload", name)
		}
		primary, ok := result.Content[0].(mcplib.TextContent)
		if !ok {
			return nil, fmt.Errorf("%s returned a non-text primary payload", name)
		}
		// Session notes are separate MCP content blocks, not trailing JSON.
		// Keep the real middleware enabled and decode only its primary record.
		if len(result.Content) > 1 {
			auxiliaryResponses.Add(1)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(primary.Text), &fields); err != nil {
			return nil, fmt.Errorf("%s primary JSON: %w; payload=%q", name, err, primary.Text)
		}
		return fields, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	start := make(chan struct{})
	type outcome struct {
		name          string
		completed     int
		sourceExact   int
		sourcePending int
		sourceRefused int
		err           error
	}
	outcomes := make(chan outcome, 2)
	var workers sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := outcome{name: name}
			defer func() { outcomes <- result }()
			session := WithSessionID(ctx, "repeat-"+name)
			<-start
			unique := 11
			if name == "second" {
				unique = 22
			}
			for ordinal := 0; ordinal < 500; ordinal++ {
				before, after := 0, unique
				if ordinal%2 == 1 {
					before, after = unique, 0
				}
				ack, err := invoke(session, "edit_symbol", map[string]any{
					"id": "sample.go::" + name, "old_source": fmt.Sprintf("\treturn %d", before), "new_source": fmt.Sprintf("\treturn %d", after), "dry_run": false, "format": "json",
				})
				if err != nil {
					result.err = fmt.Errorf("edit %d: %w", ordinal, err)
					cancel()
					return
				}
				if ack["disk_status"] != "committed" || ack["ack_mode"] != "full" || ack["graph_status"] != "fresh" {
					result.err = fmt.Errorf("edit %d lacks actual committed/full ACK: %v", ordinal, ack)
					cancel()
					return
				}
				body, readErr := invoke(session, "get_symbol_source", map[string]any{"id": "sample.go::" + name, "context_lines": 0, "format": "json"})
				want := fmt.Sprintf("func %s() int {\n\treturn %d\n}", name, after)
				switch {
				case readErr != nil:
					result.sourceRefused++
				case body["source"] == want:
					result.sourceExact++
				case body["source"] == nil && body["source_state"] == "pending" && body["complete"] == false:
					result.sourcePending++
				default:
					result.err = fmt.Errorf("read %d returned an incorrect source payload: want %q response=%v", ordinal, want, body)
					cancel()
					return
				}
				// Stabilize only the independent physical oracle, AFTER the actual
				// immediate public read above. This adds no wait to that probe and
				// prevents a peer's temporary rename from racing the oracle's open.
				path := filepath.Join(root, "sample.go")
				_, release, err := acquireMutationPaths(session, []string{path})
				if err != nil {
					result.err = err
					cancel()
					return
				}
				current, diskErr := os.ReadFile(path)
				release()
				if diskErr != nil || !strings.Contains(string(current), want) {
					result.err = fmt.Errorf("disk oracle edit %d must land in %s: err=%v want=%q actual=%q", ordinal, name, diskErr, want, current)
					cancel()
					return
				}
				for _, decoy := range []string{"func decoyZero() int {\n\treturn 0\n}", "func decoyFirst() int {\n\treturn 11\n}", "func decoySecond() int {\n\treturn 22\n}"} {
					if !strings.Contains(string(current), decoy) {
						result.err = fmt.Errorf("edit %d changed an unrelated repeated fragment: %q", ordinal, decoy)
						cancel()
						return
					}
				}
				result.completed++
			}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	total, sourceExact, sourcePending, sourceRefused := 0, 0, 0, 0
	for result := range outcomes {
		require.NoError(t, result.err, "session %s completed %d", result.name, result.completed)
		require.Equal(t, 500, result.completed)
		total += result.completed
		sourceExact += result.sourceExact
		sourcePending += result.sourcePending
		sourceRefused += result.sourceRefused
	}
	require.Equal(t, 1000, total)
	require.Equal(t, 1000, sourceExact+sourcePending+sourceRefused, "every immediate public read outcome remains accounted for")
	t.Logf("#1012 physical right-symbol writes=%d/1000, default full/fresh ACKs; unrelated repeated fragments unchanged", total)
	t.Logf("separate two-session public source census: exact=%d pending=%d refused=%d auxiliary_note_responses=%d; NO #1014 1/8/32-agent or latency acceptance credit", sourceExact, sourcePending, sourceRefused, auxiliaryResponses.Load())
	current, err := os.ReadFile(filepath.Join(root, "sample.go"))
	require.NoError(t, err)
	require.Equal(t, original, string(current), "500 alternating commits per declaration preserve both repeated fragments")
	require.Equal(t, 3, strings.Count(string(current), "\treturn 0"))
}
