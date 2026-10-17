package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// The boundary-receipt write sequence.
//
// A contract analysis job proves a read by comparing a file's receipt stamp
// taken before the read with one taken after it. Row identity alone cannot do
// that: a same-identity rewrite goes (I,accepted) → (I,pending) → (I,accepted),
// and an evict and its re-add are separate transactions. Every receipt write
// therefore bumps a process-local counter for the path it writes BEFORE the
// write executes, and per repository a begun counter before and a done counter
// after the transaction ends (commit or rollback). A scan that samples
// (begun, done) before reading rows, then reads rows, then seqs, sees every
// write that overlaps the interval between two scans as a row or seq change.
//
// The counters are not persisted: a restart loses them, and with them every
// stamp a job held. A chain fold or flatten copies rows verbatim and bumps
// nothing, so compaction does not make a settled file look moved.
//
// A mutation that wrote every core row of its files but was never accepted (a
// newer mutation took the owner over) settles its receipts at their write
// seqs: such a row stays unaccepted, and its file's core rows are complete
// until the next receipt write of the path begins. Every ended receipt write
// and every settle advances an event counter a job's retry waits on.
//
// Each path also has a core-write window: the staging write that rewrites a
// receipt unaccepted before its mutation's core writes opens it as it begins,
// and it closes when the mutation's body ends its core writes, when the
// staged row is accepted, or when the mutation settles it. While it is open
// the path's core rows may be between an evict and its add; once it is
// closed they are complete for the staged row whether or not anything
// accepted it. A window opened again is a later write seq, so an unchanged
// seq and a closed window before and after a read mean no core write of the
// path overlapped the read. A body that fails leaves its window open until the
// next mutation of the path.
//
// Each window records when it opened and closed, and a window opened after
// the previous one closed records the quiet gap between them in a bounded
// per-path ring: the time a contract job has to ask its reads again between
// two applies of the path.
//
// A mutation whose body failed after it staged its receipts marks the windows
// it left open as orphaned at the seqs that opened them: nothing of that
// mutation will close them. The mark holds while the window it names is open
// and is dropped at every close of the path, whoever closes it.
type contractReceiptWriteSequence struct {
	mu      sync.Mutex
	seq     map[contractReceiptWriteKey]uint64
	settled map[contractReceiptWriteKey]uint64
	// writing holds the write seq that opened each open core-write window.
	writing map[contractReceiptWriteKey]uint64
	// openedAt holds when each open window opened; closed holds when each
	// path's last window opened and closed.
	openedAt map[contractReceiptWriteKey]time.Time
	closed   map[contractReceiptWriteKey]contractCoreWindowSpan
	closes   map[contractReceiptWriteKey][]contractCoreWindowSpan
	gaps     map[contractReceiptWriteKey][]ContractCoreWindowGap
	// orphaned holds the opener seq of each window a failed mutation left
	// open, and when it was marked.
	orphaned map[contractReceiptWriteKey]contractCoreOrphan
	repos    map[string]*contractReceiptWriteBracket
	events   uint64
	changed  chan struct{}
	// orphanEvents counts orphan marks; orphanChanged is closed by the next.
	orphanEvents  uint64
	orphanChanged chan struct{}
	// now replaces time.Now in tests.
	now func() time.Time
}

// contractCoreOrphan is one orphan mark: the seq of the window it names.
type contractCoreOrphan struct {
	seq uint64
	at  time.Time
}

// contractCoreWindowSpan is when one closed core-write window opened and
// closed.
type contractCoreWindowSpan struct {
	opened, closed time.Time
	seq            uint64
}

// contractCoreWindowGapRing bounds the gaps kept per path.
const contractCoreWindowGapRing = 16

// ContractCoreWindowGap is the quiet gap before one core-write window of a
// path opened: Quiet runs from the close of the path's previous window to
// this window's open, and OpenPrev is how long that previous window was
// open. Seq is the write seq that opened this window.
type ContractCoreWindowGap struct {
	Path            string
	Checkout        string
	ClosedAt        time.Time
	Seq             uint64
	OpenedAt        time.Time
	Quiet, OpenPrev time.Duration
}

