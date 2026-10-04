package indexer

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Capture callbacks are installed by the selected-view owner, which retains
// its leases and applies file-mask visibility. The physical storage backend
// must not guess an ancestry, a carried actor, or a latest sibling checkout.
type contractCoreStorageCapture struct {
	Prior      func(context.Context, string) (*graph.ContractBoundaryReceipt, *graph.ContractBoundaryReceiptSource, bool, error)
	Dependents func(context.Context, []string) ([]contractCoreDependent, error)
}

// This backend is scoped to an already-serialized core apply interval. Header
// eligibility and file receipt acceptance are separate: bounded completed
// chunks may drain their receipts while the primary namespace remains blocked
// until its outer core/source acceptance fence.
type contractCoreStorageBackend struct {
	mu              sync.Mutex
	store           *store_sqlite.Store
	repo, checkout  string
	generation      int64
	capture         contractCoreStorageCapture
	state, expected *graph.ContractInputState
	headerPending   bool
	// A single later edit cannot certify an interrupted namespace's census.
	// It may publish core changes while contract reconciliation remains owed.
	recovering     bool
	stateInstalled bool
	staged         map[string]graph.ContractBoundaryReceipt
	sources        map[string]graph.ContractBoundaryReceiptSource
	cold           bool
	coldRows       []graph.ContractBoundaryReceipt
	coldSources    []graph.ContractBoundaryReceiptSource
	coldBytes      int
}

func newContractCoreStorageBackend(store *store_sqlite.Store, repo, checkout string, generation int64, cumulative, expected *graph.ContractInputState, capture contractCoreStorageCapture) (*contractCoreStorageBackend, error) {
	if store == nil || capture.Prior == nil || capture.Dependents == nil || generation < 0 || store.ViewGeneration() != generation {
		return nil, fmt.Errorf("contract core storage: incomplete selected capture")
	}
	copyState := func(state *graph.ContractInputState) *graph.ContractInputState {
		if state == nil {
			return nil
		}
		copy := *state
		return &copy
	}
	return &contractCoreStorageBackend{store: store, repo: repo, checkout: checkout, generation: generation,
		capture: capture, state: copyState(cumulative), expected: copyState(expected), stateInstalled: generation == 0 && expected != nil,
		recovering: generation == 0 && (expected == nil || !expected.Accepted),
		staged:     make(map[string]graph.ContractBoundaryReceipt), sources: make(map[string]graph.ContractBoundaryReceiptSource)}, nil
}

// The primary lane owns exactly physical0. Sparse builders supply their actual
// selected, leased predecessor callbacks instead of using this constructor.
func newPrimaryContractCoreStorageBackend(ctx context.Context, store *store_sqlite.Store, repo string) (*contractCoreStorageBackend, error) {
	if ctx == nil || store == nil || store.ViewGeneration() != 0 {
		return nil, fmt.Errorf("contract core storage: missing primary context/store")
	}
	state, found, err := store.ContractInputStateContext(ctx, repo, "")
	if err != nil {
		return nil, err
	}
	var expected *graph.ContractInputState
	if found {
		expected = &state
	}
	capture := contractCoreStorageCapture{
		Prior: func(ctx context.Context, path string) (*graph.ContractBoundaryReceipt, *graph.ContractBoundaryReceiptSource, bool, error) {
			row, known, err := store.ContractBoundaryReceiptForVersionContext(ctx, repo, "", path, contractBoundaryReceiptVersion)
			return row, nil, known, err
		},
		Dependents: func(ctx context.Context, keys []string) ([]contractCoreDependent, error) {
			rows, err := store.ContractBoundaryReceiptsForLookupKeysContext(ctx, keys, 4096)
			if err != nil {
				return nil, err
			}
			out := make([]contractCoreDependent, 0, len(rows))
			for _, row := range rows {
				out = append(out, contractCoreDependent{RepoPrefix: row.RepoPrefix, CheckoutID: row.CheckoutID,
					FilePath: row.FilePath, ReceiptVersion: row.Version, ReceiptFingerprint: row.Fingerprint,
					SourceFingerprint: row.SourceFingerprint, Accepted: row.Accepted, Scope: row.Scope})
			}
			return out, nil
		},
	}
	return newContractCoreStorageBackend(store, repo, "", 0, expected, expected, capture)
}

