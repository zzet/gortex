package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// testCommonDir is the shared git directory every fake family hangs off. It
// has to match the family row's recorded common-dir identity or every pass
// would refuse the inventory.
const testCommonDir = "/repo/.git"

// errHookFailed is what a fake hook returns when a test asks it to fail, so a
// saga stops exactly where a crashed process would have.
var errHookFailed = errors.New("hook refused on purpose")

// The fakes have to keep matching the extension points they stand in for. A
// signature drift shows up here as one compile error rather than as a
// confusing argument mismatch at every wiring site.
var (
	_ CleanupHooks    = (*recordingHooks)(nil)
	_ InventoryFunc   = (*fakeGit)(nil).inventory
	_ PathSamplerFunc = (*fakeGit)(nil).sample
	_ HEADSamplerFunc = (*fakeGit)(nil).head
)

// recordingHooks is the fake layer/graph owner. It records every call in order
// so a test can assert the sequence a saga ran, and can be told to fail a
// bounded number of times to stand in for a process dying mid-phase.
type recordingHooks struct {
	mu          sync.Mutex
	calls       []string
	failPurge   int
	failRelease int
	// onPurge runs inside the purge, which is where the layer owner stops the
	// builder that has been routing for this checkout. The cycle that builder
	// was already running finishes during the stop, so this is the one place a
	// test can put the catalog write that stop is racing.
	onPurge func(checkoutID string)
}

func (h *recordingHooks) PurgeCheckoutLayers(_ context.Context, checkoutID, incarnation string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "purge:"+checkoutID+":"+incarnation)
	if h.onPurge != nil {
		h.onPurge(checkoutID)
	}
	if h.failPurge > 0 {
		h.failPurge--
		return errHookFailed
	}
	return nil
}

func (h *recordingHooks) ReleaseGraph(_ context.Context, graphID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "release:"+graphID)
	if h.failRelease > 0 {
		h.failRelease--
		return errHookFailed
	}
	return nil
}

// snapshot returns the calls recorded so far.
func (h *recordingHooks) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

// countPrefix counts recorded calls starting with prefix.
func (h *recordingHooks) countPrefix(prefix string) int {
	n := 0
	for _, call := range h.snapshot() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// fakeGit stands in for the three gitstate readers. Driving them directly is
// the only way to reach the combinations a real repository will not produce on
// demand — an inventory that fails, a root that is prunable and absent on a
// still-mounted volume, a volume that went away.
type fakeGit struct {
	commonDir string
	records   []gitstate.WorktreeRecord
	err       error
	samples   map[string]gitstate.PathEvidence
	heads     map[string]gitstate.HEADState
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		commonDir: testCommonDir,
		samples:   map[string]gitstate.PathEvidence{},
		heads:     map[string]gitstate.HEADState{},
	}
}

func (f *fakeGit) inventory(context.Context, string) (*gitstate.FamilyInventory, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &gitstate.FamilyInventory{
		CommonDir: f.commonDir,
		GitDir:    f.commonDir,
		Records:   slices.Clone(f.records),
	}, nil
}

func (f *fakeGit) sample(root string) gitstate.PathEvidence { return f.samples[root] }

func (f *fakeGit) head(_ context.Context, dir string) (gitstate.HEADState, error) {
	state, ok := f.heads[dir]
	if !ok {
		return gitstate.HEADState{}, gitstate.ErrHEADUnavailable
	}
	return state, nil
}

// setRecords replaces the whole inventory.
func (f *fakeGit) setRecords(records ...gitstate.WorktreeRecord) { f.records = records }

// presentRecord is a linked worktree git lists and whose root is reachable.
func presentRecord(adminName, path string) gitstate.WorktreeRecord {
	return gitstate.WorktreeRecord{
		Path:           path,
		AdminName:      adminName,
		Branch:         adminName,
		HEADRef:        "refs/heads/" + adminName,
		HEADOID:        strings.Repeat("a", 40),
		RootAccessible: true,
	}
}

