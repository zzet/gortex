package indexer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// contractCoveredWork exists only after the current job's successful fence.
// Full rows, including scope and origin, are identities, never ordered clocks.
type contractCoveredWork struct {
	sealed         bool
	rowsByIdentity map[string]graph.ContractWork
}

func contractWorkIdentity(work graph.ContractWork) string {
	encoded, _ := json.Marshal(work)
	return string(encoded)
}

func (covered contractCoveredWork) rows(work []graph.ContractWork) ([]graph.ContractWork, error) {
	if !covered.sealed {
		return nil, fmt.Errorf("%w: captured work is not covered", graph.ErrContractProjectionIncomplete)
	}
	rows := make([]graph.ContractWork, 0, len(work))
	for _, row := range work {
		proved, ok := covered.rowsByIdentity[contractWorkIdentity(row)]
		if !ok {
			return nil, fmt.Errorf("%w: captured work is not covered", graph.ErrContractProjectionIncomplete)
		}
		rows = append(rows, proved)
	}
	return rows, nil
}

// This frontier belongs to one capture. Cache units and general unit.round
// cannot satisfy it: only actual extraction or whole-ledger replay admitted
// after the first successful full scan records a proof here.
type contractWorkCoverage struct {
	work                 []graph.ContractWork
	anchored             bool
	paths, keys, readers map[string]bool
	repos                map[string]bool
	own                  map[string]*contractUnit
	read                 map[string]*contractUnit
	extra                map[string]map[string]bool
	broad                bool
	producers            map[string]map[string]bool
	guards               map[string]map[string]bool
	sealed               contractCoveredWork
}

func newContractWorkCoverage(work []graph.ContractWork) *contractWorkCoverage {
	c := &contractWorkCoverage{work: work, producers: map[string]map[string]bool{}, guards: map[string]map[string]bool{}, repos: map[string]bool{}, paths: map[string]bool{}, keys: map[string]bool{}, readers: map[string]bool{}, own: map[string]*contractUnit{}, read: map[string]*contractUnit{}, extra: map[string]map[string]bool{}}
	c.broad = contractDeltaMembershipUnknown(work)
	for _, row := range work {
		c.repos[row.RepoPrefix] = true
		if row.FilePath != "" {
			c.paths[row.FilePath] = true
			c.addKey(row.FilePath, "path:"+row.FilePath)
		}
		for _, key := range row.Scope.LookupKeys {
			c.producedKey(row.FilePath, key)
		}
		for _, id := range row.Scope.SymbolIDs {
			c.addKey(row.FilePath, "id:"+id)
		}
	}
	return c
}

func (w *contractWorkCoverage) remember(unit *contractUnit) {
	if w == nil || unit == nil || !w.paths[unit.path] {
		return
	}
	for _, key := range unit.declared {
		w.addKey(unit.path, key)
	}
}

func (w *contractWorkCoverage) affected(unit *contractUnit) bool {
	if w == nil || unit == nil || unit.excluded {
		return false
	}
	if w.broad || w.paths[unit.path] || w.readers[unit.path] {
		return true
	}
	for owner := range unit.owners {
		if w.paths[owner] {
			return true
		}
	}
	for key := range unit.keys {
		if w.keys[key] {
			return true
		}
	}
	if unit.ledger != nil {
		for source := range unit.ledger.sources {
			if w.paths[source] {
				return true
			}
		}
		// These queries have a repository/census frontier, rather than a key.
		for _, read := range unit.ledger.reads {
			for _, owner := range read.owners {
				if w.paths[owner] {
					return true
				}
			}
			switch read.kind {
			case contractReadRepoFiles, contractReadLivePaths, contractReadSeeds:
				if len(read.args) > 0 && w.repos[read.args[0]] {
					return true
				}
			}
		}
	}
	return false
}

func (w *contractWorkCoverage) needsRead(unit *contractUnit) bool {
	return w != nil && w.anchored && w.affected(unit) && !w.readSatisfied(unit)
}

// Retain old declarations and readers before replacement/removal; current
// units can add negative-query readers throughout the job.
func (w *contractWorkCoverage) observe(c *contractConvergence) {
	for _, unit := range c.units {
		w.remember(unit)
	}
	for path, unit := range c.units {
		if w.affected(unit) {
			w.readers[path] = true
		}
	}
}

func (w *contractWorkCoverage) receiptKeys(receipts map[string]contractDeltaReceipt) {
	for path := range w.paths {
		for _, key := range receipts[path].producedKeys {
			w.producedKey(path, key)
		}
	}
}

func (w *contractWorkCoverage) refresh(c *contractConvergence, pending map[string]string) {
	w.observe(c)
	for path := range w.paths {
		if c.live[path] && (c.units[path] == nil || w.own[path] != c.units[path]) {
			if unit := c.units[path]; unit != nil {
				if c.retryOwners == nil {
					c.retryOwners = map[string]map[string]bool{}
				}
				owners := map[string]bool{}
				for _, owner := range w.bracket(c, unit) {
					owners[owner] = true
				}
				c.retryOwners[path] = owners
			}
			c.dropUnit(path)
			pending[path] = viewmetrics.ContractFilesKeyCheck
		}
	}
	for path := range w.readers {
		unit := c.units[path]
		if c.live[path] && unit != nil && !w.readSatisfied(unit) {
			c.staleUnit(path, viewmetrics.ContractFilesKeyCheck, pending)
		}
	}
}

