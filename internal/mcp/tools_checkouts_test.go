package mcp

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// The administrative tools are tested through their handlers against a real
// sqlite store, a real git repository and a real linked worktree. What is
// being asserted is what each tool decides — which rows it reports, and
// whether it wrote anything — and a stubbed catalog would remove exactly that.

// adminToolHandler is the shape every handler under test has.
type adminToolHandler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)

// callAdminTool runs one handler and returns its JSON payload.
func callAdminTool(t *testing.T, handler adminToolHandler, args map[string]any) []byte {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "tool failed: %+v", res.Content)
	return []byte(extractTextFromContent(t, res.Content))
}

// checkoutAdminFixture is one tracked repository and the linked worktree
// beside it, with the catalog rows the daemon made of them.
type checkoutAdminFixture struct {
	srv      *Server
	mi       *indexer.MultiIndexer
	catalog  *store_sqlite.Catalog
	dir      string
	main     string
	worktree string
	prefix   string
}

func newCheckoutAdminFixture(t *testing.T) *checkoutAdminFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	srv, mi, catalog, dir := newCatalogMCPServer(t)

	main := filepath.Join(dir, "main-repo")
	gitInitWorktreeRepo(t, main)
	worktree := filepath.Join(dir, "wt")
	gitInit(t, main, "worktree", "add", "-q", "-b", "task", worktree)

	prefix, status := trackRepoPrefix(t, srv, map[string]any{"path": main})
	require.Equal(t, "tracked", status)

	fixture := &checkoutAdminFixture{
		srv: srv, mi: mi, catalog: catalog, dir: dir,
		main: main, worktree: worktree, prefix: prefix,
	}
	// The worktree beside the tracked repository is the family's other
	// checkout. A reconciliation is what mints its identity.
	callAdminTool(t, srv.handleReconcileCheckouts, map[string]any{})
	return fixture
}

// quiesce stops the family's build loops.
//
// Reconciliation starts a coordinator for every automatic checkout, and a
// live coordinator repoints the route it finds at the layers it builds
// itself: it clears the working-tree slot and puts the route back to pending
// for the length of the rebuild. A test that installs a route by hand and
// then reads it has to stop the loops first, or it races that rebuild.
//
// Closing writes nothing to the catalog — it is about the goroutines — and it
// returns only once a cycle already in flight has finished, so no coordinator
// can touch a route after it.
func (f *checkoutAdminFixture) quiesce(t *testing.T) {
	t.Helper()
	require.NoError(t, f.srv.lifecycle.Close())
}

// families reads the listing tool's answer.
func (f *checkoutAdminFixture) families(t *testing.T, args map[string]any) indexer.FamiliesOverview {
	t.Helper()
	var overview indexer.FamiliesOverview
	require.NoError(t, json.Unmarshal(callAdminTool(t, f.srv.handleListCheckouts, args), &overview))
	return overview
}

// checkoutNamed finds one family's checkout by the name git administers it as.
func checkoutNamed(t *testing.T, family indexer.FamilyOverview, adminName string) indexer.CheckoutOverview {
	t.Helper()
	for _, checkout := range family.Checkouts {
		if checkout.AdminName == adminName {
			return checkout
		}
	}
	t.Fatalf("family %s holds no checkout administered as %q", family.FamilyID, adminName)
	return indexer.CheckoutOverview{}
}

// TestListCheckoutsReportsTheFamilyItsGraphsAndItsCheckouts is the read model
// through the tool: one family, its primary corpus, and both working copies
// with the mode each is served in.
func TestListCheckoutsReportsTheFamilyItsGraphsAndItsCheckouts(t *testing.T) {
	f := newCheckoutAdminFixture(t)

	overview := f.families(t, map[string]any{})
	require.Len(t, overview.Families, 1)
	family := overview.Families[0]
	assert.NotEmpty(t, family.FamilyID)
	assert.Equal(t, f.prefix, family.PrimaryRepoPrefix)
	assert.NotEmpty(t, family.PrimaryGraphID)

	require.Len(t, family.Graphs, 1, "only the tracked repository has a corpus")
	assert.True(t, family.Graphs[0].IsPrimary)
	assert.True(t, family.Graphs[0].Served, "the primary corpus is indexed in this process")

	require.Len(t, family.Checkouts, 2, "the family holds the main worktree and the linked one")
	primary := checkoutNamed(t, family, gitstate.MainAdminName)
	assert.Equal(t, string(store_sqlite.CheckoutModeDedicated), primary.EffectiveMode)
	assert.Equal(t, family.PrimaryGraphID, primary.GraphID)
	assert.True(t, primary.Evidence.Present, "the track sampled the root")
	assert.Contains(t, primary.Intents, string(store_sqlite.IntentSourceMCPTrack))

	linked := checkoutNamed(t, family, "wt")
	assert.Equal(t, string(store_sqlite.CheckoutModeAutomatic), linked.EffectiveMode)
	assert.Empty(t, linked.GraphID, "an automatic checkout owns no corpus")
	assert.False(t, linked.Availability.Running, "a reachable root starts no clock")
	assert.False(t, linked.Removal.Running)

	// The filter takes any of the selectors an administrator has to hand.
	byPath := f.families(t, map[string]any{"family": f.main})
	require.Len(t, byPath.Families, 1)
	assert.Equal(t, family.FamilyID, byPath.Families[0].FamilyID)
}

