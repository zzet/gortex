package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type pendingBudgetExtractor struct {
	base                       parser.Extractor
	hold                       atomic.Bool
	entered, release, finished chan struct{}
}

func (e *pendingBudgetExtractor) Language() string     { return "go" }
func (e *pendingBudgetExtractor) Extensions() []string { return []string{".go"} }
func (e *pendingBudgetExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	if e.hold.CompareAndSwap(true, false) {
		close(e.entered)
		<-e.release
		defer close(e.finished)
	}
	return e.base.Extract(path, raw)
}

func TestProjectionBudgetReturnsPendingSummary(t *testing.T) {
	selectedProjectionRegistry(t)
	e := &pendingBudgetExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	reg := parser.NewRegistry()
	reg.Register(e)
	s, root := setupTestServerWithRegistry(t, reg)
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc BeyondBudget() {}\n")...), 0600))
	e.hold.Store(true)
	var once sync.Once
	release := func() { once.Do(func() { close(e.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_file_summary"
	req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
	type reply struct {
		result *mcplib.CallToolResult
		err    error
	}
	response := make(chan reply, 1)
	go func() {
		result, err := s.wrapToolHandler(s.handleGetFileSummary)(ctx, req)
		response <- reply{result, err}
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-e.finished:
		case <-time.After(time.Second):
			t.Error("owned extractor failed to finish after release")
		}
	})
	select {
	case <-e.entered:
	case <-ctx.Done():
		t.Fatal("actual projection did not enter held extractor")
	}
	var actual reply
	select {
	case actual = <-response:
	case <-ctx.Done():
		release()
		actual = <-response
		t.Fatal("summary failed to return while projection remained held")
	}
	require.NoError(t, actual.err)
	require.NotNil(t, actual.result)
	require.False(t, actual.result.IsError, "projection budget must return a labelled graph answer: %s", resultText(actual.result))
	fields := extractTextResult(t, actual.result)
	require.NotEmpty(t, fields["nodes"], "the existing graph remains useful while a file projection is unavailable")
	freshness := fields["freshness"].(map[string]any)
	require.Equal(t, "pending", freshness["graph_freshness"])
	require.Equal(t, "external_write", freshness["file_reason"])
	require.NotEqual(t, true, freshness["readback_exact"])
	require.Empty(t, pendingFileProjections.snap.Load().entries)
	require.Nil(t, s.graph.GetNode("main.go::BeyondBudget"))
	release()
	select {
	case <-e.finished:
	case <-ctx.Done():
		t.Fatal("owned extractor did not finish")
	}
	require.Empty(t, pendingFileProjections.snap.Load().entries, "late extraction cannot publish a timed-out read projection")
}
