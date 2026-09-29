package indexer

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// indirectMutationEdgesForMethods computes the indirect-mutation slice whose
// truth can change when seedMethods change. It expands backwards through
// receiver calls to affected callers, then forwards only through the receiver
// callees needed to evaluate that bounded dependency frontier.
func indirectMutationEdgesForMethods(
	g graph.Store,
	seedMethods []*graph.Node,
) ([]indirectMutSpec, map[string]*graph.Node) {
	return indirectMutationEdgesForRoots(g, seedMethods, nil)
}

// indirectMutationEdgesForRoots is indirectMutationEdgesForMethods with the
// backward expansion restricted to the roots named in expandFrom (every root
// when expandFrom is nil). Every root is evaluated and emitted; only callers
// reachable backwards from an expanding root join them. A root whose mutated
// field set is unchanged cannot change any caller's summary — callers read a
// callee only through that set — so it need not expand.
func indirectMutationEdgesForRoots(
	g graph.Store,
	seedMethods []*graph.Node,
	expandFrom map[string]struct{},
) ([]indirectMutSpec, map[string]*graph.Node) {
	if g == nil || len(seedMethods) == 0 {
		return nil, nil
	}
	receiverOf := func(node *graph.Node) string {
		if node == nil || node.Meta == nil {
			return ""
		}
		receiver, _ := node.Meta["receiver"].(string)
		return receiver
	}
	receiverCall := func(edge *graph.Edge) (field string, self bool) {
		if edge == nil || edge.Kind != graph.EdgeCalls || edge.Meta == nil {
			return "", false
		}
		field, _ = edge.Meta["recv_field"].(string)
		self, _ = edge.Meta["recv_self"].(bool)
		return field, self
	}

	impacted := make(map[string]*graph.Node)
	frontier := make([]string, 0, len(seedMethods))
	for _, method := range seedMethods {
		if method == nil || method.Kind != graph.KindMethod || receiverOf(method) == "" {
			continue
		}
		if _, seen := impacted[method.ID]; seen {
			continue
		}
		impacted[method.ID] = method
		if expandFrom != nil {
			if _, expand := expandFrom[method.ID]; !expand {
				continue
			}
		}
		frontier = append(frontier, method.ID)
	}
	// Receiver-call dependants are the only unchanged sources whose mutation
	// summary can change when a seed method changes.
	for len(frontier) > 0 {
		incoming := g.GetInEdgesByNodeIDs(frontier)
		candidateIDs := make([]string, 0)
		seenCandidates := make(map[string]struct{})
		for _, target := range frontier {
			for _, edge := range incoming[target] {
				field, self := receiverCall(edge)
				if field == "" && !self {
					continue
				}
				if _, seen := impacted[edge.From]; seen {
					continue
				}
				if _, seen := seenCandidates[edge.From]; seen {
					continue
				}
				seenCandidates[edge.From] = struct{}{}
				candidateIDs = append(candidateIDs, edge.From)
			}
		}
		candidates := g.GetNodesByIDs(candidateIDs)
		next := make([]string, 0, len(candidates))
		for _, id := range candidateIDs {
			method := candidates[id]
			if method == nil || method.Kind != graph.KindMethod || receiverOf(method) == "" {
				continue
			}
			impacted[id] = method
			next = append(next, id)
		}
		frontier = next
	}
	if len(impacted) == 0 {
		return nil, nil
	}

	// Evaluate impacted methods with the forward receiver-call dependencies
	// their summaries consume. Each graph depth costs one batched adjacency and
	// one batched endpoint read, never one query per method or edge.
	analysisMethods := make(map[string]*graph.Node, len(impacted))
	frontier = frontier[:0]
	for id, method := range impacted {
		analysisMethods[id] = method
		frontier = append(frontier, id)
	}
	outByMethod := make(map[string][]*graph.Edge, len(analysisMethods))
	for len(frontier) > 0 {
		adjacency := g.GetOutEdgesByNodeIDs(frontier)
		targetIDs := make([]string, 0)
		seenTargets := make(map[string]struct{})
		for _, id := range frontier {
			outByMethod[id] = adjacency[id]
			for _, edge := range adjacency[id] {
				field, self := receiverCall(edge)
				if field == "" && !self {
					continue
				}
				if _, seen := analysisMethods[edge.To]; seen {
					continue
				}
				if _, seen := seenTargets[edge.To]; seen {
					continue
				}
				seenTargets[edge.To] = struct{}{}
				targetIDs = append(targetIDs, edge.To)
			}
		}
		targets := g.GetNodesByIDs(targetIDs)
		next := make([]string, 0, len(targets))
		for _, id := range targetIDs {
			method := targets[id]
			if method == nil || method.Kind != graph.KindMethod || receiverOf(method) == "" {
				continue
			}
			analysisMethods[id] = method
			next = append(next, id)
		}
		frontier = next
	}

	writeTargetIDs := make([]string, 0)
	seenWriteTargets := make(map[string]struct{})
	fieldNames := make([]string, 0)
	seenFieldNames := make(map[string]struct{})
	for id := range analysisMethods {
		for _, edge := range outByMethod[id] {
			switch edge.Kind {
			case graph.EdgeWrites:
				if _, seen := seenWriteTargets[edge.To]; !seen {
					seenWriteTargets[edge.To] = struct{}{}
					writeTargetIDs = append(writeTargetIDs, edge.To)
				}
			case graph.EdgeCalls:
				field, _ := receiverCall(edge)
				if field != "" {
					if _, seen := seenFieldNames[field]; !seen {
						seenFieldNames[field] = struct{}{}
						fieldNames = append(fieldNames, field)
					}
				}
			}
		}
	}
	writeTargets := g.GetNodesByIDs(writeTargetIDs)
	fieldsByName := g.FindNodesByNames(fieldNames)
	// ownerOf is the receiver type a method or field belongs to, qualified by
	// its package directory (receiverOwnerKey) exactly as the whole-graph
	// fixpoint keys it, so a per-save re-derivation binds the field a whole
	// index binds.
	ownerOf := func(node *graph.Node) string {
		if node == nil {
			return ""
		}
		return receiverOwnerKey(node.ID, receiverOf(node))
	}
	fieldFor := func(method *graph.Node, name string) *graph.Node {
		owner := ownerOf(method)
		if owner == "" {
			return nil
		}
		var found *graph.Node
		for _, field := range fieldsByName[name] {
			if field == nil || field.Kind != graph.KindField || ownerOf(field) != owner {
				continue
			}
			if found == nil || field.ID < found.ID {
				found = field
			}
		}
		return found
	}

	mutators := make(map[string]map[string]bool)
	addMutation := func(methodID, fieldID string) bool {
		if methodID == "" || fieldID == "" {
			return false
		}
		if mutators[methodID] == nil {
			mutators[methodID] = make(map[string]bool)
		}
		if mutators[methodID][fieldID] {
			return false
		}
		mutators[methodID][fieldID] = true
		return true
	}
	for id, method := range analysisMethods {
		for _, edge := range outByMethod[id] {
			if edge == nil || edge.Kind != graph.EdgeWrites {
				continue
			}
			field := writeTargets[edge.To]
			if field == nil || field.Kind != graph.KindField || ownerOf(field) != ownerOf(method) {
				continue
			}
			addMutation(id, field.ID)
		}
	}

	type receiverCallFact struct {
		from, calleeID, calleeName, recvField, file string
		recvSelf                                    bool
		line                                        int
	}
	calls := make([]receiverCallFact, 0)
	for id := range analysisMethods {
		for _, edge := range outByMethod[id] {
			field, self := receiverCall(edge)
			if field == "" && !self {
				continue
			}
			name := bareCallName(edge.To)
			if callee := analysisMethods[edge.To]; callee != nil && callee.Name != "" {
				name = callee.Name
			}
			calls = append(calls, receiverCallFact{
				from: id, calleeID: edge.To, calleeName: name,
				recvField: field, recvSelf: self, file: edge.FilePath, line: edge.Line,
			})
		}
	}
	sort.Slice(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		return receiverCallLess(a.from, a.file, a.line, a.calleeID, a.recvField, a.recvSelf,
			b.from, b.file, b.line, b.calleeID, b.recvField, b.recvSelf)
	})
	for {
		changed := false
		for _, call := range calls {
			calleeMutates := len(mutators[call.calleeID]) > 0
			switch {
			case call.recvField != "":
				if !calleeMutates && !isStdlibMutator(call.calleeName) {
					continue
				}
				if field := fieldFor(analysisMethods[call.from], call.recvField); field != nil {
					changed = addMutation(call.from, field.ID) || changed
				}
			case call.recvSelf:
				caller := analysisMethods[call.from]
				callee := analysisMethods[call.calleeID]
				if !calleeMutates || caller == nil || callee == nil || ownerOf(caller) != ownerOf(callee) {
					continue
				}
				for fieldID := range mutators[call.calleeID] {
					changed = addMutation(call.from, fieldID) || changed
				}
			}
		}
		if !changed {
			break
		}
	}

	var out []indirectMutSpec
	seen := make(map[string]bool)
	for _, call := range calls {
		if impacted[call.from] == nil {
			continue
		}
		calleeMutates := len(mutators[call.calleeID]) > 0
		var fieldIDs []string
		switch {
		case call.recvField != "":
			if !calleeMutates && !isStdlibMutator(call.calleeName) {
				continue
			}
			if field := fieldFor(analysisMethods[call.from], call.recvField); field != nil {
				fieldIDs = append(fieldIDs, field.ID)
			}
		case call.recvSelf:
			caller := analysisMethods[call.from]
			callee := analysisMethods[call.calleeID]
			if !calleeMutates || caller == nil || callee == nil || ownerOf(caller) != ownerOf(callee) {
				continue
			}
			for fieldID := range mutators[call.calleeID] {
				fieldIDs = append(fieldIDs, fieldID)
			}
		}
		for _, fieldID := range fieldIDs {
			key := call.from + "\x00" + fieldID + "\x00" + call.calleeName
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, indirectMutSpec{
				from: call.from, to: fieldID, file: call.file, via: call.calleeName, line: call.line,
			})
		}
	}
	return out, impacted
}
