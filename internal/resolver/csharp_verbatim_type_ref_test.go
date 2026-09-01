package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// `@event` is the ONLY legal spelling of a keyword-named C# type, and
// the declaration side mints its node ID canonically (`<file>::event`).
// canonicalizeCSharpTypeRef stripped `?`, `[]`, generics and qualifiers
// but not the verbatim `@`, so every reference emitted
// `unresolved::@event` and the two halves never met: field, parameter
// and return types naming such a type lost their bindings entirely
// (issue #723 shape 1 - a total regression, only visible THROUGH the
// resolver because extraction always emits unresolved::).
func TestResolveCSharp_VerbatimTypeReferencesBind(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{"V.cs": `namespace App {
    public interface @event { void Put(int x); }
    public class Flow {
        private readonly @event _e;
        public @event Make() { return _e; }
        public void Take(@event e) { }
    }
}`})
	New(g).ResolveAll()

	wants := map[string]graph.EdgeKind{
		"V.cs::Flow._e":            graph.EdgeTypedAs,
		"V.cs::Flow.Make":          graph.EdgeReturns,
		"V.cs::Flow.Take#param:e@0": graph.EdgeTypedAs,
	}
	for from, kind := range wants {
		bound := false
		var got []string
		for _, e := range g.AllEdges() {
			if e == nil || e.From != from || e.Kind != kind {
				continue
			}
			got = append(got, e.To)
			if e.To == "V.cs::event" {
				bound = true
			}
			assert.False(t, strings.Contains(e.To, "@"),
				"[%s] the verbatim marker is spelling, not identity - it may never appear in a target ID, got %s", from, e.To)
		}
		assert.True(t, bound,
			"[%s] the %s edge must bind to the canonical declaration V.cs::event, got targets %v", from, kind, got)
	}
}
