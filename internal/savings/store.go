// Package savings persists cumulative token-savings metrics across server
// restarts. Every source-reading tool call feeds this store through the MCP
// server's tokenStats, so over time the numbers become a credible narrative:
// "Gortex saved N tokens / $X at model rate this month".
//
// Storage: the machine-global SQLite sidecar (~/.gortex/sidecar.sqlite —
// the same database that holds notes, memories, scopes, and notebooks).
// Observations are buffered in memory and committed in batches (see
// Store.AddObservation): one transaction per flush window, not one per tool
// call. Multiple gortex processes write the same database safely through
// SQLite's WAL + busy-timeout. The flat-file era (savings.json cumulative
// totals + savings.jsonl event log under the cache dir) is imported once on
// open and the legacy files renamed to *.bak.
//
// Durability. A flush happens on a bounded timer (DefaultFlushInterval), when
// the buffer reaches DefaultFlushMax, on every read of the ledger through
// this Store, and on Close — the daemon's teardown chain calls
// Server.FlushSavings before the sidecar handle is released. What is NOT
// guaranteed is per-call durability: a process killed with observations still
// buffered loses at most one flush window of accounting. That is the trade
// this package makes deliberately — every read-only tool call used to pay a
// durable ~37 KB sidecar transaction to book its own bookkeeping, which made
// an idle agent session the dominant writer on the machine. Accounting is not
// data; losing a minute of it on a SIGKILL costs a few tokens off a
// cumulative counter.
//
// The window is not one size. The daemon — the process the write
// amplification was measured on — runs on the minute-scale default. A
// one-shot stdio server (`gortex mcp`, `gortex server`) is killed by its
// host rather than shut down, and a whole session can be shorter than a
// minute, so its entry point tightens the bound to OneshotFlushInterval /
// OneshotFlushMax through Store.SetFlushBounds. Either way an explicit
// GORTEX_SAVINGS_FLUSH_INTERVAL wins: the operator knob is never overridden.
//
// Exactness. Every read path on this Store (Snapshot, EventsSince,
// ToolTotals, ModelTotals, ClientTotals) flushes first — and not only its own
// buffer. Two Store values opened on the same path share ONE sidecar handle
// (persistence.OpenSidecar caches by absolute path) but own SEPARATE buffers,
// so a read flushes every live Store feeding that database, and Close does
// the same before it releases the shared handle. `gortex gain`'s loadHistory
// opens exactly such a second handle; without the cross-handle flush it read
// a ledger whose live window was still sitting in another Store's memory, and
// its Close then dropped that window with "database is closed".
// What the flush does not cover is a batch already detached and mid-COMMIT in
// another goroutine: nothing is lost, but a concurrent reader can miss it for
// the duration of that one transaction.
// A reader in ANOTHER PROCESS (`gortex gain` against a live daemon) sees the
// committed state only, i.e. it can lag the daemon's live counter by up to
// one flush window.
package savings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
)

// schemaVersion is the snapshot-shape version, kept for the JSON surface
// (graph_stats cumulative_savings, `gortex savings --json`) and for
// reading flat-file-era ledgers during legacy import.
const schemaVersion = 1

// Totals is the cumulative record for a single scope (top-level or per-repo).
type Totals struct {
	TokensSaved    int64 `json:"tokens_saved"`
	TokensReturned int64 `json:"tokens_returned"`
	CallsCounted   int64 `json:"calls_counted"`
}

// File is the snapshot shape callers consume (graph_stats, the CLI) and
// the on-disk schema of the flat-file era — still parsed by the one-shot
// legacy import.
type File struct {
	Version     int                `json:"version"`
	FirstSeen   time.Time          `json:"first_seen"`
	LastUpdated time.Time          `json:"last_updated"`
	Totals      Totals             `json:"totals"`
	PerRepo     map[string]*Totals `json:"per_repo,omitempty"`
	PerLanguage map[string]*Totals `json:"per_language,omitempty"`
	// DroppedObservations counts ledger writes this process discarded
	// (accounting must never fail a tool call, but drops must not be
	// invisible — a persistently failing ledger looks exactly like
	// "nothing recorded" otherwise).
	DroppedObservations int64 `json:"dropped_observations,omitempty"`
}

