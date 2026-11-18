package graph

// RepoScopedInEdgeReader reads the incoming edges of ids whose source node
// belongs to one of repos; the empty prefix names unowned nodes. An edge whose
// source has no node is left out. Only the repository narrow applies here, so
// a caller with a workspace or project boundary still checks each source.
//
// It serves targets that every repository shares, such as the bare
// `unresolved::<name>` placeholders: an ordinary incoming read returns every
// repository's call sites, which a scoped caller then drops one by one.
//
// handled is false when the reader cannot narrow the read, as when a wrapper's
// selected reader lacks it; the caller then reads every incoming edge.
type RepoScopedInEdgeReader interface {
	GetInEdgesByNodeIDsFromRepos(ids, repos []string) (rows map[string][]*Edge, handled bool, err error)
}
