package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// A contract job builds its attachment from per-file extraction units proven
// against receipt stamps, then converges inside the job instead of failing on
// the first save it observes.
//
// Every round takes a stamp scan R0 the change clock observes before any read
// (a full scan, or for a retry a point scan of the retried files and their
// owners), extracts its files on fresh private evidence, then a point scan R1
// of the files and of every owner the reads returned. A unit is proven when
// each owner's stamp is quiescent at R0 and identical at R1, and the
// attachment key's input-state witnesses still hold. Soundness rests on the
// change clock and on settling
// before publication, not on R1: every gen-0 core write of rows owned by a
// file happens inside an apply that first rewrote the file's receipt
// unaccepted with a bumped seq, and accepts it only after its outer fence,
// so any write a unit's reads could have seen changes a later full scan and
// makes the unit stale by its own file (a), an owner (b) or a key it looked
// up and a file newly declares (c).
//
// Every read a unit or the enrichment round makes goes through the ledger
// facade, which records it with a digest of its answer. Extraction and the
// enrichment round are functions of those recorded inputs only, so a stale
// unit whose own receipt identity held is revalidated, not extracted: the
// recorded reads a move could have changed are asked again inside a point
// round, and it is proven when every such answer's digest is unchanged. A
// read's answer changes only when a row it holds changes (its file moved
// first) or a moved file now declares what it asked for, so the reads asked
// again are those. The enrichment round is likewise re-checked, not rerun,
// when something moved after it: it reruns only when a live unit's records
// differ from the ones it consumed, a byte read's receipt moved, or a
// recorded read answers differently in the point round that re-checks it
// (with the suspects' revalidation when there are any). A job publishes only
// from an iteration whose final fence F finds no stale unit, an enrichment
// round current at F and every live file quiescent or proven at its stamp. F
// keeps the prior stamp of a file that moved after the last point scan
// observed it quiescent, when no read followed that scan: the move came after
// every read, so the job describes the state that scan saw, and a later scan
// sees the move.
//
// A stamp is quiescent when no core-write window of its path is open: the
// window a mutation opens as it stages the receipt closes when its body ends
// its core writes, when the row is accepted, or when an overtaken mutation
// settles it. A file whose receipt is still in flight after its body ended has
// complete rows, so acceptance is never needed: a read proven against
// quiescent stamps is proven whether or not the journal accepted them.
//
// A move whose receipt carries the contract content of the receipt it
// replaced (the receipt without its source fingerprint: the accepted parse's
// raw records and every declaration other files' reads see) keeps the file's
// content clock, and staleness follows content clocks: such a move makes no
// unit stale, not even the file's own, and no enrichment round. Reads taken
// while a file was not quiescent are the exception: a unit or an enrichment
// round that read across such a file asks those reads again.
//
// A file in flight is read from its current bytes, at its receipt's source
// fingerprint, and extracted; a unit or enrichment round that read across one
// of its core-write windows asks those reads again, each time from a scan
// that finds every input's window closed, and is proven only when the scan
// after the asks finds the same stamps. An answer asked while a window was
// open is never compared: two answers torn by two applies' evicts agree. A
// window closes before the apply is accepted, so the asks need no accepted
// receipt and no pause of the writer, only the gap between one apply's core
// writes and the next one's. Equal answers prove the reads at the stamp
// asked; a changed answer re-extracts the unit from the current bytes, and
// the loop goes on. While an apply that changes the file's contract content
// has its window open, the file's core rows are not yet those of its staged
// bytes; the asks wait for its window to close.
//
// A job's retry waits wake on Published and on every ended receipt write,
// closed core-write window and settle, or after contractRoundWait. A wait
// whose reads were refused only because an input's core-write window was open
// wakes when one of those windows closes, not on the receipt write that opened
// a window, and its round asks at once, inside the gap before the next apply.
// The loop has no fixed iteration bound and no wall-clock budget. A round
// makes progress when it proves a unit (by stamp or by asking again),
// re-extracts a changed one, or re-checks or reruns the enrichment round;
// answers asked while an input moved prove nothing; the job ends superseded
// only after contractIdleRounds counted rounds without progress, none with
// progress between them. A round is blocked when a read it refused had an
// input whose window was open or that moved while it was read; a blocked round
// an event woke never counts and leaves the count as it was. A round counts
// when it was not blocked and an input moved during it, or when its wait ran
// out with no event; a round an event woke that found its inputs unmoved never
// counts, and breaks a run of rounds that only waited out. Moving inputs end
// a job only through rounds that were not blocked; consecutive waited-out
// rounds end it with unsettled reads (an apply that never completes).
const (
	contractRoundFiles = 64
	contractRoundWait  = 3 * time.Second
	contractIdleRounds = 3
)

var (
	errContractJobKeyMoved      = errors.New("contract analysis: attachment input key moved")
	errContractJobInputsMoving  = errors.New("contract analysis: inputs kept moving through unblocked rounds that proved nothing")
	errContractJobReadUnsettled = errors.New("contract analysis: reads did not settle through consecutive rounds that proved nothing")
)

// ContractFillContinued ends a fill at a proven checkpoint so another actor
// can use the job slot. It neither publishes nor settles attachment waiters.
type ContractFillContinued struct{}

func (*ContractFillContinued) Error() string { return "contract analysis: fill checkpoint continued" }

// contractJobOutcomeOf classifies a job error for the jobs counter.
func contractJobOutcomeOf(err error) string {
	switch {
	case err == nil:
		return viewmetrics.ContractJobPublished
	case errors.As(err, new(*ContractFillContinued)):
		return viewmetrics.ContractJobFillContinued
	case errors.Is(err, errContractJobKeyMoved):
		return viewmetrics.ContractJobSupersededKeyMoved
	case errors.Is(err, errContractJobInputsMoving):
		return viewmetrics.ContractJobSupersededInputsMoving
	case errors.Is(err, errContractJobReadUnsettled):
		return viewmetrics.ContractJobSupersededReadUnsettled
	case errors.Is(err, store_sqlite.ErrCatalogStaleGuard), errors.Is(err, graph.ErrContractProjectionStale):
		return viewmetrics.ContractJobSupersededPublishRefused
	}
	return viewmetrics.ContractJobFailed
}

// contractUnit is one file's proven extraction.
type contractUnit struct {
	// Retained raw and router-fact size travels with its immutable unit; zero is unmeasured.
	retainedRawBytes int64
	path             string
	stamp            ContractReceiptStamp
	round            uint64
	raw              []contracts.Contract
	router           *contracts.RouterFacts
	owners           map[string]bool
	keys             map[string]bool
	declared         []string
	excluded         bool
	ledgerAliases    bool
	// rawDigest is the digest of raw, what the enrichment round consumed.
	rawDigest string
	// ledger holds the unit's recorded reads and byte reads, asked again to
	// revalidate the unit instead of extracting it.
	ledger *contractUnitLedger
	// seed rows the unit's own load carried, the enrichment round's seeds
	// when the snapshot cannot query them (a frozen test cohort).
	seedNodes []*graph.Node
	seedEdges []*graph.Edge
	// torn holds the files the unit read while they were not quiescent or
	// moved, each with the stamp the scan closing those reads saw: its
	// answers of them may be torn, so its revalidation asks them again
	// whatever their content did, from rows no core write overlaps.
	torn map[string]ContractReceiptStamp
}

// Recorded read kinds: what the ledger facade can ask again.
const (
	contractReadExtractionFile  = "extraction_file"
	contractReadFile            = "file"
	contractReadFileNodes       = "file_nodes"
	contractReadIDs             = "ids"
	contractReadNames           = "names"
	contractReadConstCandidates = "constant_candidates"
	contractReadConstants       = "constants"
	contractReadRepoFiles       = "repo_files"
	contractReadSemantic        = "semantic"
	contractReadSeeds           = "seeds"
	contractReadLivePaths       = "live_paths"
	contractReadEndpoints       = "endpoints"
)

// contractRecordedRead is one read the ledger facade answered: what was
// asked, in a form it can be asked again, the digest of the answer, and the
// files whose rows the answer holds (or the read named).
type contractRecordedRead struct {
	kind   string
	args   []string
	digest string
	owners []string
}

// contractUnitLedger records what one unit (or one enrichment round) read, at
// the evidence facade's return boundary: every requested key, the owning file
// of every node returned or loaded and of every byte read, every read with a
// digest of its answer, and the receipt source fingerprint of every byte read.
type contractUnitLedger struct {
	owners   map[string]bool
	keys     map[string]bool
	reads    []contractRecordedRead
	recorded map[string]bool
	sources  map[string]string
	// opaque marks a read the ledger cannot ask again: the unit is
	// re-extracted, never revalidated.
	opaque bool
}

func newContractUnitLedger() *contractUnitLedger {
	return &contractUnitLedger{owners: make(map[string]bool), keys: make(map[string]bool), recorded: make(map[string]bool), sources: make(map[string]string)}
}

// read records one answered read with the files its answer holds, plus any
// further files it depends on. Repeating an identical read with an identical
// answer records it once.
func (l *contractUnitLedger) read(kind string, args []string, answer any, owners ...string) {
	if l == nil {
		return
	}
	digest, ok := contractAnswerDigest(answer)
	if !ok {
		l.opaque = true
		return
	}
	key := kind + "\x00" + strings.Join(args, "\x00") + "\x00" + digest
	if l.recorded[key] {
		return
	}
	l.recorded[key] = true
	owners = appendUniqueSorted(owners, contractAnswerOwners(answer)...)
	if kind == contractReadSeeds {
		// An apply of an edge endpoint's file evicts the seed edge without
		// rewriting its owner's receipt: the seed read depends on both ends.
		owners = appendUniqueSorted(owners, contractEdgeEndpointFiles(answer)...)
	}
	l.reads = append(l.reads, contractRecordedRead{kind: kind, args: append([]string(nil), args...), digest: digest, owners: owners})
}

// contractEdgeEndpointFiles lists the files of an answer's edge endpoints.
func contractEdgeEndpointFiles(answer any) []string {
	file, ok := answer.(contractFileAnswer)
	if !ok {
		return nil
	}
	var out []string
	for _, edge := range file.Edges {
		if edge == nil {
			continue
		}
		for _, id := range []string{edge.From, edge.To} {
			if path := graph.IDFile(id); path != "" {
				out = append(out, path)
			}
		}
	}
	return out
}

// contractAnswerOwners lists the files whose rows an answer holds.
func contractAnswerOwners(answer any) []string {
	var out []string
	addNodes := func(nodes ...*graph.Node) {
		for _, node := range nodes {
			if node != nil && node.FilePath != "" {
				out = append(out, node.FilePath)
			}
		}
	}
	switch v := answer.(type) {
	case []*graph.Node:
		addNodes(v...)
	case contractFileAnswer:
		addNodes(v.Nodes...)
		for _, edge := range v.Edges {
			if edge != nil && edge.FilePath != "" {
				out = append(out, edge.FilePath)
			}
		}
	case map[string]*graph.Node:
		for _, node := range v {
			addNodes(node)
		}
	case map[string][]*graph.Node:
		for _, nodes := range v {
			addNodes(nodes...)
		}
	case []contracts.EndpointConstantCandidate:
		for _, candidate := range v {
			if candidate.FilePath != "" {
				out = append(out, candidate.FilePath)
			}
		}
	}
	return out
}

// contractMoves is what moved after a round: every path the change clock
// stamped later, and what those files declare now. declaredKnown is false
// while a moved live file in unknown has neither a unit at its current
// identity nor a resolved core load, so its new declarations are unknown.
type contractMoves struct {
	moved         map[string]bool
	declared      map[string]bool
	unknown       map[string]bool
	declaredKnown bool
	seeding       bool
	// byPath holds what each moved file with a known current unit or core
	// load declares, and seeders the moved files carrying seed rows.
	byPath  map[string][]string
	seeders map[string]bool
}

// movesSince is what moved in content after round, plus the files a reader
// read torn, which count as moved whatever their content did.
func (c *contractConvergence) movesSince(round uint64, torn map[string]ContractReceiptStamp) contractMoves {
	m := contractMoves{moved: make(map[string]bool), declared: make(map[string]bool), unknown: make(map[string]bool), declaredKnown: true,
		byPath: make(map[string][]string), seeders: make(map[string]bool)}
	add := func(path string) {
		if m.moved[path] {
			return
		}
		m.moved[path] = true
		m.declared["path:"+path] = true
		unit := c.units[path]
		if unit == nil || unit.round < c.contentAt[path] {
			// No unit describes the file's current content.
			if c.live[path] {
				m.unknown[path] = true
				m.declaredKnown = false
			}
			return
		}
		m.byPath[path] = unit.declared
		for _, key := range unit.declared {
			m.declared[key] = true
		}
		if len(unit.seedNodes) > 0 || len(unit.seedEdges) > 0 {
			m.seeding, m.seeders[path] = true, true
		}
	}
	for path, at := range c.contentAt {
		if at > round {
			add(path)
		}
	}
	for path := range torn {
		add(path)
	}
	return m
}

// resolve adds what a moved file declares now, read from its core rows inside
// the caller's bracket, in place of the unit it has no current one of.
func (m *contractMoves) resolve(path string, declared contractDeclaration) {
	if !m.unknown[path] {
		return
	}
	delete(m.unknown, path)
	m.byPath[path] = slices.Sorted(maps.Keys(declared.keys))
	for key := range declared.keys {
		m.declared[key] = true
	}
	if declared.seeding {
		m.seeding, m.seeders[path] = true, true
	}
	m.declaredKnown = len(m.unknown) == 0
}

// depends reports whether a recorded read's answer can depend on the rows of
// moved file path: path owns a row of the answer, declares what the read
// asked for (or its declarations are unknown), or carries seed rows a seed
// read returns. Census reads answer from the census, not from rows.
func (m contractMoves) depends(path string, read contractRecordedRead) bool {
	if slices.Contains(read.owners, path) {
		return true
	}
	prefix := ""
	args := read.args
	switch read.kind {
	case contractReadRepoFiles, contractReadLivePaths, contractReadSemantic:
		return false
	case contractReadSeeds:
		return m.unknown[path] || m.seeders[path]
	case contractReadNames:
		prefix = "name:"
	case contractReadConstCandidates:
		prefix = "name:"
		args = args[:min(1, len(args))]
	case contractReadIDs, contractReadConstants, contractReadEndpoints:
		prefix = "id:"
	case contractReadFile, contractReadFileNodes, contractReadExtractionFile:
		prefix = "path:"
	default:
		return true
	}
	if m.unknown[path] {
		return true
	}
	for _, arg := range args {
		if contractDeclaredHas(m.byPath[path], prefix+arg) || (prefix == "path:" && arg == path) {
			return true
		}
	}
	return false
}

// inputsOf lists the moved files any of reads can depend on.
func (m contractMoves) inputsOf(reads []contractRecordedRead) []string {
	var out []string
	for path := range m.moved {
		for _, read := range reads {
			if m.depends(path, read) {
				out = append(out, path)
				break
			}
		}
	}
	return out
}

// contractDeclaration is what one file's core rows declare: the name, id and
// path keys a lookup could find them by, and whether they carry enrichment
// seeds.
type contractDeclaration struct {
	keys    map[string]bool
	seeding bool
}

func contractDeclarationOf(path string, nodes []*graph.Node, edges []*graph.Edge) contractDeclaration {
	d := contractDeclaration{keys: map[string]bool{"path:" + path: true}}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		d.keys["name:"+node.Name] = true
		d.keys["id:"+node.ID] = true
		if _, spring := node.Meta["spring_config_keys"]; spring || (node.Kind == graph.KindMethod && node.Language == "java") {
			d.seeding = true
		}
	}
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
			if _, di := diContractFromEdge(edge); di {
				d.seeding = true
			}
		}
		if edge.Kind == graph.EdgeCalls && edge.Meta["via"] == "spring.Bean" {
			d.seeding = true
		}
	}
	return d
}

// affected reports whether a recorded read could answer differently after
// the moves. An answer changes only when a row it holds changes (its file's
// receipt is rewritten first, so the file moved) or a new row matches it (a
// moved file now declares the name, id or path it asked for, or carries
// seed rows). Census reads are always asked again (they read no store), and
// so are binding-type reads: enrichment writes those rows after the receipt
// is accepted, so no receipt move announces them.
func (m contractMoves) affected(read contractRecordedRead) bool {
	for _, owner := range read.owners {
		if m.moved[owner] {
			return true
		}
	}
	prefix := ""
	args := read.args
	switch read.kind {
	case contractReadRepoFiles, contractReadLivePaths, contractReadSemantic:
		return true
	case contractReadSeeds:
		return m.seeding || !m.declaredKnown
	case contractReadNames:
		prefix = "name:"
	case contractReadConstCandidates:
		prefix = "name:"
		args = args[:min(1, len(args))]
	case contractReadIDs, contractReadConstants, contractReadEndpoints:
		prefix = "id:"
	case contractReadFile, contractReadFileNodes, contractReadExtractionFile:
		prefix = "path:"
	default:
		return true
	}
	if !m.declaredKnown {
		return true
	}
	for _, arg := range args {
		if m.declared[prefix+arg] {
			return true
		}
	}
	return false
}