func (w *contractReceiptWriteSequence) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// openLocked opens key's core-write window at seq. A window opened after the
// path's previous one closed records the gap between them; one opened again
// while it is open records nothing.
func (w *contractReceiptWriteSequence) openLocked(key contractReceiptWriteKey, seq uint64, at time.Time) {
	if w.openedAt == nil {
		w.openedAt = make(map[contractReceiptWriteKey]time.Time)
	}
	if _, open := w.writing[key]; !open {
		if last, ok := w.closed[key]; ok {
			if w.gaps == nil {
				w.gaps = make(map[contractReceiptWriteKey][]ContractCoreWindowGap)
			}
			ring := append(w.gaps[key], ContractCoreWindowGap{Path: key.path, Checkout: key.checkout, ClosedAt: last.closed, Seq: seq, OpenedAt: at,
				Quiet: at.Sub(last.closed), OpenPrev: last.closed.Sub(last.opened)})
			if len(ring) > contractCoreWindowGapRing {
				ring = slices.Delete(ring, 0, len(ring)-contractCoreWindowGapRing)
			}
			w.gaps[key] = ring
		}
	}
	w.writing[key] = seq
	w.openedAt[key] = at
}

// closeLocked closes key's open core-write window at at, and drops the
// path's orphan mark.
func (w *contractReceiptWriteSequence) closeLocked(key contractReceiptWriteKey, at time.Time) {
	delete(w.orphaned, key)
	if _, open := w.writing[key]; !open {
		return
	}
	if w.closed == nil {
		w.closed = make(map[contractReceiptWriteKey]contractCoreWindowSpan)
	}
	span := contractCoreWindowSpan{opened: w.openedAt[key], closed: at, seq: w.writing[key]}
	w.closed[key] = span
	if w.closes == nil {
		w.closes = make(map[contractReceiptWriteKey][]contractCoreWindowSpan)
	}
	history := append(w.closes[key], span)
	if len(history) > contractCoreWindowGapRing {
		history = slices.Delete(history, 0, len(history)-contractCoreWindowGapRing)
	}
	w.closes[key] = history
	delete(w.writing, key)
	delete(w.openedAt, key)
}

// contractCoreWindow is what one receipt write does to the core-write windows
// of the paths it writes.
type contractCoreWindow int

const (
	contractCoreWindowKept contractCoreWindow = iota
	// contractCoreWindowOpened stages unaccepted rows ahead of their
	// mutation's core writes and opens their windows as it begins.
	contractCoreWindowOpened
	// contractCoreWindowClosed accepts staged rows and closes their windows
	// once it commits.
	contractCoreWindowClosed
)

// notifyLocked advances the event counter and wakes its waiters.
func (w *contractReceiptWriteSequence) notifyLocked() {
	w.events++
	if w.changed != nil {
		close(w.changed)
		w.changed = nil
	}
}

type contractReceiptWriteKey struct{ repo, checkout, path string }

// contractReceiptWriteStage, when set by a test, runs at "begun" (after the
// begun and seq bumps, before the transaction) and at "ending" (after commit
// or rollback, before the done bump) of every receipt write, with the paths
// the write names.
var contractReceiptWriteStage atomic.Pointer[func(stage string, paths []string)]

// SetContractReceiptWriteHookForTest installs a hook at the receipt write
// stages so a test can hold a write in flight between a scan's phases.
func SetContractReceiptWriteHookForTest(hook func(stage string, paths []string)) (restore func()) {
	previous := contractReceiptWriteStage.Swap(&hook)
	return func() { contractReceiptWriteStage.Store(previous) }
}

func contractReceiptWriteAt(stage string, keys []contractReceiptWriteKey) {
	hook := contractReceiptWriteStage.Load()
	if hook == nil || *hook == nil {
		return
	}
	paths := make([]string, 0, len(keys))
	for _, key := range keys {
		paths = append(paths, key.path)
	}
	(*hook)(stage, paths)
}

