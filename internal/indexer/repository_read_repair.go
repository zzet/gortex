package indexer

import (
	"crypto/sha1" // Git blob identity, not a security decision.
	"encoding/hex"
	"path/filepath"
	"strconv"
	"time"
	"weak"

	"github.com/zzet/gortex/internal/pathkey"
)

const repositoryReadRepairCauseCap = 8

// ReadRepairObserver requests one bounded observation of an already-needed
// repository reconcile. It cannot change its paths, scheduling or ownership.
type ReadRepairObserver struct {
	Session      string
	RequestIDKey string
	Complete     func(ReadRepairObservation)
}

// A reader is an admission cause, never the owner of the work performed by
// drain. In particular a session may reuse a wire ID during the same drain.
// The existing waiter generation distinguishes those admissions.
type repositoryReadRepairCause struct {
	session             string
	requestIDKey        string
	admissionGeneration uint64
}

type repositoryReadRepairRequest struct {
	session      string
	requestIDKey string
	complete     func(ReadRepairObservation)
}

// ReadRepairObservation is a value copy scoped to one coordinator
// incarnation. Its generation belongs to the executed drain, not any caller.
// This independent executor window is not additive with a receipt's quiet
// wait, acknowledgement, background window or later no-op watcher attempt.
type ReadRepairObservation struct {
	generation     uint64
	path           string
	started        time.Time
	finished       time.Time
	causes         [repositoryReadRepairCauseCap]repositoryReadRepairCause
	causeCount     int
	causesComplete bool
	// Execution success alone is not content-publication proof. A later
	// accepted-read receipt must still match the pending commit's exact bytes.
	executionSucceeded bool
	acceptedBlobSHA    string
	acceptedAt         time.Time
	contentAmbiguous   bool
	// Execution supplies the concrete owner; a prefix lookup at completion
	// could instead find a replacement. Do not retain a retired Indexer.
	owner weak.Pointer[Indexer]
	// The mutable carrier exists only during the observed execution and is
	// removed before callbacks; accounting retains a value-only frozen copy.
	collector  *PublicationStamps
	accounting ReadRepairPhaseAccounting
}

// AcceptedFor verifies the executed incarnation and exact accepted Git blob.
// It deliberately says nothing about the original request's B coverage.
func (o ReadRepairObservation) AcceptedFor(owner *Indexer, blobSHA string) bool {
	return owner != nil && o.owner.Value() == owner && o.executionSucceeded &&
		o.causesComplete && !o.contentAmbiguous && blobSHA != "" && o.acceptedBlobSHA == blobSHA &&
		o.generation != 0 && !o.started.IsZero() && !o.finished.Before(o.started) &&
		!o.acceptedAt.IsZero() && !o.acceptedAt.Before(o.started) && !o.acceptedAt.After(o.finished)
}

// Bounds are the actual execution endpoints, independent of receipt lifetime.
func (o ReadRepairObservation) Bounds() (time.Time, time.Time) { return o.started, o.finished }

// AcceptedAt is the actual applied-and-fresh acceptance, not callback time.
// Keep the first acceptance so repeated same-byte validation cannot move it
// past a later commit and claim a publication that preceded that commit.
func (o ReadRepairObservation) AcceptedAt() time.Time { return o.acceptedAt }

// Generation identifies the executed drain within its concrete owner.
func (o ReadRepairObservation) Generation() uint64 { return o.generation }

// Cause returns at most eight observed reader admissions. Reused wire IDs
// remain distinct through the existing admission generation, not a new ID.
func (o ReadRepairObservation) Cause(i int) (session, requestIDKey string, admissionGeneration uint64, ok bool) {
	if i < 0 || i >= o.causeCount {
		return "", "", 0, false
	}
	cause := o.causes[i]
	return cause.session, cause.requestIDKey, cause.admissionGeneration, true
}

func (o *ReadRepairObservation) acceptFreshBlob(path, blobSHA string, applied bool) {
	if o == nil || !applied || o.contentAmbiguous || path != o.path || blobSHA == "" {
		return
	}
	if o.acceptedBlobSHA != "" && o.acceptedBlobSHA != blobSHA {
		o.acceptedBlobSHA = ""
		o.acceptedAt = time.Time{}
		o.contentAmbiguous = true
		return
	}
	if o.acceptedBlobSHA == blobSHA {
		return
	}
	o.acceptedBlobSHA = blobSHA
	o.acceptedAt = time.Now()
}

// Derive the ledger's Git blob identity from the exact untransformed buffer
// already read by extraction. This must not reopen the source or substitute
// the distinct SHA-256 content-proof identity used by immutable builds.
func readRepairBlobSHA(src []byte) string {
	h := sha1.New()
	_, _ = h.Write([]byte("blob " + strconv.Itoa(len(src)) + "\x00"))
	_, _ = h.Write(src)
	return hex.EncodeToString(h.Sum(nil))
}

// newRepositoryReadRepairObservation does not read a clock or allocate on the
// ordinary unobserved path. Full/multipath drains deliberately have no scoped
// observation; they cannot prove publication of this one pending edit.
func newRepositoryReadRepairObservation(generation uint64, paths []string, waiters []*repositoryMutationWaiter) *ReadRepairObservation {
	if len(paths) != 1 {
		return nil
	}
	var observation *ReadRepairObservation
	for _, waiter := range waiters {
		request := waiter.readRepair
		if request == nil || request.complete == nil {
			continue
		}
		if observation == nil {
			observation = &ReadRepairObservation{
				generation: generation, path: pathkey.Normalize(filepath.ToSlash(paths[0])), causesComplete: true,
			}
		}
		if observation.causeCount == len(observation.causes) {
			observation.causesComplete = false
			continue
		}
		observation.causes[observation.causeCount] = repositoryReadRepairCause{
			session: request.session, requestIDKey: request.requestIDKey,
			admissionGeneration: waiter.generation,
		}
		observation.causeCount++
	}
	return observation
}

// Call only after releasing coordinator, lane and batch locks. Each consumer
// receives a fresh value; no retained waiter or mutable observation escapes.
func completeRepositoryReadRepair(observation *ReadRepairObservation, waiters []*repositoryMutationWaiter) {
	if observation == nil {
		return
	}
	for _, waiter := range waiters {
		if request := waiter.readRepair; request != nil && request.complete != nil {
			request.complete(*observation)
		}
	}
}
