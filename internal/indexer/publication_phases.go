package indexer

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// PublicationPhase names one step between an edit reaching the daemon and the
// graph publishing it. The vocabulary is fixed so a reader can compare records
// across checkouts, sources and builds.
type PublicationPhase string

const (
	// PublicationReceived is the origin of a record: the tool call carrying an
	// MCP edit was dispatched, or a require_fresh request arrived.
	PublicationReceived PublicationPhase = "received"
	// PublicationViewCheckoutResolved is the request's view selector
	// resolved to its registered checkout and scope-checked.
	PublicationViewCheckoutResolved PublicationPhase = "view_checkout_resolved"
	// PublicationViewUseNoted is the checkout's route read and its use noted
	// (a pending base advance applied), before materialization.
	PublicationViewUseNoted PublicationPhase = "view_use_noted"
	// PublicationViewMaterialized is the checkout's generation stack
	// materialized (leased, masks opened, composed).
	PublicationViewMaterialized PublicationPhase = "view_materialized"
	// PublicationViewSelected is the request's first view selection
	// returning (its generation stack composed), before any route wait.
	PublicationViewSelected PublicationPhase = "view_selected"
	// PublicationViewResolved is the request's view being selected and
	// materialized (a source mutation's route wait included).
	PublicationViewResolved PublicationPhase = "view_resolved"
	// PublicationMutationLocked is a source mutation holding its checkout's
	// cycle lock (a lock-free stale pre-check and the wait included).
	PublicationMutationLocked PublicationPhase = "mutation_locked"
	// PublicationMutationAdmitted is the checkout mutation lease being
	// granted (its output-generation receipt opened).
	PublicationMutationAdmitted PublicationPhase = "mutation_admitted"
	// PublicationHandlerStarted is the tool handler starting, every
	// middleware gate passed.
	PublicationHandlerStarted PublicationPhase = "handler_started"
	// PublicationParseGated is the pre-write parse gate's verdict.
	PublicationParseGated PublicationPhase = "parse_gated"
	// PublicationWriteSampled is the lease's one working-copy sample before
	// the write having been taken (git status, its fence, the dirty content
	// fingerprint).
	PublicationWriteSampled PublicationPhase = "write_sampled"
	// PublicationWriteValidated is the last check before the write: the
	// routed snapshot still describes the working copy.
	PublicationWriteValidated PublicationPhase = "write_validated"
	// PublicationRouteWithdrawn is the lease having withdrawn the routed
	// working-tree generation, the store write the disk commit waits on.
	PublicationRouteWithdrawn PublicationPhase = "route_withdrawn"
	// PublicationDiskWriteStarted is the atomic write beginning.
	PublicationDiskWriteStarted PublicationPhase = "disk_write_started"
	// PublicationReceiptCommitted is the edit's atomic rename returning.
	PublicationReceiptCommitted PublicationPhase = "receipt_committed"
	// PublicationTicketCaptured is the refresh ticket's capture having bound
	// the committed bytes and the post-commit working-copy state.
	PublicationTicketCaptured PublicationPhase = "ticket_captured"
	// PublicationChangeObserved is the first working-copy sample that showed
	// the tree differs from the published route — for a filesystem edit, the
	// earliest instant the daemon can be said to have seen it.
	PublicationChangeObserved PublicationPhase = "change_observed"
	// PublicationTicketEnqueued is the refresh ticket entering the checkout
	// coordinator's queue.
	PublicationTicketEnqueued PublicationPhase = "ticket_enqueued"
	// PublicationCycleStarted is the coordinator starting the cycle that will
	// serve the ticket (the quiet window has closed).
	PublicationCycleStarted PublicationPhase = "cycle_started"
	// PublicationAdmitted is the build acquiring the shared build lane.
	PublicationAdmitted PublicationPhase = "admitted"
	// PublicationPlanned is the build's file set and generation being fixed.
	PublicationPlanned PublicationPhase = "planned"
	// PublicationExtracted is extraction of the planned files finishing.
	PublicationExtracted PublicationPhase = "extracted"
	// PublicationSemanticDone is semantic enrichment finishing.
	PublicationSemanticDone PublicationPhase = "semantic_done"
	// PublicationPayloadFlushed is the generation's payload being durable.
	PublicationPayloadFlushed PublicationPhase = "payload_flushed"
	// PublicationPublished is the generation becoming servable.
	PublicationPublished PublicationPhase = "published"
	// PublicationRouteFlipped is the checkout route naming the generation.
	PublicationRouteFlipped PublicationPhase = "route_flipped"
	// PublicationTicketCompleted is the coordinator completing the ticket: the
	// route it published describes a sample taken after the ticket arrived.
	// It is terminal.
	PublicationTicketCompleted PublicationPhase = "ticket_completed"
	// PublicationTicketFailed is the ticket ending without a publication. It
	// is terminal.
	PublicationTicketFailed PublicationPhase = "ticket_failed"
)

