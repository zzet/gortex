package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type fnValueNameBatchRecordingStore struct {
	graph.Store
	findByNameCalls  int
	findByNamesCalls int
	exactNames       []string
	batchedNames     []string
}

func (s *fnValueNameBatchRecordingStore) FindNodesByName(name string) []*graph.Node {
	s.findByNameCalls++
	s.exactNames = append(s.exactNames, name)
	return s.Store.FindNodesByName(name)
}

func (s *fnValueNameBatchRecordingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.findByNamesCalls++
	s.batchedNames = append(s.batchedNames, names...)
	return s.Store.FindNodesByNames(names)
}

func TestResolveFnValueCallbacksBatchesOnlyTheCertainGlobalNameLookups(t *testing.T) {
	base := graph.New()
	source := &graph.Node{
		ID: "src.go::register", Kind: graph.KindFunction, Name: "register",
		FilePath: "src.go",
	}
	nodes := []*graph.Node{
		source,
		{ID: "handlers.go::handlerA", Kind: graph.KindFunction, Name: "handlerA", FilePath: "handlers.go"},
		{ID: "handlers.go::handlerB", Kind: graph.KindFunction, Name: "handlerB", FilePath: "handlers.go"},
		{ID: "src.go::localHandler", Kind: graph.KindFunction, Name: "localHandler", FilePath: "src.go"},
	}
	candidate := func(name string, line int, meta map[string]any) *graph.Edge {
		if meta == nil {
			meta = map[string]any{}
		}
		meta["via"] = fnValueCandidateVia
		meta[metaFnValueName] = name
		return &graph.Edge{
			From: source.ID, To: graph.FnValuePlaceholderMarker + name,
			Kind: graph.EdgeReferences, FilePath: source.FilePath, Line: line,
			Meta: meta,
		}
	}
	edges := []*graph.Edge{
		candidate("handlerA", 1, map[string]any{"skip_gate": true}),
		candidate("handlerB", 2, map[string]any{"skip_gate": true}),
		candidate("missingHandler", 3, map[string]any{"skip_gate": true}),
		// A repeated miss must reuse the negative memo entry instead of querying
		// the store again.
		candidate("missingHandler", 5, map[string]any{"skip_gate": true}),
		// This candidate may fall back globally, but its same-file definition
		// wins, so it must not inflate the global name batch.
		candidate("localHandler", 4, map[string]any{"fn_value_ungated": true}),
	}
	base.AddBatch(nodes, edges)
	store := &fnValueNameBatchRecordingStore{Store: base}

	if got := ResolveFnValueCallbacks(store); got != 3 {
		t.Fatalf("resolved callbacks = %d, want 3", got)
	}
	// The names the gate is certain to look up globally are read in one
	// batch; the ungated candidate that binds in its own file is never read,
	// and the repeated miss is served by the memo.
	if store.findByNamesCalls != 1 || store.findByNameCalls != 0 {
		t.Fatalf("FindNodesByNames calls = %d, FindNodesByName calls = %d; want 1 and 0", store.findByNamesCalls, store.findByNameCalls)
	}
	wantNames := []string{"handlerA", "handlerB", "missingHandler"}
	if len(store.batchedNames) != len(wantNames) {
		t.Fatalf("batched names = %v, want %v", store.batchedNames, wantNames)
	}
	for i := range wantNames {
		if store.batchedNames[i] != wantNames[i] {
			t.Fatalf("batched names = %v, want %v", store.batchedNames, wantNames)
		}
	}
	for _, target := range []string{
		"handlers.go::handlerA", "handlers.go::handlerB", "src.go::localHandler",
	} {
		if !hasEdgeKind(base, source.ID, target, graph.EdgeReferences) {
			t.Errorf("missing resolved callback edge %q -> %q", source.ID, target)
		}
	}
}
