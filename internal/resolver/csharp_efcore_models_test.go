package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func efModelsTableEdges(g graph.Store) []*graph.Edge {
	var out []*graph.Edge
	for edge := range g.EdgesByKind(graph.EdgeModelsTable) {
		if edge != nil {
			out = append(out, edge)
		}
	}
	return out
}

func efModelEdgesFrom(g graph.Store, entityID string) []*graph.Edge {
	var out []*graph.Edge
	for _, edge := range efModelsTableEdges(g) {
		if edge.From == entityID {
			out = append(out, edge)
		}
	}
	return out
}

func efOnlyModelEdgeFrom(t *testing.T, g graph.Store, entityID string) *graph.Edge {
	t.Helper()
	edges := efModelEdgesFrom(g, entityID)
	require.Len(t, edges, 1)
	return edges[0]
}

func efAddEntity(
	g graph.Store,
	id, repoPrefix, workspaceID, name string,
	attributeTable, attributeSchema string,
) *graph.Node {
	meta := map[string]any{}
	if attributeTable != "" {
		meta["ef_attribute_table"] = attributeTable
		if attributeSchema != "" {
			meta["ef_attribute_schema"] = attributeSchema
		}
	}
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: id, Language: "csharp", RepoPrefix: repoPrefix,
		WorkspaceID: workspaceID, StartLine: 3, Meta: meta,
	}
	g.AddNode(node)
	return node
}

func efAddConfig(
	g graph.Store,
	id, repoPrefix, workspaceID, name, entity, table, schema, relation string,
) *graph.Node {
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: id, Language: "csharp", RepoPrefix: repoPrefix,
		WorkspaceID: workspaceID, StartLine: 8,
		Meta: map[string]any{
			"ef_config_entity": entity, "ef_config_table": table,
			"ef_config_schema": schema, "ef_config_relation": relation,
		},
	}
	g.AddNode(node)
	return node
}

func efAddDbSet(g graph.Store, id, repoPrefix, workspaceID, entity, property string) *graph.Node {
	node := &graph.Node{
		ID: id, Kind: graph.KindField, Name: property,
		FilePath: id, Language: "csharp", RepoPrefix: repoPrefix,
		WorkspaceID: workspaceID, StartLine: 6,
		Meta: map[string]any{"kind": "property", "field_type": "DbSet<" + entity + ">"},
	}
	g.AddNode(node)
	return node
}

func efAddActionFile(g graph.Store, id, repoPrefix, workspaceID string, actions any) *graph.Node {
	node := &graph.Node{
		ID: id, Kind: graph.KindFile, Name: id, FilePath: id,
		Language: "csharp", RepoPrefix: repoPrefix, WorkspaceID: workspaceID,
		Meta: map[string]any{"ef_fluent": actions},
	}
	g.AddNode(node)
	return node
}

func efMappingAction(context, entity, table, schema, relation string, ordinal, line int) map[string]any {
	return map[string]any{
		"context": context, "kind": csharpEFActionMapping,
		"line": line, "ordinal": ordinal,
		"entity": entity, "table": table, "schema": schema, "relation": relation,
	}
}

func efApplyConfigAction(context, config string, ordinal, line int) map[string]any {
	return map[string]any{
		"context": context, "kind": csharpEFActionApplyConfiguration,
		"line": line, "ordinal": ordinal, "config": config,
	}
}

func efApplyAssemblyAction(context string, ordinal, line int) map[string]any {
	return map[string]any{
		"context": context, "kind": csharpEFActionApplyAssembly,
		"line": line, "ordinal": ordinal,
	}
}

func efAddOwnedProjection(g graph.Store, entity *graph.Node, table, schema, relation, binding string) *graph.Edge {
	tableID := csharpEFTableNodeID(entity.RepoPrefix, table, schema)
	csharpEFEnsureTableNode(g, tableID, table, schema, entity.FilePath, entity.RepoPrefix)
	edge := &graph.Edge{
		From: entity.ID, To: tableID, Kind: graph.EdgeModelsTable,
		FilePath: entity.FilePath, Line: entity.StartLine,
		Meta: map[string]any{
			"orm": "efcore", "binding": binding,
			"table_name": table, "schema": schema, "relation": relation,
		},
	}
	g.AddEdge(edge)
	return edge
}

