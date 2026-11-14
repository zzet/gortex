package mcp

import (
	"errors"
	"os"
)

type projectionLivenessGuard uint8

const (
	projectionOwnerLive projectionLivenessGuard = iota
	projectionOwnerMissing
	projectionOwnerSuspectBefore
	projectionOwnerChangedBefore
	projectionOwnerStatError
	projectionOwnerStampChanged
	projectionOwnerTombstonePresent
	projectionOwnerNotRegular
	projectionOwnerSuspectAfter
	projectionOwnerChangedAfter
	projectionOwnerFactsConflict
	projectionOwnerFactUnavailable
)

func (guard projectionLivenessGuard) label() string {
	switch guard {
	case projectionOwnerMissing:
		return "missing_entry"
	case projectionOwnerSuspectBefore:
		return "suspect_before"
	case projectionOwnerChangedBefore:
		return "owner_before"
	case projectionOwnerStatError:
		return "stat_error"
	case projectionOwnerStampChanged:
		return "stamp_changed"
	case projectionOwnerTombstonePresent:
		return "tombstone_present"
	case projectionOwnerNotRegular:
		return "not_regular"
	case projectionOwnerSuspectAfter:
		return "suspect_after"
	case projectionOwnerChangedAfter:
		return "owner_after"
	case projectionOwnerFactsConflict:
		return "facts_conflict"
	case projectionOwnerFactUnavailable:
		return "fact_unavailable"
	default:
		return "unobserved"
	}
}

// These copied scalars describe the last original check actually executed,
// not the owner or event state at response serialization. In particular a
// suspect-after refusal has not executed the owner-after check. Source-event
// revisions require the registry mutex and are deliberately not sampled here.
// No entry, path, content, clock or additional liveness check is retained.
type acknowledgementProjectionFailure struct {
	Guard                string  `json:"guard"`
	Owner                string  `json:"owner"`
	OwnerCheck           string  `json:"owner_check"`
	Suspect              string  `json:"suspect"`
	SuspectCheck         string  `json:"suspect_check"`
	EntryCommitSeq       *uint64 `json:"entry_commit_seq,omitempty"`
	EntrySourceCommitSeq *uint64 `json:"entry_source_commit_seq,omitempty"`
	OwnerCommitSeq       *uint64 `json:"owner_commit_seq,omitempty"`
	OwnerSourceCommitSeq *uint64 `json:"owner_source_commit_seq,omitempty"`
}

func acknowledgementProjectionLiveness(entry *projectionEntry) (projectionLivenessGuard, *acknowledgementProjectionFailure) {
	if os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") != "1" {
		return projectionLivenessFailure(entry), nil
	}
	diagnostic := &acknowledgementProjectionFailure{
		Owner: "not_observed", OwnerCheck: "not_observed",
		Suspect: "not_observed", SuspectCheck: "not_observed",
	}
	if entry != nil {
		sequence, sourceSequence := entry.commitSeq, entry.sourceCommitSeq
		diagnostic.EntryCommitSeq, diagnostic.EntrySourceCommitSeq = &sequence, &sourceSequence
	}
	guard := observeProjectionLiveness(entry, diagnostic)
	if guard == projectionOwnerLive {
		return guard, nil // The successful response remains unchanged.
	}
	diagnostic.Guard = guard.label()
	return guard, diagnostic
}

func (diagnostic *acknowledgementProjectionFailure) observeSuspect(check string, suspect bool) {
	if diagnostic == nil {
		return
	}
	diagnostic.SuspectCheck, diagnostic.Suspect = check, "clear"
	if suspect {
		diagnostic.Suspect = "suspect"
	}
}

func (diagnostic *acknowledgementProjectionFailure) observeOwner(check string, entry, owner *projectionEntry) {
	if diagnostic == nil {
		return
	}
	diagnostic.OwnerCheck, diagnostic.Owner = check, "missing"
	diagnostic.OwnerCommitSeq, diagnostic.OwnerSourceCommitSeq = nil, nil
	if owner == nil {
		return
	}
	diagnostic.Owner = "same"
	if owner != entry {
		diagnostic.Owner = "replaced"
	}
	sequence, sourceSequence := owner.commitSeq, owner.sourceCommitSeq
	diagnostic.OwnerCommitSeq, diagnostic.OwnerSourceCommitSeq = &sequence, &sourceSequence
}

type projectionReadStage uint8

const (
	projectionReadValidation projectionReadStage = iota
	projectionReadInherited
	projectionReadBorrowed
	projectionReadRetain
	projectionReadSelection
	projectionReadDecoration
	projectionReadFinal
)

func (stage projectionReadStage) label() string {
	switch stage {
	case projectionReadInherited:
		return "bind.inherited_facts"
	case projectionReadBorrowed:
		return "bind.borrowed_fact"
	case projectionReadRetain:
		return "bind.retain"
	case projectionReadSelection:
		return "bind.final_validation"
	case projectionReadDecoration:
		return "response.decoration"
	case projectionReadFinal:
		return "response.final_validation"
	default:
		return "projection.validation"
	}
}

type projectionReadDiagnostic struct {
	cause error
	stage projectionReadStage
	guard projectionLivenessGuard
}

func (e *projectionReadDiagnostic) Error() string { return e.cause.Error() }
func (e *projectionReadDiagnostic) Unwrap() error { return e.cause }

// Capture a fixed predicate at its actual refusing branch, never by sampling
// the owner or disk again while formatting a response. The existing opt-in and
// raw response retention are shared with search; no request data is retained.
func annotateProjectionReadRefusal(stage projectionReadStage, guard projectionLivenessGuard, err error) error {
	if !errors.Is(err, projectionSuperseded) || os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") != "1" {
		return err
	}
	var previous *projectionReadDiagnostic
	if errors.As(err, &previous) {
		if previous.stage != projectionReadValidation || stage == projectionReadValidation {
			return err // Keep the first actual refusing site/predicate.
		}
		return &projectionReadDiagnostic{cause: err, stage: stage, guard: previous.guard}
	}
	return &projectionReadDiagnostic{cause: err, stage: stage, guard: guard}
}
