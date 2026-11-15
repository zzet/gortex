package tstypes

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func TestFactSpoolThousandsStayInBoundedDeterministicPages(t *testing.T) {
	spool, err := newFactSpool()
	require.NoError(t, err)
	path := spool.path
	defer func() {
		spool.close()
		_, statErr := os.Stat(path)
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	}()

	const total = 4097
	batch := make([]stagedFileFacts, 0, tstypesFactPageFiles)
	for i := total - 1; i >= 0; i-- { // deliberately reverse insertion order
		facts := &fileFacts{
			file: fmt.Sprintf("src/f%05d.ts", i), repoPrefix: "repo/",
			imports: []Import{{Local: "T", Path: "./t"}},
			calls: []callFact{{
				line: 3, method: "run", recvType: fmt.Sprintf("T%d", i),
				recvChain: &callFact{line: 3, method: "step", recvIdent: "Builder"},
			}},
			supers:  []superFact{{typeName: fmt.Sprintf("T%d", i), superName: "Base", kind: graph.EdgeExtends, line: 1}},
			metas:   []metaFact{{key: "return_type", value: "Result", name: "run", line: 2}},
			aliases: []aliasFact{{typeName: fmt.Sprintf("T%d", i), alias: "go", trait: "Trait", method: "run", line: 1}},
		}
		record, encodeErr := stageFileFacts(facts)
		require.NoError(t, encodeErr)
		batch = append(batch, record)
		if len(batch) == cap(batch) {
			require.NoError(t, spool.appendFiles(batch))
			batch = batch[:0]
		}
	}
	require.NoError(t, spool.appendFiles(batch))

	after := ""
	seen := 0
	lastSeen := ""
	for {
		page, last, stats, pageErr := spool.pageClass(context.Background(), classCalls, after)
		require.NoError(t, pageErr)
		if len(page) == 0 {
			break
		}
		assert.LessOrEqual(t, stats.Files, tstypesFactPageFiles)
		assert.LessOrEqual(t, stats.Bytes, tstypesFactPageBytes)
		for _, facts := range page {
			assert.Greater(t, facts.file, lastSeen)
			lastSeen = facts.file
			require.Len(t, facts.calls, 1)
			require.NotNil(t, facts.calls[0].recvChain)
			assert.Equal(t, "step", facts.calls[0].recvChain.method)
			require.Len(t, facts.imports, 1, "imports side-fetch must ride every class page")
			assert.Empty(t, facts.supers, "a calls page must not decode other classes")
			seen++
		}
		after = last
	}
	assert.Equal(t, total, seen)
}

func TestStreamedRepoMatchesWholeCorpusCrossFileSemantics(t *testing.T) {
	fixture := map[string]string{
		"src/svc.ts": tsSvc,
		"src/iface.ts": `export interface Greeter {
  greet(): void;
}
`,
		"src/impl.ts": `import { Greeter } from "./iface";
import { Svc } from "./svc";

export class Impl extends Svc implements Greeter {
  greet(): void {}
  handle(s: Svc): void { s.run(); }
}
`,
	}
	streamGraph, streamDir := buildFixture(t, fixture)
	wholeGraph, wholeDir := buildFixture(t, fixture)
	p := NewProvider(TypeScriptSpec(), zap.NewNop())

	streamResult, err := p.EnrichRepo(streamGraph, "", streamDir)
	require.NoError(t, err)
	wholeResult := enrichWholeCorpusForTest(t, p, wholeGraph, "", wholeDir)

	assert.Equal(t, wholeResult.EdgesAdded, streamResult.EdgesAdded)
	assert.Equal(t, wholeResult.EdgesConfirmed, streamResult.EdgesConfirmed)
	assert.Equal(t, wholeResult.NodesEnriched, streamResult.NodesEnriched)
	assert.Equal(t, wholeResult.SymbolsTotal, streamResult.SymbolsTotal)
	assert.Equal(t, wholeResult.SymbolsCovered, streamResult.SymbolsCovered)
	assert.Equal(t, semanticSnapshot(wholeGraph), semanticSnapshot(streamGraph))
}