// Sources a record is opened for.
const (
	PublicationSourceMCPEdit      = "mcp_edit"
	PublicationSourceFreshRequest = "fresh_request"
	// PublicationSourceBackgroundCompaction is a background working-tree
	// chain compaction (BeginBackgroundCompaction). It is not an edit's path
	// to publication; it is recorded beside them so a reader timing an edit
	// can tell that maintenance ran inside its window, and on which
	// generation.
	PublicationSourceBackgroundCompaction = "background_compaction"
)

const (
	defaultPublicationRecordsPerCheckout = 32
	defaultPublicationCheckouts          = 256
)

// PublicationPhaseRecorder keeps a bounded, per-checkout history of
// publication records. Every offset is taken on the process's monotonic clock
// relative to the record's origin, so a record is immune to wall-clock steps;
// the origin's wall time is kept only to correlate with external logs.
//
// It is safe for concurrent use. Marking is first-wins per phase: a phase is
// the first instant the record reached it, which is the only reading a latency
// breakdown can use when a coalesced cycle serves several records.
type PublicationPhaseRecorder struct {
	mu          sync.Mutex
	perCheckout int
	checkouts   int
	byCheckout  map[string][]*PublicationPhaseRecord
	byKey       map[string]*PublicationPhaseRecord
	lastTouched map[string]time.Time
}

// NewPublicationPhaseRecorder returns a recorder that keeps at most
// perCheckout records for each of at most checkouts checkouts. Non-positive
// bounds take the defaults.
func NewPublicationPhaseRecorder(perCheckout, checkouts int) *PublicationPhaseRecorder {
	if perCheckout <= 0 {
		perCheckout = defaultPublicationRecordsPerCheckout
	}
	if checkouts <= 0 {
		checkouts = defaultPublicationCheckouts
	}
	return &PublicationPhaseRecorder{
		perCheckout: perCheckout,
		checkouts:   checkouts,
		byCheckout:  make(map[string][]*PublicationPhaseRecord),
		byKey:       make(map[string]*PublicationPhaseRecord),
		lastTouched: make(map[string]time.Time),
	}
}

var defaultPublicationPhases = NewPublicationPhaseRecorder(0, 0)

// DefaultPublicationPhases is the daemon-wide recorder. The MCP edit and
// freshness paths open records on it; the checkout coordinator marks them
// through MarkCheckoutThrough; daemon status and the mutation receipt read it.
func DefaultPublicationPhases() *PublicationPhaseRecorder { return defaultPublicationPhases }

// PublicationPhaseRecord is one edit's (or one fresh request's) path to
// publication.
type PublicationPhaseRecord struct {
	mu         sync.Mutex
	key        string
	checkoutID string
	source     string
	ticket     uint64
	generation int64
	origin     time.Time
	marks      []publicationMark
	terminal   bool
}

type publicationMark struct {
	phase PublicationPhase
	at    time.Time
}

