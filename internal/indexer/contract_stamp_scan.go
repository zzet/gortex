package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// ContractReceiptStamp is the winning receipt row of one file in a job's
// selected chain, without its payload. Two stamps of a file are equal only
// when the same row (generation and actor) carries the same identity, the
// same acceptance and the same process-local write seq: a same-identity
// rewrite or an evict/add pair between two scans always shows. Settled marks
// an unaccepted row whose mutation completed its core writes without being
// accepted (a newer mutation took its owner over). Writing marks an open
// core-write window of the path: a mutation staged its receipt and has not
// ended its core writes.
type ContractReceiptStamp struct {
	Generation                     int64
	Actor                          string
	Fingerprint, SourceFingerprint string
	Accepted                       bool
	Seq                            uint64
	Settled                        bool
	Writing                        bool
}

// quiescent reports that no core write of the file's rows is in flight: no
// core-write window of the path is open, and the row was accepted, settled,
// or staged in this process by a mutation whose body has since ended its core
// writes. Acceptance is not needed: an apply whose receipt is still in flight
// after its body ended has complete rows. Until the next receipt write of the
// path, which bumps its seq and opens a window, the file's core rows are
// complete. An unaccepted row this process never wrote (seq 0) is never
// quiescent: nothing here saw its mutation end.
func (s ContractReceiptStamp) quiescent() bool {
	return !s.Writing && (s.Accepted || s.Settled || s.Seq > 0)
}

// contractReceiptRowsComplete reports, for an unaccepted receipt row, whether
// its file's core rows are complete by the rule a job proves its reads by: the
// capture admits such a row exactly when the job's stamp of it is quiescent.
func contractReceiptRowsComplete(state store_sqlite.ContractReceiptWriteState) bool {
	return ContractReceiptStamp{Seq: state.Seq, Settled: state.Settled, Writing: state.Writing}.quiescent()
}

// contractReceiptIdentity is the content half of a stamp: what extraction of
// the file depends on.
type contractReceiptIdentity struct{ fingerprint, source string }

func (s ContractReceiptStamp) identity() contractReceiptIdentity {
	return contractReceiptIdentity{s.Fingerprint, s.SourceFingerprint}
}

// ContractFollowupScan is one stamp scan of a job's selected chain.
type ContractFollowupScan struct {
	// Stamps holds the winning row of every scanned path.
	Stamps map[string]ContractReceiptStamp
	// Settled reports that every captured input-state witness still holds:
	// the attachment key the job builds has not moved.
	Settled bool
	// Full scans cover every row and carry the live census path set.
	Full   bool
	Census map[string]bool
	// Phase durations of this scan.
	CensusDuration, StampsDuration time.Duration
}

// contractFailureEnvelopeFingerprint is the fingerprint every failure
// envelope of path carries; a stamp equal to it is an excluded file.
func contractFailureEnvelopeFingerprint(path string) string {
	encoded, err := json.Marshal(struct {
		Version, FilePath, Uncertainty string
		Deleted                        bool
	}{contractBoundaryReceiptVersion, path, contractFileIndexFailed, false})
	if err != nil {
		return ""
	}
	return contractInputHash(encoded)
}

func (s ContractReceiptStamp) envelope(path string) bool {
	return s.Fingerprint == contractFailureEnvelopeFingerprint(path)
}

// contractScanLayer is one physical generation of a repository's selected
// chain and the actors whose rows it selects.
type contractScanLayer struct {
	handle *store_sqlite.Store
	actors []string
}

