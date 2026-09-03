package store_sqlite

import (
	"fmt"
	"strconv"

	"github.com/zzet/gortex/internal/graph"
)

// unresolvedFrontierSQL intentionally uses only flat edge columns. The target
// class is a syntactic diagnostic, not resolver policy: language, repository,
// reachability, terminal stamps, and candidate quality remain untouched.
//
// INDEXED BY keeps the frontier walk on the maintained is_unresolved index.
// The aggregation is one SQLite query regardless of frontier size. A windowed
// total preserves the exact pending/group counts while LIMIT defensively caps
// the number of bucket rows crossing from SQLite into Go.
const unresolvedFrontierBucketLimit = 128

func unresolvedFrontierSQL(source, generation string) string {
	return `
WITH pending AS (
    SELECT
        kind,
        substr(to_id, instr(to_id, 'unresolved::') + length('unresolved::')) AS target_tail
    FROM ` + source + `
    WHERE ` + generation + `
      AND ` + unresolvedEdgePredicate + `
)
SELECT
    kind,
    CASE
        WHEN target_tail = '' THEN 'empty'
        WHEN substr(target_tail, 1, length('import::')) = 'import::' THEN 'import'
        WHEN substr(target_tail, 1, length('pyrel::')) = 'pyrel::' THEN 'relative_import'
        WHEN substr(target_tail, 1, length('grpc::')) = 'grpc::' THEN 'grpc'
        WHEN substr(target_tail, 1, length('razor_using::')) = 'razor_using::' THEN 'razor_using'
        WHEN substr(target_tail, 1, 2) = '*.' THEN 'wildcard_member'
        WHEN instr(target_tail, '::') > 0 THEN 'qualified_symbol'
        ELSE 'bare_symbol'
    END AS target_class,
    COUNT(*) AS bucket_count,
    SUM(COUNT(*)) OVER () AS pending_total,
    COUNT(*) OVER () AS group_count
FROM pending
GROUP BY kind, target_class
ORDER BY bucket_count DESC, kind, target_class
LIMIT ` + strconv.Itoa(unresolvedFrontierBucketLimit)
}

var _ graph.UnresolvedFrontierCounter = (*Store)(nil)

// CountUnresolvedFrontier summarizes the current generic resolver frontier in
// SQLite. It does not decode edge metadata, load nodes, or issue per-bucket
// follow-up queries.
func (s *Store) CountUnresolvedFrontier() (graph.UnresolvedFrontierStats, error) {
	stats := graph.UnresolvedFrontierStats{QueryCount: 1}
	source, generation := s.unresolvedScanSource(unresolvedFrontierBaseSource)
	rows, err := s.db.Query(unresolvedFrontierSQL(source, generation), s.viewGen)
	if err != nil {
		return stats, fmt.Errorf("count unresolved frontier: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			kind        graph.EdgeKind
			targetClass graph.UnresolvedTargetClass
			count       int64
			pending     int64
			groupCount  int
		)
		if err := rows.Scan(&kind, &targetClass, &count, &pending, &groupCount); err != nil {
			return stats, fmt.Errorf("scan unresolved frontier bucket: %w", err)
		}
		stats.Pending = pending
		stats.GroupCount = groupCount
		stats.Buckets = append(stats.Buckets, graph.UnresolvedFrontierBucket{
			TargetClass: targetClass,
			Kind:        kind,
			Count:       count,
		})
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("iterate unresolved frontier buckets: %w", err)
	}
	return stats, nil
}
