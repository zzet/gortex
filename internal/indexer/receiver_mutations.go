package indexer

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// indirectMutSpec is one synthesized indirect field-mutation edge: `from`
// (a method) mutates the field `to` indirectly by calling `via` on it (or on
// its own receiver, which transitively mutates the field).
type indirectMutSpec struct {
	from, to, file, via string
	line                int
}

// stdlibMutators are method names whose bodies are invisible to a source-level
// pass (they live in compiled stdlib export data) but are known to mutate their
// receiver: sync/atomic, sync.Mutex/RWMutex, WaitGroup, Once, sync.Map. A call
// to one of these on a receiver field is an indirect mutation of that field.
var stdlibMutators = map[string]bool{
	// sync/atomic.*
	"Store": true, "Swap": true, "CompareAndSwap": true, "Add": true,
	// sync.Mutex / RWMutex
	"Lock": true, "Unlock": true, "RLock": true, "RUnlock": true, "TryLock": true,
	// sync.WaitGroup
	"Done": true,
	// sync.Once
	"Do": true,
	// sync.Map
	"Delete": true, "LoadOrStore": true, "LoadAndDelete": true,
}

func isStdlibMutator(name string) bool { return stdlibMutators[name] }

// receiverOwnerKey identifies the type a method or field belongs to: its
// receiver type name qualified by the package directory of the declaring file
// (the file part of the node ID). The receiver-call facts this fixpoint reads
// are Go's, where a method's receiver type and that type's fields are always
// declared in the method's own package, so the directory makes the owner exact
// where the bare name is not: two packages each declaring a type Server with a
// field store must never share one owner, or which package's field a call binds
// to would depend on scan order. Empty when the receiver is.
func receiverOwnerKey(nodeID, receiver string) string {
	if receiver == "" {
		return ""
	}
	file := nodeID
	if i := strings.Index(file, "::"); i >= 0 {
		file = file[:i]
	}
	dir := ""
	if i := strings.LastIndex(file, "/"); i >= 0 {
		dir = file[:i]
	}
	return dir + "\x00" + receiver
}

// receiverCallLess is the order the fixpoint visits receiver calls in: by
// caller, then site, then callee. The emitted edge of a (caller, field, via)
// triple carries the site of the first call visited, so the order has to be a
// total one over the facts themselves, never the order a store listed them.
func receiverCallLess(aFrom, aFile string, aLine int, aCallee, aField string, aSelf bool,
	bFrom, bFile string, bLine int, bCallee, bField string, bSelf bool) bool {
	switch {
	case aFrom != bFrom:
		return aFrom < bFrom
	case aFile != bFile:
		return aFile < bFile
	case aLine != bLine:
		return aLine < bLine
	case aCallee != bCallee:
		return aCallee < bCallee
	case aField != bField:
		return aField < bField
	default:
		return !aSelf && bSelf
	}
}

// bareCallName returns the trailing method name of an edge target id, stripping
// any repo / unresolved / package qualifier ("unresolved::*.Store" → "Store").
func bareCallName(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		id = id[i+2:]
	}
	if i := strings.LastIndex(id, "."); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// indirectMutationEdges computes, over the resolved graph, the indirect field