// mainWorktreeRecord is the family's main worktree.
func mainWorktreeRecord(path string) gitstate.WorktreeRecord {
	record := presentRecord(gitstate.MainAdminName, path)
	record.IsMain = true
	record.Branch = "main"
	record.HEADRef = "refs/heads/main"
	return record
}

// gitSampleExisting is a filesystem sample of a root that is there.
func gitSampleExisting(token string) gitstate.PathEvidence {
	return gitstate.PathEvidence{
		RootExists:          true,
		RootIdentity:        gitstate.VolumeKindUnixDev + ":" + token + ":1",
		VolumeKind:          gitstate.VolumeKindUnixDev,
		VolumeToken:         token,
		AncestorPath:        "/repo",
		AncestorVolumeKind:  gitstate.VolumeKindUnixDev,
		AncestorVolumeToken: token,
	}
}

// gitSampleAbsent is a sample of a root that is gone, with its parent still
// present on token.
func gitSampleAbsent(token string) gitstate.PathEvidence {
	return gitstate.PathEvidence{
		AncestorPath:        "/repo",
		AncestorVolumeKind:  gitstate.VolumeKindUnixDev,
		AncestorVolumeToken: token,
	}
}

// fixture is one test's store, catalog, fakes and reconciler.
type fixture struct {
	t        *testing.T
	store    *store_sqlite.Store
	catalog  *store_sqlite.Catalog
	hooks    *recordingHooks
	git      *fakeGit
	rec      *Reconciler
	familyID string
}

// newFixture opens a real store and wires a reconciler over it. The store is
// deliberately real: the guarded transitions, the cascades and the foreign-key
// refusals are most of what is under test, and a mock catalog would assert
// against this package's own idea of them.
func newFixture(t *testing.T, cfg Config, opts ...Option) *fixture {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	f := &fixture{
		t:        t,
		store:    store,
		catalog:  store.Catalog(),
		hooks:    &recordingHooks{},
		git:      newFakeGit(),
		familyID: "fam-1",
	}
	base := []Option{
		WithInventory(f.git.inventory),
		WithPathSampler(f.git.sample),
		WithHEADSampler(f.git.head),
	}
	rec, err := New(f.catalog, f.hooks, cfg, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.rec = rec
	f.seedFamily()
	return f
}

// seedFamily writes the family row every fixture starts from.
func (f *fixture) seedFamily() {
	f.t.Helper()
	err := f.catalog.UpsertRepositoryFamily(context.Background(), store_sqlite.RepositoryFamily{
		FamilyID:          f.familyID,
		CommonDirIdentity: testCommonDir,
		State:             "family_ready",
	})
	if err != nil {
		f.t.Fatalf("seed family: %v", err)
	}
}

// seedPrimaryGraph installs a primary dedicated graph with no owner checkout,
// which is the cheapest way to make a family eligible for durable identities.
func (f *fixture) seedPrimaryGraph(graphID string) {
	f.t.Helper()
	err := f.catalog.UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID:       graphID,
		RepoPrefix:    "prefix-" + graphID,
		FamilyID:      f.familyID,
		IsPrimaryBase: true,
		State:         "graph_ready",
	})
	if err != nil {
		f.t.Fatalf("seed primary graph: %v", err)
	}
}

// seedCheckout writes a ready checkout row directly, for saga tests that need
// a populated catalog without walking a reconciliation to get there.
func (f *fixture) seedCheckout(checkoutID, incarnation, adminName string, mode store_sqlite.CheckoutMode) {
	f.t.Helper()
	err := f.catalog.UpsertCheckout(context.Background(), store_sqlite.Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   incarnation,
		FamilyID:      f.familyID,
		RootPath:      "/repo/" + adminName,
		GitDir:        testCommonDir + "/worktrees/" + adminName,
		AdminName:     adminName,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   mode,
		EffectiveMode: mode,
	})
	if err != nil {
		f.t.Fatalf("seed checkout %s: %v", checkoutID, err)
	}
}

