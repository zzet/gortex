package hooks

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// coverageController answers the coverage verb over the real control socket
// and records what the hook asked about.
type coverageController struct {
	*fakeController
	mu       sync.Mutex
	asked    string
	coverage daemon.FileCoverageResult
}

func (c *coverageController) FileCoverage(
	_ context.Context, p daemon.FileCoverageParams,
) (daemon.FileCoverageResult, error) {
	c.mu.Lock()
	c.asked = p.Path
	c.mu.Unlock()
	return c.coverage, nil
}

func (c *coverageController) probedPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

func startCoverageDaemon(t *testing.T, result daemon.FileCoverageResult) *coverageController {
	t.Helper()
	ctrl := &coverageController{fakeController: &fakeController{}, coverage: result}
	startTestDaemon(t, ctrl)
	return ctrl
}

// probeFileIndexScope runs the real probe against the test daemon under the
// production budget.
func probeFileIndexScope(cwd, filePath string) fileIndexStatus {
	return fileIndexScopeViaDaemon(cwd, filePath, fileIndexedTimeout)
}

// TestFileIndexedViaDaemonSendsTheAbsolutePath pins the shape of the request:
// the hook resolves nothing itself, it hands the daemon an absolute path and
// lets the catalog decide which graph serves it. Resolving the path hook-side
// is what made every worktree file look untracked.
func TestFileIndexedViaDaemonSendsTheAbsolutePath(t *testing.T) {
	ctrl := startCoverageDaemon(t, daemon.FileCoverageResult{
		Answered: true,
		Tracked:  true,
		Held:     true,
		Covered:  true,
		Symbols:  5,
		View:     &daemon.ProbeView{Kind: daemon.ProbeViewWorktree, Exact: true},
	})

	st := probeFileIndexScope("/wt", "internal/live.go")
	if !st.Indexed || st.Count != 5 {
		t.Fatalf("coverage = %+v, want the daemon's answer (indexed, 5 symbols)", st)
	}
	if want := filepath.Join("/wt", "internal/live.go"); ctrl.probedPath() != want {
		t.Fatalf("daemon was asked about %q, want %q", ctrl.probedPath(), want)
	}
}

// TestFileIndexedViaDaemonFailsOpenOnAnUnroutedWorktree pins the
// reconcile-then-enforce posture at the hook boundary: an uncovered answer is
// no signal, so the caller falls through to soft guidance and the native tool
// proceeds.
func TestFileIndexedViaDaemonFailsOpenOnAnUnroutedWorktree(t *testing.T) {
	startCoverageDaemon(t, daemon.FileCoverageResult{
		View: &daemon.ProbeView{
			Kind:           daemon.ProbeViewUnrouted,
			FallbackReason: daemon.FallbackViewBuilding,
		},
	})

	st := probeFileIndexScope("/wt", "internal/live.go")
	if st.Indexed || st.Count != 0 {
		t.Fatalf("coverage = %+v, want the fail-open", st)
	}
	// An unbuilt view is an abstention, not proof the file is uncovered.
	if st.ProbeOK {
		t.Fatalf("coverage = %+v, want no verdict at all from an unrouted worktree", st)
	}
}

// TestFileIndexedViaDaemonFailsOpenWithNoDaemon is the outage case the hook
// has always had to survive. It is asserted here because the transport moved:
// a probe that started reporting "covered" — or blocking — when the daemon is
// down would deny every read on a machine with no daemon at all.
func TestFileIndexedViaDaemonFailsOpenWithNoDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gx-hook-nocov")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "missing"))

	start := time.Now()
	st := probeFileIndexScope("/wt", "internal/live.go")
	if st.Indexed || st.Count != 0 {
		t.Fatalf("coverage = %+v with no daemon, want nothing", st)
	}
	if !st.Unreached {
		t.Fatalf("coverage = %+v with no daemon, want the probe marked unreached", st)
	}
	if elapsed := time.Since(start); elapsed > fileIndexedTimeout {
		t.Fatalf("the probe took %s with no daemon, want under the %s budget", elapsed, fileIndexedTimeout)
	}
}

// TestFileIndexedViaDaemonWithNoCWDCannotResolveARelativePath pins that a
// relative path with nothing to resolve it against is still no signal rather
// than a request the daemon would answer about its own working directory.
func TestFileIndexedViaDaemonWithNoCWDCannotResolveARelativePath(t *testing.T) {
	startCoverageDaemon(t, daemon.FileCoverageResult{Answered: true, Covered: true, Symbols: 3})

	if st := probeFileIndexScope("", "internal/live.go"); st.Indexed || st.Count != 0 || st.ProbeOK {
		t.Fatalf("coverage = %+v for an unresolvable path, want no verdict", st)
	}
}

// TestFileIndexedViaDaemonLogsOnlyFallbackAnswers pins the logging half of the
// grace rule: a fallback answer leaves a record naming the graph that stood in,
// and an exact one leaves the log alone so it stays a record of degradations.
func TestFileIndexedViaDaemonLogsOnlyFallbackAnswers(t *testing.T) {
	log := redirectTelemetry(t)
	startCoverageDaemon(t, daemon.FileCoverageResult{
		Answered: true,
		Tracked:  true,
		Held:     true,
		Covered:  true,
		Symbols:  2,
		View: &daemon.ProbeView{
			Kind:           daemon.ProbeViewBase,
			Exact:          false,
			FallbackReason: "availability_grace",
		},
	})

	if st := probeFileIndexScope("/wt", "internal/live.go"); !st.Indexed || st.Count != 2 {
		t.Fatalf("coverage = %+v, want the fallback answer honoured unchanged", st)
	}
	records := readDecisions(t, log)
	if len(records) != 1 {
		t.Fatalf("decision log holds %d records, want the one fallback record", len(records))
	}
	if records[0].Decision != DecisionViewFallback {
		t.Fatalf("logged decision = %q, want %q", records[0].Decision, DecisionViewFallback)
	}
	if records[0].View != daemon.ProbeViewBase+"/availability_grace" {
		t.Fatalf("logged view = %q, want the graph and the reason it stood in", records[0].View)
	}
}