// Observation is one source-reading tool call to book.
type Observation struct {
	Repo      string
	Language  string
	Tool      string
	SessionID string
	// Model is the LLM model that drove the call when known (resolved by
	// the recorder from the host's model hint); Client is the MCP client
	// app from the initialize handshake. Both may be empty.
	Model    string
	Client   string
	Returned int64
	Saved    int64
}

// Store is the token-savings ledger. All operations are safe for
// concurrent use. When opened with an empty path the store tracks
// in-memory only — the behaviour test fixtures and the eval servers
// rely on — and never touches disk.
//
// Write errors against the sidecar are intentionally not propagated to
// record() callers (accounting must never fail a tool call): a batch that
// cannot be committed is dropped whole and counted in DroppedObservations,
// rather than retained to grow without bound against a ledger that is
// persistently failing.
type Store struct {
	mu        sync.Mutex
	sc        *persistence.SidecarStore
	mem       File    // in-memory accumulation when sc == nil
	memEvents []Event // in-memory event log when sc == nil

	// buf holds sidecar-bound observations not yet committed. It is the
	// whole point of the coalescing: a read-only tool call appends here
	// instead of opening a transaction. Guarded by mu.
	buf []persistence.SavingsEvent
	// flushTimer bounds how long a buffered observation may stay
	// uncommitted. Armed by the first append into an empty buffer,
	// stopped and cleared by every flush, so an idle store holds no
	// timer and a read-only CLI process never starts one.
	flushTimer *time.Timer
	// flushEvery is the timer bound; flushMax the count bound. A
	// non-positive flushEvery restores the historical per-observation
	// commit (see DefaultFlushInterval).
	flushEvery time.Duration
	flushMax   int
	// flushEveryFromEnv records that flushEvery came from
	// GORTEX_SAVINGS_FLUSH_INTERVAL. SetFlushBounds refuses to overwrite
	// an operator's explicit knob — neither to tighten nor to relax it.
	flushEveryFromEnv bool

	// dropped counts observations the sidecar refused (disk full,
	// permissions, lock timeout). warnOnce emits a single stderr line
	// the first time it happens so the failure is diagnosable without
	// failing any tool call.
	dropped  atomic.Int64
	warnOnce sync.Once
}

// Flush bounds. One flush is one sidecar transaction (~37 KB of WAL), so the
// interval is what turns "per read-only tool call" into "per window". Sixty
// seconds keeps a polling agent (one query every few seconds) at one
// transaction per minute while bounding both the loss window on a crash and
// how far another process's view of the ledger can lag.
const (
	DefaultFlushInterval = 60 * time.Second
	DefaultFlushMax      = 256
)

// One-shot bounds. The ephemeral stdio MCP server (`gortex mcp`,
// `gortex server`) is not the write-amplification target — the idle floor
// the coalescing was built for is a daemon phenomenon, and a one-shot
// process books a handful of observations in total. It is, however, the
// process MCP hosts SIGKILL rather than shut down, which is the exact
// failure the flat-file era had ("permanently empty under SIGKILLing MCP
// clients"). A few seconds / a handful of events keeps the loss on a kill
// negligible while still coalescing a burst of calls into one transaction.
const (
	OneshotFlushInterval = 5 * time.Second
	OneshotFlushMax      = 16
)

// flushIntervalEnv overrides DefaultFlushInterval. A value of "0" (or any
// non-positive duration) restores a durable transaction per observation —
// the pre-coalescing behaviour, for an operator who wants per-call
// durability and is willing to pay for it.
const flushIntervalEnv = "GORTEX_SAVINGS_FLUSH_INTERVAL"

// flushInterval resolves the configured flush bound and reports whether the
// operator set it explicitly. An unparseable value falls back to the default
// rather than disabling buffering, so a typo cannot silently reinstate the
// per-call transaction — and it is NOT reported as operator-set, so a typo
// also cannot pin the window against SetFlushBounds.
func flushInterval() (time.Duration, bool) {
	raw := os.Getenv(flushIntervalEnv)
	if raw == "" {
		return DefaultFlushInterval, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return DefaultFlushInterval, false
	}
	return d, true
}

