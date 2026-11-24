package indexer

import (
	"maps"
	"slices"
	"time"

	"github.com/zzet/gortex/internal/reach"
)

// repositoryLaneUndeclared is the kind reported for a lane tenant that named
// no mutation entry.
const repositoryLaneUndeclared = "undeclared"

// repositoryLaneTenure is one tenure of a repository's mutation lane: the
// registered entry that took the lane, when, and for how long it held it.
type repositoryLaneTenure struct {
	holder OutputMutationEntry
	since  time.Time
	held   time.Duration
}

// repositoryLaneKindTotals totals the completed tenures of one holder kind.
type repositoryLaneKindTotals struct {
	tenures uint64
	held    time.Duration
	longest repositoryLaneTenure
}

// repositoryLaneTelemetry is what a coordinator records of its lane, so a wait
// on it can be attributed: the current tenant (zero since while idle), the
// callers blocked at the shared batch admission and at the lane itself, the
// last completed tenure, and totals per holder kind. It is guarded by the
// coordinator's mu.
type repositoryLaneTelemetry struct {
	tenant        repositoryLaneTenure
	batchWaiting  int
	laneWaiting   int
	last          repositoryLaneTenure
	tenuresByKind map[OutputMutationEntry]*repositoryLaneKindTotals
}

// noteLaneWait moves one caller between the batch-admission and lane waits.
func (c *repositoryMutationCoordinator) noteLaneWait(batch, lane int) {
	c.mu.Lock()
	c.laneTelemetry.batchWaiting += batch
	c.laneTelemetry.laneWaiting += lane
	c.mu.Unlock()
}

// beginLaneTenureLocked records holder as the lane's tenant from now on, the
// caller having just taken the lane token. waited says the caller was counted
// as waiting for the lane. c.mu is held.
func (c *repositoryMutationCoordinator) beginLaneTenureLocked(holder OutputMutationEntry, waited bool) {
	if waited {
		c.laneTelemetry.laneWaiting--
	}
	c.laneTelemetry.tenant = repositoryLaneTenure{holder: holder, since: time.Now()}
}

func (c *repositoryMutationCoordinator) beginLaneTenure(holder OutputMutationEntry, waited bool) {
	c.mu.Lock()
	c.beginLaneTenureLocked(holder, waited)
	c.mu.Unlock()
}

// endLaneTenure closes the current tenure. It runs before the lane token goes
// back, so it never closes the next tenant's.
func (c *repositoryMutationCoordinator) endLaneTenure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	lt := &c.laneTelemetry
	if lt.tenant.since.IsZero() {
		return
	}
	tenure := lt.tenant
	tenure.held = time.Since(tenure.since)
	lt.tenant, lt.last = repositoryLaneTenure{}, tenure
	if lt.tenuresByKind == nil {
		lt.tenuresByKind = make(map[OutputMutationEntry]*repositoryLaneKindTotals)
	}
	totals := lt.tenuresByKind[tenure.holder]
	if totals == nil {
		totals = &repositoryLaneKindTotals{}
		lt.tenuresByKind[tenure.holder] = totals
	}
	totals.tenures++
	totals.held += tenure.held
	if tenure.held >= totals.longest.held {
		totals.longest = tenure
	}
}

// RepositoryLaneTenure is one tenure of a repository's mutation lane.
type RepositoryLaneTenure struct {
	// Kind is the registered mutation entry that held the lane, or
	// "undeclared" for a tenant that named none.
	Kind        string `json:"kind"`
	SinceUnixMS int64  `json:"since_unix_ms"`
	// HeldForMS is how long the tenure lasted or, for the current tenant, how
	// long it has held the lane so far.
	HeldForMS float64 `json:"held_for_ms"`
}

// RepositoryLaneKindTotals totals the completed tenures of one holder kind
// since the lane was created, with the longest of them.
type RepositoryLaneKindTotals struct {
	Kind        string               `json:"kind"`
	Tenures     uint64               `json:"tenures"`
	HeldTotalMS float64              `json:"held_total_ms"`
	Longest     RepositoryLaneTenure `json:"longest"`
}

