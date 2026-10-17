package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
)

// savingExtractor lands one editor save on path while the per-file pass
// extracts trigger, a file the same chunk parses after path. By then path's
// bytes were read, parsed and re-confirmed for the chunk, so the save lands
// between its read and the chunk's read-receipt check: the receipt is stale.
// With early set, it first lands another save on path while the pass
// extracts early, a file the chunk parses before path, so the bytes the pass
// then reads for path are already newer than the build's sample.
// Armed once; every other call is the plain extractor. With earlyEvery set,
// the early save instead lands at every extraction of early while it reports
// true, and earlySaves counts them.
type savingExtractor struct {
	parser.Extractor
	trigger, path string
	armed         atomic.Bool
	early         string
	earlyArmed    atomic.Bool
	earlyEvery    func() bool
	earlySaves    atomic.Int32
	// line is what the save appends to path ("// moved\n" when empty), and
	// onSave runs just before it. Both are set before the extractor is armed.
	line   string
	onSave func()

	mu      sync.Mutex
	savedAt time.Time
	err     error
	// reread is when each parse of path's saved (newer) bytes began.
	reread []time.Time
}

func (e *savingExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	if filepath.Base(filePath) == filepath.Base(e.path) && strings.Contains(string(src), "// moved") {
		e.mu.Lock()
		e.reread = append(e.reread, time.Now())
		e.mu.Unlock()
	}
	if e.early != "" && filepath.Base(filePath) == e.early &&
		(e.earlyArmed.CompareAndSwap(true, false) || (e.earlyEvery != nil && e.earlyEvery())) {
		e.earlySaves.Add(1)
		if err := appendToFile(e.path, "// early\n"); err != nil {
			e.mu.Lock()
			e.err = err
			e.mu.Unlock()
		}
	}
	if filepath.Base(filePath) == e.trigger && e.armed.CompareAndSwap(true, false) {
		if e.onSave != nil {
			e.onSave()
		}
		line := e.line
		if line == "" {
			line = "// moved\n"
		}
		err := appendToFile(e.path, line)
		e.mu.Lock()
		e.savedAt = time.Now()
		if e.err == nil {
			e.err = err
		}
		e.mu.Unlock()
	}
	return e.Extractor.Extract(filePath, src)
}

// appendToFile is an in-place editor save of path: line appended.
func appendToFile(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = file.WriteString(line)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (e *savingExtractor) saved() (time.Time, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.savedAt, e.err
}

// rereadsBefore counts the parses of path's saved bytes that began before at.
func (e *savingExtractor) rereadsBefore(at time.Time) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, began := range e.reread {
		if began.Before(at) {
			n++
		}
	}
	return n
}

// A save that lands on a file between its read and the chunk's read-receipt
// check, while a require_fresh ticket's chained working-tree delta is built
// under the daemon's contract-core runtime and an open build gate, tears that
// attempt and is rebuilt when the bytes the pass read were not the build's
// sample (an earlier save landed after the sample, before the read). It
// never fails the cycle or the ticket, and the settled tree is published
// within one more build. (When the bytes read are the sample's, the attempt
// is kept and published as the sample:
// TestASaveAfterTheParseIsPublishedAsTheBuildsSample.)
func TestASaveDuringAChainedDeltaParseTearsInsteadOfFailing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// tears is editDeltaVersionRaceTears: on, the pass reports the race
		// as a torn attempt; off, the engine retries the file in the same
		// generation and the contract journal must re-stage it.
		tears bool
	}{{"the pass tears the attempt", true}, {"the same-generation retry re-stages", false}} {
		t.Run(tc.name, func(t *testing.T) {
			was := editDeltaVersionRaceTears
			editDeltaVersionRaceTears = tc.tears
			t.Cleanup(func() { editDeltaVersionRaceTears = was })
			runSaveDuringChainedDeltaParse(t, tc.tears)
		})
	}
}

