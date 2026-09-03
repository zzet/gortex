package semantic

import (
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// checkoutCensusStore is a persisting store whose language census is fixed, so
// a test can decide which languages a pass sees without indexing a tree. The
// SQLite store underneath is what makes the enrichment markers real — the
// memory graph does not persist them at all.
type checkoutCensusStore struct {
	*store_sqlite.Store
	rows []graph.RepoLanguageFileCount
}

func (s *checkoutCensusStore) RepoLanguageFileCounts([]string) []graph.RepoLanguageFileCount {
	return append([]graph.RepoLanguageFileCount(nil), s.rows...)
}

// rootRecordingProvider records the roots it was asked to enrich, which is how
// a test tells two checkouts of one repo prefix apart: the prefix is shared,
// the root is not.
type rootRecordingProvider struct {
	mu    sync.Mutex
	name  string
	langs []string
	roots []string
}

func (p *rootRecordingProvider) Name() string        { return p.name }
func (p *rootRecordingProvider) Languages() []string { return p.langs }
func (p *rootRecordingProvider) Available() bool     { return true }
func (p *rootRecordingProvider) Close() error        { return nil }

func (p *rootRecordingProvider) Enrich(_ graph.Store, repoRoot string) (*EnrichResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roots = append(p.roots, repoRoot)
	return &EnrichResult{Provider: p.name, Language: p.langs[0], CoveragePercent: 90}, nil
}

func (p *rootRecordingProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}

func (p *rootRecordingProvider) enriched() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.roots...)
}

const checkoutRepo = "family"

// checkoutManager builds a manager whose only provider records roots, with the
// workspace cap the test wants.
func checkoutManager(t *testing.T, cap int) (*Manager, *rootRecordingProvider) {
	t.Helper()
	provider := &rootRecordingProvider{name: "test-go", langs: []string{"go"}}
	mgr := NewManager(Config{
		Enabled:                  true,
		CheckoutLSPMaxWorkspaces: cap,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(provider)
	return mgr, provider
}

// checkoutStore opens a persisting store that reports one Go file per repo
// prefix, which is the evidence the language census needs.
func checkoutStore(t *testing.T) *checkoutCensusStore {
	t.Helper()
	return &checkoutCensusStore{
		Store: newMarkerStore(t),
		rows: []graph.RepoLanguageFileCount{
			{RepoPrefix: checkoutRepo, FilePath: "src/main.go", Language: "go", Count: 40},
		},
	}
}

// markerFor reads one enrichment marker row by its stored key.
func markerFor(t *testing.T, g graph.EnrichmentStateStore, provider string) (graph.EnrichmentState, bool) {
	t.Helper()
	state, found, err := g.GetEnrichmentState(checkoutRepo, provider)
	require.NoError(t, err)
	return state, found
}

// TestEnrichCheckoutKeepsTwoCheckoutsOfOneFamilyApart is the collapse this
// scoping exists to stop. Both checkouts share the repo prefix, so an unscoped
// marker would let the first one's completion answer for the second — and,
// worse, overwrite the primary's. Each pass must enrich its own root and leave
// exactly its own marker.
func TestEnrichCheckoutKeepsTwoCheckoutsOfOneFamilyApart(t *testing.T) {
	mgr, provider := checkoutManager(t, 4)
	g := checkoutStore(t)

	// The primary's base-corpus enrichment, in the legacy key shape.
	require.NoError(t, g.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: checkoutRepo, Provider: "test-go", IndexedSHA: "primary-head",
	}))

	first, err := mgr.EnrichCheckout(g, CheckoutEnrichRequest{
		RepoPrefix: checkoutRepo, CheckoutID: "checkout-a",
		Root: "/family/a", Fingerprint: "fingerprint-a",
	})
	require.NoError(t, err)
	second, err := mgr.EnrichCheckout(g, CheckoutEnrichRequest{
		RepoPrefix: checkoutRepo, CheckoutID: "checkout-b",
		Root: "/family/b", Fingerprint: "fingerprint-b",
	})
	require.NoError(t, err)

	for name, report := range map[string]CheckoutEnrichReport{"first": first, "second": second} {
		if !reflect.DeepEqual(report.Ran, []string{"go"}) {
			t.Errorf("%s pass ran %v, want [go]", name, report.Ran)
		}
		if len(report.Starved) != 0 || report.Partial || report.Disabled {
			t.Errorf("%s pass = %+v, want a clean full pass", name, report)
		}
	}

	// Each pass read its own working copy, not the other's and not the
	// primary's.
	if got, want := provider.enriched(), []string{"/family/a", "/family/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("enriched roots = %v, want %v", got, want)
	}

	// Three markers, one per checkout plus the primary's untouched original.
	primary, found := markerFor(t, g, "test-go")
	if !found || primary.IndexedSHA != "primary-head" {
		t.Errorf("the base-corpus marker is %q (found=%v), want it untouched at primary-head",
			primary.IndexedSHA, found)
	}
	for checkout, want := range map[string]string{"checkout-a": "fingerprint-a", "checkout-b": "fingerprint-b"} {
		state, found := markerFor(t, g, enrichMarkerProvider("test-go", checkout))
		if !found || state.IndexedSHA != want {
			t.Errorf("marker for %s is %q (found=%v), want %q", checkout, state.IndexedSHA, found, want)
		}
	}
}

