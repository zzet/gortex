package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
)

// floorCensusHandle is a generation handle that answers only the per-file
// language census the floor check reads.
type floorCensusHandle struct {
	graph.Store
	rows []graph.RepoLanguageFileCount
}

func (h floorCensusHandle) RepoLanguageFileCounts([]string) []graph.RepoLanguageFileCount {
	return h.rows
}

// TestAdmissionFloorReadsTheBaseCensusOnlyWhenOwnFilesFallShort pins the
// laziness of the committed-state census: counting it is a whole-generation
// scan per full generation beneath the chain (the entire plan of the first
// edit after a restart on a large store), so the floor check must not read it
// when the generation's own files and the chain already clear the floor for
// every language a provider serves, and must read it — and honour it — when
// they do not.
func TestAdmissionFloorReadsTheBaseCensusOnlyWhenOwnFilesFallShort(t *testing.T) {
	const floor = 16
	goOnly := func(language string) bool { return language == "go" }
	counting := func(counts map[string]int) (func() map[string]int, *int) {
		calls := 0
		return func() map[string]int { calls++; return counts }, &calls
	}
	row := func(file, language string, n int) graph.RepoLanguageFileCount {
		return graph.RepoLanguageFileCount{RepoPrefix: "r", FilePath: "r/" + file, Language: language, Count: n}
	}

	// A config file edit: plenty of Go nodes, two contract nodes. No provider
	// serves contracts, so they do not send the check to the base census.
	handle := floorCensusHandle{rows: []graph.RepoLanguageFileCount{row("config.go", "go", 458), row("config.go", "contract", 2)}}
	base, calls := counting(map[string]int{"go": 180000, "contract": 1})
	clears, read := chainClearsEnrichmentFloor(handle, "r", nil, base, floor, goOnly)
	if !clears || read || *calls != 0 {
		t.Fatalf("own Go nodes clear the floor: clears=%v baseRead=%v calls=%d, want true/false/0", clears, read, *calls)
	}

	// Judging every language (no provider filter), the contract language is
	// short and only the base census can decide: it is read exactly once.
	clears, read = chainClearsEnrichmentFloor(handle, "r", nil, base, floor, nil)
	if clears || !read || *calls != 1 {
		t.Fatalf("unfiltered contract language: clears=%v baseRead=%v calls=%d, want false/true/1", clears, read, *calls)
	}

	// A small edit: 5 own Go nodes plus 5 in the chain stay short; the base
	// census decides, and it clears.
	small := floorCensusHandle{rows: []graph.RepoLanguageFileCount{row("tiny.go", "go", 5)}}
	chain := map[string]map[string]int{"r/other.go": {"go": 5}}
	base, calls = counting(map[string]int{"go": 180000})
	clears, read = chainClearsEnrichmentFloor(small, "r", chain, base, floor, goOnly)
	if !clears || !read || *calls != 1 {
		t.Fatalf("short edit over a Go checkout: clears=%v baseRead=%v calls=%d, want true/true/1", clears, read, *calls)
	}

	// The chain alone lifts the small edit over the floor: no base read.
	bigChain := map[string]map[string]int{"r/other.go": {"go": 40}}
	base, calls = counting(map[string]int{"go": 180000})
	clears, read = chainClearsEnrichmentFloor(small, "r", bigChain, base, floor, goOnly)
	if !clears || read || *calls != 0 {
		t.Fatalf("chain clears the floor: clears=%v baseRead=%v calls=%d, want true/false/0", clears, read, *calls)
	}

	// No base census at all: a short language keeps the floor.
	if clears, read := chainClearsEnrichmentFloor(small, "r", chain, nil, floor, goOnly); clears || read {
		t.Fatalf("short edit without a base census: clears=%v baseRead=%v, want false/false", clears, read)
	}

	// A generation carrying only languages no provider serves does not clear.
	contractOnly := floorCensusHandle{rows: []graph.RepoLanguageFileCount{row("api.yaml", "contract", 40)}}
	if clears, _ := chainClearsEnrichmentFloor(contractOnly, "r", nil, nil, floor, goOnly); clears {
		t.Fatal("a generation with no enrichable language cleared the floor")
	}
}

// TestWorkingTreeBuildReadsTheCommittedCensusOnlyWhenItsOwnFilesFallShort is
// the coordinator half: a one-unit edit (below the floor on its own, no chain
// yet) reads the committed state's census and runs the Go pass because the
// checkout is a Go checkout; an edit whose own files clear the floor never
// reads it, and still runs the pass.
func TestWorkingTreeBuildReadsTheCommittedCensusOnlyWhenItsOwnFilesFallShort(t *testing.T) {
	f, c, _ := semanticChainFixtureWith(t, semanticBindingTree(), false)
	coordinatorReconcile(t, c)
	censusEntries := func() int {
		c.compaction.mu.Lock()
		defer c.compaction.mu.Unlock()
		return len(c.compaction.census)
	}

	semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, 0, true)
	one := coordinatorReconcile(t, c)
	if !goTypesRan(one.DirtyWork) {
		t.Fatalf("one-unit edit ran no go/types pass: %+v", one.DirtyWork.CompilerContext)
	}
	if got := censusEntries(); got != 1 {
		t.Fatalf("one-unit edit: %d committed census entries, want 1 (its own nodes are below the floor)", got)
	}

	c.compaction.mu.Lock()
	clear(c.compaction.census)
	c.compaction.mu.Unlock()
	for i := 1; i < 9; i++ {
		semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, i, true)
	}
	many := coordinatorReconcile(t, c)
	if !goTypesRan(many.DirtyWork) {
		t.Fatalf("eight-unit edit ran no go/types pass: %+v", many.DirtyWork.CompilerContext)
	}
	if got := censusEntries(); got != 0 {
		t.Fatalf("eight-unit edit read the committed census (%d entries); its own files clear the floor", got)
	}
}

func TestEnrichmentRefusesACanceledCommittedCensus(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "16")
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "census.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AddBatchChecked([]*graph.Node{{ID: "r/f.go::F", Name: "F", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/f.go", Language: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &retainingRepoProvider{retained: map[string]struct{}{}, leased: map[string]struct{}{}}
	manager := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	manager.RegisterProvider(provider)
	t.Cleanup(func() { _ = manager.Close() })
	b := &SparseGenerationBuilder{Semantic: manager, Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := 0
	req := BuildRequest{RepoPrefix: "r", Identity: GenerationIdentity{OwnerKind: checkoutLayerOwnerKind}, Enrich: &EnrichmentStage{
		BaseCensusFunc: func(readCtx context.Context) (map[string]int, error) { called++; cancel(); return nil, readCtx.Err() },
	}}
	report := &BuildReport{}
	b.runEnrichment(ctx, req, s, report)
	if called != 1 || !strings.Contains(report.Enrichment.Reason, context.Canceled.Error()) || report.Enrichment.FloorFromChain || len(report.Enrichment.Ran) > 0 {
		t.Fatalf("canceled census: calls=%d outcome=%+v", called, report.Enrichment)
	}
	if provider.retains != 0 {
		t.Fatal("provider ran after failed committed census")
	}
}
