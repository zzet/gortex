package indexer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// This proof belongs to one captured installation, including completed
// namespace debt retained in immutable ancestry. Paths, full-row
// identities and guard indices are shared across all of its admitted units.
// Unit pointers alias exactly the immutable units in the same checkpoint;
// guard stamps come from that checkpoint's immutable change-clock map.
type contractFillContinuation struct {
	identity contractDeltaIdentity
	// namespaceBaseline certifies completed barrier rows. Copying may prune
	// acknowledged non-root debt; namespace rows must remain exact.
	// Current own/read stamps, row identities and guards remain mandatory.
	namespaceBaseline            bool
	barrierRows, allRows, census [sha256.Size]byte
	namespaceRows                [sha256.Size]byte
	barrierMembers               [][sha256.Size]byte
	paths                        []string
	rows                         [][sha256.Size]byte
	units                        []contractFillUnitProof
	guards                       []uint32
}

type contractFillUnitProof struct {
	unit                 *contractUnit
	guardStart, guardEnd uint32
	flags                uint8
}

const (
	contractFillOwn uint8 = 1 << iota
	contractFillRead
	contractFillNamespace
)

// Index each canonical full row once. A new token, actor, origin, scope or
// input version cannot inherit a different captured obligation.
type contractFillRows struct {
	byPath map[string][][sha256.Size]byte
	all    [sha256.Size]byte
}

func newContractFillRows(work []graph.ContractWork) contractFillRows {
	rows := contractFillRows{byPath: map[string][][sha256.Size]byte{}}
	var all [][sha256.Size]byte
	for _, row := range work {
		identity := sha256.Sum256([]byte(contractWorkIdentity(row)))
		rows.byPath[row.FilePath] = append(rows.byPath[row.FilePath], identity)
		all = append(all, identity)
	}
	rows.all = contractFillDigest(all)
	return rows
}

func contractFillDigest(identities [][sha256.Size]byte) [sha256.Size]byte {
	slices.SortFunc(identities, func(a, b [sha256.Size]byte) int {
		for i := range a {
			if a[i] < b[i] {
				return -1
			}
			if a[i] > b[i] {
				return 1
			}
		}
		return 0
	})
	hash := sha256.New()
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(identities)))
	_, _ = hash.Write(count[:])
	for _, identity := range identities {
		_, _ = hash.Write(identity[:])
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func (rows contractFillRows) own(path string) [sha256.Size]byte {
	return contractFillDigest(slices.Clone(rows.byPath[path]))
}

// Canonical full identities retain actor, origin, scope, token and input key.
// Sorting also preserves multiplicity for conservative subset comparison.
func contractFillBarrierMembers(work []graph.ContractWork) ([][sha256.Size]byte, [sha256.Size]byte) {
	var identities, roots [][sha256.Size]byte
	for _, row := range work {
		if contractDeltaMembershipUnknown([]graph.ContractWork{row}) {
			identity := sha256.Sum256([]byte(contractWorkIdentity(row)))
			identities = append(identities, identity)
			if row.FilePath == row.RepoPrefix+"/" {
				roots = append(roots, identity)
			}
		}
	}
	slices.SortFunc(identities, func(a, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) })
	return identities, contractFillDigest(roots)
}

func contractFillBarrierRows(work []graph.ContractWork) [sha256.Size]byte {
	identities, _ := contractFillBarrierMembers(work)
	return contractFillDigest(identities)
}

func contractFillCensus(census map[string]bool) [sha256.Size]byte {
	var identities [][sha256.Size]byte
	for path, live := range census {
		if live {
			identities = append(identities, sha256.Sum256([]byte(path)))
		}
	}
	return contractFillDigest(identities)
}

func contractFillNamespaceRead(w *contractWorkCoverage, unit *contractUnit) bool {
	if w.broad {
		return true
	}
	if unit.ledger != nil {
		for _, read := range unit.ledger.reads {
			switch read.kind {
			case contractReadRepoFiles, contractReadLivePaths, contractReadSeeds:
				return true
			}
		}
	}
	return false
}

func contractFillRequiredGuards(c *contractConvergence, unit *contractUnit) []string {
	paths := append(c.coverage.bracket(c, unit), unit.path)
	if unit.ledger != nil {
		for source := range unit.ledger.sources {
			paths = append(paths, source)
		}
	}
	return appendUniqueSorted(nil, paths...)
}