func TestFileIndexedViaDaemonLogsNothingForAnExactAnswer(t *testing.T) {
	log := redirectTelemetry(t)
	startCoverageDaemon(t, daemon.FileCoverageResult{
		Answered: true,
		Tracked:  true,
		Held:     true,
		Covered:  true,
		Symbols:  2,
		View:     &daemon.ProbeView{Kind: daemon.ProbeViewWorktree, Exact: true},
	})

	if st := probeFileIndexScope("/wt", "internal/live.go"); !st.Indexed {
		t.Fatal("an exact covered answer must still deny")
	}
	if records := readDecisions(t, log); len(records) != 0 {
		t.Fatalf("decision log holds %d records for an exact answer, want none", len(records))
	}
}

// A daemon predating the Answered field still reports coverage truthfully, and
// daemons outlive the binary upgrade that starts them. Gating the deny on the
// absent field would switch enforcement off for that process's whole life —
// the exact silent bypass the field was added to prevent.
func TestFileCoverageWithoutTheAnsweredFieldStillDenies(t *testing.T) {
	startCoverageDaemon(t, daemon.FileCoverageResult{Covered: true, Symbols: 4})

	st := probeFileIndexScope("/wt", "internal/live.go")
	if !st.Indexed || st.Count != 4 {
		t.Fatalf("coverage = %+v, want an older daemon's answer honoured", st)
	}
	if st.noGraphAnswer() {
		t.Fatal("a covered file must never silence the read door")
	}
}

// TestProbeViaDaemonForwardsTheScope pins that the symbol probe tells the
// daemon where the search was issued from, which is what lets a worktree's
// grep be answered from its own composed view.
func TestProbeViaDaemonForwardsTheScope(t *testing.T) {
	ctrl := &searchScopeController{fakeController: &fakeController{}}
	startTestDaemon(t, ctrl)

	if _, err := probeViaDaemon("handleFoo", "/wt/internal", 2*time.Second); err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if got := ctrl.probedPath(); got != "/wt/internal" {
		t.Fatalf("daemon was told the scope was %q, want %q", got, "/wt/internal")
	}
}

// TestProbeViaDaemonLogsOnlyFallbackAnswers pins that the symbol probe records
// the same degradation the coverage probe does: a deny whose evidence came from
// a stand-in graph leaves a record naming that graph and the verb that answered.
func TestProbeViaDaemonLogsOnlyFallbackAnswers(t *testing.T) {
	log := redirectTelemetry(t)
	startTestDaemon(t, &searchScopeController{
		fakeController: &fakeController{},
		result: daemon.SearchSymbolsResult{
			Hits: []daemon.SymbolHit{{Name: "handleFoo", FilePath: "internal/live.go", Line: 9}},
			View: &daemon.ProbeView{
				Kind:           daemon.ProbeViewBase,
				Exact:          false,
				FallbackReason: "availability_grace",
			},
		},
	})

	hits, err := probeViaDaemon("handleFoo", "/wt/internal", 2*time.Second)
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "handleFoo" {
		t.Fatalf("hits = %+v, want the fallback answer honoured unchanged", hits)
	}
	records := readDecisions(t, log)
	if len(records) != 1 {
		t.Fatalf("decision log holds %d records, want the one fallback record", len(records))
	}
	if records[0].Decision != DecisionViewFallback {
		t.Fatalf("logged decision = %q, want %q", records[0].Decision, DecisionViewFallback)
	}
	if records[0].Tool != daemon.ControlSearchSymbols {
		t.Fatalf("logged tool = %q, want the verb that answered %q", records[0].Tool, daemon.ControlSearchSymbols)
	}
	if records[0].View != daemon.ProbeViewBase+"/availability_grace" {
		t.Fatalf("logged view = %q, want the graph and the reason it stood in", records[0].View)
	}
}

func TestProbeViaDaemonLogsNothingForAnExactAnswer(t *testing.T) {
	log := redirectTelemetry(t)
	startTestDaemon(t, &searchScopeController{
		fakeController: &fakeController{},
		result: daemon.SearchSymbolsResult{
			Hits: []daemon.SymbolHit{{Name: "handleFoo", FilePath: "internal/live.go", Line: 9}},
			View: &daemon.ProbeView{Kind: daemon.ProbeViewWorktree, Exact: true},
		},
	})

	hits, err := probeViaDaemon("handleFoo", "/wt/internal", 2*time.Second)
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the exact answer", hits)
	}
	if records := readDecisions(t, log); len(records) != 0 {
		t.Fatalf("decision log holds %d records for an exact answer, want none", len(records))
	}
}

// searchScopeController records the path a symbol probe carried and answers
// with a canned result.
type searchScopeController struct {
	*fakeController
	mu     sync.Mutex
	asked  string
	result daemon.SearchSymbolsResult
}

func (c *searchScopeController) SearchSymbols(
	_ context.Context, p daemon.SearchSymbolsParams,
) (daemon.SearchSymbolsResult, error) {
	c.mu.Lock()
	c.asked = p.Path
	c.mu.Unlock()
	return c.result, nil
}

func (c *searchScopeController) probedPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}