// DefaultDBPath returns the canonical savings ledger location: the
// machine-global sidecar database under the Gortex data dir.
func DefaultDBPath() string {
	return persistence.DefaultSidecarPath(platform.DataDir())
}

// DefaultPath returns the flat-file era's savings.json location under the
// Gortex cache dir. The live ledger no longer writes it — the path is the
// default source for the one-shot legacy import (see Store.ImportLegacy).
//
// An absolute $XDG_CACHE_HOME is honoured; otherwise the location stays
// under os.UserCacheDir() — the historical default for this store, kept
// so an existing savings file is not orphaned. Returns an empty string
// when no cache dir can be resolved.
func DefaultPath() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v == "" || !filepath.IsAbs(v) {
		if base, err := os.UserCacheDir(); err != nil || base == "" {
			return ""
		}
	}
	return filepath.Join(platform.OSCacheDir(), "savings.json")
}

// DefaultEventsPath returns the flat-file era's savings.jsonl event-log
// path next to DefaultPath. Empty when the cache dir is unavailable.
func DefaultEventsPath() string {
	p := DefaultPath()
	if p == "" {
		return ""
	}
	return EventsPathFor(p)
}

// EventsPathFor returns the JSONL event-log path that corresponds to a
// flat-file cumulative savings JSON path — `<dir>/savings.jsonl` alongside
// the JSON file. Empty when storePath is empty.
func EventsPathFor(storePath string) string {
	if storePath == "" {
		return ""
	}
	dir := filepath.Dir(storePath)
	base := filepath.Base(storePath)
	ext := filepath.Ext(base)
	stem := base
	if ext != "" {
		stem = base[:len(base)-len(ext)]
	}
	return filepath.Join(dir, stem+".jsonl")
}

// Open opens the savings ledger inside the sidecar database at dbPath
// (creating tables as needed). An empty dbPath yields an in-memory-only
// store. The sidecar handle is process-shared: opening the same path the
// notes/memories managers use reuses their connection.
func Open(dbPath string) (*Store, error) {
	s := &Store{}
	s.mem = emptyFile()
	if dbPath == "" {
		return s, nil
	}
	sc, err := persistence.OpenSidecar(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open savings ledger: %w", err)
	}
	s.sc = sc
	s.flushEvery, s.flushEveryFromEnv = flushInterval()
	s.flushMax = DefaultFlushMax
	// Every sidecar-backed Store joins the registry for its shared handle,
	// so a read (or a Close) through ANY handle on this database flushes
	// the buffers of ALL of them. See the package doc's Exactness note.
	registerStore(sc, s)
	return s, nil
}

// ---------------------------------------------------------------------------
// Cross-handle flush registry
// ---------------------------------------------------------------------------

// persistence.OpenSidecar caches one *SidecarStore per absolute path, so two
// savings.Store values opened on the same ledger share a handle while owning
// separate buffers. Keying the registry on that shared handle is what makes
// "flush before reading" mean "flush every buffer that feeds this database",
// and it is generation-correct: closing the handle evicts it from the
// persistence cache, so a later Open allocates a fresh *SidecarStore and a
// fresh registry entry rather than inheriting stale peers.
var (
	openStoresMu sync.Mutex
	openStores   = map[*persistence.SidecarStore]map[*Store]struct{}{}
)

func registerStore(sc *persistence.SidecarStore, s *Store) {
	if sc == nil || s == nil {
		return
	}
	openStoresMu.Lock()
	defer openStoresMu.Unlock()
	set := openStores[sc]
	if set == nil {
		set = make(map[*Store]struct{}, 2)
		openStores[sc] = set
	}
	set[s] = struct{}{}
}

// forgetSidecar drops every Store registered against sc. Called when the
// shared handle itself is closed: the handle is dead for every holder, and a
// later OpenSidecar on the same path allocates a different *SidecarStore, so
// retaining the entry would only keep closed stores reachable forever.
func forgetSidecar(sc *persistence.SidecarStore) {
	if sc == nil {
		return
	}
	openStoresMu.Lock()
	delete(openStores, sc)
	openStoresMu.Unlock()
}

