package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestContractCoreIndexAndHTTPMutationDoNotWaitForHeldAnalysis(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "routes.go")
	source := `package fixture
func register(r Router) { r.GET("/before", users) }
func users() { before() }
func before() {}
func after() {}
`
	writeFile(t, path, source)
	g := graph.New()
	probe := &contractHydrationProbe{Graph: g}
	idx := newTestIndexer(probe)
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	held := make(chan struct{})
	started := make(chan struct{})
	analysisDone := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held) }) }
	t.Cleanup(release)
	backend := &contractCoreBackendProbe{known: true, receipts: make(map[string]*contractBoundaryReceipt), pendingReceipts: make(map[string]*contractBoundaryReceipt)}
	backend.onAccept = func() {
		startOnce.Do(func() {
			go func() {
				close(started)
				<-held
				// Execute the existing real contract pass after the gate. This fixture
				// coalesces to the latest accepted source; production historical workers
				// additionally need the durable receipt/source eligibility protocol.
				analysis := newTestIndexer(g)
				analysis.SetRepoPrefix("fixture")
				analysis.storeRootPath(root)
				analysis.extractContracts()
				analysis.Close()
				close(analysisDone)
			}()
		})
	}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	idx.contractCoreInputs = journal
	initial := make(chan error, 1)
	go func() { _, err := idx.Index(root); initial <- err }()
	select {
	case err := <-initial:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial core index waited for held contract analysis")
	}
	<-started
	require.NotEmpty(t, backend.begun)
	require.NotEmpty(t, backend.begun[0][0].Delta.Scope.Groups, "cold interface work is pending, not a clean empty baseline")
	require.Nil(t, idx.ContractRegistry())
	require.Zero(t, probe.ownerReads)
	require.Zero(t, probe.scalarReads)
	require.Empty(t, g.NodesByKinds([]graph.NodeKind{graph.KindContract}))
	require.Nil(t, idx.contractInputWitness, "the core-only path has no contract publication witness")
	callers := g.FindNodesByNameInRepo("users", "fixture")
	require.Len(t, callers, 1)
	before := g.FindNodesByNameInRepo("before", "fixture")
	require.Len(t, before, 1)
	hasCall := func(from, to string) bool {
		for _, edge := range g.GetOutEdges(from) {
			if edge.Kind == graph.EdgeCalls && edge.To == to {
				return true
			}
		}
		return false
	}
	require.True(t, hasCall(callers[0].ID, before[0].ID), "ordinary internal calls resolve before analysis")

	changed := strings.Replace(strings.Replace(source, "/before", "/after", 1), "users() { before() }", "users() { after() }", 1)
	writeFile(t, path, changed)
	edited := make(chan error, 1)
	go func() { edited <- idx.IndexFile(path) }()
	select {
	case err := <-edited:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP change waited for held contract analysis")
	}
	require.Equal(t, 2, backend.accepted)
	last := backend.begun[len(backend.begun)-1][0]
	require.Contains(t, last.Delta.Scope.Causes, "local_boundary_changed")
	require.Len(t, last.Delta.Scope.Groups, 2, "old endpoint removal and new endpoint both remain owed")
	after := g.FindNodesByNameInRepo("after", "fixture")
	require.Len(t, after, 1)
	require.True(t, hasCall(callers[0].ID, after[0].ID))
	require.False(t, hasCall(callers[0].ID, before[0].ID), "core call graph cannot retain the old call")
	require.False(t, idx.IsStale("routes.go"), "ordinary accepted source receipt is current")
	require.Nil(t, idx.ContractRegistry())
	require.Zero(t, idx.contractRegistryLoads)
	require.Zero(t, idx.contractRegistrySeedCalls)
	require.Zero(t, probe.ownerReads)
	require.Zero(t, probe.scalarReads)
	select {
	case <-analysisDone:
		t.Fatal("analysis gate unexpectedly opened")
	default:
	}
	release()
	select {
	case <-analysisDone:
	case <-time.After(5 * time.Second):
		t.Fatal("released contract analysis did not finish")
	}
	records := contracts.LoadRegistryFromGraph(g, "fixture").ByRepo("fixture")
	require.NotEmpty(t, records)
	foundAfter := false
	for _, record := range records {
		require.NotContains(t, record.ID, "/before")
		if strings.Contains(record.ID, "/after") {
			foundAfter = true
		}
	}
	require.True(t, foundAfter, "released real analysis emits the changed endpoint")
}

// Removing only the optional accepted AST leaves normal core extraction
// usable, but makes the checked Go boundary collector refuse a certificate.
type coreOnlyGoExtractor struct{ parser.Extractor }

func (e coreOnlyGoExtractor) Extract(path string, source []byte) (*parser.ExtractionResult, error) {
	result, err := e.Extractor.Extract(path, source)
	if result != nil {
		result.ReleaseTree()
	}
	return result, err
}

