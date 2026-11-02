//go:build acceptance

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// The micro fixture owns exactly twenty affected Go files and a real watcher.
// It is not the SQLite/reference-host/transport acceptance workload.
type renameTwentyFixture struct {
	server   *Server
	root     string
	original map[string]string
	invoke   mcpserver.ToolHandlerFunc
}

func newRenameTwentyFixture(tb testing.TB, mode config.EditAckMode) *renameTwentyFixture {
	tb.Helper()
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	tb.Cleanup(func() { pendingFileProjections = previous })
	root := tb.TempDir()
	files := map[string]string{"target.go": "package sample\nfunc Target() string { return \"unchanged\" }\n"}
	for i := 1; i < 20; i++ {
		files[fmt.Sprintf("caller%02d.go", i)] = fmt.Sprintf("package sample\nfunc Caller%02d() string { return Target() }\n", i)
	}
	require.NoError(tb, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/rename20\n\ngo 1.21\n"), 0600))
	for name, body := range files {
		require.NoError(tb, os.WriteFile(filepath.Join(root, name), []byte(body), 0600))
	}
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(root)
	require.NoError(tb, err)
	configPath := filepath.Join(tb.TempDir(), "config.yaml")
	// Omission exercises the production default full; local is explicit.
	configBody := ""
	if mode == config.EditAckLocal {
		configBody = "edit:\n  ack: local\n"
	}
	require.NoError(tb, os.WriteFile(configPath, []byte(configBody), 0600))
	manager, err := config.NewConfigManager(configPath)
	require.NoError(tb, err)
	s := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil, MultiRepoOptions{ConfigManager: manager})
	s.RunAnalysis()
	require.Equal(tb, mode, s.sourceAckPolicy(tb.Context()).mode)
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(tb, err)
	s.SetWatcher(watcher)
	require.NoError(tb, watcher.Start([]string{root}))
	tb.Cleanup(func() { require.NoError(tb, watcher.Stop()); idx.Close() })
	invoke := s.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return s.runAcknowledgedEdit(ctx, req, s.handleRenameSymbol)
	})
	return &renameTwentyFixture{server: s, root: root, original: files, invoke: invoke}
}