// sharedStores returns every live Store writing through s.sc, s included.
// The registry lock is released before the caller touches any of them: a
// peer's Flush takes that peer's mu and then commits outside it, and holding
// openStoresMu across that would serialise every ledger read in the process
// behind one transaction.
func (s *Store) sharedStores() []*Store {
	if s == nil || s.sc == nil {
		return nil
	}
	openStoresMu.Lock()
	set := openStores[s.sc]
	out := make([]*Store, 0, len(set)+1)
	self := false
	for p := range set {
		out = append(out, p)
		if p == s {
			self = true
		}
	}
	openStoresMu.Unlock()
	if !self {
		// s was never registered (or its handle has been closed): it is
		// still the store the caller asked about.
		out = append(out, s)
	}
	return out
}

// flushShared commits the buffers of every Store sharing this ledger handle.
// This — not Flush — is what a read path owes its caller: a reader that
// drained only its own (usually empty) buffer would report a stale total
// whenever another handle in the same process holds the live window, which
// is precisely the `gortex gain` case. Returns the first commit error.
func (s *Store) flushShared() error {
	if s == nil || s.sc == nil {
		return nil
	}
	var firstErr error
	for _, p := range s.sharedStores() {
		if err := p.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// dropBuffered discards this store's buffer without committing it. Only
// Reset uses it: a batch that landed after the DELETE would resurrect part
// of what was reset.
func (s *Store) dropBuffered() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.takeLocked()
	s.mu.Unlock()
}

// SetFlushBounds narrows (or widens) this store's flush window after Open —
// the seam the one-shot stdio server uses to trade the daemon's minute-scale
// coalescing for a few seconds of exposure (see OneshotFlushInterval).
//
// A non-positive argument leaves that bound alone. An interval the operator
// set through GORTEX_SAVINGS_FLUSH_INTERVAL is never overwritten: the knob is
// honoured, not silently raised or dropped. Tightening re-arms an already
// running timer on the new interval, and commits immediately if the new count
// bound is already met, so the narrower window takes effect for observations
// that are already buffered rather than only for the next ones.
func (s *Store) SetFlushBounds(interval time.Duration, max int) {
	if s == nil || s.sc == nil {
		return
	}
	s.mu.Lock()
	if interval > 0 && !s.flushEveryFromEnv {
		s.flushEvery = interval
	}
	if max > 0 {
		s.flushMax = max
	}
	var batch []persistence.SavingsEvent
	switch {
	case len(s.buf) == 0:
		// Nothing armed; the next append picks up the new bounds.
	case s.flushEvery <= 0 || (s.flushMax > 0 && len(s.buf) >= s.flushMax):
		batch = s.takeLocked()
	default:
		if s.flushTimer != nil {
			s.flushTimer.Stop()
			s.flushTimer = nil
		}
		s.armLocked()
	}
	s.mu.Unlock()
	if len(batch) > 0 {
		_ = s.commit(batch)
	}
}

// FlushBounds reports the effective flush window. Diagnostic — it is what
// makes "the one-shot entry point actually tightened the bound" checkable
// without waiting out a timer.
func (s *Store) FlushBounds() (time.Duration, int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushEvery, s.flushMax
}

func emptyFile() File {
	return File{
		Version:     schemaVersion,
		PerRepo:     make(map[string]*Totals),
		PerLanguage: make(map[string]*Totals),
	}
}

// AddObservation books one source-reading tool call. Sidecar-backed stores
// buffer the observation and commit it with the rest of its flush window
// (see the package doc for the durability trade); in-memory stores accumulate
// immediately.
//
// This is on the hot path of every source-reading tool call, so the
// sidecar-backed path must stay an append: the flush that a full buffer
// triggers is performed outside the store lock.
func (s *Store) AddObservation(o Observation) {
	if s == nil {
		return
	}
	if o.Saved < 0 {
		o.Saved = 0
	}
	now := time.Now().UTC()

	if s.sc != nil {
		ev := persistence.SavingsEvent{
			TS:        now,
			SessionID: o.SessionID,
			Tool:      o.Tool,
			Repo:      o.Repo,
			Language:  o.Language,
			Model:     o.Model,
			Client:    o.Client,
			Returned:  o.Returned,
			Saved:     o.Saved,
		}
		s.mu.Lock()
		if s.flushEvery <= 0 {
			s.mu.Unlock()
			_ = s.commit([]persistence.SavingsEvent{ev})
			return
		}
		s.buf = append(s.buf, ev)
		if s.flushMax > 0 && len(s.buf) >= s.flushMax {
			batch := s.takeLocked()
			s.mu.Unlock()
			_ = s.commit(batch)
			return
		}
		s.armLocked()
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mem.FirstSeen.IsZero() {
		s.mem.FirstSeen = now
	}
	s.mem.LastUpdated = now
	s.mem.Totals.TokensSaved += o.Saved
	s.mem.Totals.TokensReturned += o.Returned
	s.mem.Totals.CallsCounted++
	addBucket := func(bucket map[string]*Totals, key string) {
		if key == "" {
			return
		}
		t := bucket[key]
		if t == nil {
			t = &Totals{}
			bucket[key] = t
		}
		t.TokensSaved += o.Saved
		t.TokensReturned += o.Returned
		t.CallsCounted++
	}
	addBucket(s.mem.PerRepo, o.Repo)
	addBucket(s.mem.PerLanguage, o.Language)
	s.memEvents = append(s.memEvents, Event{
		TS:        now,
		SessionID: o.SessionID,
		Repo:      o.Repo,
		Language:  o.Language,
		Tool:      o.Tool,
		Model:     o.Model,
		Client:    o.Client,
		Returned:  o.Returned,
		Saved:     o.Saved,
	})
}

// Snapshot returns the current cumulative totals. Sidecar-backed stores
// read the live aggregates, so the snapshot reflects every writer process,
// not just this one. FirstSeen stays the zero time until something has
// actually been recorded — callers must not present it as "tracking since"
// when it is zero.
//
// On a read error the returned File is empty and the error is non-nil —
// callers that render the empty state must distinguish "nothing recorded"
// from "ledger unreadable".
//
// Buffered observations are committed first — those of every Store in this
// process sharing the ledger handle, not just this one — so the snapshot is
// exact for this process however long the flush window is, except for a
// batch another goroutine has already detached and is mid-COMMIT on.
func (s *Store) Snapshot() (File, error) {
	if s == nil {
		return emptyFile(), nil
	}
	_ = s.flushShared()
	if s.sc == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		cp := s.mem
		cp.PerRepo = copyTotalsMap(s.mem.PerRepo)
		cp.PerLanguage = copyTotalsMap(s.mem.PerLanguage)
		cp.DroppedObservations = s.dropped.Load()
		return cp, nil
	}

	buckets, firstSeen, lastUpdated, err := s.sc.SavingsTotals()
	if err != nil {
		return emptyFile(), fmt.Errorf("savings totals read: %w", err)
	}
	out := emptyFile()
	out.FirstSeen = firstSeen
	out.LastUpdated = lastUpdated
	out.DroppedObservations = s.dropped.Load()
	for bucket, r := range buckets {
		t := &Totals{TokensSaved: r.Saved, TokensReturned: r.Returned, CallsCounted: r.Calls}
		switch {
		case bucket == "":
			out.Totals = *t
		case len(bucket) > 5 && bucket[:5] == "repo:":
			out.PerRepo[bucket[5:]] = t
		case len(bucket) > 5 && bucket[:5] == "lang:":
			out.PerLanguage[bucket[5:]] = t
		}
	}
	return out, nil
}

// ToolTotals returns the per-tool aggregate over events with TS >= since
// (zero = all time), sorted by tokens saved descending. Sidecar-backed
// stores aggregate in SQL so the dashboard never materializes the full
// event history just to render a breakdown.
func (s *Store) ToolTotals(since time.Time) ([]ToolTotal, error) {
	if s == nil {
		return nil, nil
	}
	_ = s.flushShared()
	if s.sc == nil {
		s.mu.Lock()
		events := FilterSince(s.memEvents, since)
		s.mu.Unlock()
		_, per := AggregateByTool(events)
		return per, nil
	}
	rows, err := s.sc.SavingsToolTotals(since)
	if err != nil {
		return nil, err
	}
	out := make([]ToolTotal, 0, len(rows))
	for _, r := range rows {
		out = append(out, ToolTotal{Tool: r.Tool, Totals: Totals{
			TokensSaved:    r.Saved,
			TokensReturned: r.Returned,
			CallsCounted:   r.Calls,
		}})
	}
	return out, nil
}

// Close flushes anything buffered, then releases the underlying sidecar
// handle (dropping it from the process-wide cache). Safe on nil and
// in-memory stores. The daemon keeps its store open for the process
// lifetime — its shutdown chain calls Server.FlushSavings — while tests and
// one-shot CLI paths should Close when done.
//
// The flush covers every Store sharing this handle, not just this one:
// closing it takes the database away from all of them, so a peer's buffered
// window would otherwise be dropped ("database is closed") rather than
// merely delayed. Observations booked by a peer AFTER this returns still hit
// a dead handle and are counted in DroppedObservations — closing a shared
// ledger out from under a live writer remains the caller's problem.
func (s *Store) Close() error {
	if s == nil || s.sc == nil {
		return nil
	}
	ferr := s.flushShared()
	sc := s.sc
	forgetSidecar(sc)
	if err := sc.Close(); err != nil {
		return err
	}
	return ferr
}

// EventsSince returns the per-call events with TS >= since, oldest first.
// since=zero returns the full event history.
func (s *Store) EventsSince(since time.Time) ([]Event, error) {
	if s == nil {
		return nil, nil
	}
	_ = s.flushShared()
	if s.sc == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return FilterSince(s.memEvents, since), nil
	}
	rows, err := s.sc.SavingsEventsSince(since)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, Event{
			TS:        r.TS,
			SessionID: r.SessionID,
			Repo:      r.Repo,
			Language:  r.Language,
			Tool:      r.Tool,
			Model:     r.Model,
			Client:    r.Client,
			Returned:  r.Returned,
			Saved:     r.Saved,
		})
	}
	return out, nil
}

