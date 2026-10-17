package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// The batch is private to one revalidation's R0/R1 bracket. It groups only
// endpoint questions already present in that bracket's ledgers; each logical
// question still records/checks its own answer and owners. It is never retained
// in an actor checkpoint, and errors or incomplete/oversized grouped reads use
// the original checked single-name path instead of proving an empty answer.
type contractEndpointReplayBatch struct {
	names []string
	bytes int
	tried bool
	rows  map[string]contractEndpointReplayRows
}
type contractEndpointReplayRows struct {
	count     int // includes non-constant rows, preserving the original per-name cap
	constants []graph.Node
}

func (b *contractEndpointReplayBatch) add(name string) {
	for _, old := range b.names {
		if old == name {
			return
		}
	}
	if len(b.names) == contractFrontierReadBatchSize || len(name) > contractFollowupCompactLimit-b.bytes {
		return
	}
	b.names = append(b.names, name)
	b.bytes += len(name)
}
func (b *contractEndpointReplayBatch) close() { b.names = nil; b.rows = nil; b.tried = true }
func (b *contractEndpointReplayBatch) lookup(e *contractFollowupEvidence, name string) (contractEndpointReplayRows, bool) {
	if b == nil || e.ctx.Err() != nil {
		return contractEndpointReplayRows{}, false
	}
	if !b.tried {
		b.tried = true
		if len(b.names) < 2 {
			return contractEndpointReplayRows{}, false
		}
		pending := make(map[string]contractEndpointReplayRows, len(b.names))
		for _, name := range b.names {
			pending[name] = contractEndpointReplayRows{}
		}
		count, size, complete := 0, b.bytes, true
		err := graph.VisitNodesByNamesContext(e.ctx, e.core, b.names, func(node *graph.Node) bool {
			count++
			if node == nil || count > graph.ContractProjectionRowLimit {
				complete = false
				return false
			}
			rows, ok := pending[node.Name]
			if !ok {
				complete = false
				return false
			}
			rows.count++
			if node.Kind == graph.KindConstant {
				// Full selected rows were decoded above; retain only the declaration
				// fields this endpoint door consumes, detached from reader allocations.
				cost := len(node.ID) + len(node.FilePath) + len(node.RepoPrefix)
				if cost > contractFollowupCompactLimit-size {
					complete = false
					return false
				}
				size += cost
				rows.constants = append(rows.constants, graph.Node{ID: strings.Clone(node.ID), Kind: node.Kind, FilePath: strings.Clone(node.FilePath), RepoPrefix: strings.Clone(node.RepoPrefix)})
			}
			pending[node.Name] = rows
			return true
		})
		if err == nil && complete && e.ctx.Err() == nil {
			b.rows = pending
		}
	}
	rows, ok := b.rows[name]
	return rows, ok
}