// Begin opens a record whose origin is origin (time.Now() when zero) and
// records PublicationReceived at it. origin must come from time.Now() in this
// process so its monotonic reading is present. A key already open returns the
// existing record.
func (r *PublicationPhaseRecorder) Begin(checkoutID, key, source string, origin time.Time) *PublicationPhaseRecord {
	if r == nil || key == "" {
		return nil
	}
	if origin.IsZero() {
		origin = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byKey[key]; ok {
		return existing
	}
	record := &PublicationPhaseRecord{
		key: key, checkoutID: checkoutID, source: source, origin: origin,
		marks: []publicationMark{{phase: PublicationReceived, at: origin}},
	}
	if _, known := r.byCheckout[checkoutID]; !known && len(r.byCheckout) >= r.checkouts {
		r.evictOldestCheckoutLocked()
	}
	records := append(r.byCheckout[checkoutID], record)
	if len(records) > r.perCheckout {
		for _, dropped := range records[:len(records)-r.perCheckout] {
			delete(r.byKey, dropped.key)
		}
		records = append([]*PublicationPhaseRecord(nil), records[len(records)-r.perCheckout:]...)
	}
	r.byCheckout[checkoutID] = records
	r.byKey[key] = record
	r.lastTouched[checkoutID] = origin
	return record
}

func (r *PublicationPhaseRecorder) evictOldestCheckoutLocked() {
	oldest, oldestAt := "", time.Time{}
	for checkoutID, at := range r.lastTouched {
		if oldest == "" || at.Before(oldestAt) {
			oldest, oldestAt = checkoutID, at
		}
	}
	for _, record := range r.byCheckout[oldest] {
		delete(r.byKey, record.key)
	}
	delete(r.byCheckout, oldest)
	delete(r.lastTouched, oldest)
}

// Lookup returns the record opened under key.
func (r *PublicationPhaseRecorder) Lookup(key string) (*PublicationPhaseRecord, bool) {
	if r == nil || key == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.byKey[key]
	return record, ok
}

// Rekey moves the record opened under oldKey to newKey, so a record opened
// before the identifier a reader will look it up by exists (an MCP edit's
// receipt id is minted only after its refresh ticket is admitted) can still
// be found under that identifier. It reports false, and changes nothing, when
// oldKey is unknown or newKey is empty or already names another record.
func (r *PublicationPhaseRecorder) Rekey(oldKey, newKey string) bool {
	if r == nil || oldKey == "" || newKey == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.byKey[oldKey]
	if !ok {
		return false
	}
	if oldKey == newKey {
		return true
	}
	if _, taken := r.byKey[newKey]; taken {
		return false
	}
	delete(r.byKey, oldKey)
	r.byKey[newKey] = record
	record.mu.Lock()
	record.key = newKey
	record.mu.Unlock()
	return true
}

// PublicationRecordFromContext returns the record WithPublicationRecord put
// on ctx, nil when there is none. An adapter that admits refresh tickets by
// some other route than the checkout coordinator uses it to honour the same
// bind-before-wake contract.
func PublicationRecordFromContext(ctx context.Context) *PublicationPhaseRecord {
	return publicationRecordFrom(ctx)
}

// MarkCheckoutThrough marks phase on every open record of checkoutID whose
// refresh ticket was admitted at or before through — the records a
// coordinator cycle captured with checkoutRefreshHighWater — and reports how
// many it marked. through == 0 marks nothing: a cycle that captured no ticket
// serves no record. It is the one call the coordinator and builder need at
// each phase boundary.
func (r *PublicationPhaseRecorder) MarkCheckoutThrough(checkoutID string, through uint64, phase PublicationPhase) int {
	if r == nil || through == 0 {
		return 0
	}
	now := time.Now()
	r.mu.Lock()
	records := append([]*PublicationPhaseRecord(nil), r.byCheckout[checkoutID]...)
	r.mu.Unlock()
	marked := 0
	for _, record := range records {
		if record.markIfServed(through, phase, now) {
			marked++
		}
	}
	return marked
}

// Snapshot returns the records of one checkout, oldest first.
func (r *PublicationPhaseRecorder) Snapshot(checkoutID string) []PublicationPhaseSnapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	records := append([]*PublicationPhaseRecord(nil), r.byCheckout[checkoutID]...)
	r.mu.Unlock()
	out := make([]PublicationPhaseSnapshot, 0, len(records))
	for _, record := range records {
		out = append(out, record.Snapshot())
	}
	return out
}

