package indexer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// slowExtractor is a parser.Extractor whose Extract sleeps, used to
// exercise the per-file extraction timeout.
type slowExtractor struct{ delay time.Duration }

func (s *slowExtractor) Language() string     { return "slow" }
func (s *slowExtractor) Extensions() []string { return []string{".slow"} }
func (s *slowExtractor) Extract(filePath string, _ []byte) (*parser.ExtractionResult, error) {
	time.Sleep(s.delay)
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: filePath, Kind: graph.KindFile, Name: filePath}},
	}, nil
}

func TestExtractWithTimeout_NoBudget(t *testing.T) {
	idx := newTestIndexer(graph.New()) // MaxExtractMillis = 0
	r, err := idx.extractWithTimeoutDone(&slowExtractor{delay: 5 * time.Millisecond}, "x.slow", []byte("x"), nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestExtractWithTimeout_FastFileUnderBudget(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.config.MaxExtractMillis = 2000
	r, err := idx.extractWithTimeoutDone(&slowExtractor{delay: 5 * time.Millisecond}, "x.slow", []byte("x"), nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestExtractWithTimeout_SlowFileTimesOut(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.config.MaxExtractMillis = 50
	_, err := idx.extractWithTimeoutDone(&slowExtractor{delay: 800 * time.Millisecond}, "x.slow", []byte("x"), nil)
	require.ErrorIs(t, err, errExtractTimeout)
}

func TestSizeSkipNode(t *testing.T) {
	n := sizeSkipNode(skippedFile{relPath: "gen/big.ts", lang: "typescript", size: 9_000_000}, 2_000_000)
	require.Equal(t, graph.KindFile, n.Kind)
	require.Equal(t, "gen/big.ts", n.ID)
	require.Equal(t, "big.ts", n.Name)
	require.Equal(t, "typescript", n.Language)
	require.Equal(t, true, n.Meta["skipped_due_to_size"])
	require.Equal(t, int64(9_000_000), n.Meta["file_size_bytes"])
}

func TestLargeFileReadParallelism(t *testing.T) {
	require.Equal(t, 1, largeFileReadParallelism(0))
	require.Equal(t, 1, largeFileReadParallelism(1))
	require.Equal(t, 2, largeFileReadParallelism(2))
	require.Equal(t, 2, largeFileReadParallelism(20))
	require.Equal(t, int64(16<<20), largeFileReadThresholdBytes)
}

func TestTimeoutSkipResult(t *testing.T) {
	r := timeoutSkipResult("huge.ts", "typescript", 10000)
	require.Len(t, r.Nodes, 1)
	require.Equal(t, true, r.Nodes[0].Meta["skipped_due_to_timeout"])
	require.Equal(t, 10000, r.Nodes[0].Meta["extract_budget_ms"])
}

// TestIndex_SizeSkipTelemetry verifies a file dropped by the size cap
// becomes a synthetic node carrying skip telemetry instead of vanishing.
func TestIndex_SizeSkipTelemetry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "small.go"), "package main\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "big.go"),
		"package main\n"+strings.Repeat("// filler line for bulk\n", 5000))

	g := graph.New()
	idx := newTestIndexer(g)
	idx.config.MaxFileSize = 1024 // big.go (~115 KB) is over the cap

	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 1, result.FileCount)    // only small.go parsed
	require.Equal(t, 1, result.SkippedFiles) // big.go skipped

	n := g.GetNode("big.go")
	require.NotNil(t, n, "size-skipped file must still have a node")
	require.Equal(t, graph.KindFile, n.Kind)
	require.Equal(t, true, n.Meta["skipped_due_to_size"])
	require.Equal(t, int64(1024), n.Meta["max_file_size_bytes"])
}