// contractCoreWindowOpenHook, when set by a test, observes each primary
// core-write window as it opens, under the write sequence's lock.
var contractCoreWindowOpenHook atomic.Pointer[func(repo, checkout, path string)]

// SetContractCoreWindowOpenHookForTest installs a hook called as each primary
// core-write window opens, so a test can check what holds it open.
func SetContractCoreWindowOpenHookForTest(hook func(repo, checkout, path string)) (restore func()) {
	previous := contractCoreWindowOpenHook.Swap(&hook)
	return func() { contractCoreWindowOpenHook.Store(previous) }
}

// ForgetContractReceiptWritesForTest drops what this process knows of the
// receipt writes of paths, as a restarted process starts: their rows read
// as written by an earlier one (seq 0, no window). Tests only.
func (s *Store) ForgetContractReceiptWritesForTest(repo, checkout string, paths []string) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, path := range paths {
		key := contractReceiptWriteKey{repo, checkout, path}
		delete(w.seq, key)
		delete(w.settled, key)
		delete(w.writing, key)
		delete(w.openedAt, key)
		delete(w.closed, key)
		delete(w.closes, key)
		delete(w.gaps, key)
		delete(w.orphaned, key)
	}
	w.notifyLocked()
}

func contractCoreWindowOpenedAt(key contractReceiptWriteKey) {
	if hook := contractCoreWindowOpenHook.Load(); hook != nil && *hook != nil {
		(*hook)(key.repo, key.checkout, key.path)
	}
}

type contractReceiptWriteBracket struct{ begun, done uint64 }

// beginContractReceiptWrites bumps the begun counter of every repository
// written and the seq of every path written, opens the core-write windows of
// staged unaccepted rows when window says so, and returns the matching done
// bump. The caller holds the write gate and defers done past its commit or
// rollback, reporting whether it committed: an accepting write closes its
// rows' windows only then.
func (s *Store) beginContractReceiptWrites(rows []graph.ContractBoundaryReceipt, sources []graph.ContractBoundaryReceiptSource, window contractCoreWindow) func(committed bool) {
	if len(rows) == 0 && len(sources) == 0 {
		return func(bool) {}
	}
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seq == nil {
		w.seq = make(map[contractReceiptWriteKey]uint64)
		w.repos = make(map[string]*contractReceiptWriteBracket)
	}
	if w.writing == nil {
		w.writing = make(map[contractReceiptWriteKey]uint64)
	}
	var repos []string
	var keys []contractReceiptWriteKey
	bump := func(key contractReceiptWriteKey) {
		w.seq[key]++
		keys = append(keys, key)
		if !slices.Contains(repos, key.repo) {
			repos = append(repos, key.repo)
		}
	}
	opened := w.clock()
	for _, row := range rows {
		key := contractReceiptWriteKey{row.RepoPrefix, row.CheckoutID, row.FilePath}
		bump(key)
		if window == contractCoreWindowOpened && !row.Accepted {
			w.openLocked(key, w.seq[key], opened)
			if s.viewGen == 0 {
				contractCoreWindowOpenedAt(key)
			}
		}
	}
	for _, source := range sources {
		bump(contractReceiptWriteKey{source.Receipt.RepoPrefix, source.TargetCheckoutID, source.Receipt.FilePath})
	}
	for _, repo := range repos {
		bracket := w.repos[repo]
		if bracket == nil {
			bracket = &contractReceiptWriteBracket{}
			w.repos[repo] = bracket
		}
		bracket.begun++
	}
	w.mu.Unlock()
	contractReceiptWriteAt("begun", keys)
	w.mu.Lock()
	return func(committed bool) {
		contractReceiptWriteAt("ending", keys)
		w.mu.Lock()
		defer w.mu.Unlock()
		if committed && window == contractCoreWindowClosed {
			// The window closes when the accept commits, not when it began.
			closed := w.clock()
			for _, row := range rows {
				w.closeLocked(contractReceiptWriteKey{row.RepoPrefix, row.CheckoutID, row.FilePath}, closed)
			}
		}
		for _, repo := range repos {
			w.repos[repo].done++
		}
		w.notifyLocked()
	}
}

