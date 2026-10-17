package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
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
	// logger receives the Debug begin/accept bracket lines. They time each
	// path's unaccepted window, the interval a contract read cannot prove.
	logger *zap.Logger
	// sources receives the exact bytes each prepared receipt fingerprints.
	sources *contractAcceptedSourceCache
}

// contractJournalLogPaths bounds the paths one bracket line carries.
const contractJournalLogPaths = 16

func contractJournalPathFields(paths []string) []zap.Field {
	shown := paths
	if len(shown) > contractJournalLogPaths {
		shown = shown[:contractJournalLogPaths]
	}
	return []zap.Field{zap.Int("count", len(paths)), zap.Strings("paths", shown)}
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
		j.sources.put(change.SourceFingerprint, source)
	}
	if !deleted && silentZeroExtraction(result) {
		// A parse that minted no symbols is a deterministic content failure:
		// the file is excluded as a failure envelope instead of certifying an
		// empty receipt, decided before begin so no second move follows.
		change.Uncertainty = contractFileIndexFailed
		if j.priorFailureEnvelope(ctx, path) {
			// Only a transition moves the key: a file still failing the same
			// way restates its envelope with no new contract cause.
			j.stage(change)
			return nil
		}
	} else if !deleted {
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
	j.stage(change)
	return nil
}

func (j *contractCoreInputJournal) stage(change contractCoreInputChange) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.changes[change.FilePath] = change
}

// contractFileIndexFailed is the uncertainty a failure envelope carries: the
// file's accepted state is a deterministic indexing failure, so contract
// analysis excludes the file instead of reading prior or partial facts.
const contractFileIndexFailed = "file_index_failed"

// errContractJournalWrite marks a refused journal begin. The core write that
// needed it was refused too, so the file's prior receipt still describes it
// and no failure envelope may stand in for it.
var errContractJournalWrite = errors.New("contract journal write refused")

// contractContentError marks a failure to read or extract a file's own bytes.
// Its message is the wrapped error's, so failure rows read the same.
type contractContentError struct{ err error }

func (e *contractContentError) Error() string { return e.err.Error() }
func (e *contractContentError) Unwrap() error { return e.err }

// contractContentFailure marks err, when set, as a content failure.
func contractContentFailure(err error) error {
	if err == nil {
		return nil
	}
	return &contractContentError{err: err}
}

// contractFailureEnvelopeEligible decides whether a recorded file failure is
// excluded as a failure envelope. In a cold door every failure qualifies: the
// namespace reset dropped the file's core, so its prior receipt no longer
// describes anything. Elsewhere only a deterministic content failure
// qualifies: a read or extraction failure of the file's own bytes, or a
// silent-zero parse. Anything else (a version race, a cancellation, a journal
// failure, a vanished file, an admission or path error) leaves the prior
// receipt to the retry, the caller or the deletion pass.
func contractFailureEnvelopeEligible(err error, cold bool) bool {
	if err == nil {
		return false
	}
	if cold {
		return true
	}
	switch {
	case errors.Is(err, errFileVersionChanged), errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, errContractJournalWrite),
		errors.Is(err, fs.ErrNotExist):
		return false
	}
	var content *contractContentError
	return errors.As(err, &content) || errors.Is(err, errSilentZeroExtraction)
}

// contractFailureEnvelopeSource is the source identity of an envelope whose
// bytes could not be read: stable per path, never equal to real bytes.
func contractFailureEnvelopeSource(path string) string {
	return contractInputHash([]byte(contractFileIndexFailed + ":" + path))
}

// contractReceiptIsFailureEnvelope reports whether a stored receipt (or the
// accepted predecessor of an in-flight one) is a failure envelope.
func contractReceiptIsFailureEnvelope(row *graph.ContractBoundaryReceipt) bool {
	if row == nil {
		return false
	}
	if !row.Accepted && row.Previous != nil {
		row = row.Previous
	}
	return !row.Deleted && slices.Contains(row.Scope.Causes, contractFileIndexFailed)
}

// contractCoreFailureEnvelopeReader is the optional backend capability that
// lets prepare see a prior failure envelope through the decoded-receipt API.
type contractCoreFailureEnvelopeReader interface {
	PriorFailureEnvelope(context.Context, string) (bool, error)
}

func (j *contractCoreInputJournal) priorFailureEnvelope(ctx context.Context, path string) bool {
	reader, ok := j.backend.(contractCoreFailureEnvelopeReader)
	if !ok {
		return false
	}
	failed, err := reader.PriorFailureEnvelope(ctx, path)
	return err == nil && failed
}

// stageFailureEnvelopes prepares and begins one failure envelope per path
// whose prior receipt is not already one. The caller's outer acceptance
// accepts them with the rest of the pass.
func (j *contractCoreInputJournal) stageFailureEnvelopes(ctx context.Context, paths []string) error {
	return j.stageFailureEnvelopesOver(ctx, paths, false)
}