func TestResolveCSharpEFCoreModels_DbSetConventionIsIdempotent(t *testing.T) {
	g := graph.New()
	entity := efAddEntity(g, "Domain/Widget.cs::Widget", "", "", "Widget", "", "")
	efAddDbSet(g, "Data/Ctx.cs::Ctx.StockWidgets", "", "", "Widget", "StockWidgets")
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	edge := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, "db::orm::StockWidgets", edge.To)
	assert.Equal(t, "dbset", edge.Meta["binding"])
	assert.Equal(t, "StockWidgets", edge.Meta["table_name"])
	assert.Equal(t, "convention", edge.Meta["derivation"])
	require.NotNil(t, g.GetNode("db::orm::StockWidgets"))
	assert.Equal(t, 0, ResolveCSharpEFCoreModels(g))
	assert.Len(t, efModelEdgesFrom(g, entity.ID), 1)
}

func TestResolveCSharpEFCoreModels_UnusedConfigurationIsInert(t *testing.T) {
	g := graph.New()
	entity := efAddEntity(g, "Domain/Widget.cs::Widget", "", "", "Widget", "", "")
	efAddConfig(g, "Config/WidgetConfig.cs::WidgetConfig", "", "", "WidgetConfig", "Widget", "fluent_widgets", "", "table")
	efAddDbSet(g, "Data/Ctx.cs::Ctx.Widgets", "", "", "Widget", "Widgets")
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	edge := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, "db::orm::Widgets", edge.To)
	assert.Equal(t, "dbset", edge.Meta["binding"])
}

func TestResolveCSharpEFCoreModels_ActivationAndInlineFollowSourceOrder(t *testing.T) {
	tests := []struct {
		name    string
		actions []map[string]any
		want    string
	}{
		{"configuration then inline", []map[string]any{
			efApplyConfigAction("Ctx", "WidgetConfig", 0, 10),
			efMappingAction("Ctx", "Widget", "inline_widgets", "", "table", 1, 11),
		}, "inline_widgets"},
		{"inline then configuration", []map[string]any{
			efMappingAction("Ctx", "Widget", "inline_widgets", "", "table", 0, 10),
			efApplyConfigAction("Ctx", "WidgetConfig", 1, 11),
		}, "config_widgets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := graph.New()
			entity := efAddEntity(g, "Domain/Widget.cs::Widget", "", "", "Widget", "", "")
			efAddConfig(g, "Config/WidgetConfig.cs::WidgetConfig", "", "", "WidgetConfig", "Widget", "config_widgets", "", "table")
			efAddActionFile(g, "Data/Ctx.cs", "", "", tt.actions)
			require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
			edge := efOnlyModelEdgeFrom(t, g, entity.ID)
			assert.Equal(t, "db::orm::"+tt.want, edge.To)
			assert.Equal(t, "fluent", edge.Meta["binding"])
		})
	}
}

func TestResolveCSharpEFCoreModels_AssemblyAgreementCoalesces(t *testing.T) {
	g := graph.New()
	entity := efAddEntity(g, "Domain/Widget.cs::Widget", "", "", "Widget", "", "")
	efAddConfig(g, "Config/A.cs::AConfig", "", "", "AConfig", "Widget", "widgets", "sales", "table")
	efAddConfig(g, "Config/B.cs::BConfig", "", "", "BConfig", "Widget", "widgets", "sales", "table")
	efAddActionFile(g, "Data/Ctx.cs", "", "", []map[string]any{efApplyAssemblyAction("Ctx", 0, 10)})
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	edge := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, "db::orm::sales.widgets", edge.To)
	assert.Equal(t, "fluent", edge.Meta["binding"])
	assert.Equal(t, 0, ResolveCSharpEFCoreModels(g))
}

func TestResolveCSharpEFCoreModels_AssemblyConflictBlocksFallbackUntilLaterOverride(t *testing.T) {
	g := graph.New()
	entity := efAddEntity(g, "Domain/Widget.cs::Widget", "", "", "Widget", "attribute_widgets", "")
	efAddOwnedProjection(g, entity, "attribute_widgets", "", "table", "attribute")
	efAddDbSet(g, "Data/Ctx.cs::Ctx.Widgets", "", "", "Widget", "Widgets")
	efAddConfig(g, "Config/A.cs::AConfig", "", "", "AConfig", "Widget", "widgets_a", "", "table")
	efAddConfig(g, "Config/B.cs::BConfig", "", "", "BConfig", "Widget", "widgets_b", "", "table")
	file := efAddActionFile(g, "Data/Ctx.cs", "", "", []map[string]any{efApplyAssemblyAction("Ctx", 0, 10)})
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	assert.Empty(t, efModelEdgesFrom(g, entity.ID), "rejection blocks both attribute and DbSet fallback")
	file.Meta["ef_fluent"] = []map[string]any{
		efApplyAssemblyAction("Ctx", 0, 10),
		efMappingAction("Ctx", "Widget", "later_widgets", "", "view", 1, 11),
	}
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	edge := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, "db::orm::later_widgets", edge.To)
	assert.Equal(t, "view", edge.Meta["relation"])
}