func (b *contractCoreStorageBackend) PriorBoundaryReceipt(ctx context.Context, path string) (*contractBoundaryReceipt, bool, error) {
	row, source, known, err := b.capture.Prior(ctx, path)
	if err != nil {
		return nil, false, err
	}
	if source != nil {
		b.mu.Lock()
		b.sources[path] = *source
		b.mu.Unlock()
	}
	return contractCorePriorReceipt(row, known)
}

func (b *contractCoreStorageBackend) BoundaryDependents(ctx context.Context, keys []string) ([]contractCoreDependent, error) {
	return b.capture.Dependents(ctx, keys)
}

func (b *contractCoreStorageBackend) BeginBoundaryMutation(ctx context.Context, changes []contractCoreInputChange) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cold {
		for _, change := range changes {
			row, e := contractCoreStoredReceipt(b.repo, b.checkout, change)
			if e != nil {
				return e
			}
			if len(b.coldRows) > 0 && (len(b.coldRows) >= 64 || b.coldBytes+len(row.Payload) > contractCoreReceiptPayloadLimit) {
				if e := b.flushColdRows(ctx); e != nil {
					return e
				}
			}
			b.coldRows = append(b.coldRows, row)
			b.coldBytes += len(row.Payload)
			if source, ok := b.sources[change.FilePath]; ok {
				b.coldSources = append(b.coldSources, source)
				delete(b.sources, change.FilePath)
			}
		}
		return nil
	}
	rows := make([]graph.ContractBoundaryReceipt, 0, len(changes))
	bounded := append([]contractCoreInputChange(nil), changes...)
	var sources []graph.ContractBoundaryReceiptSource
	for i, change := range changes {
		row, err := contractCoreStoredReceipt(b.repo, b.checkout, change)
		if err != nil {
			return err
		}
		if slices.Contains(row.Scope.Causes, "contract_receipt_envelope_exceeded") {
			bounded[i].Delta.Scope = row.Scope
			bounded[i].Current = nil
			bounded[i].Uncertainty = "contract_receipt_envelope_exceeded"
			bounded[i].Delta.ChangedProducedKeys = nil
		}
		rows = append(rows, row)
		if source, ok := b.sources[change.FilePath]; ok {
			sources = append(sources, source)
		}
	}
	next, changed, err := nextContractCoreInputState(b.state, b.repo, b.checkout, bounded)
	if err != nil {
		return err
	}
	var work []graph.ContractWork
	if changed {
		work, err = contractCoreWorkForChanges(b.generation, next, bounded)
		if err != nil {
			return err
		}
		if len(work) == 0 {
			// A missing cumulative baseline is not a certified zero registry.
			work, err = contractCoreWorkForChanges(b.generation, next, []contractCoreInputChange{{FilePath: b.repo + "/", Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"namespace_baseline_unknown"}}}}})
			if err != nil {
				return err
			}
		}
	}
	if b.generation == 0 {
		if changed {
			err = b.store.BeginContractInputMutationWithReceiptsContext(ctx, b.expected, next, work, rows)
		} else {
			err = b.store.SetContractBoundaryReceiptsContext(ctx, rows)
		}
	} else {
		for i := range rows {
			rows[i].Accepted = true
		}
		next.Accepted = true
		if changed || !b.stateInstalled {
			err = b.store.SetContractInputStateWithWorkReceiptsAndSourcesContext(ctx, b.expected, next, work, rows, sources)
		} else {
			err = b.store.SetContractBoundaryReceiptsWithSourcesContext(ctx, rows, sources)
		}
	}
	if err != nil {
		return err
	}
	if changed || !b.stateInstalled {
		b.state, b.expected = &next, &next
		b.headerPending = b.generation == 0
		b.stateInstalled = true
	}
	for _, row := range rows {
		// Durable rows own payloads, lookup memberships and the old receipt.
		// The cold foreground retains only a compact acceptance identity.
		row.Payload, row.LookupKeys, row.ProducedKeys, row.Previous = nil, nil, nil, nil
		row.Scope = graph.ContractWorkScope{}
		b.staged[row.FilePath] = row
		delete(b.sources, row.FilePath)
	}
	return nil
}

