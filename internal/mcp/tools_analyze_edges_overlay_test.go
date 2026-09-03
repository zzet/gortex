package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The edge-driven analyzers walk the graph through edgesByKinds plus
// per-row GetNode lookups. Both must ride the request reader, so an
// overlay-active call reports what the pushed buffers say instead of
// what is on disk. Each test below pins one handler per analyzer file:
// the base answer first (so a broken fixture is obvious), then the
// overlay answer, which differs in both directions — a row whose
// payload the buffer replaced, and a row the buffer deleted.

// TestAnalyzeConfigReadersReadsThroughOverlay covers the config_readers
// handler: under an overlay the only row is the key the buffer now
// reads, and the key whose readers the buffer dropped is gone.
func TestAnalyzeConfigReadersReadsThroughOverlay(t *testing.T) {
	const (
		readerFile = "cfg/reader.go"
		legacyFile = "cfg/legacy.go"
		loadID     = readerFile + "::Load"
		legacyID   = legacyFile + "::Legacy"
		dbKeyID    = "env::DB_URL"
		apiKeyID   = "env::API_KEY"
	)
	srv := concurrencyServer(t)
	addFn(srv.graph, loadID, "Load", readerFile)
	addFn(srv.graph, legacyID, "Legacy", legacyFile)
	srv.graph.AddNode(&graph.Node{
		ID: dbKeyID, Kind: graph.KindConfigKey, Name: "DB_URL",
		FilePath: "cfg/config.go", Meta: map[string]any{"source": "env"},
	})
	srv.graph.AddNode(&graph.Node{
		ID: apiKeyID, Kind: graph.KindConfigKey, Name: "API_KEY",
		FilePath: "cfg/config.go", Meta: map[string]any{"source": "env"},
	})
	addEdge(srv.graph, loadID, dbKeyID, graph.EdgeReadsConfig, readerFile, 12)
	addEdge(srv.graph, legacyID, dbKeyID, graph.EdgeReadsConfig, legacyFile, 30)

	// The buffer switched reader.go over to API_KEY and deleted legacy.go.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(readerFile, false)
	layer.AddNode(readerFile, &graph.Node{
		ID: loadID, Kind: graph.KindFunction, Name: "Load",
		FilePath: readerFile, Language: "go",
	})
	layer.AddEdge(&graph.Edge{
		From: loadID, To: apiKeyID, Kind: graph.EdgeReadsConfig,
		FilePath: readerFile, Line: 12, Confidence: 1,
	})
	layer.MarkFile(legacyFile, true)
	layer.MarkRemoved("Legacy", legacyID)

	type configRows struct {
		Total      int `json:"total"`
		ConfigKeys []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Reads   int      `json:"reads"`
			Readers []string `json:"readers"`
		} `json:"config_keys"`
	}
	run := func(ctx context.Context) configRows {
		t.Helper()
		res, err := srv.handleAnalyzeConfigReaders(ctx, mcplib.CallToolRequest{})
		require.NoError(t, err)
		require.False(t, res.IsError)
		var payload configRows
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
		return payload
	}

	base := run(context.Background())
	require.Equal(t, 1, base.Total)
	require.Equal(t, dbKeyID, base.ConfigKeys[0].ID)
	assert.Equal(t, 2, base.ConfigKeys[0].Reads)

	over := run(overlayCtx(t, srv, layer))
	require.Equal(t, 1, over.Total, "the overlay leaves exactly one config key with readers")
	assert.Equal(t, apiKeyID, over.ConfigKeys[0].ID, "the buffer's replacement key must be the row")
	assert.Equal(t, "API_KEY", over.ConfigKeys[0].Name)
	assert.Equal(t, []string{loadID}, over.ConfigKeys[0].Readers)
	for _, row := range over.ConfigKeys {
		assert.NotEqual(t, dbKeyID, row.ID, "no reader is left for the key the buffer stopped reading")
	}

	// The base store must be untouched by the overlay request.
	again := run(context.Background())
	require.Equal(t, 1, again.Total)
	assert.Equal(t, dbKeyID, again.ConfigKeys[0].ID)
}