// source records the receipt source fingerprint of bytes read for path.
func (l *contractUnitLedger) source(path, fingerprint string) {
	if l != nil && path != "" {
		l.sources[path] = fingerprint
	}
}

// contractFileAnswer is a file load's answer.
type contractFileAnswer struct {
	Nodes []*graph.Node
	Edges []*graph.Edge
}

// contractAnswerDigest digests an answer in a canonical form: node and edge
// lists are order-free, map keys sorted. Equal digests mean equal answers.
func contractAnswerDigest(answer any) (string, bool) {
	canonical, err := contractCanonicalAnswer(answer)
	if err != nil {
		return "", false
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", false
	}
	return contractInputHash(encoded), true
}

func contractCanonicalAnswer(answer any) (any, error) {
	switch v := answer.(type) {
	case []contracts.Contract:
		// Registry.All enumerates map buckets; record order is not an input.
		return contractSortedJSON(v)
	case []*graph.Node:
		return contractSortedJSON(v)
	case []*graph.Edge:
		return contractSortedEdgeJSON(v)
	case []contracts.EndpointConstantCandidate:
		return contractSortedJSON(v)
	case contractFileAnswer:
		nodes, err := contractSortedJSON(v.Nodes)
		if err != nil {
			return nil, err
		}
		edges, err := contractSortedEdgeJSON(v.Edges)
		if err != nil {
			return nil, err
		}
		return [2][]string{nodes, edges}, nil
	case map[string][]*graph.Node:
		out := make(map[string][]string, len(v))
		for name, nodes := range v {
			sorted, err := contractSortedJSON(nodes)
			if err != nil {
				return nil, err
			}
			out[name] = sorted
		}
		return out, nil
	case map[graph.SemanticBindingSite]string:
		pairs := make([]string, 0, len(v))
		for site, typeName := range v {
			encoded, err := json.Marshal([2]any{site, typeName})
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, string(encoded))
		}
		sort.Strings(pairs)
		return pairs, nil
	case []string:
		if v == nil {
			return []string{}, nil
		}
		return v, nil
	}
	return answer, nil
}

func contractSortedJSON[T any](items []T) ([]string, error) {
	out := make([]string, 0, len(items))
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		out = append(out, string(encoded))
	}
	sort.Strings(out)
	return out, nil
}

// contractSortedEdgeJSON encodes edges with the Meta their JSON form leaves
// out: it carries what the enrichment round reads off an edge (a dependency
// injection token and binding, a Spring bean call's via). The numeric
// confidence the JSON form also leaves out is read by no contract pass; its
// label is in the JSON form.
func contractSortedEdgeJSON(edges []*graph.Edge) ([]string, error) {
	type canonical struct {
		Edge *graph.Edge
		Meta map[string]any
	}
	wrapped := make([]canonical, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			wrapped = append(wrapped, canonical{})
			continue
		}
		wrapped = append(wrapped, canonical{Edge: edge, Meta: edge.Meta})
	}
	return contractSortedJSON(wrapped)
}

// contractReadGuard brackets the reads the ledger facade answers from the
// selected core while an enrichment round is open. A core read during the
// round with no bracket open bypassed the ledger, so the round's staleness
// would not cover it; an instrumented reader counts such reads.
type contractReadGuard struct {
	open  bool
	depth int
}

// enter opens a bracket around one recorded read and returns its close.
func (g *contractReadGuard) enter() func() {
	if g == nil {
		return func() {}
	}
	g.depth++
	return func() { g.depth-- }
}

// recorded reports whether a core read now is covered by the ledger: no
// enrichment round is open, or a recorded read is in progress.
func (g *contractReadGuard) recorded() bool {
	return g == nil || !g.open || g.depth > 0
}

// contractRevalidationStats is one pass's revalidation work: units asked,
// left unproven in flight or with unknown declarations, moved during the
// pass or changed, distinct reads asked again, and the time spent scanning,
// reading declarations and asking. span is the wall time from the start of
// the last pass's opening scan to the end of its closing scan: the gap
// between two applies a re-ask must fit in.
type contractRevalidationStats struct {
	units, inFlight, unknown, moved, equalMoved, changed, reads int
	scans, declare, ask, span                                   time.Duration
	// writing is the first input found inside a core-write window.
	writing string
	// movedFields counts, per stamp field, the inputs that moved while a read
	// was asked and differed in that field ("gone" for a departed row).
	movedFields map[string]int
}

// noteMoved records which fields of an input's stamp changed between the
// scans around a read.
func (s *contractRevalidationStats) noteMoved(before, after ContractReceiptStamp, has bool) {
	if s.movedFields == nil {
		s.movedFields = make(map[string]int)
	}
	for _, field := range contractStampChanges(before, after, has) {
		s.movedFields[field]++
	}
}

// contractStampChanges names what differs between two stamps of a path:
// "gone" when the row departed, else every field that differs.
func contractStampChanges(before, after ContractReceiptStamp, has bool) []string {
	if !has {
		return []string{"gone"}
	}
	var out []string
	for _, field := range []struct {
		name    string
		differs bool
	}{
		{"generation", before.Generation != after.Generation}, {"actor", before.Actor != after.Actor},
		{"fingerprint", before.Fingerprint != after.Fingerprint}, {"source", before.SourceFingerprint != after.SourceFingerprint},
		{"accepted", before.Accepted != after.Accepted}, {"seq", before.Seq != after.Seq},
		{"settled", before.Settled != after.Settled}, {"writing", before.Writing != after.Writing},
	} {
		if field.differs {
			out = append(out, field.name)
		}
	}
	return out
}

func (s contractRevalidationStats) fields() []zap.Field {
	moved := make([]string, 0, len(s.movedFields))
	for _, field := range slices.Sorted(maps.Keys(s.movedFields)) {
		moved = append(moved, fmt.Sprintf("%s:%d", field, s.movedFields[field]))
	}
	return []zap.Field{
		zap.Int("revalidate_units", s.units), zap.Int("revalidate_in_flight", s.inFlight), zap.String("revalidate_writing", s.writing),
		zap.Int("revalidate_unknown", s.unknown), zap.Int("revalidate_moved", s.moved), zap.Int("revalidate_moved_equal", s.equalMoved),
		zap.String("revalidate_moved_fields", strings.Join(moved, ",")),
		zap.Int("revalidate_changed", s.changed), zap.Int("revalidate_reads", s.reads),
		zap.Duration("revalidate_scans", s.scans), zap.Duration("revalidate_declare", s.declare),
		zap.Duration("revalidate_ask", s.ask), zap.Duration("revalidate_span", s.span),
	}
}

// noteReask records one re-ask that ran its reads, whether its answers were
// admitted or refused because an input moved while they were asked: its span
// from the opening scan to the closing one, and its lead from the event that
// woke the wait before it, when one did. A re-ask every input of which was
// inside an open window at its opening scan asked nothing and is not one.
func (c *contractConvergence) noteReask(lead time.Duration, woken bool, span time.Duration, moved bool, source string, closedAt time.Time) {
	c.spans = append(c.spans, span)
	leadMS := -1.0
	if woken {
		c.leads = append(c.leads, lead)
		leadMS = contractMillis(lead)
	}
	c.logger.Info("contract analysis reask", zap.String("repo", c.snap.Key.RepoPrefix), zap.Int("iteration", c.iteration),
		zap.Float64("lead_ms", leadMS), zap.Bool("lead_valid", woken), zap.String("lead_source", source), zap.Int64("wake_closed_at_ns", contractWindowUnixNano(closedAt)), zap.Float64("span_ms", contractMillis(span)), zap.Bool("moved", moved))
}

