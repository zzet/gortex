package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func csharpFieldEdgeCount(res *parser.ExtractionResult, fromID, name string, kind graph.EdgeKind) int {
	n := 0
	for _, e := range res.Edges {
		if e.Kind == kind && e.From == fromID && e.To == "unresolved::*."+name {
			n++
		}
	}
	return n
}

// A same-named binder elsewhere in the method must not delete a genuine
// field use outside its extent (round-5 finding 3): the assignment
// buffer carried no coordinate (offset -1), so `_box = next;` AFTER a
// foreach whose loop variable was also named _box fell to the
// function-wide shadow question and the write vanished. Reads had the
// parallel hole through the function-wide builtin veto: an expired
// `{ int _box = 0; }` deleted a later `_box.Fill()` receiver read.
func TestCSharpFieldIdentifier_UsesSurviveExpiredShadows(t *testing.T) {
	src := []byte(`namespace App {
    public sealed class FWBag {
        public void Fill() { }
    }
    public sealed class FWFlow {
        private FWBag _box = new FWBag();
        public void WriteAfterForeach(int[] xs, FWBag next) {
            foreach (var _box in xs) { _ = _box; }
            _box = next;
        }
        public void WriteBeforeForeach(int[] xs, FWBag next) {
            _box = next;
            foreach (var _box in xs) { _ = _box; }
        }
        public void ReadAfterExpiredBuiltin() {
            { int _box = 0; _ = _box; }
            _box.Fill();
        }
        public void LocalOnly() {
            int _box = 1;
            _box = 2;
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("F.cs", src)
	require.NoError(t, err)

	assert.Equal(t, 1, csharpFieldEdgeCount(res, "F.cs::FWFlow.WriteAfterForeach", "_box", graph.EdgeWrites),
		"the loop variable's scope closed with the foreach - the later assignment writes the FIELD")
	assert.Equal(t, 1, csharpFieldEdgeCount(res, "F.cs::FWFlow.WriteBeforeForeach", "_box", graph.EdgeWrites),
		"the loop variable's scope has not opened yet - the earlier assignment writes the FIELD")
	assert.Equal(t, 1, csharpFieldEdgeCount(res, "F.cs::FWFlow.ReadAfterExpiredBuiltin", "_box", graph.EdgeReads),
		"the builtin-typed local died with its block - the later call receiver reads the FIELD")
	assert.Equal(t, 0, csharpFieldEdgeCount(res, "F.cs::FWFlow.LocalOnly", "_box", graph.EdgeWrites),
		"guard: an assignment to a LIVE same-named local is not a field write")
}

// A pattern variable declared in a catch FILTER scopes over its catch
// clause, and one declared in a query clause over its query - not over
// the whole method. csharpLocalScopeOf climbed past both (neither
// catch_clause nor query_expression formed a scope), so
// `catch (...) when (o is int store)` shadowed `store` method-wide and
// an EARLIER receiver read of the `store` field lost its reads edge
// (round-6 non-blocking finding 1; PR-introduced - the merge base
// mints the edge).
func TestCSharpFieldIdentifier_FilterAndQueryPatternsScopeToTheirClause(t *testing.T) {
	src := []byte(`using System.Linq;
namespace App {
    public sealed class CFBag {
        public void Fill() { }
    }
    public sealed class CFFlow {
        private CFBag store = new CFBag();
        public void ReadBeforeCatchFilter(object o) {
            store.Fill();
            try { } catch (System.Exception e) when (o is int store) { _ = store; }
        }
        public void ReadBeforeQueryPattern(object o, int[] xs) {
            store.Fill();
            var q = from x in xs where o is int store select x;
            _ = q;
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("CF.cs", src)
	require.NoError(t, err)

	assert.Equal(t, 1, csharpFieldEdgeCount(res, "CF.cs::CFFlow.ReadBeforeCatchFilter", "store", graph.EdgeReads),
		"the filter pattern's scope is its catch clause - the earlier receiver read is the FIELD")
	assert.Equal(t, 1, csharpFieldEdgeCount(res, "CF.cs::CFFlow.ReadBeforeQueryPattern", "store", graph.EdgeReads),
		"the query pattern's scope is its query - the earlier receiver read is the FIELD")
}
