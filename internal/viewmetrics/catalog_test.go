package viewmetrics_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// The catalog is the allow-list: a series absent from it records NOTHING, and
// a label value absent from its vocabulary collapses to "other". Both failures
// are silent — Count has no error return and no caller checks anything — so a
// counter that was declared wrongly looks exactly like a counter whose call
// site never fired.
//
// These tests are the declaration's own guard. They record one sample per
// declared value through the package's public door and assert the exact
// flattened key, which is both halves at once: a series that is not declared
// yields an empty snapshot, and a value outside the vocabulary yields an
// {…=other} key instead of the one asserted.

// committedBaseSeries is the vocabulary the sustained-I/O measurement cites.
// Each row is a series, one of its declared label values, and the flattened
// key that value must produce. The keys are spelled out rather than composed
// because the flattened key is what a status payload and a ledger row carry:
// renaming a label while keeping the constants compiling has to fail here.
var committedBaseSeries = []struct {
	name   string
	labels []string
	key    string
}{
	{viewmetrics.DedicatedBasePublishTotal, []string{viewmetrics.DedicatedBaseRoot},
		"views_dedicated_base_publish_total{shape=root}"},
	{viewmetrics.DedicatedBasePublishTotal, []string{viewmetrics.DedicatedBaseDelta},
		"views_dedicated_base_publish_total{shape=delta}"},
	{viewmetrics.DedicatedBaseClaimTotal, []string{viewmetrics.DedicatedBaseBuilt},
		"views_dedicated_base_claim_total{outcome=built}"},
	{viewmetrics.DedicatedBaseClaimTotal, []string{viewmetrics.DedicatedBaseCoalesced},
		"views_dedicated_base_claim_total{outcome=coalesced}"},
	{viewmetrics.DedicatedBaseClaimTotal, []string{viewmetrics.DedicatedBaseReused},
		"views_dedicated_base_claim_total{outcome=reused}"},
	{viewmetrics.DedicatedBaseClosureTruncatedTotal, nil,
		"views_dedicated_base_closure_truncated_total"},
	{viewmetrics.DedicatedBasePublicationTotal, []string{viewmetrics.PublicationPublished},
		"views_dedicated_base_publication_total{outcome=published}"},
	{viewmetrics.DedicatedBasePublicationTotal, []string{viewmetrics.PublicationCoalesced},
		"views_dedicated_base_publication_total{outcome=coalesced}"},
	{viewmetrics.DedicatedBasePublicationTotal, []string{viewmetrics.PublicationReadopted},
		"views_dedicated_base_publication_total{outcome=readopted}"},
	{viewmetrics.DedicatedBasePublicationTotal, []string{viewmetrics.PublicationSkipped},
		"views_dedicated_base_publication_total{outcome=skipped}"},
	{viewmetrics.DedicatedBasePublicationTotal, []string{viewmetrics.PublicationFailed},
		"views_dedicated_base_publication_total{outcome=failed}"},
	{viewmetrics.DedicatedBaseAdvanceTotal, []string{viewmetrics.AdvanceDispatched},
		"views_dedicated_base_advance_total{outcome=dispatched}"},
	{viewmetrics.DedicatedBaseAdvanceTotal, []string{viewmetrics.AdvanceRepeat},
		"views_dedicated_base_advance_total{outcome=repeat}"},
	{viewmetrics.DedicatedBaseAdvanceTotal, []string{viewmetrics.AdvanceRefused},
		"views_dedicated_base_advance_total{outcome=refused}"},
	{viewmetrics.DedicatedBaseDrainTotal, []string{viewmetrics.DrainImmediate},
		"views_dedicated_base_drain_total{outcome=immediate}"},
	{viewmetrics.DedicatedBaseDrainTotal, []string{viewmetrics.DrainWaited},
		"views_dedicated_base_drain_total{outcome=waited}"},
	{viewmetrics.DependentRecompositionTotal, nil,
		"views_dependent_recomposition_total"},
}