// contractMillis is d in milliseconds, to the microsecond.
func contractMillis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// contractQuantile is the nearest-rank q-quantile of samples, 0 for none.
func contractQuantile(samples []time.Duration, q float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// contractEnrichment is what the enrichment round consumed: the clock its
// inputs were last shown current at, the digest of every live unit's records,
// and its own recorded reads.
type contractEnrichment struct {
	round   uint64
	raws    map[string]string
	ledger  *contractUnitLedger
	routers []contractRouterProof
	// stale is set when a re-check found a recorded input changed.
	stale bool
	// torn holds the files the round read that were not quiescent at its R0
	// or moved before its reads ended, each with its stamp at the round's R1:
	// the round's reads of them are asked again before it is current. A file
	// the round never read cannot tear it.
	torn map[string]ContractReceiptStamp
}

func (l *contractUnitLedger) owner(path string) {
	if l != nil && path != "" {
		l.owners[path] = true
	}
}

func (l *contractUnitLedger) key(prefix string, values ...string) {
	if l == nil {
		return
	}
	for _, value := range values {
		l.keys[prefix+value] = true
	}
}

func (l *contractUnitLedger) nodes(nodes ...*graph.Node) {
	if l == nil {
		return
	}
	for _, node := range nodes {
		if node != nil {
			l.owner(node.FilePath)
		}
	}
}

// contractConvergenceHooks are test seams at the engine's interleaving
// points. Production leaves them nil.
type contractConvergenceHooks struct {
	// roundFiles narrows a proof round to place files in separate rounds.
	roundFiles int
	// roundWait replaces contractRoundWait.
	roundWait      time.Duration
	openedEvidence func(*store_sqlite.Store)
	beforeExtract  func(path string)
	afterExtract   func(path string)
	proven         func(unit *contractUnit)
	beforeR1       func(paths []string)
	afterPP        func()
	beforeFence    func(iteration int)
	// fenced receives each iteration's fence and the paths whose moves it
	// left for a later scan.
	fenced func(iteration int, deferred []string)
	// revalidated runs after a suspect unit's recorded reads were asked
	// again, with whether every answer was unchanged.
	revalidated func(path string, equal bool)
	// woke runs when a retry wait ends on an event, before the retry pass.
	woke func()
	// beforeRevalidateR1 runs after a revalidation pass asked its reads,
	// before its closing point scan.
	beforeRevalidateR1 func()
	// beforeAsk runs after a revalidation pass's opening scan, before it
	// reads declarations or asks.
	beforeAsk func()
	// guard receives the job's core read guard.
	guard func(*contractReadGuard)
}

type contractConvergence struct {
	coverage         *contractWorkCoverage
	fillReadCensus   map[*contractUnit][sha256.Size]byte
	fillCensusDigest [sha256.Size]byte
	fillCensusClock  uint64
	fillCensusKnown  bool
	ctx              context.Context
	req              ContractFollowupRequest
	snap             ContractFollowupSnapshot
	repos            map[string]bool
	logger           *zap.Logger
	files            map[string]ContractFollowupFile
	live             map[string]bool
	excluded         map[string]bool
	units            map[string]*contractUnit
	// suspect units are kept with their recorded reads but are not proven at
	// the current clock: an owner or a key they looked up moved, or their own
	// receipt was rewritten with the same identity. They are revalidated by
	// asking their reads again before any publish.
	suspect map[string]bool
	// retryOwners holds the owners an unproven unit read, for its retry
	// round's R0.
	retryOwners map[string]map[string]bool
	// declarers indexes the units' declared keys: the files whose current
	// unit answers a key a lookup can ask.
	declarers contractDeclarerIndex
	clock     contractChangeClock
	// contentAt is the clock of each path's last observed move that did not
	// keep its contract content (a new or departed row, a census join or
	// departure, a receipt of other or unknown content); maxContent is the
	// latest. contents caches receipt contents by receipt fingerprint, ""
	// for one that cannot be read.
	contentAt  map[string]uint64
	maxContent uint64
	contents   map[string]string
	// progress counts the job's proofs: units proven, changed units dropped
	// for extraction, enrichment rounds run or re-checked current.
	progress int
	// idle counts the unblocked rounds without progress since the last
	// progress that saw an input move or waited out contractRoundWait (a run
	// of waited-out rounds only while no unblocked event-woken round came
	// between them); idleMoving records whether one of them saw an input
	// move. waited is the run of consecutive rounds, blocked or not, that
	// waited out contractRoundWait.
	idle       int
	idleMoving bool
	waited     int
	// blocks counts reads refused because an input's core-write window was
	// open or the input moved while they were read. waitOn holds the inputs
	// of the current round's refusals whose windows were open at their last
	// observation, and waitAny is set by a refusal whose input no window
	// holds (it moved and closed, or departed): the round's wait then wakes
	// on any event, else on the close of a waitOn window.
	blocks  int
	waitOn  map[string]ContractReceiptStamp
	waitAny bool
	// stuck holds, per path a read was refused on with its window open,
	// the stamp it was refused on and since when: a stamp no core write
	// moved for a repair period is reported to the stuck-input repair once.
	stuck map[string]contractStuckSample
	// quietFrom is the clock of a point scan taken after the last read the
	// job made, or 0 once a read may have followed it: a stamp that scan
	// observed and a later scan finds moved moved after every read.
	quietFrom uint64
	pp        *contractEnrichment
	// enrichReads is the ledger of the enrichment pass in progress, what a
	// pass that failed had read.
	enrichReads *contractUnitLedger
	guard       *contractReadGuard
	// revalidation counts one retry pass's revalidation work for its log line.
	revalidation contractRevalidationStats
	// What an iteration needs of a writer's quiet gap, measured: iteration is
	// the current iteration; wokeAt is the exact recorded window close that
	// woke the last wait, taken by the next re-ask as its lead; leads and spans are the
	// iteration's re-asks that ran their reads (from the wake to the opening
	// scan, and from the opening scan to the closing one). blocked holds the
	// paths whose open window refused a read from the second iteration on,
	// blockedSince when that iteration began.
	iteration    int
	wokeAt       time.Time
	wakeSource   string
	leads, spans []time.Duration
	blocked      map[string]bool
	blockedSince time.Time
	report       *ContractFollowupReport
	hooks        *contractConvergenceHooks
	// The final enrichment round's evidence and registry.
	registry   *contracts.Registry
	evidence   *contractFollowupEvidence
	scratch    *store_sqlite.Store
	scratchDir string
}

func newContractConvergence(ctx context.Context, req ContractFollowupRequest, files map[string]ContractFollowupFile, repos map[string]bool, report *ContractFollowupReport) *contractConvergence {
	logger := req.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.With(zap.String("window_process", contractWindowProcess), zap.Uint64("window_job", contractWindowJobs.Add(1)))
	excluded := make(map[string]bool, len(req.Snapshot.Excluded))
	for _, path := range req.Snapshot.Excluded {
		excluded[path] = true
	}
	c := &contractConvergence{ctx: ctx, req: req, snap: req.Snapshot, repos: repos, logger: logger, files: files,
		live: make(map[string]bool), excluded: excluded, units: make(map[string]*contractUnit), suspect: make(map[string]bool),
		retryOwners: make(map[string]map[string]bool), contentAt: make(map[string]uint64), contents: make(map[string]string),
		guard: &contractReadGuard{}, report: report, hooks: req.convergenceHooks}
	work := req.coverageWork
	if work == nil {
		work = req.Snapshot.Work
	}
	c.coverage = newContractWorkCoverage(work)
	if c.hooks != nil && c.hooks.guard != nil {
		c.hooks.guard(c.guard)
	}
	return c
}

// scan takes a stamp scan. A frozen snapshot answers from its census.
func (c *contractConvergence) scan(paths []string) (ContractFollowupScan, error) {
	if c.snap.Scan != nil {
		out, err := c.snap.Scan(c.ctx, paths)
		c.report.StampsDuration += out.StampsDuration
		c.report.CensusDuration += out.CensusDuration
		return out, err
	}
	out := ContractFollowupScan{Stamps: make(map[string]ContractReceiptStamp), Settled: true, Full: paths == nil}
	census := make(map[string]bool)
	for path, file := range c.files {
		out.Stamps[path] = ContractReceiptStamp{Fingerprint: "captured:" + file.SourceFingerprint, SourceFingerprint: file.SourceFingerprint, Accepted: true}
		census[path] = true
	}
	for path := range c.excluded {
		out.Stamps[path] = ContractReceiptStamp{Fingerprint: contractFailureEnvelopeFingerprint(path), Accepted: true}
		census[path] = true
	}
	if out.Full {
		out.Census = census
	}
	return out, c.ctx.Err()
}

// fullScan advances the change clock with a full scan and refreshes the live
// census: joins are classified, departures dropped, envelopes excluded.
func (c *contractConvergence) fullScan() (ContractFollowupScan, uint64, error) {
	scan, err := c.scan(nil)
	if err != nil {
		return scan, 0, err
	}
	return c.observeFull(scan)
}

// fence takes an iteration's final full scan. A receipt that moved after the
// quiet point scan observed its prior stamp quiescent moved after every read
// the job made: its file's rewrite began after that observation, and every
// core write of its rows follows the rewrite. The job's units and enrichment
// round then describe the state that scan saw, so the fence keeps the prior
// stamp for it and leaves the move to a later scan (the next iteration's, or
// the next job's). A census join or departure, a vanished row and a prior
// stamp that was not quiescent are never deferred.
func (c *contractConvergence) fence() (ContractFollowupScan, uint64, []string, error) {
	scan, err := c.scan(nil)
	if err != nil {
		return scan, 0, nil, err
	}
	var deferred []string
	if c.quietFrom > 0 && scan.Settled {
		stamps := make(map[string]ContractReceiptStamp, len(scan.Stamps))
		for path, stamp := range scan.Stamps {
			stamps[path] = stamp
			prior, had := c.clock.last[path]
			if !had || prior == stamp || !prior.quiescent() || c.clock.seenAt[path] < c.quietFrom || scan.Census[path] != c.clock.lastCensus[path] {
				continue
			}
			stamps[path] = prior
			deferred = append(deferred, path)
		}
		scan.Stamps = stamps
		sort.Strings(deferred)
	}
	seen := make(map[string]uint64, len(deferred))
	for _, path := range deferred {
		seen[path] = c.clock.seenAt[path]
	}
	out, clock, err := c.observeFull(scan)
	// The fence did not see a deferred path's prior stamp; its last sighting
	// stays the quiet scan's.
	maps.Copy(c.clock.seenAt, seen)
	return out, clock, deferred, err
}

// observeFull advances the change clock with a full scan, classifies the
// moves it stamped, and refreshes the live census.
func (c *contractConvergence) observeFull(scan ContractFollowupScan) (ContractFollowupScan, uint64, error) {
	prior, priorCensus := c.clock.last, c.clock.lastCensus
	clock := c.clock.observe(scan.Stamps, scan.Census)
	for path, at := range c.clock.changedAt {
		if at == clock {
			c.noteMove(clock, path, prior, priorCensus[path] == scan.Census[path])
		}
	}
	live := make(map[string]bool, len(scan.Census))
	excluded := make(map[string]bool)
	for path := range scan.Census {
		if stamp, ok := scan.Stamps[path]; ok && stamp.envelope(path) {
			excluded[path] = true
			continue
		}
		if _, known := c.files[path]; !known {
			if c.snap.CensusFile == nil {
				continue
			}
			file, ok, err := c.snap.CensusFile(c.ctx, path)
			if err != nil {
				return scan, 0, err
			}
			if !ok {
				continue
			}
			c.files[path] = file
		}
		live[path] = true
	}
	for path := range c.files {
		if !live[path] {
			delete(c.files, path)
		}
	}
	c.live, c.excluded = live, excluded
	return scan, clock, nil
}

// observePoint advances the change clock with a point scan of paths and
// classifies the moves it stamped.
func (c *contractConvergence) observePoint(paths []string, stamps map[string]ContractReceiptStamp) uint64 {
	prior := c.clock.last
	clock := c.clock.observePoint(paths, stamps)
	noted := make(map[string]bool, len(paths))
	for _, path := range paths {
		if !noted[path] && c.clock.changedAt[path] == clock {
			noted[path] = true
			c.noteMove(clock, path, prior, true)
		}
	}
	return clock
}

// noteMove classifies one move the change clock stamped at clock against the
// path's prior observation: a row that keeps its contract content in the same
// census leaves the path's content clock where it was; any other move
// advances it.
func (c *contractConvergence) noteMove(clock uint64, path string, prior map[string]ContractReceiptStamp, sameCensus bool) {
	if c.contentAt == nil {
		c.contentAt = make(map[string]uint64)
	}
	before, had := prior[path]
	now, has := c.clock.last[path]
	if sameCensus && had && has && c.sameContent(path, before, now) {
		c.report.ContentKept++
		return
	}
	c.contentAt[path] = clock
	c.maxContent = max(c.maxContent, clock)
}

// sameContent reports whether two stamps of path carry the same contract
// content: the same receipt, or receipts whose contents are known and equal.
func (c *contractConvergence) sameContent(path string, before, now ContractReceiptStamp) bool {
	if before.Fingerprint == now.Fingerprint {
		return true
	}
	if c.snap.ReceiptContents == nil {
		return false
	}
	if c.contents == nil {
		c.contents = make(map[string]string)
	}
	if _, known := c.contents[now.Fingerprint]; !known {
		if read, err := c.snap.ReceiptContents(c.ctx, map[string]ContractReceiptStamp{path: now}); err == nil {
			maps.Copy(c.contents, read)
		}
		if _, known := c.contents[now.Fingerprint]; !known {
			// The row moved past the stamp: its content is never known.
			c.contents[now.Fingerprint] = ""
		}
	}
	content := c.contents[now.Fingerprint]
	return content != "" && c.contents[before.Fingerprint] == content
}

// loadContents reads the contract content of every stamp of the job's first
// full scan: a file's first move is then classified against the content its
// units were proven at, whose row the move replaces. A content it cannot read
// stays unknown, and that file's first move counts as a content move.
func (c *contractConvergence) loadContents(stamps map[string]ContractReceiptStamp) {
	if c.snap.ReceiptContents == nil || len(stamps) == 0 {
		return
	}
	if c.contents == nil {
		c.contents = make(map[string]string, len(stamps))
	}
	missing := make(map[string]ContractReceiptStamp)
	for path, stamp := range stamps {
		if c.contents[stamp.Fingerprint] == "" {
			missing[path] = stamp
		}
	}
	if len(missing) == 0 {
		return
	}
	start := time.Now()
	if read, err := c.snap.ReceiptContents(c.ctx, missing); err == nil {
		maps.Copy(c.contents, read)
	}
	c.report.StampsDuration += time.Since(start)
}

// contentChangedSince reports whether path's contract content moved after
// round.
func (c *contractConvergence) contentChangedSince(path string, round uint64) bool {
	return c.contentAt[path] > round
}

// roundWait is the longest a retry waits for an event before it runs its
// round anyway.
func (c *contractConvergence) roundWait() time.Duration {
	if c.hooks != nil && c.hooks.roundWait > 0 {
		return c.hooks.roundWait
	}
	return contractRoundWait
}

// contractRoundMark is what a round's accounting compares against: the
// progress count, the change clock's latest move and the refused reads before
// the round.
type contractRoundMark struct {
	progress int
	moved    uint64
	blocks   int
}

// markRound opens a round: its mark, and an empty set of inputs its wait is
// for.
func (c *contractConvergence) markRound() contractRoundMark {
	c.waitOn, c.waitAny = nil, false
	return contractRoundMark{progress: c.progress, moved: c.clock.maxChanged, blocks: c.blocks}
}

// block records a read refused because path's core-write window was open or
// path moved while it was read; stamp is path's last observation. A window
// still open is what the round's wait is for; one that closed already has
// announced itself, and the round is asked again on the next event.
func (c *contractConvergence) block(path string, stamp ContractReceiptStamp) {
	c.blocks++
	if stamp.quiescent() {
		c.waitAny = true
		return
	}
	c.noteStuck(path, stamp)
	if c.waitOn == nil {
		c.waitOn = make(map[string]ContractReceiptStamp)
	}
	c.waitOn[path] = stamp
	if c.iteration >= 1 {
		if c.blocked == nil {
			c.blocked = make(map[string]bool)
		}
		c.blocked[path] = true
	}
}

// blockMoved records a read refused because an input moved, with no stamp to
// wait on: the round is asked again on the next event.
func (c *contractConvergence) blockMoved() {
	c.blocks++
	c.waitAny = true
}

// endRound accounts one round. Progress resets the idle count and the
// waited-out run. A blocked round never counts: an input of a read it refused
// was inside an apply's core-write window or moved while it was read, so it
// waits for that apply, however long the writer keeps writing. Its wait
// running out only extends the waited-out run: nothing it waited on happened.
// An unblocked round without progress counts when an input moved during it or
// when the wait before it ran out with no event. An unblocked round an event
// woke that found its inputs unchanged never counts: it breaks a run of rounds
// that only waited out, and leaves a count with a move in it as it was, so an
// event stream of unrelated writes never ends a job.
func (c *contractConvergence) endRound(mark contractRoundMark, waitedOut bool) {
	if c.progress > mark.progress {
		c.idle, c.idleMoving, c.waited = 0, false, 0
		return
	}
	if c.blocks > mark.blocks {
		if waitedOut {
			c.waited++
		}
		return
	}
	moved := c.clock.maxChanged > mark.moved
	if !moved && !waitedOut {
		c.waited = 0
		if !c.idleMoving {
			c.idle = 0
		}
		return
	}
	c.idle++
	c.idleMoving = c.idleMoving || moved
	if waitedOut {
		c.waited++
	} else {
		c.waited = 0
	}
}

// idleErr ends the job: superseded by moving inputs once contractIdleRounds
// unblocked rounds without progress counted and one of them saw an input
// move, unsettled reads once contractIdleRounds consecutive rounds each
// waited out (an apply that never completes).
func (c *contractConvergence) idleErr() error {
	if c.idle >= contractIdleRounds && c.idleMoving {
		return errContractJobInputsMoving
	}
	if c.waited >= contractIdleRounds {
		return errContractJobReadUnsettled
	}
	return nil
}

// contractWake is the level a retry waits past: the Published epoch and the
// receipt event count, sampled before the scan that left a read unproven, so
// an event landing between that scan and the wait still wakes it.
type contractWake struct{ published, receipts uint64 }

func (c *contractConvergence) wake() contractWake {
	var w contractWake
	if c.req.PublishedEpoch != nil {
		w.published = c.req.PublishedEpoch()
	}
	if c.snap.ReceiptEvents != nil {
		w.receipts, _ = c.snap.ReceiptEvents()
	}
	return w
}

// waitWake blocks until a Published event or a receipt event (an ended
// receipt write, a closed core-write window or a settle) past since, or
// until. It reports whether an event woke it; with no Published source
// nothing can, and it sleeps until then and reports none. An overtaken
// mutation is never accepted and announces nothing, so the receipt events
// are what wake a retry while a writer outpaces its applies.
//
// When the round's refused reads all wait on open core-write windows
// (waitOn), a receipt event wakes it only once one of those windows closed:
// the write that opens a window is the worst moment to ask again, and the
// close is the start of the gap before the next apply.
func (c *contractConvergence) waitWake(since contractWake, until time.Time) bool {
	c.wokeAt, c.wakeSource = time.Time{}, "none"
	if c.req.WaitPublished == nil {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
		case <-timer.C:
		}
		return false
	}
	if c.snap.ReceiptEvents == nil {
		published := c.req.WaitPublished(c.ctx, since.published, until)
		if published {
			c.wakeSource = "published_unscoped"
		}
		return published
	}
	var windows map[string]ContractReceiptStamp
	if !c.waitAny && c.snap.Scan != nil {
		windows = c.waitOn
	}
	ready := func(ctx context.Context, events uint64) bool {
		if len(windows) == 0 {
			return events > since.receipts
		}
		return c.windowClosed(ctx, windows)
	}
	events, changed := c.snap.ReceiptEvents()
	if ready(c.ctx, events) {
		c.measureWindowWake(windows, "receipt_unmatched")
		return true
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	done := make(chan struct{})
	woke := false
	go func() {
		defer close(done)
		for {
			select {
			case <-changed:
			case <-ctx.Done():
				return
			}
			var now uint64
			now, changed = c.snap.ReceiptEvents()
			if ready(ctx, now) && ctx.Err() == nil {
				woke = true
				cancel()
				return
			}
		}
	}()
	published := c.req.WaitPublished(ctx, since.published, until)
	cancel()
	<-done
	switch {
	case woke:
		c.measureWindowWake(windows, "receipt_unmatched")
	case published:
		c.measureWindowWake(windows, "published_unmatched")
	}
	return published || woke
}

// windowClosed reports whether a core-write window a wait is for has closed:
// one of the paths is quiescent, has no row now or carries a stamp other than
// the one recorded when the wait was armed, or the key moved. A stamp that
// changed in any way is a close: the next apply's receipt write can land
// between a close and this scan, and its window is not the one waited on. It
// reads one point scan of the paths.
func (c *contractConvergence) windowClosed(ctx context.Context, windows map[string]ContractReceiptStamp) bool {
	paths := make([]string, 0, len(windows))
	for path := range windows {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	scan, err := c.snap.Scan(ctx, paths)
	if err != nil || !scan.Settled {
		return true
	}
	for _, path := range paths {
		if now, ok := scan.Stamps[path]; !ok || now.quiescent() || now != windows[path] {
			return true
		}
	}
	return false
}

// openEvidence opens a fresh private scratch store for one round. Nothing a
// round imported outlives it.
func (c *contractConvergence) openEvidence() (*contractFollowupEvidence, *store_sqlite.Store, string, error) {
	start := time.Now()
	defer func() { c.report.ScratchDuration += time.Since(start) }()
	dir, err := os.MkdirTemp(c.req.ScratchParent, "gortex-contract-evidence-")
	if err != nil {
		return nil, nil, "", err
	}
	scratch, err := store_sqlite.Open(filepath.Join(dir, "evidence.sqlite"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, "", err
	}
	files := make(map[string]ContractFollowupFile, len(c.files))
	for path, file := range c.files {
		files[path] = file
	}
	excluded := make(map[string]bool, len(c.excluded))
	for path := range c.excluded {
		excluded[path] = true
	}
	evidence := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: c.ctx, core: c.snap.Core, files: files, excluded: excluded, readCore: c.snap.ReadCoreFile, allowedRepos: c.repos, guard: c.guard}
	if reader, ok := c.snap.Core.(graph.SemanticBindingTypeReader); ok {
		evidence.semantic = reader
	}
	if c.hooks != nil && c.hooks.openedEvidence != nil {
		c.hooks.openedEvidence(scratch)
	}
	return evidence, scratch, dir, nil
}

func closeContractEvidence(scratch *store_sqlite.Store, dir string) {
	if scratch != nil {
		_ = scratch.Close()
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// pinnedReader serves the bytes of census files at a scan's stamps: the
// bytes whose hash is the stamped receipt's source fingerprint, whether or
// not that receipt is accepted yet. A read whose bytes are not the stamped
// receipt's is unproven, recorded in moved, and answered as unreadable.
type contractPinnedReader struct {
	c      *contractConvergence
	stamps map[string]ContractReceiptStamp
	ledger func() *contractUnitLedger
	moved  map[string]bool
}

func (r *contractPinnedReader) read(path string) ([]byte, error) {
	c := r.c
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	if ledger := r.ledger(); ledger != nil {
		ledger.owner(path)
		ledger.key("path:", path)
	}
	file, ok := c.files[path]
	if !ok {
		if c.excluded[path] {
			return nil, nil
		}
		return nil, fmt.Errorf("contract followup: accepted source outside census %q", path)
	}
	stamp, stamped := r.stamps[path]
	if !stamped {
		r.moved[path] = true
		return nil, errContractSourceMoved
	}
	var data []byte
	if c.snap.ReadPinned != nil {
		read, err := c.snap.ReadPinned(c.ctx, file, stamp.SourceFingerprint)
		if errors.Is(err, errContractSourceMoved) {
			r.moved[path] = true
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		data = read
	} else {
		accepted, err := c.snap.ReadAccepted(c.ctx, file)
		if err != nil {
			return nil, err
		}
		if accepted.SourceFingerprint != stamp.SourceFingerprint || accepted.Policy != file.Policy || contractInputHash(accepted.Bytes) != stamp.SourceFingerprint {
			r.moved[path] = true
			return nil, fmt.Errorf("%w: accepted source proof %s", errContractInputsChanged, path)
		}
		data = accepted.Bytes
	}
	if len(data) > contractFollowupCompactLimit {
		return nil, graph.ErrContractProjectionLimit
	}
	if ledger := r.ledger(); ledger != nil {
		ledger.source(path, stamp.SourceFingerprint)
	}
	c.report.SourceReads++
	c.report.SourceBytes += len(data)
	return bytes.Clone(data), nil
}

// indexers builds one analysis-only Indexer per repository over evidence.
func (c *contractConvergence) indexers(evidence *contractFollowupEvidence, read func(string) ([]byte, error)) map[string]*Indexer {
	out := make(map[string]*Indexer)
	add := func(repo, workspace, project string) {
		cfg := c.req.Config
		if override, ok := c.req.RepoConfigs[repo]; ok {
			cfg = override
		}
		root := filepath.Join(string(filepath.Separator), "contract-followup", repo)
		idx := &Indexer{graph: evidence, rootPath: root, repoPrefix: repo, workspaceID: workspace, projectID: project, config: cfg, logger: c.logger, registry: c.req.Registry}
		opts := c.snap.RepoExtractionOptions[repo]
		idx.extractionOptions.Store(&opts)
		idx.contractAnalysisOnly = true
		idx.contractAcceptedFileSource = read
		if evidence.semantic != nil {
			// Binding-type lookups answer through the ledger facade.
			idx.contractSemanticReader = contractLedgerSemanticReader{evidence: evidence}
		}
		out[repo] = idx
	}
	paths := make([]string, 0, len(c.files))
	for path := range c.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file := c.files[path]
		if out[file.RepoPrefix] == nil {
			add(file.RepoPrefix, file.WorkspaceID, file.ProjectID)
		}
	}
	// A certified empty own namespace still receives a complete empty snapshot.
	if out[c.snap.Key.RepoPrefix] == nil {
		cfg := c.req.Config
		idx := &Indexer{graph: evidence, rootPath: "/contract-followup", repoPrefix: c.snap.Key.RepoPrefix, workspaceID: c.req.WorkspaceID, projectID: c.req.ProjectID, config: cfg, logger: c.logger, registry: c.req.Registry}
		opts := c.snap.RepoExtractionOptions[c.snap.Key.RepoPrefix]
		idx.extractionOptions.Store(&opts)
		idx.contractAnalysisOnly = true
		idx.contractAcceptedFileSource = read
		out[c.snap.Key.RepoPrefix] = idx
	}
	return out
}

// extract runs one file's per-file extraction on the round's evidence.
func (c *contractConvergence) extract(idx *Indexer, evidence *contractFollowupEvidence, reader *contractPinnedReader, file ContractFollowupFile, ledger *contractUnitLedger) (*contractUnit, error) {
	unit := &contractUnit{path: file.Path, owners: ledger.owners, keys: ledger.keys, declared: []string{"path:" + file.Path}, ledger: ledger}
	projected, err := idx.contractFollowupPolicyMode(file.Language, file.Policy)
	if err != nil {
		return nil, err
	}
	if projected {
		// The generated-projection proof reads the core outside the facade.
		ledger.opaque = true
	}
	// The load target is an owner before the read: a target caught between
	// its evict and add is in the round's point scan.
	ledger.owner(file.Path)
	ledger.key("path:", file.Path)
	var nodes []*graph.Node
	var edges []*graph.Edge
	if projected {
		nodes, edges, err = evidence.loadFile(file.Path)
	} else {
		nodes, edges, err = evidence.loadExtractionFile(file.Path)
	}
	if err != nil {
		return nil, err
	}
	src, err := reader.read(file.Path)
	if err != nil {
		return nil, err
	}
	_, byLanguage := idx.buildPerFileContractExtractors()
	var tree *parser.ParseTree
	if projected {
		if !idx.contractGeneratedCoreProof(c.ctx, file, src, nodes, edges, c.snap.Core) {
			return nil, fmt.Errorf("contract followup: unproved accepted projection %s", file.Path)
		}
	} else {
		tree = contracts.ParseTreeForLang(file.Language, src)
	}
	found := idx.collectContractRecordsForFile(file.Path, src, nodes, edges, byLanguage[file.Language], tree, evidence)
	if tree != nil {
		tree.Release()
	}
	if file.Language == "gomod" || strings.HasSuffix(file.Path, "/go.mod") || file.Path == "go.mod" {
		found = append(found, (&contracts.GoModExtractor{TrackedRepos: c.snap.TrackedRepoModules}).Extract(file.Path, src, nodes, edges)...)
	}
	for i := range found {
		found[i].RepoPrefix = file.RepoPrefix
		if file.WorkspaceID != "" && found[i].WorkspaceID == "" {
			found[i].WorkspaceID = file.WorkspaceID
		}
		if file.ProjectID != "" && found[i].ProjectID == "" {
			found[i].ProjectID = file.ProjectID
		}
	}
	if contractRouterSupported(file, found) {
		facts := contracts.RouterFileFacts(file.Path, src)
		unit.router = &facts
	}
	unit.raw = cloneContractRecords(found)
	digest, ok := contractAnswerDigest(unit.raw)
	if !ok {
		return nil, fmt.Errorf("contract followup: unencodable records %s", file.Path)
	}
	unit.rawDigest = digest
	for _, node := range nodes {
		if node == nil {
			continue
		}
		unit.declared = append(unit.declared, "name:"+node.Name, "id:"+node.ID)
		if _, spring := node.Meta["spring_config_keys"]; spring || (node.Kind == graph.KindMethod && node.Language == "java") {
			unit.seedNodes = append(unit.seedNodes, node)
		}
	}
	sources := make(map[string]bool)
	for _, edge := range edges {
		if contractUnitSeedEdge(edge) {
			unit.seedEdges = append(unit.seedEdges, edge)
			sources[edge.From] = true
		}
	}
	for _, node := range nodes {
		if node != nil && sources[node.ID] {
			unit.seedNodes = append(unit.seedNodes, node)
		}
	}
	c.report.Files++
	slices.Sort(unit.declared)
	unit.declared = slices.Compact(unit.declared)
	return unit, evidence.err
}

// cloneContractRecords deep-copies records so postpasses, which mutate Meta
// in place, never write into a unit another enrichment round reuses.
func cloneContractRecords(in []contracts.Contract) []contracts.Contract {
	if in == nil {
		return nil
	}
	out := make([]contracts.Contract, len(in))
	for i, record := range in {
		out[i] = record
		if record.Meta != nil {
			out[i].Meta = contractFollowupCloneValue(reflect.ValueOf(record.Meta)).Interface().(map[string]any)
		}
	}
	return out
}

// round extracts paths on fresh evidence and proves each against R0 and R1.
// It returns the proven units and the unproven paths.
//
// A first round takes a full scan R0 and proves its units together against
// one point scan R1 after its last extraction. A retry round (point) runs
// right after an event: its R0 is a point scan of the paths and of the owners
// their last attempts read, observed by the change clock, and each unit is
// proven by its own point scan right after its extraction, so a unit's reads
// need only fit between two saves of a file it read, not a whole round. A
// file in flight at R0 is extracted from its current bytes all the same: a
// unit whose own stamp held across its reads but read a file in flight is a
// suspect, proven by asking its reads again. R1 is an early discard either
// way; a unit's round is the clock of a scan taken before its reads, and the
// clock carries soundness.
func (c *contractConvergence) round(paths []string, point bool) (proven []*contractUnit, unproven []string, err error) {
	roundStart := time.Now()
	defer func() {
		c.report.RoundsDuration += time.Since(roundStart)
		c.report.Rounds++
		viewmetrics.Count(viewmetrics.ContractAnalysisRoundsTotal)
	}()
	c.quietFrom = 0
	var r0 ContractFollowupScan
	var clock uint64
	var sampled []string
	if point {
		scanned := append([]string(nil), paths...)
		for _, path := range paths {
			for owner := range c.retryOwners[path] {
				scanned = append(scanned, owner)
			}
		}
		for _, path := range paths {
			if unit := c.units[path]; unit != nil && c.coverage != nil {
				scanned = append(scanned, c.coverage.bracket(c, unit)...)
			}
		}
		scanned = appendUniqueSorted(nil, scanned...)
		sampled = scanned
		r0, err = c.scan(scanned)
		if err != nil {
			return nil, nil, err
		}
		if !r0.Settled {
			return nil, nil, errContractJobKeyMoved
		}
		clock = c.observePoint(scanned, r0.Stamps)
		// Proof stamps: the last full scan's, overlaid with this observation.
		r0.Stamps = c.clock.last
	} else {
		r0, clock, err = c.fullScan()
		if err != nil {
			return nil, nil, err
		}
		if !r0.Settled {
			return nil, nil, errContractJobKeyMoved
		}
	}
	evidence, scratch, dir, err := c.openEvidence()
	if err != nil {
		return nil, nil, err
	}
	defer closeContractEvidence(scratch, dir)
	var current *contractUnitLedger
	reader := &contractPinnedReader{c: c, stamps: r0.Stamps, ledger: func() *contractUnitLedger { return current }, moved: make(map[string]bool)}
	indexers := c.indexers(evidence, reader.read)
	extractStart := time.Now()
	var extracted []*contractUnit
	var pointSet []string
	for i, path := range paths {
		if err := c.req.Yield(c.ctx); err != nil {
			return nil, nil, err
		}
		if err := c.ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !c.live[path] {
			if !c.excluded[path] {
				c.dropUnit(path)
			}
			continue
		}
		stamp, ok := r0.Stamps[path]
		if !ok && point {
			// Its row left since the last full scan; the fence's full scan
			// classifies the departure.
			c.waitAny = true
			unproven = append(unproven, path)
			continue
		}
		if !ok {
			// A census member with no receipt needs the namespace baseline.
			return nil, nil, fmt.Errorf("%w: census member %s has no boundary receipt", graph.ErrContractProjectionIncomplete, path)
		}
		if c.hooks != nil && c.hooks.beforeExtract != nil {
			c.hooks.beforeExtract(path)
		}
		ledger := newContractUnitLedger()
		current = ledger
		evidence.ledger = ledger
		unit, extractErr := c.extract(indexers[c.files[path].RepoPrefix], evidence, reader, c.files[path], ledger)
		evidence.ledger = nil
		current = nil
		if c.hooks != nil && c.hooks.afterExtract != nil {
			c.hooks.afterExtract(path)
		}
		if extractErr != nil && evidence.err == nil && (reader.moved[path] || errors.Is(extractErr, errContractSourceMoved)) {
			// Its own bytes moved past the stamp: unproven, evidence intact.
			c.blockMoved()
			unproven = append(unproven, path)
			continue
		}
		if extractErr != nil {
			// A sticky evidence error ends the round's extraction. It is a
			// job failure only when nothing the failed read touched moved
			// (load targets included) and the key held.
			touched := append([]string{path}, pointSet...)
			for owner := range ledger.owners {
				touched = append(touched, owner)
			}
			touched = appendUniqueSorted(nil, touched...)
			r1, err := c.scan(touched)
			if err != nil {
				return nil, nil, err
			}
			if r1.Settled && !c.anyMoved(r0, r1, touched) {
				return nil, nil, extractErr
			}
			c.blockMoved()
			unproven = append(unproven, paths[i:]...)
			break
		}
		unit.stamp = stamp
		unit.round = clock
		if point {
			ok, err := c.prove([]*contractUnit{unit}, r0, sampled)
			if err != nil {
				return nil, nil, err
			}
			proven = append(proven, ok...)
			if len(ok) == 0 {
				unproven = append(unproven, path)
			}
			continue
		}
		extracted = append(extracted, unit)
		pointSet = append(pointSet, path)
		for owner := range unit.owners {
			pointSet = append(pointSet, owner)
		}
	}
	c.report.ExtractDuration += time.Since(extractStart)
	if len(extracted) > 0 {
		ok, err := c.prove(extracted, r0)
		if err != nil {
			return nil, nil, err
		}
		proven = append(proven, ok...)
		held := make(map[*contractUnit]bool, len(ok))
		for _, unit := range ok {
			held[unit] = true
		}
		for _, unit := range extracted {
			if !held[unit] {
				unproven = append(unproven, unit.path)
			}
		}
	}
	return proven, appendUniqueSorted(nil, unproven...), nil
}

// prove takes one point scan R1 of the units' files and owners and returns
// the units proven against r0 and R1. An unproven unit's owners are kept for
// its retry round's R0.
func (c *contractConvergence) prove(units []*contractUnit, r0 ContractFollowupScan, sampled ...[]string) ([]*contractUnit, error) {
	var pointSet []string
	declarers := make(map[*contractUnit][]string, len(units))
	for _, unit := range units {
		pointSet = append(pointSet, unit.path)
		for owner := range unit.owners {
			pointSet = append(pointSet, owner)
		}
		declarers[unit] = c.keyDeclarers(unit)
		if c.coverage != nil && len(c.coverage.work) > 0 {
			declarers[unit] = appendUniqueSorted(declarers[unit], c.coverage.bracket(c, unit)...)
		}
		pointSet = append(pointSet, declarers[unit]...)
	}
	pointSet = appendUniqueSorted(nil, pointSet...)
	if c.hooks != nil && c.hooks.beforeR1 != nil {
		c.hooks.beforeR1(pointSet)
	}
	r1, err := c.scan(pointSet)
	if err != nil {
		return nil, err
	}
	// R1 is an observation too: a move it shows is on the clock, so the
	// revalidation of a unit it left unproven brackets the moved file. No read
	// of the round follows it.
	c.quietFrom = c.observePoint(pointSet, r1.Stamps)
	var proven []*contractUnit
	for _, unit := range units {
		coverageHeld := c.coverage == nil || len(c.coverage.work) == 0 || contractCoverageBracketHolds(r0, r1, declarers[unit])
		if c.coverage != nil && len(c.coverage.work) > 0 && len(sampled) > 0 {
			needed := append([]string{unit.path}, declarers[unit]...)
			for owner := range unit.owners {
				needed = append(needed, owner)
			}
			for _, owner := range needed {
				if !slices.Contains(sampled[0], owner) {
					coverageHeld = false
				}
			}
		}
		if !r1.Settled || !coverageHeld || !c.unitProven(unit, r0, r1, declarers[unit]) {
			own := r0.Stamps[unit.path]
			if r1.Settled && r1.Stamps[unit.path] == own && !unit.ledger.opaque {
				// Its own receipt held across its reads, so its bytes are the
				// stamped ones: it is kept and revalidated by asking its reads
				// again, those of every file that moved or was in flight under
				// them included (its own, read in flight, too).
				unit.torn = make(map[string]ContractReceiptStamp)
				if !own.quiescent() {
					unit.torn[unit.path] = own
				}
				for owner := range unit.owners {
					if c.movedUnder(owner, r0, r1) {
						unit.torn[owner] = r1.Stamps[owner]
					}
				}
				for _, declarer := range declarers[unit] {
					if c.movedUnder(declarer, r0, r1) {
						unit.torn[declarer] = r1.Stamps[declarer]
					}
				}
				c.coverage.ownExtracted(unit, r0, r1, sampled...)
				c.setUnit(unit)
				c.suspect[unit.path] = true
				// Extracted from the current bytes: progress, though not yet
				// a proof.
				c.progress++
				continue
			}
			owners := make(map[string]bool, len(unit.owners))
			for owner := range unit.owners {
				owners[owner] = true
			}
			for _, owner := range declarers[unit] {
				owners[owner] = true
			}
			c.retryOwners[unit.path] = owners
			c.blockUnproven(unit, r0, r1, declarers[unit])
			continue
		}
		delete(c.retryOwners, unit.path)
		guards := append([]string{unit.path}, declarers[unit]...)
		for owner := range unit.owners {
			guards = append(guards, owner)
		}
		c.coverage.extracted(unit, guards...)
		c.noteFillRead(unit)
		proven = append(proven, unit)
	}
	return proven, nil
}

// revalidate re-proves suspect units by asking their recorded reads again
// instead of extracting them. The pass is one point round: R0 of every
// unit's bracket (its own file, its byte reads and every file that moved
// after its round), observed by the change clock; the affected reads asked
// again on fresh evidence; R1 of the same paths. Only a read a move could
// have changed is asked again: an answer changes only when a row it holds
// changes (its file's receipt is rewritten first, so the file moved) or a
// moved file now declares what it asked for. Moves are content moves, plus
// the files the unit read torn. A moved file with no unit at its current
// content has its declarations read from its core rows inside the bracket. A
// plan's bracket is its own file, its byte reads and the moved files an asked
// read can depend on; a moved file no asked read depends on cannot change an
// answer. A unit is proven at R0's clock when its own contract content held,
// its bracket was quiescent (every core-write window closed, accepted or not)
// at R0 and unchanged at R1, every byte read's receipt still names the bytes
// it read, and every asked read's digest is the recorded one: its records are
// a function of exactly those inputs, and any later write to a file it read
// differs from that file's last observation, so a later scan stamps it past
// the round. A unit with a changed answer, own content or byte read is dropped
// for extraction; one whose bracket had a window open at R0 or moved while it
// was asked stays suspect and is asked again, its round blocked whatever its
// answers were. A changed answer acts only when its
// inputs were quiescent and unchanged around it, so it is a real change, never
// a tear.
//
// The enrichment round's re-check rides in the same bracket when something
// moved after its inputs were last shown current: its byte reads' receipts
// must still name the bytes read and its affected reads must answer with
// their recorded digests. It is then current at R0's clock; a changed input
// marks it stale, to rerun; a bracket with a window open or moved leaves it as
// it was, to ask again.
func (c *contractConvergence) revalidate(paths []string) (proven []*contractUnit, extract, unproven []string, err error) {
	type plan struct {
		// unit is nil for the enrichment round's plan.
		unit      *contractUnit
		ledger    *contractUnitLedger
		moves     contractMoves
		bracket   []string
		own       ContractReceiptStamp
		equal     bool
		askErr    error
		forced    bool
		newOwners []string
	}
	var plans []*plan
	var union []string
	add := func(p *plan) {
		if p.forced {
			p.bracket = append(p.bracket, c.coverage.bracket(c, p.unit)...)
		}
		for source := range p.ledger.sources {
			p.bracket = append(p.bracket, source)
		}
		for moved := range p.moves.moved {
			p.bracket = append(p.bracket, moved)
		}
		p.bracket = appendUniqueSorted(nil, p.bracket...)
		plans = append(plans, p)
		union = append(union, p.bracket...)
	}
	for _, path := range paths {
		unit := c.units[path]
		if unit == nil || !c.suspect[path] {
			extract = append(extract, path)
			continue
		}
		if !c.live[path] {
			c.dropUnit(path)
			continue
		}
		add(&plan{unit: unit, ledger: unit.ledger, moves: c.movesSince(unit.round, unit.torn), bracket: []string{path}, forced: c.coverage.needsRead(unit)})
	}
	var enrichment *plan
	if c.enrichmentNeedsRecheck() {
		if c.pp.ledger.opaque {
			// A read it cannot ask again: any move reruns it.
			c.pp.stale = true
		} else {
			enrichment = &plan{ledger: c.pp.ledger, moves: c.movesSince(c.pp.round, c.pp.torn)}
			add(enrichment)
		}
	}
	if len(plans) == 0 {
		return nil, extract, nil, nil
	}
	c.quietFrom = 0
	if err := c.req.Yield(c.ctx); err != nil {
		return nil, nil, nil, err
	}
	union = appendUniqueSorted(nil, union...)
	stats := &c.revalidation
	stats.units += len(plans)
	if enrichment != nil {
		stats.units--
	}
	started := time.Now()
	opened := started
	// The first re-ask after a wake carries its lead: from the event that
	// woke the wait to this opening scan.
	lead, woken := opened.Sub(c.wokeAt), !c.wokeAt.IsZero()
	source, closedAt := c.wakeSource, c.wokeAt
	c.wokeAt = time.Time{}
	c.wakeSource = "no_wake"
	r0, err := c.scan(union)
	if err != nil {
		return nil, nil, nil, err
	}
	if !r0.Settled {
		return nil, nil, nil, errContractJobKeyMoved
	}
	clock := c.observePoint(union, r0.Stamps)
	stats.scans += time.Since(started)
	if c.hooks != nil && c.hooks.beforeAsk != nil {
		c.hooks.beforeAsk()
	}
	// A move R0 itself found is a move since every plan's round: each plan's
	// moves are taken again so the reads it affects are asked again too. Every
	// such path is in the union R0 scanned and R1 scans again.
	for _, p := range plans {
		if p.unit != nil {
			p.moves = c.movesSince(p.unit.round, p.unit.torn)
		} else {
			p.moves = c.movesSince(c.pp.round, c.pp.torn)
		}
		for moved := range p.moves.moved {
			p.bracket = append(p.bracket, moved)
		}
		p.bracket = appendUniqueSorted(nil, p.bracket...)
	}
	// Only extraction questions inside this R0/R1 bracket may share a checked
	// backend projection. Every logical question still enters ReadCoreFile.
	batch := &contractReplayCoreBatch{}
	for _, p := range plans {
		for _, read := range p.ledger.reads {
			if read.kind != contractReadExtractionFile || len(read.args) != 1 {
				continue
			}
			path := read.args[0]
			if file, ok := c.files[path]; ok && slices.Contains(union, path) {
				if stamp, stamped := r0.Stamps[path]; stamped && stamp.quiescent() {
					batch.add(file)
				}
			}
		}
	}
	defer batch.close()
	endpointBatch := &contractEndpointReplayBatch{}
	for _, p := range plans {
		for _, read := range p.ledger.reads {
			if read.kind == contractReadConstCandidates && len(read.args) == 2 {
				endpointBatch.add(read.args[0])
			}
		}
	}
	defer endpointBatch.close()
	var evidence *contractFollowupEvidence
	var scratch *store_sqlite.Store
	var dir string
	defer func() { closeContractEvidence(scratch, dir) }()
	ensureEvidence := func() error {
		if evidence != nil && evidence.err == nil {
			return nil
		}
		closeContractEvidence(scratch, dir)
		evidence, scratch, dir, err = c.openEvidence()
		if err != nil {
			evidence, scratch, dir = nil, nil, ""
		} else {
			evidence.ctx = context.WithValue(evidence.ctx, contractReplayCoreBatchKey{}, batch)
			evidence.endpointReplay = endpointBatch
		}
		return err
	}
	asked := make(map[string]string)
	askedOwners := make(map[string][]string)
	started = time.Now()
	// What each moved file without a current unit declares now, read only
	// while its core-write window is closed (accepted or not). A write that
	// begins after R0 and overlaps the read is observed by R1 (the union
	// scans every moved file) past every plan's round, and the file, live
	// with no current unit, is proven before any publish: its proof's key
	// check re-marks every lookup its declarations answer.
	declarations := make(map[string]contractDeclaration)
	for _, p := range plans {
		for path := range p.moves.unknown {
			if _, done := declarations[path]; done {
				continue
			}
			if stamp, ok := r0.Stamps[path]; !ok || !stamp.quiescent() {
				continue
			}
			if err := ensureEvidence(); err != nil {
				return nil, nil, nil, err
			}
			ledger := evidence.ledger
			evidence.ledger = newContractUnitLedger()
			nodes, edges, loadErr := evidence.loadFile(path)
			evidence.ledger = ledger
			if loadErr == nil && evidence.err == nil {
				declarations[path] = contractDeclarationOf(path, nodes, edges)
			}
		}
	}
	stats.declare += time.Since(started)
	started = time.Now()
	var asking []*plan
	for _, p := range plans {
		path := ""
		if p.unit != nil {
			path = p.unit.path
			own, ok := r0.Stamps[path]
			if !ok {
				// Its row left; the fence's full scan classifies the departure.
				c.waitAny = true
				unproven = append(unproven, path)
				continue
			}
			if (p.unit.router != nil && own.SourceFingerprint != p.unit.stamp.SourceFingerprint) || (own.identity() != p.unit.stamp.identity() && c.contentChangedSince(path, p.unit.round)) {
				extract = append(extract, c.dropForExtraction(path))
				continue
			}
			p.own = own
		}
		for unknown := range p.moves.unknown {
			if declaration, ok := declarations[unknown]; ok {
				p.moves.resolve(unknown, declaration)
			}
		}
		var reads []contractRecordedRead
		for _, read := range p.ledger.reads {
			if p.forced || p.moves.affected(read) {
				reads = append(reads, read)
			}
		}
		// What must hold across the pass: its own file, its byte reads and
		// the moved files an asked read can depend on.
		p.bracket = p.bracket[:0]
		if p.forced {
			p.bracket = append(p.bracket, c.coverage.bracket(c, p.unit)...)
		}
		if p.unit != nil {
			p.bracket = append(p.bracket, path)
		}
		for source := range p.ledger.sources {
			p.bracket = append(p.bracket, source)
		}
		p.bracket = appendUniqueSorted(nil, append(p.bracket, p.moves.inputsOf(reads)...)...)
		// Answers are admissible only from rows no core write overlapped:
		// every input's core-write window closed at R0 and its stamp
		// unchanged at R1. An input whose window is open is asked again once
		// it closes, never compared: its rows may be between an evict and its
		// add, and two such answers can agree while both are torn. Its
		// receipt need not be accepted.
		settled := true
		for _, input := range p.bracket {
			if stamp, stamped := r0.Stamps[input]; stamped && !stamp.quiescent() {
				settled = false
				c.block(input, stamp)
				if stats.writing == "" {
					stats.writing = input
				}
			}
		}
		if p.unit == nil && !settled {
			continue
		}
		// The enrichment round asks every name, id and path read again while
		// a moved file's declarations are unknown.
		if p.unit != nil && (!settled || !p.moves.declaredKnown) {
			if !settled {
				stats.inFlight++
			} else {
				stats.unknown++
				// A declaration read only inside a closed window: one still
				// open is waited for.
				for unknown := range p.moves.unknown {
					if stamp, stamped := r0.Stamps[unknown]; stamped && !stamp.quiescent() {
						c.block(unknown, stamp)
					} else {
						c.waitAny = true
					}
				}
			}
			unproven = append(unproven, path)
			continue
		}
		moved := false
		for source, fingerprint := range p.ledger.sources {
			stamp, stamped := r0.Stamps[source]
			if p.unit != nil && source == path && !c.contentChangedSince(path, p.unit.round) {
				// Its own bytes moved without moving its contract content.
				continue
			}
			if (stamped && stamp.SourceFingerprint != fingerprint) || (!stamped && p.unit == nil) {
				moved = true
			}
		}
		if moved {
			if p.unit == nil {
				c.pp.stale = true
				continue
			}
			extract = append(extract, c.dropForExtraction(path))
			continue
		}
		p.equal = true
		if len(reads) > 0 {
			if err := ensureEvidence(); err != nil {
				return nil, nil, nil, err
			}
			if p.forced {
				p.equal, p.askErr = c.replayObserved(evidence, reads, asked, askedOwners, func(owner string) { p.newOwners = append(p.newOwners, owner) })
			} else {
				p.equal, p.askErr = c.replayObserved(evidence, reads, asked, askedOwners, nil)
			}
			if ctxErr := c.ctx.Err(); ctxErr != nil {
				return nil, nil, nil, ctxErr
			}
		}
		if p.unit == nil {
			c.report.EnrichRechecks++
			for _, read := range reads {
				if read.kind != contractReadRepoFiles && read.kind != contractReadLivePaths {
					c.report.EnrichRecheckReads++
				}
			}
		}
		asking = append(asking, p)
	}
	stats.ask += time.Since(started)
	stats.reads += len(asked)
	if len(asking) == 0 {
		return nil, extract, unproven, nil
	}
	if c.hooks != nil && c.hooks.beforeRevalidateR1 != nil {
		c.hooks.beforeRevalidateR1()
	}
	started = time.Now()
	r1, err := c.scan(union)
	if err != nil {
		return nil, nil, nil, err
	}
	if !r1.Settled {
		return nil, nil, nil, errContractJobKeyMoved
	}
	// No read of the pass follows R1.
	c.quietFrom = c.observePoint(union, r1.Stamps)
	stats.scans += time.Since(started)
	stats.span = time.Since(opened)
	moved := false
	defer func() { c.noteReask(lead, woken, stats.span, moved, source, closedAt) }()
	// An answer asked while an input moved may be torn: it is asked again,
	// never acted on, and the round waits for that input's window to close.
	blockMoves := func(bracket []string) {
		for _, path := range bracket {
			before, had := r0.Stamps[path]
			after, has := r1.Stamps[path]
			if had != has || before != after {
				stats.noteMoved(before, after, has)
				c.block(path, r1.Stamps[path])
			}
		}
	}
	for _, p := range asking {
		held := p.askErr == nil && p.equal
		if p.unit == nil {
			switch {
			case changedBetween(r0, r1, p.bracket):
				moved = true
				blockMoves(p.bracket)
			case !held:
				c.pp.stale = true
			default:
				c.pp.round, c.pp.torn = clock, nil
				c.progress++
			}
			continue
		}
		path := p.unit.path
		if p.forced {
			// Newly returned owners were not necessarily in this point R0.
			// Retain them and retry with their windows in the opening scan.
			missing := false
			for _, owner := range p.newOwners {
				if !slices.Contains(p.bracket, owner) {
					if c.coverage.extra[path] == nil {
						c.coverage.extra[path] = map[string]bool{}
					}
					c.coverage.extra[path][owner] = true
					missing = true
				}
			}
			if missing {
				c.blockMoved()
				unproven = append(unproven, path)
				continue
			}
		}
		if c.hooks != nil && c.hooks.revalidated != nil {
			c.hooks.revalidated(path, held)
		}
		switch {
		case changedBetween(r0, r1, p.bracket) || (p.forced && !contractCoverageBracketHolds(r0, r1, p.bracket)):
			// An input moved while it was asked: its answers prove nothing,
			// equal or not, and the round is blocked.
			moved = true
			blockMoves(p.bracket)
			stats.moved++
			if held {
				stats.equalMoved++
			}
			unproven = append(unproven, path)
		case !held:
			stats.changed++
			extract = append(extract, c.dropForExtraction(path))
		default:
			p.unit.round, p.unit.stamp, p.unit.torn = clock, p.own, nil
			if p.forced {
				c.coverage.record(p.unit, p.bracket)
				c.noteFillRead(p.unit)
			}
			c.report.Revalidated++
			proven = append(proven, p.unit)
		}
	}
	return proven, extract, unproven, nil
}

// dropForExtraction drops a suspect whose inputs changed, keeping its owners
// for the R0 of the point round that extracts it again. Finding a changed
// unit is progress: the extraction that follows reads the current state.
func (c *contractConvergence) dropForExtraction(path string) string {
	c.progress++
	if unit := c.units[path]; unit != nil {
		owners := make(map[string]bool, len(unit.owners))
		for owner := range unit.owners {
			owners[owner] = true
		}
		// A changed answer requires fresh extraction, but its already-known
		// proof frontier still belongs in that extraction's opening scan.
		// Point R0 must sample read owners and declarers too; otherwise the
		// new raw unit is needlessly retained for another coverage replay.
		if c.coverage != nil && len(c.coverage.work) > 0 {
			for _, owner := range c.coverage.bracket(c, unit) {
				owners[owner] = true
			}
		}
		c.retryOwners[path] = owners
	}
	c.dropUnit(path)
	return path
}

// replay asks recorded reads again on evidence and reports whether every
// answer's digest is the recorded one. A read that fails is an error, which
// the caller treats as a changed answer unless its inputs moved meanwhile.
func (c *contractConvergence) replay(evidence *contractFollowupEvidence, reads []contractRecordedRead, asked map[string]string) (bool, error) {
	return c.replayObserved(evidence, reads, asked, nil, nil)
}

func (c *contractConvergence) replayObserved(evidence *contractFollowupEvidence, reads []contractRecordedRead, asked map[string]string, owners map[string][]string, observe func(string)) (bool, error) {
	ledger := evidence.ledger
	evidence.ledger = newContractUnitLedger()
	defer func() { evidence.ledger = ledger }()
	for _, read := range reads {
		// One pass asks each distinct read once: units of a pass that looked
		// up the same names compare against the same answer.
		key := read.kind + "\x00" + strings.Join(read.args, "\x00")
		digest, done := asked[key]
		if !done {
			evidence.ledger = newContractUnitLedger()
			answer, err := c.ask(evidence, read)
			if err != nil {
				return false, err
			}
			if evidence.err != nil {
				return false, evidence.err
			}
			if owners != nil {
				found := contractAnswerOwners(answer)
				for owner := range evidence.ledger.owners {
					found = append(found, owner)
				}
				for _, recorded := range evidence.ledger.reads {
					found = append(found, recorded.owners...)
				}
				owners[key] = appendUniqueSorted(nil, found...)
			}
			encoded, ok := contractAnswerDigest(answer)
			if !ok {
				return false, nil
			}
			digest = encoded
			if asked != nil {
				asked[key] = digest
			}
		}
		if observe != nil {
			for _, owner := range owners[key] {
				observe(owner)
			}
		}
		if digest != read.digest {
			return false, nil
		}
	}
	return true, nil
}

// ask repeats one recorded read through the same door that answered it.
func (c *contractConvergence) ask(evidence *contractFollowupEvidence, read contractRecordedRead) (any, error) {
	switch read.kind {
	case contractReadFile:
		if len(read.args) != 1 {
			break
		}
		nodes, edges, err := evidence.loadFile(read.args[0])
		return contractFileAnswer{Nodes: nodes, Edges: edges}, err
	case contractReadFileNodes:
		if len(read.args) != 1 {
			break
		}
		return evidence.loadFileNodes(read.args[0])
	case contractReadExtractionFile:
		if len(read.args) != 1 {
			break
		}
		nodes, edges, err := evidence.readExtractionFileAnswer(read.args[0])
		return contractFileAnswer{Nodes: nodes, Edges: edges}, err
	case contractReadIDs:
		return c.askIDs(evidence, read.args)
	case contractReadNames:
		return c.askNames(evidence, read.args)
	case contractReadConstCandidates:
		if len(read.args) != 2 {
			break
		}
		return evidence.endpointConstantCandidates(read.args[0], read.args[1])
	case contractReadConstants:
		return evidence.ConstantValuesByNodeIDs(read.args)
	case contractReadRepoFiles:
		if len(read.args) != 4 {
			break
		}
		return evidence.RepoFilePaths(read.args[0], read.args[1], contractSplitArgs(read.args[2]), contractSplitArgs(read.args[3])), evidence.err
	case contractReadSemantic:
		if len(read.args) != 1 {
			break
		}
		var sites []graph.SemanticBindingSite
		if err := json.Unmarshal([]byte(read.args[0]), &sites); err != nil {
			return nil, err
		}
		return evidence.semanticTypes(sites)
	case contractReadSeeds:
		if len(read.args) != 1 {
			break
		}
		nodes, edges, _, err := c.seedRows(evidence, read.args[0])
		return contractFileAnswer{Nodes: nodes, Edges: edges}, err
	case contractReadLivePaths:
		if len(read.args) != 1 {
			break
		}
		return c.livePathsOf(read.args[0]), nil
	case contractReadEndpoints:
		return c.endpointRows(evidence, read.args)
	}
	return nil, fmt.Errorf("contract followup: unreplayable read %s", read.kind)
}

// askNames asks a name lookup again of the selected core directly. The facade
// answered it from its scratch store, which holds only rows imported from the
// core, so the core's rows by those names, detached the same way, are the
// same answer; reading them directly imports nothing.
func (c *contractConvergence) askNames(evidence *contractFollowupEvidence, names []string) (map[string][]*graph.Node, error) {
	defer c.guard.enter()()
	out := make(map[string][]*graph.Node)
	err := graph.VisitNodesByNamesContext(c.ctx, c.snap.Core, names, func(node *graph.Node) bool {
		if node == nil || node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			return true
		}
		detached := evidence.detach(node)
		if detached == nil {
			return false
		}
		out[detached.Name] = append(out[detached.Name], detached)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, evidence.err
}

// askIDs asks an id lookup again of the selected core directly, as askNames.
func (c *contractConvergence) askIDs(evidence *contractFollowupEvidence, ids []string) (map[string]*graph.Node, error) {
	defer c.guard.enter()()
	found, err := graph.ContractSourceNodesContext(c.ctx, c.snap.Core, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*graph.Node, len(found))
	for id, node := range found {
		if node == nil || node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			return nil, fmt.Errorf("contract followup: attempted to import core contract output")
		}
		detached := evidence.detach(node)
		if detached == nil {
			return nil, evidence.err
		}
		out[id] = detached
	}
	return out, evidence.err
}

// contractJoinArgs and contractSplitArgs carry a list in one recorded
// argument.
func contractJoinArgs(values []string) string { return strings.Join(values, "\x1f") }

func contractSplitArgs(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "\x1f")
}

func (c *contractConvergence) anyMoved(r0, r1 ContractFollowupScan, paths []string) bool {
	for _, path := range paths {
		before, ok := r0.Stamps[path]
		if !ok {
			continue
		}
		if !before.quiescent() || r1.Stamps[path] != before {
			return true
		}
	}
	return false
}

// changedBetween reports whether a path stamped at r0 has another stamp at r1:
// a receipt write began or ended, or a core-write window opened or closed,
// while the pass asked its reads.
func changedBetween(r0, r1 ContractFollowupScan, paths []string) bool {
	for _, path := range paths {
		if before, ok := r0.Stamps[path]; ok && r1.Stamps[path] != before {
			return true
		}
	}
	return false
}

// keyDeclarers lists the files other than the unit's own whose current unit
// declares a key the unit looked up: a lookup that missed while one of them
// was between its evict and its add tore without any row naming the file.
func (c *contractConvergence) keyDeclarers(unit *contractUnit) []string {
	var out []string
	for key := range unit.keys {
		for _, path := range c.declarers.lookup(key, c.units) {
			if path != unit.path {
				out = append(out, path)
			}
		}
	}
	return appendUniqueSorted(nil, out...)
}

// movedUnder reports whether path was not quiescent at r0 or changed by r1.
func (c *contractConvergence) movedUnder(path string, r0, r1 ContractFollowupScan) bool {
	before, stamped := r0.Stamps[path]
	return stamped && (!before.quiescent() || r1.Stamps[path] != before)
}

// setUnit installs a unit for its path and indexes its declarations.
func (c *contractConvergence) setUnit(unit *contractUnit) {
	if c.req.delta != nil {
		c.req.delta.ownRaw(unit)
	}
	c.coverage.remember(c.units[unit.path])
	c.coverage.remember(unit)
	c.unindex(c.units[unit.path])
	c.units[unit.path] = unit
	c.declarers.add(unit)
}

func (c *contractConvergence) unindex(unit *contractUnit) {
	if unit == nil {
		return
	}
	c.declarers.remove(unit)
}

// unitProven: every owner of the unit, every file declaring a key it looked
// up and its own file is quiescent at R0 and unchanged at R1. An owner without
// a receipt row (an unstamped file) is a path key instead, covered by the key
// check.
func (c *contractConvergence) unitProven(unit *contractUnit, r0, r1 ContractFollowupScan, declarers []string) bool {
	for _, declarer := range declarers {
		if c.movedUnder(declarer, r0, r1) {
			return false
		}
	}
	for owner := range unit.owners {
		before, stamped := r0.Stamps[owner]
		if !stamped {
			delete(unit.owners, owner)
			unit.keys["path:"+owner] = true
			viewmetrics.Count(viewmetrics.ContractAnalysisUnstampedReadsTotal)
			continue
		}
		if !before.quiescent() || r1.Stamps[owner] != before {
			return false
		}
	}
	before := r0.Stamps[unit.path]
	return before.quiescent() && r1.Stamps[unit.path] == before
}

// blockUnproven records why a unit was left unproven: each of its own file,
// owners and key declarers that was not quiescent at R0 or moved by R1, or,
// when none was (the key moved), a refusal with nothing to wait on.
func (c *contractConvergence) blockUnproven(unit *contractUnit, r0, r1 ContractFollowupScan, declarers []string) {
	blocked := c.blocks
	for _, path := range append(append([]string{unit.path}, declarers...), slices.Sorted(maps.Keys(unit.owners))...) {
		if c.movedUnder(path, r0, r1) {
			if after, has := r1.Stamps[path]; !has || after != r0.Stamps[path] {
				c.revalidation.noteMoved(r0.Stamps[path], after, has)
			}
			c.block(path, r1.Stamps[path])
		}
	}
	if c.blocks == blocked {
		c.waitAny = true
	}
}

// keyCheck makes stale every unit that looked up something path now
// declares (or path itself) before path's receipt last moved: a declaration
// the lookup missed is either new or was torn by an apply of path. It runs
// on proofs of path, which a move that keeps path's contract content does not
// cause; a lookup a later such move tears is the proving unit's own check.
func (c *contractConvergence) keyCheck(path string, declared []string, stale map[string]string) {
	movedAt := c.clock.changedAt[path]
	for other, unit := range c.units {
		if other == path || unit.round >= movedAt {
			continue
		}
		for key := range unit.keys {
			if contractDeclaredHas(declared, key) {
				// The lookups path's declarations answer are asked again
				// whatever path's content did.
				if unit.torn == nil {
					unit.torn = make(map[string]ContractReceiptStamp)
				}
				unit.torn[path] = c.clock.last[path]
				c.staleUnit(other, viewmetrics.ContractFilesKeyCheck, stale)
				break
			}
		}
	}
}

// bytesMoved reports whether the reader read path's bytes (path is not the
// reader's own file) and path's receipt no longer names those bytes. A move
// that keeps a file's contract content can still change bytes another file's
// extraction or the enrichment round read directly.
func (c *contractConvergence) bytesMoved(ledger *contractUnitLedger, path, own string) bool {
	if ledger == nil || path == own {
		return false
	}
	fingerprint, read := ledger.sources[path]
	if !read {
		return false
	}
	stamp, ok := c.clock.last[path]
	return !ok || stamp.SourceFingerprint != fingerprint
}

// staleUnit takes a unit out of the proven set for a move of something it
// read: one whose reads can be asked again stays as a suspect to revalidate,
// any other is dropped for extraction.
func (c *contractConvergence) staleUnit(path, reason string, stale map[string]string) {
	stale[path] = reason
	if unit := c.units[path]; unit != nil && !unit.excluded && unit.ledger != nil && !unit.ledger.opaque {
		c.suspect[path] = true
		return
	}
	c.dropUnit(path)
}

func (c *contractConvergence) dropUnit(path string) {
	c.coverage.remember(c.units[path])
	c.unindex(c.units[path])
	delete(c.units, path)
	delete(c.suspect, path)
}

// accept installs proven units and runs their key checks.
func (c *contractConvergence) accept(proven []*contractUnit, stale map[string]string) {
	c.progress += len(proven)
	for _, unit := range proven {
		c.setUnit(unit)
		delete(c.suspect, unit.path)
		// Proven at a scan after whatever marked it pending: the key checks
		// below re-mark it if a later proof calls for it.
		delete(stale, unit.path)
		if c.hooks != nil && c.hooks.proven != nil {
			c.hooks.proven(unit)
		}
	}
	for _, unit := range proven {
		c.keyCheck(unit.path, unit.declared, stale)
	}
}

// staleUnits applies rules (a) and (b) to every unit, and the removal key
// check to every unit whose file left the census.
func (c *contractConvergence) staleUnits(stale map[string]string) {
	for path := range c.units {
		if !c.live[path] && !c.excluded[path] {
			c.dropUnit(path)
			c.keyCheck(path, []string{"path:" + path}, stale)
		}
	}
	for path, unit := range c.units {
		last := c.clock.last[path]
		if !unit.excluded && unit.router != nil && unit.stamp.SourceFingerprint != last.SourceFingerprint {
			stale[path] = viewmetrics.ContractFilesStale
			c.dropUnit(path)
			continue
		}
		if !unit.excluded && unit.stamp != last && !c.contentChangedSince(path, unit.round) {
			// Its receipt moved without moving its contract content: the unit
			// describes the current stamp.
			unit.stamp = last
		}
		if unit.stamp.identity() != last.identity() {
			// Its own content moved: extracted again.
			stale[path] = viewmetrics.ContractFilesStale
			c.dropUnit(path)
			continue
		}
		if c.contentChangedSince(path, unit.round) {
			// Its row was replaced: its own load is asked again.
			c.staleUnit(path, viewmetrics.ContractFilesStale, stale)
			continue
		}
		for owner := range unit.owners {
			if c.contentChangedSince(owner, unit.round) || c.bytesMoved(unit.ledger, owner, path) {
				c.staleUnit(path, viewmetrics.ContractFilesStale, stale)
				break
			}
		}
	}
	for path := range c.live {
		if _, ok := c.units[path]; !ok {
			if _, marked := stale[path]; !marked {
				stale[path] = viewmetrics.ContractFilesCold
			}
		}
	}
	for path := range c.excluded {
		delete(stale, path)
		if _, ok := c.units[path]; !ok {
			c.installExcluded(path)
			c.keyCheck(path, []string{"path:" + path}, stale)
		}
	}
}

// installExcluded records an excluded failure envelope as a unit with no
// records and no reads, proven at the current clock.
func (c *contractConvergence) installExcluded(path string) {
	c.setUnit(&contractUnit{path: path, stamp: c.clock.last[path], round: c.clock.clock, excluded: true,
		owners: map[string]bool{}, keys: map[string]bool{}, declared: []string{"path:" + path}})
}

// rounds proves every pending path. A path's first attempt runs in a full
// round of up to contractRoundFiles files; a path left unproven retries in
// point rounds, each started by the next Published or receipt event (or by a
// wait of contractRoundWait that saw none), until it is proven. The job ends
// superseded after contractIdleRounds counted rounds without progress.
func (c *contractConvergence) rounds(pending map[string]string) error {
	var unproven []string
	// The level a retry waits past is sampled before the R0 of the round that
	// left its files unproven: an event landing after that sample wakes the
	// retry even before the wait begins.
	var since contractWake
	for len(pending) > 0 || len(unproven) > 0 {
		if len(pending) > 0 {
			paths := make([]string, 0, len(pending))
			for path := range pending {
				if c.excluded[path] {
					delete(pending, path)
					continue
				}
				paths = append(paths, path)
			}
			sort.Strings(paths)
			// A pending path takes a first round again, not a retry.
			retained := unproven[:0]
			for _, path := range unproven {
				if _, again := pending[path]; !again {
					retained = append(retained, path)
				}
			}
			unproven = retained
			// Suspects are revalidated first, in point rounds; one whose
			// answers changed is extracted at once in its own point round.
			var suspects, extracted []string
			for _, path := range paths {
				if c.suspect[path] && c.units[path] != nil {
					suspects = append(suspects, path)
				} else {
					extracted = append(extracted, path)
				}
			}
			if len(suspects) > 0 {
				if c.fillContended() {
					suspects = suspects[:min(16, len(suspects))]
				}
				for _, path := range suspects {
					delete(pending, path)
				}
				// The retry waits for what this pass's refusals name beside
				// what the files already unproven wait for.
				waitOn, waitAny := c.waitOn, c.waitAny
				mark := c.markRound()
				if len(unproven) > 0 {
					c.waitOn, c.waitAny = waitOn, waitAny
				}
				_, missed, epoch, err := c.settle(suspects, pending, viewmetrics.ContractFilesStale)
				if err != nil {
					return err
				}
				c.endRound(mark, false)
				if len(missed) > 0 {
					if len(unproven) == 0 {
						since = epoch
					}
					unproven = appendUniqueSorted(unproven, missed...)
				}
				continue
			}
			paths = extracted
			size := contractRoundFiles
			if c.hooks != nil && c.hooks.roundFiles > 0 {
				size = c.hooks.roundFiles
			}
			if c.fillContended() {
				size = min(size, 16)
			}
			// A first round is not accounted, but its retry waits for what
			// its refusals name, beside what the files already unproven
			// (refused by the suspects' pass or the last retry) wait for.
			if len(unproven) == 0 {
				c.waitOn, c.waitAny = nil, false
			}
			for start := 0; start < len(paths); start += size {
				chunk := paths[start:min(start+size, len(paths))]
				for _, path := range chunk {
					countContractFiles(pending[path], 1)
				}
				epoch := c.wake()
				proven, missed, err := c.round(chunk, false)
				if err != nil {
					return err
				}
				for _, path := range chunk {
					delete(pending, path)
				}
				c.accept(proven, pending)
				if err := c.continueFill(len(proven), len(pending) > 0 || len(unproven) > 0 || len(missed) > 0); err != nil {
					return err
				}
				if len(missed) > 0 {
					if len(unproven) == 0 {
						since = epoch
					}
					unproven = appendUniqueSorted(unproven, missed...)
				}
			}
			continue
		}
		if err := c.idleErr(); err != nil {
			return fmt.Errorf("%w: %d files unproven, first %s", err, len(unproven), unproven[0])
		}
		waitedOut := !c.waitWake(since, time.Now().Add(c.roundWait()))
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if !waitedOut && c.hooks != nil && c.hooks.woke != nil {
			c.hooks.woke()
		}
		// One retry round per file: each unit's R0 is taken right before its
		// own reads, so its proof spans only its own extraction. A suspect
		// is revalidated, the pass sharing one fresh evidence store.
		passStart, extractedBefore, extractBefore, revalidatedBefore := time.Now(), c.report.Files, c.report.ExtractDuration, c.report.Revalidated
		c.revalidation = contractRevalidationStats{}
		mark := c.markRound()
		progress, missed, epoch, err := c.settle(unproven, pending, viewmetrics.ContractFilesRetry)
		if err != nil {
			return err
		}
		c.endRound(mark, waitedOut)
		if len(missed) > 0 {
			// Wait for the event after the scan that left them unproven, not
			// for one that landed earlier in this pass.
			since = epoch
		}
		fields := []zap.Field{zap.String("repo", c.snap.Key.RepoPrefix), zap.Int("files", len(unproven)),
			zap.Int("extracted", c.report.Files-extractedBefore), zap.Int("revalidated", c.report.Revalidated-revalidatedBefore),
			zap.Int("progress", progress), zap.Int("idle", c.idle), zap.Int("waited", c.waited), zap.Bool("waited_out", waitedOut),
			zap.Bool("blocked", c.blocks > mark.blocks), zap.Int("wait_on", len(c.waitOn)), zap.Bool("wait_any", c.waitAny),
			zap.Duration("extract", c.report.ExtractDuration-extractBefore),
			zap.Duration("elapsed", time.Since(passStart))}
		c.logger.Info("contract analysis retry", append(fields, c.revalidation.fields()...)...)
		unproven = missed
	}
	return nil
}

// fillCanContinue restricts cooperative checkpoints to resumable cache jobs.
func (c *contractConvergence) fillCanContinue() bool {
	return c.req.delta != nil && !c.req.delta.disabled && (c.report.Mode == viewmetrics.ContractModeFill || c.report.Mode == viewmetrics.ContractModeCold) && c.req.OtherActorWaiting != nil
}

func (c *contractConvergence) fillContended() bool {
	return c.fillCanContinue() && c.req.OtherActorWaiting()
}

func (c *contractConvergence) continueFill(proven int, more bool) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if proven > 0 && more && c.fillContended() {
		c.req.delta.continued = true
		return &ContractFillContinued{}
	}
	return nil
}

// settle proves paths in point rounds, right after a wake or at an
// iteration's start: a path with no unit is extracted in its own point round,
// then the suspects are revalidated together, and one whose answers changed is
// extracted at once in its own point round, inside the same quiet window. It
// returns the progress it made (proofs, and changed units dropped for
// extraction) and the wake level sampled right before the
// earliest scan that left a path unproven: the next event after that scan is
// the one a retry waits for.
func (c *contractConvergence) settle(paths []string, pending map[string]string, reason string) (int, []string, contractWake, error) {
	var suspects, retried []string
	for _, path := range paths {
		if c.suspect[path] && c.units[path] != nil {
			suspects = append(suspects, path)
		} else {
			retried = append(retried, path)
		}
	}
	progress := c.progress
	var missed []string
	var since contractWake
	note := func(epoch contractWake, miss []string) {
		// Samples are taken in order: the first miss's is the earliest.
		if len(miss) > 0 && len(missed) == 0 {
			since = epoch
		}
		missed = append(missed, miss...)
	}
	extractEach := func(paths []string, moreAfter bool) error {
		countContractFiles(reason, len(paths))
		for index, path := range paths {
			epoch := c.wake()
			proven, miss, err := c.round([]string{path}, true)
			if err != nil {
				return err
			}
			c.accept(proven, pending)
			if err := c.continueFill(len(proven), moreAfter || index+1 < len(paths) || len(pending) > 0 || len(missed) > 0 || len(miss) > 0); err != nil {
				return err
			}
			note(epoch, miss)
		}
		return nil
	}
	// Files whose own content moved go first: their new declarations are
	// what a suspect's reads are checked against.
	if err := extractEach(retried, len(suspects) > 0); err != nil {
		return 0, nil, since, err
	}
	size := max(1, len(suspects))
	if c.fillContended() {
		size = min(size, 16)
	}
	for start := 0; start < len(suspects); start += size {
		epoch := c.wake()
		proven, extract, miss, err := c.revalidate(suspects[start:min(start+size, len(suspects))])
		if err != nil {
			return 0, nil, since, err
		}
		c.accept(proven, pending)
		if err := c.continueFill(len(proven), start+size < len(suspects) || len(extract) > 0 || len(pending) > 0 || len(missed) > 0 || len(miss) > 0); err != nil {
			return 0, nil, since, err
		}
		note(epoch, miss)
		if err := extractEach(extract, start+size < len(suspects)); err != nil {
			return 0, nil, since, err
		}
	}
	return c.progress - progress, missed, since, nil
}

// run converges the job and returns the final enrichment round.
func (c *contractConvergence) run() error {
	if c.coverage == nil {
		c.coverage = newContractWorkCoverage(c.snap.Work)
	}
	complete := false
	if c.req.delta != nil {
		c.req.delta.adopt(c)
		defer func() { c.req.delta.finish(c, complete) }()
	}
	// A full scan first, so changes since the last job are on the clock
	// before the first round.
	first, _, err := c.fullScan()
	if err != nil {
		return err
	}
	if !first.Settled {
		return errContractJobKeyMoved
	}
	if err := c.coverage.validate(c); err != nil {
		return err
	}
	c.coverage.anchored = true
	c.loadContents(first.Stamps)
	pending := make(map[string]string)
	c.coverage.observe(c)
	c.report.Mode = viewmetrics.ContractModeCold
	if session := c.req.delta; session != nil && !session.disabled {
		if session.cache != nil {
			c.coverage.receiptKeys(session.cache.receipts)
		}
		receipts, readErr := session.readReceipts(c, first.Stamps)
		if readErr != nil {
			return readErr
		}
		c.coverage.receiptKeys(receipts)
		membershipUnknown := session.membershipUnknown || contractDeltaMembershipUnknown(c.snap.Work)
		if session.cache != nil && session.cache.continuation.resumesBarrier(session.identity, c.coverage.work) {
			membershipUnknown = false
		}
		plan := session.cache.plan(session.identity, first.Census, receipts, membershipUnknown)
		c.report.Mode, c.report.CacheReason, c.report.ChangedFiles = plan.mode, plan.reason, slices.Clone(plan.extract)
		frontier, _ := json.Marshal(plan.extract)
		c.logger.Info("contract delta plan", zap.String("mode", plan.mode), zap.String("reason", plan.reason), zap.Int("extract_count", len(plan.extract)), zap.Strings("extract_paths", plan.extract[:min(16, len(plan.extract))]), zap.String("extract_digest", contractInputHash(frontier)), zap.Int("revalidate_count", len(plan.revalidate)), zap.Strings("revalidate_paths", plan.revalidate[:min(16, len(plan.revalidate))]), zap.Int("removed_count", len(plan.removed)))
		if plan.mode == viewmetrics.ContractModeDelta {
			c.report.RebuildReason = ContractFollowupChangedBoundary
		} else {
			c.report.RebuildReason = ContractFollowupColdBaseline
		}
		if session.cache != nil {
			session.cache.continuation.resume(c, session.cache)
		}
		for _, path := range plan.removed {
			c.dropUnit(path)
		}
		for _, path := range plan.revalidate {
			c.staleUnit(path, viewmetrics.ContractFilesKeyCheck, pending)
		}
		reason := viewmetrics.ContractFilesDelta
		if plan.mode == viewmetrics.ContractModeFill {
			reason = viewmetrics.ContractFilesFill
		}
		if plan.mode == viewmetrics.ContractModeCold {
			reason = viewmetrics.ContractFilesCold
		}
		for _, path := range plan.extract {
			if c.live[path] {
				c.dropUnit(path)
				pending[path] = reason
			}
		}
		// Candidate deltas never replace clock/byte/query proof, including source
		// changes whose boundary and ProducedInputs digest stayed equal.
		c.staleUnits(pending)
	} else {
		if c.snap.opaqueContractInputs {
			c.report.CacheReason = "receipt_envelope_exceeded"
			c.report.RebuildReason = ContractFollowupColdBaseline
		}
		for path := range c.live {
			pending[path] = viewmetrics.ContractFilesCold
		}
	}
	for path := range c.excluded {
		c.installExcluded(path)
	}
	c.coverage.refresh(c, pending)
	if err := c.coverage.validate(c); err != nil {
		return err
	}
	defer c.noteWindowGaps()
	for iteration := 0; ; iteration++ {
		c.report.Iterations++
		viewmetrics.Count(viewmetrics.ContractAnalysisIterationsTotal)
		iterationStart := time.Now()
		c.iteration, c.leads, c.spans = iteration, nil, nil
		if iteration == 1 {
			c.blockedSince = iterationStart
		}
		phase := *c.report
		progress := c.progress
		if err := c.rounds(pending); err != nil {
			return err
		}
		ppStart := time.Now()
		ran := false
		if c.enrichmentHolds() && c.enrichmentNeedsRecheck() {
			// Something moved after the enrichment round's inputs were last
			// shown current and no revalidation pass re-checked them since.
			if err := c.recheckEnrichment(); err != nil {
				return err
			}
		}
		if !c.enrichmentHolds() {
			if c.pp != nil {
				c.report.EnrichReruns++
			}
			if err := c.enrich(); err != nil {
				return err
			}
			ran = true
		}
		ppDuration := time.Since(ppStart)
		if c.hooks != nil && c.hooks.beforeFence != nil {
			c.hooks.beforeFence(iteration)
		}
		// The fence is the iteration's last round: it counts like any
		// unblocked round an event woke, against the iteration's progress.
		fenceMark := c.markRound()
		fenceMark.progress = progress
		fenceStart := time.Now()
		fence, _, deferred, err := c.fence()
		// The fence's full scan, its census included.
		fenceDuration := time.Since(fenceStart)
		if err != nil {
			return err
		}
		if !fence.Settled {
			return errContractJobKeyMoved
		}
		if c.hooks != nil && c.hooks.fenced != nil {
			c.hooks.fenced(iteration, deferred)
		}
		pending = make(map[string]string)
		// After staleUnits every live file is pending or carries a unit proven
		// at its very stamp at the fence: a move that kept the file's contract
		// content carries its unit to the new stamp, and any other later stamp
		// makes it stale (TestContractStaleUnitsLeavesEveryUnpendingFileAtItsStamp).
		// Nothing pending is therefore the publish condition for the units.
		c.staleUnits(pending)
		c.coverage.refresh(c, pending)
		if err := c.coverage.validate(c); err != nil {
			return err
		}
		current := c.enrichmentCurrent()
		// What one more iteration needs of a writer's quiet gap: a re-ask's
		// lead and span, the enrichment re-check and the fence.
		leadP90, spanP90 := contractQuantile(c.leads, 0.9), contractQuantile(c.spans, 0.9)
		need := leadP90 + spanP90 + ppDuration + fenceDuration
		if iteration >= 1 {
			c.report.NeedSum += need
			c.report.NeedIterations++
		}
		c.logger.Info("contract analysis iteration",
			zap.String("repo", c.snap.Key.RepoPrefix), zap.Int("iteration", iteration),
			zap.Int("stale", len(pending)), zap.Bool("pp_ran", ran), zap.Int("pp_rechecks", c.report.EnrichRechecks-phase.EnrichRechecks),
			zap.Bool("pp_current", current), zap.Int("deferred", len(deferred)), zap.Int("revalidated", c.report.Revalidated-phase.Revalidated),
			zap.Int("content_kept", c.report.ContentKept-phase.ContentKept), zap.Int("progress", c.progress-progress), zap.Int("idle", c.idle),
			zap.Duration("elapsed", time.Since(iterationStart)),
			zap.Duration("census", c.report.CensusDuration-phase.CensusDuration),
			zap.Duration("stamps", c.report.StampsDuration-phase.StampsDuration),
			zap.Duration("scratch", c.report.ScratchDuration-phase.ScratchDuration),
			zap.Duration("extract", c.report.ExtractDuration-phase.ExtractDuration),
			zap.Duration("rounds", c.report.RoundsDuration-phase.RoundsDuration),
			zap.Duration("pp", ppDuration),
			zap.Duration("w_lead_p90", leadP90), zap.Int("w_leads", len(c.leads)),
			zap.Duration("w_span_p90", spanP90), zap.Int("w_spans", len(c.spans)),
			zap.Duration("w_pp", ppDuration), zap.Duration("w_fence", fenceDuration), zap.Duration("w_need", need))
		if len(pending) == 0 && current && c.coverage.seal(c) {
			complete = true
			return nil
		}
		// An iteration that gets here made progress, so this only resets the
		// count; the rounds that count check it. A re-check that succeeded
		// returned without accounting its round: the waits it counted before
		// do not join the next iteration's.
		c.endRound(fenceMark, false)
	}
}

// noteWindowGaps records the proof interval and every primary path that
// blocked after initial extraction, with bounded close and gap summaries.
func (c *contractConvergence) noteWindowGaps() {
	end := time.Now()
	paths := slices.Sorted(maps.Keys(c.blocked))
	var closes []store_sqlite.ContractCoreWindowClose
	missing := paths
	if c.snap.WindowLastCloses != nil {
		closes, missing = c.snap.WindowLastCloses(paths, end)
	}
	if !c.blockedSince.IsZero() {
		c.logger.Info("contract analysis window scope", zap.String("repo", c.snap.Key.RepoPrefix), zap.String("checkout", ""), zap.String("actor_checkout", c.snap.Key.CheckoutID), zap.Any("last_closes", closes), zap.Strings("last_close_missing", missing), zap.Strings("repos", slices.Sorted(maps.Keys(c.repos))), zap.Int64("postextract_since_ns", c.blockedSince.UnixNano()), zap.Int64("measurement_end_ns", end.UnixNano()), zap.Strings("blocked_paths", slices.Sorted(maps.Keys(c.blocked))))
	}
	if c.snap.WindowGaps == nil || len(c.blocked) == 0 {
		return
	}
	c.report.WindowGaps = c.snap.WindowGaps(slices.Sorted(maps.Keys(c.blocked)), c.blockedSince)
}

// enrichmentHolds reports whether the last enrichment round still describes
// the live units: it ran, no re-check found one of its recorded inputs
// changed, and every live unit's records are byte-identical to the ones it
// consumed. A unit re-extracted with unchanged records does not rerun it.
func (c *contractConvergence) enrichmentHolds() bool {
	if c.pp == nil || c.pp.stale || c.registry == nil {
		return false
	}
	return maps.Equal(c.unitRaws(), c.pp.raws) && c.routerProofsHold(c.pp.routers)
}

// unitRaws is the record digest of every live unit the enrichment round
// consumes.
func (c *contractConvergence) unitRaws() map[string]string {
	out := make(map[string]string, len(c.units))
	for path, unit := range c.units {
		if unit.excluded || !c.live[path] {
			continue
		}
		out[path] = unit.rawDigest
	}
	return out
}

// enrichmentNeedsRecheck reports whether a recorded input of the enrichment
// round may have changed since it was last shown current: a file's contract
// content moved, a file it read bytes of no longer has those bytes, or it
// read a file that was not quiescent.
func (c *contractConvergence) enrichmentNeedsRecheck() bool {
	if c.pp == nil || c.pp.stale || c.registry == nil {
		return false
	}
	if c.maxContent > c.pp.round || len(c.pp.torn) > 0 {
		return true
	}
	for path := range c.pp.ledger.sources {
		if c.bytesMoved(c.pp.ledger, path, "") {
			return true
		}
	}
	return false
}

// enrichmentCurrent reports whether the enrichment round is current for the
// last observation of every input.
func (c *contractConvergence) enrichmentCurrent() bool {
	return c.pp != nil && !c.pp.stale && !c.enrichmentNeedsRecheck()
}

// recheckEnrichment re-validates the enrichment round's recorded inputs
// instead of rerunning it, in a point round of its own (revalidate with no
// suspect): every byte read's receipt must still name the bytes read, and
// every recorded read a move since the round (or a torn read of it) could
// have changed must answer again with its recorded digest; the round reads
// nothing else. A bracket with a window open or moved is asked again once a
// window it waits on closes, a round each, blocked rounds never counted; the
// job ends once contractIdleRounds counted rounds made no progress.
func (c *contractConvergence) recheckEnrichment() error {
	waitedOut := false
	for c.enrichmentNeedsRecheck() {
		epoch := c.wake()
		mark := c.markRound()
		if _, _, _, err := c.revalidate(nil); err != nil {
			return err
		}
		if !c.enrichmentNeedsRecheck() {
			return nil
		}
		c.endRound(mark, waitedOut)
		if err := c.idleErr(); err != nil {
			return fmt.Errorf("%w: enrichment re-check", err)
		}
		waitedOut = !c.waitWake(epoch, time.Now().Add(c.roundWait()))
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// enrich runs the enrichment round: seeds re-derived from the selected core,
// every postpass over fresh clones of the units' records, on a fresh store,
// every core read and byte read recorded through the ledger facade. A read it
// cannot prove is retried on the next wake, a round each.
func (c *contractConvergence) enrich() error {
	waitedOut := false
	for {
		epoch := c.wake()
		passStart := time.Now()
		mark := c.markRound()
		ok, err := c.enrichOnce()
		// The job's pp phase is every enrichment pass of every iteration.
		c.report.EnrichDuration += time.Since(passStart)
		if err != nil || ok {
			if ok {
				c.progress++
				c.endRound(mark, false)
			}
			return err
		}
		// The pass could not complete a read while something moved.
		c.endRound(mark, waitedOut)
		if err := c.idleErr(); err != nil {
			return fmt.Errorf("%w: enrichment reads", err)
		}
		waitedOut = !c.waitWake(epoch, time.Now().Add(c.roundWait()))
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
}

func (c *contractConvergence) enrichOnce() (bool, error) {
	r0, clock, err := c.fullScan()
	if err != nil {
		return false, err
	}
	if !r0.Settled {
		return false, errContractJobKeyMoved
	}
	c.enrichReads = nil
	ok, err := c.enrichFrom(r0, clock)
	if err == nil && !ok {
		// A file whose bytes the round read moved past its stamp.
		c.blockMoved()
	}
	if err != nil && c.ctx.Err() == nil {
		// A read the round could not complete is a move to retry when a
		// file the round read moved since its R0 (a seed or load torn by an
		// apply); an error no read of a moving file explains is the job's.
		read := map[string]bool{}
		if c.enrichReads != nil {
			read = c.enrichmentReadFiles(c.enrichReads)
		}
		torn, scanErr := c.tornSince(r0, read)
		if scanErr != nil {
			return false, scanErr
		}
		if torn {
			return false, nil
		}
	}
	return ok, err
}

// tornSince reports whether anything an enrichment round read moved since its
// R0: the key or the live census moved, or a file it read was not quiescent at
// R0 or has another stamp now. Each torn file is a refused read the retry
// waits on. A file the round never read cannot have torn it.
func (c *contractConvergence) tornSince(r0 ContractFollowupScan, read map[string]bool) (bool, error) {
	now, err := c.scan(nil)
	if err != nil {
		return false, err
	}
	if !now.Settled || !maps.Equal(now.Census, r0.Census) {
		c.blockMoved()
		return true, nil
	}
	torn := false
	for _, path := range slices.Sorted(maps.Keys(read)) {
		before, had := r0.Stamps[path]
		after, has := now.Stamps[path]
		if had != has || (had && (!before.quiescent() || after != before)) {
			torn = true
			if has {
				c.block(path, after)
			} else {
				c.blockMoved()
			}
		}
	}
	return torn, nil
}

func (c *contractConvergence) enrichFrom(r0 ContractFollowupScan, clock uint64) (bool, error) {
	c.quietFrom = 0
	c.closeEnrichment()
	evidence, scratch, dir, err := c.openEvidence()
	if err != nil {
		return false, err
	}
	c.evidence, c.scratch, c.scratchDir = evidence, scratch, dir
	// Every read of the round goes through the ledger facade: the round is a
	// function of what this ledger records and of the units' records.
	ledger := newContractUnitLedger()
	evidence.ledger = ledger
	c.enrichReads = ledger
	c.guard.open = true
	defer func() { c.guard.open = false }()
	reader := &contractPinnedReader{c: c, stamps: r0.Stamps, ledger: func() *contractUnitLedger { return ledger }, moved: make(map[string]bool)}
	indexers := c.indexers(evidence, reader.read)
	raws := c.unitRaws()
	if err := c.seed(evidence); err != nil {
		return false, err
	}
	paths := make([]string, 0, len(c.units))
	for path := range c.units {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	registry := contracts.NewRegistry()
	retainedRecordBytes, retainedRecords := 0, 0
	for _, path := range paths {
		unit := c.units[path]
		if unit.excluded || !c.live[path] {
			continue
		}
		records := cloneContractRecords(unit.raw)
		file := c.files[path]
		registry.AddAllScoped(records, file.RepoPrefix, file.WorkspaceID, file.ProjectID)
		encoded, err := json.Marshal(records)
		if err != nil {
			return false, err
		}
		retainedRecordBytes += len(encoded)
		retainedRecords += len(records)
		if retainedRecordBytes > contractFollowupCompactLimit || retainedRecords > graph.ContractProjectionRowLimit {
			return false, graph.ErrContractProjectionLimit
		}
	}
	var routerProofs []contractRouterProof
	prefixes := make([]string, 0, len(indexers))
	for repo := range indexers {
		prefixes = append(prefixes, repo)
	}
	sort.Strings(prefixes)
	for _, repo := range prefixes {
		if err := c.req.Yield(c.ctx); err != nil {
			return false, err
		}
		idx := indexers[repo]
		local := contracts.NewRegistry()
		local.AddAll(registry.ByRepo(repo), repo)
		idx.extractDIContracts(local)
		idx.resolveProviderHandlers(local)
		idx.upgradeContractBareTypeRefs(local)
		if contractRouterEligible(local) {
			before := cloneContractRecords(local.All())
			contracts.JoinRouterPrefixFacts(local, c.routerFacts(repo, local))
			digest, ok := contractAnswerDigest(local.All())
			if !ok {
				return false, fmt.Errorf("contract followup: unencodable router result")
			}
			routerProofs = append(routerProofs, contractRouterProof{repo: repo, records: before, digest: digest})
		}
		contracts.BindSpringConfig(evidence, contracts.SpringConfigScope{RepoPrefix: repo, RepoRoot: idx.rootPath, WorkspaceID: idx.workspaceID, ReadSource: func(path string) ([]byte, error) {
			data, err := reader.read(path)
			if err != nil {
				idx.rememberContractInputError(err)
			}
			return data, err
		}})
		idx.resolveCallReturnTypes(local)
		idx.snapshotContractShapes(local)
		idx.inlineEnvelopeShapes(local)
		if len(reader.moved) > 0 {
			return false, nil
		}
		if err := idx.contractInputError(); err != nil {
			return false, err
		}
		if evidence.err != nil {
			return false, evidence.err
		}
		for _, id := range registry.AllIDs() {
			registry.ReplaceByID(id, contractFollowupOtherOwners(registry.ByID(id), repo))
		}
		registry.AddAll(local.All(), repo)
	}
	allRecords := registry.All()
	encodedRecords, err := json.Marshal(allRecords)
	if err != nil {
		return false, err
	}
	if len(encodedRecords) > contractFollowupCompactLimit || len(allRecords) > graph.ContractProjectionRowLimit {
		return false, graph.ErrContractProjectionLimit
	}
	// Import every record owner and every derived output endpoint of a live
	// file now, inside the round, so the payload built after the fence reads
	// this round's evidence, not later core rows. A record whose owner the
	// core did not answer was read across an evict: the round is a read it
	// could not complete, retried while anything moved, never certified.
	if _, _, missing := contractGraphRows(evidence, allRecords, true); missing != 0 {
		return false, fmt.Errorf("contract followup: %d missing selected source owners", missing)
	}
	if err := c.importOutputEndpoints(evidence); err != nil {
		return false, err
	}
	if len(reader.moved) > 0 {
		return false, nil
	}
	if evidence.err != nil {
		return false, evidence.err
	}
	evidence.ledger = nil
	// R1, right after the round's last read: a file the round read that was
	// not quiescent at R0, or that moved since, may have torn the round's
	// reads of it, which are asked again before the round is current. A file
	// it never read cannot have torn it, whatever its writes did.
	c.guard.open = false
	r1, _, err := c.fullScan()
	if err != nil {
		return false, err
	}
	if !r1.Settled {
		return false, errContractJobKeyMoved
	}
	read := c.enrichmentReadFiles(ledger)
	torn := make(map[string]ContractReceiptStamp)
	for path, stamp := range r0.Stamps {
		if read[path] && (!stamp.quiescent() || r1.Stamps[path] != stamp) {
			torn[path] = r1.Stamps[path]
		}
	}
	for path, stamp := range r1.Stamps {
		if _, ok := r0.Stamps[path]; !ok && read[path] {
			torn[path] = stamp
		}
	}
	c.report.EnrichRuns++
	if c.hooks != nil && c.hooks.afterPP != nil {
		c.hooks.afterPP()
	}
	c.registry = registry
	c.pp = &contractEnrichment{round: clock, raws: raws, ledger: ledger, torn: torn, routers: routerProofs}
	return true, nil
}

// enrichmentReadFiles lists the files an enrichment round's reads can depend
// on: the owners of everything its ledger recorded and loaded, the files it
// read bytes of, the files whose units declare a key it looked up (a lookup
// that missed a row torn away names no owner), and, when it read a seed
// query, every file whose unit carries seed rows.
func (c *contractConvergence) enrichmentReadFiles(ledger *contractUnitLedger) map[string]bool {
	out := maps.Clone(ledger.owners)
	if out == nil {
		out = make(map[string]bool)
	}
	for path := range ledger.sources {
		out[path] = true
	}
	seeds := false
	for _, read := range ledger.reads {
		for _, owner := range read.owners {
			out[owner] = true
		}
		seeds = seeds || read.kind == contractReadSeeds
	}
	for key := range ledger.keys {
		for _, path := range c.declarers.lookup(key, c.units) {
			out[path] = true
		}
	}
	if seeds {
		for path, unit := range c.units {
			if len(unit.seedNodes) > 0 || len(unit.seedEdges) > 0 {
				out[path] = true
			}
		}
	}
	return out
}

// seed loads the round's enrichment seeds into its evidence: from the
// selected core's seed queries, by loading every live file of a repository
// whose chain has no physical seed query, or, for a frozen test cohort, from
// the units' own loads.
func (c *contractConvergence) seed(evidence *contractFollowupEvidence) error {
	repos := make([]string, 0, len(c.repos))
	for repo := range c.repos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if c.snap.Seeds == nil && c.req.delta != nil {
			for _, path := range c.livePathsOf(repo) {
				if _, _, err := evidence.loadFile(path); err != nil {
					return err
				}
			}
			continue
		}
		if c.snap.Seeds == nil {
			for _, path := range c.livePathsOf(repo) {
				unit := c.units[path]
				if unit == nil || unit.excluded {
					continue
				}
				evidence.AddBatch(unit.seedNodes, unit.seedEdges)
			}
			if evidence.err != nil {
				return evidence.err
			}
			continue
		}
		nodes, edges, supported, err := c.seedRows(evidence, repo)
		if err != nil {
			return err
		}
		if !supported {
			// The live file set is itself a read: a join or departure reruns
			// the loads.
			paths := c.livePathsOf(repo)
			evidence.ledger.read(contractReadLivePaths, []string{repo}, paths)
			for _, path := range paths {
				if _, _, err := evidence.loadFile(path); err != nil {
					return err
				}
			}
			continue
		}
		evidence.ledger.read(contractReadSeeds, []string{repo}, contractFileAnswer{Nodes: nodes, Edges: edges})
		// The edges' sources land before the edges, for the DI pass's lookups.
		evidence.AddBatch(nodes, nil)
		evidence.AddBatch(nil, edges)
		if evidence.err != nil {
			return evidence.err
		}
	}
	return nil
}

// seedRows is one repository's enrichment seed read: the selected core's seed
// nodes of live files and its dependency-injection edges whose source a live
// file owns, with those sources, detached. It is a function of the core and
// the live census, asked again by a re-check. supported=false means the chain
// has no physical seed query.
func (c *contractConvergence) seedRows(evidence *contractFollowupEvidence, repo string) ([]*graph.Node, []*graph.Edge, bool, error) {
	defer c.guard.enter()()
	nodes, edges, supported, err := c.snap.Seeds(c.ctx, repo)
	if err != nil || !supported {
		return nil, nil, supported, err
	}
	var seedNodes []*graph.Node
	seen := make(map[string]bool)
	for _, node := range nodes {
		if node == nil || !c.live[node.FilePath] {
			continue
		}
		detached := evidence.detach(node)
		if detached == nil {
			return nil, nil, true, evidence.err
		}
		seen[detached.ID] = true
		seedNodes = append(seedNodes, detached)
	}
	var sources []string
	var seedEdges []*graph.Edge
	for _, edge := range edges {
		if edge == nil || edge.From == "" || edge.To == "" {
			continue
		}
		if _, di := diContractFromEdge(edge); !di {
			continue
		}
		copyEdge := *edge
		if edge.Meta != nil {
			copyEdge.Meta = contractFollowupCloneValue(reflect.ValueOf(edge.Meta)).Interface().(map[string]any)
		}
		seedEdges = append(seedEdges, &copyEdge)
		sources = append(sources, edge.From)
	}
	if len(seedEdges) == 0 {
		return seedNodes, nil, true, nil
	}
	found, err := graph.ContractSourceNodesContext(c.ctx, c.snap.Core, appendUniqueSorted(nil, sources...))
	if err != nil {
		return nil, nil, true, err
	}
	live := seedEdges[:0]
	for _, edge := range seedEdges {
		source := found[edge.From]
		if source == nil || !c.live[source.FilePath] {
			continue
		}
		live = append(live, edge)
		if !seen[source.ID] && source.Kind != graph.KindContract && source.Kind != graph.KindContractBridge {
			detached := evidence.detach(source)
			if detached == nil {
				return nil, nil, true, evidence.err
			}
			seen[source.ID] = true
			seedNodes = append(seedNodes, detached)
		}
	}
	return seedNodes, live, true, nil
}

// importOutputEndpoints loads the endpoints of derived output edges (Spring
// bean calls, config reads) that the enrichment round wrote but did not load:
// a whole-cohort load had every live file's nodes in its evidence.
func (c *contractConvergence) importOutputEndpoints(evidence *contractFollowupEvidence) error {
	ids := make([]string, 0, len(evidence.outputIDs))
	for id := range evidence.outputIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += contractFrontierReadBatchSize {
		chunk := ids[start:min(start+contractFrontierReadBatchSize, len(ids))]
		present, err := evidence.scratch.GetNodesByIDsContext(c.ctx, chunk)
		if err != nil {
			return err
		}
		var missing []string
		for _, id := range chunk {
			if present[id] == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		load, err := c.endpointRows(evidence, missing)
		if err != nil {
			return err
		}
		evidence.ledger.read(contractReadEndpoints, missing, load)
		evidence.AddBatch(load, nil)
		if evidence.err != nil {
			return evidence.err
		}
	}
	return nil
}

// endpointRows reads output endpoint nodes by id from the selected core,
// keeping those a live file owns, detached: a recorded read of the
// enrichment round.
func (c *contractConvergence) endpointRows(evidence *contractFollowupEvidence, ids []string) ([]*graph.Node, error) {
	defer c.guard.enter()()
	found, err := graph.ContractSourceNodesContext(c.ctx, c.snap.Core, ids)
	if err != nil {
		return nil, err
	}
	var load []*graph.Node
	for _, id := range ids {
		if node := found[id]; node != nil && c.live[node.FilePath] && node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge {
			if detached := evidence.detach(node); detached != nil {
				load = append(load, detached)
			}
		}
	}
	return load, evidence.err
}

func (c *contractConvergence) livePathsOf(repo string) []string {
	var out []string
	for path, file := range c.files {
		if file.RepoPrefix == repo && c.live[path] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func (c *contractConvergence) closeEnrichment() {
	closeContractEvidence(c.scratch, c.scratchDir)
	c.evidence, c.scratch, c.scratchDir, c.registry = nil, nil, "", nil
}

// contractStuckSample is one path's refused stamp and since when.
type contractStuckSample struct {
	stamp    ContractReceiptStamp
	since    time.Time
	reported bool
}

// noteStuck tracks a read refused on path's open core-write window. A window
// no core write moved for a repair period was left by a mutation that will
// not close it: it is reported once, and the repair checks it again under
// the repository's lane.
func (c *contractConvergence) noteStuck(path string, stamp ContractReceiptStamp) {
	if c.snap.Suspect == nil {
		return
	}
	now := time.Now()
	seen, ok := c.stuck[path]
	if !ok || seen.stamp != stamp {
		if c.stuck == nil {
			c.stuck = make(map[string]contractStuckSample)
		}
		c.stuck[path] = contractStuckSample{stamp: stamp, since: now}
		return
	}
	if !seen.reported && now.Sub(seen.since) >= ContractRepairPeriod {
		seen.reported = true
		c.stuck[path] = seen
		c.snap.Suspect([]string{path})
	}
}

// measureWindowWake records timing evidence without changing wake admission.
func (c *contractConvergence) measureWindowWake(windows map[string]ContractReceiptStamp, missing string) {
	c.wakeSource = missing
	if len(windows) == 0 || c.snap.ReceiptWindowClose == nil {
		return
	}
	if at, ok := c.snap.ReceiptWindowClose(windows); ok {
		c.wokeAt, c.wakeSource = at, "window_close"
	}
}
