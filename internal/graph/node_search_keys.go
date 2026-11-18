package graph

import "context"

// DefaultNodeSearchKeyPageSize is the bounded page used by compact node-key
// scans when callers do not request a positive size.
const DefaultNodeSearchKeyPageSize = 256

// NodeSearchKey is the metadata-free projection needed to rank degraded-mode
// symbol-search candidates. Full nodes are loaded only for the winning IDs.
type NodeSearchKey struct {
	ID          string
	Kind        NodeKind
	Name        string
	FilePath    string
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string
}

func nodeSearchKey(node *Node) NodeSearchKey {
	return NodeSearchKey{ID: node.ID, Kind: node.Kind, Name: node.Name,
		FilePath: node.FilePath, RepoPrefix: node.RepoPrefix, WorkspaceID: node.WorkspaceID, ProjectID: node.ProjectID}
}

// NodeSearchKeyScanner is an optional Reader capability. Implementations must
// visit every node visible through the reader exactly once. Ordering is backend
// specific; callers that need deterministic results must rank or sort keys.
//
// A page is borrowed storage and is valid only until yield returns. Producers
// must release database rows, connections, and graph locks before calling
// yield, so the callback may safely re-enter the same reader. Returning false
// stops the scan without error. Context cancellation is returned as ctx.Err().
type NodeSearchKeyScanner interface {
	ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]NodeSearchKey) bool) error
}

// ScanNodeSearchKeys selects the compact optional capability. Unknown Reader
// implementations retain compatibility through an already-materialized light
// snapshot; production Graph, SQLite, and OverlaidView readers all implement
// NodeSearchKeyScanner directly.
func ScanNodeSearchKeys(ctx context.Context, r Reader, pageSize int, yield func([]NodeSearchKey) bool) error {
	if r == nil || yield == nil {
		return nil
	}
	ctx = nodeSearchContext(ctx)
	pageSize = nodeSearchPageSize(pageSize)
	if scanner, ok := r.(NodeSearchKeyScanner); ok {
		return scanner.ScanNodeSearchKeys(ctx, pageSize, yield)
	}

	// AllNodesLight has returned before yield runs, so even the compatibility
	// path cannot retain an open cursor or reader lock across callback re-entry.
	nodes := AllNodesLight(r)
	page := make([]NodeSearchKey, 0, pageSize)
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node == nil {
			continue
		}
		page = append(page, nodeSearchKey(node))
		if len(page) < pageSize {
			continue
		}
		if !yield(page) {
			return nil
		}
		clear(page)
		page = page[:0]
	}
	if len(page) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		yield(page)
	}
	return nil
}

// ScanNodeSearchKeys is the in-memory implementation. NodesLightSeq copies at
// most one shard's pointers while holding its lock, then releases that lock
// before yielding any node, so callbacks here are safe to re-enter Graph.
func (g *Graph) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]NodeSearchKey) bool) error {
	if g == nil || yield == nil {
		return nil
	}
	ctx = nodeSearchContext(ctx)
	pageSize = nodeSearchPageSize(pageSize)
	page := make([]NodeSearchKey, 0, pageSize)
	for node := range g.NodesLightSeq() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node == nil {
			continue
		}
		page = append(page, nodeSearchKey(node))
		if len(page) < pageSize {
			continue
		}
		if !yield(page) {
			return nil
		}
		clear(page)
		page = page[:0]
	}
	if len(page) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		yield(page)
	}
	return nil
}

func nodeSearchContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func nodeSearchPageSize(pageSize int) int {
	if pageSize <= 0 {
		return DefaultNodeSearchKeyPageSize
	}
	return pageSize
}

var _ NodeSearchKeyScanner = (*Graph)(nil)