// Forced reads bracket every debt producer, all known query owners and
// declarers, and owners discovered by a previous refused replay.
func (w *contractWorkCoverage) bracket(c *contractConvergence, unit *contractUnit) []string {
	var paths []string
	paths = append(paths, w.relevant(unit)...)
	for owner := range unit.owners {
		paths = append(paths, owner)
	}
	if unit.ledger != nil {
		for _, read := range unit.ledger.reads {
			paths = append(paths, read.owners...)
		}
	}
	paths = append(paths, c.keyDeclarers(unit)...)
	for owner := range w.extra[unit.path] {
		paths = append(paths, owner)
	}
	return appendUniqueSorted(nil, paths...)
}

func (w *contractWorkCoverage) extracted(unit *contractUnit, bracket ...string) {
	if w == nil || !w.anchored {
		return
	}
	w.own[unit.path] = unit
	w.record(unit, append(bracket, unit.path))
}

// Source bytes and recorded query answers have separate obligations. A torn
// dependency answer can retain a closed extraction, but cannot cover any read.
func (w *contractWorkCoverage) ownExtracted(unit *contractUnit, r0, r1 ContractFollowupScan, sampled ...[]string) {
	if w == nil || !w.anchored || !r1.Settled {
		return
	}
	before, had := r0.Stamps[unit.path]
	after, has := r1.Stamps[unit.path]
	if !had || !has || !before.quiescent() || before != after {
		return
	}
	if len(sampled) > 0 && !slices.Contains(sampled[0], unit.path) {
		return
	}
	w.own[unit.path] = unit
}

func (w *contractWorkCoverage) seal(c *contractConvergence) bool {
	if w.validate(c) != nil {
		return false
	}
	for path := range w.paths {
		if c.live[path] && (c.units[path] == nil || w.own[path] != c.units[path]) {
			return false
		}
	}
	for path := range w.readers {
		if c.live[path] && (c.units[path] == nil || !w.readSatisfied(c.units[path])) {
			return false
		}
	}
	w.sealed = contractCoveredWork{sealed: true, rowsByIdentity: map[string]graph.ContractWork{}}
	for _, row := range w.work {
		w.sealed.rowsByIdentity[contractWorkIdentity(row)] = row
	}
	return true
}

// Absence is part of an exclusive bracket too: a new receipt between R0
// and R1 cannot be ignored merely because R0 had no row for that owner.
func contractCoverageBracketHolds(r0, r1 ContractFollowupScan, paths []string) bool {
	for _, path := range paths {
		before, had := r0.Stamps[path]
		after, has := r1.Stamps[path]
		if had != has || before != after || (had && !before.quiescent()) {
			return false
		}
	}
	return true
}

func (w *contractWorkCoverage) addKey(path, key string) {
	w.keys[key] = true
	if w.producers[key] == nil {
		w.producers[key] = map[string]bool{}
	}
	w.producers[key][path] = true
}

func (w *contractWorkCoverage) producedKey(path, key string) {
	keys := map[string]bool{}
	contractDeltaProducedKeys(keys, key)
	for key := range keys {
		w.addKey(path, key)
	}
}

func (w *contractWorkCoverage) relevant(unit *contractUnit) []string {
	var paths []string
	if w.paths[unit.path] {
		paths = append(paths, unit.path)
	}
	for owner := range unit.owners {
		if w.paths[owner] {
			paths = append(paths, owner)
		}
	}
	for key := range unit.keys {
		for path := range w.producers[key] {
			paths = append(paths, path)
		}
	}
	if unit.ledger != nil {
		for source := range unit.ledger.sources {
			if w.paths[source] {
				paths = append(paths, source)
			}
		}
		for _, read := range unit.ledger.reads {
			for _, owner := range read.owners {
				if w.paths[owner] {
					paths = append(paths, owner)
				}
			}
			switch read.kind {
			case contractReadRepoFiles, contractReadLivePaths, contractReadSeeds:
				if len(read.args) > 0 {
					for _, row := range w.work {
						if row.RepoPrefix == read.args[0] {
							paths = append(paths, row.FilePath)
						}
					}
				}
			}
		}
	}
	return appendUniqueSorted(nil, paths...)
}

func (w *contractWorkCoverage) record(unit *contractUnit, bracket []string) {
	w.read[unit.path] = unit
	w.guards[unit.path] = map[string]bool{}
	for _, path := range bracket {
		w.guards[unit.path][path] = true
	}
}

func (w *contractWorkCoverage) readSatisfied(unit *contractUnit) bool {
	if unit == nil || w.read[unit.path] != unit {
		return false
	}
	for _, path := range w.relevant(unit) {
		if !w.guards[unit.path][path] {
			return false
		}
	}
	return true
}

func (w *contractWorkCoverage) validate(c *contractConvergence) error {
	for _, row := range w.work {
		if !c.repos[row.RepoPrefix] || row.FilePath == "" || (row.RepoPrefix != "" && !strings.HasPrefix(row.FilePath, row.RepoPrefix+"/")) {
			return fmt.Errorf("%w: captured work is outside selected cohort", graph.ErrContractProjectionIncomplete)
		}
		if row.FilePath == row.RepoPrefix+"/" {
			continue
		}
		if c.clock.lastCensus[row.FilePath] && !c.live[row.FilePath] && !c.excluded[row.FilePath] {
			return fmt.Errorf("%w: captured work census member is unclassified", graph.ErrContractProjectionIncomplete)
		}
	}
	for path := range w.readers {
		if c.clock.lastCensus[path] && !c.live[path] && !c.excluded[path] {
			return fmt.Errorf("%w: captured work reader is unclassified", graph.ErrContractProjectionIncomplete)
		}
	}
	if w.broad {
		for path := range c.clock.lastCensus {
			if !c.live[path] && !c.excluded[path] {
				return fmt.Errorf("%w: selected census member is unclassified", graph.ErrContractProjectionIncomplete)
			}
		}
	}
	return nil
}
