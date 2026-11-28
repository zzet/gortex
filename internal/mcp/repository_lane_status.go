package mcp

import (
	"github.com/zzet/gortex/internal/indexer"
)

// attachPendingRepositoryLane adds, to the payload of a receipt still waiting
// on its publication, the mutation lane of the repository the edit belongs to:
// what holds the lane and for how long, the callers queued for it, and the
// topology gates its tenant takes next. A caller polling a receipt can then
// tell an edit queued behind another holder of its repository's lane from one
// whose own patch is running. repo names the repository when the caller knows
// it; otherwise the owner of absPath is used.
//
// Receipts are polled, so the history (last tenure, per-kind totals, the last
// coupling read) stays in daemon status and index_health.
func (s *Server) attachPendingRepositoryLane(payload map[string]any, repo, absPath string) {
	if s.multiIndexer == nil || payload == nil {
		return
	}
	if repo == "" {
		if absPath == "" {
			return
		}
		repo = s.multiIndexer.RepoForFile(absPath)
	}
	lane, ok := s.multiIndexer.RepositoryLane(repo)
	if !ok {
		return
	}
	lane.Last, lane.ByKind = nil, nil
	if lane.Topology != nil {
		lane.Topology.LastRead = nil
	}
	payload["repository_lane"] = lane
}

// indexHealthRepositoryLanes summarises the repository mutation lanes for
// index_health: how many lanes exist, the ones held or queued for now, and the
// topology gates their tenants take next. Daemon status carries every lane
// with its per-kind totals.
func (s *Server) indexHealthRepositoryLanes() map[string]any {
	if s.multiIndexer == nil {
		return nil
	}
	lanes := s.multiIndexer.RepositoryLanes()
	active := make([]indexer.RepositoryLaneStatus, 0)
	for _, lane := range lanes {
		if lane.Holder == nil && lane.LaneWaiting == 0 && lane.BatchAdmissionWaiting == 0 {
			continue
		}
		lane.ByKind = nil
		active = append(active, lane)
	}
	return map[string]any{
		"lanes":    len(lanes),
		"active":   active,
		"topology": s.multiIndexer.ReachTopology(),
	}
}
