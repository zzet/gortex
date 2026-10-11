package indexer

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const receiverMutationProjectionPageSize = 4096

type projectedReceiverField struct {
	id    string
	name  string
	owner string
}

type projectedReceiverCall struct {
	from, calleeID, calleeName, recvField string
	recvSelf                              bool
	file                                  string
	line                                  int
}

func indirectMutationEdgesProjected(scanner graph.ReceiverMutationScanner) []indirectMutSpec {
	recvType := map[string]string{}
	scanner.ScanReceiverMutationMethods(receiverMutationProjectionPageSize, func(page []graph.ReceiverMutationMethod) bool {
		for _, row := range page {
			if row.Receiver != "" {
				recvType[row.ID] = receiverOwnerKey(row.ID, row.Receiver)
			}
		}
		return true
	})
	if len(recvType) == 0 {
		return nil
	}

	fieldByOwner := map[string]map[string]string{}
	fieldByID := map[string]projectedReceiverField{}
	scanner.ScanReceiverMutationFields(receiverMutationProjectionPageSize, func(page []graph.ReceiverMutationField) bool {
		for _, row := range page {
			if row.Receiver == "" || row.Name == "" {
				continue
			}
			owner := receiverOwnerKey(row.ID, row.Receiver)
			if fieldByOwner[owner] == nil {
				fieldByOwner[owner] = map[string]string{}
			}
			// One field per (owner, name): the smallest ID when build-tagged
			// files declare the same type twice, as the per-save path picks.
			if current := fieldByOwner[owner][row.Name]; current == "" || row.ID < current {
				fieldByOwner[owner][row.Name] = row.ID
			}
			fieldByID[row.ID] = projectedReceiverField{id: row.ID, name: row.Name, owner: owner}
		}
		return true
	})

	mutators := map[string]map[string]bool{}
	addMut := func(methodID, fieldName string) bool {
		if mutators[methodID] == nil {
			mutators[methodID] = map[string]bool{}
		}
		if mutators[methodID][fieldName] {
			return false
		}
		mutators[methodID][fieldName] = true
		return true
	}
	scanner.ScanReceiverMutationWrites(receiverMutationProjectionPageSize, func(page []graph.ReceiverMutationWrite) bool {
		for _, row := range page {
			owner, ok := recvType[row.From]
			if !ok {
				continue
			}
			field, ok := fieldByID[row.To]
			if ok && field.owner == owner {
				addMut(row.From, field.name)
			}
		}
		return true
	})

	var calls []projectedReceiverCall
	scanner.ScanReceiverMutationCalls(receiverMutationProjectionPageSize, func(page []graph.ReceiverMutationCall) bool {
		for _, row := range page {
			if _, ok := recvType[row.From]; !ok {
				continue
			}
			name := row.TargetMethodName
			if name == "" {
				name = bareCallName(row.To)
			}
			calls = append(calls, projectedReceiverCall{
				from: row.From, calleeID: row.To, calleeName: name,
				recvField: row.RecvField, recvSelf: row.RecvSelf,
				file: row.FilePath, line: row.Line,
			})
		}
		return true
	})
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
				owner := recvType[call.from]
				if fieldByOwner[owner][call.recvField] == "" {
					continue
				}
				if addMut(call.from, call.recvField) {
					changed = true
				}
			case call.recvSelf:
				if !calleeMutates || recvType[call.calleeID] != recvType[call.from] || recvType[call.from] == "" {
					continue
				}
				for fieldName := range mutators[call.calleeID] {
					if addMut(call.from, fieldName) {
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}

	var out []indirectMutSpec
	seen := map[string]bool{}
	emit := func(from, fieldID, file, via string, line int) {
		key := from + "\x00" + fieldID + "\x00" + via
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, indirectMutSpec{from: from, to: fieldID, file: file, via: via, line: line})
	}
	for _, call := range calls {
		owner := recvType[call.from]
		calleeMutates := len(mutators[call.calleeID]) > 0
		switch {
		case call.recvField != "":
			if !calleeMutates && !isStdlibMutator(call.calleeName) {
				continue
			}
			if fieldID := fieldByOwner[owner][call.recvField]; fieldID != "" {
				emit(call.from, fieldID, call.file, call.calleeName, call.line)
			}
		case call.recvSelf && calleeMutates:
			if recvType[call.calleeID] != owner || owner == "" {
				continue
			}
			for fieldName := range mutators[call.calleeID] {
				if fieldID := fieldByOwner[owner][fieldName]; fieldID != "" {
					emit(call.from, fieldID, call.file, call.calleeName, call.line)
				}
			}
		}
	}
	return out
}
