package store_sqlite

import (
	"context"
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// NodeIdentityLocalizationSummariesContext reads a bounded requested-ID set in
// this exact positive generation. It never scans an upper file/repository or
// falls back to generation zero. Only ExcludeTests needs legacy meta decoding;
// the existing decoder retains is_test alone, never retrieval payload.
func (s *Store) NodeIdentityLocalizationSummariesContext(ctx context.Context, ids []string, includeTestFlag bool) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.requireDerivedGeneration(); err != nil {
		return nil, err
	}
	if len(ids) > 256 {
		return nil, &graph.BoundedLocalizationLimitError{Resource: "carried localization identities", Limit: 256}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if s.coreless() || s.db == nil {
		return nil, graph.ErrBoundedLocalizationUnavailable
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("%w: empty localization identity", ErrGenerationMaskIntegrity)
		}
		wanted[id] = struct{}{}
	}
	ordered := make([]string, 0, len(wanted))
	for id := range wanted {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	args := make([]any, 0, len(ordered)+1)
	args = append(args, s.viewGen)
	for _, id := range ordered {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, nodeIdentityLocalizationQuery(includeTestFlag, len(ordered)), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*graph.Node, 0, len(ordered))
	for rows.Next() {
		var node *graph.Node
		var scanErr error
		if includeTestFlag {
			node, scanErr = scanLocalizationNode(rows)
		} else {
			node, scanErr = scanNodeSummary(rows)
		}
		if scanErr != nil {
			return nil, scanErr
		}
		if _, expected := wanted[node.ID]; !expected {
			return nil, fmt.Errorf("%w: unexpected localization identity", ErrGenerationMaskIntegrity)
		}
		delete(wanted, node.ID)
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(wanted) != 0 {
		return nil, fmt.Errorf("%w: missing carried localization rows", ErrGenerationMaskIntegrity)
	}
	return out, nil
}

func nodeIdentityLocalizationQuery(includeTestFlag bool, count int) string {
	columns := lookupNodeSummaryCols
	if includeTestFlag {
		columns = lookupLocalizationNodeCols
	}
	return `SELECT ` + columns + ` FROM nodes WHERE view_gen = ? AND id IN (` + inPlaceholders(count) + `) ORDER BY id`
}