// TestIndex_ExtractTimeoutSkip verifies a file whose extraction blows
// the time budget is skipped with a synthetic timeout node and the
// index pass still completes.
func TestIndex_ExtractTimeoutSkip(t *testing.T) {
	reg := parser.NewRegistry()
	reg.Register(&slowExtractor{delay: 800 * time.Millisecond})
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.MaxExtractMillis = 80
	idx := New(graph.New(), reg, cfg, zap.NewNop())

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.slow"), "some content")

	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 1, result.SkippedFiles)

	n := idx.graph.GetNode("a.slow")
	require.NotNil(t, n)
	require.Equal(t, true, n.Meta["skipped_due_to_timeout"])
}

func TestIndexFile_SizeSkip(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	idx := newTestIndexer(g)
	idx.config.MaxFileSize = 512
	if _, err := idx.Index(dir); err != nil {
		t.Fatalf("index: %v", err)
	}

	big := filepath.Join(dir, "huge.go")
	writeFile(t, big, "package main\n"+strings.Repeat("// x\n", 2000))
	require.NoError(t, idx.indexFile(big, false))

	n := g.GetNode("huge.go")
	require.NotNil(t, n)
	require.Equal(t, true, n.Meta["skipped_due_to_size"])
}

// TestSkipNodes_CarryUnifiedReason verifies every skip-node shape stamps a
// uniform skip_reason so index_health can roll them up by reason.
func TestSkipNodes_CarryUnifiedReason(t *testing.T) {
	cases := []struct {
		name   string
		node   *graph.Node
		reason string
	}{
		{"size", sizeSkipNode(skippedFile{relPath: "big.go", lang: "go", size: 1 << 20}, 1024), "size"},
		{"timeout", timeoutSkipResult("slow.go", "go", 500).Nodes[0], "timeout"},
		{"minified", minifiedSkipResult("bundle.js", "javascript", "long-lines").Nodes[0], "minified"},
		{"binary", binarySkipResult("cache.pkl", "pkl", "binary source").Nodes[0], "binary"},
		{"parse_failed", parseFailedSkipResult("bad.go", "go", errors.New("boom")).Nodes[0], "parse_failed"},
		{"parse_panic", quarantineResult("crash.go", "go", "panic").Nodes[0], "parse_panic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, graph.KindFile, c.node.Kind)
			require.Equal(t, c.reason, c.node.Meta["skip_reason"])
		})
	}
}

func TestParseFailedSkipResult_RecordsError(t *testing.T) {
	r := parseFailedSkipResult("bad.go", "go", errors.New("unexpected token"))
	require.Len(t, r.Nodes, 1)
	n := r.Nodes[0]
	require.Equal(t, "bad.go", n.FilePath)
	require.Equal(t, "bad.go", n.Name)
	require.Equal(t, "parse_failed", n.Meta["skip_reason"])
	require.Equal(t, "unexpected token", n.Meta["parse_error"])
}

// TestIndex_ParseFailedSkipTelemetry verifies a file that fails to parse
// during a FULL index stays visible as a parse_failed skip node instead of
// vanishing — the safe counterpart to the live-modify path, which keeps a
// file's prior nodes through a transient failure (see
// TestPatchGraphModify_ParseFailureKeepsPriorNodes).
func TestIndex_ParseFailedSkipTelemetry(t *testing.T) {
	idx, ext, _ := newToggleIndexer(t)
	ext.setFail(true) // every extraction returns an error

	dir := t.TempDir()
	idx.SetRootPath(dir)
	writeFile(t, filepath.Join(dir, "broken.fk"), "this does not parse")

	_, err := idx.Index(dir)
	require.NoError(t, err)

	n := idx.graph.GetNode("broken.fk")
	require.NotNil(t, n, "a full-index parse failure must leave a visible skip node")
	require.Equal(t, graph.KindFile, n.Kind)
	require.Equal(t, "parse_failed", n.Meta["skip_reason"])
}

// countingExtractor is a parser.Extractor that counts Extract calls, so
// a test can prove the binary guard fired BEFORE any extraction work.
// The counter lives on the extractor instance so each test owns its own —
// a package-level counter made the assertions order- and run-count-
// dependent (failing under -count=2 and -shuffle).
type countingExtractor struct{ calls atomic.Int32 }

