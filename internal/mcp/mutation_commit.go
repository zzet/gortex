package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/agents"
)

// The commit ledger exists because a tool call has two independent terminal
// states and the transport only ever reported one of them.
//
// boundHandler answers the client when the deadline fires and lets the handler
// keep running (tool_deadline.go). For a read that is harmless. For a write it
// was not: agents.AtomicWriteFile takes no context, so a handler that had
// already reached the rename committed to disk regardless, and the caller was
// told only "treat any side effect as unknown". A client that then retried, or
// fell back to its own editor, applied the same logical change twice.
//
// Every disk commit therefore registers here BEFORE the write and is stamped
// terminal immediately after it. The record outlives the request, so:
//
//   - the abandoned response can name what actually landed instead of guessing
//     (mutationCommitNote, read by boundToolHandler),
//   - a client that lost the response can ask (mutation_status),
//   - a client that supplied a mutation_id gets the original result replayed
//     rather than writing twice.
//
// Disk state and graph state are deliberately separate fields. "The bytes are
// on disk" and "the graph has caught up" are different questions with different
// failure modes, and collapsing them is what made the original error message
// unusable.
const (
	// mutationDiskNotApplied means the handler stopped before the write. This
	// is a guarantee, not a guess: the ledger entry is created ahead of the
	// cancellation check, so a record can only reach this state by taking the
	// branch that skips agents.AtomicWriteFile entirely.
	mutationDiskNotApplied = "not_applied"
	// mutationDiskInFlight means the write was entered but has not reported
	// back. A caller observing this must query rather than retry.
	mutationDiskInFlight = "in_flight"
	// mutationDiskCommitted means the atomic rename returned success.
	mutationDiskCommitted = "committed"
	// mutationDiskFailed means the write was attempted and errored. Nothing was
	// renamed into place, so the target still holds its previous content.
	mutationDiskFailed = "failed"
)

const (
	mutationGraphPending = "pending"
	mutationGraphFresh   = "fresh"
	mutationGraphStale   = "stale"
	mutationGraphFailed  = "failed"
)

const (
	// mutationCommitRetention outlives the transport deadline by a wide margin:
	// a client only learns it needs the receipt after its call was abandoned,
	// and it may spend several turns reasoning before it asks.
	mutationCommitRetention = 30 * time.Minute
	// maxMutationCommits bounds the ledger for a long-lived daemon doing bulk
	// edits. Oldest-first eviction keeps the entries a recovering client is
	// most likely to want.
	maxMutationCommits = 512
	// maxMutationCommitListing caps an unfiltered mutation_status listing.
	maxMutationCommitListing = 20
)

// mutationIDParamDescription is shared across the three single-file mutating
// tools for the same reason the physical-evidence blurbs are (mutation_evidence.go):
// the cold tools/list byte ceiling is measured, and three copies of one sentence
// is pure tax.
const mutationIDParamDescription = "Optional idempotency key. Retrying with the same key and the identical edit " +
	"replays the first result instead of writing twice — the safe way to retry an abandoned call. " +
	"A different edit under the same key is refused."

var mutationCommitSequence atomic.Uint64

// errMutationNotApplied is returned by commitFileMutation when the request was
// cancelled before the disk commit. Callers surface it as a terminal refusal so
// the client knows retrying is safe.
var errMutationNotApplied = errors.New("mutation not applied")

type mutationCommitRecord struct {
	mu sync.RWMutex

	id      string
	tool    string
	key     string
	relPath string
	absPath string
	// fingerprint identifies the logical edit this record was created for. It
	// is only consulted when key != "", to reject a mutation_id reused for a
	// different payload.
	fingerprint string

	disk  string
	graph string

	newSHA         string
	bytesWritten   int
	reindexReceipt string
	errText        string

	startedAt   time.Time
	committedAt time.Time

	// response is the successful tool payload, retained only for records that
	// carry a caller-chosen mutation_id. Without a key there is nothing to
	// replay against, so ordinary edits pay no memory for this.
	response map[string]any
}

