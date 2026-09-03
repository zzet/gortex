package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// C# permits every partial part to repeat the base class as long as the
// parts agree. The shared extends budget was a bare boolean that never
// inspected the base TARGET, so the second and later fragments demoted
// the repeat to EdgeImplements - a class posing as an interface in
// implementor fan-out and hierarchy walks (issue #725 item 2). The
// repeat names the same base class the budget already spent: it is
// DROPPED, not demoted.
func TestCSharpPartialBase_RepeatedBaseClassDroppedNotDemoted(t *testing.T) {
	count := func(t *testing.T, src string) (extends, implements int, implTargets map[string]int) {
		t.Helper()
		res, err := NewCSharpExtractor().Extract("P.cs", []byte(src))
		require.NoError(t, err)
		implTargets = map[string]int{}
		for _, e := range csharpBaseEdges(res, "P.cs::Box") {
			switch e.Kind {
			case graph.EdgeExtends:
				extends++
			case graph.EdgeImplements:
				implements++
				implTargets[e.To]++
			}
		}
		return
	}

	t.Run("two parts", func(t *testing.T) {
		extends, implements, implTargets := count(t, `namespace App {
    public class BaseA {}
    public interface IA {}
    public interface IB {}
    public partial class Box : BaseA, IA {}
    public partial class Box : BaseA, IB {}
}`)
		assert.Equal(t, 1, extends, "one base class, one extends edge - the repeat is the SAME base")
		assert.Equal(t, 2, implements, "IA and IB, nothing else")
		assert.Zero(t, implTargets["unresolved::BaseA"],
			"a repeated base CLASS must never surface as implements")
	})

	t.Run("three parts", func(t *testing.T) {
		extends, implements, implTargets := count(t, `namespace App {
    public class BaseA {}
    public interface IA {}
    public interface IB {}
    public interface IC {}
    public partial class Box : BaseA, IA {}
    public partial class Box : BaseA, IB {}
    public partial class Box : BaseA, IC {}
}`)
		assert.Equal(t, 1, extends)
		assert.Equal(t, 3, implements)
		assert.Zero(t, implTargets["unresolved::BaseA"])
	})

	// A DIFFERENT class name in a later fragment's base position is not
	// a repeat (it is invalid C#, but the extractor must not guess): the
	// existing demote-to-implements degrade stays for that shape.
	t.Run("different class keeps the demote", func(t *testing.T) {
		extends, implements, implTargets := count(t, `namespace App {
    public class BaseA {}
    public class BaseB {}
    public partial class Box : BaseA {}
    public partial class Box : BaseB {}
}`)
		assert.Equal(t, 1, extends)
		assert.Equal(t, 1, implements)
		assert.NotZero(t, implTargets["unresolved::BaseB"])
	})
}
