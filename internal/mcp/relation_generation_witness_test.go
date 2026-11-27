package mcp

import (
	"sync"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// relationGenerationWitness records the newest generation with unpublished
// relations that the server read from the watcher for one file. Every
// relation freshness label is built from these watcher snapshots, so the
// witness sees exactly the generations a reply can report as pending.
//
// The watcher numbers generations in admission order. A read that starts once
// the file has no admitted generation can still report one: the read's own
// catch-up for a projection that is retired only after its edit's receipt
// completes, or a native event for the file. FSEvents can deliver the event
// for a tool's own write after that write's ticket has finished, and the
// watcher then admits it as a new generation for the same bytes. A newer
// generation after such a read than before it shows that the reply's relation
// state came from a generation admitted during that read.
type relationGenerationWitness struct {
	key    string
	mu     sync.Mutex
	newest uint64
}

func newRelationGenerationWitness(path string) *relationGenerationWitness {
	return &relationGenerationWitness{key: pathkey.MutationKey(path)}
}

// observe records a watcher snapshot the server read and returns it unchanged.
func (r *relationGenerationWitness) observe(paths map[string]indexer.MutationTierState) map[string]indexer.MutationTierState {
	if r == nil {
		return paths
	}
	for path, tiers := range paths {
		if tiers.Generation == 0 || tiers.Failed || !tiers.RelationsAt.IsZero() || pathkey.MutationKey(path) != r.key {
			continue
		}
		r.mu.Lock()
		r.newest = max(r.newest, tiers.Generation)
		r.mu.Unlock()
	}
	return paths
}

// generation is the newest generation with unpublished relations that the
// server has read for the file so far, or zero.
func (r *relationGenerationWitness) generation() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.newest
}
