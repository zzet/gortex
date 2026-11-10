package languages

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestJavaScriptExtractor_ClassEdges(t *testing.T) {
	const src = `const Base = require("./base");
class Dot extends Base {
  static create() {
    ping();
    return new Dot();
  }
}
class Circle extends ns.Shape {}
class Mixed extends mixin(Base) {}
class Plain {}
function build() {
  ping();
  return new ns.Circle();
}
new Dot();
new (make())();
new this.constructor();
`
	for _, path := range []string{"classes.js", "classes.jsx", "classes.mjs", "classes.cjs"} {
		t.Run(path, func(t *testing.T) {
			res, err := NewJavaScriptExtractor().Extract(path, []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			type edgeKey struct {
				from, to string
				line     int
			}
			wantExtends := map[edgeKey]bool{
				{path + "::Dot", "unresolved::Base", 2}:     true,
				{path + "::Circle", "unresolved::Shape", 8}: true,
			}
			wantNew := map[edgeKey]bool{
				{path + "::Dot.create", "unresolved::Dot", 5}: true,
				{path + "::build", "unresolved::Circle", 13}:  true,
				{path, "unresolved::Dot", 15}:                 true,
			}
			wantCalls := map[edgeKey]bool{
				{path, "unresolved::require", 1}:               true,
				{path + "::Dot.create", "unresolved::ping", 4}: true,
				{path, "unresolved::mixin", 9}:                 true,
				{path + "::build", "unresolved::ping", 12}:     true,
				{path, "unresolved::make", 16}:                 true,
			}
			for kind, want := range map[graph.EdgeKind]map[edgeKey]bool{
				graph.EdgeExtends: wantExtends, graph.EdgeInstantiates: wantNew, graph.EdgeCalls: wantCalls,
			} {
				got := map[edgeKey]bool{}
				count := 0
				for _, e := range res.Edges {
					if e.Kind != kind {
						continue
					}
					count++
					got[edgeKey{e.From, e.To, e.Line}] = true
					if e.FilePath != path {
						t.Errorf("%s file = %q, want %q", kind, e.FilePath, path)
					}
				}
				if !reflect.DeepEqual(got, want) || count != len(want) {
					t.Errorf("%s edges = %v (count %d), want %v", kind, got, count, want)
				}
			}
			wantNodes := map[string]graph.NodeKind{
				path:                  graph.KindFile,
				path + "::Base":       graph.KindVariable,
				path + "::Dot":        graph.KindType,
				path + "::Dot.create": graph.KindMethod,
				path + "::Circle":     graph.KindType,
				path + "::Mixed":      graph.KindType,
				path + "::Plain":      graph.KindType,
				path + "::build":      graph.KindFunction,
			}
			gotNodes := map[string]graph.NodeKind{}
			for _, n := range res.Nodes {
				gotNodes[n.ID] = n.Kind
			}
			if !reflect.DeepEqual(gotNodes, wantNodes) || len(res.Nodes) != len(wantNodes) {
				t.Errorf("nodes = %v, want %v", gotNodes, wantNodes)
			}
			imports := edgesOfKind(res.Edges, graph.EdgeImports)
			if len(imports) != 1 || imports[0].From != path || imports[0].To != "unresolved::import::./base" {
				t.Errorf("imports = %v, want file -> ./base", imports)
			}
		})
	}
}