// Checkouts lists the checkouts that have records, sorted.
func (r *PublicationPhaseRecorder) Checkouts() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]string, 0, len(r.byCheckout))
	for checkoutID := range r.byCheckout {
		out = append(out, checkoutID)
	}
	r.mu.Unlock()
	sort.Strings(out)
	return out
}

// BindTicket records the refresh ticket's admission sequence, which is what
// MarkCheckoutThrough matches a cycle against.
func (p *PublicationPhaseRecord) BindTicket(sequence uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.ticket == 0 {
		p.ticket = sequence
	}
	p.mu.Unlock()
}

// SetGeneration records the dirty generation that published this record.
func (p *PublicationPhaseRecord) SetGeneration(generationID int64) {
	if p == nil || generationID <= 0 {
		return
	}
	p.mu.Lock()
	p.generation = generationID
	p.mu.Unlock()
}

// Mark records phase now. First mark wins; a terminal record takes no more
// marks.
func (p *PublicationPhaseRecord) Mark(phase PublicationPhase) { p.MarkAt(phase, time.Now()) }

// MarkAt records phase at a known instant, which must come from time.Now() in
// this process (so it carries a monotonic reading) — a commit ledger's
// committed-at stamp, for example. A zero instant, or one earlier than the
// origin, is refused: an instant that did not happen after the record opened
// is not a phase of it.
func (p *PublicationPhaseRecord) MarkAt(phase PublicationPhase, at time.Time) {
	if p == nil || at.IsZero() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markLocked(phase, at)
}

func (p *PublicationPhaseRecord) markLocked(phase PublicationPhase, at time.Time) bool {
	if p.terminal || at.Before(p.origin) {
		return false
	}
	for _, mark := range p.marks {
		if mark.phase == phase {
			return false
		}
	}
	p.marks = append(p.marks, publicationMark{phase: phase, at: at})
	if phase == PublicationTicketCompleted || phase == PublicationTicketFailed {
		p.terminal = true
	}
	return true
}

func (p *PublicationPhaseRecord) markIfServed(through uint64, phase PublicationPhase, at time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ticket == 0 || p.ticket > through {
		return false
	}
	return p.markLocked(phase, at)
}

// PublicationPhaseOffset is one phase and its monotonic offset from the
// record's origin.
type PublicationPhaseOffset struct {
	Phase    PublicationPhase `json:"phase"`
	OffsetNS int64            `json:"offset_ns"`
	OffsetMS float64          `json:"offset_ms"`
}

// PublicationPhaseSnapshot is a record rendered for a receipt or a status
// answer. Phases are ordered by offset.
type PublicationPhaseSnapshot struct {
	Key               string                   `json:"key"`
	CheckoutID        string                   `json:"checkout_id"`
	Source            string                   `json:"source"`
	Ticket            uint64                   `json:"ticket,omitempty"`
	DirtyGenerationID int64                    `json:"dirty_generation_id,omitempty"`
	OriginWall        string                   `json:"origin_wall"`
	Clock             string                   `json:"clock"`
	Terminal          bool                     `json:"terminal"`
	Phases            []PublicationPhaseOffset `json:"phases"`
}

