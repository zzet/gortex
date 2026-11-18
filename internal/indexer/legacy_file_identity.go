package indexer

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// legacyFilePath is deliberately host-specific. On POSIX a backslash is a
// legal filename character, not an alternative spelling of a directory slash.
// Old Windows extractors used native relative paths beneath the repo prefix.
func (idx *Indexer) legacyFilePath(graphPath string) string {
	if _, ok := idx.graph.(graph.FileIdentityBatchEvicter); !ok {
		return ""
	}
	if filepath.Separator != '\\' {
		return ""
	}
	rel := strings.TrimPrefix(graphPath, idx.prefixPath(""))
	if !strings.Contains(rel, "/") {
		return ""
	}
	return idx.prefixPath(filepath.FromSlash(rel))
}

// legacyFileCandidates only probes exact file identities in the requested
// discovery frontier. An unchanged explicit reindex must reach the repair
// before the mtime/Merkle and extraction-fingerprint shortcuts.
func (idx *Indexer) legacyFileCandidates(diskFiles map[string]bool) []string {
	if filepath.Separator != '\\' {
		return nil
	}
	var ids []string
	rels := make(map[string]string)
	for rel := range diskFiles {
		if native := idx.legacyFilePath(idx.prefixPath(rel)); native != "" {
			ids = append(ids, native)
			rels[native] = rel
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var paths []string
	for id, node := range idx.graph.GetNodesByIDs(ids) {
		if node != nil && (node.FilePath == id || node.FilePath == idx.prefixPath(rels[id])) {
			paths = append(paths, filepath.Join(idx.rootPath, filepath.FromSlash(rels[id])))
		}
	}
	return paths
}

// legacyNodesByPath shares the existing bounded chunk read shape. POSIX and
// root files issue no lookup; Windows subdirectories use one indexed batch.
func (idx *Indexer) legacyNodesByPath(graphPaths []string) map[string][]*graph.Node {
	var nativePaths []string
	for _, path := range graphPaths {
		if native := idx.legacyFilePath(path); native != "" {
			nativePaths = append(nativePaths, native)
		}
	}
	if len(nativePaths) == 0 {
		return nil
	}
	return idx.graph.GetFileNodesByPaths(nativePaths)
}

func (idx *Indexer) filePriorWithLegacy(graphPath string, prior []*graph.Node) ([]*graph.Node, []string, string) {
	native := idx.legacyFilePath(graphPath)
	if native == "" {
		return prior, nil, ""
	}
	return mergeLegacyFilePrior(native, prior, idx.legacyNodesByPath([]string{graphPath})[native])
}

// mergeLegacyFilePrior admits only ordinary IDs rooted in this exact file.
// A shared license/team/module can be anchored at the old scalar path, but
// its identity is outside this prefix and must not enter endpoint eviction.
func mergeLegacyFilePrior(native string, prior, legacyNodes []*graph.Node) ([]*graph.Node, []string, string) {
	if native == "" {
		return prior, nil, ""
	}
	seen := make(map[string]bool, len(prior))
	var ids []string
	for _, node := range prior {
		if node != nil {
			seen[node.ID] = true
			if node.ID == native || strings.HasPrefix(node.ID, native+"::") {
				ids = append(ids, node.ID)
			}
		}
	}
	for _, node := range legacyNodes {
		if node == nil || (node.ID != native && !strings.HasPrefix(node.ID, native+"::")) {
			continue
		}
		if !seen[node.ID] {
			prior = append(prior, node)
			ids = append(ids, node.ID)
			seen[node.ID] = true
		}
	}
	if len(ids) == 0 {
		return prior, nil, ""
	}
	return prior, ids, native
}

func (idx *Indexer) evictFileIdentities(paths, legacyIDs, legacyPaths []string) {
	if len(legacyIDs) == 0 {
		evictFilesBatched(idx.graph, paths)
		return
	}
	// The optional capability is checked before discovery/staging; never widen
	// an unsupported store's eviction to a whole native file-path bucket.
	idx.graph.(graph.FileIdentityBatchEvicter).EvictFilesWithNodeIDs(paths, legacyIDs)
	evictDetachedRecordedEdges(idx.graph, append(append([]string(nil), paths...), legacyPaths...))
	idx.deleteIncrementalSidecars(legacyPaths)
	idx.deleteRefFactsForFiles(idx.repoPrefix, legacyPaths)
	idx.clearIncrementalContent(legacyPaths)
	idx.deleteEnrichmentByNodeIDs(legacyIDs)
}

// legacyReplacements recognizes identity-equivalent declarations only. The
// normal restub frontier still checks their shapes, visibility and parameters.
func legacyReplacements(stage *incrementalBatchStage) map[string]string {
	if stage.legacyPath == "" {
		return nil
	}
	freshByID := make(map[string]*graph.Node, len(stage.result.Nodes))
	for _, fresh := range stage.result.Nodes {
		if fresh != nil {
			freshByID[fresh.ID] = fresh
		}
	}
	replacements := make(map[string]string)
	for _, node := range stage.priorNodes {
		if node == nil || (node.ID != stage.legacyPath && !strings.HasPrefix(node.ID, stage.legacyPath+"::")) {
			continue
		}
		id := stage.graphPath + strings.TrimPrefix(node.ID, stage.legacyPath)
		if fresh := freshByID[id]; fresh != nil && fresh.Kind == node.Kind && fresh.Name == node.Name {
			replacements[node.ID] = id
		}
	}
	return replacements
}

func extractionNodes(result *parser.ExtractionResult) []*graph.Node {
	if result == nil {
		return nil
	}
	return result.Nodes
}

// canonicalizeLegacyCarried runs after the existing frontier rules selected
// safe rows. In particular, a changed parameter first moves back to its owner,
// and changed declarations' outgoing dataflow first moves to the call stub.
// Rewrite only endpoints actually evicted as ordinary identities, never nodes
// that merely share the legacy FilePath scalar (licenses, teams, modules).
func canonicalizeLegacyCarried(stages []*incrementalBatchStage, evicted map[string]struct{}, carried []*graph.Edge) []*graph.Edge {
	var replacements map[string]string
	for _, stage := range stages {
		if stage.legacyPath == "" {
			continue
		}
		if replacements == nil {
			replacements = make(map[string]string)
		}
		for _, node := range stage.priorNodes {
			if node == nil {
				continue
			}
			if _, removed := evicted[node.ID]; !removed {
				continue
			}
			if node.ID == stage.legacyPath || strings.HasPrefix(node.ID, stage.legacyPath+"::") {
				replacements[node.ID] = stage.graphPath + strings.TrimPrefix(node.ID, stage.legacyPath)
			}
		}
	}
	if len(replacements) == 0 {
		return carried
	}
	for i, edge := range carried {
		if edge == nil {
			continue
		}
		from, to := replacements[edge.From], replacements[edge.To]
		if from == "" && to == "" {
			continue
		}
		moved := *edge
		if from != "" {
			moved.From = from
		}
		if to != "" {
			moved.To = to
		}
		carried[i] = &moved
	}
	return carried
}
