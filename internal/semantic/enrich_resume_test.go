package semantic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The resume scope follows the completion marker's gating: a point is
// written only where a marker could be, and honoured only where nothing this
// run invalidated the persisted enrichment.
func TestEnrichResumeScopeFollowsMarkerGating(t *testing.T) {
	cases := []struct {
		name   string
		rs     RepoEnrichState
		env    string
		ok     bool
		record bool
		resume bool
	}{
		{name: "clean tree", rs: RepoEnrichState{SHA: "abc"}, ok: true, record: true, resume: true},
		{name: "no sha", rs: RepoEnrichState{}},
		{name: "checkout scoped", rs: RepoEnrichState{SHA: "abc", CheckoutID: "wt"}},
		{name: "dirty tree", rs: RepoEnrichState{SHA: "abc", Dirty: true}, ok: true},
		{name: "whole repo re-parsed this run", rs: RepoEnrichState{SHA: "abc", Force: true}, ok: true, record: true},
		{name: "re-parsed files are on the ledger", rs: RepoEnrichState{SHA: "abc", Force: true, ReparsedFilesMarked: true}, ok: true, record: true, resume: true},
		{name: "ledger marked on a dirty tree", rs: RepoEnrichState{SHA: "abc", Dirty: true, Force: true, ReparsedFilesMarked: true}, ok: true},
		{name: "ledger marked but forced by env", rs: RepoEnrichState{SHA: "abc", Force: true, ReparsedFilesMarked: true}, env: "1", ok: true, record: true},
		{name: "forced by env", rs: RepoEnrichState{SHA: "abc"}, env: "1", ok: true, record: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", tc.env)
			scope, ok := enrichResumeScope(tc.rs)
			assert.Equal(t, tc.ok, ok)
			if !ok {
				return
			}
			assert.Equal(t, tc.rs.SHA, scope.SHA)
			assert.Equal(t, tc.record, scope.Record)
			assert.Equal(t, tc.resume, scope.Resume)
		})
	}

	_, ok := EnrichResumeFrom(context.Background())
	assert.False(t, ok, "a context without a scope must not resume")
	scope := EnrichResumeScope{SHA: "abc", Record: true, Resume: true}
	got, ok := EnrichResumeFrom(WithEnrichResume(context.Background(), scope))
	assert.True(t, ok)
	assert.Equal(t, scope, got)
}
