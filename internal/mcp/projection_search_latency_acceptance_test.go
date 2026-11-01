//go:build acceptance

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type projectionLatencyFixture struct {
	server   *Server
	registry *pendingProjections
	snapshot *projectionSnapshot
	root     string
	covered  map[string]bool
	files    int
}

func prepareProjectionLatencyFixture(t *testing.T, source, revision string, entries int) *projectionLatencyFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "corpus")
	require.NoError(t, os.Mkdir(root, 0o700))
	archive := filepath.Join(t.TempDir(), "corpus.tar")
	command := exec.Command("git", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "archive", "--format=tar", "--output="+archive, revision)
	command.Dir = source
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "owned pinned archive: %s", output)
	output, err = exec.Command("tar", "-xf", archive, "-C", root).CombinedOutput()
	require.NoError(t, err, "owned archive extraction: %s", output)
	_, err = config.ParseWorkspaceFile(filepath.Join(root, ".gortex.yaml"))
	require.NoError(t, err, "comparison must use the pinned corpus's own index scope")
	manager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "absent-global.yaml"))
	require.NoError(t, err)
	manager.LoadWorkspaceConfig("", root)
	cfg := manager.GetRepoConfig("").Index
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	indexed, err := idx.Index(root)
	require.NoError(t, err)
	engine := query.NewEngine(store)
	engine.SetSearch(search.NewSymbolSearcherBackend(store))
	server := NewServer(engine, store, idx, nil, zap.NewNop(), nil)
	// Pending graph publication is fixture state, never a held SQLite writer
	// or read gate. This inert watcher also prevents legacy read repair from
	// changing the population being compared.
	server.SetWatcher(mutationTestWatcher{done: make(chan indexer.MutationResult)})
	fixture := &projectionLatencyFixture{server: server, registry: newPendingProjections(), root: root, covered: map[string]bool{}, files: indexed.FileCount}
	if entries != 0 {
		var paths []string
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if idx.IsTracked(filepath.ToSlash(rel)) {
				paths = append(paths, path)
			}
			return nil
		})
		require.NoError(t, err)
		sort.Strings(paths)
		require.GreaterOrEqual(t, len(paths), entries, "do not substitute a smaller projection population")
		for _, path := range paths[:entries] {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			// Existing declaration coordinates and query literals stay
			// identical while the disk bytes are genuinely ahead of base.
			raw = append(raw, []byte("\n// pending search comparison fixture\n")...)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			stamp, err := statDiskStamp(path)
			require.NoError(t, err)
			entry, err := projectFile(t.Context(), idx, path, raw, 0, stamp)
			require.NoError(t, err, "a refused projection cannot be skipped")
			require.NoError(t, fixture.registry.publish(entry), "all512 entries must fit existing caps")
			fixture.covered[entry.graphPath] = true
		}
	}
	fixture.snapshot = fixture.registry.snap.Load()
	require.Len(t, fixture.snapshot.entries, entries)
	return fixture
}

func projectionLatencyProbes(t *testing.T, fixture *projectionLatencyFixture) []projectionLatencyProbe {
	t.Helper()
	names := map[string]int{}
	var nodes []*graph.Node
	for node := range fixture.server.graph.NodesByKind(graph.KindFunction) {
		names[node.Name]++
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	groups := map[bool][]*graph.Node{}
	for _, node := range nodes {
		if names[node.Name] != 1 || len(node.Name) < 5 || !strings.HasSuffix(node.FilePath, ".go") {
			continue
		}
		covered := fixture.covered[node.FilePath]
		if len(groups[covered]) < 10 {
			groups[covered] = append(groups[covered], node)
		}
	}
	require.Len(t, groups[true], 10, "covered query population unavailable")
	require.Len(t, groups[false], 10, "uncovered query population unavailable")
	var probes []projectionLatencyProbe
	for _, covered := range []bool{true, false} {
		for _, node := range groups[covered] {
			probes = append(probes, projectionLatencyProbe{Tool: "search_symbols", Query: node.Name, ID: node.ID, File: node.FilePath, Line: node.StartLine}, projectionLatencyProbe{Tool: "search_text", Query: "func " + node.Name, ID: node.ID, File: node.FilePath, Line: node.StartLine})
		}
	}
	return probes
}

func validateProjectionLatencyResult(probe projectionLatencyProbe, result *mcplib.CallToolResult) error {
	var body map[string]any
	decoded := false
	for _, raw := range result.Content {
		if text, ok := raw.(mcplib.TextContent); ok {
			if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
				return err
			}
			decoded = true
			break
		}
	}
	if !decoded {
		return errors.New("missing JSON text response")
	}
	key := "results"
	if probe.Tool == "search_text" {
		key = "matches"
	}
	rows, _ := body[key].([]any)
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if probe.Tool == "search_symbols" {
			if row["id"] == probe.ID && row["file_path"] == probe.File && row["start_line"] == float64(probe.Line) {
				return nil
			}
		} else if row["path"] == probe.File && row["line"] == float64(probe.Line) {
			if text, ok := row["text"].(string); ok && strings.Contains(text, probe.Query) {
				return nil
			}
		}
	}
	return fmt.Errorf("missing exact %s witness %s:%d", probe.Tool, probe.File, probe.Line)
}