func (e *countingExtractor) Language() string     { return "fake" }
func (e *countingExtractor) Extensions() []string { return []string{".pkl", ".fk"} }
func (e *countingExtractor) Extract(filePath string, _ []byte) (*parser.ExtractionResult, error) {
	e.calls.Add(1)
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: filePath, Kind: graph.KindFile, Name: filePath}},
	}, nil
}

// TestIndex_BinarySkipTelemetry verifies a binary payload claimed by a
// registered language (a .pkl tool cache against the Pkl extractor, the
// reported case) becomes a synthetic skip node instead of feeding
// tree-sitter's error recovery, and that the pass still completes. The
// custom registry isolates the guard from the real Pkl grammar.
func TestIndex_BinarySkipTelemetry(t *testing.T) {
	ext := &countingExtractor{}
	reg := parser.NewRegistry()
	reg.Register(ext)
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(graph.New(), reg, cfg, zap.NewNop())

	dir := t.TempDir()
	binary := append([]byte("\x80\x04\x95"), 0x00, 0x00, 0x01, 0x02)
	writeFile(t, filepath.Join(dir, "raw_document_symbols.pkl"), string(binary))
	writeFile(t, filepath.Join(dir, "text.fk"), "real source")

	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 1, result.SkippedFiles)

	n := idx.graph.GetNode("raw_document_symbols.pkl")
	require.NotNil(t, n, "a binary skip must leave a visible node")
	require.Equal(t, graph.KindFile, n.Kind)
	require.Equal(t, true, n.Meta["skipped_due_to_binary"])
	require.NotEmpty(t, n.Meta["binary_reason"])
	// The extractor never ran on the binary bytes.
	require.Equal(t, int32(1), ext.calls.Load())

	// The skip is a successful read: no failure row is recorded, so the
	// file does not re-enter the failure-ledger retry path on the next
	// reconcile. (fileIndexFailureError returns the errFileVersionChanged
	// sentinel for a path with no ledger row — "nothing failed".)
	for _, graphPath := range idx.fileIndexFailurePaths() {
		require.NotEqual(t, "raw_document_symbols.pkl", graphPath,
			"a binary skip must not land in the failure ledger")
	}
}

// TestIndexFile_BinarySkip mirrors the skip onto the single-file
// watcher path: a save of a binary file claimed by an extension yields
// a synthetic node and a clean return, not a parse failure.
func TestIndexFile_BinarySkip(t *testing.T) {
	ext := &countingExtractor{}
	reg := parser.NewRegistry()
	reg.Register(ext)
	cfg := config.Default().Index
	idx := New(graph.New(), reg, cfg, zap.NewNop())

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.fk"), "real source")
	if _, err := idx.Index(dir); err != nil {
		t.Fatalf("index: %v", err)
	}

	before := ext.calls.Load()
	binary := filepath.Join(dir, "raw_document_symbols.pkl")
	writeFile(t, binary, "\x80\x04\x95\x00\x00")
	require.NoError(t, idx.indexFile(binary, false))

	n := idx.graph.GetNode("raw_document_symbols.pkl")
	require.NotNil(t, n)
	require.Equal(t, true, n.Meta["skipped_due_to_binary"])
	// The extractor never ran on the binary bytes.
	require.Equal(t, before, ext.calls.Load())
}

func walkedFilePaths(fs []walkedFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.path
	}
	return out
}

