package mcp

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

// PathIndexability returns the zero PathSkip both for "indexable" and for "this
// repo cannot answer", so counting the second as a vote let one non-answering
// repo bail the whole verdict to "no reason known".
func TestUnanimousPathSkip(t *testing.T) {
	var (
		indexable = indexer.PathSkip{}
		byRule    = indexer.PathSkip{Skipped: true, ByRule: true}
		unclaimed = indexer.PathSkip{Skipped: true}

		answered = func(s indexer.PathSkip) pathSkipVote { return pathSkipVote{Skip: s, Answered: true} }
		abstain  = pathSkipVote{}
	)

	for _, tc := range []struct {
		name     string
		votes    []pathSkipVote
		want     indexer.PathSkip
		wantOK   bool
		scenario string
	}{{
		name:     "one repo, excluded by rule",
		votes:    []pathSkipVote{answered(byRule)},
		want:     byRule,
		wantOK:   true,
		scenario: "the ordinary single-answer case",
	}, {
		name:     "every answering repo agrees, one abstains",
		votes:    []pathSkipVote{answered(byRule), abstain},
		want:     byRule,
		wantOK:   true,
		scenario: "repoB has no stored root yet; its silence must not veto repoA",
	}, {
		name:     "abstention first, then a real vote",
		votes:    []pathSkipVote{abstain, answered(byRule)},
		want:     byRule,
		wantOK:   true,
		scenario: "iteration order over RepoPrefixes() is unspecified",
	}, {
		name:     "abstentions on both sides of a vote",
		votes:    []pathSkipVote{abstain, answered(unclaimed), abstain},
		want:     unclaimed,
		wantOK:   true,
		scenario: "any number of non-answering repos still leaves one verdict",
	}, {
		name:     "genuine disagreement",
		votes:    []pathSkipVote{answered(byRule), answered(indexable)},
		wantOK:   false,
		scenario: "two repos that both looked and reached opposite verdicts",
	}, {
		name:     "disagreement on the REASON alone still bails",
		votes:    []pathSkipVote{answered(byRule), answered(unclaimed)},
		wantOK:   false,
		scenario: "ByRule drives the rendered message, so a split on it is a split",
	}, {
		name:     "nobody could answer",
		votes:    []pathSkipVote{abstain, abstain},
		wantOK:   false,
		scenario: "no evidence at all leaves enforcement on",
	}, {
		name:     "no repos at all",
		votes:    nil,
		wantOK:   false,
		scenario: "an empty workspace has nothing to say",
	}, {
		name:     "unanimous indexable",
		votes:    []pathSkipVote{answered(indexable), answered(indexable)},
		want:     indexable,
		wantOK:   true,
		scenario: "agreeing that the walk WOULD hold it is a verdict too",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := unanimousPathSkip(tc.votes)
			if ok != tc.wantOK {
				t.Fatalf("unanimousPathSkip() ok = %v, want %v (%s)", ok, tc.wantOK, tc.scenario)
			}
			if ok && got != tc.want {
				t.Errorf("unanimousPathSkip() = %+v, want %+v (%s)", got, tc.want, tc.scenario)
			}
		})
	}
}

// The two wire flags differ in width on purpose: Excluded is the narrow "a RULE
// is the reason", Unindexable the full verdict. Wiring off Excluded alone
// under-silences.
func TestSkipStateFlagWidths(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip indexer.PathSkip
		want fileNotIndexedState
	}{
		{"indexable", indexer.PathSkip{}, fileNotIndexedState{}},
		{"excluded by rule", indexer.PathSkip{Skipped: true, ByRule: true}, fileNotIndexedState{Unindexable: true, Excluded: true}},
		{"unindexable, no rule", indexer.PathSkip{Skipped: true}, fileNotIndexedState{Unindexable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipState(tc.skip); got != tc.want {
				t.Errorf("skipState(%+v) = %+v, want %+v", tc.skip, got, tc.want)
			}
		})
	}
}

// Same distinction as the fold. The two render identically today, so this pins
// the intent before they drift.
func TestPathIndexability_SingleRepoCannotAnswer(t *testing.T) {
	idx := indexer.New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	srv := &Server{indexer: idx}

	if got := srv.pathIndexability("node_modules/dpack/lib/Block.js"); got != (fileNotIndexedState{}) {
		t.Errorf("an indexer with no stored root must yield no reason, got %+v", got)
	}
}
