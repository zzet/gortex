package main

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/intern"
	"github.com/zzet/gortex/internal/serverstack"
)

func TestWarmupStepLogsStartBeforeCompletion(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	finish := startWarmupStep(zap.New(core), "resolver_helper", zap.String("repo", "example"))
	if observed.Len() != 1 {
		t.Fatalf("expected a start event before work completes, got %d", observed.Len())
	}
	if got := observed.All()[0]; got.Message != "daemon: warmup step start" || got.ContextMap()["step"] != "resolver_helper" {
		t.Fatalf("unexpected start event: %+v", got)
	}
	elapsed := finish(zap.String("outcome", "skipped"), zap.Int("files", 0))
	if elapsed < 0 || observed.Len() != 2 {
		t.Fatalf("expected completion with nonnegative elapsed, elapsed=%v events=%d", elapsed, observed.Len())
	}
	event := observed.All()[1]
	fields := event.ContextMap()
	if event.Message != "daemon: warmup step complete" || fields["repo"] != "example" ||
		fields["outcome"] != "skipped" || fields["files"] != int64(0) {
		t.Fatalf("missing no-op result or correlation fields: %+v", event)
	}
	if _, ok := fields["elapsed"]; !ok {
		t.Fatal("completion has no elapsed field")
	}
}

func TestWarmupStepAllowsNilLogger(t *testing.T) {
	if elapsed := startWarmupStep(nil, "no_logger")(); elapsed < 0 {
		t.Fatalf("negative elapsed: %v", elapsed)
	}
}

func TestWarmupSummaryIncludesPrelude(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	logWarmupSummary(zap.New(core), &warmupTimings{}, 0, 0)
	if observed.Len() != 1 {
		t.Fatalf("expected one summary, got %d", observed.Len())
	}
	if _, ok := observed.All()[0].ContextMap()["prelude_s"]; !ok {
		t.Fatal("summary omits prelude")
	}
}

func TestWarmupSummaryAccountsOnlyTopLevelPhases(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	timings := &warmupTimings{
		prelude: time.Second, prepareResolve: time.Second, parse: time.Second,
		resolve: 3 * time.Second, resolveCompute: time.Second, resolveTail: 2 * time.Second,
		enrich: time.Second, contractRehydrate: time.Second, workspaceBackfill: time.Second,
		globalResolve: time.Second, endBatch: time.Second, watchers: time.Second, analysis: time.Second,
	}
	logWarmupSummary(zap.New(core), timings, 5*time.Second, 15*time.Second)
	fields := observed.All()[0].ContextMap()
	if fields["accounted_s"] != float64(13) || fields["other_s"] != float64(2) {
		t.Fatalf("nested measurements counted twice or phases omitted: %+v", fields)
	}
	if fields["resolve_compute_s"] != float64(1) || fields["resolve_tail_s"] != float64(2) {
		t.Fatalf("missing resolver split: %+v", fields)
	}
}

func TestWarmupResolveTimerPreservesCallbacks(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	timer := newWarmupResolveTimer(zap.New(core))
	calls := 0
	ready := timer.ready(func() { calls++ })
	ready()
	ready()
	compute, tail := timer.finish(nil)
	if calls != 2 || compute <= 0 || tail < 0 {
		t.Fatalf("callbacks=%d compute=%v tail=%v", calls, compute, tail)
	}
	entries := observed.All()
	if len(entries) != 4 || entries[1].ContextMap()["step"] != "resolve_compute" ||
		entries[2].ContextMap()["step"] != "resolve_tail" || entries[3].Message != "daemon: warmup step complete" {
		t.Fatalf("expected exactly one compute and tail pair, got %+v", entries)
	}
}

func TestWarmupResolveTimerWithoutReadyReportsFailure(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	timer := newWarmupResolveTimer(zap.New(core))
	compute, tail := timer.finish(errors.New("resolve failed"))
	if compute < 0 || tail != 0 || observed.Len() != 2 {
		t.Fatalf("compute=%v tail=%v events=%d", compute, tail, observed.Len())
	}
	fields := observed.All()[1].ContextMap()
	if fields["queryable_callback"] != false || fields["error"] != "resolve failed" {
		t.Fatalf("missing failure without readiness: %+v", fields)
	}
}

