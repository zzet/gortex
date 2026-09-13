package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The view-lifecycle counters' last hop.
//
// `StatusResponse.Views` was assembled by the controller, shipped over the
// control socket, and read by nobody: `gortex daemon status` rendered a header,
// the workspaces, the repos, the sessions and the servers, and the whole view
// block — families, levels, the metric registry, and the two reason lists that
// explain a level no count can — fell on the floor at the CLI. The counters in
// it are the W8 measurement's reuse evidence, so "shipped but never rendered"
// is the same as not existing.
//
// These tests cover both doors: the text renderer a person reads, and the
// `--format json` payload a harness parses.

// countersStatus is a status payload with a populated views block, shaped like
// what a daemon that has been advancing a committed base actually sends.
func countersStatus() daemon.StatusResponse {
	st := sampleStatus()
	st.Views = &daemon.ViewsStatus{
		Families:     2,
		Checkouts:    map[string]int{"checkout_ready": 3, "availability_grace": 1},
		Coordinators: 3,
		Generations:  map[string]int{"ready": 4, "retiring": 1},
		Leases:       2,
		RefViews:     map[string]int{"ready": 1},
		Counters: map[string]int64{
			"views_dedicated_base_publish_total{shape=delta}":  17,
			"views_dedicated_base_publish_total{shape=root}":   1,
			"views_dedicated_base_claim_total{outcome=built}":  6,
			"views_dedicated_base_claim_total{outcome=reused}": 12,
			"views_dependent_recomposition_total":              18,
		},
		CoordinatorStartFailures: []daemon.CoordinatorStartFailure{{
			CheckoutID: "co-abc",
			RootPath:   "/tmp/worktree-a",
			Reason:     "freeze the index configuration",
			At:         1757000000,
		}},
		StorageFailures: []daemon.StorageFailure{{
			GenerationID: 41,
			Reason:       "the store volume is full",
		}},
	}
	return st
}

// TestRenderDaemonViewsShowsTheLevelsCountersAndReasons is the text door.
//
// Revert-red: drop the renderDaemonViews call from runDaemonStatus and this
// still passes (it calls the renderer directly), which is why
// TestDaemonStatusTextRunsTheViewsRenderer below exists as well; delete the
// counters loop from the renderer and the counter assertions fail here.
func TestRenderDaemonViewsShowsTheLevelsCountersAndReasons(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonViews(&buf, countersStatus())
	out := buf.String()

	for _, want := range []string{
		"views:",
		"families=2",
		"coordinators=3",
		"leases=2",
		"checkout_ready=3",
		"availability_grace=1",
		"retiring=1",
		// The reuse evidence itself, key and value.
		"views_dedicated_base_publish_total{shape=delta}",
		"17",
		"views_dedicated_base_claim_total{outcome=reused}",
		"12",
		"views_dependent_recomposition_total",
		// Both reason lists.
		"co-abc",
		"/tmp/worktree-a",
		"freeze the index configuration",
		"generation 41",
		"the store volume is full",
	} {
		require.Contains(t, out, want, "the views block does not render %q:\n%s", want, out)
	}
}

// TestRenderDaemonViewsOrdersCountersStably pins the one property a
// diff-and-watch workflow needs: two renders of an unchanged payload produce
// byte-identical output. Ranging a map directly would not.
func TestRenderDaemonViewsOrdersCountersStably(t *testing.T) {
	st := countersStatus()
	var first, second bytes.Buffer
	renderDaemonViews(&first, st)
	renderDaemonViews(&second, st)
	require.Equal(t, first.String(), second.String(),
		"two renders of one payload differ, so the counter order is map order")

	out := first.String()
	claim := strings.Index(out, "views_dedicated_base_claim_total")
	publish := strings.Index(out, "views_dedicated_base_publish_total")
	require.Positive(t, claim)
	require.Less(t, claim, publish, "the counters are not sorted by series name")
}

