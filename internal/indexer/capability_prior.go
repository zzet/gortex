package indexer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
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
	// inputs is every prior method's capability-input signature
	// (capabilityInputSignature): what its mutated-field set is derived from.
	inputs map[string]string
	// fnValue is the files' fn-value state (resolver.FnValuePrior), so the
	// derived framework pass re-resolves only what the save can have moved.
	fnValue *resolver.FnValuePrior
	// factoryChain is the files' factory-chain state
	// (resolver.FactoryChainPrior): the chains that failed before a save that
	// kept every declaration are not walked again.
	factoryChain *resolver.FactoryChainPrior
	// fullInputs is every prior node's complete capability input
	// (capabilityFullInputSignature) and rows its capability rows: a node
	// whose input is unchanged, and which no expansion reaches, gets its rows
	// back instead of re-deriving them.
	fullInputs map[string]string
	rows       map[string][]*graph.Edge
	// starts is every prior node's start line: the capability input's sites
	// are compared relative to it, so a save that only moves a method (a line
	// typed above it) leaves its input, and its rows, as they were.
	starts map[string]int
	// hierarchy is the implements/overrides rows recorded at the files,
	// republished when the save left their hierarchy as it was
	// (DerivedInvalidationPlan.HierarchyUnchanged).
	hierarchy []*graph.Edge
}

func newCapabilityPrior() *capabilityPrior {
	return &capabilityPrior{
		files:        make(map[string]struct{}),
		priorIDs:     make(map[string]struct{}),
		receivers:    make(map[string]string),
		writes:       make(map[string]map[string]struct{}),
		restoration:  make(map[string]map[string]struct{}),
		inputs:       make(map[string]string),
		fnValue:      resolver.NewFnValuePrior(),
		factoryChain: resolver.NewFactoryChainPrior(),
		fullInputs:   make(map[string]string),
		rows:         make(map[string][]*graph.Edge),
		starts:       make(map[string]int),
	}
}

// capabilityFieldWrites is the set of fields a source writes, read from its
// outgoing rows the way the per-save pass computes the new set: every field
// it writes directly (a writes edge to a target the capability rows record as
// a field access) and every field an accesses_field row records as written
// (directly, or indirectly through a receiver call).
//
// The accesses_field rows alone are not that set: a source that both reads
// and writes one field keeps a single direct row for it, and the whole-index
// derivation keeps the read (capabilityRepresentativeLess), so a set read off
// the rows misses the field and every save of such a method looked like a
// change of its mutated set, expanding to every transitive receiver caller.
func capabilityFieldWrites(out []*graph.Edge) map[string]struct{} {
	fields := make(map[string]struct{})
	set := make(map[string]struct{})
	for _, edge := range out {
		if edge == nil || edge.Kind != graph.EdgeAccessesField {
			continue
		}
		fields[edge.To] = struct{}{}
		if capabilityAccessIsWrite(edge) {
			set[edge.To] = struct{}{}
		}
	}
	for _, edge := range out {
		if edge == nil || edge.Kind != graph.EdgeWrites {
			continue
		}
		if _, isField := fields[edge.To]; isField {
			set[edge.To] = struct{}{}
		}
	}
	return set
}