// ModelTotals returns the per-model aggregate over events with TS >= since
// (zero = all time), tokens-saved descending. Only calls with a resolved
// model are counted — clients that don't surface their model are absent.
func (s *Store) ModelTotals(since time.Time) ([]DimTotal, error) {
	return s.dimTotals(since, AggregateByModel, func(t time.Time) ([]persistence.SavingsDimRow, error) {
		return s.sc.SavingsModelTotals(t)
	})
}

// ClientTotals returns the per-MCP-client aggregate over events with
// TS >= since (zero = all time), tokens-saved descending.
func (s *Store) ClientTotals(since time.Time) ([]DimTotal, error) {
	return s.dimTotals(since, AggregateByClient, func(t time.Time) ([]persistence.SavingsDimRow, error) {
		return s.sc.SavingsClientTotals(t)
	})
}

// dimTotals is the shared model/client breakdown: aggregate in SQL when
// sidecar-backed, fold the in-memory event log otherwise.
func (s *Store) dimTotals(since time.Time, memAgg func([]Event) []DimTotal, scQuery func(time.Time) ([]persistence.SavingsDimRow, error)) ([]DimTotal, error) {
	if s == nil {
		return nil, nil
	}
	_ = s.flushShared()
	if s.sc == nil {
		s.mu.Lock()
		events := FilterSince(s.memEvents, since)
		s.mu.Unlock()
		return memAgg(events), nil
	}
	rows, err := scQuery(since)
	if err != nil {
		return nil, err
	}
	out := make([]DimTotal, 0, len(rows))
	for _, r := range rows {
		out = append(out, DimTotal{Name: r.Name, Totals: Totals{
			TokensSaved:    r.Saved,
			TokensReturned: r.Returned,
			CallsCounted:   r.Calls,
		}})
	}
	return out, nil
}

