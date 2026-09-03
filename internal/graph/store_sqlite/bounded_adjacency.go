package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.BoundedOutgoingEdgeIdentityReader     = (*Store)(nil)
	_ graph.BoundedIncomingEdgeIdentityReader     = (*Store)(nil)
	_ graph.BoundedOutgoingSiteEdgeIdentityReader = (*Store)(nil)
)

func canonicalSQLiteBoundedAdjacencyKinds(ctx context.Context, kinds []graph.EdgeKind) ([]graph.EdgeKind, error) {
	if len(kinds) > graph.MaxBoundedAdjacencyKinds {
		return nil, &graph.BoundedLocalizationLimitError{
			Resource: "SQLite adjacency kinds",
			Limit:    graph.MaxBoundedAdjacencyKinds,
		}
	}
	seen := make(map[graph.EdgeKind]struct{}, len(kinds))
	out := make([]graph.EdgeKind, 0, len(kinds))
	for index, kind := range kinds {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		out = append(out, kind)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func canonicalSQLiteBoundedAdjacencyEndpoints(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) > graph.MaxBoundedAdjacencyKeys {
		return nil, &graph.BoundedLocalizationLimitError{
			Resource: "SQLite adjacency endpoint keys",
			Limit:    graph.MaxBoundedAdjacencyKeys,
		}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for index, id := range ids {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func canonicalSQLiteBoundedAdjacencySites(ctx context.Context, sites []graph.EdgeSourceSite) ([]graph.EdgeSourceSite, error) {
	if len(sites) > graph.MaxBoundedAdjacencyKeys {
		return nil, &graph.BoundedLocalizationLimitError{
			Resource: "SQLite adjacency source-site keys",
			Limit:    graph.MaxBoundedAdjacencyKeys,
		}
	}
	seen := make(map[graph.EdgeSourceSite]struct{}, len(sites))
	out := make([]graph.EdgeSourceSite, 0, len(sites))
	for index, site := range sites {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if site.From == "" {
			continue
		}
		if _, duplicate := seen[site]; duplicate {
			continue
		}
		seen[site] = struct{}{}
		out = append(out, site)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

func validateSQLiteBoundedAdjacencyLimit(limit int) error {
	if limit < 1 || limit > graph.MaxBoundedAdjacencyRowsPerKey {
		return &graph.BoundedLocalizationLimitError{
			Resource: "SQLite adjacency rows per key",
			Limit:    graph.MaxBoundedAdjacencyRowsPerKey,
		}
	}
	return nil
}

func boundedAdjacencyKindList(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

// The generation predicate is appended after the indexed conjuncts, so each
// shape keeps the index named in its INDEXED BY clause and only filters the
// rows that index already produced.

func boundedOutgoingAdjacencySQL(kindCount int) string {
	return `SELECT from_id, to_id, kind, file_path, line
FROM edges INDEXED BY edges_by_from
WHERE from_id = ? AND kind IN (` + boundedAdjacencyKindList(kindCount) + `) AND view_gen = ?
LIMIT ?`
}

func boundedIncomingAdjacencySQL(kindCount int) string {
	return `SELECT from_id, to_id, kind, file_path, line
FROM edges INDEXED BY edges_by_to
WHERE to_id = ? AND kind IN (` + boundedAdjacencyKindList(kindCount) + `) AND view_gen = ?
LIMIT ?`
}

func boundedOutgoingSiteAdjacencySQL(kindCount int) string {
	return `SELECT from_id, to_id, kind, file_path, line
FROM edges INDEXED BY edges_by_from_line_kind
WHERE from_id = ? AND line = ? AND kind IN (` + boundedAdjacencyKindList(kindCount) + `) AND view_gen = ?
LIMIT ?`
}

func boundedAdjacencyArgs(prefix []any, kinds []graph.EdgeKind, viewGen int64, limit int) []any {
	args := make([]any, 0, len(prefix)+len(kinds)+2)
	args = append(args, prefix...)
	for _, kind := range kinds {
		args = append(args, string(kind))
	}
	args = append(args, viewGen, limit+1)
	return args
}

func scanSQLiteBoundedAdjacencyRows(
	ctx context.Context,
	rows *sql.Rows,
	limit int,
	total *int,
) ([]graph.EdgeIdentity, bool, error) {
	var identities []graph.EdgeIdentity
	var seen map[graph.EdgeIdentity]struct{}
	rowIndex := 0
	for rows.Next() {
		if rowIndex&127 == 0 {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
		}
		rowIndex++
		var identity graph.EdgeIdentity
		var kind string
		if err := rows.Scan(&identity.From, &identity.To, &kind, &identity.FilePath, &identity.Line); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		identity.Kind = graph.EdgeKind(kind)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		if seen == nil {
			identities = make([]graph.EdgeIdentity, 0, 1)
			seen = make(map[graph.EdgeIdentity]struct{}, 1)
		}
		seen[identity] = struct{}{}
		if *total == graph.MaxBoundedAdjacencyTotalIdentities {
			_ = rows.Close()
			return nil, false, &graph.BoundedLocalizationLimitError{
				Resource: "SQLite adjacency matching identities",
				Limit:    graph.MaxBoundedAdjacencyTotalIdentities,
			}
		}
		*total++
		if len(identities) == limit {
			if err := rows.Close(); err != nil {
				return nil, false, err
			}
			return nil, true, nil
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, fmt.Errorf("bounded adjacency query: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].From != identities[j].From {
			return identities[i].From < identities[j].From
		}
		if identities[i].To != identities[j].To {
			return identities[i].To < identities[j].To
		}
		if identities[i].Kind != identities[j].Kind {
			return identities[i].Kind < identities[j].Kind
		}
		if identities[i].FilePath != identities[j].FilePath {
			return identities[i].FilePath < identities[j].FilePath
		}
		return identities[i].Line < identities[j].Line
	})
	return identities, false, nil
}

func (s *Store) FindOutgoingEdgeIdentitiesBounded(
	ctx context.Context,
	sourceIDs []string,
	kinds []graph.EdgeKind,
	limit int,
) (graph.BoundedEdgeIdentityProjection, error) {
	return s.findEndpointEdgeIdentitiesBounded(ctx, sourceIDs, kinds, limit, true)
}

func (s *Store) FindIncomingEdgeIdentitiesBounded(
	ctx context.Context,
	targetIDs []string,
	kinds []graph.EdgeKind,
	limit int,
) (graph.BoundedEdgeIdentityProjection, error) {
	return s.findEndpointEdgeIdentitiesBounded(ctx, targetIDs, kinds, limit, false)
}

func (s *Store) findEndpointEdgeIdentitiesBounded(
	ctx context.Context,
	endpointIDs []string,
	kinds []graph.EdgeKind,
	limit int,
	outgoing bool,
) (graph.BoundedEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	if err := validateSQLiteBoundedAdjacencyLimit(limit); err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalSQLiteBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	ids, err := canonicalSQLiteBoundedAdjacencyEndpoints(ctx, endpointIDs)
	if err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	projection := graph.BoundedEdgeIdentityProjection{
		ByEndpoint: make(map[string][]graph.EdgeIdentity),
		Truncated:  make(map[string]bool),
	}
	if len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query := boundedIncomingAdjacencySQL(len(kindSet))
	if outgoing {
		query = boundedOutgoingAdjacencySQL(len(kindSet))
	}
	total := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return graph.BoundedEdgeIdentityProjection{}, err
		}
		rows, queryErr := tx.QueryContext(ctx, query, boundedAdjacencyArgs([]any{id}, kindSet, s.viewGen, limit)...)
		if queryErr != nil {
			return graph.BoundedEdgeIdentityProjection{}, queryErr
		}
		identities, truncated, scanErr := scanSQLiteBoundedAdjacencyRows(ctx, rows, limit, &total)
		if scanErr != nil {
			return graph.BoundedEdgeIdentityProjection{}, scanErr
		}
		if truncated {
			projection.Truncated[id] = true
			continue
		}
		if len(identities) > 0 {
			projection.ByEndpoint[id] = identities
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	return projection, nil
}

func (s *Store) FindOutgoingSiteEdgeIdentitiesBounded(
	ctx context.Context,
	sites []graph.EdgeSourceSite,
	kinds []graph.EdgeKind,
	limit int,
) (graph.BoundedSiteEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	if err := validateSQLiteBoundedAdjacencyLimit(limit); err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalSQLiteBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	canonical, err := canonicalSQLiteBoundedAdjacencySites(ctx, sites)
	if err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	projection := graph.BoundedSiteEdgeIdentityProjection{
		BySite:    make(map[graph.EdgeSourceSite][]graph.EdgeIdentity),
		Truncated: make(map[graph.EdgeSourceSite]bool),
	}
	if len(canonical) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query := boundedOutgoingSiteAdjacencySQL(len(kindSet))
	total := 0
	for _, site := range canonical {
		if err := ctx.Err(); err != nil {
			return graph.BoundedSiteEdgeIdentityProjection{}, err
		}
		rows, queryErr := tx.QueryContext(
			ctx, query, boundedAdjacencyArgs([]any{site.From, site.Line}, kindSet, s.viewGen, limit)...,
		)
		if queryErr != nil {
			return graph.BoundedSiteEdgeIdentityProjection{}, queryErr
		}
		identities, truncated, scanErr := scanSQLiteBoundedAdjacencyRows(ctx, rows, limit, &total)
		if scanErr != nil {
			return graph.BoundedSiteEdgeIdentityProjection{}, scanErr
		}
		if truncated {
			projection.Truncated[site] = true
			continue
		}
		if len(identities) > 0 {
			projection.BySite[site] = identities
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	return projection, nil
}