// TestEnrichCheckoutNeverSkipsOnItsOwnMarker pins why the scoped marker is a
// record and not a gate: every working-tree build mints a fresh payload, so a
// marker an earlier generation left describes edges this one does not carry.
func TestEnrichCheckoutNeverSkipsOnItsOwnMarker(t *testing.T) {
	mgr, provider := checkoutManager(t, 4)
	g := checkoutStore(t)

	req := CheckoutEnrichRequest{
		RepoPrefix: checkoutRepo, CheckoutID: "checkout-a",
		Root: "/family/a", Fingerprint: "unchanged",
	}
	if _, err := mgr.EnrichCheckout(g, req); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if _, err := mgr.EnrichCheckout(g, req); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := provider.enriched(); len(got) != 2 {
		t.Errorf("the marker skipped a checkout pass: enriched %v, want two passes", got)
	}
}

// TestEnrichCheckoutStarvesWithoutFailing is the cap-starved case: the second
// checkout's language cannot be admitted while the first holds the only slot,
// and the pass says so instead of erroring or waiting.
func TestEnrichCheckoutStarvesWithoutFailing(t *testing.T) {
	mgr, provider := checkoutManager(t, 1)
	g := checkoutStore(t)

	// Hold the only slot for the duration of the second pass, the way a
	// concurrent build over another checkout would.
	release, ok := mgr.CheckoutWorkspaces().Acquire("go", "/family/a")
	if !ok {
		t.Fatal("the cap refused the first checkout")
	}
	defer release()

	report, err := mgr.EnrichCheckout(g, CheckoutEnrichRequest{
		RepoPrefix: checkoutRepo, CheckoutID: "checkout-b",
		Root: "/family/b", Fingerprint: "fingerprint-b",
	})
	require.NoError(t, err)

	if len(report.Ran) != 0 {
		t.Errorf("a starved pass ran %v, want nothing", report.Ran)
	}
	if !reflect.DeepEqual(report.Starved, []string{"go"}) {
		t.Errorf("starved = %v, want [go]", report.Starved)
	}
	if report.Reason == "" {
		t.Error("a starved pass reported no reason")
	}
	if got := provider.enriched(); len(got) != 0 {
		t.Errorf("a starved pass still enriched %v", got)
	}
	if _, found := markerFor(t, g, enrichMarkerProvider("test-go", "checkout-b")); found {
		t.Error("a starved pass left a completion marker")
	}
}

