package languages

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The shape pass's per-record guard, distinguished from the function-wide
// map it replaced (issue #726: the guard's revert left the suite green).
// Two same-named explicitly-typed locals in SIBLING scopes carry
// DIFFERENT generic shapes; each call site must stamp ITS declaration's
// shape. A function-wide guard skips the second declaration, its record
// never learns a shape, and the second site loses the receiver_shape
// stamp the generic-extension applicability veto reads.
func TestCSharpSiblingShape_RedeclarationStampsPerSite(t *testing.T) {
	src := []byte(`using System.Collections.Generic;
namespace App {
    public static class ShpExt {
        public static void Drain(this List<int> rack) { }
        public static void Drain(this List<string> rack) { }
    }
    public sealed class ShpRunner {
        public void Run(List<int> a, List<string> b, bool flag) {
            if (flag) { List<int> conv = a; conv.Drain(); }
            else      { List<string> conv = b; conv.Drain(); }
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Shp.cs", src)
	require.NoError(t, err)

	type site struct {
		line  int
		shape string
	}
	var sites []site
	for _, e := range res.Edges {
		if e == nil || e.Kind != graph.EdgeCalls || e.From != "Shp.cs::ShpRunner.Run" || e.To != "unresolved::*.Drain" {
			continue
		}
		require.NotNil(t, e.Meta, "line %d: the receiver is a typed local - evidence must ride", e.Line)
		shape, _ := e.Meta["receiver_shape"].(string)
		sites = append(sites, site{line: e.Line, shape: shape})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].line < sites[j].line })
	require.Len(t, sites, 2, "fixture: both sibling Drain sites must emit")
	assert.Equal(t, "List<int>", sites[0].shape,
		"the first declaration's generic arguments survive at ITS site")
	assert.Equal(t, "List<string>", sites[1].shape,
		"the sibling redeclaration mints its OWN record - a function-wide guard starves it and the stamp vanishes")
}