// claims adds to hidden every path of repo (or of wanted, for a point scan)
// this layer claims by a replace or delete mask. The base corpus claims
// nothing.
func (l contractScanLayer) claims(ctx context.Context, repo string, wanted []string, full bool, hidden map[string]bool) error {
	if l.handle.ViewGeneration() <= 0 {
		return nil
	}
	claimed := func(mode store_sqlite.OwnershipMode) bool {
		return mode == store_sqlite.OwnershipReplace || mode == store_sqlite.OwnershipDelete
	}
	if !full {
		masks, err := l.handle.FileMasksForContext(ctx, repo, wanted)
		if err != nil {
			return err
		}
		for _, mask := range masks {
			if claimed(mask.Mode) {
				hidden[mask.FilePath] = true
			}
		}
		return ctx.Err()
	}
	masks, err := l.handle.FileMasksContext(ctx)
	if err != nil {
		return err
	}
	for _, mask := range masks {
		if mask.RepoPrefix == repo && claimed(mask.Mode) {
			hidden[mask.FilePath] = true
		}
	}
	return nil
}

// contractRepoScan is the reusable part of a repository's last full scan.
type contractRepoScan struct {
	chain       string
	begun, done uint64
	stamps      map[string]ContractReceiptStamp
}

// contractStampScanner scans the receipt rows of one job's captured chain.
// It lives for one job: rows are reused only between scans of that job, so a
// chain change between jobs (a published worktree generation, a fold) can
// never hide behind a reused scan.
type contractStampScanner struct {
	repos   []string
	chains  map[string][]contractScanLayer // top-down per repository
	inputs  *graphview.SelectedContractInputs
	census  func(context.Context) (map[string]bool, error)
	mu      sync.Mutex
	last    map[string]contractRepoScan
	reused  int
	scanned int
	// phase, when set by a test, runs after each scan phase: "sample",
	// "rows", "seqs" and "inputs".
	phase func(string)
}

func (s *contractStampScanner) at(phase string) {
	if s.phase != nil {
		s.phase(phase)
	}
}

func newContractStampScanner(inputs *graphview.SelectedContractInputs, handlesByRepo map[string][]*store_sqlite.Store, census func(context.Context) (map[string]bool, error)) *contractStampScanner {
	s := &contractStampScanner{inputs: inputs, census: census, chains: make(map[string][]contractScanLayer), last: make(map[string]contractRepoScan)}
	for repo, handles := range handlesByRepo {
		s.repos = append(s.repos, repo)
		var chain []contractScanLayer
		for i := len(handles) - 1; i >= 0; i-- {
			layer := contractScanLayer{handle: handles[i]}
			for _, w := range inputs.Witnesses {
				if w.GenerationID == handles[i].ViewGeneration() && w.State.RepoPrefix == repo {
					layer.actors = append(layer.actors, w.State.CheckoutID)
				}
			}
			chain = append(chain, layer)
		}
		s.chains[repo] = chain
	}
	sort.Strings(s.repos)
	return s
}

// chainKey names a repository's selected chain: its ordered generations,
// their actors and the repository. A reused scan must have been taken over
// the same chain.
func (s *contractStampScanner) chainKey(repo string) string {
	var b strings.Builder
	b.WriteString(repo)
	for _, layer := range s.chains[repo] {
		fmt.Fprintf(&b, "|%d:%s", layer.handle.ViewGeneration(), strings.Join(layer.actors, ","))
	}
	return b.String()
}

func contractPathInRepo(path, repo string, repos []string) bool {
	if repo != "" {
		return strings.HasPrefix(path, repo+"/")
	}
	for _, other := range repos {
		if other != "" && strings.HasPrefix(path, other+"/") {
			return false
		}
	}
	return true
}