// TestSortBySizeDesc pins the cold-index dispatch ordering: the walked
// slice must come out largest-first, equal sizes must keep their input
// order (stable), and the sort must be a pure permutation — never adding
// or dropping a file.
func TestSortBySizeDesc(t *testing.T) {
	tests := []struct {
		name     string
		in       []walkedFile
		wantPath []string
	}{
		{
			name:     "empty",
			in:       []walkedFile{},
			wantPath: []string{},
		},
		{
			name:     "single",
			in:       []walkedFile{{path: "a", size: 10}},
			wantPath: []string{"a"},
		},
		{
			name: "unsorted becomes descending",
			in: []walkedFile{
				{path: "small", size: 1},
				{path: "big", size: 100},
				{path: "mid", size: 50},
			},
			wantPath: []string{"big", "mid", "small"},
		},
		{
			name: "already descending stays put",
			in: []walkedFile{
				{path: "big", size: 100},
				{path: "mid", size: 50},
				{path: "small", size: 1},
			},
			wantPath: []string{"big", "mid", "small"},
		},
		{
			name: "equal sizes keep input order (stable)",
			in: []walkedFile{
				{path: "first", size: 42},
				{path: "second", size: 42},
				{path: "third", size: 42},
			},
			wantPath: []string{"first", "second", "third"},
		},
		{
			name: "ties within mixed sizes keep input order",
			in: []walkedFile{
				{path: "tie-a", size: 10},
				{path: "huge", size: 999},
				{path: "tie-b", size: 10},
				{path: "tie-c", size: 10},
				{path: "mid", size: 50},
			},
			wantPath: []string{"huge", "mid", "tie-a", "tie-b", "tie-c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]walkedFile(nil), tc.in...)

			sortBySizeDesc(tc.in)

			assert.Equal(t, tc.wantPath, walkedFilePaths(tc.in), "dispatch order")

			// Sizes must be non-increasing across the whole slice.
			for i := 1; i < len(tc.in); i++ {
				assert.GreaterOrEqual(t, tc.in[i-1].size, tc.in[i].size,
					"sizes must be non-increasing at index %d", i)
			}

			// Pure permutation: identical multiset of files before and
			// after, so the sort can never change which files get indexed.
			assert.ElementsMatch(t, walkedFilePaths(before), walkedFilePaths(tc.in),
				"sort must not add or drop files")
		})
	}
}

// TestColdIndexLargestFirstKeepsGraphIdentical confirms the size-first
// dispatch only reorders work: a cold index over a fixture whose lexical
// walk order differs from its size order produces a complete, identical
// graph run-to-run. aaa.go is tiny and walked first; zzz.go is the
// largest and walked last, so the descending-size sort dispatches them
// in the opposite order from the walk.
func TestColdIndexLargestFirstKeepsGraphIdentical(t *testing.T) {
	mkFixture := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "aaa.go"),
			"package p\n\nfunc Tiny() {}\n")
		writeFile(t, filepath.Join(dir, "mmm.go"),
			"package p\n\nfunc Mid() string { return \"x\" }\n")
		// Deliberately the largest file: walked last, dispatched first.
		var b strings.Builder
		b.WriteString("package p\n\n")
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&b, "func Big%d() int { return %d }\n", i, i)
		}
		writeFile(t, filepath.Join(dir, "zzz.go"), b.String())
		return dir
	}

	index := func(t *testing.T) *IndexResult {
		t.Helper()
		g := graph.New()
		idx := newTestIndexer(g)
		res, err := idx.Index(mkFixture(t))
		require.NoError(t, err)
		// Every file survived the reordering.
		assert.Equal(t, 3, res.FileCount)
		// Symbols from both the first-walked tiny file and the
		// last-walked large file are present.
		assert.NotEmpty(t, g.FindNodesByName("Tiny"))
		assert.NotEmpty(t, g.FindNodesByName("Big0"))
		assert.NotEmpty(t, g.FindNodesByName("Big199"))
		return res
	}

	a := index(t)
	b := index(t)

	assert.Equal(t, a.FileCount, b.FileCount)
	assert.Equal(t, a.NodeCount, b.NodeCount,
		"cold-index node count must be identical regardless of dispatch order")
	assert.Equal(t, a.EdgeCount, b.EdgeCount,
		"cold-index edge count must be identical regardless of dispatch order")
}
