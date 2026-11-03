package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
)

// Observe actual tools/call frames, not just a declared offered plan.
func TestStartupProbeSendsRequiredIntrospectionOnBothSurfaces(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[int]map[string]bool)
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		name, _ := frame.Params["name"].(string)
		arguments, _ := frame.Params["arguments"].(map[string]any)
		operation, _ := arguments["operation"].(string)
		if operation != "" {
			name += ":" + operation
		}
		mu.Lock()
		if calls[frame.Connection] == nil {
			calls[frame.Connection] = make(map[string]bool)
		}
		calls[frame.Connection][name] = true
		mu.Unlock()
		return stubContent(map[string]any{"status": "loading"}), nil
	})
	probe := &startupProbe{socket: socket, plan: startupOperations(Corpus{Query: "Marker"}), encoder: json.NewEncoder(io.Discard)}
	probe.mcpTick(context.Background(), probeCWD{Role: "primary", Dir: t.TempDir()})
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"index_health", "capabilities", "tools_search", "graph_stats"} {
		if !calls[1][name] {
			t.Errorf("direct session never called %s: %v", name, calls[1])
		}
	}
	for _, name := range []string{"workspace:index", "workspace:graph", "capabilities", "tools_search"} {
		if !calls[2][name] {
			t.Errorf("facade session never called supported door %s: %v", name, calls[2])
		}
	}
}

func TestStartupCoordinatorMetricRejectsAggregateWithoutMembersOrAnchor(t *testing.T) {
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		return map[string]any{"views": map[string]any{"coordinators": 24}}, nil
	})
	timing := bootTiming{Boot: 1}
	readCoordinators(t.Context(), &privateRun{Socket: socket}, &timing)
	metric := summarizeStartup(nil, []bootTiming{timing}, "app", 220).metrics()["startup_coordinators_within_2s_min"]
	if metric.Value != nil {
		t.Fatalf("aggregate-only status earned identified transition credit without member IDs/incarnations/registration anchor: %+v", metric)
	}
}