// TestExplainViewWalksTheBindingChain covers the answers the diagnostic has to
// tell apart: routed automatic and dedicated checkouts, a dedicated checkout
// served directly from its sealed base, and a path no checkout contains.
func TestExplainViewWalksTheBindingChain(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	// What is under test is the chain the binding walks, not the builder that
	// fills it, so the builder is stopped before the chain is laid out by hand.
	f.quiesce(t)
	ctx := context.Background()
	family := f.families(t, map[string]any{}).Families[0]
	linked := checkoutNamed(t, family, "wt")
	primary := checkoutNamed(t, family, gitstate.MainAdminName)

	// A route with both slots filled is what makes a composed view answer.
	// The generations are seeded rather than built.
	commitGen := seedGeneration(t, f.catalog, family.PrimaryGraphID, linked.CheckoutID, "commit")
	dirtyGen := seedGeneration(t, f.catalog, family.PrimaryGraphID, linked.CheckoutID, "dirty")
	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         linked.CheckoutID,
		GraphID:            family.PrimaryGraphID,
		CommitGenerationID: commitGen,
		DirtyGenerationID:  dirtyGen,
		RouteEpoch:         1,
		State:              store_sqlite.RouteActive,
	}))

	routed := explainView(t, f.srv, f.worktree)
	assert.True(t, routed.Matched)
	assert.Equal(t, "wt", routed.AdminName)
	assert.Equal(t, string(store_sqlite.CheckoutModeAutomatic), routed.EffectiveMode)
	assert.True(t, routed.Composed, "both layers are published, so the composed view answers")
	assert.Empty(t, routed.Reason)
	require.NotNil(t, routed.Route)
	assert.Equal(t, commitGen, routed.Route.CommitGenerationID)
	assert.Equal(t, dirtyGen, routed.Route.DirtyGenerationID)
	assert.True(t, routed.Route.Ready)
	assert.Equal(t, family.PrimaryGraphID, routed.PrimaryGraphID)

	dedicatedRouted := explainView(t, f.srv, f.main)
	assert.True(t, dedicatedRouted.Matched)
	assert.Equal(t, string(store_sqlite.CheckoutModeDedicated), dedicatedRouted.EffectiveMode)
	assert.True(t, dedicatedRouted.Composed)
	assert.Empty(t, dedicatedRouted.Reason)
	assert.Equal(t, family.PrimaryGraphID, dedicatedRouted.GraphID)
	assert.Equal(t, family.Graphs[0].State, dedicatedRouted.GraphState)
	assert.Equal(t, family.Graphs[0].ActiveGenerationID, dedicatedRouted.ActiveGenerationID)
	require.NotNil(t, dedicatedRouted.Route)
	assert.True(t, dedicatedRouted.Route.Ready)

	require.NoError(t, f.catalog.DeleteCheckoutRoute(ctx, primary.CheckoutID))
	dedicatedDirect := explainView(t, f.srv, f.main)
	assert.True(t, dedicatedDirect.Matched)
	assert.Equal(t, string(store_sqlite.CheckoutModeDedicated), dedicatedDirect.EffectiveMode)
	assert.False(t, dedicatedDirect.Composed)
	assert.Contains(t, dedicatedDirect.Reason, "sealed base")
	assert.Equal(t, family.PrimaryGraphID, dedicatedDirect.GraphID)
	assert.Equal(t, family.Graphs[0].State, dedicatedDirect.GraphState)
	assert.Equal(t, family.Graphs[0].ActiveGenerationID, dedicatedDirect.ActiveGenerationID)
	assert.Nil(t, dedicatedDirect.Route)

	commitOnly := seedGeneration(t, f.catalog, family.PrimaryGraphID, primary.CheckoutID, "commit")
	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID: primary.CheckoutID, GraphID: family.PrimaryGraphID,
		CommitGenerationID: commitOnly, State: store_sqlite.RouteActive,
	}))
	dedicatedIncomplete := explainView(t, f.srv, f.main)
	assert.False(t, dedicatedIncomplete.Composed)
	require.NotNil(t, dedicatedIncomplete.Route)
	assert.False(t, dedicatedIncomplete.Route.Ready)
	assert.Contains(t, dedicatedIncomplete.Reason, "does not name both generations")

	unknown := explainView(t, f.srv, t.TempDir())
	assert.False(t, unknown.Matched)
	assert.False(t, unknown.Composed)
	assert.Contains(t, unknown.Reason, "no registered checkout")
	assert.Empty(t, unknown.CheckoutID)
}