// TestCommittedBaseSeriesAreDeclaredWithTheirWholeVocabulary records one
// sample per declared value on a private registry and asserts the key it
// lands under.
//
// Revert-red: remove any one of the six series from the catalog map and its
// rows fail with an empty snapshot; remove one value from a vocabulary and its
// row fails with an {…=other} key.
func TestCommittedBaseSeriesAreDeclaredWithTheirWholeVocabulary(t *testing.T) {
	for _, series := range committedBaseSeries {
		t.Run(series.key, func(t *testing.T) {
			r := viewmetrics.New()
			r.Count(series.name, series.labels...)
			counters := r.Snapshot().Counters
			require.Len(t, counters, 1,
				"%s recorded %d series for one sample — an undeclared series records nothing",
				series.name, len(counters))
			require.Equal(t, int64(1), counters[series.key],
				"the sample landed under %v, not %q", counters, series.key)
		})
	}
}

// TestAnUndeclaredCommittedBaseLabelCollapses is the clamp half, and the
// reason the vocabularies above have to be complete: a value the catalog does
// not know does not mint a series, it lands in the "other" bucket. A counter
// whose call site passes a value nobody declared is therefore invisible as
// itself — which is what a test that only asserted "something was recorded"
// would miss.
func TestAnUndeclaredCommittedBaseLabelCollapses(t *testing.T) {
	r := viewmetrics.New()
	r.Count(viewmetrics.DedicatedBasePublishTotal, "squashed")
	require.Equal(t, map[string]int64{
		"views_dedicated_base_publish_total{shape=other}": 1,
	}, r.Snapshot().Counters)
}

// TestCommittedBaseSeriesRefuseTheWrongShape pins the other silent drop: the
// catalog declares a kind and an arity, and a call that gets either wrong
// records nothing at all rather than failing loudly. Both mistakes are one
// keystroke away at a call site — a gauge where a counter belongs, a missing
// label — so the refusal is worth stating.
func TestCommittedBaseSeriesRefuseTheWrongShape(t *testing.T) {
	t.Run("a counter is not a gauge", func(t *testing.T) {
		r := viewmetrics.New()
		r.SetGauge(viewmetrics.DedicatedBaseClaimTotal, 3, viewmetrics.DedicatedBaseBuilt)
		require.Empty(t, r.Snapshot().Gauges)
		require.Empty(t, r.Snapshot().Counters)
	})

	t.Run("a labelled series refuses a bare call", func(t *testing.T) {
		r := viewmetrics.New()
		r.Count(viewmetrics.DedicatedBaseAdvanceTotal)
		require.Empty(t, r.Snapshot().Counters)
	})

	t.Run("an unlabelled series refuses a label", func(t *testing.T) {
		r := viewmetrics.New()
		r.Count(viewmetrics.DependentRecompositionTotal, viewmetrics.DedicatedBaseRoot)
		require.Empty(t, r.Snapshot().Counters)
	})
}

// TestCommittedBaseSeriesReachTheFlatStatusPayload is the last hop: the
// daemon's views block is Snapshot.Flat, and a series that does not survive
// that flattening never reaches `gortex daemon status` however correctly it
// was counted. Zero-valued series are dropped by design, so the assertion is
// that a counted one is kept under its own key.
func TestCommittedBaseSeriesReachTheFlatStatusPayload(t *testing.T) {
	r := viewmetrics.New()
	r.Count(viewmetrics.DedicatedBaseClaimTotal, viewmetrics.DedicatedBaseReused)
	r.Add(viewmetrics.DedicatedBasePublishTotal, 4, viewmetrics.DedicatedBaseDelta)
	r.Count(viewmetrics.DependentRecompositionTotal)

	flat := r.Snapshot().Flat()
	require.Equal(t, int64(1), flat["views_dedicated_base_claim_total{outcome=reused}"])
	require.Equal(t, int64(4), flat["views_dedicated_base_publish_total{shape=delta}"])
	require.Equal(t, int64(1), flat["views_dependent_recomposition_total"])
	require.NotContains(t, flat, "views_dedicated_base_publish_total{shape=root}",
		"a series nothing counted rides on every status poll")
}
