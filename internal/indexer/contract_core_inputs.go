package indexer

import (
	"context"
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// A journal is scoped to one already-coordinated core mutation/build. It is
// supplied only by the complete asynchronous maintenance runtime; nil preserves
// the legacy path while its storage/consumer foundations are being qualified.
// It does not load a registry, match contracts or resolve graph-wide names.
type contractCoreInputJournal struct {
	mu      sync.Mutex
	ctx     context.Context
	backend contractCoreInputBackend
	changes map[string]contractCoreInputChange
	begun   map[string]struct{}
}

// Backend methods perform bounded selected-view receipt/dependency reads and
// durable pending/acceptance writes. Begin MUST leave contract analysis blocked
// before the caller makes its first ordinary graph mutation. Accept is called
// only after the outer source/application acceptance fence has succeeded.
type contractCoreInputBackend interface {
	PriorBoundaryReceipt(context.Context, string) (*contractBoundaryReceipt, bool, error)
	BoundaryDependents(context.Context, []string) ([]contractCoreDependent, error)
	BeginBoundaryMutation(context.Context, []contractCoreInputChange) error
	AcceptBoundaryMutation(context.Context) error
}

type contractCoreInputChange struct {
	FilePath          string
	Deleted           bool
	Dependents        []contractCoreDependent
	Prior, Current    *contractBoundaryReceipt
	Delta             contractBoundaryDelta
	SourceFingerprint string
	// A contract-only error is an uncertainty receipt, not a failed core parse.
	Uncertainty string
}

// The owner/version stays explicit through staging. A global-name or matcher
// key can affect another repository/checkout; its debt cannot be merged into
// the changed source's actor scope or silently acknowledged by that actor.
type contractCoreDependent struct {
	RepoPrefix, CheckoutID, FilePath                      string
	ReceiptVersion, ReceiptFingerprint, SourceFingerprint string
	Accepted                                              bool
	Scope                                                 graph.ContractWorkScope
}

func newContractCoreInputJournal(ctx context.Context, backend contractCoreInputBackend) (*contractCoreInputJournal, error) {
	if ctx == nil || backend == nil {
		return nil, fmt.Errorf("contract core inputs: missing context/backend")
	}
	return &contractCoreInputJournal{ctx: ctx, backend: backend, changes: make(map[string]contractCoreInputChange), begun: make(map[string]struct{})}, nil
}

func (j *contractCoreInputJournal) prepare(ctx context.Context, idx *Indexer, path, language string, source []byte, result *parser.ExtractionResult, deleted bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prior, known, priorErr := j.backend.PriorBoundaryReceipt(ctx, path)
	if err := ctx.Err(); err != nil {
		return err
	}
	change := contractCoreInputChange{FilePath: path, Prior: prior, Deleted: deleted}
	if !deleted {
		change.SourceFingerprint = contractInputHash(source)
	}
	if !deleted {
		current, err := idx.collectContractBoundaryReceipt(ctx, path, language, source, result)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			change.Uncertainty = "accepted_contract_extraction_incomplete"
		} else {
			change.Current = &current
		}
	}
	change.Delta = diffContractBoundaryReceipts(prior, change.Current)
	if !known || priorErr != nil {
		change.Delta.Scope.Unknown = true
		change.Delta.Scope.Causes = appendUniqueSorted(change.Delta.Scope.Causes, "legacy_inputs_unknown")
	}
	if change.Uncertainty != "" {
		change.Delta.Scope.Unknown = true
		change.Delta.Scope.Causes = appendUniqueSorted(change.Delta.Scope.Causes, change.Uncertainty)
	}
	// Current=nil on contract uncertainty is not an actual source deletion.
	change.Delta.Scope.Deleted = deleted
	// Worker rereads are checked against the accepted source fingerprint.
	// The journal never retains a cold corpus of source bytes. If an older dirty
	// source has moved before followup, that analysis stays incomplete.
	j.mu.Lock()
	defer j.mu.Unlock()
	j.changes[path] = change
	return nil
}

// begin prepares exact pending work for the new chunks before applying any core
// writes, including a later accepted reparse of an already-staged file.
// Dependency failures are conservative async debt. A failed pending
// write is different: the caller must refuse the core write, since otherwise
// stale analysis could still be advertised as complete.
func (j *contractCoreInputJournal) begin(paths []string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.ctx.Err(); err != nil {
		return err
	}
	var pending []contractCoreInputChange
	for _, path := range appendUniqueSorted(nil, paths...) {
		change, ok := j.changes[path]
		if !ok {
			return fmt.Errorf("contract core inputs: missing accepted boundary input for %s", path)
		}
		if len(change.Delta.ChangedProducedKeys) > 0 {
			dependents, err := j.backend.BoundaryDependents(j.ctx, change.Delta.ChangedProducedKeys)
			if ctxErr := j.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				change.Delta.Scope.Unknown = true
				change.Delta.Scope.Causes = appendUniqueSorted(change.Delta.Scope.Causes, "dependency_membership_unknown")
			} else {
				change.Dependents = append(change.Dependents, dependents...)
			}
		}
		pending = append(pending, change)
	}
	if len(pending) == 0 {
		return nil
	}
	if err := j.backend.BeginBoundaryMutation(j.ctx, pending); err != nil {
		return err
	}
	for _, change := range pending {
		j.begun[change.FilePath] = struct{}{}
		delete(j.changes, change.FilePath)
	}
	return nil
}

func (j *contractCoreInputJournal) accept(coreErr error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	// A canceled/failed/superseded core never acknowledges its pending work.
	if coreErr != nil {
		return coreErr
	}
	if err := j.ctx.Err(); err != nil {
		return err
	}
	if len(j.begun) == 0 {
		return nil
	}
	if err := j.backend.AcceptBoundaryMutation(j.ctx); err != nil {
		return err
	}
	j.begun = make(map[string]struct{})
	return nil
}
