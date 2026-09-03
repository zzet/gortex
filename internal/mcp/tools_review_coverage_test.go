package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// coverageProbeServer builds a bare Server around g — enough for the
// coverage-attestation probe, which reads nothing but the graph.
func coverageProbeServer(g graph.Store) *Server {
	return &Server{graph: g}
}

func testSymbol(id, file string, meta map[string]any) *graph.Node {
	return &graph.Node{
		ID: id, Name: id, Kind: graph.KindFunction, FilePath: file, Meta: meta,
	}
}

// TestCoverageKnownFromTestStamp is the regression for issue #350: a vitest
// probe co-located with the code it exercises carries no path fragment the
// old probe looked for beyond the filename, and an annotation-driven test
// carries none at all. The indexer stamps both is_test, so the attestation
// must follow the stamp rather than a hardcoded fragment list.
func TestCoverageKnownFromTestStamp(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol("src/production.ts::resolveEntityOrderBy", "src/production.ts", nil))
	g.AddNode(testSymbol(
		"src/production.test.ts::expectResolveEntityOrderByBehaviors",
		"src/production.test.ts",
		map[string]any{"is_test": true, "test_runner": "vitest"},
	))
	s := coverageProbeServer(g)

	require.True(t, s.coverageKnownForDiff(context.Background(), "", []string{"src/production.ts", "src/production.test.ts"}),
		"a stamped vitest probe attests coverage for the changed TypeScript")
}

// TestCoverageKnownAnnotationDrivenTest pins the case the fragment probe
// could never see: a Rust #[test] in a production-path file. It has no test
// filename and no test directory — only the indexer's stamp.
func TestCoverageKnownAnnotationDrivenTest(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol("src/lib.rs::parse", "src/lib.rs", nil))
	g.AddNode(testSymbol("src/lib.rs::parses_empty", "src/lib.rs", map[string]any{"is_test": true}))

	require.True(t, coverageProbeServer(g).coverageKnownForDiff(context.Background(), "", []string{"src/lib.rs"}))
}

// TestCoverageUnknownWhenIndexExcludesTests pins the behaviour the
// attestation exists for: an index whose config drops the language's test
// files must NOT read as "untested", it must read as unknown.
func TestCoverageUnknownWhenIndexExcludesTests(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol("pkg/foo.go::Bar", "pkg/foo.go", nil))

	require.False(t, coverageProbeServer(g).coverageKnownForDiff(context.Background(), "", []string{"pkg/foo.go"}))
}

// TestCoverageKnownIsPerLanguage pins that attesting one language does not
// attest another: a repo that indexes Go tests but excludes its TypeScript
// tests can still only speak for the Go half of a polyglot diff.
func TestCoverageKnownIsPerLanguage(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol("pkg/foo.go::Bar", "pkg/foo.go", nil))
	g.AddNode(testSymbol("pkg/foo_test.go::TestBar", "pkg/foo_test.go", nil))
	g.AddNode(testSymbol("src/app.ts::render", "src/app.ts", nil))
	s := coverageProbeServer(g)

	require.True(t, s.coverageKnownForDiff(context.Background(), "", []string{"pkg/foo.go"}))
	require.False(t, s.coverageKnownForDiff(context.Background(), "", []string{"pkg/foo.go", "src/app.ts"}),
		"Go tests say nothing about whether TypeScript tests are indexed")
}

// TestCoverageKnownWindowsSeparators pins the Windows shape from the issue
// report: graph nodes carry backslash-separated paths while git emits
// forward slashes, and both spellings must classify the same.
func TestCoverageKnownWindowsSeparators(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol(`src\production.ts::resolve`, `src\production.ts`, nil))
	g.AddNode(testSymbol(`src\__tests__\production.ts::probe`, `src\__tests__\production.ts`, nil))

	require.True(t, coverageProbeServer(g).coverageKnownForDiff(context.Background(), "", []string{"src/production.ts"}))
}

// TestTestIndexProbeResetsOnReindex pins the staleness fix: the probe is
// cached per repo prefix, and a probe that ran before the test-edge pass
// stamped its symbols used to pin "no tests" for the daemon's lifetime.
func TestTestIndexProbeResetsOnReindex(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol("pkg/foo.go::Bar", "pkg/foo.go", nil))
	s := coverageProbeServer(g)

	require.False(t, s.coverageKnownForDiff(context.Background(), "", []string{"pkg/foo.go"}), "mid-warmup: no test symbols yet")

	g.AddNode(testSymbol("pkg/foo_test.go::TestBar", "pkg/foo_test.go", nil))
	require.False(t, s.coverageKnownForDiff(context.Background(), "", []string{"pkg/foo.go"}), "still serving the cached answer")

	s.resetTestIndexProbe()
	require.True(t, s.coverageKnownForDiff(context.Background(), "", []string{"pkg/foo.go"}),
		"the reset must let the next review see the test symbols the reindex landed")
}

// TestCoverageKnownIgnoresNonCodeChanges pins that a docs-only changeset
// carries no language with a test convention to attest.
func TestCoverageKnownIgnoresNonCodeChanges(t *testing.T) {
	g := graph.New()
	g.AddNode(testSymbol("pkg/foo_test.go::TestBar", "pkg/foo_test.go", nil))

	require.False(t, coverageProbeServer(g).coverageKnownForDiff(context.Background(), "", []string{"README.md", "docs/guide.md"}))
}
