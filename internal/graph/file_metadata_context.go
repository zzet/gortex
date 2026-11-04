package graph

import (
	"context"
	"errors"
)

var errFileMetadataReadBusy = errors.New("in-memory file metadata is busy")

// FileMetasByPathsContext is the bounded canonical-content lookup. It reads
// this graph's own metadata, never a delta's base or sidecar ledger. A busy
// mutex leaves matching unknown rather than waiting past a caller deadline;
// the legacy blocking reader retains its existing behavior.
func (g *Graph) FileMetasByPathsContext(ctx context.Context, repoPrefix string, filePaths []string) (map[string]FileMetaRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]FileMetaRow, len(filePaths))
	if len(filePaths) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return out, nil
	}
	if !g.fileMetasMu.TryLock() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errFileMetadataReadBusy
	}
	defer g.fileMetasMu.Unlock()
	byFile := g.fileMetas[repoPrefix]
	for _, filePath := range filePaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if row, ok := byFile[filePath]; ok {
			out[filePath] = row
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
