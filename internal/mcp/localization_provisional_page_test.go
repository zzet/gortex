package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func provisionalTestDigest(t *testing.T, rows ...localizationDigestRow) *localizationEvidenceDigest {
	t.Helper()
	digest := &localizationEvidenceDigest{Evidence: rows}
	rebuildLocalizationDigestSkeleton(digest)
	refreshLocalizationDigestResponses(digest, "loading a stored record leaves the path empty", nil)
	return digest
}

func provisionalTestRow() localizationDigestRow {
	return localizationDigestRow{
		Rank: 1, ID: "repo/storage/disk.go::DiskStorage.Load", Name: "DiskStorage.Load",
		QualName: "storage.DiskStorage.Load", Kind: "method", File: "storage/disk.go", Line: 42,
	}
}

// A session can stop at any turn. Every state that has ranked something must
// hand back a page, or a caller that runs out of steps ends with nothing.
func TestEveryLocalizationStateHoldingEvidenceHandsBackAPage(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	states := []string{
		localizationStateNeedsExactRead, localizationStateExactReadInFlight,
		localizationStateNeedsRefinement, localizationStateRefineInFlight,
		localizationStateNeedsRecovery, localizationStateRecoveryInFlight,
		localizationStateLocalized,
	}
	for _, state := range states {
		enriched := localizationCompletionWithDigest(localizationCompletion{State: state}, digest)
		require.NotEmpty(t, enriched.FinalResponse, "state %q must carry a page", state)
		require.Contains(t, enriched.FinalResponse, "storage/disk.go", "state %q must name the located file", state)
		require.Contains(t, enriched.FinalResponse, "DiskStorage.Load", "state %q must name the located symbol", state)
	}
}

// The page a caller cannot trust yet must be impossible to mistake for the one
// it can. Both the heading and the closing line carry that distinction.
func TestAProvisionalPageDoesNotClaimToBeAnAnswer(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	page := localizationCompletionWithDigest(
		localizationCompletion{State: localizationStateNeedsRecovery}, digest).FinalResponse

	require.Contains(t, page, localizationProvisionalHeading)
	require.NotContains(t, page, localizationAnswerReadyDirective,
		"an unproven page must not carry the directive that ends a session")
	require.NotContains(t, strings.ToLower(page), "localization for this task is complete")
	require.Contains(t, strings.ToLower(page), "unconfirmed")
}

// The page has to survive the real envelope budget or it never reaches anyone.
// Pin its fixed cost: everything that is not a located identity stays small
// enough to fit the slack a full page leaves behind.
func TestAProvisionalPageCostsLittleBeyondItsIdentities(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	overhead := len(digest.provisionalResponse) -
		len(provisionalTestRow().File) - len(provisionalTestRow().ID)
	// The leading answer-shape directive is fixed cost on every page: it is the
	// only instruction placed where it is read before the answer is formed.
	require.Less(t, overhead, 400,
		"fixed page cost must stay a small fraction of the %d-byte envelope budget",
		exploreDefaultBudgetTokens*localizationEnvelopeBytesPerToken)
}

// The proven page is what it always was: adding a provisional twin must not
// change one byte of the answer a completed localization emits.
func TestAProvenPageKeepsItsOwnWording(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	page := localizationCompletionWithDigest(
		localizationCompletion{State: localizationStateAnswerReady}, digest).FinalResponse

	require.Equal(t, digest.finalResponse, page)
	require.True(t, strings.HasPrefix(page, localizationAnswerHeading+"\n"))
	require.Contains(t, page, localizationAnswerReadyDirective)
	require.NotContains(t, page, localizationProvisionalHeading)
}

// A response that was never a localization request carries no candidate page:
// boilerplate with no question behind it is payload the caller cannot use.
func TestAResponseOutsideLocalizationCarriesNoPage(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	enriched := localizationCompletionWithDigest(
		localizationCompletion{State: localizationStateInactive}, digest)
	require.Empty(t, enriched.FinalResponse)
}

func TestAStateWithNoRankedEvidenceCarriesNoPage(t *testing.T) {
	empty := provisionalTestDigest(t)
	enriched := localizationCompletionWithDigest(
		localizationCompletion{State: localizationStateNeedsRecovery}, empty)
	require.Empty(t, enriched.FinalResponse)

	missing := localizationCompletionWithDigest(
		localizationCompletion{State: localizationStateNeedsRecovery}, nil)
	require.Empty(t, missing.FinalResponse)
}

// The provisional page is new payload on responses that previously carried
// none, so it trims its own tail rather than pushing an envelope over budget.
func TestAProvisionalPageStaysInsideTheAnswerByteCap(t *testing.T) {
	rows := make([]localizationDigestRow, 0, localizationReplayEvidenceLimit)
	long := strings.Repeat("deeply/nested/package/", 12)
	for index := 0; index < localizationReplayEvidenceLimit; index++ {
		rows = append(rows, localizationDigestRow{
			Rank: index + 1,
			ID:   fmt.Sprintf("repo/%sservice%d.go::Handler.Process%d", long, index, index),
			Name: fmt.Sprintf("Handler.Process%d", index),
			File: fmt.Sprintf("%sservice%d.go", long, index),
			Line: index + 1,
		})
	}
	digest := provisionalTestDigest(t, rows...)
	require.LessOrEqual(t, len(digest.provisionalResponse), localizationFinalResponseMaxBytes)
	require.NotEmpty(t, digest.provisionalResponse)
}

// Both pages are rebuilt from one row set, so a row shed for budget cannot
// survive in the page the caller is invited to answer from.
func TestBothPagesAreRebuiltFromTheSameRows(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow(), localizationDigestRow{
		Rank: 2, ID: "repo/storage/cache.go::Cache.Warm", Name: "Cache.Warm",
		QualName: "storage.Cache.Warm", Kind: "method", File: "storage/cache.go", Line: 8,
	})
	require.Contains(t, digest.provisionalResponse, "storage/cache.go")

	digest.Evidence = digest.Evidence[:1]
	rebuildLocalizationDigestSkeleton(digest)
	refreshLocalizationDigestResponses(digest, "loading a stored record leaves the path empty", nil)
	require.NotContains(t, digest.provisionalResponse, "storage/cache.go")
	require.NotContains(t, digest.finalResponse, "storage/cache.go")
}
