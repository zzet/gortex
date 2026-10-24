package graph

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Distinct copied rows can differ between stores. These cases pin Graph's
// consumed-identity and first-creator rules, not cross-store convergence.
func TestGraphReindexEdgesFoldingGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moves [][2]string
		want  map[string]string
	}{
		{
			name:  "consumed_stored_identity",
			moves: [][2]string{{"X", "Y"}, {"X", "Z"}},
			want:  map[string]string{"Y": "a"},
		},
		{
			name:  "first_direct_creator",
			moves: [][2]string{{"X", "K"}, {"W", "K"}, {"K", "Z"}, {"Z", "Q"}},
			want:  map[string]string{"K": "b", "Q": "d"},
		},
		{
			name:  "first_folded_creator",
			moves: [][2]string{{"X", "Y"}, {"W", "K"}, {"Y", "K"}, {"K", "Z"}},
			want:  map[string]string{"K": "c", "Z": "d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New()
			g.AddNode(&Node{ID: "caller", FilePath: "a.go", RepoPrefix: "repo", Kind: KindFunction})
			stored := []string{"X"}
			if tc.name != "consumed_stored_identity" {
				stored = append(stored, "W")
			}
			for _, target := range stored {
				g.AddEdge(&Edge{From: "caller", To: target, Kind: EdgeCalls, FilePath: "a.go", Line: 10})
			}
			var batch []EdgeReindex
			for i, move := range tc.moves {
				row := &Edge{From: "caller", To: move[1], Kind: EdgeCalls, FilePath: "a.go", Line: 10,
					Origin: string(rune('a' + i))}
				batch = append(batch, EdgeReindex{Edge: row, OldTo: move[0]})
			}
			g.ReindexEdges(batch)
			for name, rows := range map[string][]*Edge{"all": g.AllEdges(), "out": g.GetOutEdges("caller")} {
				require.Len(t, rows, len(tc.want), name)
				got := make(map[string]string)
				for _, row := range rows {
					got[row.To] = row.Origin
				}
				require.Equal(t, tc.want, got, name)
			}
			for _, target := range []string{"X", "W", "Y", "K", "Z", "Q"} {
				rows := g.GetInEdges(target)
				if origin, ok := tc.want[target]; ok {
					require.Equal(t, []*Edge{{From: "caller", To: target, Kind: EdgeCalls, FilePath: "a.go", Line: 10, Origin: origin}}, rows)
				} else {
					require.Empty(t, rows, target)
				}
			}
			require.Equal(t, len(tc.want), g.EdgeCount())
			require.NoError(t, g.VerifyEdgeIdentities())
		})
	}
}