// EndContractCoreWrites closes the core-write windows one repository and
// actor's mutation opened at the given write seqs: its body ended every core
// write of those paths. A path written again since keeps its newer window.
// Closing moves no row, so the repository's bracket steps as a write would,
// and waiters are woken.
func (s *Store) EndContractCoreWrites(repo, checkout string, seqs map[string]uint64) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	closed := false
	at := w.clock()
	for path, seq := range seqs {
		key := contractReceiptWriteKey{repo, checkout, path}
		if seq == 0 || w.writing[key] != seq {
			continue
		}
		w.closeLocked(key, at)
		closed = true
	}
	if !closed {
		return
	}
	if bracket := w.repos[repo]; bracket != nil {
		bracket.begun++
		bracket.done++
	}
	w.notifyLocked()
}

// SettleContractReceiptWrites marks one repository and actor's receipts
// settled at the given write seqs: the mutation that wrote them completed its
// core writes without accepting them. A path written again since keeps its
// newer, unsettled seq. A settled path's window closes with the settle, in
// the one event that announces both. Settling moves no row, so the
// repository's bracket steps as a write would: a full scan taken before the
// settle is never reused for one after it.
func (s *Store) SettleContractReceiptWrites(repo, checkout string, seqs map[string]uint64) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	settled := false
	at := w.clock()
	for path, seq := range seqs {
		key := contractReceiptWriteKey{repo, checkout, path}
		if seq == 0 || w.seq[key] != seq {
			continue
		}
		if w.settled == nil {
			w.settled = make(map[contractReceiptWriteKey]uint64)
		}
		w.settled[key] = seq
		if w.writing[key] == seq {
			w.closeLocked(key, at)
		}
		settled = true
	}
	if !settled {
		return
	}
	if bracket := w.repos[repo]; bracket != nil {
		bracket.begun++
		bracket.done++
	}
	w.notifyLocked()
}

// ContractReceiptWriteState is one path's write seq, whether the mutation
// that wrote it settled it, and whether a core-write window of the path is
// open.
type ContractReceiptWriteState struct {
	Seq     uint64
	Settled bool
	Writing bool
}

// ContractReceiptWriteStates returns the write state of each path of one
// repository and actor. A path never written in this process has seq 0, is
// not settled and has no open window.
func (s *Store) ContractReceiptWriteStates(repo, checkout string, paths []string) map[string]ContractReceiptWriteState {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]ContractReceiptWriteState, len(paths))
	for _, path := range paths {
		key := contractReceiptWriteKey{repo, checkout, path}
		seq := w.seq[key]
		_, writing := w.writing[key]
		out[path] = ContractReceiptWriteState{Seq: seq, Settled: seq > 0 && w.settled[key] == seq, Writing: writing}
	}
	return out
}

// ContractReceiptEvents returns the count of ended receipt writes and settles
// in this process, and a channel closed by the next one.
func (s *Store) ContractReceiptEvents() (uint64, <-chan struct{}) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	return w.events, w.changed
}

// ContractReceiptWriteSample reports how many receipt write transactions of a
// repository have begun and how many have ended. begun == done means none is
// in flight. Scans take it before reading any row.
func (s *Store) ContractReceiptWriteSample(repo string) (begun, done uint64) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	if bracket := w.repos[repo]; bracket != nil {
		return bracket.begun, bracket.done
	}
	return 0, 0
}

// ContractReceiptWriteSeqs returns the write seq of each path of one
// repository and actor. A path never written in this process has seq 0.
func (s *Store) ContractReceiptWriteSeqs(repo, checkout string, paths []string) map[string]uint64 {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]uint64, len(paths))
	for _, path := range paths {
		out[path] = w.seq[contractReceiptWriteKey{repo, checkout, path}]
	}
	return out
}