// TestRenderDaemonViewsIsAbsentWithoutAViewLifecycle keeps a single-repo
// daemon's status exactly the shape it had. A block of zeroes would read as
// "the view lifecycle is broken" on every daemon that simply has no worktrees.
func TestRenderDaemonViewsIsAbsentWithoutAViewLifecycle(t *testing.T) {
	t.Run("no views block at all", func(t *testing.T) {
		var buf bytes.Buffer
		renderDaemonViews(&buf, sampleStatus())
		require.Empty(t, buf.String())
	})

	t.Run("an empty views block", func(t *testing.T) {
		st := sampleStatus()
		st.Views = &daemon.ViewsStatus{}
		var buf bytes.Buffer
		renderDaemonViews(&buf, st)
		require.Empty(t, buf.String())
	})

	t.Run("a census with nothing but a series", func(t *testing.T) {
		// One counted series is enough to earn the block: it is the whole
		// point of rendering it.
		st := sampleStatus()
		st.Views = &daemon.ViewsStatus{Counters: map[string]int64{
			"views_dedicated_base_claim_total{outcome=reused}": 3,
		}}
		var buf bytes.Buffer
		renderDaemonViews(&buf, st)
		require.Contains(t, buf.String(), "views_dedicated_base_claim_total{outcome=reused}")
	})
}

// TestDaemonStatusJSONCarriesTheWholeViewBlock is the machine door. A
// measurement harness cannot read the tables — they round, drop and re-shape —
// so `--format json` has to carry the payload as the daemon sent it, counters
// and both reason lists included.
//
// Revert-red: render only a subset in renderDaemonStatusJSON and the decode
// loses the field the subset dropped.
func TestDaemonStatusJSONCarriesTheWholeViewBlock(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderDaemonStatusJSON(&buf, countersStatus()))

	var got daemon.StatusResponse
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.NotNil(t, got.Views, "the json payload carries no views block")
	require.Equal(t, int64(17), got.Views.Counters["views_dedicated_base_publish_total{shape=delta}"])
	require.Equal(t, int64(12), got.Views.Counters["views_dedicated_base_claim_total{outcome=reused}"])
	require.Equal(t, int64(18), got.Views.Counters["views_dependent_recomposition_total"])
	require.Len(t, got.Views.CoordinatorStartFailures, 1)
	require.Equal(t, "co-abc", got.Views.CoordinatorStartFailures[0].CheckoutID)
	require.Len(t, got.Views.StorageFailures, 1)
	require.Equal(t, int64(41), got.Views.StorageFailures[0].GenerationID)
	require.Equal(t, "the store volume is full", got.Views.StorageFailures[0].Reason)

	// The rest of the payload is still there: a format is not a filter.
	require.Equal(t, "v0.7.1", got.Version)
	require.Len(t, got.TrackedRepos, 2)
}

// TestDaemonStatusFormatChoiceRefusesAnUnknownFormat pins the flag's
// validation. A script that asked for json and silently got tables would parse
// garbage, so an unknown value is named back rather than defaulted away.
func TestDaemonStatusFormatChoiceRefusesAnUnknownFormat(t *testing.T) {
	for _, raw := range []string{"", "text", "TEXT", " json ", "json"} {
		choice, err := daemonStatusFormatChoice(raw)
		require.NoError(t, err, "format %q", raw)
		require.Contains(t, []string{"text", "json"}, choice)
	}
	for _, raw := range []string{"yaml", "jsonl", "tabel"} {
		_, err := daemonStatusFormatChoice(raw)
		require.Error(t, err, "format %q was accepted", raw)
		require.Contains(t, err.Error(), "text, json")
	}
}

// TestTheOneShotStatusRendererReachesTheViewsBlock is the wiring trace for the
// text door: renderDaemonStatusTo is the whole of `gortex daemon status`'s
// output path (runDaemonStatus is that call plus the socket dial), so a views
// block that exists as a renderer but is never called fails here.
//
// Revert-red: delete the renderDaemonViews line from renderDaemonStatusTo and
// the first two assertions fail while the renderer's own tests above still
// pass — which is exactly the state the CLI was in before this item.
func TestTheOneShotStatusRendererReachesTheViewsBlock(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderDaemonStatusTo(&buf, countersStatus(), "text"))
	out := buf.String()

	require.Contains(t, out, "views:",
		"`gortex daemon status` renders no views block, so the counters reach no reader")
	require.Contains(t, out, "views_dedicated_base_claim_total{outcome=reused}")
	// The blocks that were already there are untouched, and still in order.
	require.Contains(t, out, "v0.7.1")
	require.Contains(t, out, "project1")
	require.Less(t, strings.Index(out, "project1"), strings.Index(out, "views:"),
		"the views block was inserted ahead of the repo table")
}

