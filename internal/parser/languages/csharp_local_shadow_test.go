package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestCSharpExtractor_ReceiverlessCallShadowedByLocalIsStamped: a bare
// `Clamp(1)` whose simple name a local declaration binds — a local
// function, a delegate-typed parameter, a delegate-typed local — is that
// local (C# spec §12.8.4: the enclosing declaration spaces come before
// members, bases and every using-static import). No node is emitted for
// a local function, so the resolver would otherwise bind the outer
// candidate; the call edge carries Meta["local_shadow"] so every tier
// can refuse. A call nothing local binds carries no stamp.
func TestCSharpExtractor_ReceiverlessCallShadowedByLocalIsStamped(t *testing.T) {
	src := []byte(`namespace App
{
    public class Runner
    {
        public int Clamp(int x) { return x; }

        public int ViaLocalFunction()
        {
            int Clamp(int x) { return x + 1; }
            return Clamp(1);
        }

        public int ViaDelegateParam(System.Func<int, int> Clamp)
        {
            return Clamp(1);
        }

        public int ViaDelegateLocal()
        {
            System.Func<int, int> Clamp = x => x;
            return Clamp(1);
        }

        public int LocalFunctionDeclaredAfterUse()
        {
            int r = Clamp(1);
            int Clamp(int x) { return x + 2; }
            return r;
        }

        public int Plain()
        {
            return Clamp(1);
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Runner.cs", src)
	require.NoError(t, err)

	edgeFor := func(owner string) *graph.Edge {
		t.Helper()
		var found *graph.Edge
		for _, e := range res.Edges {
			if e.Kind == graph.EdgeCalls && e.From == "Runner.cs::Runner."+owner && e.To == "unresolved::Clamp" {
				require.Nil(t, found, "one Clamp call per fixture method")
				found = e
			}
		}
		require.NotNil(t, found, "%s: the bare Clamp call must still be emitted", owner)
		return found
	}
	for _, owner := range []string{"ViaLocalFunction", "ViaDelegateParam", "ViaDelegateLocal", "LocalFunctionDeclaredAfterUse"} {
		e := edgeFor(owner)
		shadow, _ := e.Meta["local_shadow"].(bool)
		assert.True(t, shadow, "%s: a local declaration binds the simple name — the edge must say so", owner)
	}
	plain := edgeFor("Plain")
	_, stamped := plain.Meta["local_shadow"]
	assert.False(t, stamped, "Plain: nothing local binds Clamp, no stamp")
}
