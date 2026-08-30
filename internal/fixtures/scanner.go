// Package fixtures detects test-fixture files and surfaces them as
// KindFixture nodes so agents can answer "what fixtures does this
// repo have" or "is this golden file still referenced" with a graph
// query rather than a directory walk.
//
// Scope (v1): per-file path-based detection. A file qualifies as a
// fixture when its path contains a `testdata/` segment — Go's
// well-known convention for test data, also adopted by many other
// ecosystems (Python, Rust, JS). The reference edge from test
// functions to fixtures (`EdgeReferences` per the broader coverage
// spec) is a v2 follow-up; today the fixture node lands without
// an inbound link, which still serves enumeration and cleanup
// queries.
package fixtures

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// IsFixturePath reports whether a repo-relative file path lives
// under a `testdata/` directory. Forward-slash and back-slash
// separators are both accepted so Windows-indexed paths produce the
// same result as POSIX-indexed ones. Path-only — does not consult
// the filesystem.
func IsFixturePath(filePath string) bool {
	p := filepath.ToSlash(filePath)
	if p == "" {
		return false
	}
	// Match `testdata` as a whole segment, not as a prefix or
	// substring of a longer name (e.g. `mytestdata/`). The two
	// patterns are: leading `testdata/` and any `/testdata/` mid-
	// path.
	if strings.HasPrefix(p, "testdata/") {
		return true
	}
	return strings.Contains(p, "/testdata/")
}

// BuildGraphArtifacts emits a KindFixture node for the given file.
// Returns nil when the path doesn't qualify so the caller can
// unconditionally invoke this without an explicit IsFixturePath
// guard at every call site.
//
// filePath is the unprefixed repo-relative path; applyRepoPrefix
// downstream handles multi-repo namespacing. The fixture node
// shares the same ID as the file node so a single graph query can
// scope to fixtures via kind, and the existing file-shaped node
// from the language extractor (when present) carries its own
// outgoing edges.
//
// We deliberately do not emit a separate node ID — the fixture is
// the file. Keeping a single ID keeps cross-referencing simple
// (any edge that lands on the file path also lands on the fixture
// classification) and avoids the de-dup gymnastics that emitting a
// twin synthetic ID would require. That sharing only holds when the
// spelling matches the extractor's relPath exactly (OS-native
// separators on Windows), so filePath is preserved verbatim.
func BuildGraphArtifacts(filePath, language string) []*graph.Node {
	if !IsFixturePath(filePath) {
		return nil
	}
	return []*graph.Node{{
		ID:       filePath,
		Kind:     graph.KindFixture,
		Name:     filepath.Base(filePath),
		FilePath: filePath,
		Language: language,
		Meta: map[string]any{
			"fixture": true,
		},
	}}
}

// TestContractSource reports the test-fixture category of the given
// path, or "" if the path is production code. Used by the contracts
// pipeline to tag — not drop — contracts emitted from synthetic
// fixtures so the dashboard can filter them out by default while
// still keeping them in the graph for drift checks (e.g. a test
// pinned to an old API contract that production has since changed).
//
// Categories:
//   - "testdata"       — paths under testdata/ (Go convention)
//   - "bench_fixtures" — paths under bench/fixtures/ (gortex bench corpus)
//   - "js_fixtures"    — paths under __fixtures__/ (TS/JS convention)
//
// The path may be repo-prefixed (e.g. "gortex/bench/fixtures/...");
// we match the segment regardless of leading prefix. Path-only — does
// not consult the filesystem.
func TestContractSource(filePath string) string {
	if IsFixturePath(filePath) {
		return "testdata"
	}
	p := filepath.ToSlash(filePath)
	if p == "" {
		return ""
	}
	for _, c := range []struct {
		seg, label string
	}{
		{"bench/fixtures/", "bench_fixtures"},
		{"__fixtures__/", "js_fixtures"},
	} {
		if strings.HasPrefix(p, c.seg) || strings.Contains(p, "/"+c.seg) {
			return c.label
		}
	}
	return ""
}

// ReclassifyFileToFixture rewrites an existing KindFile node to
// KindFixture when its path qualifies. Used by the indexer's
// per-file coverage step so the language extractor's emitted file
// node is upgraded in place rather than producing two nodes that
// share an ID. Returns true when reclassification fired so callers
// can skip BuildGraphArtifacts for the same path.
func ReclassifyFileToFixture(node *graph.Node) bool {
	if node == nil || node.Kind != graph.KindFile {
		return false
	}
	if !IsFixturePath(node.FilePath) {
		return false
	}
	node.Kind = graph.KindFixture
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	node.Meta["fixture"] = true
	return true
}
