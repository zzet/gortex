package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func csharpNodeByID(t *testing.T, result *parser.ExtractionResult, id string) *graph.Node {
	t.Helper()
	for _, n := range result.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %s not extracted", id)
	return nil
}

// A partial property whose declaring fragment extracts first keeps the
// declaring fragment's lines on its node, while its calls are owned by
// the implementing fragment's span. That span has to travel with the
// node (issue #731): downstream consumers that test a call's line
// against the owner's extent would otherwise refuse every site the
// extractor deliberately attributed to it.
func TestCSharpExtractor_PartialPropertyStampsOwnershipSpan(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
    }
    public partial class KPart {
        private readonly Crank _crank = new Crank();
        public partial int P { get; set; }
    }
    public partial class KPart {
        public partial int P {
            get { return _crank.Turn(); }
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	p := csharpNodeByID(t, result, "App.cs::KPart.P")
	assert.Equal(t, 7, p.StartLine, "node keeps the declaring fragment's line")
	assert.Equal(t, 7, p.EndLine)
	assert.Equal(t, 10, p.Meta["ownership_start_line"], "recorded ownership span starts at the implementing fragment")
	assert.Equal(t, 12, p.Meta["ownership_end_line"])

	calls := callEdgesFrom(result.Edges, "App.cs::KPart.P", "Turn")
	require.Len(t, calls, 1)
	assert.Equal(t, 11, calls[0].Line, "the owned call sits inside the stamped span, outside the node's")
}

// When the node's lines already contain the recorded span (the ordinary
// property, and the implementing-fragment-first order) nothing is
// stamped: the node's own lines answer, and the meta stays as before.
func TestCSharpExtractor_OwnershipSpanStampedOnlyWhenItDiffers(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
    }
    public partial class KPart {
        private readonly Crank _crank = new Crank();
        public partial int P {
            get { return _crank.Turn(); }
        }
        public int Q { get { return _crank.Turn(); } }
    }
    public partial class KPart {
        public partial int P { get; set; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, id := range []string{"App.cs::KPart.P", "App.cs::KPart.Q"} {
		n := csharpNodeByID(t, result, id)
		_, hasStart := n.Meta["ownership_start_line"]
		_, hasEnd := n.Meta["ownership_end_line"]
		assert.False(t, hasStart || hasEnd, "%s: span equals the node's, no stamp expected; meta=%v", id, n.Meta)
	}
}