func TestStreamedRepoThousandsBoundPeakFactsAndApplierCaches(t *testing.T) {
	if testing.Short() {
		t.Skip("thousands-file retention test")
	}
	const fileCount = 2049
	fixture := make(map[string]string, fileCount)
	for i := 0; i < fileCount; i++ {
		fixture[fmt.Sprintf("src/c%05d.ts", i)] = fmt.Sprintf(
			"export class C%05d { run(): void {} }\n", i)
	}
	g, dir := buildFixture(t, fixture)
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	maxStats := factPageStats{}
	pageObservations := 0
	p.observePage = func(stats factPageStats) {
		pageObservations++
		if stats.Files > maxStats.Files {
			maxStats.Files = stats.Files
		}
		if stats.Facts > maxStats.Facts {
			maxStats.Facts = stats.Facts
		}
		if stats.Bytes > maxStats.Bytes {
			maxStats.Bytes = stats.Bytes
		}
		if stats.CacheNodes > maxStats.CacheNodes {
			maxStats.CacheNodes = stats.CacheNodes
		}
		if stats.CacheEdges > maxStats.CacheEdges {
			maxStats.CacheEdges = stats.CacheEdges
		}
		if stats.CacheNames > maxStats.CacheNames {
			maxStats.CacheNames = stats.CacheNames
		}
	}
	result, err := p.EnrichRepo(g, "", dir)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Greater(t, pageObservations, fileCount/tstypesFactPageFiles)
	assert.LessOrEqual(t, maxStats.Files, tstypesFactPageFiles)
	assert.LessOrEqual(t, maxStats.Bytes, tstypesFactPageBytes)
	// Each page owns only its local symbols/frontier. This threshold is far
	// below the 4k+ symbols in the corpus and catches accidental corpus cache
	// retention without coupling the test to extractor minutiae.
	assert.Less(t, maxStats.CacheNodes, 512)
	assert.Less(t, maxStats.CacheNames, 256)
	assert.Equal(t, result.SymbolsTotal, result.SymbolsCovered)
	assert.Equal(t, 100.0, result.CoveragePercent)
}

func TestStreamedRepoCancellationCleansSpoolAndHasNoPostReturnMutation(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts": tsSvc,
		"src/app.ts": `import { Svc } from "./svc";
export function main(): void { const s = new Svc(); s.run(); }
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	var spoolPath string
	p.observeSpool = func(path string) {
		spoolPath = path
		cancel()
	}
	before := g.EdgeCount()
	result, err := p.EnrichRepoContext(ctx, g, "", dir, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Partial)
	assert.NotEmpty(t, spoolPath)
	_, statErr := os.Stat(spoolPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	assert.Equal(t, before, g.EdgeCount())
	time.Sleep(25 * time.Millisecond)
	assert.Equal(t, before, g.EdgeCount(), "worker mutated graph after EnrichRepoContext returned")
}

func enrichWholeCorpusForTest(t *testing.T, p *Provider, g graph.Store, repoPrefix, repoRoot string) *semantic.EnrichResult {
	t.Helper()
	refs := languageFiles(g, p.spec, repoPrefix, repoRoot)
	all := make([]*fileFacts, 0, len(refs))
	analyzed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		facts, err := analyzeFile(p.spec, ref)
		require.NoError(t, err)
		if facts != nil {
			all = append(all, facts)
			analyzed[facts.file] = true
		}
	}
	result := &semantic.EnrichResult{Provider: p.Name(), Language: p.spec.Languages[0]}
	mu := g.ResolveMutex()
	mu.Lock()
	ap := newApplier(g, p.spec, p.Name())
	ap.applyAll(all, result)
	ap.flush()
	p.countCoverage(g, repoPrefix, analyzed, result)
	mu.Unlock()
	return result
}

func semanticSnapshot(g graph.Store) []string {
	var snapshot []string
	for _, edge := range g.AllEdges() { // test-only exact parity oracle
		if edge == nil {
			continue
		}
		source, strategy := "", ""
		if edge.Meta != nil {
			source, _ = edge.Meta["semantic_source"].(string)
			strategy = fmt.Sprint(edge.Meta["resolution_strategy"])
		}
		snapshot = append(snapshot, fmt.Sprintf("e|%s|%s|%s|%d|%s|%.4f|%s|%s|%s",
			edge.From, edge.To, edge.Kind, edge.Line, edge.Origin, edge.Confidence, edge.ConfidenceLabel, source, strategy))
	}
	for _, node := range g.AllNodes() { // test-only exact parity oracle
		if node == nil || node.Meta == nil {
			continue
		}
		var fields []string
		for _, key := range []string{"return_type", "semantic_source", "semantic_type"} {
			if value, ok := node.Meta[key]; ok {
				fields = append(fields, key+"="+fmt.Sprint(value))
			}
		}
		if len(fields) > 0 {
			snapshot = append(snapshot, "n|"+node.ID+"|"+strings.Join(fields, ","))
		}
	}
	sort.Strings(snapshot)
	return snapshot
}
