package indexer

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

// Capture complete buckets rather than Registry.All alone: All deliberately
// coalesces same-ID/file/symbol/role records even when their metadata differs.
// Secondary views retain their actual order; AllIDs/AllWorkspaces and All have
// unspecified map traversal order and are canonicalized only for comparison.
func contractFollowupRegistryViews(registry *contracts.Registry, rows []contracts.Contract) map[string][]contracts.Contract {
	views := make(map[string][]contracts.Contract)
	for _, row := range rows {
		views["id:"+row.ID] = cloneContractRecords(registry.ByID(row.ID))
		views["repo:"+row.RepoPrefix] = cloneContractRecords(registry.ByRepo(row.RepoPrefix))
		views["symbol:"+row.SymbolID] = cloneContractRecords(registry.BySymbol(row.SymbolID))
		views["file:"+row.FilePath] = cloneContractRecords(registry.ByFile(row.FilePath))
		views["workspace:"+row.EffectiveWorkspace()] = cloneContractRecords(registry.ByWorkspace(row.EffectiveWorkspace()))
	}
	all := cloneContractRecords(registry.All())
	slices.SortFunc(all, func(a, b contracts.Contract) int {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return slices.Compare(x, y)
	})
	views["all"] = all
	return views
}

func TestContractFollowupForeignOnlyRegistryViewsRemainUnchanged(t *testing.T) {
	registry := contracts.NewRegistry()
	var rows []contracts.Contract
	for i := 0; i < 24; i++ {
		rows = append(rows, contracts.Contract{
			ID: fmt.Sprintf("http::GET::/foreign-%02d", i), Type: contracts.ContractHTTP,
			Role: contracts.RoleProvider, RepoPrefix: "foreign", WorkspaceID: "shared",
			FilePath: "foreign/routes.go", SymbolID: "foreign/routes.go::Handler",
			Meta: map[string]any{"position": int64(i), "schema": []string{"unchanged"}},
		})
	}
	registry.AddAll(rows, "foreign")
	before := contractFollowupRegistryViews(registry, rows)
	ids, workspaces := registry.AllIDs(), registry.AllWorkspaces()
	contractFollowupRemoveRepo(registry, "local")
	require.Equal(t, before, contractFollowupRegistryViews(registry, rows))
	require.ElementsMatch(t, ids, registry.AllIDs())
	require.ElementsMatch(t, workspaces, registry.AllWorkspaces())
}

func TestContractFollowupSharedIDRetainsFullForeignBucketAndIndexes(t *testing.T) {
	foreign := contracts.Contract{ID: "http::GET::/shared", Type: contracts.ContractHTTP,
		Role: contracts.RoleProvider, RepoPrefix: "foreign", WorkspaceID: "shared",
		FilePath: "foreign/routes.go", SymbolID: "foreign/routes.go::Handler",
		Meta: map[string]any{"variant": "first", "typed": int64(7)}, Confidence: 0.75}
	second := foreign
	second.Meta = map[string]any{"variant": "second", "typed": float64(7)}
	local := foreign
	local.RepoPrefix, local.FilePath, local.SymbolID = "local", "local/routes.go", "local/routes.go::Handler"
	local.Meta = map[string]any{"variant": "old-local"}
	other := foreign
	other.ID, other.Meta = "http::GET::/other", map[string]any{"variant": "foreign-only"}
	removed := local
	removed.ID = "http::GET::/removed"
	rows := []contracts.Contract{foreign, local, second, other, removed}
	registry, expected := contracts.NewRegistry(), contracts.NewRegistry()
	for _, row := range rows {
		registry.Add(row)
		expected.Add(row)
	}
	// Only the genuinely changed buckets use the established ReplaceByID
	// semantics. In particular, preserve both same-identity foreign records.
	expected.ReplaceByID(foreign.ID, []contracts.Contract{foreign, second})
	expected.ReplaceByID(removed.ID, nil)
	contractFollowupRemoveRepo(registry, "local")
	require.Equal(t, []contracts.Contract{foreign, second}, registry.ByID(foreign.ID))
	require.Empty(t, registry.ByRepo("local"))
	require.Equal(t, contractFollowupRegistryViews(expected, rows), contractFollowupRegistryViews(registry, rows))

	local.Meta = map[string]any{"variant": "enriched-local"}
	registry.AddAll([]contracts.Contract{local}, "local")
	expected.AddAll([]contracts.Contract{local}, "local")
	require.Equal(t, []contracts.Contract{foreign, second, local}, registry.ByID(foreign.ID))
	require.Equal(t, contractFollowupRegistryViews(expected, rows), contractFollowupRegistryViews(registry, rows))
	require.ElementsMatch(t, expected.AllIDs(), registry.AllIDs())
	require.ElementsMatch(t, expected.AllWorkspaces(), registry.AllWorkspaces())
}
