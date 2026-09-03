package viewmetrics_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The cardinality guard.
//
// The registry's promise is that its series count is a property of the build
// and not of the workload. That promise rests on one thing: every label value
// a series can hold is enumerated in the catalog, and none of those values is
// an identity. Clamping is tested against the registry in viewmetrics_test.go;
// what is tested here is the vocabularies themselves — because a clamp that
// works perfectly is no help if someone declares a checkout id as an allowed
// value.

// idShaped matches the identities the view lifecycle deals in: uuids, git
// object ids, view fingerprints, generation ids, and anything carrying a path
// separator. A label value matching one of these is a cardinality bug however
// harmless it looks in isolation.
var idShaped = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"uuid", regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-`)},
	{"hex object id or fingerprint", regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)},
	{"bare number", regexp.MustCompile(`^[0-9]+$`)},
}

// TestLabelVocabulariesCarryNoIdentities walks every declared series and
// refuses any value that could name one thing rather than one class.
func TestLabelVocabulariesCarryNoIdentities(t *testing.T) {
	for _, series := range viewmetrics.SeriesNames() {
		for label, values := range viewmetrics.LabelVocabularies(series) {
			if len(values) == 0 {
				t.Errorf("%s: label %q declares no vocabulary, so it is unbounded", series, label)
				continue
			}
			for _, value := range values {
				assertNotIdentity(t, series, label, value)
			}
		}
	}
}

func assertNotIdentity(t *testing.T, series, label, value string) {
	t.Helper()
	switch {
	case value == "":
		t.Errorf("%s{%s}: an empty label value is not a class", series, label)
	case len(value) > 40:
		t.Errorf("%s{%s}: %q is %d bytes; a class name is short", series, label, value, len(value))
	case strings.ContainsAny(value, "/\\ \t\n"):
		t.Errorf("%s{%s}: %q carries a path separator or whitespace", series, label, value)
	}
	for _, shape := range idShaped {
		if shape.pattern.MatchString(value) {
			t.Errorf("%s{%s}: %q is %s shaped", series, label, value, shape.name)
		}
	}
}

// TestEverySeriesIsDeclaredOnce guards the catalog's own shape: a series with
// no name, or a label name reused inside one series, would make two different
// facts share a key.
func TestEverySeriesIsDeclaredOnce(t *testing.T) {
	names := viewmetrics.SeriesNames()
	if len(names) == 0 {
		t.Fatal("the catalog declares no series")
	}
	for _, series := range names {
		if !strings.HasPrefix(series, "views_") {
			t.Errorf("series %q does not carry the subsystem prefix", series)
		}
		seen := map[string]bool{}
		for label := range viewmetrics.LabelVocabularies(series) {
			if seen[label] {
				t.Errorf("%s: label %q is declared twice", series, label)
			}
			seen[label] = true
		}
	}
}

// TestFallbackVocabularyTracksTheViewErrorCodes pins the one vocabulary that
// is a copy rather than an original. graphview cannot be imported from the
// registry — the packages the registry is called from are the ones graphview
// is built under — so the codes are restated there and checked here.
func TestFallbackVocabularyTracksTheViewErrorCodes(t *testing.T) {
	want := graphview.ErrorCodes()
	got := viewmetrics.ViewErrorCodes
	if len(got) != len(want) {
		t.Fatalf("fallback vocabulary has %d codes, graphview has %d:\n got %v\nwant %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fallback vocabulary[%d] = %q, graphview has %q", i, got[i], want[i])
		}
	}
}

// TestCheckoutStateVocabularyTracksTheCatalog pins the transition labels: they
// are the catalog's own state strings, so a state added there without being
// added here would silently collapse into the other bucket.
func TestCheckoutStateVocabularyTracksTheCatalog(t *testing.T) {
	declared := map[string]bool{}
	for _, values := range viewmetrics.LabelVocabularies(viewmetrics.CheckoutTransitionTotal) {
		for _, value := range values {
			declared[value] = true
		}
	}
	for _, state := range []store_sqlite.CheckoutState{
		store_sqlite.CheckoutStateReady,
		store_sqlite.CheckoutStateAvailabilityGrace,
		store_sqlite.CheckoutStateUnavailable,
		store_sqlite.CheckoutStateReconciling,
		store_sqlite.CheckoutStateDemoting,
		store_sqlite.CheckoutStateForgetting,
		store_sqlite.CheckoutStatePrimaryClosureRetiring,
	} {
		if !declared[string(state)] {
			t.Errorf("checkout state %q is not in the transition vocabulary", state)
		}
	}
	if !declared[viewmetrics.StateNone] {
		t.Error("the transition vocabulary has no name for a checkout that does not exist")
	}
}

// TestCapabilityRefusalVocabularyIsTheTwoRefusalCodes pins the refusal labels
// to the two codes a capability evaluation can produce.
func TestCapabilityRefusalVocabularyIsTheTwoRefusalCodes(t *testing.T) {
	declared := map[string]bool{}
	for _, values := range viewmetrics.LabelVocabularies(viewmetrics.CapabilityRefusedTotal) {
		for _, value := range values {
			declared[value] = true
		}
	}
	for _, code := range []string{
		graphview.CodeCapabilityUnavailable,
		graphview.CodeRequiredCapabilityIncomplete,
	} {
		if !declared[code] {
			t.Errorf("capability refusal code %q is not in the vocabulary", code)
		}
	}
}