func TestResolveCSharpEFCoreModels_ExactRepositoryWorkspaceAndEntityBoundary(t *testing.T) {
	g := graph.New()
	entityA := efAddEntity(g, "repo-a/ws-1/Widget.cs::Widget", "repo-a", "ws-1", "Widget", "", "")
	entityB := efAddEntity(g, "repo-b/ws-1/Widget.cs::Widget", "repo-b", "ws-1", "Widget", "", "")
	entityC := efAddEntity(g, "repo-a/ws-2/Widget.cs::Widget", "repo-a", "ws-2", "Widget", "", "")
	efAddActionFile(g, "repo-a/ws-1/Ctx.cs", "repo-a", "ws-1", []map[string]any{efMappingAction("Ctx", "Widget", "widgets_a", "", "table", 0, 10)})
	efAddActionFile(g, "repo-b/ws-1/Ctx.cs", "repo-b", "ws-1", []map[string]any{efMappingAction("Ctx", "Widget", "widgets_b", "", "table", 0, 10)})
	efAddActionFile(g, "repo-a/ws-2/Ctx.cs", "repo-a", "ws-2", []map[string]any{efMappingAction("Ctx", "Widget", "widgets_c", "", "table", 0, 10)})
	require.Equal(t, 3, ResolveCSharpEFCoreModels(g))
	assert.Equal(t, "repo-a/db::orm::widgets_a", efOnlyModelEdgeFrom(t, g, entityA.ID).To)
	assert.Equal(t, "repo-b/db::orm::widgets_b", efOnlyModelEdgeFrom(t, g, entityB.ID).To)
	assert.Equal(t, "repo-a/db::orm::widgets_c", efOnlyModelEdgeFrom(t, g, entityC.ID).To)
}

func TestResolveCSharpEFCoreModels_AmbiguousEntityWithinBoundaryIsRejected(t *testing.T) {
	g := graph.New()
	efAddEntity(g, "repo-a/Domain/Widget.cs::Widget", "repo-a", "ws", "Widget", "", "")
	efAddEntity(g, "repo-a/Legacy/Widget.cs::Widget", "repo-a", "ws", "Widget", "", "")
	efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []map[string]any{efMappingAction("Ctx", "Widget", "widgets", "", "table", 0, 10)})
	assert.Equal(t, 0, ResolveCSharpEFCoreModels(g))
	assert.Empty(t, efModelsTableEdges(g))
}