// TestLSPDisabledSet_ConfigOnly — a `semantic.providers` entry with
// `enabled: false` whose name matches a known LSP spec lands in the
// disabled set. Entries with unknown names are ignored (so an
// `enabled: false` for a custom non-registry daemon doesn't shadow
// a same-named LSP).
func TestLSPDisabledSet_ConfigOnly(t *testing.T) {
	got := serverstack.LspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
		{Name: "tsserver", Enabled: true}, // explicitly enabled — must NOT land in disabled
		{Name: "not-a-real-lsp", Enabled: false},
	}, "")
	want := map[string]bool{"gopls": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvOnly — comma-separated names land in the
// disabled set. Whitespace is trimmed; empty entries are skipped.
func TestLSPDisabledSet_EnvOnly(t *testing.T) {
	got := serverstack.LspDisabledSet(nil, "gopls, tsserver,, ,pyright")
	want := map[string]bool{"gopls": true, "tsserver": true, "pyright": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvAllKillSwitch — the literal value "all" or
// "*" sets the special "__all__" key, signalling callers to skip
// auto-registration entirely.
func TestLSPDisabledSet_EnvAllKillSwitch(t *testing.T) {
	for _, env := range []string{"all", "ALL", "*", " all "} {
		got := serverstack.LspDisabledSet(nil, env)
		if !got["__all__"] {
			t.Fatalf("env=%q: expected __all__ kill switch, got %v", env, got)
		}
	}
}

// TestLSPDisabledSet_ConfigAndEnvMerge — disables from both sources
// merge cleanly into one map.
func TestLSPDisabledSet_ConfigAndEnvMerge(t *testing.T) {
	got := serverstack.LspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
	}, "tsserver,pyright")
	want := map[string]bool{
		"gopls":    true,
		"tsserver": true,
		"pyright":  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_Empty — no providers, empty env yields an empty
// map (not nil — callers index into it).
func TestLSPDisabledSet_Empty(t *testing.T) {
	got := serverstack.LspDisabledSet(nil, "")
	if got == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// TestWarmMtimePrefix covers the key the warm-restart mtime lookup hangs on.
// It must be the prefix the indexer actually wrote the rows under, which is
// now always the effective prefix — a lone repo included.
//
// This used to mirror the indexer's single-vs-multi gate and return "" for a
// lone repo. The two decisions had to agree exactly, and when they drifted
// the symptom was not an error but a full cold re-index (with an API
// embedder, a paid re-embed) on every restart.
func TestWarmMtimePrefix(t *testing.T) {
	cases := []struct {
		name       string
		effective  string
		wantPrefix string
		wantOK     bool
	}{
		{"a lone repo is keyed by its prefix like any other", "drools", "drools", true},
		{"a derived worktree prefix is preserved", "drools@ws", "drools@ws", true},
		{"no effective prefix is untrustworthy — cold index instead", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPrefix, gotOK := warmMtimePrefix(tc.effective)
			if gotPrefix != tc.wantPrefix || gotOK != tc.wantOK {
				t.Fatalf("warmMtimePrefix(%q) = (%q, %v), want (%q, %v)",
					tc.effective, gotPrefix, gotOK, tc.wantPrefix, tc.wantOK)
			}
		})
	}
}

func TestCanSkipWarmGlobalResolveRequiresCompletionAndStableRevision(t *testing.T) {
	base := warmGlobalResolveSafety{
		resolveOK:                     true,
		deferredCrossRepoComplete:     true,
		deferredMutationRevision:      42,
		deferredMutationRevisionKnown: true,
		backfillRevisionBefore:        42,
		backfillRevisionBeforeKnown:   true,
		backfillRevisionAfter:         43,
		backfillRevisionAfterKnown:    true,
		currentMutationRevision:       43,
		currentMutationRevisionKnown:  true,
		backfillResolutionAffected:    0,
	}
	if !canSkipWarmGlobalResolve(base) {
		t.Fatal("physical-only backfill at a bounded revision should elide the duplicate full sweep")
	}

	cases := []struct {
		name   string
		mutate func(*warmGlobalResolveSafety)
	}{
		{"pre-enrichment resolve failed", func(s *warmGlobalResolveSafety) { s.resolveOK = false }},
		{"deferred catch-up failed", func(s *warmGlobalResolveSafety) { s.deferredCrossRepoComplete = false }},
		{"completion revision unsupported", func(s *warmGlobalResolveSafety) { s.deferredMutationRevisionKnown = false }},
		{"pre-backfill revision unsupported", func(s *warmGlobalResolveSafety) { s.backfillRevisionBeforeKnown = false }},
		{"post-backfill revision unsupported", func(s *warmGlobalResolveSafety) { s.backfillRevisionAfterKnown = false }},
		{"current revision unsupported", func(s *warmGlobalResolveSafety) { s.currentMutationRevisionKnown = false }},
		{"writer interleaved before backfill", func(s *warmGlobalResolveSafety) { s.backfillRevisionBefore++ }},
		{"writer interleaved after backfill", func(s *warmGlobalResolveSafety) { s.currentMutationRevision++ }},
		{"workspace eligibility changed", func(s *warmGlobalResolveSafety) { s.backfillResolutionAffected = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			safety := base
			tc.mutate(&safety)
			if canSkipWarmGlobalResolve(safety) {
				t.Fatal("incomplete or stale cross-repo proof elided the full sweep")
			}
		})
	}
}

func TestSelectWarmGlobalResolveAction(t *testing.T) {
	complete := warmGlobalResolveSafety{
		resolveOK:                     true,
		deferredCrossRepoComplete:     true,
		deferredMutationRevision:      17,
		deferredMutationRevisionKnown: true,
		backfillRevisionBefore:        17,
		backfillRevisionBeforeKnown:   true,
		backfillRevisionAfter:         18,
		backfillRevisionAfterKnown:    true,
		currentMutationRevision:       18,
		currentMutationRevisionKnown:  true,
	}
	interleaved := complete
	interleaved.currentMutationRevision++
	eligibilityChanged := complete
	eligibilityChanged.backfillResolutionAffected = 1
	cases := []struct {
		name                string
		anyChanged          bool
		safety              warmGlobalResolveSafety
		backfilledContracts int
		want                warmGlobalResolveAction
	}{
		{"changed completed deferred catch-up across physical stamps", true, complete, 0, warmGlobalResolveContracts},
		{"changed failed initial resolve", true, warmGlobalResolveSafety{}, 0, warmGlobalResolveFull},
		{"changed failed deferred catch-up", true, warmGlobalResolveSafety{resolveOK: true}, 0, warmGlobalResolveFull},
		{"changed interleaving graph writer", true, interleaved, 0, warmGlobalResolveFull},
		{"changed workspace eligibility", true, eligibilityChanged, 0, warmGlobalResolveFull},
		{"unchanged physical-only node stamps", false, warmGlobalResolveSafety{}, 0, warmGlobalResolveNone},
		{"unchanged workspace eligibility", false, warmGlobalResolveSafety{backfillResolutionAffected: 1}, 0, warmGlobalResolveFull},
		{"unchanged legacy contract stamp", false, warmGlobalResolveSafety{}, 1, warmGlobalResolveContracts},
		{"unchanged physical and contract stamps", false, warmGlobalResolveSafety{}, 1, warmGlobalResolveContracts},
		{"unchanged clean snapshot", false, warmGlobalResolveSafety{}, 0, warmGlobalResolveNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectWarmGlobalResolveAction(tc.anyChanged, tc.safety, tc.backfilledContracts); got != tc.want {
				t.Fatalf("action = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRotateColdInternGeneration(t *testing.T) {
	intern.Reset()
	t.Cleanup(func() { intern.Reset() })

	for _, phase := range []string{"parallel_parse", "deferred_passes_all"} {
		intern.String("daemon-warmup/" + phase)
		if got := rotateColdInternGeneration(zap.NewNop(), phase); got != 1 {
			t.Fatalf("rotateColdInternGeneration(%q) released %d strings, want 1", phase, got)
		}
		if got := intern.Len(); got != 0 {
			t.Fatalf("interner length after %q rotation = %d, want 0", phase, got)
		}
	}
}