// capabilityInputSignature is what a method's mutated-field set is derived
// from, independent of the capability rows stored for it: its writes (with
// the owner of a written node the caller can see, which decides whether the
// write is to its own receiver's field) and its receiver calls (callee, the
// receiver field or self). Two equal signatures derive the same set whatever
// derivation wrote the stored rows, so a base whose rows an older derivation
// wrote does not make every save look like a change.
func capabilityInputSignature(out []*graph.Edge, node func(string) *graph.Node) string {
	rows := make([]string, 0, len(out))
	for _, edge := range out {
		if edge == nil {
			continue
		}
		switch edge.Kind {
		case graph.EdgeWrites:
			owner := ""
			if target := node(edge.To); target != nil {
				receiver, _ := target.Meta["receiver"].(string)
				owner = string(target.Kind) + "\x00" + receiverOwnerKey(target.ID, receiver)
			}
			rows = append(rows, "w\x00"+edge.To+"\x00"+owner)
		case graph.EdgeCalls:
			if edge.Meta == nil {
				continue
			}
			field, _ := edge.Meta["recv_field"].(string)
			self, _ := edge.Meta["recv_self"].(bool)
			if field == "" && !self {
				continue
			}
			selfMark := "0"
			if self {
				selfMark = "1"
			}
			rows = append(rows, "c\x00"+edge.To+"\x00"+field+"\x00"+selfMark)
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\x01")
}

// isCapabilityEdgeKind reports the kinds the capability pass writes.
func isCapabilityEdgeKind(kind graph.EdgeKind) bool {
	return kind == graph.EdgeAccessesField || kind == graph.EdgeReadsEnv || kind == graph.EdgeExecutesProcess
}

// capabilityFullInputSignature is everything a node's capability rows are
// derived from, sites included: its receiver, and every read, write, config
// read and call it makes with target, file and line (the rows carry the
// input's file and line, and an indirect mutation the receiver call's). Two
// equal signatures derive the same direct rows, and the same indirect rows
// while the callees' mutated sets are unchanged.
//
// A site's line is taken relative to the node's start line: a save that
// types a line above a method moves every site in it by the same amount, and
// its rows are the prior ones moved by that amount (reusableIndirectRows).
func capabilityFullInputSignature(node *graph.Node, out []*graph.Edge) string {
	receiver, start := "", 0
	if node != nil {
		start = node.StartLine
		if node.Meta != nil {
			receiver, _ = node.Meta["receiver"].(string)
		}
	}
	rows := make([]string, 0, len(out)+1)
	for _, edge := range out {
		if edge == nil {
			continue
		}
		switch edge.Kind {
		case graph.EdgeReads, graph.EdgeWrites, graph.EdgeReadsConfig, graph.EdgeCalls:
		default:
			continue
		}
		field, self := "", false
		if edge.Meta != nil {
			field, _ = edge.Meta["recv_field"].(string)
			self, _ = edge.Meta["recv_self"].(bool)
		}
		rows = append(rows, fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%t", edge.Kind, edge.To, edge.FilePath, edge.Line-start, field, self))
	}
	sort.Strings(rows)
	return receiver + "\x02" + strings.Join(rows, "\x01")
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
		prior.fnValue.AddFile(stage.graphPath, stage.priorNodes, view.outByNode)
		prior.factoryChain.AddFile(stage.graphPath, stage.priorNodes, view.outByNode)
		for _, node := range stage.priorNodes {
			if node == nil {
				continue
			}
			for _, edge := range view.outByNode[node.ID] {
				if edge != nil && edge.FilePath == stage.graphPath &&
					(edge.Kind == graph.EdgeImplements || edge.Kind == graph.EdgeOverrides) {
					c := *edge
					prior.hierarchy = append(prior.hierarchy, &c)
				}
			}
		}
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
				prior.inputs[node.ID] = capabilityInputSignature(view.outByNode[node.ID], func(id string) *graph.Node {
					return view.nodesByID[id]
				})
			}
			prior.fullInputs[node.ID] = capabilityFullInputSignature(node, view.outByNode[node.ID])
			prior.starts[node.ID] = node.StartLine
			for _, edge := range view.outByNode[node.ID] {
				if edge != nil && isCapabilityEdgeKind(edge.Kind) {
					c := *edge
					prior.rows[node.ID] = append(prior.rows[node.ID], &c)
				}
			}
			writes := prior.writes[node.ID]
			if writes == nil {
				writes = make(map[string]struct{})
				prior.writes[node.ID] = writes
			}
			for target := range capabilityFieldWrites(view.outByNode[node.ID]) {
				writes[target] = struct{}{}
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
	for id, signature := range other.inputs {
		p.inputs[id] = signature
	}
	p.fnValue = p.fnValue.Merge(other.fnValue)
	p.factoryChain = p.factoryChain.Merge(other.factoryChain)
	if p.fullInputs == nil {
		p.fullInputs = make(map[string]string)
	}
	if p.rows == nil {
		p.rows = make(map[string][]*graph.Edge)
	}
	for id, signature := range other.fullInputs {
		p.fullInputs[id] = signature
	}
	for id, rows := range other.rows {
		p.rows[id] = rows
	}
	if p.starts == nil {
		p.starts = make(map[string]int)
	}
	for id, start := range other.starts {
		p.starts[id] = start
	}
	p.hierarchy = append(p.hierarchy, other.hierarchy...)
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
