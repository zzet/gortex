package indexer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// canonicalTempRoot returns a temporary directory in the form the OS reports
// filesystem events for.
//
// On macOS t.TempDir() hands back a path under /var, which is a symlink to
// /private/var. FSEvents reports the resolved form, so the backend cannot
// prefix-match an event against a root registered under /var and silently
// skips every path-scoped option — including the exclude filter. A watcher
// test on the raw path therefore exercises a backend with no filter attached,
// which is how a filter that swallowed the watcher's own startup handshake
// reached a release. Resolving up front keeps these tests on the same code
// path a real repository under /Users or /home takes.
func canonicalTempRoot(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return resolved
}

func newBarrierTestWatcher(t *testing.T, dir string, cfg config.WatchConfig) *Watcher {
	t.Helper()
	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(g, registry, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	w, err := NewWatcher(idx, cfg, zap.NewNop())
	require.NoError(t, err)
	return w
}

// TestWatcherStartCompletesOnCanonicalGitRoot is the regression for the
// watcher that never started on macOS. The startup barrier writes its marker
// into <root>/.git to keep it out of git status, and the backend was handed an
// exclude matcher whose builtin list drops .git — so the barrier waited five
// seconds for an event the backend had already thrown away, Start failed for
// every tracked repo on every daemon start, and live watching was dead while
// the daemon kept reporting the index fresh.
func TestWatcherStartCompletesOnCanonicalGitRoot(t *testing.T) {
	dir := canonicalTempRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	writeTestFile(t, filepath.Join(dir, "main.go"), "package sample\n\nfunc Value() {}\n")

	w := newBarrierTestWatcher(t, dir, config.WatchConfig{Enabled: true, DebounceMs: 1})
	require.NoError(t, w.Start([]string{dir}))
	t.Cleanup(func() { _ = w.Stop() })

	require.Empty(t, w.DegradedReason(), "a healthy start must not report degradation")
}

// TestWatcherExcludeFilterAdmitsHandshakeProbes pins the asymmetry the fix
// depends on: ignored trees stay filtered at the backend, but the watcher's
// own handshake probe is never filtered, wherever it is written. Both the
// Darwin startup barrier and the Linux confirmWatchActive probe rely on
// observing their own marker, so filtering it is always a self-inflicted hang.
func TestWatcherExcludeFilterAdmitsHandshakeProbes(t *testing.T) {
	root := t.TempDir()
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Paths: []string{root}}, zap.NewNop())
	require.NoError(t, err)
	filter := &watcherExcludeFilter{w: w}

	for _, rel := range []string{
		filepath.Join(".git", probeMarker+"1234-5678-0"),
		probeMarker + "1234-5678-0",
		filepath.Join("node_modules", probeMarker+"1234-5678-0"),
	} {
		require.True(t, filter.ShouldInclude(filepath.Join(root, rel)),
			"%s is the watcher's own handshake and must reach the barrier", rel)
	}

	// The optimisation the filter exists for is unchanged.
	for _, rel := range []string{".git/objects/ab/cdef", "node_modules/pkg/index.js"} {
		require.False(t, filter.ShouldInclude(filepath.Join(root, rel)),
			"%s must still be filtered at the backend", rel)
	}
}

// TestWatcherStartDegradesWhenBarrierIncomplete pins the second half of the
// outage: a barrier that does not complete used to fail Start outright, which
// left the repository with no live watching, no poller and no degraded reason
// — staleness with nothing to report it. The barrier is an ordering guarantee,
// not a liveness requirement, so an incomplete one now degrades loudly and
// keeps watching.
func TestWatcherStartDegradesWhenBarrierIncomplete(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the startup marker barrier is Darwin-specific")
	}
	t.Setenv("GORTEX_WATCHER_STARTUP_BARRIER_TIMEOUT", "1ns")
	dir := canonicalTempRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	writeTestFile(t, filepath.Join(dir, "main.go"), "package sample\n\nfunc Value() {}\n")

	w := newBarrierTestWatcher(t, dir, config.WatchConfig{Enabled: true, DebounceMs: 1})
	var degraded string
	w.OnDegraded(func(reason string) { degraded = reason })

	require.NoError(t, w.Start([]string{dir}),
		"an unobserved barrier must not leave the repository unwatched")
	t.Cleanup(func() { _ = w.Stop() })

	require.NotEmpty(t, w.DegradedReason(), "silent staleness: the watcher degraded without saying so")
	require.NotEmpty(t, degraded, "the degraded callback drives the daemon health push")
	require.NotNil(t, w.poller, "the adaptive poller is the fallback for what the barrier missed")

	// Live watching still works — the whole point of degrading over failing.
	writeTestFile(t, filepath.Join(dir, "added.go"), "package sample\n\nfunc Added() {}\n")
	require.Eventually(t, func() bool {
		return len(w.indexer.graph.FindNodesByName("Added")) > 0
	}, 10*time.Second, 25*time.Millisecond, "a degraded watcher must still deliver live edits")
}

