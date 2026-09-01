package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A foreach variable is not in scope in its own collection expression,
// and a query clause's range variable is not in scope in its own source:
// `foreach (var FQBagExt in FQBagExt.Add(bag, 5))` still names the
// static class in the header, so the call is the STATIC form of an
// extension call and must bind the two-parameter overload.
//
// The recorded extent started the shadow at the beginning of the whole
// foreach / query expression, so `receiver_name` was refused before the
// variable existed and the binder read static form as instance form -
// subtracting a `this` slot the argument list had actually filled and
// landing one parameter wide.
func TestResolveCSharpExtension_ForeachSourceKeepsStaticForm(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System.Collections.Generic;
namespace Lib {
    public sealed class FQBag { }
    public static class FQBagExt {
        public static IEnumerable<int> Add(this FQBag bag, int value) { return new int[0]; }
        public static IEnumerable<int> Add(this FQBag bag, int value, int extra) { return new int[0]; }
    }
}`,
		"Caller.cs": `using System.Collections.Generic;
using Lib;
namespace App {
    public class Use {
        public void ForeachSource(FQBag bag) {
            foreach (var FQBagExt in FQBagExt.Add(bag, 5)) { _ = FQBagExt; }
        }
        public void AfterLoop(FQBag bag, int[] xs) {
            foreach (var FQBagExt in xs) { _ = FQBagExt; }
            FQBagExt.Add(bag, 5);
        }
    }
}`,
	})
	New(g).ResolveAll()

	// Ext.cs declares the 2-param overload first, so its node keeps the
	// bare name; the 3-param sibling carries the line suffix.
	const twoParam = "Ext.cs::FQBagExt.Add"

	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.ForeachSource", "Add"),
		"the loop variable is not in scope in its own collection expression - the header call keeps static form")
	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.AfterLoop", "Add"),
		"guard: the loop variable's scope still ends with the statement")
}

// The query half of the same finding, one clause shape per subtest: a
// range variable begins only after its own source/RHS, a join variable
// is not in scope in its own `in` expression, a joined variable dies at
// `into`, and a query continuation ends every earlier range variable's
// scope.
func TestResolveCSharpExtension_QueryClauseSourcesKeepStaticForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"first from source", `
        public IEnumerable<int> M(FQBag bag) {
            return from FQBagExt in FQBagExt.Add(bag, 5)
                   select FQBagExt;
        }`},
		{"second from source", `
        public IEnumerable<int> M(FQBag bag, int[] xs) {
            return from x in xs
                   from FQBagExt in FQBagExt.Add(bag, 5)
                   select x + FQBagExt;
        }`},
		{"join in-expression", `
        public IEnumerable<int> M(FQBag bag, int[] xs) {
            return from x in xs
                   join FQBagExt in FQBagExt.Add(bag, 5) on x equals FQBagExt
                   select x;
        }`},
		{"joined variable dead after into", `
        public IEnumerable<IEnumerable<int>> M(FQBag bag, int[] xs) {
            return from x in xs
                   join FQBagExt in xs on x equals FQBagExt into g
                   select FQBagExt.Add(bag, 5);
        }`},
		{"continuation ends earlier range variables", `
        public IEnumerable<IEnumerable<int>> M(FQBag bag, int[] xs) {
            return from FQBagExt in xs
                   select FQBagExt into g
                   select FQBagExt.Add(bag, 5);
        }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildCSharpResolverGraph(t, map[string]string{
				"Ext.cs": `using System.Collections.Generic;
namespace Lib {
    public sealed class FQBag { }
    public static class FQBagExt {
        public static IEnumerable<int> Add(this FQBag bag, int value) { return new int[0]; }
        public static IEnumerable<int> Add(this FQBag bag, int value, int extra) { return new int[0]; }
    }
}`,
				"Caller.cs": `using System.Collections.Generic;
using System.Linq;
using Lib;
namespace App {
    public class Use {
` + tc.body + `
    }
}`,
			})
			New(g).ResolveAll()

			assert.Equal(t, "Ext.cs::FQBagExt.Add",
				namedCallTarget(t, g, "Caller.cs::Use.M", "Add"),
				"the range variable is not in scope at this call site, so the static form keeps its two-parameter overload")
		})
	}
}