func runSaveDuringChainedDeltaParse(t *testing.T, tears bool) {
	f := newCoordinatorFixture(t)
	registry := builderRegistry()
	goExtractor, ok := registry.GetByLanguage("go")
	if !ok {
		t.Fatal("no Go extractor registered")
	}
	saver := &savingExtractor{Extractor: goExtractor, trigger: "island.go", early: "core.go", path: filepath.Join(f.worktree, "helper.go")}
	registry.Register(saver)
	core, logs := observer.New(zap.InfoLevel)
	builder := builderNewBuilder(f.store)
	builder.Registry = registry
	builder.Logger = zap.New(core)
	// The daemon installs the contract-core runtime on every builder.
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	gate := NewViewBuildGate()
	gate.Open()
	cycles := &motionFixtureCycles{}
	c := f.coordinator(t, CheckoutCoordinatorConfig{Builder: builder, Gate: gate, PollInterval: -1, cycleDone: cycles.record})
	c.Signal("initial")
	cycles.published(t, f, c, time.Time{}, 60*time.Second)

	// D1: helper.go's accepted contract receipt lives in a published
	// working-tree generation, the parent the next delta chains on and
	// carries the receipt from. caller.go keeps the next delta smaller than
	// the dirty set, so it chains.
	saved := time.Now()
	builderWriteFile(t, f.worktree, "caller.go", "package fixture\n\nfunc Run() {\n\tCompute(Options{})\n\t_ = 1\n}\n")
	builderWriteFile(t, f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tHelper()\n\tIsland()\n}\n")
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 1\n}\n")
	builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 1\n}\n")
	c.Signal("save")
	cycles.published(t, f, c, saved, 60*time.Second)
	parent := f.route().DirtyGenerationID
	if parent <= 0 {
		t.Fatal("the first save published no working-tree generation")
	}

	cycles.mu.Lock()
	mark := len(cycles.cycles)
	cycles.mu.Unlock()
	saver.armed.Store(true)
	// The pass parses core.go before helper.go: the early save there makes
	// the bytes it reads for helper.go newer than the build's sample.
	saver.earlyArmed.Store(true)
	edited := time.Now()
	builderWriteFile(t, f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tIsland()\n\tHelper()\n}\n")
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
	builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
	// A require_fresh request: the ticket is bound to the first state a sample
	// begun after its admission shows.
	lifecycle := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	ticket, err := lifecycle.requestBoundCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	var result MutationResult
	select {
	case result = <-ticket.Ticket.Done:
	case <-time.After(60 * time.Second):
		t.Fatal("the refresh ticket was never answered")
	}
	settled, saveErr := saver.saved()
	if saveErr != nil {
		t.Fatalf("the mid-pass save failed: %v", saveErr)
	}
	if settled.IsZero() {
		t.Fatal("the ticket's build never parsed island.go: the mid-pass save was not exercised")
	}
	cycles.mu.Lock()
	for _, out := range cycles.cycles[mark:] {
		if out.Err != nil {
			t.Errorf("cycle failed: %v", out.Err)
		}
	}
	cycles.mu.Unlock()
	if result.Err != nil {
		t.Fatalf("the refresh ticket failed: %v", result.Err)
	}
	took := cycles.published(t, f, c, settled, 5*time.Second)
	t.Logf("settled tree published %s after the mid-pass save", took)
	if took > 5*time.Second {
		t.Fatalf("the settled tree was published %s after the save, want within 5s", took)
	}

	torn := 0
	var firstTorn time.Time
	for _, entry := range logs.FilterMessage("indexer: working-tree build phases").All() {
		if !entry.Time.After(edited) {
			continue
		}
		fields := entry.ContextMap()
		message, _ := fields["error"].(string)
		if message == "" {
			continue
		}
		if strings.Contains(message, store_sqlite.ErrCatalogStaleGuard.Error()) {
			t.Errorf("a build hit the contract carry guard: %s", message)
		}
		if got, _ := fields["parent"].(int64); got != parent {
			t.Errorf("torn attempt built over parent %v, want the chained parent %d", fields["parent"], parent)
		}
		if strings.Contains(message, ErrDirtySnapshotChanged.Error()) {
			torn++
			if firstTorn.IsZero() {
				firstTorn = entry.Time
			}
		}
		// The torn attempt's generation left building for failed and is
		// owed a retirement, whichever arm tore it.
		generation, _ := fields["generation"].(int64)
		if row, found := f.generation(generation); !found || row.State != store_sqlite.ViewGenerationFailed {
			t.Errorf("torn generation %d is %q, want failed", generation, row.State)
		}
		c.mu.Lock()
		_, owed := c.backlog[generation]
		c.mu.Unlock()
		if !owed {
			t.Errorf("torn generation %d is not owed a retirement", generation)
		}
		if tears && !strings.Contains(message, errFileVersionChanged.Error()) {
			t.Errorf("the attempt was torn at the fence, not by the pass's version race: %s", message)
		}
	}
	if torn == 0 {
		t.Fatal("no attempt was torn by the mid-pass save")
	}
	// The torn attempt never parsed helper.go's newer bytes: tearing replaces
	// the same-generation retry rather than following it. With the retry
	// kept, the attempt re-parses them before the fence tears it.
	rereads := saver.rereadsBefore(firstTorn)
	if tears && rereads != 0 {
		t.Errorf("the torn attempt re-parsed the saved helper.go %d time(s) in its own generation", rereads)
	}
	if !tears && rereads == 0 {
		t.Error("the same-generation retry never re-parsed the saved helper.go")
	}
}
