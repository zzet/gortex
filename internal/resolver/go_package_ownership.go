package resolver

import "github.com/zzet/gortex/internal/graph"

// GoPackageOwnershipResult distinguishes certified package identity from
// unavailable evidence. Lookup absence MUST NOT be returned as Different.
type GoPackageOwnershipResult uint8

const (
	GoPackageOwnershipUnknown GoPackageOwnershipResult = iota
	GoPackageOwnershipExact
	GoPackageOwnershipDifferent
)

// GoImportCandidate contains canonical graph identities, not filesystem paths.
// CandidateNode is present for rich symbol/qualified-name candidates and nil
// for a compact file identity. Its fields are read-only. The provider must keep
// virtual dependency/contract nodes and uncertified placements Unknown.
type GoImportCandidate struct {
	ImportPath          string
	ImporterRepoPrefix  string
	ImporterFilePath    string
	CandidateID         string
	CandidateRepoPrefix string
	CandidateFilePath   string
	CandidateNode       *graph.Node
}

// GoPackageOwnershipLookup is optional immutable target-view evidence supplied
// by the indexer. It must be concurrency-safe and perform no filesystem/store
// I/O in the parallel resolver loop. Unknown includes uncovered repositories,
// vendor/replace ambiguity, refused manifests and incomplete inventories.
type GoPackageOwnershipLookup func(GoImportCandidate) GoPackageOwnershipResult

type goImportCandidateGate struct {
	lookup GoPackageOwnershipLookup
	query  GoImportCandidate
}

func (g goImportCandidateGate) retain(id, repo, file string, node *graph.Node) bool {
	if g.lookup == nil || file == "" {
		return true
	}
	query := g.query
	query.CandidateID = id
	query.CandidateRepoPrefix = repo
	query.CandidateFilePath = file
	query.CandidateNode = node
	return g.lookup(query) != GoPackageOwnershipDifferent
}

func (g goImportCandidateGate) retainNode(node *graph.Node) bool {
	return node == nil || g.retain(node.ID, node.RepoPrefix, node.FilePath, node)
}

func (g goImportCandidateGate) retainFile(file graph.FileNodeIdentity) bool {
	return g.retain(file.ID, file.RepoPrefix, file.FilePath, nil)
}

func (g goImportCandidateGate) retainTarget(r *Resolver, target string) bool {
	if g.lookup == nil {
		return true
	}
	return g.retainNode(r.cachedGetNode(target))
}

// filterNodes never mutates a shared per-pass candidate slice. Nil-provider
// calls return the same slice immediately; an enabled provider allocates only
// after the first certified rejection. Retained order/duplicates are unchanged.
func (g goImportCandidateGate) filterNodes(nodes []*graph.Node) []*graph.Node {
	if g.lookup == nil {
		return nodes
	}
	var filtered []*graph.Node
	for i, node := range nodes {
		keep := g.retainNode(node)
		if filtered == nil {
			if keep {
				continue
			}
			filtered = make([]*graph.Node, 0, len(nodes)-1)
			filtered = append(filtered, nodes[:i]...)
		} else if keep {
			filtered = append(filtered, node)
		}
	}
	if filtered == nil {
		return nodes
	}
	return filtered
}

// SetGoPackageOwnership configures an optional target-bound rejection rule.
// Like the other resolver configuration setters, call only before resolution,
// under the owning indexer's lifecycle exclusion; not while a pass is active
// (including its inter-chunk mutex yields). Install a fresh immutable provider
// for a new target/config. Passing nil restores existing resolution behavior.
func (r *Resolver) SetGoPackageOwnership(lookup GoPackageOwnershipLookup) {
	r.goPackageOwnership = lookup
}

func (r *Resolver) goImportGateForEdge(e *graph.Edge, importPath string) goImportCandidateGate {
	if (r.goPackageOwnership == nil && len(r.goPackageOwnershipPrepared) == 0) || e == nil {
		return goImportCandidateGate{}
	}
	// Match the existing resolverNameScopeForEdge source-language contract.
	// An authoritative populated page cache must not trigger point lookups for
	// missing sources. Single-file/direct resolution may have no page cache.
	source := r.nodeByID[e.From]
	if source == nil && r.nodeByID == nil && r.graph != nil {
		source = r.graph.GetNode(e.From)
	}
	if source == nil || source.Language != "go" {
		return goImportCandidateGate{}
	}
	lookup := r.goPackageOwnership
	if r.goPackageOwnershipFactory != nil {
		// A factory is authoritative for this pass's coverage, including an
		// explicitly prepared Unknown repo. Never reuse a setup-time snapshot
		// for a repo that this pass's source authority does not cover.
		lookup = r.goPackageOwnershipPrepared[source.RepoPrefix]
	}
	return goImportCandidateGate{
		lookup: lookup,
		query: GoImportCandidate{
			ImportPath: importPath, ImporterRepoPrefix: source.RepoPrefix,
			ImporterFilePath: source.FilePath,
		},
	}
}
