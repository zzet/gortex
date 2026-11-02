package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
)

// Absence in a projection is not by itself evidence of removal. The same
// selected checked base must positively contain the requested, visible ID.
func (s *Server) removedByPendingProjection(ctx context.Context, req mcplib.CallToolRequest, id string) (bool, error) {
	file, _, ok := strings.Cut(id, "::")
	if !ok {
		return false, nil
	}
	abs, err := s.resolveGraphPath(ctx, file)
	if err != nil {
		return false, nil
	}
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return false, nil
	}
	facts.mu.RLock()
	entry := facts.entries[pathkey.MutationKey(abs)]
	facts.mu.RUnlock()
	if entry == nil || entry.graphPath != file || !liveProjectionForAcknowledgement(entry) || entry.layer.NodeByID(id) != nil {
		return false, nil
	}
	if !entry.tombstone {
		owner, _ := s.indexerForRel(file)
		if owner == nil {
			return false, nil
		}
		root, rel, key, err := selectedProjectionReadPath(ctx, owner.RootPath(), owner.RepoPrefix(), abs)
		if err != nil || key != entry.key {
			return false, nil
		}
		files, err := source.NewFilesystemSource(root)
		if err != nil {
			return false, err
		}
		raw, _, err := files.ReadRegularFile(ctx, filepath.ToSlash(rel), 1<<20)
		files.Close()
		if err != nil {
			return false, err
		}
		if blake3.Sum256(raw) != entry.rawHash {
			return false, invalidateProjectedSource(entry)
		}
	}
	budget, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	rows, err := graph.ContractFileNodesContext(budget, s.requestSelectedBaseReader(ctx), entry.repoPrefix, []string{file})
	if err != nil {
		return false, nil
	}
	scope, refused := s.resolveScope(ctx, req, toolIntentForName(req.Params.Name))
	if refused != nil {
		return false, nil
	}
	for _, node := range rows[file] {
		if node != nil && node.ID == id && node.FilePath == file && s.nodeInSessionScope(ctx, node) && s.contractScopeAllowsNode(ctx, scope, node) && liveProjectionForAcknowledgement(entry) {
			return true, nil
		}
	}
	return false, nil
}

func removedProjectedSymbolGuidance(id string, isError bool) *mcplib.CallToolResult {
	result := newRecoverableResult(RecoverableGuidance{
		Condition:      ErrCodeSymbolNotFound,
		Message:        "symbol not found: removed by pending edit",
		SuggestedTools: []string{"get_file_summary", "search_symbols"},
		Data:           map[string]any{"id": id, "reason": "removed by pending edit"},
	})
	result.IsError = isError
	return result
}
