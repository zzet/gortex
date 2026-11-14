package tstypes

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Every way a stored point can be refused is named, so the discard is
// logged with its cause instead of reading as a resume that never happened.
func TestResumeRefusalNamesEachCause(t *testing.T) {
	const mayNotResume = "this pass may not resume (dirty tree, forced re-enrich, or a re-parse this run the ledger does not cover)"
	scope := semantic.EnrichResumeScope{SHA: "abc", Record: true, Resume: true}
	noResume := scope
	noResume.Resume = false
	prior := graph.EnrichmentResumePoint{IndexedSHA: "abc", Version: "v1", Phase: int(stagedMetas)}
	otherSHA := prior
	otherSHA.IndexedSHA = "def"
	aliases := prior
	aliases.Phase = int(stagedAliases)

	cases := []struct {
		name    string
		readErr error
		scope   semantic.EnrichResumeScope
		prior   graph.EnrichmentResumePoint
		version string
		want    string
	}{
		{"resumable", nil, scope, prior, "v1", ""},
		{"read failed", errors.New("boom"), scope, prior, "v1", "read failed"},
		{"pass may not resume", nil, noResume, prior, "v1", mayNotResume},
		{"unidentified build", nil, scope, prior, "", "this build has no identity"},
		{"other revision", nil, scope, otherSHA, "v1", "recorded at another revision"},
		{"other build", nil, scope, prior, "v2", "recorded by another build"},
		{"unresumable phase", nil, scope, aliases, "v1", "recorded in a phase that is not resumable"},
		// When several causes hold, the pass outranks the point, and a
		// missing identity outranks what the point recorded.
		{"pass may not resume at another revision", nil, noResume, otherSHA, "v2", mayNotResume},
		{"unidentified build at another revision", nil, scope, otherSHA, "", "this build has no identity"},
		{"other revision by another build", nil, scope, otherSHA, "v2", "recorded at another revision"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resumeRefusal(c.readErr, c.scope, c.prior, c.version))
		})
	}
}