// armLocked starts the flush timer if it is not already running. Caller
// holds mu.
func (s *Store) armLocked() {
	if s.flushTimer != nil || s.flushEvery <= 0 {
		return
	}
	s.flushTimer = time.AfterFunc(s.flushEvery, func() { _ = s.Flush() })
}

// takeLocked detaches the buffered batch and disarms the timer. Caller holds
// mu. Returns nil when nothing is buffered.
func (s *Store) takeLocked() []persistence.SavingsEvent {
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	batch := s.buf
	s.buf = nil
	return batch
}

// commit writes one batch as a single sidecar transaction. Never called with
// mu held: the ledger write must not block the tool calls appending behind
// it. A failure drops the batch (accounting must never fail a tool call) and
// is counted, with one stderr line the first time so a persistently failing
// ledger is diagnosable instead of looking like "nothing recorded".
func (s *Store) commit(batch []persistence.SavingsEvent) error {
	if len(batch) == 0 {
		return nil
	}
	err := s.sc.AddSavingsObservations(batch)
	if err != nil {
		s.dropped.Add(int64(len(batch)))
		s.warnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "gortex: savings ledger write failed, observations will be dropped: %v\n", err)
		})
	}
	return err
}

// Flush commits everything buffered on THIS store, in one transaction. Safe
// on nil and in-memory stores (nothing is ever buffered there). Called by the
// timer and by the daemon's shutdown chain via Server.FlushSavings — the
// daemon owns exactly one handle, so its own buffer is the whole window.
// Read paths and Close use flushShared instead, which additionally drains
// every other Store on the same ledger handle.
func (s *Store) Flush() error {
	if s == nil || s.sc == nil {
		return nil
	}
	s.mu.Lock()
	batch := s.takeLocked()
	s.mu.Unlock()
	return s.commit(batch)
}

