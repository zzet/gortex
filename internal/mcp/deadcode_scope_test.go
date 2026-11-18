package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

type deadCodeLookupCountingReader struct {
	*outlineCountingReader
	scalarLookups *int
}

func (r *deadCodeLookupCountingReader) GetNode(id string) *graph.Node {
	(*r.scalarLookups)++
	return r.Store.GetNode(id)
}
func (r *deadCodeLookupCountingReader) BindReadContext(ctx context.Context) graph.Reader {
	return &deadCodeLookupCountingReader{
		outlineCountingReader: &outlineCountingReader{Store: r.WithReadContext(ctx), source: r.counters()},
		scalarLookups:         r.scalarLookups,
	}
}

func TestDeadCodeHandlerOwnershipFilterNeedsNoScalarLookup(t *testing.T) {
	s, store := outlineFixture(t)
	store.AddBatch([]*graph.Node{
		{ID: "a-private", Name: "privatea", Kind: graph.KindFunction, FilePath: "a/private.go", Language: "go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "b-private", Name: "privateb", Kind: graph.KindFunction, FilePath: "b/private.go", Language: "go", RepoPrefix: "b", WorkspaceID: "ws"},
		{ID: "unowned-private", Name: "privateunowned", Kind: graph.KindFunction, FilePath: "shared/private.go", Language: "go", WorkspaceID: "ws"},
	}, nil)
	for _, tc := range []struct {
		name, workspace string
		ceiling, narrow map[string]bool
		unbound         bool
	}{
		{name: "session ceiling includes unowned", ceiling: map[string]bool{"a": true}},
		{name: "workspace ceiling", workspace: "ws", ceiling: map[string]bool{"a": true, "b": true}},
		{name: "explicit narrow excludes unowned", workspace: "ws", ceiling: map[string]bool{"a": true, "b": true}, narrow: map[string]bool{"a": true}},
		{name: "disjoint narrow", ceiling: map[string]bool{"a": true}, narrow: map[string]bool{"b": true}},
		{name: "workspace only preserves fallback", workspace: "ws"},
		{name: "unbound explicit repo", narrow: map[string]bool{"a": true}, unbound: true},
		{name: "unbound", unbound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if !tc.unbound {
				ctx = outlineBoundContext(s, tc.name, tc.workspace, tc.ceiling)
			}
			if tc.narrow != nil {
				ctx = withRepoAllow(ctx, tc.narrow)
			}
			// Independent old algorithm: whole reader, followed by the exact
			// original node lookup/session/narrow filter.
			want := analysis.FindDeadCode(legacyOutlineReader{store}, s.getProcesses(), nil)
			if s.scopeFiltersActive(ctx) {
				kept := make([]analysis.DeadCodeEntry, 0, len(want))
				for _, entry := range want {
					if s.analyzeNodeVisible(ctx, store.GetNode(entry.ID)) {
						kept = append(kept, entry)
					}
				}
				want = kept
			}
			scalarLookups := 0
			counter := &outlineCountingReader{Store: store}
			reader := &deadCodeLookupCountingReader{outlineCountingReader: counter, scalarLookups: &scalarLookups}
			ctx = withRequestView(ctx, &requestView{reader: reader})
			result, err := s.handleFindDeadCode(ctx, makeReq("find_dead_code", map[string]any{"format": "json"}))
			require.NoError(t, err)
			payload := resultJSON(t, result)
			gotJSON, err := json.Marshal(payload["dead_code"])
			require.NoError(t, err)
			wantJSON, err := json.Marshal(want)
			require.NoError(t, err)
			require.JSONEq(t, string(wantJSON), string(gotJSON))
			require.Zero(t, scalarLookups, "ownership metadata must replace per-result GetNode")
			require.Zero(t, counter.allNodes, "read-context wrapping must preserve server-side capabilities")
		})
	}
}