// TestEnrichCheckoutOffByConfig pins the switch: the stage is off, which is a
// different answer from being unable to run, because waiting cannot fix it.
func TestEnrichCheckoutOffByConfig(t *testing.T) {
	mgr, provider := checkoutManager(t, 4)
	mgr.config.CheckoutLSP = "off"
	g := checkoutStore(t)

	report, err := mgr.EnrichCheckout(g, CheckoutEnrichRequest{
		RepoPrefix: checkoutRepo, CheckoutID: "checkout-a", Root: "/family/a",
	})
	require.NoError(t, err)
	if !report.Disabled || len(report.Ran) != 0 {
		t.Errorf("report = %+v, want a disabled pass that ran nothing", report)
	}
	if got := provider.enriched(); len(got) != 0 {
		t.Errorf("a switched-off stage still enriched %v", got)
	}
}

// TestBaseCorpusMarkersAreUnchanged is the golden over the existing behaviour:
// an unscoped pass writes the bare provider key, and the gate that reads it
// still skips a clean repo at the same sha and still refuses to record over a
// dirty tree.
func TestBaseCorpusMarkersAreUnchanged(t *testing.T) {
	mgr, ran := markerManager(t)
	g := newMarkerStore(t)
	roots := markerRoots(t)

	// A clean pass records under the bare provider name.
	_, _, err := mgr.EnrichAll(g, roots, EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "head-1"},
	}})
	require.NoError(t, err)
	require.True(t, *ran)
	state, found, err := g.GetEnrichmentState(markerRepo, "test-go")
	require.NoError(t, err)
	if !found || state.IndexedSHA != "head-1" {
		t.Fatalf("base marker = %q (found=%v), want head-1", state.IndexedSHA, found)
	}

	// The same sha on a clean tree skips the provider entirely.
	*ran = false
	_, _, err = mgr.EnrichAll(g, roots, EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "head-1"},
	}})
	require.NoError(t, err)
	if *ran {
		t.Error("the base-corpus gate stopped skipping a clean repo at a recorded sha")
	}

	// A dirty tree neither skips nor records.
	*ran = false
	_, _, err = mgr.EnrichAll(g, roots, EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "head-2", Dirty: true},
	}})
	require.NoError(t, err)
	require.True(t, *ran)
	state, _, err = g.GetEnrichmentState(markerRepo, "test-go")
	require.NoError(t, err)
	if state.IndexedSHA != "head-1" {
		t.Errorf("a dirty base pass recorded %q, want the marker left at head-1", state.IndexedSHA)
	}
}

// TestEnrichAllLanguageFilter pins the narrowing EnrichCheckout hands the cap's
// verdict down through: a provider whose language the caller did not admit
// stays unrun even though the graph holds nodes for it.
func TestEnrichAllLanguageFilter(t *testing.T) {
	goProvider := &rootRecordingProvider{name: "test-go", langs: []string{"go"}}
	tsProvider := &rootRecordingProvider{name: "test-ts", langs: []string{"typescript"}}
	mgr := NewManager(Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
			{Name: "test-ts", Languages: []string{"typescript"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(goProvider)
	mgr.RegisterProvider(tsProvider)

	g := &checkoutCensusStore{
		Store: newMarkerStore(t),
		rows: []graph.RepoLanguageFileCount{
			{RepoPrefix: checkoutRepo, FilePath: "src/main.go", Language: "go", Count: 40},
			{RepoPrefix: checkoutRepo, FilePath: "src/app.ts", Language: "typescript", Count: 40},
		},
	}

	results, _, err := mgr.EnrichAll(g, map[string]string{checkoutRepo: "/family/a"},
		EnrichOptions{Languages: []string{"go"}})
	require.NoError(t, err)

	languages := enrichedLanguages(results)
	sort.Strings(languages)
	if !reflect.DeepEqual(languages, []string{"go"}) {
		t.Errorf("enriched languages = %v, want [go]", languages)
	}
	if got := tsProvider.enriched(); len(got) != 0 {
		t.Errorf("a language the caller did not admit was enriched: %v", got)
	}
}