// Pending reports how many observations are buffered but not yet committed.
// Diagnostic — it is what makes the coalescing observable without reading
// the database.
func (s *Store) Pending() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

// Reset wipes all cumulative data and events. Used by
// `gortex savings --reset`. The legacy-import mark survives, so flat
// files already imported (and renamed *.bak) are not re-imported.
func (s *Store) Reset() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.mem = emptyFile()
	s.memEvents = nil
	// Buffered observations are discarded, not flushed: `gortex savings
	// --reset` wipes the ledger, and a pending batch that landed after the
	// DELETE would resurrect part of what was reset.
	s.takeLocked()
	s.mu.Unlock()
	if s.sc == nil {
		return nil
	}
	// Same reasoning across handles: every other Store on this ledger would
	// otherwise flush its window back over the reset. (A writer in another
	// PROCESS still can — see the package doc.)
	for _, p := range s.sharedStores() {
		if p != s {
			p.dropBuffered()
		}
	}
	return s.sc.ResetSavings()
}

// ImportLegacy imports the flat-file era's ledger — the cumulative
// savings.json at jsonPath and its sibling savings.jsonl — into the
// sidecar, then renames both files to *.bak. Idempotent: a migration
// mark guarantees the import runs once per sidecar, including when
// there was nothing to import. In-memory stores skip the import.
//
// The cumulative file was flush-batched while the event log appended
// eagerly, so a SIGKILL-era ledger can carry fewer totals than its own
// events; totals are floored per bucket and per field at what the
// events reconstruct, or "Last 7 days" could exceed "All time".
func (s *Store) ImportLegacy(jsonPath string) error {
	if s == nil || s.sc == nil || jsonPath == "" {
		return nil
	}
	eventsPath := EventsPathFor(jsonPath)
	if s.sc.SavingsLegacyImportDone() {
		// Self-heal lingering files: a crash between the import commit
		// and the renames — or an old-version daemon recreating the
		// files after migration — leaves live-looking flat files no
		// future open would ever touch. Their content is intentionally
		// not imported (the mark owns that decision); sweep them aside.
		_ = renameLegacySavings(jsonPath)
		_ = renameLegacySavings(eventsPath)
		return nil
	}

	legacy, err := readLegacyFile(jsonPath)
	if err != nil {
		return err
	}
	// A hard read error must abort without marking or renaming so the
	// import retries next open — importing a truncated event log and
	// renaming the original away would make the loss permanent.
	events, err := LoadEvents(eventsPath, time.Time{})
	if err != nil {
		return fmt.Errorf("read legacy savings events: %w", err)
	}

	// Rebuild totals from the events unconditionally...
	rebuilt := make(map[string]persistence.SavingsTotalsRow)
	for _, ev := range events {
		bump := func(bucket string) {
			r := rebuilt[bucket]
			r.Saved += ev.Saved
			r.Returned += ev.Returned
			r.Calls++
			rebuilt[bucket] = r
		}
		bump("")
		if ev.Repo != "" {
			bump("repo:" + ev.Repo)
		}
		if ev.Language != "" {
			bump("lang:" + ev.Language)
		}
	}

	// ...then overlay the cumulative file's (possibly larger, possibly
	// flush-lagged) numbers, taking the per-field max of the two views.
	buckets := rebuilt
	var firstSeen, lastUpdated time.Time
	if legacy != nil {
		overlay := func(bucket string, t Totals) {
			r := buckets[bucket]
			r.Saved = max(r.Saved, t.TokensSaved)
			r.Returned = max(r.Returned, t.TokensReturned)
			r.Calls = max(r.Calls, t.CallsCounted)
			buckets[bucket] = r
		}
		overlay("", legacy.Totals)
		for k, v := range legacy.PerRepo {
			overlay("repo:"+k, *v)
		}
		for k, v := range legacy.PerLanguage {
			overlay("lang:"+k, *v)
		}
		firstSeen, lastUpdated = legacy.FirstSeen, legacy.LastUpdated
	}
	if len(events) > 0 {
		if firstSeen.IsZero() || events[0].TS.Before(firstSeen) {
			firstSeen = events[0].TS
		}
		if last := events[len(events)-1].TS; last.After(lastUpdated) {
			lastUpdated = last
		}
	}

	pevents := make([]persistence.SavingsEvent, 0, len(events))
	for _, ev := range events {
		pevents = append(pevents, persistence.SavingsEvent{
			TS:        ev.TS,
			SessionID: ev.SessionID,
			Tool:      ev.Tool,
			Repo:      ev.Repo,
			Language:  ev.Language,
			Returned:  ev.Returned,
			Saved:     ev.Saved,
		})
	}
	if err := s.sc.ImportLegacySavings(buckets, firstSeen, lastUpdated, pevents); err != nil {
		return err
	}
	return errors.Join(
		renameLegacySavings(jsonPath),
		renameLegacySavings(eventsPath),
	)
}