// ContractCoreWindow is one open core-write window of a primary path: the
// write seq that opened it and when. MarkedAt is when a failed mutation
// marked it orphaned, zero for a window not marked.
type ContractCoreWindow struct {
	Checkout, Path string
	OpenerSeq      uint64
	OpenedAt       time.Time
	MarkedAt       time.Time
}

// OrphanContractCoreWrites marks the core-write windows one repository and
// actor's mutation opened at the given write seqs and left open: its body
// failed after staging their receipts, and nothing of it will close them. A
// path written again since keeps its newer window unmarked. Marking moves no
// row and closes nothing; it wakes ContractCoreOrphanEvents waiters.
func (s *Store) OrphanContractCoreWrites(repo, checkout string, seqs map[string]uint64) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.clock()
	marked := false
	for path, seq := range seqs {
		key := contractReceiptWriteKey{repo, checkout, path}
		if seq == 0 || w.writing[key] != seq {
			continue
		}
		if w.orphaned == nil {
			w.orphaned = make(map[contractReceiptWriteKey]contractCoreOrphan)
		}
		w.orphaned[key] = contractCoreOrphan{seq: seq, at: at}
		marked = true
	}
	if !marked {
		return
	}
	w.orphanEvents++
	if w.orphanChanged != nil {
		close(w.orphanChanged)
		w.orphanChanged = nil
	}
}

// ContractCoreOrphanEvents returns the count of orphan marks in this process
// and a channel closed by the next one.
func (s *Store) ContractCoreOrphanEvents() (uint64, <-chan struct{}) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.orphanChanged == nil {
		w.orphanChanged = make(chan struct{})
	}
	return w.orphanEvents, w.orphanChanged
}

// OpenContractCoreWindows returns every open core-write window of one
// repository's primary paths, by checkout then path. Every primary window
// opens under the repository's mutation lane, so a caller holding that lane
// sees only windows nothing is still writing in.
func (s *Store) OpenContractCoreWindows(repo string) []ContractCoreWindow {
	return s.contractCoreWindows(repo, false)
}

// OrphanedContractCoreWindows returns the open windows of one repository a
// failed mutation marked orphaned, by checkout then path.
func (s *Store) OrphanedContractCoreWindows(repo string) []ContractCoreWindow {
	return s.contractCoreWindows(repo, true)
}

