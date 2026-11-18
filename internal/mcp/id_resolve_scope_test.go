package mcp

import (
	"context"
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A bare-name target (get_callers, why) from a bound session lists only the
// definitions the session may see and reads only the session's repositories,
// plus the unowned bucket every scope sees. The every-repository read handed
// other workspaces' ids and file paths to the ambiguity answer, and paid for
// every repository's same-named symbols. Unbound sessions keep the
// every-repository read and answer.
func TestBareNameTargetStaysInTheSessionScope(t *testing.T) {
	f := newShorthandScopeFixture(t)
	cases := []struct {
		name, cwd string
		// candidates is how many definitions each name matches in scope.
		candidates map[string]int
		// repos are the repositories read by name; nil means the
		// every-repository read.
		repos []string
	}{
		{"single repository", f.repoA, map[string]int{"Needle": 1, "Twin": 2, "Shared": 1, "Absent": 0}, []string{"", "repo-a"}},
		{"two-repository ceiling", f.parent, map[string]int{"Needle": 2, "Twin": 3, "Shared": 1, "Absent": 0}, []string{"", "repo-a", "repo-b"}},
		{"unbound", "", map[string]int{"Needle": 42, "Twin": 43, "Shared": 41, "Absent": 0}, nil},
	}
	for _, tc := range cases {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%v", tc.name, wrapped), func(t *testing.T) {
				ctx := context.Background()
				if tc.cwd != "" {
					ctx = sessionCtx("s-bare-name", tc.cwd)
				}
				// every has no repository-scoped finder, like an overlay view.
				every := f.server(shorthandLegacyStore{f.g, &shorthandNameReads{}}, wrapped)
				reads := &shorthandNameReads{}
				scoped := f.server(shorthandScopedStore{shorthandLegacyStore{f.g, reads}}, wrapped)
				for name, count := range tc.candidates {
					// The session's share of the every-repository answer.
					var want []string
					for _, id := range every.resolveNameToIDs(context.Background(), name) {
						if every.nodeInSessionScope(ctx, f.g.GetNode(id)) {
							want = append(want, id)
						}
					}
					require.Lenf(t, want, count, "%s", name)
					assert.Equalf(t, want, every.resolveNameToIDs(ctx, name), "%s: a reader without the finder", name)

					reads.reset()
					assert.Equalf(t, want, scoped.resolveNameToIDs(ctx, name), "%s", name)
					if tc.repos == nil {
						assert.Equalf(t, 1, reads.storeWide, "%s: an unbound session keeps the every-repository read", name)
						assert.Emptyf(t, reads.repos, "%s", name)
					} else {
						assert.Zerof(t, reads.storeWide, "%s: a bound session read every repository's nodes of that name", name)
						assert.Equalf(t, tc.repos, reads.repos, "%s", name)
					}
					if count < 2 {
						continue
					}
					// What the agent is shown: the same in-scope list, nothing else.
					tools := map[string]func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error){
						"get_callers": scoped.handleGetCallers,
						"why":         scoped.handleWhy,
					}
					for tool, handle := range tools {
						res, err := handle(ctx, makeReq(tool, map[string]any{"id": name}))
						require.NoError(t, err)
						out := unmarshalResult(t, res)
						require.Equalf(t, true, out["ambiguous"], "%s %s", tool, name)
						listed, ok := out["candidates"].([]any)
						require.Truef(t, ok, "%s %s: candidates are a list, got %T", tool, name, out["candidates"])
						ids := make([]string, 0, len(listed))
						for _, c := range listed {
							ids = append(ids, fmt.Sprint(c.(map[string]any)["id"]))
						}
						assert.Equalf(t, want, ids, "%s %s lists only the session's definitions", tool, name)
					}
				}
			})
		}
	}
}
