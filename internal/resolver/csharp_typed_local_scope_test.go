package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// An explicitly-TYPED local dies with its block exactly like a var
// local, but the typed lookup (tenv/builtin maps) was function-wide:
// `{ int TLBagExt = 1; }` kept answering "int" for every later site in
// the method, so the static-form call after the block resolved its
// receiver as int and landed on unresolved::*.Add (round-5 finding 2 -
// the var twin of the committed nested-block pin, which passes because
// var locals ride the offset-aware scope records).
// A switch SECTION is not a C# declaration space - the switch BLOCK is
// (that is exactly why redeclaring a name in a sibling section is
// CS0128). A typed local declared in one section is alive in its
// siblings, so a sibling-section use must keep the receiver's type and
// an assignment there must stay a local write, not a field write
// (round-6 finding B1).
//
// SwRunner declares a SAME-NAMED field: without it the no-field-write
// branch was unreachable - writes are only minted for names that ARE
// fields of the enclosing type, so the assertion held vacuously however
// the extent was drawn (issue #726).
func TestResolveCSharpTypedLocal_AliveAcrossSwitchSections(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Switch.cs": `namespace App {
    public interface ISwConverter { string Convert(int n); }
    public class SwEnglish : ISwConverter { public string Convert(int n) { return "en"; } }
    public class SwRunner {
        private ISwConverter conv;
        public void Run(ISwConverter c, int k) {
            switch (k) {
                case 1: ISwConverter conv = c; conv.Convert(1); break;
                default: conv = c; conv.Convert(2); break;
            }
        }
    }
}`,
	})
	New(g).ResolveAll()

	const iface = "Switch.cs::ISwConverter.Convert"
	resolved := 0
	for _, to := range callsFrom(g, "Switch.cs::SwRunner.Run") {
		if to == iface {
			resolved++
		}
	}
	assert.Equal(t, 2, resolved,
		"conv is declared in `case 1:` and still alive in `default:` - both Convert sites carry the receiver's interface type")

	for _, e := range g.AllEdges() {
		if e != nil && e.Kind == graph.EdgeWrites && e.From == "Switch.cs::SwRunner.Run" {
			t.Fatalf("the sibling-section assignment targets a live LOCAL - no field write may be minted, got writes -> %s", e.To)
		}
	}
}

// The var-tier passes skipped a declaration when the FUNCTION-WIDE tenv
// already knew the name - so a second same-named `var` in a SIBLING
// scope never minted its own offset record, csharpTypedLocalAt answered
// Expired at the second site, and the receiver evidence vanished
// (round-6 finding B5). The guard must be per-record: every declaration
// mints its own record and the offset lookup never sees a name it only
// half-knows.
func TestResolveCSharpTypedLocal_SiblingScopeRedeclarationKeepsItsRecord(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sibling.cs": `namespace App {
    public class SbConverter { public string Convert(int n) { return "en"; } }
    public class SbOther { public string Convert(int n) { return "uk"; } }
    public class SbRunner {
        public void Run(bool flag) {
            if (flag) { var conv = new SbConverter(); conv.Convert(1); }
            else      { var conv = new SbConverter(); conv.Convert(2); }
        }
    }
}`,
	})
	New(g).ResolveAll()

	resolved := 0
	for _, to := range callsFrom(g, "Sibling.cs::SbRunner.Run") {
		if to == "Sibling.cs::SbConverter.Convert" {
			resolved++
		}
	}
	assert.Equal(t, 2, resolved,
		"each sibling-scope `var conv` declares its own receiver - both Convert sites must resolve")
}

func TestResolveCSharpExtension_ExpiredTypedLocalKeepsStaticForm(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System.Collections.Generic;
namespace Lib {
    public sealed class TLBag { }
    public static class TLBagExt {
        public static IEnumerable<int> Add(this TLBag bag, int value) { return new int[0]; }
        public static IEnumerable<int> Add(this TLBag bag, int value, int extra) { return new int[0]; }
    }
}`,
		"Caller.cs": `using System.Collections.Generic;
using Lib;
namespace App {
    public class Use {
        public IEnumerable<int> ExpiredTypedLocal(TLBag bag) {
            { int TLBagExt = 1; _ = TLBagExt; }
            return TLBagExt.Add(bag, 5);
        }
        public IEnumerable<int> Control(TLBag bag) {
            return TLBagExt.Add(bag, 5);
        }
    }
}`,
	})
	New(g).ResolveAll()

	const twoParam = "Ext.cs::TLBagExt.Add"

	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.Control", "Add"),
		"control: no local anywhere - the static form binds")
	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.ExpiredTypedLocal", "Add"),
		"the typed local's block has closed before the call - the receiver is the static class again, not an int")
}
