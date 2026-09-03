package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A query continuation (`select ... into g`) ends every earlier range
// variable's scope: a receiver spelled like a field AFTER the continuation
// reads the field again, while the same spelling inside the variable's own
// tail stays the range variable. queryTail clips each binding clause's
// extent at the first continuation past it - the pin for that clipping,
// which the per-query continuation memo must preserve.
func TestCSharpBindingScopes_QueryContinuationEndsRangeVariable(t *testing.T) {
	src := []byte(`using System.Linq;
namespace App {
    public sealed class QCBag {
        public int Peek() { return 0; }
    }
    public sealed class QCFlow {
        private QCBag store = new QCBag();
        // The continuation-free query comes FIRST on purpose: a memo that
        // keyed continuations too coarsely (one entry for the whole file)
        // would be primed by this query's empty list and then hide the
        // continuation of the next one.
        public void RangeVarNoContinuation(QCBag[] xs) {
            var q = from store in xs select store.Peek();
            _ = q;
        }
        public void RangeVarThenContinuation(QCBag[] xs) {
            var q = from store in xs select store.Peek() into g select store.Peek();
            _ = q;
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("QC.cs", src)
	require.NoError(t, err)

	assert.Equal(t, 1, csharpFieldEdgeCount(res, "QC.cs::QCFlow.RangeVarThenContinuation", "store", graph.EdgeReads),
		"the continuation ended the range variable's scope - the later receiver read is the FIELD")
	assert.Equal(t, 0, csharpFieldEdgeCount(res, "QC.cs::QCFlow.RangeVarNoContinuation", "store", graph.EdgeReads),
		"guard: without a continuation the range variable shadows the field to the query's end")
}

// One query expression with many binding clauses (issue 727): every clause
// asks queryTail for its extent, and rescanning the query's children per
// clause made extraction quadratic in clause count - 400 -> 800 clauses
// went 36 ms -> 129 ms and 6 MB -> 36 MB before the per-query memo.
func BenchmarkCSharpExtractLinqClauseHeavy(b *testing.B) {
	for _, n := range []int{400, 800} {
		var sb strings.Builder
		sb.WriteString("using System.Linq;\nnamespace App {\n    public class Q {\n        public object Run(int[] src) {\n            var r = from x0 in src\n")
		for i := 1; i <= n; i++ {
			sb.WriteString("                let x" + itoa(i) + " = x" + itoa(i-1) + " + 1\n")
		}
		sb.WriteString("                select x" + itoa(n) + ";\n            return r;\n        }\n    }\n}\n")
		src := []byte(sb.String())
		e := NewCSharpExtractor()
		b.Run(itoa(n)+"clauses", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := e.Extract("Query.cs", src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
