package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/testpath"
)

// isTestFilePath reports whether a source path follows a recognised test
// convention.
//
// PURPOSE: let the Temporal orphan detector drop dispatches that
// originate in test fixtures (the dominant broken_dispatch false
// positive) without depending on Node.Meta test flags — which are not
// re-stamped on the incremental-reindex path.
//
// RATIONALE: the convention table lives in the stdlib-only leaf
// internal/testpath. The resolver cannot import the indexer package
// (indexer → resolver is the established import direction; the reverse
// would be a cycle), so the predicate used to be duplicated here — and the
// copies drifted. Both now delegate to the one definition.
//
// KEYWORDS: test-file, predicate, temporal, broken_dispatch, no-cycle
func isTestFilePath(path string) bool { return testpath.IsTestFile(path) }

// noteRetargetedCall records the caller file of a calls edge a resolution
// pass just bound, when that caller is test-classified — by file path or
// by an is_test stamp on the source symbol (annotation-marked tests can
// live outside test-named paths). The test projection skips unresolved
// calls, so a call that binds later must have its caller's projection
// reconciled; the drained frontier (TakeRetargetedTestCallFiles) is how
// the indexer learns which callers those are. Bounded: only calls edges,
// only test-classified callers, one entry per file.
func (r *Resolver) noteRetargetedCall(e *graph.Edge) {
	if e == nil || e.Kind != graph.EdgeCalls || e.FilePath == "" {
		return
	}
	if graph.IsUnresolvedTarget(e.To) {
		return
	}
	if !isTestFilePath(e.FilePath) && !nodeStampedTest(r.cachedGetNode(e.From)) {
		return
	}
	r.retargetedMu.Lock()
	if r.retargetedTestCallFiles == nil {
		r.retargetedTestCallFiles = make(map[string]struct{})
	}
	r.retargetedTestCallFiles[e.FilePath] = struct{}{}
	r.retargetedMu.Unlock()
}

// TakeRetargetedTestCallFiles drains the accumulated test-caller frontier,
// sorted for determinism. The caller re-runs the scoped test projection
// over the returned files.
func (r *Resolver) TakeRetargetedTestCallFiles() []string {
	r.retargetedMu.Lock()
	defer r.retargetedMu.Unlock()
	if len(r.retargetedTestCallFiles) == 0 {
		return nil
	}
	files := make([]string, 0, len(r.retargetedTestCallFiles))
	for file := range r.retargetedTestCallFiles {
		files = append(files, file)
	}
	r.retargetedTestCallFiles = nil
	sort.Strings(files)
	return files
}

func nodeStampedTest(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	value, _ := n.Meta["is_test"].(bool)
	return value
}
