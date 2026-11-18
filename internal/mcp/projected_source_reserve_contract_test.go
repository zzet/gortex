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
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

type sourceReserveEntry struct {
	path  string
	bytes int
}

type heldSourceReserveExtractor struct {
	parser.Extractor
	entered, finished  chan sourceReserveEntry
	release            <-chan struct{}
	entries, completed atomic.Int64
}

func (e *heldSourceReserveExtractor) hold(path string, raw []byte) func() {
	entry := sourceReserveEntry{path: path, bytes: len(raw)}
	e.entries.Add(1)
	e.entered <- entry
	<-e.release
	return func() { e.completed.Add(1); e.finished <- entry }
}
func (e *heldSourceReserveExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	defer e.hold(path, raw)()
	return e.Extractor.Extract(path, raw)
}
func (e *heldSourceReserveExtractor) ExtractWithOptions(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	defer e.hold(path, raw)()
	return parser.Extract(e.Extractor, path, raw, options)
}

// The same literal direct-handler request can legitimately refuse even when
// the physical new symbol exists and no canonical mutation is allowed. Real
// entered parses occupy the existing2MiB projection reserve; no budget reset,
// synthetic acceptance, manually fabricated lease or source marker is used.
func TestSourceProjectionFullReservePreservesTypedRefusal(t *testing.T) {
	result := sourceFullReserveCounterexample(t)
	require.True(t, result.IsError, "occupied projection capacity is a public transient, never a successful source answer")
	require.True(t, sourceReadinessPending(result), "only the exact documented data-free refusal qualifies")
}

func sourceFullReserveCounterexample(t *testing.T) *mcplib.CallToolResult {
	t.Helper()
	srv, dir := setupTestServer(t)
	t.Cleanup(func() { srv.DrainBackground(); srv.indexer.Close() })
	srv.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(dir, "main.go")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	physical := append(append([]byte(nil), original...), []byte("\nfunc Added() int { return 1 }\n")...)
	require.NoError(t, os.WriteFile(path, physical, 0600))
	require.Nil(t, srv.graph.GetNode("main.go::Added"))

	// ProjectSource's existing hard maximum is1MiB and its separate global
	// reserve is2maximum sources. Default MaxFileSize0 admits this exact size.
	const maximumSourceBytes = 1 << 20
	root := t.TempDir()
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	extractor := &heldSourceReserveExtractor{Extractor: languages.NewGoExtractor(), entered: make(chan sourceReserveEntry, 2), finished: make(chan sourceReserveEntry, 2), release: release}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	cfg := config.Default().Index
	require.Zero(t, cfg.MaxFileSize, "use the actual default admission policy")
	producer := indexer.New(graph.New(), registry, cfg, zap.NewNop())
	producer.SetRootPath(root)
	caller, cancel := context.WithTimeout(t.Context(), 3*time.Second) // existing source-fixture caller bound
	type answer struct {
		projection *indexer.Projection
		err        error
	}
	answers := make(chan answer, 2)
	callersStarted, callersJoined, extractorsJoined := 0, 0, 0
	var closeOnce sync.Once
	join := func() {
		closeOnce.Do(func() {
			cancel()
			unblock()
			// The wrapper exposes actual parser return, and Close is the
			// existing lifecycle owner for timed-out background extractions.
		joiningCallers:
			for callersJoined < callersStarted {
				select {
				case <-answers:
					callersJoined++
				case <-time.After(3 * time.Second):
					t.Error("actual projection caller did not join after release")
					break joiningCallers
				}
			}
			producer.Close() // documented owner joins actual late extractions
			for {
				select {
				case <-extractor.finished:
					extractorsJoined++
				default:
					if int64(extractorsJoined) != extractor.entries.Load() || extractor.completed.Load() != extractor.entries.Load() {
						t.Errorf("actual extraction completion mismatch entered=%d completed=%d joined=%d", extractor.entries.Load(), extractor.completed.Load(), extractorsJoined)
					}
					return
				}
			}
		})
	}
	t.Cleanup(join)
	prefix := "package p\nfunc Held() {}\n"
	padding := "// bounded reserve fixture padding\n"
	remaining := maximumSourceBytes - len(prefix)
	raw := []byte(prefix + strings.Repeat(padding, remaining/len(padding)) + strings.Repeat("\n", remaining%len(padding)))
	require.Len(t, raw, maximumSourceBytes)
	for i := range 2 {
		file := filepath.Join(root, fmt.Sprintf("hold_%d.go", i))
		require.NoError(t, os.WriteFile(file, raw, 0600))
		callersStarted++
		go func() {
			projection, callErr := producer.ProjectSource(caller, file, raw)
			answers <- answer{projection, callErr}
		}()
	}
	var actualPaths []string
	for range 2 {
		select {
		case entry := <-extractor.entered:
			actualPaths = append(actualPaths, filepath.Base(entry.path))
			require.Equal(t, maximumSourceBytes, entry.bytes)
			require.Contains(t, []string{"hold_0.go", "hold_1.go"}, filepath.Base(entry.path))
		case <-caller.Done():
			t.Fatalf("both actual full-reserve parses must enter: %v", caller.Err())
		}
	}
	require.ElementsMatch(t, []string{"hold_0.go", "hold_1.go"}, actualPaths)
	// Both bounded callers return while their ACTUAL extraction/leases stay
	// held. This is the production late-extraction lifetime, not fake capacity.
	for callersJoined < 2 {
		select {
		case got := <-answers:
			callersJoined++
			require.Nil(t, got.projection)
			require.ErrorIs(t, got.err, indexer.ErrProjectionDeadline)
		case <-caller.Done():
			t.Fatalf("bounded projection caller did not return: %v", caller.Err())
		}
	}
	select {
	case <-extractor.finished:
		t.Fatal("held extraction finished before actual refusal")
	default:
	}
	require.NoError(t, caller.Err())
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol_source"
	req.Params.Arguments = map[string]any{"id": "main.go::Added", "context_lines": float64(0), "format": "json"}
	result, err := findAndCallHandler(srv, "get_symbol_source", caller, req)
	require.NoError(t, err)
	require.NoError(t, caller.Err(), "only the product150ms allowance may end this read")
	require.True(t, sourceReadinessPending(result))
	require.True(t, result.IsError)
	require.NotContains(t, resultText(result), "func Added()", "no source body may accompany refusal")
	require.Nil(t, srv.graph.GetNode("main.go::Added"), "read refusal must not publish canonical source")
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, physical, current)
	wire, err := json.Marshal(result)
	require.NoError(t, err)
	t.Logf("ACTUAL_FULL_PROJECTION_RESERVE_REFUSAL entered=2 bytes_each=%d bounded_callers_returned=2 actual_extractors_still_held=true caller_live=true physical_target_present=true canonical_target_absent=true first_reply=REFUSAL raw=%s", maximumSourceBytes, wire)
	join()
	require.Equal(t, 2, callersJoined)
	require.Equal(t, 2, extractorsJoined)
	t.Log("ACTUAL_FULL_PROJECTION_RESERVE_JOIN callers=2 extractors=2 producer_close_returned=true")
	return result
}
