package indexer

import (
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// Two packages each declare a type Server with a field mu and a method that
// locks it. The receiver-mutation fixpoint must bind each method's `s.mu.Lock()`
// to its OWN package's field: keyed by the bare receiver name, both methods
// shared one owner and the field they bound to was whichever declaration the
// store listed last — a different package's field, and a different one from
// one whole index to the next. The whole-graph pass (in-memory and SQLite
// projection) and the per-save scoped pass must agree on it.
func receiverOwnerTree() map[string]string {
	server := func(pkg string) string {
		return "package " + pkg + `

import "sync"

type Server struct {
	mu    sync.Mutex
	store int
}

func (s *Server) Guard() {
	s.mu.Lock()
	s.mu.Unlock()
}
`
	}
	return map[string]string{
		"go.mod":         "module example.com/owner\n\ngo 1.22\n",
		"alpha/alpha.go": server("alpha"),
		"beta/beta.go":   server("beta"),
		"gamma/gamma.go": server("gamma"),
	}
}

func receiverOwnerWant() map[string]string {
	want := map[string]string{}
	for _, pkg := range []string{"alpha", "beta", "gamma"} {
		want[builderRepoPrefix+"/"+pkg+"/"+pkg+".go::Server.Guard"] = builderRepoPrefix + "/" + pkg + "/" + pkg + ".go::Server.mu"
	}
	return want
}

func receiverOwnerTargets(t *testing.T, specs []indirectMutSpec) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, spec := range specs {
		if !strings.HasSuffix(spec.from, "::Server.Guard") {
			continue
		}
		if prev, ok := got[spec.from]; ok && prev != spec.to {
			t.Fatalf("%s binds two fields: %s and %s", spec.from, prev, spec.to)
		}
		got[spec.from] = spec.to
	}
	return got
}

func TestReceiverMutationBindsTheMethodsOwnPackageField(t *testing.T) {
	repo := builderTempDir(t, "repo")
	builderWriteTree(t, repo, receiverOwnerTree())

	// The whole-graph fixpoint over the SQLite projection and over the
	// generic decode.
	store := builderOpenStore(t, "owner")
	builderIndex(t, store, repo)
	projected := receiverOwnerTargets(t, indirectMutationEdges(store))
	decoded := receiverOwnerTargets(t, indirectMutationEdges(receiverMutationFallbackStore{Store: store}))
	if want := receiverOwnerWant(); !reflect.DeepEqual(projected, want) {
		t.Fatalf("projected whole pass bound %v, want %v", projected, want)
	}
	if !reflect.DeepEqual(decoded, projected) {
		t.Fatalf("decoded whole pass bound %v, projected %v", decoded, projected)
	}

	// The per-save scoped pass, seeded with one method at a time.
	for method, field := range receiverOwnerWant() {
		node := store.GetNode(method)
		if node == nil {
			t.Fatalf("no node %s", method)
		}
		specs, _ := indirectMutationEdgesForMethods(store, []*graph.Node{node})
		if got := receiverOwnerTargets(t, specs)[method]; got != field {
			t.Fatalf("scoped pass bound %s to %q, want %q", method, got, field)
		}
	}

	// The in-memory whole index writes the same accesses_field edges.
	g := graph.New()
	idx := New(g, builderRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix(builderRepoPrefix)
	if _, err := idx.Index(repo); err != nil {
		t.Fatalf("index: %v", err)
	}
	idx.Close()
	for method, field := range receiverOwnerWant() {
		var bound []string
		for _, e := range g.GetOutEdges(method) {
			if e.Kind == graph.EdgeAccessesField && e.Meta["indirect"] == true {
				bound = append(bound, e.To)
			}
		}
		if len(bound) == 0 {
			t.Fatalf("in-memory whole index: %s has no indirect field access", method)
		}
		for _, to := range bound {
			if to != field {
				t.Fatalf("in-memory whole index: %s indirect field accesses %v, want only %s", method, bound, field)
			}
		}
	}
}

// One (method, field, via) triple reached from two call sites is one indirect
// edge, and it carries the earliest site however the calls were listed.
func TestReceiverMutationKeepsTheEarliestCallSite(t *testing.T) {
	const (
		file   = builderRepoPrefix + "/alpha/alpha.go"
		method = file + "::Server.Guard"
		field  = file + "::Server.mu"
	)
	for _, laterFirst := range []bool{true, false} {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: method, Kind: graph.KindMethod, Name: "Guard", FilePath: file, RepoPrefix: builderRepoPrefix,
				Meta: map[string]any{"receiver": "Server"}},
			{ID: field, Kind: graph.KindField, Name: "mu", FilePath: file, RepoPrefix: builderRepoPrefix,
				Meta: map[string]any{"receiver": "Server"}},
		}, nil)
		lines := []int{12, 10}
		if !laterFirst {
			lines = []int{10, 12}
		}
		for _, line := range lines {
			g.AddEdge(&graph.Edge{From: method, To: "unresolved::*.Lock", Kind: graph.EdgeCalls,
				FilePath: file, Line: line, Meta: map[string]any{"recv_field": "mu"}})
		}
		for _, g := range []graph.Store{g, receiverMutationFallbackStore{Store: g}} {
			specs := indirectMutationEdges(g)
			if len(specs) != 1 || specs[0].to != field || specs[0].line != 10 {
				t.Fatalf("laterFirst=%t: specs %+v, want one at line 10", laterFirst, specs)
			}
		}
	}
}