// RepositoryLaneStatus is one repository's mutation lane, which every mutation
// of its graph rows runs on: who holds it and for how long, how many callers
// queue for it, its last tenure, and totals per holder kind. Topology is the
// state of the topology gates the lane's tenant takes next.
type RepositoryLaneStatus struct {
	Repo   string                `json:"repo"`
	Holder *RepositoryLaneTenure `json:"holder,omitempty"`
	// BatchAdmissionWaiting counts callers waiting for the batch gate every
	// repository shares, which they take before the lane; LaneWaiting those
	// waiting for the lane itself.
	BatchAdmissionWaiting int                        `json:"batch_admission_waiting"`
	LaneWaiting           int                        `json:"lane_waiting"`
	Last                  *RepositoryLaneTenure      `json:"last,omitempty"`
	ByKind                []RepositoryLaneKindTotals `json:"by_kind,omitempty"`
	Topology              *reach.TopologyStatus      `json:"topology,omitempty"`
}

func repositoryLaneTenureStatus(tenure repositoryLaneTenure, held time.Duration) RepositoryLaneTenure {
	kind := string(tenure.holder)
	if kind == "" {
		kind = repositoryLaneUndeclared
	}
	return RepositoryLaneTenure{
		Kind:        kind,
		SinceUnixMS: tenure.since.UnixMilli(),
		HeldForMS:   float64(held.Microseconds()) / 1000,
	}
}

func (c *repositoryMutationCoordinator) laneStatus(repo string, now time.Time) RepositoryLaneStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	lt := &c.laneTelemetry
	status := RepositoryLaneStatus{
		Repo:                  repo,
		BatchAdmissionWaiting: lt.batchWaiting,
		LaneWaiting:           lt.laneWaiting,
	}
	if !lt.tenant.since.IsZero() {
		holder := repositoryLaneTenureStatus(lt.tenant, max(now.Sub(lt.tenant.since), 0))
		status.Holder = &holder
	}
	if !lt.last.since.IsZero() {
		last := repositoryLaneTenureStatus(lt.last, lt.last.held)
		status.Last = &last
	}
	for _, holder := range slices.Sorted(maps.Keys(lt.tenuresByKind)) {
		totals := lt.tenuresByKind[holder]
		kind := repositoryLaneTenureStatus(totals.longest, totals.longest.held)
		status.ByKind = append(status.ByKind, RepositoryLaneKindTotals{
			Kind:        kind.Kind,
			Tenures:     totals.tenures,
			HeldTotalMS: float64(totals.held.Microseconds()) / 1000,
			Longest:     kind,
		})
	}
	return status
}

// RepositoryLanes reports the mutation lane of every repository that has one,
// sorted by prefix.
func (mi *MultiIndexer) RepositoryLanes() []RepositoryLaneStatus {
	if mi == nil {
		return nil
	}
	mi.repositoryMutationMu.Lock()
	lanes := maps.Clone(mi.repositoryMutations)
	mi.repositoryMutationMu.Unlock()
	now := time.Now()
	out := make([]RepositoryLaneStatus, 0, len(lanes))
	for _, prefix := range slices.Sorted(maps.Keys(lanes)) {
		out = append(out, lanes[prefix].laneStatus(prefix, now))
	}
	return out
}

// RepositoryLane reports the mutation lane of one repository with the state
// of the topology gates its tenant takes; false when the repository has no
// lane yet.
func (mi *MultiIndexer) RepositoryLane(repoPrefix string) (RepositoryLaneStatus, bool) {
	if mi == nil {
		return RepositoryLaneStatus{}, false
	}
	coordinator := mi.existingRepositoryMutationCoordinator(repoPrefix)
	if coordinator == nil {
		return RepositoryLaneStatus{}, false
	}
	status := coordinator.laneStatus(repoPrefix, time.Now())
	topology := reach.Topology(mi.graph, repoPrefix)
	status.Topology = &topology
	return status, true
}

// ReachTopology reports the reach topology gates over this indexer's store,
// which a repository mutation takes after its lane.
func (mi *MultiIndexer) ReachTopology() reach.TopologyStatus {
	if mi == nil {
		return reach.TopologyStatus{Coupling: "unsupported"}
	}
	return reach.Topology(mi.graph)
}