// rekey moves a checkout to a new incarnation, which is what another actor
// re-allocating a removed-and-recreated path looks like to a pass still
// holding the old one.
func (f *fixture) rekey(checkoutID, incarnation string) {
	f.t.Helper()
	row := f.checkout(checkoutID)
	row.Incarnation = incarnation
	if err := f.catalog.UpsertCheckout(context.Background(), row); err != nil {
		f.t.Fatalf("re-key checkout %s: %v", checkoutID, err)
	}
}

// seedOwnedGraph gives a checkout its own dedicated graph.
func (f *fixture) seedOwnedGraph(graphID, ownerCheckoutID string) {
	f.t.Helper()
	err := f.catalog.UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: ownerCheckoutID,
		RepoPrefix:      "prefix-" + graphID,
		FamilyID:        f.familyID,
		State:           "graph_ready",
	})
	if err != nil {
		f.t.Fatalf("seed dedicated graph %s: %v", graphID, err)
	}
}

// promoteToPrimary walks the real promotion path, so the family's primary
// epoch is what a live promotion would have left behind.
func (f *fixture) promoteToPrimary(graphID string) int64 {
	f.t.Helper()
	ctx := context.Background()
	family, ok, err := f.catalog.GetRepositoryFamily(ctx, f.familyID)
	if err != nil || !ok {
		f.t.Fatalf("GetRepositoryFamily = %v %v", ok, err)
	}
	err = f.catalog.SetPrimaryDedicatedGraph(ctx, store_sqlite.SetPrimaryDedicatedGraphRequest{
		FamilyID:             f.familyID,
		GraphID:              graphID,
		ExpectedPrimaryEpoch: family.PrimaryEpoch,
	})
	if err != nil {
		f.t.Fatalf("SetPrimaryDedicatedGraph: %v", err)
	}
	return family.PrimaryEpoch + 1
}

// seedRoute installs a checkout's route.
func (f *fixture) seedRoute(checkoutID, graphID string) {
	f.t.Helper()
	err := f.catalog.UpsertCheckoutRoute(context.Background(), store_sqlite.CheckoutRoute{
		CheckoutID: checkoutID,
		GraphID:    graphID,
		State:      store_sqlite.RouteActive,
	})
	if err != nil {
		f.t.Fatalf("seed route for %s: %v", checkoutID, err)
	}
}

// seedRefView installs a named view rooted in a graph.
func (f *fixture) seedRefView(refViewID, graphID string) {
	f.t.Helper()
	err := f.catalog.UpsertRefView(context.Background(), store_sqlite.RefView{
		RefViewID:         refViewID,
		GraphID:           graphID,
		SelectorKind:      "branch",
		SelectorValue:     refViewID,
		EnrichmentProfile: "default",
		State:             store_sqlite.RefViewReady,
	})
	if err != nil {
		f.t.Fatalf("seed ref view %s: %v", refViewID, err)
	}
}

// seedIntent gives a checkout a tracking intent, so the cascade on delete has
// something to carry away.
func (f *fixture) seedIntent(checkoutID string) {
	f.t.Helper()
	err := f.catalog.UpsertTrackingIntent(context.Background(), store_sqlite.TrackingIntent{
		IntentID:      "intent-" + checkoutID,
		CheckoutID:    checkoutID,
		SourceKind:    store_sqlite.IntentSourceCLITrack,
		SourceLocator: "gortex track " + checkoutID,
		Active:        true,
	})
	if err != nil {
		f.t.Fatalf("seed intent for %s: %v", checkoutID, err)
	}
}

// graphExists reports whether a dedicated-graph row is still there.
func (f *fixture) graphExists(graphID string) bool {
	f.t.Helper()
	_, ok, err := f.catalog.GetDedicatedGraph(context.Background(), graphID)
	if err != nil {
		f.t.Fatalf("GetDedicatedGraph: %v", err)
	}
	return ok
}