func TestResolveCSharpEFCoreModelsScoped_ConfigOnlyChangeReconcilesOneRepository(t *testing.T) {
	g := graph.New()
	entityA := efAddEntity(g, "repo-a/Domain/Widget.cs::Widget", "repo-a", "ws", "Widget", "", "")
	configA := efAddConfig(g, "repo-a/Config/WidgetConfig.cs::WidgetConfig", "repo-a", "ws", "WidgetConfig", "Widget", "widgets_a", "", "table")
	efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []map[string]any{efApplyConfigAction("Ctx", "WidgetConfig", 0, 10)})
	entityB := efAddEntity(g, "repo-b/Domain/Widget.cs::Widget", "repo-b", "ws", "Widget", "", "")
	efAddConfig(g, "repo-b/Config/WidgetConfig.cs::WidgetConfig", "repo-b", "ws", "WidgetConfig", "Widget", "widgets_b", "", "table")
	efAddActionFile(g, "repo-b/Data/Ctx.cs", "repo-b", "ws", []map[string]any{efApplyConfigAction("Ctx", "WidgetConfig", 0, 10)})
	require.Equal(t, 2, ResolveCSharpEFCoreModels(g))
	configA.Meta["ef_config_table"] = "widgets_a_v2"
	require.Equal(t, 1, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
	assert.Equal(t, "repo-a/db::orm::widgets_a_v2", efOnlyModelEdgeFrom(t, g, entityA.ID).To)
	assert.Equal(t, "repo-b/db::orm::widgets_b", efOnlyModelEdgeFrom(t, g, entityB.ID).To)
	assert.Equal(t, 0, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
}

func TestResolveCSharpEFCoreModels_RemovalRestoresAttributeOrDbSetAndDeletesOtherwise(t *testing.T) {
	t.Run("attribute", func(t *testing.T) {
		g := graph.New()
		entity := efAddEntity(g, "repo-a/Domain/Widget.cs::Widget", "repo-a", "ws", "Widget", "attribute_widgets", "catalog")
		efAddConfig(g, "repo-a/Config/WidgetConfig.cs::WidgetConfig", "repo-a", "ws", "WidgetConfig", "Widget", "fluent_widgets", "sales", "table")
		file := efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []map[string]any{efApplyConfigAction("Ctx", "WidgetConfig", 0, 10)})
		require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
		assert.Equal(t, "repo-a/db::orm::sales.fluent_widgets", efOnlyModelEdgeFrom(t, g, entity.ID).To)
		file.Meta["ef_fluent"] = []map[string]any{}
		require.Equal(t, 1, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
		edge := efOnlyModelEdgeFrom(t, g, entity.ID)
		assert.Equal(t, "repo-a/db::orm::catalog.attribute_widgets", edge.To)
		assert.Equal(t, "attribute", edge.Meta["binding"])
	})
	t.Run("dbset", func(t *testing.T) {
		g := graph.New()
		entity := efAddEntity(g, "repo-a/Domain/Widget.cs::Widget", "repo-a", "ws", "Widget", "", "")
		efAddConfig(g, "repo-a/Config/WidgetConfig.cs::WidgetConfig", "repo-a", "ws", "WidgetConfig", "Widget", "fluent_widgets", "", "table")
		efAddDbSet(g, "repo-a/Data/Ctx.cs::Ctx.Widgets", "repo-a", "ws", "Widget", "Widgets")
		file := efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []map[string]any{efApplyConfigAction("Ctx", "WidgetConfig", 0, 10)})
		require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
		assert.Equal(t, "repo-a/db::orm::fluent_widgets", efOnlyModelEdgeFrom(t, g, entity.ID).To)
		file.Meta["ef_fluent"] = []map[string]any{}
		require.Equal(t, 1, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
		edge := efOnlyModelEdgeFrom(t, g, entity.ID)
		assert.Equal(t, "repo-a/db::orm::Widgets", edge.To)
		assert.Equal(t, "dbset", edge.Meta["binding"])
	})
	t.Run("no fallback", func(t *testing.T) {
		g := graph.New()
		entity := efAddEntity(g, "repo-a/Domain/Widget.cs::Widget", "repo-a", "ws", "Widget", "", "")
		efAddConfig(g, "repo-a/Config/WidgetConfig.cs::WidgetConfig", "repo-a", "ws", "WidgetConfig", "Widget", "fluent_widgets", "", "table")
		file := efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []map[string]any{efApplyConfigAction("Ctx", "WidgetConfig", 0, 10)})
		require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
		file.Meta["ef_fluent"] = []map[string]any{}
		require.Equal(t, 1, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
		assert.Empty(t, efModelEdgesFrom(g, entity.ID))
	})
}

func TestResolveCSharpEFCoreModels_DuplicateSiblingProjectionsCoalesce(t *testing.T) {
	g := graph.New()
	entity := efAddEntity(g, "repo-a/Domain/Widget.cs::Widget", "repo-a", "ws", "Widget", "", "")
	efAddConfig(g, "repo-a/Config/WidgetConfig.cs::WidgetConfig", "repo-a", "ws", "WidgetConfig", "Widget", "widgets", "", "table")
	efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []map[string]any{efApplyConfigAction("Ctx", "WidgetConfig", 0, 10)})
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	efAddOwnedProjection(g, entity, "stale_widgets", "", "table", "fluent")
	require.Len(t, efModelEdgesFrom(g, entity.ID), 2)
	require.Equal(t, 1, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
	edge := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, "repo-a/db::orm::widgets", edge.To)
	assert.Equal(t, 0, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
}

func TestResolveCSharpEFCoreModels_ToTableToViewSameTargetIsSemanticReplacement(t *testing.T) {
	g := graph.New()
	entity := efAddEntity(g, "repo-a/Domain/Tally.cs::Tally", "repo-a", "ws", "Tally", "", "")
	file := efAddActionFile(g, "repo-a/Data/Ctx.cs", "repo-a", "ws", []any{
		efMappingAction("Ctx", "Tally", "tallies", "", "table", 0, 10),
	})
	require.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	before := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, "repo-a/db::orm::tallies", before.To)
	assert.Equal(t, "table", before.Meta["relation"])
	file.Meta["ef_fluent"] = []any{
		efMappingAction("Ctx", "Tally", "tallies", "", "view", 0, 10),
	}
	require.Equal(t, 1, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
	after := efOnlyModelEdgeFrom(t, g, entity.ID)
	assert.Equal(t, before.To, after.To)
	assert.Equal(t, "view", after.Meta["relation"])
	assert.Equal(t, 0, ResolveCSharpEFCoreModelsScoped(g, map[string]bool{"repo-a": true}))
}