// renameLegacySavings moves an already-imported flat file aside to
// <file>.bak. Never deletes; a missing file is fine. A rename failure
// is reported so the caller can surface it — silently leaving the file
// in place makes it look live when it no longer is.
func renameLegacySavings(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if err := os.Rename(path, path+".bak"); err != nil {
		return fmt.Errorf("rename legacy savings file: %w", err)
	}
	return nil
}

// readLegacyFile loads a flat-file era savings.json. Returns (nil, nil)
// when the file doesn't exist; corrupt or version-mismatched files are
// skipped the same way (the import has nothing trustworthy to carry over).
func readLegacyFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read legacy savings: %w", err)
	}
	var loaded File
	if jerr := json.Unmarshal(data, &loaded); jerr != nil || loaded.Version != schemaVersion {
		return nil, nil
	}
	if loaded.PerRepo == nil {
		loaded.PerRepo = make(map[string]*Totals)
	}
	if loaded.PerLanguage == nil {
		loaded.PerLanguage = make(map[string]*Totals)
	}
	// JSON null map values unmarshal to nil pointers — drop them here,
	// the single choke point, so no consumer dereferences one.
	for k, v := range loaded.PerRepo {
		if v == nil {
			delete(loaded.PerRepo, k)
		}
	}
	for k, v := range loaded.PerLanguage {
		if v == nil {
			delete(loaded.PerLanguage, k)
		}
	}
	return &loaded, nil
}

// copyTotalsMap returns a deep copy of a name → Totals map.
func copyTotalsMap(src map[string]*Totals) map[string]*Totals {
	if src == nil {
		return make(map[string]*Totals)
	}
	dst := make(map[string]*Totals, len(src))
	for k, v := range src {
		cp := *v
		dst[k] = &cp
	}
	return dst
}
