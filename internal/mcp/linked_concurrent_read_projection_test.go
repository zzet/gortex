package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Several sessions read the same clean linked checkout file concurrently and
// nothing writes it. The selected view already holds those bytes, so no reply
// may claim they changed, and no read-built owner is published for them. A
// read whose projection budget runs out may answer pending for the file.
func TestLinkedCheckoutConcurrentReadsWithoutWritesNeverReportMovedSource(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	var source strings.Builder
	source.WriteString("package sample\n\n")
	for i := range 80 {
		fmt.Fprintf(&source, "// Helper%d returns its ordinal.\nfunc Helper%d(v int) int {\n\tif v > %d {\n\t\treturn v - %d\n\t}\n\treturn v + %d\n}\n\n", i, i, i, i, i)
	}
	source.WriteString("func Target() string { return \"clean\" }\n")
	f := newCleanLinkedSourceFixtureWithSource(t, []byte(source.String()), "Target")
	root := f.roots[1]
	path := filepath.Join(root, "main.go")
	publishedBefore := pendingFileProjections.published.Load()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	// Two agents on one linked checkout, each with five requests in flight.
	const sessions, slots, rounds = 2, 5, 12
	operations := []map[string]any{
		{"operation": "summary", "target": map[string]any{"file": "main.go"}},
		{"operation": "editing_context", "target": map[string]any{"file": "main.go"}},
		{"operation": "source", "target": map[string]any{"symbol": "repo/main.go::Target"}},
		{"operation": "file", "target": map[string]any{"file": "main.go"}},
	}
	var (
		requestID atomic.Int64
		mu        sync.Mutex
		moved     = map[string]int{}
		failures  []string
		wg        sync.WaitGroup
	)
	requestID.Store(100)
	for s := range sessions {
		session := f.session(t, ctx, root, fmt.Sprintf("linked-concurrent-reader-%d", s))
		for slot := range slots {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for round := range rounds {
					op := operations[(slot+round)%len(operations)]
					args := map[string]any{"output": map[string]any{"format": "json", "max_bytes": 0}}
					for k, v := range op {
						args[k] = v
					}
					name := op["operation"].(string)
					result, err := externalVersionRegisteredCall(f.srv, session, int(requestID.Add(1)), "read", args)
					mu.Lock()
					switch {
					case err != nil:
						failures = append(failures, fmt.Sprintf("%s: %v", name, err))
					case result.IsError && strings.Contains(resultText(result), errProjectedSourceMoved.Error()):
						moved[name]++
					case result.IsError:
						failures = append(failures, fmt.Sprintf("%s: %s", name, resultText(result)))
					case !strings.Contains(resultText(result), "Target") && !pendingForFile(resultText(result), "repo/main.go"):
						failures = append(failures, fmt.Sprintf("%s: reply lacks the target symbol: %s", name, resultText(result)))
					}
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	require.Empty(t, moved, "unchanged linked bytes must not be reported as moved")
	require.Empty(t, failures)
	require.Equal(t, publishedBefore, pendingFileProjections.published.Load(), "reads of bytes the selected view holds must not publish owners")
	require.Nil(t, liveEntry(pendingFileProjections, path))
}

// pendingForFile reports whether a JSON reply labels path's graph pending. A
// session note may follow the JSON object, so only the first value is read.
func pendingForFile(reply, path string) bool {
	var payload struct {
		Freshness struct {
			GraphFreshness string   `json:"graph_freshness"`
			PendingFiles   []string `json:"pending_files"`
		} `json:"freshness"`
	}
	return json.NewDecoder(strings.NewReader(reply)).Decode(&payload) == nil && payload.Freshness.GraphFreshness == "pending" && slices.Contains(payload.Freshness.PendingFiles, path)
}
