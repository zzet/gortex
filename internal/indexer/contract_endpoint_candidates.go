package indexer

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

var _ contracts.EndpointConstantCandidateStore = (*contractFollowupEvidence)(nil)

// FindEndpointConstantCandidates records only the declaration fields endpoint
// resolution consumes. The general names facade retains its full-node proof
// for binding, shapes, and other consumers that read symbol metadata.
func (e *contractFollowupEvidence) FindEndpointConstantCandidates(name, repoPrefix string) []contracts.EndpointConstantCandidate {
	e.ledger.key("name:", name)
	out, err := e.endpointConstantCandidates(name, repoPrefix)
	if err != nil {
		e.fail(err)
		return nil
	}
	for _, candidate := range out {
		e.ledger.owner(candidate.FilePath)
	}
	e.ledger.read(contractReadConstCandidates, []string{name, repoPrefix}, out)
	return out
}

func (e *contractFollowupEvidence) endpointConstantCandidates(name, repoPrefix string) ([]contracts.EndpointConstantCandidate, error) {
	defer e.guard.enter()()
	if e.err != nil {
		return nil, e.err
	}
	var out []contracts.EndpointConstantCandidate
	count := 0
	limited := false
	visit := func(node *graph.Node) bool {
		count++
		if count > graph.ContractProjectionRowLimit {
			limited = true
			return false
		}
		if node == nil || node.Kind != graph.KindConstant {
			return true
		}
		// Keep the resolver's exact legacy scope rule: RepoPrefix alone
		// does not admit a declaration whose identity and file lie outside.
		if repoPrefix != "" && !strings.HasPrefix(node.ID, repoPrefix+"/") && !strings.HasPrefix(node.FilePath, repoPrefix+"/") {
			return true
		}
		if node.ID == "" || !e.allowedRepos[node.RepoPrefix] {
			e.fail(fmt.Errorf("contract followup: invalid endpoint constant declaration %q", node.ID))
			return false
		}
		out = append(out, contracts.EndpointConstantCandidate{ID: node.ID, FilePath: node.FilePath})
		return true
	}
	var err error
	if rows, ok := e.endpointReplay.lookup(e, name); ok {
		count = rows.count - len(rows.constants)
		for i := range rows.constants {
			if !visit(&rows.constants[i]) {
				break
			}
		}
	} else {
		err = graph.VisitNodesByNamesContext(e.ctx, e.core, []string{name}, visit)
	}
	if limited {
		return nil, graph.ErrContractProjectionLimit
	}
	if err != nil {
		return nil, err
	}
	return out, e.err
}