// This is invoked ONCE by a separately reviewed exclusive operator. Ordinary
// tests neither stage a corpus nor infer performance from synthetic controls.
func TestProjectionSearchHandlerLatencyAcceptance(t *testing.T) {
	if os.Getenv("GORTEX_SEARCH_LATENCY_ACCEPTANCE") != "1" {
		t.Skip("requires the reviewed exclusive same-input search comparison operator")
	}
	source, revision, out := os.Getenv("GORTEX_SEARCH_LATENCY_SOURCE"), os.Getenv("GORTEX_SEARCH_LATENCY_REVISION"), os.Getenv("GORTEX_SEARCH_LATENCY_OUTPUT")
	require.NotEmpty(t, source)
	require.Len(t, revision, 40)
	require.NotEmpty(t, out)
	require.True(t, filepath.IsAbs(source) && filepath.IsAbs(out), "reviewed source and output paths must be absolute")
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = source
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	head, err := command.Output()
	require.NoError(t, err)
	require.Equal(t, revision, strings.TrimSpace(string(head)))
	command = exec.Command("git", "-c", "core.fsmonitor=false", "status", "--porcelain", "--untracked-files=no")
	command.Dir = source
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	dirty, err := command.Output()
	require.NoError(t, err)
	require.Empty(t, string(dirty))
	require.NoError(t, os.Mkdir(out, 0o700), "fresh output required")
	previous := pendingFileProjections
	t.Cleanup(func() { pendingFileProjections = previous })
	fixtures := map[int]*projectionLatencyFixture{0: prepareProjectionLatencyFixture(t, source, revision, 0), 512: prepareProjectionLatencyFixture(t, source, revision, 512)}
	probes := projectionLatencyProbes(t, fixtures[512])
	selectPopulation := func(entries int) (func() error, error) {
		fixture := fixtures[entries]
		pendingFileProjections = fixture.registry
		verify := func() error {
			if pendingFileProjections != fixture.registry || fixture.registry.snap.Load() != fixture.snapshot || len(fixture.snapshot.entries) != entries {
				return errors.New("projection population changed")
			}
			return nil
		}
		return verify, verify()
	}
	call := func(entries int, probe projectionLatencyProbe) (*mcplib.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := mcplib.CallToolRequest{}
		req.Params.Name = probe.Tool
		arguments := map[string]any{"query": probe.Query, "limit": 1000, "max_bytes": 0, "format": "json"}
		if probe.Tool == "search_symbols" {
			arguments["query_class"] = "symbol"
			arguments["expand"] = "off"
		}
		req.Params.Arguments = arguments
		if probe.Tool == "search_symbols" {
			return fixtures[entries].server.handleSearchSymbols(ctx, req)
		}
		return fixtures[entries].server.handleSearchText(ctx, req)
	}
	warmup, err := sampleProjectionHandlers(probes, 1, selectPopulation, call, validateProjectionLatencyResult)
	warmupData, encodeErr := json.MarshalIndent(warmup, "", "  ")
	require.NoError(t, encodeErr)
	require.NoError(t, os.WriteFile(filepath.Join(out, "warmup.json"), append(warmupData, '\n'), 0o600))
	require.NoError(t, err, "identical complete warmup required on both populations")
	raw, err := os.OpenFile(filepath.Join(out, "samples.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	encoder := json.NewEncoder(raw)
	samples, runErr := sampleProjectionHandlers(probes, 50, selectPopulation, call, validateProjectionLatencyResult, func(sample projectionLatencySample) error { return encoder.Encode(sample) })
	require.NoError(t, raw.Close())
	type comparison struct {
		Tool         string `json:"tool"`
		Count0       int    `json:"count_0"`
		Count512     int    `json:"count_512"`
		P950         *int64 `json:"p95_0_ns"`
		P95512       *int64 `json:"p95_512_ns"`
		LimitDeltaNS int64  `json:"limit_delta_ns"`
		Pass         bool   `json:"pass"`
	}
	var comparisons []comparison
	pass := runErr == nil && len(samples) == 4000
	for _, tool := range []string{"search_symbols", "search_text"} {
		values := map[int][]int64{}
		for _, sample := range samples {
			if sample.Probe.Tool == tool && sample.Error == "" && sample.ElapsedNS != nil && *sample.ElapsedNS >= 0 {
				values[sample.Entries] = append(values[sample.Entries], *sample.ElapsedNS)
			}
		}
		row := comparison{Tool: tool, Count0: len(values[0]), Count512: len(values[512]), LimitDeltaNS: int64(25 * time.Millisecond)}
		if runErr == nil && row.Count0 == 1000 && row.Count512 == 1000 {
			slices.Sort(values[0])
			slices.Sort(values[512])
			base, pending := values[0][949], values[512][949]
			row.P950, row.P95512 = &base, &pending
			row.Pass = pending <= base+row.LimitDeltaNS
		}
		pass = pass && row.Pass
		comparisons = append(comparisons, row)
	}
	receipt := map[string]any{"schema_version": 1, "source_revision": revision, "go": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0), "samples": len(samples), "warmup_samples": len(warmup), "comparisons": comparisons, "pass": pass, "indexed_files": fixtures[0].files, "projection_entries": len(fixtures[512].snapshot.entries), "projection_charged_bytes": fixtures[512].snapshot.bytes, "fixture": "same pinned configured corpus;512 path-ordered tracked Go files with appended comments; actual native SQLite handlers; setup/warmup/validation excluded equally; alternating adjacent matched requests"}
	if runErr != nil {
		receipt["error"] = runErr.Error()
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(out, "comparison.json"), append(data, '\n'), 0o600))
	require.True(t, pass, "retained comparison failed or had missing/refused/incorrect samples")
}