// stageFailureEnvelopesOver is stageFailureEnvelopes; with over it stages an
// envelope even where the receipt under the path's row is one already. The
// stuck-input repair excludes a row left in flight over an envelope that
// way: only its own staging opens the window its accept then closes.
func (j *contractCoreInputJournal) stageFailureEnvelopesOver(ctx context.Context, paths []string, over bool) error {
	var staged []string
	for _, path := range appendUniqueSorted(nil, paths...) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !over && j.priorFailureEnvelope(ctx, path) && !j.coldNamespace() {
			continue
		}
		change := contractCoreInputChange{FilePath: path, SourceFingerprint: contractFailureEnvelopeSource(path), Uncertainty: contractFileIndexFailed}
		change.Delta.Scope = graph.ContractWorkScope{Unknown: true, Causes: []string{contractFileIndexFailed}}
		j.stage(change)
		staged = append(staged, path)
	}
	if len(staged) == 0 {
		return nil
	}
	return j.begin(staged)
}

func (j *contractCoreInputJournal) coldNamespace() bool {
	storage, ok := j.backend.(*contractCoreStorageBackend)
	return ok && storage.hasPendingColdNamespace()
}

// hasBegun reports whether path's receipt was already rewritten by this pass.
func (j *contractCoreInputJournal) hasBegun(path string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, begun := j.begun[path]
	return begun
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
			// A bounded opaque receipt cannot list its real memberships. Include
			// its indexed uncertainty sentinel without scanning any registry.
			keys := appendUniqueSorted([]string{contractOverflowLookupKey}, change.Delta.ChangedProducedKeys...)
			dependents, err := j.backend.BoundaryDependents(j.ctx, keys)
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
		if ctxErr := j.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %w", errContractJournalWrite, err)
	}
	begun := make([]string, 0, len(pending))
	for _, change := range pending {
		j.begun[change.FilePath] = struct{}{}
		delete(j.changes, change.FilePath)
		begun = append(begun, change.FilePath)
	}
	if j.logger != nil {
		j.logger.Debug("contract journal begin", contractJournalPathFields(begun)...)
		// One line per window this begin opened after the path's previous
		// window closed: the writer's quiet gap a contract job re-asks in.
		if gaps, ok := j.backend.(interface {
			takeWindowGaps() (string, []store_sqlite.ContractCoreWindowGap)
		}); ok {
			repo, opened := gaps.takeWindowGaps()
			for _, gap := range opened {
				j.logger.Info("contract receipt window", zap.String("repo", repo), zap.String("path", gap.Path), zap.Uint64("seq", gap.Seq), zap.String("window_process", contractWindowProcess), zap.String("checkout", gap.Checkout), zap.Int64("opened_at_ns", gap.OpenedAt.UnixNano()), zap.Int64("closed_at_ns", gap.ClosedAt.UnixNano()),
					zap.Float64("quiet_ms", contractMillis(gap.Quiet)), zap.Float64("open_prev_ms", contractMillis(gap.OpenPrev)))
			}
		}
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
		storage, ok := j.backend.(*contractCoreStorageBackend)
		if !ok || !storage.hasPendingColdNamespace() {
			return nil
		}
	}
	if err := j.backend.AcceptBoundaryMutation(j.ctx); err != nil {
		return err
	}
	if j.logger != nil {
		accepted := make([]string, 0, len(j.begun))
		for path := range j.begun {
			accepted = append(accepted, path)
		}
		sort.Strings(accepted)
		j.logger.Debug("contract journal accept", contractJournalPathFields(accepted)...)
	}
	j.begun = make(map[string]struct{})
	return nil
}

// written ends the core-write windows of the receipts this pass began: the
// pass's body returned, every core write of their files done.
func (j *contractCoreInputJournal) written() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.begun) == 0 {
		return
	}
	if ender, ok := j.backend.(interface{ EndBoundaryCoreWrites() }); ok {
		ender.EndBoundaryCoreWrites()
	}
	if j.logger != nil {
		written := make([]string, 0, len(j.begun))
		for path := range j.begun {
			written = append(written, path)
		}
		sort.Strings(written)
		j.logger.Debug("contract journal written", contractJournalPathFields(written)...)
	}
}

// orphan ends a pass whose body failed after it began receipts: their
// core-write windows stay open, since the body may have stopped between an
// evict and its add, and are marked orphaned so a repair can find them.
func (j *contractCoreInputJournal) orphan() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.begun) == 0 {
		return
	}
	if orphaner, ok := j.backend.(interface{ OrphanBoundaryCoreWrites() }); ok {
		orphaner.OrphanBoundaryCoreWrites()
	}
	if j.logger != nil {
		orphaned := make([]string, 0, len(j.begun))
		for path := range j.begun {
			orphaned = append(orphaned, path)
		}
		sort.Strings(orphaned)
		j.logger.Info("contract journal orphaned", contractJournalPathFields(orphaned)...)
	}
}

// settle ends a pass that completed its core writes without being accepted
// (a newer mutation took its owner over): its begun receipts stay unaccepted
// and are marked settled at their write seqs.
func (j *contractCoreInputJournal) settle() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.begun) == 0 {
		return
	}
	if settler, ok := j.backend.(interface{ SettleBoundaryMutation() }); ok {
		settler.SettleBoundaryMutation()
	}
}