// Namespace reads retain the census at their actual admitted query, not a
// later checkpoint's census. A changed census requires guarded replay.
func (c *contractConvergence) noteFillRead(unit *contractUnit) {
	if !contractFillNamespaceRead(c.coverage, unit) {
		return
	}
	if c.fillReadCensus == nil {
		c.fillReadCensus = map[*contractUnit][sha256.Size]byte{}
	}
	if c.fillCensusClock != c.clock.clock || !c.fillCensusKnown {
		c.fillCensusDigest = contractFillCensus(c.clock.lastCensus)
		c.fillCensusClock, c.fillCensusKnown = c.clock.clock, true
	}
	c.fillReadCensus[unit] = c.fillCensusDigest
}

// Only actual admitted own/read pointers can mint this provenance. A later
// clock observation cannot substitute for a read's original guard stamps.
func checkpointContractFill(c *contractConvergence, p *contractDeltaCache, pool *contractProofStrings) *contractFillContinuation {
	if c.coverage == nil || !c.coverage.anchored {
		return nil
	}
	w := c.coverage
	rows := newContractFillRows(w.work)
	proof := &contractFillContinuation{identity: p.identity, barrierRows: contractFillBarrierRows(w.work), allRows: rows.all, census: contractFillCensus(c.clock.lastCensus)}
	proof.barrierMembers, proof.namespaceRows = contractFillBarrierMembers(w.work)
	type candidate struct {
		unit   *contractUnit
		flags  uint8
		guards []string
	}
	var candidates []candidate
	paths := map[string]bool{}
	for path, unit := range c.units {
		if unit == nil || unit.excluded {
			continue
		}
		var flags uint8
		if w.own[path] == unit && c.clock.last[path] == unit.stamp && unit.stamp.quiescent() {
			flags |= contractFillOwn
		}
		read := w.readSatisfied(unit) && len(unit.torn) == 0 && !c.suspect[path]
		namespace := contractFillNamespaceRead(w, unit)
		if namespace {
			at, admitted := c.fillReadCensus[unit]
			read = read && admitted && at == proof.census
		}
		var guards []string
		if read {
			guards = contractFillRequiredGuards(c, unit)
			for _, guard := range guards {
				stamp, found := c.clock.last[guard]
				if !w.guards[path][guard] || (found && !stamp.quiescent()) || c.clock.changedAt[guard] > unit.round {
					read = false
					break
				}
			}
		}
		if read {
			flags |= contractFillRead
			if namespace {
				flags |= contractFillNamespace
			}
		} else {
			guards = nil
		}
		if flags == 0 {
			continue
		}
		paths[path] = true
		for _, guard := range guards {
			paths[guard] = true
		}
		candidates = append(candidates, candidate{unit: unit, flags: flags, guards: guards})
	}
	if len(candidates) == 0 || uint64(len(paths)) > math.MaxUint32 {
		return nil
	}
	for path := range paths {
		proof.paths = append(proof.paths, path)
	}
	slices.Sort(proof.paths)
	proof.paths = cloneContractDeltaStrings(proof.paths, pool)
	proof.rows = make([][sha256.Size]byte, len(proof.paths))
	indices := make(map[string]uint32, len(proof.paths))
	for i, path := range proof.paths {
		indices[path] = uint32(i)
		proof.rows[i] = rows.own(path)
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return strings.Compare(a.unit.path, b.unit.path) })
	proof.units = make([]contractFillUnitProof, 0, len(candidates))
	for _, candidate := range candidates {
		if uint64(len(proof.guards))+uint64(len(candidate.guards)) > math.MaxUint32 {
			return nil
		}
		entry := contractFillUnitProof{unit: p.units[candidate.unit.path], flags: candidate.flags, guardStart: uint32(len(proof.guards))}
		for _, path := range candidate.guards {
			proof.guards = append(proof.guards, indices[path])
		}
		entry.guardEnd = uint32(len(proof.guards))
		proof.units = append(proof.units, entry)
	}
	proof.guards = slices.Clone(proof.guards)
	return proof
}

func (proof *contractFillContinuation) row(path string) [sha256.Size]byte {
	if index, found := slices.BinarySearch(proof.paths, path); found {
		return proof.rows[index]
	}
	return contractFillDigest(nil)
}

