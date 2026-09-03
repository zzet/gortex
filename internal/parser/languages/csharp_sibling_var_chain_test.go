package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Sibling `var` redeclarations mint per-scope offset records, but
// setLocalType also writes the function-wide tenv last-wins - and the
// chained-receiver and awaited paths consulted THAT map, offset-blind
// (issue #725 item 3: first-wins flipped to last-wins; every revision
// was wrong on one of the two sites). The chain/awaited walkers now
// receive an env whose HEAD entry is corrected by the offset-aware
// records, so each site types through its own block's local.
func csharpPingReceivers(t *testing.T, src string) map[int]string {
	t.Helper()
	res, err := NewCSharpExtractor().Extract("C.cs", []byte(src))
	require.NoError(t, err)
	got := map[int]string{}
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls || e.Meta == nil {
			continue
		}
		if e.To == "unresolved::*.Ping" {
			if rt, _ := e.Meta["receiver_type"].(string); rt != "" {
				got[e.Line] = rt
			}
		}
	}
	return got
}

func TestCSharpSiblingVar_ChainedReceiverTypesPerSite(t *testing.T) {
	got := csharpPingReceivers(t, `namespace App {
    public class Widget { public void Ping() { } }
    public class Gadget { public void Ping() { } }
    public class MakerA { public Widget Make() { return null; } }
    public class MakerB { public Gadget Make() { return null; } }
    public sealed class ChainFlow {
        public void Run() {
            { var h = new MakerA(); h.Make().Ping(); }
            { var h = new MakerB(); h.Make().Ping(); }
        }
    }
}`)
	assert.Equal(t, "Widget", got[8], "first block's h is a MakerA - its chain types Widget, got %v", got)
	assert.Equal(t, "Gadget", got[9], "second block's h is a MakerB - its chain types Gadget, got %v", got)
}

func TestCSharpSiblingVar_AwaitedReceiverTypesPerSite(t *testing.T) {
	got := csharpPingReceivers(t, `namespace App {
    public class Widget { public void Ping() { } }
    public class Gadget { public void Ping() { } }
    public class LoaderA { public System.Threading.Tasks.Task<Widget> Load() { return null; } }
    public class LoaderB { public System.Threading.Tasks.Task<Gadget> Load() { return null; } }
    public sealed class AwaitFlow {
        public async System.Threading.Tasks.Task Run() {
            { var h = new LoaderA(); (await h.Load()).Ping(); }
            { var h = new LoaderB(); (await h.Load()).Ping(); }
        }
    }
}`)
	assert.Equal(t, "Widget", got[8], "first block awaits LoaderA.Load - Task<Widget> unwraps per site, got %v", got)
	assert.Equal(t, "Gadget", got[9], "second block awaits LoaderB.Load - Task<Gadget> unwraps per site, got %v", got)
}

func TestCSharpSiblingVar_AwaitAssignedLocalTypesPerSite(t *testing.T) {
	got := csharpPingReceivers(t, `namespace App {
    public class Widget { public void Ping() { } }
    public class Gadget { public void Ping() { } }
    public class LoaderA { public System.Threading.Tasks.Task<Widget> Load() { return null; } }
    public class LoaderB { public System.Threading.Tasks.Task<Gadget> Load() { return null; } }
    public sealed class VarFlow {
        public async System.Threading.Tasks.Task Run() {
            { var h = new LoaderA(); var w = await h.Load(); w.Ping(); }
            { var h = new LoaderB(); var w = await h.Load(); w.Ping(); }
        }
    }
}`)
	assert.Equal(t, "Widget", got[8], "tier-2 types the first w through ITS block's h, got %v", got)
	assert.Equal(t, "Gadget", got[9], "tier-2 types the second w through ITS block's h, got %v", got)
}
