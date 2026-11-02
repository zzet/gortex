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
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

// This is a physical-writer/read-back correctness control. It samples no
// latency and provides no reference-host, WAL, or full-mode performance proof.
func TestEditSymbolTwoSessionsRepeatedFragment1000(t *testing.T) {
	selectedProjectionRegistry(t)
	const original = "package sample\n\nfunc first() int {\n\treturn 0\n}\n\nfunc second() int {\n\treturn 0\n}\n"
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
		var fields map[string]any
		if err := json.Unmarshal([]byte(resultText(result)), &fields); err != nil {
			return nil, err
		}
		return fields, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	start := make(chan struct{})
	type outcome struct {
		name      string
		completed int
		err       error
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
			for ordinal := 0; ordinal < 500; ordinal++ {
				before, after := ordinal%2, (ordinal+1)%2
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
				body, err := invoke(session, "get_symbol_source", map[string]any{"id": "sample.go::" + name, "context_lines": 0, "format": "json"})
				if err != nil {
					result.err = fmt.Errorf("read %d: %w", ordinal, err)
					cancel()
					return
				}
				want := fmt.Sprintf("func %s() int {\n\treturn %d\n}", name, after)
				if body["source"] != want {
					result.err = fmt.Errorf("read %d used the wrong repeated fragment: got %q want %q", ordinal, body["source"], want)
					cancel()
					return
				}
				result.completed++
			}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	total := 0
	for result := range outcomes {
		require.NoError(t, result.err, "session %s completed %d", result.name, result.completed)
		require.Equal(t, 500, result.completed)
		total += result.completed
	}
	require.Equal(t, 1000, total)
	t.Logf("actual committed full/fresh ACKs and exact source readbacks: %d/1000; two sessions, 500 edits each; no latency sample", total)
	current, err := os.ReadFile(filepath.Join(root, "sample.go"))
	require.NoError(t, err)
	require.Equal(t, original, string(current), "500 alternating commits per declaration preserve both repeated fragments")
	require.Equal(t, 2, strings.Count(string(current), "\treturn 0"))
}