// Call before the delta plan or stale-unit handling can mutate transferred
// units. Ordinary cache identity, receipt/census planning and the final fence
// still run after these exact original-obligation proofs are checked.
func (proof *contractFillContinuation) resume(c *contractConvergence, p *contractDeltaCache) {
	if proof == nil || proof.identity != c.req.delta.identity || p.identity != proof.identity {
		return
	}
	w := c.coverage
	// Old guards are conservative dependency hints, never read satisfaction.
	// Keep discovered owners in the new frontier before recomputing its rows.
	for _, entry := range proof.units {
		unit := entry.unit
		if unit == nil || c.units[unit.path] != unit || p.units[unit.path] != unit || !c.live[unit.path] || entry.flags&contractFillRead == 0 {
			continue
		}
		if w.extra[unit.path] == nil {
			w.extra[unit.path] = map[string]bool{}
		}
		for _, index := range proof.guards[entry.guardStart:entry.guardEnd] {
			path := proof.paths[index]
			w.extra[unit.path][path] = true
			if w.paths[path] {
				w.readers[unit.path] = true
			}
		}
	}
	w.observe(c)
	rows := newContractFillRows(w.work)
	census := contractFillCensus(c.clock.lastCensus)
	for _, entry := range proof.units {
		unit := entry.unit
		if unit == nil || c.units[unit.path] != unit || p.units[unit.path] != unit || !c.live[unit.path] {
			continue
		}
		ownCurrent := c.clock.last[unit.path] == unit.stamp && unit.stamp.quiescent()
		if entry.flags&contractFillOwn != 0 && ownCurrent && proof.row(unit.path) == rows.own(unit.path) {
			w.own[unit.path] = unit
		}
		if entry.flags&contractFillRead == 0 || len(unit.torn) != 0 || c.suspect[unit.path] || (w.broad && proof.allRows != rows.all && !proof.completedBarrierCurrent(c.req.delta.identity, w.work)) {
			continue
		}
		if contractFillNamespaceRead(w, unit) && (entry.flags&contractFillNamespace == 0 || proof.census != census) {
			continue
		}
		guardRefs := proof.guards[entry.guardStart:entry.guardEnd]
		required := contractFillRequiredGuards(c, unit)
		valid := true
		for _, path := range required {
			index, found := slices.BinarySearch(proof.paths, path)
			if !found {
				valid = false
				break
			}
			if _, found = slices.BinarySearch(guardRefs, uint32(index)); !found {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		guards := make([]string, 0, len(guardRefs))
		for _, index := range guardRefs {
			path := proof.paths[index]
			before, had := p.clock.last[path]
			after, has := c.clock.last[path]
			if had != has || before != after || (had && !before.quiescent()) || proof.rows[index] != rows.own(path) {
				valid = false
				break
			}
			guards = append(guards, path)
		}
		if valid {
			w.record(unit, guards)
			c.noteFillRead(unit)
		}
	}
}

// The estimator charges pointer slots but not their already charged immutable
// unit referents. All shared vectors and row hashes are still fully charged.
func (proof *contractFillContinuation) metadataSize() int64 {
	if proof == nil {
		return 0
	}
	copyProof := *proof
	copyProof.units = slices.Clone(proof.units)
	for i := range copyProof.units {
		copyProof.units[i].unit = nil
	}
	return contractRetainedSizeMode(reflect.ValueOf(&copyProof), false)
}

func (proof *contractFillContinuation) resumesBarrier(identity contractDeltaIdentity, work []graph.ContractWork) bool {
	if proof == nil || proof.identity != identity {
		return false
	}
	members, roots := contractFillBarrierMembers(work)
	if proof.barrierRows == contractFillDigest(members) {
		return true
	}
	// Only a completed baseline can survive removal of already completed
	// non-root uncertainty. New/changed debt and namespace roots still refuse;
	// initial partial fills retain exact-set semantics. Unit reuse below still
	// checks the actual current own/read/census/source stamps independently.
	if !proof.namespaceBaseline || roots != proof.namespaceRows || len(members) > len(proof.barrierMembers) {
		return false
	}
	previous := 0
	for _, member := range members {
		for previous < len(proof.barrierMembers) && bytes.Compare(proof.barrierMembers[previous][:], member[:]) < 0 {
			previous++
		}
		if previous == len(proof.barrierMembers) || proof.barrierMembers[previous] != member {
			return false
		}
		previous++
	}
	return true
}

// Completed namespace debt remains in immutable ancestry. New local work may
// reuse only individually current proofs. Namespace changes or added/changed
// unknown debt cannot inherit completion; acknowledged non-root removal can.
func (proof *contractFillContinuation) completedBarrierCurrent(identity contractDeltaIdentity, work []graph.ContractWork) bool {
	return proof != nil && proof.namespaceBaseline && proof.resumesBarrier(identity, work)
}