func (s *Store) contractCoreWindows(repo string, orphanedOnly bool) []ContractCoreWindow {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []ContractCoreWindow
	for key, seq := range w.writing {
		if key.repo != repo {
			continue
		}
		window := ContractCoreWindow{Checkout: key.checkout, Path: key.path, OpenerSeq: seq, OpenedAt: w.openedAt[key]}
		if mark, ok := w.orphaned[key]; ok && mark.seq == seq {
			window.MarkedAt = mark.at
		} else if orphanedOnly {
			continue
		}
		out = append(out, window)
	}
	slices.SortFunc(out, func(a, b ContractCoreWindow) int {
		if a.Checkout != b.Checkout {
			return strings.Compare(a.Checkout, b.Checkout)
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// ContractCoreWindowGaps returns the recorded quiet gaps of one repository's
// primary paths whose window opened at or after since, oldest first per path,
// paths in the order given. Each path keeps only its latest gaps.
func (s *Store) ContractCoreWindowGaps(repo string, paths []string, since time.Time) []ContractCoreWindowGap {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []ContractCoreWindowGap
	for _, path := range paths {
		for _, gap := range w.gaps[contractReceiptWriteKey{repo, "", path}] {
			if !gap.OpenedAt.Before(since) {
				out = append(out, gap)
			}
		}
	}
	return out
}

// ContractBoundaryReceiptStamp is a receipt row's identity without its
// payload: what a contract job compares before and after a read.
type ContractBoundaryReceiptStamp struct {
	FilePath, Fingerprint, SourceFingerprint string
	Accepted                                 bool
}

// ContractBoundaryReceiptStampsContext reads the non-payload columns of this
// generation's receipt rows for one repository and actor: every row when
// paths is nil, otherwise the named paths. The covering stamp index keeps
// seeks and returned columns off the primary table's receipt overflow pages.
func (s *Store) ContractBoundaryReceiptStampsContext(ctx context.Context, repo, actor string, paths []string) (map[string]ContractBoundaryReceiptStamp, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]ContractBoundaryReceiptStamp)
	const columns = `SELECT file_path,fingerprint,source_fingerprint,accepted FROM generation_contract_boundary_receipt INDEXED BY contract_boundary_receipt_stamps WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`
	queries := [][]any{{columns, s.viewGen, repo, actor}}
	if paths != nil {
		queries = nil
		unique := slices.Clone(paths)
		slices.Sort(unique)
		unique = slices.Compact(unique)
		for start := 0; start < len(unique); start += lookupChunkSize {
			chunk := unique[start:min(start+lookupChunkSize, len(unique))]
			query := []any{columns + ` AND file_path IN (` + inPlaceholders(len(chunk)) + `)`, s.viewGen, repo, actor}
			queries = append(queries, append(query, toAnyArgs(chunk)...))
		}
	}
	for _, query := range queries {
		if err := s.readContractBoundaryStamps(ctx, query[0].(string), query[1:], out); err != nil {
			return nil, err
		}
	}
	return out, ctx.Err()
}

func (s *Store) readContractBoundaryStamps(ctx context.Context, query string, args []any, out map[string]ContractBoundaryReceiptStamp) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var stamp ContractBoundaryReceiptStamp
		if err := rows.Scan(&stamp.FilePath, &stamp.Fingerprint, &stamp.SourceFingerprint, &stamp.Accepted); err != nil {
			return err
		}
		out[stamp.FilePath] = stamp
	}
	return rows.Err()
}

// ContractSeedEdgesContext returns this generation's dependency-injection
// owner edges of one repository: provides and consumes edges whose source
// node the repository owns. A contract job's enrichment round re-derives its
// DI seeds from them every round instead of from per-file loads, because an
// evicted file tears other files' edges into it without rewriting their
// receipts.
func (s *Store) ContractSeedEdgesContext(ctx context.Context, repo string) ([]*graph.Edge, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, err
	}
	reposJSON, err := json.Marshal([]string{repo})
	if err != nil {
		return nil, err
	}
	kindsJSON, err := json.Marshal([]string{string(graph.EdgeProvides), string(graph.EdgeConsumes)})
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, repoContractEdgesByKindsQuery(), string(reposJSON), string(kindsJSON), s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		var repoPrefix string
		edge, err := s.scanEdgeCursor(repoPrefixedEdgeScanner{scanner: rows, repo: &repoPrefix})
		if err != nil {
			return nil, err
		}
		if edge != nil {
			out = append(out, edge)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}

// ContractSeedNodesContext returns this generation's enrichment seed nodes of
// one repository: Java methods (Spring bean injection candidates) and nodes
// that carry Spring configuration keys.
func (s *Store) ContractSeedNodesContext(ctx context.Context, repo string) ([]*graph.Node, error) {
	return s.contractSeedNodesContext(ctx, repo, nil)
}