// TestSetPrimaryCheckoutPreviewsBeforeItMoves proves the preview writes
// nothing and the confirm moves the family's base.
func TestSetPrimaryCheckoutPreviewsBeforeItMoves(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	ctx := context.Background()
	before := f.families(t, map[string]any{}).Families[0]

	worktreePrefix, status := trackRepoPrefix(t, f.srv, map[string]any{"path": f.worktree})
	require.Equal(t, "tracked", status)
	require.NotEqual(t, f.prefix, worktreePrefix, "the worktree gets a corpus of its own")

	var preview struct {
		Status          string `json:"status"`
		Ready           bool   `json:"ready"`
		ConfirmRequired bool   `json:"confirm_required"`
		GraphID         string `json:"graph_id"`
		CurrentGraphID  string `json:"current_graph_id"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleSetPrimaryCheckout, map[string]any{"graph": worktreePrefix}),
		&preview))
	assert.Equal(t, "preview", preview.Status)
	assert.True(t, preview.ConfirmRequired)
	assert.True(t, preview.Ready)
	assert.Equal(t, before.PrimaryGraphID, preview.CurrentGraphID)
	assert.Equal(t, indexer.GraphIDFor(worktreePrefix), preview.GraphID)

	graphs, err := f.catalog.ListDedicatedGraphs(ctx, before.FamilyID)
	require.NoError(t, err)
	assert.Equal(t, before.PrimaryGraphID, primaryGraphOf(graphs), "a preview writes nothing")

	var confirmed struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleSetPrimaryCheckout,
			map[string]any{"graph": worktreePrefix, "confirm": true}),
		&confirmed))
	assert.Equal(t, "primary_set", confirmed.Status)

	graphs, err = f.catalog.ListDedicatedGraphs(ctx, before.FamilyID)
	require.NoError(t, err)
	assert.Equal(t, indexer.GraphIDFor(worktreePrefix), primaryGraphOf(graphs))
}

// TestForgetCheckoutPreviewsThenRemovesIt proves forget never runs without an
// explicit confirm, and that the confirm takes the corpus with it.
func TestForgetCheckoutPreviewsThenRemovesIt(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	ctx := context.Background()
	family := f.families(t, map[string]any{}).Families[0]

	worktreePrefix, _ := trackRepoPrefix(t, f.srv, map[string]any{"path": f.worktree})
	require.NotNil(t, f.mi.GetMetadata(worktreePrefix))

	var preview struct {
		Status          string `json:"status"`
		Plan            string `json:"plan"`
		Prefix          string `json:"prefix"`
		ConfirmRequired bool   `json:"confirm_required"`
		Closure         []struct {
			Kind string `json:"kind"`
		} `json:"closure"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleForgetCheckout, map[string]any{"path": f.worktree}),
		&preview))
	assert.Equal(t, "preview", preview.Status)
	assert.Equal(t, "forget", preview.Plan)
	assert.True(t, preview.ConfirmRequired)
	assert.Equal(t, worktreePrefix, preview.Prefix)
	assert.NotEmpty(t, preview.Closure, "the preview names what goes")
	require.NotNil(t, f.mi.GetMetadata(worktreePrefix), "a preview writes nothing")

	var confirmed struct {
		Status string `json:"status"`
		Plan   string `json:"plan"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleForgetCheckout,
			map[string]any{"path": f.worktree, "confirm": true}),
		&confirmed))
	assert.Equal(t, "forgotten", confirmed.Status)
	assert.Equal(t, "forget", confirmed.Plan)
	assert.Nil(t, f.mi.GetMetadata(worktreePrefix), "the corpus is gone")

	checkouts, err := f.catalog.ListCheckouts(ctx, family.FamilyID)
	require.NoError(t, err)
	for _, checkout := range checkouts {
		assert.NotEqual(t, "wt", checkout.AdminName, "the identity is gone")
	}
}

// TestUntrackDemotesAWorktreeWithoutAConfirm is the other half of the untrack
// rule: a plan that keeps the checkout is not destructive, so it runs.
func TestUntrackDemotesAWorktreeWithoutAConfirm(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	ctx := context.Background()
	family := f.families(t, map[string]any{}).Families[0]

	worktreePrefix, _ := trackRepoPrefix(t, f.srv, map[string]any{"path": f.worktree})
	require.NotNil(t, f.mi.GetMetadata(worktreePrefix))

	var payload struct {
		Status  string `json:"status"`
		Plan    string `json:"plan"`
		Demoted bool   `json:"demoted"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleUntrackRepository, map[string]any{"path": f.worktree}),
		&payload))
	assert.Equal(t, "demoted", payload.Status)
	assert.Equal(t, "demote", payload.Plan)
	assert.True(t, payload.Demoted)

	checkouts, err := f.catalog.ListCheckouts(ctx, family.FamilyID)
	require.NoError(t, err)
	found := false
	for _, checkout := range checkouts {
		if checkout.AdminName != "wt" {
			continue
		}
		found = true
		assert.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)
	}
	assert.True(t, found, "the demoted checkout keeps its identity")
}