func (f *renameTwentyFixture) rename(tb testing.TB, before, after string) (time.Duration, []map[string]any) {
	tb.Helper()
	req := makeReq("rename_symbol", map[string]any{"id": "target.go::" + before, "new_name": after, "format": "json"})
	started := time.Now()
	result, err := f.invoke(tb.Context(), req)
	elapsed := time.Since(started)
	if bench, ok := tb.(*testing.B); ok {
		bench.StopTimer()
		if result != nil {
			bench.Logf("rename_ack_sample mode=%s before=%s after=%s elapsed_ns=%d response=%s", f.server.sourceAckPolicy(tb.Context()).mode, before, after, elapsed.Nanoseconds(), resultText(result))
		}
	}
	require.NoError(tb, err)
	require.NotNil(tb, result)
	require.False(tb, result.IsError, "%s", resultText(result))
	tb.Logf("semantic_rename mode=%s before=%s after=%s raw_response=%s", f.server.sourceAckPolicy(tb.Context()).mode, before, after, resultText(result))
	var body map[string]any
	require.NoError(tb, json.Unmarshal([]byte(resultText(result)), &body))
	require.Equal(tb, "applied", body["status"])
	require.EqualValues(tb, 20, body["files_written"])
	rows, ok := body["files"].([]any)
	require.True(tb, ok)
	require.Len(tb, rows, 20)
	files := make([]map[string]any, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, raw := range rows {
		file, ok := raw.(map[string]any)
		require.True(tb, ok)
		name, ok := file["file"].(string)
		require.True(tb, ok)
		require.Contains(tb, f.original, name)
		require.False(tb, seen[name])
		seen[name] = true
		require.Equal(tb, "applied", file["status"])
		require.Equal(tb, "committed", file["disk_status"])
		require.Equal(tb, string(f.server.sourceAckPolicy(tb.Context()).mode), file["ack_mode"])
		if f.server.sourceAckPolicy(tb.Context()).mode == config.EditAckFull {
			require.Equal(tb, "fresh", file["graph_status"], "full mode needs actual completion for every file")
		}
		current, err := os.ReadFile(filepath.Join(f.root, name))
		require.NoError(tb, err)
		require.Equal(tb, strings.ReplaceAll(f.original[name], "Target", after), string(current))
		files = append(files, file)
	}
	return elapsed, files
}

// Prepare the NEXT sample from an actually completed graph. This wait occurs
// after the measured ACK, is reported separately, and never grants local ACKs
// full-mode credit or represents sustained offered-load latency.
func (f *renameTwentyFixture) waitForPublication(tb testing.TB, files []map[string]any) time.Duration {
	tb.Helper()
	started := time.Now()
	ctx, cancel := context.WithTimeout(tb.Context(), time.Minute)
	defer cancel()
	for _, file := range files {
		id, ok := file["reindex_receipt"].(string)
		if !ok {
			// Completed full replies omit the queued-only receipt field. Require
			// the actual completion facts already asserted on this same row.
			require.Equal(tb, "full", file["ack_mode"])
			require.Equal(tb, "fresh", file["graph_status"])
			require.Equal(tb, true, file["reindexed"])
			continue
		}
		raw, ok := f.server.mutationReceipts.Load(id)
		require.True(tb, ok)
		receipt, ok := raw.(*mutationReceipt)
		require.True(tb, ok)
		select {
		case <-receipt.done:
			outcome := receipt.outcome(false)
			require.NoError(tb, outcome.Err)
			require.True(tb, outcome.Reindexed)
		case <-ctx.Done():
			tb.Fatal(ctx.Err())
		}
	}
	// Native watcher echoes can carry a newer generation than the returned
	// tool ticket. Prepare the next isolated sample from the CURRENT real
	// relations tier for all affected paths, not merely a closed old ticket.
	paths := make([]string, 0, len(f.original))
	for path := range f.original {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	deadline, ok := ctx.Deadline()
	require.True(tb, ok)
	fresh := f.server.awaitRelationFreshness(ctx, nil, paths, deadline)
	require.NoError(tb, ctx.Err())
	require.Empty(tb, fresh.state, "next-sample preparation must not permit provisional or failed relations: %v", fresh.files)
	return time.Since(started)
}

func TestRenameSymbolTwentyFileMicroFixture(t *testing.T) {
	for _, mode := range []config.EditAckMode{config.EditAckFull, config.EditAckLocal} {
		t.Run(string(mode), func(t *testing.T) {
			fixture := newRenameTwentyFixture(t, mode)
			_, files := fixture.rename(t, "Target", "Renamed")
			fixture.waitForPublication(t, files)
			_, files = fixture.rename(t, "Renamed", "Target")
			fixture.waitForPublication(t, files)
		})
	}
}

func BenchmarkRenameSymbolAck20Files(b *testing.B) {
	for _, mode := range []config.EditAckMode{config.EditAckFull, config.EditAckLocal} {
		b.Run(string(mode), func(b *testing.B) {
			fixture := newRenameTwentyFixture(b, mode)
			samples := make([]time.Duration, 0, b.N)
			var between time.Duration
			before, after := "Target", "Renamed"
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				elapsed, files := fixture.rename(b, before, after)
				b.StopTimer()
				samples = append(samples, elapsed)
				between += fixture.waitForPublication(b, files)
				before, after = after, before
				b.StartTimer()
			}
			b.StopTimer()
			slices.Sort(samples)
			require.NotEmpty(b, samples)
			p95 := (95*len(samples)+99)/100 - 1
			b.ReportMetric(float64(samples[p95])/float64(time.Millisecond), "ack-p95-ms")
			b.ReportMetric(float64(samples[len(samples)-1])/float64(time.Millisecond), "ack-max-ms")
			b.ReportMetric(float64(between)/float64(time.Millisecond)/float64(len(samples)), "between-sample-publication-ms")
			b.ReportMetric(float64(len(samples)), "actual-acks")
			b.ReportMetric(20, "affected-files")
		})
	}
}