// Called only after the corresponding existing core graph/source receipt
// fence. It bounds retained payload memory without certifying namespace-ready.
func (b *contractCoreStorageBackend) AcceptBoundaryFiles(ctx context.Context, paths []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acceptBoundaryFiles(ctx, paths)
}

func (b *contractCoreStorageBackend) acceptBoundaryFiles(ctx context.Context, paths []string) error {
	paths = appendUniqueSorted(nil, paths...)
	for start := 0; start < len(paths); start += 64 {
		chunk := paths[start:min(start+64, len(paths))]
		stored, err := b.store.ContractBoundaryReceiptsForPathsContext(ctx, b.repo, b.checkout, chunk)
		if err != nil {
			return err
		}
		rows := make([]graph.ContractBoundaryReceipt, 0, len(chunk))
		for _, path := range chunk {
			expected, staged := b.staged[path]
			if !staged {
				continue
			}
			actual := stored[path]
			if actual == nil || actual.Version != expected.Version || actual.Fingerprint != expected.Fingerprint ||
				actual.SourceFingerprint != expected.SourceFingerprint || actual.Accepted != expected.Accepted || actual.Deleted != expected.Deleted {
				return fmt.Errorf("contract core storage: staged receipt changed before acceptance: %s", path)
			}
			if !actual.Accepted {
				rows = append(rows, *actual)
			}
		}
		if err := b.store.AcceptContractBoundaryReceiptsContext(ctx, rows); err != nil {
			return err
		}
		for _, path := range chunk {
			delete(b.staged, path)
		}
	}
	return nil
}

func (b *contractCoreStorageBackend) AcceptBoundaryMutation(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.flushColdRows(ctx); err != nil {
		return err
	}
	paths := make([]string, 0, len(b.staged))
	for path := range b.staged {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if err := b.acceptBoundaryFiles(ctx, paths); err != nil {
		return err
	}
	if b.headerPending && !b.recovering {
		if err := b.store.AcceptContractInputMutationWithReceiptsContext(ctx, *b.expected, nil); err != nil {
			return err
		}
		b.expected.Accepted = true
		b.headerPending = false
	}
	return nil
}

// A full pass blocks the namespace once BEFORE reset/drain/core writes. Its
// optional receipts then flush in bounded chunks rather than a state transaction
// per parsed file. The initial unknown scope covers additions and removals even
// if cancellation prevents the final receipt chunk from being persisted.
func (b *contractCoreStorageBackend) beginFullCoreNamespace(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cold {
		return nil
	}
	changes := []contractCoreInputChange{{FilePath: b.repo + "/", Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"full_core_census_pending"}}}}}
	next, _, e := nextContractCoreInputState(b.state, b.repo, b.checkout, changes)
	if e != nil {
		return e
	}
	work, e := contractCoreWorkForChanges(b.generation, next, changes)
	if e != nil {
		return e
	}
	if b.generation == 0 {
		e = b.store.BeginContractInputMutationContext(ctx, b.expected, next, work)
		b.headerPending = true
	} else {
		next.Accepted = true
		e = b.store.SetContractInputStateWithWorkContext(ctx, b.expected, next, work)
	}
	if e != nil {
		return e
	}
	b.state = &next
	b.expected = &next
	b.stateInstalled = true
	b.cold = true
	return nil
}
func (b *contractCoreStorageBackend) flushColdRows(ctx context.Context) error {
	if len(b.coldRows) == 0 {
		return nil
	}
	rows := b.coldRows
	var e error
	if b.generation == 0 {
		e = b.store.SetContractBoundaryReceiptsContext(ctx, rows)
	} else {
		for i := range rows {
			rows[i].Accepted = true
		}
		e = b.store.SetContractBoundaryReceiptsWithSourcesContext(ctx, rows, b.coldSources)
	}
	if e != nil {
		return e
	}
	for _, row := range rows {
		row.Payload = nil
		row.LookupKeys = nil
		row.ProducedKeys = nil
		row.Previous = nil
		row.Scope = graph.ContractWorkScope{}
		b.staged[row.FilePath] = row
	}
	b.coldRows = nil
	b.coldSources = nil
	b.coldBytes = 0
	return nil
}
