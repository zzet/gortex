package store_sqlite

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"unsafe"
)

// ContractProjectionDomain names potential query results, including absence.
// It is not a receipt, an answer, or permission to acknowledge contract work.
// A consumer must define its complete dependency domains before using exclusion.
// Scope is literal, including the empty string; it never implies all scopes.
type ContractProjectionDomain struct {
	Kind, Scope, Key string
}

// ContractProjectionCursor contains owned scalar identity only. In particular it
// retains no store, reader, lease, transaction or context. Its unexported fields
// can only be minted by a current store capture.
type ContractProjectionCursor struct {
	instance, analysis, input, bindings, admin uint64
	generation                                 int64
}

// ContractProjectionDelta is current committed history, not publication proof.
// Complete=false requires the original physical read proof; Domains must never
// be used when incomplete. Publication still needs a locked input witness.
type ContractProjectionDelta struct {
	After    ContractProjectionCursor
	Complete bool
	Domains  []ContractProjectionDomain
}

type contractProjectionMutation struct {
	complete bool
	domains  []ContractProjectionDomain
}

type contractProjectionEvent struct {
	generation                                  int64
	analysis, input, bindings                   uint64
	beforeAnalysis, beforeInput, beforeBindings uint64
	complete                                    bool
	domains                                     []ContractProjectionDomain
	bytes                                       int64
}

type contractProjectionHistory struct {
	instance, budget, retained uint64
	slots                      []contractProjectionEvent
	start, count               int
}

var contractProjectionInstances atomic.Uint64