// scan takes a full scan (paths == nil) or a point scan of the named paths,
// in a fixed order: (1) each repository's write bracket, (2) rows, (3) write
// seqs, (4) the input-state witnesses. Reading rows before seqs, with every
// write bumping its seq before it executes, makes a write that overlaps the
// interval between two scans show as a row or seq difference.
//
// A full scan reuses a repository's rows and seqs from the previous full scan
// when that scan saw no receipt write in flight (begun == done) and no write
// has begun since (the bracket is unchanged): it then read every committed
// row, and none was written after. The witnesses are always re-read.
func (s *contractStampScanner) scan(ctx context.Context, paths []string) (ContractFollowupScan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	full := paths == nil
	out := ContractFollowupScan{Stamps: make(map[string]ContractReceiptStamp), Full: full}
	stampsStart := time.Now()
	type bracket struct{ begun, done uint64 }
	samples := make(map[string]bracket, len(s.repos))
	for _, repo := range s.repos {
		chain := s.chains[repo]
		if len(chain) == 0 {
			continue
		}
		begun, done := chain[0].handle.ContractReceiptWriteSample(repo)
		samples[repo] = bracket{begun, done}
	}
	s.at("sample")
	type winner struct {
		stamp store_sqlite.ContractBoundaryReceiptStamp
		gen   int64
		actor string
	}
	reusedRepos := make(map[string]bool)
	winners := make(map[string]winner)
	for _, repo := range s.repos {
		if full {
			if last, ok := s.last[repo]; ok && last.chain == s.chainKey(repo) && last.begun == last.done &&
				samples[repo] == (bracket{last.begun, last.done}) {
				for path, stamp := range last.stamps {
					out.Stamps[path] = stamp
				}
				reusedRepos[repo] = true
				s.reused++
				continue
			}
		}
		var wanted []string
		if !full {
			for _, path := range paths {
				if contractPathInRepo(path, repo, s.repos) {
					wanted = append(wanted, path)
				}
			}
			if len(wanted) == 0 {
				continue
			}
		}
		s.scanned++
		// A path an upper generation claims hides every row below it, as the
		// selected core does, whether or not that generation has a row.
		hidden := make(map[string]bool)
		for _, layer := range s.chains[repo] {
			for _, actor := range layer.actors {
				rows, err := layer.handle.ContractBoundaryReceiptStampsContext(ctx, repo, actor, wanted)
				if err != nil {
					return ContractFollowupScan{}, err
				}
				for path, row := range rows {
					if _, won := winners[path]; !won && !hidden[path] {
						winners[path] = winner{stamp: row, gen: layer.handle.ViewGeneration(), actor: actor}
					}
				}
			}
			if err := layer.claims(ctx, repo, wanted, full, hidden); err != nil {
				return ContractFollowupScan{}, err
			}
		}
	}
	s.at("rows")
	// Seqs after rows, grouped by the winning row's own (repository, actor).
	type group struct {
		repo, actor string
		gen         int64
	}
	byGroup := make(map[group][]string)
	for path, w := range winners {
		for _, repo := range s.repos {
			if contractPathInRepo(path, repo, s.repos) {
				byGroup[group{repo, w.actor, w.gen}] = append(byGroup[group{repo, w.actor, w.gen}], path)
				break
			}
		}
	}
	for g, groupPaths := range byGroup {
		chain := s.chains[g.repo]
		states := chain[0].handle.ContractReceiptWriteStates(g.repo, g.actor, groupPaths)
		for _, path := range groupPaths {
			w := winners[path]
			state := states[path]
			out.Stamps[path] = ContractReceiptStamp{Generation: w.gen, Actor: w.actor, Fingerprint: w.stamp.Fingerprint,
				SourceFingerprint: w.stamp.SourceFingerprint, Accepted: w.stamp.Accepted, Seq: state.Seq, Settled: state.Settled && !w.stamp.Accepted,
				Writing: state.Writing}
		}
	}
	s.at("seqs")
	// The witnesses last: an input-state move needs no receipt write.
	if err := s.inputs.Validate(ctx); err == nil {
		out.Settled = true
	} else if !errors.Is(err, graph.ErrContractProjectionStale) && !errors.Is(err, graph.ErrContractInputVector) {
		return ContractFollowupScan{}, err
	}
	s.at("inputs")
	out.StampsDuration = time.Since(stampsStart)
	if full {
		for _, repo := range s.repos {
			if reusedRepos[repo] {
				continue
			}
			stamps := make(map[string]ContractReceiptStamp)
			for path, stamp := range out.Stamps {
				if contractPathInRepo(path, repo, s.repos) {
					stamps[path] = stamp
				}
			}
			sample := samples[repo]
			s.last[repo] = contractRepoScan{chain: s.chainKey(repo), begun: sample.begun, done: sample.done, stamps: stamps}
		}
		if s.census != nil {
			censusStart := time.Now()
			census, err := s.census(ctx)
			if err != nil {
				return ContractFollowupScan{}, err
			}
			out.Census = census
			out.CensusDuration = time.Since(censusStart)
		}
	}
	return out, ctx.Err()
}