func TestContractCoreCollectionFailurePreservesCoreAndOwesUnknownAnalysis(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "value.go"), "package fixture\nfunc Value() int { return 2 }\n")
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	idx.registry.Register(coreOnlyGoExtractor{Extractor: languages.NewGoExtractor()})
	backend := &contractCoreBackendProbe{known: true}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	idx.contractCoreInputs = journal
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Len(t, g.FindNodesByNameInRepo("Value", "fixture"), 1)
	require.False(t, idx.IsStale("value.go"))
	require.Equal(t, 1, backend.accepted)
	require.True(t, backend.begun[0][0].Delta.Scope.Unknown)
	require.Contains(t, backend.begun[0][0].Delta.Scope.Causes, "accepted_contract_extraction_incomplete")
	require.False(t, backend.begun[0][0].Deleted)
	require.Nil(t, idx.ContractRegistry())
}

func TestContractCoreColdWorkersDrainBoundedJournal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		writeFile(t, filepath.Join(root, name+".go"), "package fixture\nfunc "+strings.ToUpper(name)+"() {}\n")
	}
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	backend := &contractCoreBackendProbe{known: true}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	idx.contractCoreInputs = journal
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Len(t, backend.begun, 8)
	require.Equal(t, 1, backend.accepted)
	require.Empty(t, journal.changes, "cold workers must drain accepted source receipts by durable chunk")
	require.Empty(t, journal.begun, "outer acceptance releases the operation's journal inventory")
}

type firstProbeRefusedGoExtractor struct {
	parser.Extractor
	calls int
}

func (e *firstProbeRefusedGoExtractor) Extract(path string, source []byte) (*parser.ExtractionResult, error) {
	e.calls++
	if e.calls == 1 {
		return nil, errors.New("controlled speculative probe refusal")
	}
	return e.Extractor.Extract(path, source)
}

func TestContractCoreLegacyFallbackStagesBeforeMutationWithoutRegistry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "routes.go")
	writeFile(t, path, "package fixture\nfunc register(r Router) { r.GET(\"/before\", users) }\nfunc users() {}\n")
	probe := &contractHydrationProbe{Graph: graph.New()}
	idx := newTestIndexer(probe)
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	backend := &contractCoreBackendProbe{known: true, receipts: make(map[string]*contractBoundaryReceipt), pendingReceipts: make(map[string]*contractBoundaryReceipt)}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	idx.contractCoreInputs = journal
	_, err = idx.Index(root)
	require.NoError(t, err)
	extractor := &firstProbeRefusedGoExtractor{Extractor: languages.NewGoExtractor()}
	idx.registry.Register(extractor)
	writeFile(t, path, "package fixture\nfunc register(r Router) { r.GET(\"/after\", users) }\nfunc users() {}\nfunc Added() {}\n")
	refusal := errors.New("pending storage refused")
	backend.beginErr = refusal
	require.ErrorIs(t, idx.IndexFile(path), refusal)
	require.GreaterOrEqual(t, extractor.calls, 2, "exercise the real failed probe then direct legacy fallback")
	require.Empty(t, probe.FindNodesByNameInRepo("Added", "fixture"), "fallback must not evict/add before durable pending succeeds")
	require.Len(t, probe.FindNodesByNameInRepo("users", "fixture"), 1)
	require.Equal(t, 1, backend.accepted)
	require.Zero(t, probe.ownerReads)
	require.Zero(t, probe.scalarReads)
	backend.beginErr = nil
	require.NoError(t, idx.IndexFile(path))
	require.Len(t, probe.FindNodesByNameInRepo("Added", "fixture"), 1)
	require.Zero(t, idx.contractRegistryLoads)
	require.Nil(t, idx.ContractRegistry())
}

func TestContractCoreInertSaveRetainsCoreFastPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.go")
	source := "package fixture\nfunc Value() int { return 1 }\n"
	writeFile(t, path, source)
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	backend := &contractCoreBackendProbe{known: true, receipts: make(map[string]*contractBoundaryReceipt), pendingReceipts: make(map[string]*contractBoundaryReceipt)}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	idx.contractCoreInputs = journal
	_, err = idx.Index(root)
	require.NoError(t, err)
	writeFile(t, path, source)
	result, err := idx.IncrementalReindexPaths(root, []string{path})
	require.NoError(t, err)
	require.Equal(t, 1, result.DerivedInvalidation.InertFiles)
	require.Empty(t, result.DerivedInvalidation.Files, "inert source must not become a structural resolver frontier")
	require.Empty(t, backend.begun[len(backend.begun)-1][0].Delta.Scope.Causes)
	require.Zero(t, idx.contractRegistryLoads)
	require.Nil(t, idx.contractInputWitness)
}
