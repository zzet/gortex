package indexer

import (
	"errors"
	"sort"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

// This private capability is used only for provider-handler discovery. Generic
// name lookups retain their full rows, imports and completed-name bookkeeping.
var errContractHandlerDeclarationFallback = errors.New("handler declaration proof requires full lookup")

func contractHandlerDeclaration(n *graph.Node) *graph.Node {
	return &graph.Node{ID: n.ID, Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, RepoPrefix: n.RepoPrefix}
}

func contractSameHandlerDeclaration(a, b *graph.Node) bool {
	return a != nil && b != nil && a.ID == b.ID && a.Name == b.Name && a.Kind == b.Kind && a.FilePath == b.FilePath && a.RepoPrefix == b.RepoPrefix
}

func contractHydrateHandler(declaration, full *graph.Node) *graph.Node {
	if contractSameHandlerDeclaration(declaration, full) {
		return full
	}
	return nil
}

// readHandlerDeclarations is also the replay door: every matching kind and
// namespace participates before the picker filters functions/methods. It uses
// checked selected readers and never writes these incomplete nodes to scratch.
func (e *contractFollowupEvidence) readHandlerDeclarations(core graph.Reader, names []string) (map[string][]*graph.Node, error) {
	out := make(map[string][]*graph.Node)
	rows := 0
	bytes := 0
	fallback := false
	requested := make(map[string]bool, len(names))
	for _, name := range names {
		requested[name] = true
	}
	err := graph.VisitNodeDeclarationsByNamesContext(e.ctx, core, names, func(n *graph.Node) bool {
		rows++
		if rows > graph.ContractProjectionRowLimit {
			e.fail(graph.ErrContractProjectionLimit)
			return false
		}
		if n == nil || n.Kind == graph.KindContract || n.Kind == graph.KindContractBridge {
			return true
		}
		if n.ID == "" || !e.allowedRepos[n.RepoPrefix] {
			// Apply precisely the selected namespace/identity validation of full reads.
			e.detach(n)
			return false
		}
		if n.Kind == graph.KindConfigKey {
			fallback = true
			return false
		}
		bytes += len(n.ID) + len(n.Name) + len(n.Kind) + len(n.FilePath) + len(n.RepoPrefix)
		if !requested[n.Name] || bytes > contractFollowupCompactLimit ||
			!utf8.ValidString(n.ID) || !utf8.ValidString(n.Name) || !utf8.ValidString(string(n.Kind)) ||
			!utf8.ValidString(n.FilePath) || !utf8.ValidString(n.RepoPrefix) {
			fallback = true
			return false
		}
		out[n.Name] = append(out[n.Name], contractHandlerDeclaration(n))
		return true
	})
	if err != nil {
		e.fail(err)
	}
	if e.err != nil {
		return nil, e.err
	}
	if fallback {
		return nil, errContractHandlerDeclarationFallback
	}
	for _, nodes := range out {
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	}
	return out, nil
}

func (e *contractFollowupEvidence) contractHandlerDeclarations(names []string) (map[string][]*graph.Node, bool) {
	if e.err != nil {
		return nil, true
	}
	// Repeated names can have reader-specific visit multiplicity. Preserve the
	// existing full lookup rather than inventing a different cap/order policy.
	seenNames := make(map[string]bool, len(names))
	for _, name := range names {
		if seenNames[name] {
			return nil, false
		}
		seenNames[name] = true
	}
	e.ledger.key("name:", names...)
	defer e.guard.enter()()
	out, err := e.readHandlerDeclarations(e.core, names)
	if errors.Is(err, errContractHandlerDeclarationFallback) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	byID := make(map[string]*graph.Node)
	var ids []string
	for _, nodes := range out {
		for _, n := range nodes {
			switch n.Kind {
			case graph.KindField, graph.KindType, graph.KindInterface, graph.KindMethod:
				// Full name imports update Spring/Java trackers. Those values must
				// already have been loaded by a complete seed round for this file;
				// declarations intentionally contain neither Language nor metadata.
				file, admitted := e.files[n.FilePath]
				if !e.handlerSeededFiles[n.FilePath] || !admitted || file.RepoPrefix != n.RepoPrefix {
					return nil, false
				}
			case graph.KindFunction, graph.KindBuiltin, graph.KindConstant, graph.KindLocal, graph.KindParam, graph.KindVariable:
				// Non-handler name collisions still participate in the complete
				// declaration proof and scratch identity checks. These kinds have
				// no payload-output, Spring-binding or Java-method import effect.
				// The picker continues to consider only functions and methods.
			default:
				return nil, false
			}
			if byID[n.ID] != nil {
				return nil, false
			}
			byID[n.ID] = n
			ids = append(ids, n.ID)
		}
	}
	// A scratch row may override a core ID even after being renamed out of the
	// requested names. Probe identities as well as matching declarations. Only
	// exact agreement permits the read-only path; all uncertainty uses legacy.
	scratchIDs := make(map[string]bool)
	compatible := true
	err = graph.VisitNodeDeclarationsByNamesContext(e.ctx, e.scratch, names, func(n *graph.Node) bool {
		if n == nil || !contractSameHandlerDeclaration(byID[n.ID], n) {
			compatible = false
			return false
		}
		scratchIDs[n.ID] = true
		return true
	})
	if err != nil {
		e.fail(err)
		return nil, true
	}
	if !compatible {
		return nil, false
	}
	present, err := e.scratch.GetNodePresenceByIDsContext(e.ctx, ids)
	if err != nil {
		e.fail(err)
		return nil, true
	}
	for id := range present {
		if !scratchIDs[id] {
			return nil, false
		}
	}
	for _, nodes := range out {
		e.ledger.nodes(nodes...)
	}
	e.ledger.read(contractReadHandlerDeclarations, names, out)
	return out, true
}