// contents reads the contract content of each stamped path's receipt row and
// of its accepted predecessor, keyed by receipt fingerprint: the receipt with
// its source fingerprint left out, the identity the attachment key itself uses
// for a file. The receipt is the accepted parse's own extraction of the file's
// raw records and of every declaration other files' contract reads can see, so
// two receipts with equal content describe the same contract inputs whatever
// the bytes between them. A row that no longer carries its stamp's identity
// yields nothing for it.
func (s *contractStampScanner) contents(ctx context.Context, stamps map[string]ContractReceiptStamp) (map[string]string, error) {
	rows, err := s.selectedReceiptRows(ctx, stamps)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		for _, receipt := range []*graph.ContractBoundaryReceipt{row, row.Previous} {
			if receipt != nil && !receipt.Deleted && receipt.Fingerprint == contractInputHash(receipt.Payload) {
				out[receipt.Fingerprint] = contractReceiptContent(receipt.Payload, receipt.SourceFingerprint)
			}
		}
	}
	return out, nil
}

func (s *contractStampScanner) selectedReceiptRows(ctx context.Context, stamps map[string]ContractReceiptStamp) (map[string]*graph.ContractBoundaryReceipt, error) {
	type group struct {
		repo  string
		gen   int64
		actor string
	}
	groups := make(map[group][]string)
	for path, stamp := range stamps {
		for _, repo := range s.repos {
			if contractPathInRepo(path, repo, s.repos) {
				key := group{repo, stamp.Generation, stamp.Actor}
				groups[key] = append(groups[key], path)
				break
			}
		}
	}
	out := make(map[string]*graph.ContractBoundaryReceipt, len(stamps))
	for g, paths := range groups {
		var handle *store_sqlite.Store
		for _, layer := range s.chains[g.repo] {
			if layer.handle.ViewGeneration() == g.gen {
				handle = layer.handle
				break
			}
		}
		if handle == nil {
			continue
		}
		sort.Strings(paths)
		for start := 0; start < len(paths); start += 64 {
			rows, err := handle.ContractBoundaryReceiptsForPathsContext(ctx, g.repo, g.actor, paths[start:min(start+64, len(paths))])
			if err != nil {
				return out, err
			}
			for path, row := range rows {
				stamp := stamps[path]
				if row == nil || row.Fingerprint != stamp.Fingerprint || row.SourceFingerprint != stamp.SourceFingerprint {
					continue
				}
				out[path] = row
			}
		}
	}
	return out, ctx.Err()
}

// deltaReceipts reads exact winning payloads selected by stamps, then closes
// their stamp bracket. Missing/torn rows remain unknown local deltas; no work
// row or accepted/latest lookup substitutes for this selected-view evidence.
func (s *contractStampScanner) deltaReceipts(ctx context.Context, stamps map[string]ContractReceiptStamp) (map[string]contractDeltaReceipt, error) {
	out := make(map[string]contractDeltaReceipt)
	rows, err := s.selectedReceiptRows(ctx, stamps)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(stamps))
	for path := range stamps {
		paths = append(paths, path)
	}
	for path, row := range rows {
		stamp := stamps[path]
		if !stamp.quiescent() || row.Deleted || row.Fingerprint != contractInputHash(row.Payload) {
			continue
		}
		if _, overflow := contractReceiptOverflowPolicy(row); overflow {
			continue // A source witness supplies no dependency/no-change facts.
		}
		var receipt contractBoundaryReceipt
		if json.Unmarshal(row.Payload, &receipt) != nil || receipt.Version != contractBoundaryReceiptVersion || receipt.FilePath != path {
			continue
		}
		out[path] = newContractDeltaReceipt(contractReceiptContent(row.Payload, row.SourceFingerprint), receipt.ProducedInputs)
	}
	closing, err := s.scan(ctx, paths)
	if err != nil {
		return nil, err
	}
	for path := range out {
		if closing.Stamps[path] != stamps[path] || !closing.Stamps[path].quiescent() {
			delete(out, path)
		}
	}
	return out, nil
}

