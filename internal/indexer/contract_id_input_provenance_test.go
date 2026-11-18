package indexer

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractIDReplayProvesCoreInputsBeforeLocalShapeWrites(t *testing.T) {
	for _, preload := range []bool{false, true} {
		t.Run(fmt.Sprint(preload), func(t *testing.T) {
			nodes := fileNodeInputs()
			id := nodes[1].ID
			missing := "repo/handler.go::Missing"
			e, _, _ := fileNodesEvidence(t, nodes, nil)
			if preload {
				e.GetFileNodesByPaths([]string{"repo/handler.go"})
			}
			selected := e.GetNodesByIDs([]string{id, missing})
			require.NoError(t, e.err)
			require.Contains(t, selected, id)
			require.NotContains(t, selected, missing)
			local := selected[id]
			local.Meta["shape"] = map[string]any{"kind": "struct", "fields": []any{map[string]any{"name": "name", "type": "string"}}}
			e.AddNode(local)
			reread := e.GetNodesByIDs([]string{id, missing})
			require.NoError(t, e.err)
			require.NotNil(t, reread[id].Meta["shape"], "consumers still read their locally snapshotted shape")
			reads := e.ledger.reads
			require.NotEmpty(t, reads)
			require.Contains(t, e.ledger.owners, "repo/handler.go")
			for _, change := range []string{"unchanged", "core_metadata", "core_deleted", "negative_added"} {
				t.Run(change, func(t *testing.T) {
					current := fileNodeInputs()
					switch change {
					case "core_metadata":
						current[1].Meta["signature"] = "func Handler(changed int)"
					case "core_deleted":
						current = current[:1]
					case "negative_added":
						current = append(current, &graph.Node{ID: missing, Name: "Missing", Kind: graph.KindType, FilePath: "repo/handler.go", RepoPrefix: "repo"})
					}
					next, _, _ := fileNodesEvidence(t, current, nil)
					c := &contractConvergence{ctx: t.Context(), snap: ContractFollowupSnapshot{Core: next.core}, guard: next.guard}
					equal, err := c.replay(next, reads, nil)
					require.NoError(t, err)
					require.Equal(t, change == "unchanged", equal, "local outputs are not core inputs; actual core metadata/removal/negative changes remain inputs")
				})
			}
		})
	}
}

func TestContractShapePostpassRechecksWithoutTreatingOwnWritesAsInputs(t *testing.T) {
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", serveItems) }\nfunc serveItems(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Item{}) }\nfunc writeJSON(w http.ResponseWriter, code int, body any) {}\n"
	types := "package fixture\ntype Item struct { Name string `json:\"name\"` }\n"
	files := map[string]string{"routes.go": routes, "types.go": types, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	armed, staged := false, false
	waits := 0
	var release func()
	hooks := &contractConvergenceHooks{beforeR1: func([]string) { armed = true }, openedEvidence: func(*store_sqlite.Store) {
		if armed && !staged {
			staged = true
			f.stageInFlight("fixture/types.go")
		}
	}}
	f.onWait = func() {
		waits++
		switch waits {
		case 1:
			release = f.beginHeld("fixture/types.go")
		case 2:
			if release != nil {
				release()
				release = nil
			}
		}
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, staged)
	require.GreaterOrEqual(t, waits, 2)
	require.Equal(t, 1, report.EnrichRuns)
	require.Zero(t, report.EnrichReruns)
	require.Positive(t, report.EnrichRechecks)
	want := coldContracts(t, files)
	require.Contains(t, fmt.Sprint(contractMeta(want)), "name")
	require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
}