type mutationCommitSnapshot struct {
	Receipt      string `json:"receipt"`
	Tool         string `json:"tool,omitempty"`
	MutationID   string `json:"mutation_id,omitempty"`
	Path         string `json:"path,omitempty"`
	DiskStatus   string `json:"disk_status"`
	GraphStatus  string `json:"graph_status"`
	NewSHA       string `json:"new_sha,omitempty"`
	BytesWritten int    `json:"bytes_written,omitempty"`
	Error        string `json:"error,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	CommittedAt  string `json:"committed_at,omitempty"`
}

func (r *mutationCommitRecord) snapshot() mutationCommitSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := mutationCommitSnapshot{
		Receipt:      r.id,
		Tool:         r.tool,
		MutationID:   r.key,
		Path:         r.relPath,
		DiskStatus:   r.disk,
		GraphStatus:  r.graph,
		NewSHA:       r.newSHA,
		BytesWritten: r.bytesWritten,
		Error:        r.errText,
	}
	if !r.startedAt.IsZero() {
		snap.StartedAt = r.startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !r.committedAt.IsZero() {
		snap.CommittedAt = r.committedAt.UTC().Format(time.RFC3339Nano)
	}
	return snap
}

func (r *mutationCommitRecord) diskStatus() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.disk
}

func (r *mutationCommitRecord) markNotApplied(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disk = mutationDiskNotApplied
	r.graph = mutationGraphStale
	if err != nil {
		r.errText = err.Error()
	}
}

func (r *mutationCommitRecord) markFailed(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disk = mutationDiskFailed
	r.graph = mutationGraphStale
	if err != nil {
		r.errText = err.Error()
	}
}

func (r *mutationCommitRecord) markCommitted(newSHA string, bytesWritten int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disk = mutationDiskCommitted
	r.newSHA = newSHA
	r.bytesWritten = bytesWritten
	r.committedAt = time.Now()
}

// recordGraph stamps the graph half of the receipt from the freshness outcome
// the reindex path produced. It never touches the disk half: a failed reindex
// does not un-write a committed file, and conflating the two is the reporting
// bug this ledger exists to fix.
func (r *mutationCommitRecord) recordGraph(outcome mutationReindexOutcome) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.graph = graphStatusFor(outcome)
	r.reindexReceipt = outcome.Receipt
}

// retainResponse stores the successful payload for idempotent replay. Only
// keyed records retain it — see mutationCommitRecord.response.
func (r *mutationCommitRecord) retainResponse(resp map[string]any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == "" {
		return
	}
	clone := make(map[string]any, len(resp)+1)
	for key, value := range resp {
		clone[key] = value
	}
	r.response = clone
}

func (r *mutationCommitRecord) replayResponse() (map[string]any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.response == nil {
		return nil, false
	}
	clone := make(map[string]any, len(r.response)+1)
	for key, value := range r.response {
		clone[key] = value
	}
	return clone, true
}

func (r *mutationCommitRecord) pendingReindexReceipt() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.graph != mutationGraphPending {
		return ""
	}
	return r.reindexReceipt
}

// graphStatusFor collapses a reindex outcome into the graph half of a receipt.
func graphStatusFor(outcome mutationReindexOutcome) string {
	switch {
	case outcome.Err != nil:
		return mutationGraphFailed
	case outcome.Pending:
		return mutationGraphPending
	case outcome.Reindexed:
		return mutationGraphFresh
	default:
		return mutationGraphStale
	}
}

// mutationCommitLedger is the daemon-lifetime store. It is a plain guarded map
// rather than a sync.Map because eviction needs a total order over entries, and
// bounding it matters more here than lock-free reads on a path that already
// performs a filesystem write.
type mutationCommitLedger struct {
	mu      sync.Mutex
	byID    map[string]*mutationCommitRecord
	byKey   map[string]*mutationCommitRecord
	ordered []*mutationCommitRecord
}

func (l *mutationCommitLedger) put(record *mutationCommitRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = make(map[string]*mutationCommitRecord)
		l.byKey = make(map[string]*mutationCommitRecord)
	}
	l.evictLocked(time.Now())
	l.byID[record.id] = record
	if record.key != "" {
		l.byKey[record.key] = record
	}
	l.ordered = append(l.ordered, record)
}

// evictLocked drops expired entries first, then the oldest survivors if the
// ledger is still over its cap.
func (l *mutationCommitLedger) evictLocked(now time.Time) {
	keep := l.ordered[:0]
	for _, record := range l.ordered {
		record.mu.RLock()
		expired := now.Sub(record.startedAt) > mutationCommitRetention
		record.mu.RUnlock()
		if expired {
			delete(l.byID, record.id)
			if record.key != "" && l.byKey[record.key] == record {
				delete(l.byKey, record.key)
			}
			continue
		}
		keep = append(keep, record)
	}
	l.ordered = keep
	for len(l.ordered) >= maxMutationCommits {
		oldest := l.ordered[0]
		l.ordered = l.ordered[1:]
		delete(l.byID, oldest.id)
		if oldest.key != "" && l.byKey[oldest.key] == oldest {
			delete(l.byKey, oldest.key)
		}
	}
}

func (l *mutationCommitLedger) byReceipt(id string) (*mutationCommitRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.byID[id]
	return record, ok
}

func (l *mutationCommitLedger) byMutationID(key string) (*mutationCommitRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.byKey[key]
	return record, ok
}

// recentForPath returns the newest record touching relPath. Path recovery is
// what a client has left when it lost the response entirely and therefore never
// saw a receipt id.
func (l *mutationCommitLedger) recentForPath(relPath string) (*mutationCommitRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.ordered) - 1; i >= 0; i-- {
		record := l.ordered[i]
		record.mu.RLock()
		match := record.relPath == relPath || record.absPath == relPath
		record.mu.RUnlock()
		if match {
			return record, true
		}
	}
	return nil, false
}

func (l *mutationCommitLedger) recent(limit int) []*mutationCommitRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.ordered) {
		limit = len(l.ordered)
	}
	out := make([]*mutationCommitRecord, 0, limit)
	for i := len(l.ordered) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l.ordered[i])
	}
	return out
}

// mutationCommitNote is the per-request channel between a mutating handler and
// the deadline wrapper that may answer for it. boundHandler installs one on
// every call context; only handlers that reach a disk commit ever write to it.
type mutationCommitNote struct {
	mu      sync.Mutex
	records []*mutationCommitRecord
}

type mutationCommitNoteKey struct{}

func withMutationCommitNote(ctx context.Context) (context.Context, *mutationCommitNote) {
	note := &mutationCommitNote{}
	return context.WithValue(ctx, mutationCommitNoteKey{}, note), note
}

func mutationCommitNoteFrom(ctx context.Context) *mutationCommitNote {
	note, _ := ctx.Value(mutationCommitNoteKey{}).(*mutationCommitNote)
	return note
}

func (n *mutationCommitNote) observe(record *mutationCommitRecord) {
	if n == nil || record == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.records = append(n.records, record)
}

// mutationCommitVerdict is what the deadline wrapper reports. It is read at
// abandon time from a handler that is still running, so it is deliberately
// conservative: anything short of a confirmed terminal state reads as unknown.
type mutationCommitVerdict struct {
	// Status is one of "", "not_applied", "unknown", or "committed".
	Status    string                   `json:"disk_status"`
	Snapshots []mutationCommitSnapshot `json:"mutations,omitempty"`
}

func (n *mutationCommitNote) verdict() mutationCommitVerdict {
	if n == nil {
		return mutationCommitVerdict{}
	}
	n.mu.Lock()
	records := append([]*mutationCommitRecord(nil), n.records...)
	n.mu.Unlock()
	if len(records) == 0 {
		return mutationCommitVerdict{}
	}
	verdict := mutationCommitVerdict{Status: mutationDiskNotApplied}
	for _, record := range records {
		snap := record.snapshot()
		verdict.Snapshots = append(verdict.Snapshots, snap)
		switch snap.DiskStatus {
		case mutationDiskCommitted:
			verdict.Status = mutationDiskCommitted
		case mutationDiskInFlight:
			if verdict.Status != mutationDiskCommitted {
				verdict.Status = mutationDiskInFlight
			}
		}
	}
	return verdict
}

// beginMutationCommit registers an in-flight disk commit and publishes it to
// the request's note. It must be called before the cancellation check that
// guards the write, so that a record exists for every path on which a write is
// still reachable.
func (s *Server) beginMutationCommit(ctx context.Context, tool, mutationID, fingerprint, relPath, absPath string) *mutationCommitRecord {
	record := &mutationCommitRecord{
		id:          fmt.Sprintf("commit-%d", mutationCommitSequence.Add(1)),
		tool:        tool,
		key:         mutationID,
		fingerprint: fingerprint,
		relPath:     relPath,
		absPath:     absPath,
		disk:        mutationDiskInFlight,
		graph:       mutationGraphStale,
		startedAt:   time.Now(),
	}
	s.mutationCommits.put(record)
	mutationCommitNoteFrom(ctx).observe(record)
	return record
}

// commitFileMutation is the single guarded disk-commit primitive for the
// single-file mutating tools. Every caller gets three things it did not have
// before: a refusal when the request is already dead, a durable receipt, and a
// note the deadline wrapper can read if it answers first.
func (s *Server) commitFileMutation(
	ctx context.Context,
	tool, mutationID, fingerprint, relPath, absPath string,
	data []byte,
	perm os.FileMode,
) (*mutationCommitRecord, error) {
	record := s.beginMutationCommit(ctx, tool, mutationID, fingerprint, relPath, absPath)
	if s.mutationPreCommitHook != nil {
		s.mutationPreCommitHook(record)
	}
	// The cancellation gate. Before this line no bytes can have been written,
	// so refusing here is the "cancellation guarantees no mutation" half of the
	// contract. After it the window is one atomic rename wide, and that window
	// is exactly what the in_flight receipt covers.
	if err := ctx.Err(); err != nil {
		record.markNotApplied(err)
		return record, fmt.Errorf("%w: %w", errMutationNotApplied, err)
	}
	if err := agents.AtomicWriteFile(absPath, data, perm); err != nil {
		record.markFailed(err)
		return record, err
	}
	record.markCommitted(gitBlobSHA(data), len(data))
	return record, nil
}

// mutationNotAppliedMessage is the terminal refusal for a write that was
// cancelled before it started. It says "nothing changed" plainly, because that
// is the one case where a client may retry without checking anything.
func mutationNotAppliedMessage(what string, record *mutationCommitRecord, err error) string {
	return fmt.Sprintf(
		"%s cancelled before the disk commit — nothing was written and the file is unchanged (disk_status=not_applied, receipt=%s): %v. "+
			"Retrying is safe.",
		what, record.id, err)
}

// attachMutationCommit puts the disk half of the receipt on a success payload.
// The graph half rides on the same response via attachMutationFreshness.
func attachMutationCommit(resp map[string]any, record *mutationCommitRecord) {
	if record == nil {
		return
	}
	resp["mutation_receipt"] = record.id
	resp["disk_status"] = record.diskStatus()
	if record.key != "" {
		resp["mutation_id"] = record.key
	}
}

// mutationFingerprint identifies one logical mutation. It is only ever compared
// against another fingerprint stored under the same caller-chosen mutation_id,
// so it does not need to be globally unique — it needs to catch a key reused
// for a different edit.
func mutationFingerprint(tool string, parts ...string) string {
	sum := sha256.Sum256([]byte(tool + "\x00" + strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// replayMutation resolves a caller-supplied mutation_id against the ledger.
//
// Reusing a key with the SAME payload replays the original result instead of
// writing again — this is what makes a retry after an abandoned call safe.
// Reusing it with a DIFFERENT payload is a caller bug and is refused rather
// than silently treated as a new edit, matching atomic_batch_edit.
func (s *Server) replayMutation(mutationID, fingerprint string) (map[string]any, string, bool) {
	if mutationID == "" {
		return nil, "", false
	}
	record, ok := s.mutationCommits.byMutationID(mutationID)
	if !ok {
		return nil, "", false
	}
	record.mu.RLock()
	storedFingerprint := record.fingerprint
	disk := record.disk
	record.mu.RUnlock()
	if storedFingerprint != fingerprint {
		return nil, fmt.Sprintf(
			"mutation_id %q was already used for a different edit — reuse it only to retry the identical mutation, or choose a new id",
			mutationID), true
	}
	if disk != mutationDiskCommitted {
		// A prior attempt under this key did not commit. Nothing to replay:
		// let the caller through so the edit can actually land.
		return nil, "", false
	}
	resp, ok := record.replayResponse()
	if !ok {
		snap := record.snapshot()
		encoded, _ := json.Marshal(snap)
		return nil, fmt.Sprintf(
			"mutation_id %q already committed but its response was not retained; query mutation_status for the receipt: %s",
			mutationID, encoded), true
	}
	resp["replayed"] = true
	resp["mutation_receipt"] = record.id
	return resp, "", true
}

// mutationCommitListing renders records for the mutation_status tool.
func mutationCommitListing(records []*mutationCommitRecord) []mutationCommitSnapshot {
	out := make([]mutationCommitSnapshot, 0, len(records))
	for _, record := range records {
		out = append(out, record.snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}