// Snapshot renders the record.
func (p *PublicationPhaseRecord) Snapshot() PublicationPhaseSnapshot {
	if p == nil {
		return PublicationPhaseSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := PublicationPhaseSnapshot{
		Key: p.key, CheckoutID: p.checkoutID, Source: p.source, Ticket: p.ticket,
		DirtyGenerationID: p.generation,
		OriginWall:        p.origin.UTC().Format(time.RFC3339Nano),
		Clock:             "monotonic",
		Terminal:          p.terminal,
		Phases:            make([]PublicationPhaseOffset, 0, len(p.marks)),
	}
	for _, mark := range p.marks {
		offset := mark.at.Sub(p.origin)
		out.Phases = append(out.Phases, PublicationPhaseOffset{
			Phase: mark.phase, OffsetNS: offset.Nanoseconds(),
			OffsetMS: float64(offset.Microseconds()) / 1000,
		})
	}
	sort.SliceStable(out.Phases, func(i, j int) bool { return out.Phases[i].OffsetNS < out.Phases[j].OffsetNS })
	return out
}

// BeginBackgroundCompaction opens the record of one background chain
// compaction of checkoutID, keyed uniquely, with its origin at started (the
// instant the compaction was scheduled; time.Now() when zero). The compactor
// marks PublicationAdmitted when it holds the build lane,
// PublicationPublished and PublicationRouteFlipped as they happen, sets the
// generation it published, and closes the record with FinishBackgroundWork.
// No refresh ticket is ever bound to it, so no coordinator cycle marks it.
func (r *PublicationPhaseRecorder) BeginBackgroundCompaction(checkoutID string, started time.Time) *PublicationPhaseRecord {
	if r == nil || checkoutID == "" {
		return nil
	}
	if started.IsZero() {
		started = time.Now()
	}
	key := "compaction-" + strconv.FormatUint(backgroundRecordSequence.Add(1), 10)
	return r.Begin(checkoutID, key, PublicationSourceBackgroundCompaction, started)
}

var backgroundRecordSequence atomic.Uint64

// FinishBackgroundWork closes a background record: completed when the work
// published what it set out to, failed otherwise (canceled, torn, refused).
func (p *PublicationPhaseRecord) FinishBackgroundWork(completed bool) {
	if completed {
		p.Mark(PublicationTicketCompleted)
		return
	}
	p.Mark(PublicationTicketFailed)
}

// PublicationStamps collects publication phases a request reaches before the
// record that will carry them exists: an MCP edit's record is opened only
// after its disk commit, but what came before the commit (view selection,
// admission, the parse gate, the write's own checks) is exactly where a slow
// commit's time goes. The record absorbs them when it opens.
type PublicationStamps struct {
	mu    sync.Mutex
	marks []publicationMark
}

type publicationStampsKey struct{}

// WithPublicationStamps returns ctx carrying a stamp collector; ctx already
// carrying one is returned unchanged.
func WithPublicationStamps(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(publicationStampsKey{}).(*PublicationStamps); ok {
		return ctx
	}
	return context.WithValue(ctx, publicationStampsKey{}, &PublicationStamps{})
}

// PublicationStampsFrom returns the collector on ctx, nil when there is none.
func PublicationStampsFrom(ctx context.Context) *PublicationStamps {
	if ctx == nil {
		return nil
	}
	stamps, _ := ctx.Value(publicationStampsKey{}).(*PublicationStamps)
	return stamps
}

// StampPublicationPhase records phase now for the request on ctx: on its
// publication record when one is already bound (WithPublicationRecord), else
// on its stamp collector. The first stamp of a phase wins once absorbed, as on
// a record. A context with
// neither records nothing, so the call is free on paths no edit uses.
func StampPublicationPhase(ctx context.Context, phase PublicationPhase) {
	if record := publicationRecordFrom(ctx); record != nil {
		record.Mark(phase)
		return
	}
	stamps := PublicationStampsFrom(ctx)
	if stamps == nil {
		return
	}
	// Repeats are kept; the record keeps the first when it absorbs them.
	now := time.Now()
	stamps.mu.Lock()
	stamps.marks = append(stamps.marks, publicationMark{phase: phase, at: now})
	stamps.mu.Unlock()
}

// Absorb marks every collected stamp on the record at its own instant.
// Stamps taken before the record's origin are refused by MarkAt.
func (p *PublicationPhaseRecord) Absorb(stamps *PublicationStamps) {
	if p == nil || stamps == nil {
		return
	}
	stamps.mu.Lock()
	marks := append([]publicationMark(nil), stamps.marks...)
	stamps.mu.Unlock()
	for _, mark := range marks {
		p.MarkAt(mark.phase, mark.at)
	}
}
