package store_sqlite

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolverScopedProjectionProductionQueriesUseRepoKindIndex(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name: "high water", query: resolverScopedProjectionHighWaterQuery,
			args: []any{"repo", graph.KindFile, baseViewGeneration},
		},
		{
			name: "first page", query: resolverScopedProjectionPageQuery("id, file_path, repo_prefix, workspace_id", false),
			args: []any{"repo", graph.KindFile, "repo::z", baseViewGeneration, resolverProjectionPageSize},
		},
		{
			name: "next page", query: resolverScopedProjectionPageQuery("id, file_path, repo_prefix, workspace_id", true),
			args: []any{"repo", graph.KindFile, "repo::a", "repo::z", baseViewGeneration, resolverProjectionPageSize},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+tt.query, tt.args...)
			if err != nil {
				t.Fatalf("explain query: %v", err)
			}
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					_ = rows.Close()
					t.Fatalf("scan query plan: %v", err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				t.Fatalf("query plan rows: %v", err)
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("close query plan: %v", err)
			}
			plan := strings.Join(details, "\n")
			if !strings.Contains(plan, "nodes_by_repo_kind") {
				t.Fatalf("query plan does not use nodes_by_repo_kind:\n%s", plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE") || strings.Contains(plan, "SCAN nodes") {
				t.Fatalf("query plan is not an indexed keyset seek:\n%s", plan)
			}
		})
	}
}