// TestTheOneShotStatusRendererEmitsOnlyJSON pins that json REPLACES the tables
// rather than being appended to them.
func TestTheOneShotStatusRendererEmitsOnlyJSON(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderDaemonStatusTo(&buf, countersStatus(), "json"))
	out := buf.String()
	require.True(t, strings.HasPrefix(strings.TrimSpace(out), "{"),
		"`--format json` emitted something before the payload:\n%s", out)

	var got daemon.StatusResponse
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got.Views)
	require.Equal(t, int64(12), got.Views.Counters["views_dedicated_base_claim_total{outcome=reused}"])
}

// TestDaemonStatusValidatesTheFormatBeforeDialing drives the cobra RunE the
// control socket's CLI half actually invokes. It is reachable without a live
// daemon precisely because the format is parsed first — which is the behaviour
// worth having: a typo in --format must not cost a socket dial and must not be
// reported as "daemon not reachable".
func TestDaemonStatusValidatesTheFormatBeforeDialing(t *testing.T) {
	prevFormat := daemonStatusFormat
	t.Cleanup(func() { daemonStatusFormat = prevFormat })
	daemonStatusFormat = "yaml"

	err := runDaemonStatus(daemonStatusCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown --format")
	require.NotContains(t, err.Error(), "not reachable",
		"an unknown format was reported only after the dial failed")
}

// TestDaemonStatusJSONRefusesWatch keeps the two output contracts apart. The
// watch mode is an alt-screen TUI; there is no honest way for it to also be a
// machine-readable stream, so the combination is refused instead of one flag
// being silently ignored.
func TestDaemonStatusJSONRefusesWatch(t *testing.T) {
	prevWatch := daemonStatusWatch
	prevFormat := daemonStatusFormat
	t.Cleanup(func() {
		daemonStatusWatch = prevWatch
		daemonStatusFormat = prevFormat
	})
	daemonStatusWatch = true
	daemonStatusFormat = "json"

	err := runDaemonStatus(daemonStatusCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--watch")
}

// TestAStorageFailureReachesTheStatusPayload is the production trace for the
// census half: a real store records a real storage-maintenance refusal, and
// the reason comes back out of realController.Status — the method the control
// socket dispatches `gortex daemon status` to.
//
// Nothing between the store and the payload is stubbed, so a Status assembled
// from a literal that drops the register fails here even though
// viewsStatusFromHealth still compiles and its own unit test still passes.
//
// The seal is opened with AtGeneration because the register deliberately
// refuses to create state for a generation this process holds no handle on —
// that refusal is what bounds it by the work in flight rather than by the ids
// a caller passes in.
func TestAStorageFailureReachesTheStatusPayload(t *testing.T) {
	ctx := context.Background()
	f := newProbeFixture(t)

	before, err := f.controller.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, before.Views, "the status payload carries no views census")
	require.Empty(t, before.Views.StorageFailures,
		"the fixture already has a generation whose maintenance failed")

	const stuck int64 = 4242
	require.NotNil(t, f.store.AtGeneration(stuck),
		"the fixture store refused a handle on the generation under test")
	require.True(t, f.store.RecordStorageFailure(stuck, &store_sqlite.StorageError{}),
		"a *StorageError was not recorded as a storage failure")

	after, err := f.controller.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, after.Views)
	require.Len(t, after.Views.StorageFailures, 1,
		"a generation whose storage maintenance failed is explained nowhere in `daemon status`")
	require.Equal(t, stuck, after.Views.StorageFailures[0].GenerationID)
	require.NotEmpty(t, after.Views.StorageFailures[0].Reason,
		"the payload names a stuck generation and does not say why")
}

// TestTheViewCountersReachTheStatusPayload is the production trace for the
// counters: a series counted in the process registry comes back out of
// realController.Status under its own flattened key.
//
// It counts a committed-base series rather than an arbitrary one because that
// is the family this item added and the family the W8 ledger cites — a status
// payload that carried the older coordinator series and dropped these would
// look healthy and measure nothing.
func TestTheViewCountersReachTheStatusPayload(t *testing.T) {
	ctx := context.Background()
	f := newProbeFixture(t)

	const key = "views_dedicated_base_claim_total{outcome=reused}"
	before, err := f.controller.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, before.Views)
	baseline := before.Views.Counters[key]

	viewmetrics.Count(viewmetrics.DedicatedBaseClaimTotal, viewmetrics.DedicatedBaseReused)

	after, err := f.controller.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, after.Views)
	require.Equal(t, baseline+1, after.Views.Counters[key],
		"the committed-base reuse counter does not reach `daemon status`")
}