// TestStartupBarrierIncompleteErrorIsDiagnosable pins the operator-facing half
// of the report: the single warn line named neither which root was waiting nor
// whether any events arrived at all, so a dead stream and a filter swallowing
// the handshake were indistinguishable.
func TestStartupBarrierIncompleteErrorIsDiagnosable(t *testing.T) {
	dir := t.TempDir()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	w.fsw = newStartupBarrierFakeWatcher()

	err = w.reconcileInitialReplayThroughMarkers([]string{dir}, 10*time.Millisecond)
	require.ErrorIs(t, err, errStartupBarrierIncomplete,
		"a published-but-unobserved barrier is a liveness fault, not a setup fault")
	require.ErrorContains(t, err, "did not complete")
	require.ErrorContains(t, err, dir, "the failing root must be named")
	require.ErrorContains(t, err, "0 event(s) observed",
		"an empty stream must be distinguishable from a filtered handshake")
}

// TestStartupBarrierMarkerCreationStaysFailClosed guards the line between the
// two failure classes: a root we cannot write a marker into is a setup fault
// the operator must fix, and degrading past it would hide a broken install.
func TestStartupBarrierMarkerCreationStaysFailClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	dir := t.TempDir()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	w.fsw = newStartupBarrierFakeWatcher()
	w.initialReplayMarkerCreating = blockStartupMarkerCreation(t, dir)

	err = w.reconcileInitialReplayThroughMarkers([]string{dir}, time.Second)
	require.Error(t, err)
	require.NotErrorIs(t, err, errStartupBarrierIncomplete,
		"an unwritable root must stay fail-closed rather than degrade")
	require.ErrorContains(t, err, "create startup stream marker")
}

func TestStartupBarrierTimeoutHonoursEnvOverride(t *testing.T) {
	require.Equal(t, defaultStartupBarrierTimeout, startupBarrierTimeout())

	t.Setenv("GORTEX_WATCHER_STARTUP_BARRIER_TIMEOUT", "30s")
	require.Equal(t, 30*time.Second, startupBarrierTimeout())

	for _, bad := range []string{"", "nonsense", "0s", "-5s"} {
		t.Setenv("GORTEX_WATCHER_STARTUP_BARRIER_TIMEOUT", bad)
		require.Equal(t, defaultStartupBarrierTimeout, startupBarrierTimeout(),
			"%q must fall back to the default rather than disarm the barrier", bad)
	}
}

// TestBackendWatchPathMatchesReportedEventForm pins that the root handed to the
// backend is the form the OS reports events in. When the two disagree the
// backend attaches no path-scoped options at all, so the exclude filter goes
// silently inert and ignored-tree churn crosses the channel, the aggregator and
// the storm counter unfiltered.
func TestBackendWatchPathMatchesReportedEventForm(t *testing.T) {
	raw := t.TempDir()
	resolved, err := filepath.EvalSymlinks(raw)
	require.NoError(t, err)

	if runtime.GOOS == "darwin" {
		require.Equal(t, resolved, backendWatchPath(raw))
	} else {
		require.Equal(t, raw, backendWatchPath(raw),
			"inotify reports paths as registered — resolving would desync the daemon")
	}

	// A path that cannot be resolved is registered as given rather than dropped.
	missing := filepath.Join(raw, "no-such-dir")
	require.Equal(t, missing, backendWatchPath(missing))
}