// TestReconcileCheckoutsReportsWhatItDecided covers both scopes of the
// force-reconcile verb.
func TestReconcileCheckoutsReportsWhatItDecided(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	family := f.families(t, map[string]any{}).Families[0]

	var all struct {
		Status   string `json:"status"`
		Families []struct {
			FamilyID        string `json:"family_id"`
			InventoryUsable bool   `json:"inventory_usable"`
			PrimaryGraphID  string `json:"primary_graph_id"`
			Checkouts       []struct {
				AdminName string `json:"admin_name"`
				Action    string `json:"action"`
			} `json:"checkouts"`
		} `json:"families"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts, map[string]any{}), &all))
	assert.Equal(t, "reconciled", all.Status)
	require.Len(t, all.Families, 1)
	assert.Equal(t, family.FamilyID, all.Families[0].FamilyID)
	assert.True(t, all.Families[0].InventoryUsable)
	assert.Equal(t, family.PrimaryGraphID, all.Families[0].PrimaryGraphID)
	assert.Len(t, all.Families[0].Checkouts, 2)

	var one struct {
		Families []struct {
			FamilyID string `json:"family_id"`
		} `json:"families"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts, map[string]any{"family": f.worktree}), &one))
	require.Len(t, one.Families, 1)
	assert.Equal(t, family.FamilyID, one.Families[0].FamilyID)
}

// TestReconcileOneFamilyReportsItsLiveCoordinators pins the count the verb
// renders as "%d coordinators live". A scope that leaves it out of the answer
// renders a daemon running build loops as one running none.
func TestReconcileOneFamilyReportsItsLiveCoordinators(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	family := f.families(t, map[string]any{}).Families[0]

	var all struct {
		Coordinators int `json:"coordinators"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts, map[string]any{}), &all))
	require.Positive(t, all.Coordinators, "the linked worktree runs no coordinator to count")

	var one struct {
		Coordinators int `json:"coordinators"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts,
			map[string]any{"family": family.FamilyID}), &one))
	assert.Equal(t, all.Coordinators, one.Coordinators,
		"the family holding every live coordinator reports none")
}

// explainView runs the diagnostic for one path.
func explainView(t *testing.T, srv *Server, path string) indexer.ViewBinding {
	t.Helper()
	var binding indexer.ViewBinding
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, srv.handleExplainView, map[string]any{"path": path}), &binding))
	return binding
}

// seedGeneration writes one published generation so a route may name it.
func seedGeneration(t *testing.T, catalog *store_sqlite.Catalog, graphID, checkoutID, kind string) int64 {
	t.Helper()
	id, err := catalog.CreateViewGeneration(context.Background(), store_sqlite.ViewGeneration{
		OwnerKind:      "checkout",
		GraphID:        graphID,
		CheckoutID:     checkoutID,
		GenerationKind: kind,
		State:          store_sqlite.ViewGenerationReady,
	})
	require.NoError(t, err)
	return id
}

// primaryGraphOf reports which of a family's graphs is the base.
func primaryGraphOf(graphs []store_sqlite.DedicatedGraph) string {
	for _, dedicated := range graphs {
		if dedicated.IsPrimaryBase {
			return dedicated.GraphID
		}
	}
	return ""
}
