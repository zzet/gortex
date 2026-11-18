package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

// Both a completed full resolve and a file reparse must bind the first read
// to the zero-value declaration, never to a later assignment's flow node.
func TestZeroInitializedLocalReadsKeepColdAndFileResolution(t *testing.T) {
	const path = "fixture.go"
	const src = `package fixture
func Consume(int) {}
func Run() {
 var total int
 Consume(total)
 total += 1
 Consume(total)
}
`
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			open := func() graph.Store {
				if backend == "memory" {
					return graph.New()
				}
				s, err := store_sqlite.Open(t.TempDir() + "/graph.sqlite")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				return s
			}
			extract := func(g graph.Store, text string) {
				result, err := languages.NewGoExtractor().Extract(path, []byte(text))
				require.NoError(t, err)
				g.AddBatch(result.Nodes, result.Edges)
			}
			full := open()
			extract(full, src+"\n// EOF\n")
			r := New(full)
			r.SetStampTerminal(true)
			r.ResolveAll()
			warm := open()
			extract(warm, src)
			w := New(warm)
			w.SetStampTerminal(true)
			w.ResolveAll()
			warm.EvictFile(path)
			extract(warm, src+"\n// EOF\n")
			w.ResolveFilesAndIncoming([]string{path})
			reads := func(g graph.Store) map[int]string {
				out := map[int]string{}
				for _, e := range g.GetOutEdges(path + "::Run") {
					if e.Kind == graph.EdgeReads && (e.Line == 5 || e.Line == 7) {
						out[e.Line] = e.To
					}
				}
				require.Equal(t, path+"::Run#local:total@+2", out[5], "first read belongs to the real declaration at4")
				require.Equal(t, path+"::Run#local:total@+4", out[7], "later read retains the assignment flow identity at6")
				return out
			}
			require.Equal(t, reads(full), reads(warm))
		})
	}
}