// refViewExists reports whether a ref-view row is still there.
func (f *fixture) refViewExists(refViewID string) bool {
	f.t.Helper()
	_, ok, err := f.catalog.GetRefView(context.Background(), refViewID)
	if err != nil {
		f.t.Fatalf("GetRefView: %v", err)
	}
	return ok
}

// familyExists reports whether the family row is still there.
func (f *fixture) familyExists() bool {
	f.t.Helper()
	_, ok, err := f.catalog.GetRepositoryFamily(context.Background(), f.familyID)
	if err != nil {
		f.t.Fatalf("GetRepositoryFamily: %v", err)
	}
	return ok
}

// primaryGraphID returns the family's primary base, empty when it has none.
func (f *fixture) primaryGraphID() string {
	f.t.Helper()
	graphs, err := f.catalog.ListDedicatedGraphs(context.Background(), f.familyID)
	if err != nil {
		f.t.Fatalf("ListDedicatedGraphs: %v", err)
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			return graph.GraphID
		}
	}
	return ""
}

// reconcile runs one pass over the fixture's family.
func (f *fixture) reconcile() FamilyReport {
	f.t.Helper()
	report, err := f.rec.ReconcileFamily(context.Background(), f.familyID, "/repo")
	if err != nil {
		f.t.Fatalf("ReconcileFamily: %v", err)
	}
	return report
}

// entry returns the report entry for one admin name.
func (f *fixture) entry(report FamilyReport, adminName string) CheckoutReport {
	f.t.Helper()
	for _, entry := range report.Checkouts {
		if entry.AdminName == adminName {
			return entry
		}
	}
	f.t.Fatalf("report has no entry for %q: %+v", adminName, report.Checkouts)
	return CheckoutReport{}
}

// checkout reads one checkout row back, failing when it is gone.
func (f *fixture) checkout(checkoutID string) store_sqlite.Checkout {
	f.t.Helper()
	row, ok, err := f.catalog.GetCheckout(context.Background(), checkoutID)
	if err != nil {
		f.t.Fatalf("GetCheckout: %v", err)
	}
	if !ok {
		f.t.Fatalf("checkout %s is gone", checkoutID)
	}
	return row
}

// checkouts lists the family's checkout rows.
func (f *fixture) checkouts() []store_sqlite.Checkout {
	f.t.Helper()
	rows, err := f.catalog.ListCheckouts(context.Background(), f.familyID)
	if err != nil {
		f.t.Fatalf("ListCheckouts: %v", err)
	}
	return rows
}

// assertNoCheckoutRows fails when anything still references the checkout.
func (f *fixture) assertNoCheckoutRows(checkoutID string) {
	f.t.Helper()
	ctx := context.Background()
	if _, ok, err := f.catalog.GetCheckout(ctx, checkoutID); err != nil || ok {
		f.t.Fatalf("checkouts row survives (%v, %v)", ok, err)
	}
	if _, ok, err := f.catalog.GetCheckoutRoute(ctx, checkoutID); err != nil || ok {
		f.t.Fatalf("checkout_routes row survives (%v, %v)", ok, err)
	}
	if _, ok, err := f.catalog.GetCheckoutPathEvidence(ctx, checkoutID); err != nil || ok {
		f.t.Fatalf("checkout_path_evidence row survives (%v, %v)", ok, err)
	}
	if _, ok, err := f.catalog.GetIntentTransition(ctx, checkoutID); err != nil || ok {
		f.t.Fatalf("intent_transitions row survives (%v, %v)", ok, err)
	}
	intents, err := f.catalog.ListTrackingIntents(ctx, checkoutID)
	if err != nil || len(intents) != 0 {
		f.t.Fatalf("tracking_intents rows survive (%d, %v)", len(intents), err)
	}
}

// journal returns the whole cleanup journal.
func (f *fixture) journal() []store_sqlite.CleanupEntry {
	f.t.Helper()
	entries, err := f.catalog.ListCleanupEntries(context.Background())
	if err != nil {
		f.t.Fatalf("ListCleanupEntries: %v", err)
	}
	return entries
}
