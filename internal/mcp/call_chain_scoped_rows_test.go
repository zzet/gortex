package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A repo-scoped call chain reads only the scoped repository's adjacency: a
// wide seed through the one-statement admission projection, a narrow seed hop
// by hop. Other repositories sharing its names, paths and bare placeholders
// change neither the chain nor what was read to build it.
func TestGetCallChainScopedNeverScansAndRowsStayFlat(t *testing.T) {
	srv, store, counter := scopedRowServer(t)
	require.Implements(t, (*graph.FilteredBFSAdmissionReader)(nil), counter)
	require.Implements(t, (*graph.ReadContextBinder)(nil), counter)
	chain := func() string {
		wide := scopedRowCall(t, srv, srv.handleGetCallChain, t.Context(), "get_call_chain", map[string]any{"id": scopedRowID("a", scopedRowHandler), "repo": "a", "format": "json"})
		narrow := scopedRowCall(t, srv, srv.handleGetCallChain, t.Context(), "get_call_chain", map[string]any{"id": scopedRowID("a", scopedRowMain), "repo": "a", "format": "json"})
		return "[" + wide + "," + narrow + "]"
	}
	requireScopedReadsStayFlat(t, counter, func() { addUnrelatedScopedRowRepos(t, store, 5) }, chain)
	rows := counter.rows()
	require.Positive(t, rows["BFSAdmissionFilteredContext"], "the wide seed reads its neighborhood in one statement")
	require.Positive(t, rows["GetOutEdges"], "the narrow seed walks hop by hop")
}
