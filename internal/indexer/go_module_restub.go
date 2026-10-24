package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

func goModuleRestubTarget(e *graph.Edge, fallback string) string {
	if spec, ok := e.Meta["go_import"].(string); ok && spec != "" && e.CrossRepo && strings.HasSuffix(e.FilePath, ".go") {
		return graph.UnresolvedMarker + "extern::" + spec
	}
	return fallback
}

// Park only module-bound incoming references from surviving callers before a
// whole provider replacement/removal. Eviction otherwise deletes these edges.
func restubGoModuleRepoIncoming(g graph.Store, repo string) {
	if repo == "" || len(graph.ReadRepoFilePaths(g, repo, "", []string{"go"}, nil)) == 0 {
		return // persisted Go files survive missing manifests and offline checkouts
	}
	var ids []string
	flush := func() {
		var batch []graph.EdgeReindex
		for _, incoming := range g.GetInEdgesByNodeIDs(ids) {
			for _, e := range incoming {
				if !graph.IsResolvableRefEdge(e.Kind) || graph.RepoPrefixOfID(e.From) == repo {
					continue
				}
				stub := goModuleRestubTarget(e, "")
				if stub == "" {
					continue
				}
				oldTo := e.To
				graph.StashRestubProvenance(e)
				e.To = stub
				e.CrossRepo, e.ConfidenceLabel = false, ""
				batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			}
		}
		g.ReindexEdges(batch)
		ids = ids[:0]
	}
	for n := range graph.RepoNodeIdentitiesSeq(g, []string{repo}, graph.KindFunction, graph.KindType, graph.KindInterface) {
		ids = append(ids, n.ID)
		if len(ids) == 512 {
			flush()
		}
	}
	if len(ids) > 0 {
		flush()
	}
}