// EnableContractProjectionHistory starts bounded process-local data history.
// The caller must reserve budgetBytes from the EXISTING contract cache budget,
// including the reported retained amount. It must not add this to that budget.
// Existing history cannot be enlarged by a second caller. A different requested
// budget refuses, preserving all prior cursors. No graph behavior is changed.
func (s *Store) EnableContractProjectionHistory(ctx context.Context, budgetBytes int64) error {
	if ctx == nil || s == nil || s.coreless() || budgetBytes < 4096 || budgetBytes > 64<<20 {
		return fmt.Errorf("%w: contract proof history budget", ErrCatalogInvalidValue)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	if h := s.contractProjectionHistory; h != nil {
		if h.budget != uint64(budgetBytes) {
			return fmt.Errorf("%w: contract proof history budget differs", ErrCatalogInvalidValue)
		}
		return nil
	}
	instance := s.contractProjectionIdentityLocked()
	if instance == 0 {
		return fmt.Errorf("%w: contract proof instance exhausted", ErrCatalogInvalidValue)
	}

	// Ring storage is charged before allocation; the rest is for owned domains.
	count := min(256, int(budgetBytes/4/int64(unsafe.Sizeof(contractProjectionEvent{}))))
	base := uint64(unsafe.Sizeof(contractProjectionHistory{})) + uint64(count)*uint64(unsafe.Sizeof(contractProjectionEvent{}))
	if count < 1 || base > uint64(budgetBytes) {
		return fmt.Errorf("%w: contract proof history budget", ErrCatalogInvalidValue)
	}
	s.contractProjectionHistory = &contractProjectionHistory{instance: instance, budget: uint64(budgetBytes), retained: base, slots: make([]contractProjectionEvent, count)}
	return nil
}

// ContractProjectionHistoryBytes reports the actual retained data charge, with
// no traversal of graph rows. Disabled history has zero retained allocation.
func (s *Store) ContractProjectionHistoryBytes(ctx context.Context) (int64, error) {
	if ctx == nil || s == nil || s.coreless() {
		return 0, ErrCatalogInvalidValue
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.writeMu.Unlock()
	if h := s.contractProjectionHistory; h != nil {
		return int64(h.retained), nil
	}
	return 0, nil
}

// CaptureContractProjectionCursor samples this exact physical generation's
// committed clocks under the existing write gate. It does not add generation0,
// infer selected ancestry, or certify a whole composed view.
func (s *Store) CaptureContractProjectionCursor(ctx context.Context) (ContractProjectionCursor, error) {
	if ctx == nil || s == nil || s.coreless() {
		return ContractProjectionCursor{}, ErrCatalogInvalidValue
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return ContractProjectionCursor{}, err
	}
	defer s.writeMu.Unlock()
	if s.contractProjectionHistory == nil {
		return ContractProjectionCursor{}, fmt.Errorf("%w: contract proof history is disabled", ErrCatalogInvalidValue)
	}
	return s.contractProjectionCursorLocked(), nil
}

func (s *Store) contractProjectionCursorLocked() ContractProjectionCursor {
	return ContractProjectionCursor{instance: s.contractProjectionInstance, generation: s.viewGen,
		analysis: s.analysisViewCounter(s.viewGen).Load(), input: s.constantInputCounter(s.viewGen).Load(), bindings: s.semanticBindingInputCounter(s.viewGen).Load(), admin: s.payloadInputAdminRevision.Load()}
}

// ContractProjectionChanges returns an owned description only when EVERY
// selected analysis revision is represented. Uninstrumented writers, evicted
// events, administration, input/mask/copy writes and invalid cursors fail closed.
// The latter input families will require their own exact descriptors before a
// consumer may carry proofs through them. Counter equality is not a claim that
// source bytes or a newly selected layer stayed the same.
func (s *Store) ContractProjectionChanges(ctx context.Context, from ContractProjectionCursor) (ContractProjectionDelta, error) {
	if ctx == nil || s == nil || s.coreless() {
		return ContractProjectionDelta{}, ErrCatalogInvalidValue
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return ContractProjectionDelta{}, err
	}
	defer s.writeMu.Unlock()
	h := s.contractProjectionHistory
	if h == nil {
		return ContractProjectionDelta{}, nil
	}
	after := s.contractProjectionCursorLocked()
	out := ContractProjectionDelta{After: after}
	if from.instance == 0 || from.instance != after.instance || from.generation != after.generation || from.analysis > after.analysis || from.input > after.input || from.bindings > after.bindings || from.admin != after.admin {
		return out, nil
	}
	if from.analysis == after.analysis && from.input == after.input && from.bindings == after.bindings {
		out.Complete = true
		return out, nil
	}
	currentAnalysis, currentInput, currentBindings := from.analysis, from.input, from.bindings
	domainCount := 0
	for i := 0; i < h.count; i++ {
		e := h.slots[(h.start+i)%len(h.slots)]
		if e.generation != from.generation || e.analysis <= from.analysis && e.input <= from.input && e.bindings <= from.bindings {
			continue
		}
		if e.beforeAnalysis != currentAnalysis || e.beforeInput != currentInput || e.beforeBindings != currentBindings || !e.complete {
			return out, nil
		}
		domainCount += len(e.domains)
		currentAnalysis, currentInput, currentBindings = e.analysis, e.input, e.bindings
		if currentAnalysis == after.analysis && currentInput == after.input && currentBindings == after.bindings {
			// Only allocate after the complete chain is known, at exact capacity.
			out.Domains = make([]ContractProjectionDomain, domainCount)
			at := 0
			for j := 0; j <= i; j++ {
				entry := h.slots[(h.start+j)%len(h.slots)]
				if entry.generation != from.generation || entry.analysis <= from.analysis && entry.input <= from.input && entry.bindings <= from.bindings {
					continue
				}
				for _, d := range entry.domains {
					out.Domains[at] = ContractProjectionDomain{Kind: strings.Clone(d.Kind), Scope: strings.Clone(d.Scope), Key: strings.Clone(d.Key)}
					at++
				}
			}
			out.Complete = true
			return out, nil
		}
	}

	return out, nil
}

// Every normal graph commit calls this through finishAnalysisMutationLocked.
// Unknown is the default: adding a new mutation method never silently declares
// it unrelated to contract proofs. Callers supply complete domains only after
// recording old/new names, owners and edge endpoints for their actual writes.
func (s *Store) noteContractProjectionMutationLocked(generation int64, revision uint64, change contractProjectionMutation) {
	if s.contractProjectionHistory == nil {
		return
	}
	s.recordContractProjectionMutationLocked(generation, revision, s.constantInputCounter(generation).Load(), s.semanticBindingInputCounter(generation).Load(), false, change)
}

func (s *Store) noteContractProjectionBindingMutationLocked(generation int64, revision uint64, change contractProjectionMutation) {
	if s.contractProjectionHistory == nil {
		return
	}
	s.recordContractProjectionMutationLocked(generation, s.analysisViewCounter(generation).Load(), s.constantInputCounter(generation).Load(), revision, true, change)
}

func (s *Store) recordContractProjectionMutationLocked(generation int64, analysis, input, bindings uint64, bindingChanged bool, change contractProjectionMutation) {
	h := s.contractProjectionHistory
	e := contractProjectionEvent{generation: generation, analysis: analysis, input: input, bindings: bindings, beforeAnalysis: analysis, beforeInput: input, beforeBindings: bindings}
	revision := analysis
	if bindingChanged {
		revision = bindings
		e.beforeBindings--
	} else {
		e.beforeAnalysis--
	}
	width := uint64(unsafe.Sizeof(ContractProjectionDomain{}))
	valid := change.complete && len(change.domains) > 0 && revision > 0 && uint64(len(change.domains)) <= h.budget/width
	bytes := uint64(0)
	if valid {
		bytes = uint64(len(change.domains)) * width
	}
	if valid {
		for _, d := range change.domains {
			n := uint64(len(d.Kind)) + uint64(len(d.Scope)) + uint64(len(d.Key))
			if !contractProjectionDomainValid(d) || n > h.budget || bytes > h.budget-n {
				valid = false
				break
			}
			bytes += n
		}
	}
	// No owned strings/slices are allocated until the whole descriptor fits.
	base := uint64(unsafe.Sizeof(contractProjectionHistory{})) + uint64(len(h.slots))*uint64(unsafe.Sizeof(contractProjectionEvent{}))
	if bytes > h.budget-base {
		valid = false
	}
	if valid {
		for h.count > 0 && (h.count == len(h.slots) || h.retained+bytes > h.budget) {
			h.dropOldest()
		}
		e.complete, e.bytes = true, int64(bytes)
		e.domains = make([]ContractProjectionDomain, len(change.domains))
		for i, d := range change.domains {
			e.domains[i] = ContractProjectionDomain{Kind: strings.Clone(d.Kind), Scope: strings.Clone(d.Scope), Key: strings.Clone(d.Key)}
		}
	}
	if h.count == len(h.slots) {
		h.dropOldest()
	}
	h.slots[(h.start+h.count)%len(h.slots)] = e
	h.count++
	h.retained += uint64(e.bytes)
}

func (h *contractProjectionHistory) dropOldest() {
	e := &h.slots[h.start]
	h.retained -= uint64(e.bytes)
	*e = contractProjectionEvent{}
	h.start = (h.start + 1) % len(h.slots)
	h.count--
}

func contractProjectionDomainValid(d ContractProjectionDomain) bool {
	if d.Key == "" {
		return false
	}
	switch d.Kind {
	case "file_nodes", "node_id", "name", "edge_file", "edge_endpoint", "seed_repo", "census_repo", "semantic_id", "semantic_file", "semantic_repo":
		return true
	}
	return false
}

// CaptureContractProjectionClock is an equality-only, owned scalar observation.
// It allocates no history and grants no domain exclusion, selected-view proof
// or publication authority. A changed tuple always requires fresh evidence.
func (s *Store) CaptureContractProjectionClock(ctx context.Context) (ContractProjectionCursor, error) {
	if ctx == nil || s == nil || s.coreless() {
		return ContractProjectionCursor{}, ErrCatalogInvalidValue
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return ContractProjectionCursor{}, err
	}
	defer s.writeMu.Unlock()
	if s.contractProjectionIdentityLocked() == 0 {
		return ContractProjectionCursor{}, ErrCatalogInvalidValue
	}
	return s.contractProjectionCursorLocked(), nil
}
func (s *Store) contractProjectionIdentityLocked() uint64 {
	if s.contractProjectionInstance == 0 {
		s.contractProjectionInstance = contractProjectionInstances.Add(1)
	}
	return s.contractProjectionInstance
}