// afterCandidates is a deterministic snapshot/cancellation test seam. Public
// callers always pass nil; there is no new runtime option or retained cache.
func (s *Store) contractSeedNodesContext(ctx context.Context, repo string, afterCandidates func()) ([]*graph.Node, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, err
	}
	// A mutable corpus can change between these two reads. Candidate eligibility
	// and the full returned rows must come from the SAME SQLite snapshot.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := s.contractSeedNodeIDsContext(ctx, tx, repo)
	if err != nil {
		return nil, err
	}
	if afterCandidates != nil {
		afterCandidates()
	}
	var out []*graph.Node
	for start := 0; start < len(ids); start += 128 {
		keys := ids[start:min(start+128, len(ids))]
		// Bound raw string parameters instead of JSON encoding identities: even a
		// legacy empty or non-UTF8 SQLite identity keeps its exact spelling.
		args := []any{s.viewGen}
		for _, id := range keys {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+lookupNodeCols+` FROM nodes WHERE view_gen=? AND id IN (`+inPlaceholders(len(keys))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			n, err := scanNodeCursor(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, n)
		}
		re, ce := rows.Err(), rows.Close()
		if re != nil {
			return nil, re
		}
		if ce != nil {
			return nil, ce
		}
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("%w: incomplete contract seed rows", graph.ErrContractProjectionIncomplete)
	}
	slices.SortFunc(out, func(a, b *graph.Node) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out, ctx.Err()
}

// Eligibility consumes only identity, kind, language and metadata. Decode ALL
// candidate metadata, including non-seeds, so malformed metadata still refuses
// the checked read. Hydrate other rich fields only for actual Java/Spring seeds.
func (s *Store) contractSeedNodeIDsContext(ctx context.Context, tx *sql.Tx, repo string) ([]string, error) {
	kindsJSON, err := json.Marshal([]string{string(graph.KindField), string(graph.KindType), string(graph.KindInterface), string(graph.KindMethod)})
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, repoNodesByKindsProjectionQuery(false, "n.id,n.kind,n.language,n.meta"), string(kindsJSON), repo, s.viewGen, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	var ids []string
	for rows.Next() {
		var id, language string
		var kind graph.NodeKind
		var blob sql.RawBytes
		if err := rows.Scan(&id, &kind, &language, &blob); err != nil {
			return nil, err
		}
		meta, err := decodeMeta(blob)
		if err != nil {
			return nil, err
		}
		_, spring := meta["spring_config_keys"]
		if !seen[id] && (spring || kind == graph.KindMethod && language == "java") {
			seen[id] = true
			ids = append(ids, id)
			if len(ids) > graph.ContractProjectionRowLimit {
				return nil, fmt.Errorf("%w: contract seed nodes", graph.ErrContractProjectionLimit)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return ids, ctx.Err()
}

// ContractCoreWindowClosedAt returns the recorded close of the exact window
// opened at seq. A later window is never evidence for an earlier close; a
// close outside the bounded history is reported missing.
func (s *Store) ContractCoreWindowClosedAt(repo, checkout, path string, seq uint64) (time.Time, bool) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, span := range w.closes[contractReceiptWriteKey{repo, checkout, path}] {
		if span.seq == seq {
			return span.closed, true
		}
	}
	return time.Time{}, false
}

// ContractCoreWindowClose is one bounded-history close used to associate a
// proof interval with a writer's next open, or identify a censored gap.
type ContractCoreWindowClose struct {
	Repo       string `json:"repo"`
	Checkout   string `json:"checkout"`
	Path       string `json:"path"`
	OpenerSeq  uint64 `json:"opener_seq"`
	ClosedAtNS int64  `json:"closed_at_ns"`
}

// ContractCoreWindowLastClose returns the newest retained close at or before
// the bound for one path, without substituting a later event.
func (s *Store) ContractCoreWindowLastClose(repo, checkout, path string, before time.Time) (ContractCoreWindowClose, bool) {
	w := &s.contractReceiptWrites
	w.mu.Lock()
	defer w.mu.Unlock()
	history := w.closes[contractReceiptWriteKey{repo, checkout, path}]
	for i := len(history) - 1; i >= 0; i-- {
		span := history[i]
		if !span.closed.After(before) {
			return ContractCoreWindowClose{Repo: repo, Checkout: checkout, Path: path, OpenerSeq: span.seq, ClosedAtNS: span.closed.UnixNano()}, true
		}
	}
	return ContractCoreWindowClose{}, false
}
