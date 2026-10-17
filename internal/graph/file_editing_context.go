package graph

// FileEditingContextOf computes the FileEditingContext projection through r's
// own Reader methods: the file node, the symbols defined in filePath, the file
// node's import out-edges, and the 1-hop callers / callees (via EdgeCalls) of
// the defined call targets, kept to symbols outside the file. kinds is the set
// of node kinds treated as call targets (function + method). An empty path or
// a file with no nodes returns nil.
//
// Every read is a batched Reader call, so a reader wrapper that narrows
// adjacency gets the same fixed round-trip count with its own edge semantics.
func FileEditingContextOf(r Reader, filePath string, kinds []NodeKind) *FileEditingContextResult {
	if filePath == "" {
		return nil
	}
	nodes := r.GetFileNodes(filePath)
	if len(nodes) == 0 {
		return nil
	}
	kset := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	res := &FileEditingContextResult{}
	var fileNodeID string
	var defNodeIDs []string
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == KindFile {
			res.FileNode = n
			fileNodeID = n.ID
			continue
		}
		res.Defines = append(res.Defines, n)
		if _, ok := kset[n.Kind]; ok {
			defNodeIDs = append(defNodeIDs, n.ID)
		}
	}
	if fileNodeID != "" {
		for _, e := range r.GetOutEdges(fileNodeID) {
			if e == nil {
				continue
			}
			if e.Kind == EdgeImports {
				res.Imports = append(res.Imports, e)
			}
		}
	}
	if len(defNodeIDs) == 0 {
		return res
	}
	inEdges := r.GetInEdgesByNodeIDs(defNodeIDs)
	outEdges := r.GetOutEdgesByNodeIDs(defNodeIDs)
	callerIDSet := make(map[string]struct{})
	calleeIDSet := make(map[string]struct{})
	for _, id := range defNodeIDs {
		for _, e := range inEdges[id] {
			if e == nil || e.Kind != EdgeCalls {
				continue
			}
			if e.From == "" {
				continue
			}
			callerIDSet[e.From] = struct{}{}
		}
		for _, e := range outEdges[id] {
			if e == nil || e.Kind != EdgeCalls {
				continue
			}
			if e.To == "" {
				continue
			}
			calleeIDSet[e.To] = struct{}{}
		}
	}
	callerIDs := make([]string, 0, len(callerIDSet))
	for id := range callerIDSet {
		callerIDs = append(callerIDs, id)
	}
	calleeIDs := make([]string, 0, len(calleeIDSet))
	for id := range calleeIDSet {
		calleeIDs = append(calleeIDs, id)
	}
	callerNodes := r.GetNodesByIDs(callerIDs)
	calleeNodes := r.GetNodesByIDs(calleeIDs)
	for _, id := range callerIDs {
		n := callerNodes[id]
		if n == nil || n.FilePath == filePath {
			continue
		}
		res.CalledBy = append(res.CalledBy, n)
	}
	for _, id := range calleeIDs {
		n := calleeNodes[id]
		if n == nil || n.FilePath == filePath {
			continue
		}
		res.Calls = append(res.Calls, n)
	}
	return res
}
