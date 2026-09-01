package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// `receiver_name` is the only evidence that tells the binder a call is the
// STATIC form of an extension call — where the first argument fills the
// `this` slot rather than the receiver. The extractor refuses that stamp
// when a parameter or local declares the receiver's name, which is right:
// such a name is the local, not a static class.
//
// The refusal's SCOPE is wrong. `localNamesByOwner` is keyed on the
// enclosing function, so a local buried in a nested block vetoes the stamp
// for every call in the method — including calls the local cannot possibly
// bind at, because its block has already closed. The evidence vanishes, the
// binder reads the call as extension form, subtracts a `this` slot the
// argument list had actually filled, and the arity window lands one
// parameter too wide.
//
// Nothing here is generic or dispatch-related: this is the extension
// binder reading a two-argument call as three.
func TestResolveCSharpExtension_NestedBlockLocalKeepsStaticForm(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace Lib {
    public class Bag { }
    public static class BagExt {
        public static void Add(this Bag b, int x) { }
        public static void Add(this Bag b, int x, int y) { }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Use {
        public void Shadowed(Bag bag) {
            if (bag != null) { var BagExt = 1; System.Console.WriteLine(BagExt); }
            BagExt.Add(bag, 5);
        }
        public void Control(Bag bag) {
            BagExt.Add(bag, 5);
        }
    }
}`,
	})
	New(g).ResolveAll()

	// Ext.cs:4 takes (this Bag, int) — two parameters, which is what a
	// static-form `BagExt.Add(bag, 5)` fills. Ext.cs:5 takes three.
	const twoParam = "Ext.cs::BagExt.Add"

	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.Control", "Add"),
		"control: with no local anywhere in the method the static form already binds correctly")
	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.Shadowed", "Add"),
		"a local in a closed nested block shadows nothing at the call site and must not cost the call its static-form evidence")
}

// The extent recorded for a binding decides where the shadow refusal
// fires, and "the nearest block ancestor" over-widens for every binder
// that sits in a scope the grammar does not spell as a block: a switch
// section, a switch-expression arm, a loop condition, an expression
// lambda. Each of these bodies binds `BagExt` somewhere the call at the
// end can never see, so the static-form evidence must survive.
//
// An if-condition pattern is deliberately NOT here: `if (o is int x)`
// genuinely escapes to the enclosing block (definite-assignment
// scoping), so the block extent is its correct one.
func TestResolveCSharpExtension_NonBlockScopesKeepStaticForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"switch-section pattern variable", `
        public void M(Bag bag, object o) {
            switch (o) { case int BagExt: System.Console.WriteLine(BagExt); break; }
            BagExt.Add(bag, 5);
        }`},
		{"switch-expression arm", `
        public void M(Bag bag, object o) {
            var q = o switch { int BagExt => BagExt, _ => 0 };
            BagExt.Add(bag, 5);
        }`},
		{"out var inside an expression lambda", `
        public void M(Bag bag, int[] xs, System.Collections.Generic.Dictionary<int,int> map) {
            var ok = System.Linq.Enumerable.Any(xs, x => map.TryGetValue(x, out var BagExt));
            BagExt.Add(bag, 5);
        }`},
		{"declaration pattern inside an expression lambda", `
        public void M(Bag bag, int[] xs) {
            var ok = System.Linq.Enumerable.Any(xs, x => ((object)x) is int BagExt);
            BagExt.Add(bag, 5);
        }`},
		{"pattern variable in a while condition", `
        public void M(Bag bag, object o) {
            while (o is int BagExt) { System.Console.WriteLine(BagExt); break; }
            BagExt.Add(bag, 5);
        }`},
		{"switch-section local declaration", `
        public void M(Bag bag, object o) {
            switch (((object)1)) { default: var BagExt = 1; System.Console.WriteLine(BagExt); break; }
            BagExt.Add(bag, 5);
        }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildCSharpResolverGraph(t, map[string]string{
				"Ext.cs": `namespace Lib {
    public class Bag { }
    public static class BagExt {
        public static void Add(this Bag b, int x) { }
        public static void Add(this Bag b, int x, int y) { }
    }
}`,
				"Caller.cs": `using Lib;
namespace App {
    public class Use {
` + tc.body + `
    }
}`,
			})
			New(g).ResolveAll()

			assert.Equal(t, "Ext.cs::BagExt.Add",
				namedCallTarget(t, g, "Caller.cs::Use.M", "Add"),
				"the binder's scope has closed before the call, so the static form keeps its two-parameter overload")
		})
	}
}
