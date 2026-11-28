package mcp

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// TestWatchedRelationFreshnessKinds holds a real watcher publication for the
// edit kinds the watched matrix did not hold yet, and reads the callers of a
// fixed target through each machine output that reports relation freshness:
// registered JSON (get_callers), facade JSON and facade TOON (relations).
//
//   - rename: a tool edit renames the caller, so the caller edge moves from
//     the old name to the new one;
//   - native_write: a write outside any tool adds a call to an existing file;
//   - out_of_band_delete: a file holding a caller is removed outside any tool.
//
// A held reply must disclose pending relations for the changed file in band,
// claim nothing fresh, and hold one of the two real edge sets or only the
// edges both agree on: an old edge set without that disclosure is silent
// stale. After publication every reply
// holds the exact edge set, and once no generation is pending for the file it
// carries no relation state.
func TestWatchedRelationFreshnessKinds(t *testing.T) {
	const (
		target    = "main.go::Target"
		control   = "main.go::Control"
		mainFile  = "package main\nfunc Target() {}\nfunc Control() { Target() }\n"
		withCall  = "func Changed() int { Target(); return 1 }"
		withoutIt = "func Changed() int { return 2 }"
		renamed   = "func Renamed() int { Target(); return 1 }"
	)
	type edgeSet [2]int // caller -> target edges from callers[0] and callers[1]
	kinds := []struct {
		name   string
		files  map[string]string
		file   string // the changed file
		old    string // empty: the file is deleted out of band
		new    string
		native bool // written outside any tool
		// callers[0] is the caller before the change; callers[1], when set,
		// is the same caller under its new name.
		callers           [2]string
		gone              bool // callers[0] no longer exists after publication
		before, published edgeSet
	}{
		{
			name: "rename", files: map[string]string{"main.go": mainFile + withCall + "\n"},
			file: "main.go", old: withCall, new: renamed,
			callers: [2]string{"main.go::Changed", "main.go::Renamed"}, gone: true,
			before: edgeSet{1, 0}, published: edgeSet{0, 1},
		},
		{
			name: "native_write", files: map[string]string{"main.go": mainFile + withoutIt + "\n"},
			file: "main.go", old: withoutIt, new: withCall, native: true,
			callers: [2]string{"main.go::Changed"},
			before:  edgeSet{0, 0}, published: edgeSet{1, 0},
		},
		{
			name: "out_of_band_delete", files: map[string]string{
				"main.go": mainFile,
				"gone.go": "package main\nfunc Gone() { Target() }\n",
			},
			file: "gone.go", native: true,
			callers: [2]string{"gone.go::Gone"}, gone: true,
			before: edgeSet{1, 0}, published: edgeSet{0, 0},
		},
	}
	for _, kind := range kinds {
		for _, surface := range []string{"json", "facade_json", "facade_toon"} {
			if kind.name == "out_of_band_delete" && surface == "facade_toon" {
				continue // TestWatchedRelationFreshnessMatrix holds this cell
			}
			t.Run(kind.name+"/"+surface, func(t *testing.T) {
				facade, asTOON := surface != "json", surface == "facade_toon"
				f := newWatchedMatrixFixture(t, kind.files, kind.file, holdBeforeEviction, facade)
				stored := func() edgeSet {
					var set edgeSet
					for _, edge := range f.store.GetInEdges(target) {
						for i, caller := range kind.callers {
							if caller != "" && edge.From == caller && edge.Kind == graph.EdgeCalls {
								set[i]++
							}
						}
					}
					return set
				}
				probe := func(id int, phase string) string {
					t.Helper()
					tool, args := "get_callers", map[string]any{"id": target, "format": "json", "max_bytes": 0}
					if facade {
						format := "json"
						if asTOON {
							format = "toon"
						}
						tool, args = "relations", map[string]any{
							"operation": "callers",
							"target":    map[string]any{"symbol": target},
							"output":    map[string]any{"format": format, "max_bytes": 0},
						}
					}
					payload, state, fields := f.call(t, id, tool, args, asTOON, phase == "held")
					var got edgeSet
					controls := 0
					edges, _ := payload["edges"].([]any)
					for _, value := range edges {
						edge, ok := value.(map[string]any)
						require.True(t, ok)
						if edge["to"] != target || edge["kind"] != "calls" {
							continue
						}
						if edge["from"] == control {
							controls++
						}
						for i, caller := range kind.callers {
							if caller != "" && edge["from"] == caller {
								got[i]++
							}
						}
					}
					require.Equal(t, 1, controls, "%s: the control caller must stay in the relation set", phase)
					switch phase {
					case "held":
						f.requirePending(t, asTOON, state, fields)
						// A held reply serves the old edge set, the new one, or
						// only the edges both agree on: a held rename can drop
						// the old name, which the file no longer declares,
						// before the new name resolves. Anything else is an
						// invented edge set.
						agreed := edgeSet{min(kind.before[0], kind.published[0]), min(kind.before[1], kind.published[1])}
						require.Contains(t, []edgeSet{kind.before, kind.published, agreed}, got, "held reply invented an edge set")
					case "before":
						require.Equal(t, kind.before, got)
						require.Empty(t, state)
					case "recovered":
						require.Equal(t, kind.published, got, "a reply after publication returned a stale edge set")
						if state != "" {
							f.requirePending(t, asTOON, state, fields)
						}
					}
					return state
				}

				require.Equal(t, kind.before, stored())
				probe(2, "before")
				changed := strings.Replace(kind.files[kind.file], kind.old, kind.new, 1)
				var ticket *indexer.MutationTicket
				var receipt *mutationReceipt
				switch {
				case kind.native && kind.old == "":
					f.store.armed.Store(true)
					require.NoError(t, os.Remove(f.path))
				case kind.native:
					f.store.armed.Store(true)
					require.NoError(t, os.WriteFile(f.path, []byte(changed), 0o600))
				case facade:
					ticket, receipt = f.edit(t, 3, map[string]any{
						"operation": "file", "target": map[string]any{"file": kind.file},
						"match": kind.old, "replacement": kind.new,
						"guard":  map[string]any{"expected_occurrences": 1},
						"output": map[string]any{"format": "json", "max_bytes": 0},
					})
				default:
					ticket, receipt = f.edit(t, 3, map[string]any{
						"path": kind.file, "old_string": kind.old, "new_string": kind.new,
						"expected_occurrences": 1, "format": "json", "max_bytes": 0,
					})
				}
				f.awaitHold(t)
				if kind.old == "" {
					_, err := os.Stat(f.path)
					require.ErrorIs(t, err, os.ErrNotExist)
				} else {
					bytes, err := os.ReadFile(f.path)
					require.NoError(t, err)
					require.Equal(t, changed, string(bytes))
				}
				if ticket != nil {
					require.True(t, ticket.TierState().RelationsAt.IsZero())
				}
				held := f.watcher.TierState(f.path)
				require.NotZero(t, held.Generation)
				require.True(t, held.RelationsAt.IsZero())
				require.Equal(t, kind.before, stored())
				probe(4, "held")
				require.Equal(t, kind.before, stored(), "publication must stay held through the reply")

				f.release(t, receipt)
				if ticket != nil {
					require.False(t, ticket.TierState().RelationsAt.IsZero())
				}
				if kind.gone {
					require.Nil(t, f.store.GetNode(kind.callers[0]))
				}
				require.Equal(t, kind.published, stored())
				f.recovered(t, func(id int) string { return probe(id, "recovered") })
			})
		}
	}
}
