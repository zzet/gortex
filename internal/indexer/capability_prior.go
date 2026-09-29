package indexer

import (
	"github.com/zzet/gortex/internal/graph"
)

// capabilityPrior is the capability-edge state of the files one incremental
// mutation re-parses, read from the prior view before the files are evicted.
// It lets the per-save capability pass decide which receiver methods' mutated
// field sets changed, instead of re-deriving every transitive receiver caller:
//
//   - writes: every prior node of the files -> the targets of its
//     accesses_field edges with access=write (direct and indirect); these rows
//     are deleted with their source when the file is evicted;
//   - receivers: every prior method -> its receiver;
//   - restoration: every node OUTSIDE the files that had an accesses_field edge
//     INTO a prior node of the files that the eviction deletes (the target's ID
//     or contract changed, so restubIncomingRefsFromView did not carry it) ->
//     the written targets among them. These sources are re-derived by the
//     capability pass.
//
// It is only meaningful for the mutation that captured it: the incremental
// executor moves it into its IndexResult and hands it to that mutation's own
// derived pass. Any other derived pass runs without it (the full frontier).
type capabilityPrior struct {
	files       map[string]struct{}
	priorIDs    map[string]struct{}
	receivers   map[string]string
	writes      map[string]map[string]struct{}
	restoration map[string]map[string]struct{}
}

func newCapabilityPrior() *capabilityPrior {
	return &capabilityPrior{
		files:       make(map[string]struct{}),
		priorIDs:    make(map[string]struct{}),
		receivers:   make(map[string]string),
		writes:      make(map[string]map[string]struct{}),
		restoration: make(map[string]map[string]struct{}),
	}
}

func capabilityAccessIsWrite(edge *graph.Edge) bool {
	if edge == nil || edge.Meta == nil {
		return false
	}
	access, _ := edge.Meta["access"].(string)
	return access == "write"
}

// captureCapabilityPrior reads the capability state of the structural stages
// from the prior view (loaded before eviction).
func captureCapabilityPrior(
	stages []*incrementalBatchStage,
	view incrementalPriorView,
	carried []*graph.Edge,
) *capabilityPrior {
	carriedSet := make(map[*graph.Edge]struct{}, len(carried))
	for _, edge := range carried {
		carriedSet[edge] = struct{}{}
	}
	prior := newCapabilityPrior()
	for _, stage := range stages {
		prior.files[stage.graphPath] = struct{}{}
		for _, node := range stage.priorNodes {
			if node != nil && node.ID != "" {
				prior.priorIDs[node.ID] = struct{}{}
			}
		}
	}
	for _, stage := range stages {
		for _, node := range stage.priorNodes {
			if node == nil || node.ID == "" {
				continue
			}
			if node.Kind == graph.KindMethod {
				receiver, _ := node.Meta["receiver"].(string)
				prior.receivers[node.ID] = receiver
			}
			writes := prior.writes[node.ID]
			if writes == nil {
				writes = make(map[string]struct{})
				prior.writes[node.ID] = writes
			}
			for _, edge := range view.outByNode[node.ID] {
				if edge != nil && edge.Kind == graph.EdgeAccessesField && capabilityAccessIsWrite(edge) {
					writes[edge.To] = struct{}{}
				}
			}
			for _, edge := range view.inByNode[node.ID] {
				if edge == nil || edge.Kind != graph.EdgeAccessesField || edge.From == "" {
					continue
				}
				if _, inside := prior.priorIDs[edge.From]; inside {
					continue
				}
				if _, kept := carriedSet[edge]; kept {
					continue
				}
				targets := prior.restoration[edge.From]
				if targets == nil {
					targets = make(map[string]struct{})
					prior.restoration[edge.From] = targets
				}
				if capabilityAccessIsWrite(edge) {
					targets[edge.To] = struct{}{}
				}
			}
		}
	}
	return prior
}

func (p *capabilityPrior) merge(other *capabilityPrior) *capabilityPrior {
	if other == nil {
		return p
	}
	if p == nil {
		return other
	}
	for file := range other.files {
		p.files[file] = struct{}{}
	}
	for id := range other.priorIDs {
		p.priorIDs[id] = struct{}{}
	}
	for id, receiver := range other.receivers {
		p.receivers[id] = receiver
	}
	for _, pair := range []struct {
		dst, src map[string]map[string]struct{}
	}{{p.writes, other.writes}, {p.restoration, other.restoration}} {
		for id, targets := range pair.src {
			merged := pair.dst[id]
			if merged == nil {
				merged = make(map[string]struct{}, len(targets))
				pair.dst[id] = merged
			}
			for target := range targets {
				merged[target] = struct{}{}
			}
		}
	}
	return p
}

// covers reports whether the prior was captured for every file of a derived
// frontier. A partial prior cannot bound the pass; the caller then runs the
// full frontier.
func (p *capabilityPrior) covers(files []string) bool {
	if p == nil || len(files) == 0 {
		return false
	}
	for _, file := range files {
		if _, ok := p.files[file]; !ok {
			return false
		}
	}
	return true
}

func (idx *Indexer) resetCapabilityPrior() {
	idx.capabilityPriorMu.Lock()
	idx.capabilityPriorPending = nil
	idx.capabilityPriorMu.Unlock()
}

func (idx *Indexer) noteCapabilityPrior(prior *capabilityPrior) {
	idx.capabilityPriorMu.Lock()
	idx.capabilityPriorPending = idx.capabilityPriorPending.merge(prior)
	idx.capabilityPriorMu.Unlock()
}

func (idx *Indexer) takeCapabilityPrior() *capabilityPrior {
	idx.capabilityPriorMu.Lock()
	defer idx.capabilityPriorMu.Unlock()
	prior := idx.capabilityPriorPending
	idx.capabilityPriorPending = nil
	return prior
}