// mutations: `s.counter.Increment()` mutates field counter because Increment
// mutates its receiver; `s.helper()` mutates s's fields because helper does.
// A transitive fixpoint propagates "this method mutates its receiver" through
// own-receiver field-method calls and sibling-method calls — the piece gograph
// explicitly defers. Pure graph traversal, no SSA, no type-checking.
func indirectMutationEdges(g graph.Store) []indirectMutSpec {
	if g == nil {
		return nil
	}
	if scanner, ok := g.(graph.ReceiverMutationScanner); ok {
		return indirectMutationEdgesProjected(scanner)
	}
	recvType := map[string]string{} // methodID → its receiver owner (receiverOwnerKey)
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Meta == nil {
			continue
		}
		if rt, _ := n.Meta["receiver"].(string); rt != "" {
			recvType[n.ID] = receiverOwnerKey(n.ID, rt)
		}
	}
	if len(recvType) == 0 {
		return nil
	}
	// fieldByOwner[receiverType][fieldName] = fieldID.
	fieldByOwner := map[string]map[string]string{}
	for n := range g.NodesByKind(graph.KindField) {
		if n == nil || n.Meta == nil {
			continue
		}
		receiver, _ := n.Meta["receiver"].(string)
		owner := receiverOwnerKey(n.ID, receiver)
		if owner == "" || n.Name == "" {
			continue
		}
		if fieldByOwner[owner] == nil {
			fieldByOwner[owner] = map[string]string{}
		}
		if current := fieldByOwner[owner][n.Name]; current == "" || n.ID < current {
			fieldByOwner[owner][n.Name] = n.ID
		}
	}

	// mutators[methodID] = set of own-receiver field names it mutates.
	mutators := map[string]map[string]bool{}
	addMut := func(m, f string) bool {
		if mutators[m] == nil {
			mutators[m] = map[string]bool{}
		}
		if mutators[m][f] {
			return false
		}
		mutators[m][f] = true
		return true
	}
	// Seed: a method that directly writes a field of its own receiver type.
	// Resolve all field endpoints in one point batch; a disk store must never
	// pay one GetNode query per write edge.
	var writes []*graph.Edge
	var writeTargets []string
	for e := range g.EdgesByKind(graph.EdgeWrites) {
		if e == nil {
			continue
		}
		if _, ok := recvType[e.From]; !ok {
			continue
		}
		writes = append(writes, e)
		writeTargets = append(writeTargets, e.To)
	}
	writeTargetNodes := g.GetNodesByIDs(writeTargets)
	for _, e := range writes {
		owner := recvType[e.From]
		fn := writeTargetNodes[e.To]
		if fn == nil || fn.Kind != graph.KindField {
			continue
		}
		if freceiver, _ := fn.Meta["receiver"].(string); receiverOwnerKey(fn.ID, freceiver) == owner {
			addMut(e.From, fn.Name)
		}
	}

	// Collect own-receiver calls once (recv_field or recv_self stamped).
	type ocall struct {
		from, calleeID, calleeName, recvField string
		recvSelf                              bool
		file                                  string
		line                                  int
	}
	var callEdges []*graph.Edge
	var callTargets []string
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		rf, _ := e.Meta["recv_field"].(string)
		rs, _ := e.Meta["recv_self"].(bool)
		if rf == "" && !rs {
			continue
		}
		if _, ok := recvType[e.From]; !ok {
			continue
		}
		callEdges = append(callEdges, e)
		callTargets = append(callTargets, e.To)
	}
	callTargetNodes := g.GetNodesByIDs(callTargets)
	var ocalls []ocall
	for _, e := range callEdges {
		rf, _ := e.Meta["recv_field"].(string)
		rs, _ := e.Meta["recv_self"].(bool)
		name := bareCallName(e.To)
		if cn := callTargetNodes[e.To]; cn != nil && cn.Kind == graph.KindMethod && cn.Name != "" {
			name = cn.Name
		}
		ocalls = append(ocalls, ocall{
			from: e.From, calleeID: e.To, calleeName: name,
			recvField: rf, recvSelf: rs, file: e.FilePath, line: e.Line,
		})
	}
	sort.Slice(ocalls, func(i, j int) bool {
		a, b := ocalls[i], ocalls[j]
		return receiverCallLess(a.from, a.file, a.line, a.calleeID, a.recvField, a.recvSelf,
			b.from, b.file, b.line, b.calleeID, b.recvField, b.recvSelf)
	})

	// Transitive fixpoint.
	for {
		changed := false
		for _, c := range ocalls {
			calleeMutates := len(mutators[c.calleeID]) > 0
			switch {
			case c.recvField != "":
				if !calleeMutates && !isStdlibMutator(c.calleeName) {
					continue
				}
				owner := recvType[c.from]
				if fieldByOwner[owner][c.recvField] == "" {
					continue // type-consistency: caller's receiver has this field
				}
				if addMut(c.from, c.recvField) {
					changed = true
				}
			case c.recvSelf:
				// Sibling method call on the own receiver: callee must be a
				// method of the same receiver type, and it must mutate.
				if !calleeMutates || recvType[c.calleeID] != recvType[c.from] || recvType[c.from] == "" {
					continue
				}
				for f := range mutators[c.calleeID] {
					if addMut(c.from, f) {
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}

	// Emit indirect accesses_field edges.
	var out []indirectMutSpec
	seen := map[string]bool{}
	emit := func(from, fieldID, file, via string, line int) {
		k := from + "\x00" + fieldID + "\x00" + via
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, indirectMutSpec{from: from, to: fieldID, file: file, via: via, line: line})
	}
	for _, c := range ocalls {
		owner := recvType[c.from]
		calleeMutates := len(mutators[c.calleeID]) > 0
		switch {
		case c.recvField != "":
			if !calleeMutates && !isStdlibMutator(c.calleeName) {
				continue
			}
			if fid := fieldByOwner[owner][c.recvField]; fid != "" {
				emit(c.from, fid, c.file, c.calleeName, c.line)
			}
		case c.recvSelf && calleeMutates:
			if recvType[c.calleeID] != owner || owner == "" {
				continue
			}
			for f := range mutators[c.calleeID] {
				if fid := fieldByOwner[owner][f]; fid != "" {
					emit(c.from, fid, c.file, c.calleeName, c.line)
				}
			}
		}
	}
	return out
}