// contractReceiptContent digests a stored receipt payload with its source
// fingerprint left out. The payload is the receipt's canonical encoding, so
// blanking the one source field leaves every other byte to compare; a mount
// file's mount inputs carry its source too and keep it.
func contractReceiptContent(payload []byte, source string) string {
	blanked := bytes.Replace(payload, []byte(`"source":"`+source+`"`), []byte(`"source":""`), 1)
	return contractInputHash(append([]byte("contract-content:"), blanked...))
}

// contractChangeClock orders a job's observations of its inputs. Each full
// scan advances the clock and stamps changed every path whose winning row
// differs from the previous full scan (appearance and disappearance count)
// and every path that joined or left the census since then, even when its
// receipt row did not change. It is a pure function of its previous state,
// the scan and the census. seenAt is the clock of the latest scan, full or
// point, that observed a path's current stamp.
type contractChangeClock struct {
	clock      uint64
	last       map[string]ContractReceiptStamp
	lastCensus map[string]bool
	changedAt  map[string]uint64
	seenAt     map[string]uint64
	maxChanged uint64
}

func (c *contractChangeClock) observe(stamps map[string]ContractReceiptStamp, census map[string]bool) uint64 {
	c.clock++
	if c.changedAt == nil {
		c.changedAt = make(map[string]uint64)
	}
	if c.seenAt == nil {
		c.seenAt = make(map[string]uint64)
	}
	mark := func(path string) {
		c.changedAt[path] = c.clock
		c.maxChanged = c.clock
	}
	for path, stamp := range stamps {
		c.seenAt[path] = c.clock
		if prior, ok := c.last[path]; !ok || prior != stamp {
			mark(path)
		}
	}
	for path := range c.last {
		if _, ok := stamps[path]; !ok {
			mark(path)
		}
	}
	for path := range census {
		if !c.lastCensus[path] {
			mark(path)
		}
	}
	for path := range c.lastCensus {
		if !census[path] {
			mark(path)
		}
	}
	c.last = stamps
	c.lastCensus = census
	return c.clock
}

// observePoint advances the clock with a point scan of paths: each scanned
// path whose winning row differs from its last observation, or that lost its
// row, is stamped changed at the new clock. Paths outside the scan keep their
// last observation, and census moves stay full-scan terms. A unit whose reads
// follow a point observation may take its clock as its round: any later write
// to a file it read differs from that file's last observation, so a later
// scan stamps it past the round.
func (c *contractChangeClock) observePoint(paths []string, stamps map[string]ContractReceiptStamp) uint64 {
	c.clock++
	if c.changedAt == nil {
		c.changedAt = make(map[string]uint64)
	}
	if c.seenAt == nil {
		c.seenAt = make(map[string]uint64)
	}
	// A fresh map: earlier scans' stamp maps stay as their holders read them.
	last := make(map[string]ContractReceiptStamp, len(c.last)+len(paths))
	for path, stamp := range c.last {
		last[path] = stamp
	}
	for _, path := range paths {
		prior, had := last[path]
		stamp, has := stamps[path]
		if has {
			c.seenAt[path] = c.clock
		}
		switch {
		case has && (!had || prior != stamp):
			last[path] = stamp
		case !has && had:
			delete(last, path)
		default:
			continue
		}
		c.changedAt[path] = c.clock
		c.maxChanged = c.clock
	}
	c.last = last
	return c.clock
}

// changedSince reports whether path moved after round.
func (c *contractChangeClock) changedSince(path string, round uint64) bool {
	return c.changedAt[path] > round
}
