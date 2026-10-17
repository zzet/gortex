package store_sqlite

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

var repoExactRawNameSeekSQL = `SELECT ` + lookupNodeCols + ` FROM nodes
 WHERE name = ? AND view_gen = ? AND +repo_prefix = ?`

// VisitNodesByNamesInRepoContext preserves checked exact-name iteration while
// keeping repository ownership in SQLite. The existing name/view seek reads
// candidate rows; the residual repo predicate avoids decoding unrelated rows.
func (s *Store) VisitNodesByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil || len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	rawNames := false
	for _, name := range names {
		if _, duplicate := seen[name]; !duplicate {
			seen[name] = struct{}{}
			uniq = append(uniq, name)
			rawNames = rawNames || !utf8.ValidString(name)
		}
	}
	if rawNames {
		// JSON would replace malformed UTF-8; preserve the exact bound bytes.
		for _, name := range uniq {
			stopped, err := s.visitExactNamesQuery(ctx, repoExactRawNameSeekSQL, []any{name, s.viewGen, repo}, yield)
			if err != nil {
				return err
			}
			if stopped {
				break
			}
		}
		return ctx.Err()
	}
	for start := 0; start < len(uniq); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := json.Marshal(uniq[start:min(start+lookupChunkSize, len(uniq))])
		if err != nil {
			return err
		}
		stopped, err := s.visitExactNamesQuery(ctx, repoNamesSeekSQL, []any{string(payload), s.viewGen, repo}, yield)
		if err != nil {
			return err
		}
		if stopped {
			break
		}
	}
	return ctx.Err()
}