// TestAnalyzeRaceWritesReadsThroughOverlay covers the race_writes
// handler and its two helpers (the goroutine-reachable closure and the
// lock-guard probe): under an overlay the surviving writer is reported
// at the buffer's line, and the writer the buffer deleted is absent.
func TestAnalyzeRaceWritesReadsThroughOverlay(t *testing.T) {
	const (
		workerFile = "worker.go"
		workerID   = workerFile + "::Worker"
		helperID   = workerFile + "::Helper"
		fieldID    = "state.go::State.counter"
	)
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addFn(srv.graph, workerID, "Worker", workerFile)
	addFn(srv.graph, helperID, "Helper", workerFile)
	addField(srv.graph, fieldID, "counter", "state.go")
	addEdge(srv.graph, "main.go::Main", workerID, graph.EdgeSpawns, "main.go", 10)
	addEdge(srv.graph, "main.go::Main", helperID, graph.EdgeSpawns, "main.go", 11)
	addEdge(srv.graph, workerID, fieldID, graph.EdgeWrites, workerFile, 20)
	addEdge(srv.graph, helperID, fieldID, graph.EdgeWrites, workerFile, 30)

	// The buffer moved Worker's write and deleted Helper.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(workerFile, false)
	layer.AddNode(workerFile, &graph.Node{
		ID: workerID, Kind: graph.KindFunction, Name: "Worker",
		FilePath: workerFile, Language: "go",
	})
	layer.AddEdge(&graph.Edge{
		From: workerID, To: fieldID, Kind: graph.EdgeWrites,
		FilePath: workerFile, Line: 55, Confidence: 1,
	})
	layer.MarkRemoved("Helper", helperID)

	type raceRows struct {
		Total      int `json:"total"`
		RaceWrites []struct {
			Field  string `json:"field"`
			Writer string `json:"writer"`
			Line   int    `json:"line"`
		} `json:"race_writes"`
	}
	run := func(ctx context.Context) raceRows {
		t.Helper()
		res, err := srv.handleAnalyzeRaceWrites(ctx, mcplib.CallToolRequest{})
		require.NoError(t, err)
		require.False(t, res.IsError)
		var payload raceRows
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
		return payload
	}

	base := run(context.Background())
	require.Equal(t, 2, base.Total)

	over := run(overlayCtx(t, srv, layer))
	require.Equal(t, 1, over.Total, "only the writer the buffer kept is racy")
	assert.Equal(t, workerID, over.RaceWrites[0].Writer)
	assert.Equal(t, fieldID, over.RaceWrites[0].Field)
	assert.Equal(t, 55, over.RaceWrites[0].Line, "the row must carry the buffer's line, not the on-disk one")
	for _, row := range over.RaceWrites {
		assert.NotEqual(t, helperID, row.Writer, "the deleted writer must not be reported")
	}
}

// TestAnalyzeK8sResourcesReadsThroughOverlay covers the k8s_resources
// handler: the per-resource edge tally is built from the buffer's
// edges, and a resource whose manifest the buffer deleted is absent.
func TestAnalyzeK8sResourcesReadsThroughOverlay(t *testing.T) {
	const (
		apiFile  = "k8s/api.yaml"
		oldFile  = "k8s/old.yaml"
		apiID    = apiFile + "::Deployment.api"
		oldID    = oldFile + "::Deployment.old"
		configID = "k8s/cm.yaml::ConfigMap.settings"
		secretID = "k8s/secret.yaml::Secret.creds"
	)
	srv := concurrencyServer(t)
	srv.graph.AddNode(&graph.Node{
		ID: apiID, Kind: graph.KindResource, Name: "api", FilePath: apiFile, StartLine: 1,
		Meta: map[string]any{"k8s_kind": "Deployment", "namespace": "prod"},
	})
	srv.graph.AddNode(&graph.Node{
		ID: oldID, Kind: graph.KindResource, Name: "old", FilePath: oldFile, StartLine: 1,
		Meta: map[string]any{"k8s_kind": "Deployment", "namespace": "prod"},
	})
	srv.graph.AddNode(&graph.Node{
		ID: configID, Kind: graph.KindResource, Name: "settings", FilePath: "k8s/cm.yaml",
		Meta: map[string]any{"k8s_kind": "ConfigMap"},
	})
	srv.graph.AddNode(&graph.Node{
		ID: secretID, Kind: graph.KindResource, Name: "creds", FilePath: "k8s/secret.yaml",
		Meta: map[string]any{"k8s_kind": "Secret"},
	})
	addEdge(srv.graph, apiID, configID, graph.EdgeDependsOn, apiFile, 5)
	addEdge(srv.graph, oldID, configID, graph.EdgeDependsOn, oldFile, 5)

	// The buffer moved api to staging, gave it a second dependency, and
	// deleted old.yaml.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(apiFile, false)
	layer.AddNode(apiFile, &graph.Node{
		ID: apiID, Kind: graph.KindResource, Name: "api", FilePath: apiFile, StartLine: 1,
		Meta: map[string]any{"k8s_kind": "Deployment", "namespace": "staging"},
	})
	layer.AddEdge(&graph.Edge{From: apiID, To: configID, Kind: graph.EdgeDependsOn, FilePath: apiFile, Line: 5, Confidence: 1})
	layer.AddEdge(&graph.Edge{From: apiID, To: secretID, Kind: graph.EdgeDependsOn, FilePath: apiFile, Line: 9, Confidence: 1})
	layer.MarkFile(oldFile, true)
	layer.MarkRemoved("old", oldID)

	type resourceRow struct {
		ID        string `json:"id"`
		Namespace string `json:"namespace"`
		DependsOn int    `json:"depends_on"`
	}
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"k8s_kind": "Deployment"}
	run := func(ctx context.Context) map[string]resourceRow {
		t.Helper()
		res, err := srv.handleAnalyzeK8sResources(ctx, req)
		require.NoError(t, err)
		require.False(t, res.IsError)
		var payload struct {
			Resources []resourceRow `json:"resources"`
		}
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
		out := make(map[string]resourceRow, len(payload.Resources))
		for _, r := range payload.Resources {
			out[r.ID] = r
		}
		return out
	}

	base := run(context.Background())
	require.Len(t, base, 2)
	assert.Equal(t, 1, base[apiID].DependsOn)
	assert.Equal(t, "prod", base[apiID].Namespace)

	over := run(overlayCtx(t, srv, layer))
	require.Len(t, over, 1, "the deleted manifest's resource must not surface")
	assert.Equal(t, 2, over[apiID].DependsOn, "the tally must come from the buffer's edges")
	assert.Equal(t, "staging", over[apiID].Namespace)
	_, deleted := over[oldID]
	assert.False(t, deleted, "the resource the buffer deleted must be absent")
}
