package graph

import "iter"

// ScopeBindingNode is the exact row retained by lexical bare-name indexes.
// File paths, text, repository state, and Meta never cross the storage boundary.
type ScopeBindingNode struct {
	ID        string
	Kind      NodeKind
	Name      string
	StartLine int
}

// ScopeBindingNodeSequencer streams lexical binding rows for an exact kind set.
type ScopeBindingNodeSequencer interface {
	ScopeBindingNodesSeq(kinds ...NodeKind) iter.Seq[ScopeBindingNode]
}

// CallableBindingNode is the exact row retained by dataflow callee indexes.
// Source lines, text, repository state, and Meta never cross the boundary.
type CallableBindingNode struct {
	ID       string
	Kind     NodeKind
	Name     string
	FilePath string
}

// CallableBindingNodeSequencer streams callable rows for an exact kind set.
type CallableBindingNodeSequencer interface {
	CallableBindingNodesSeq(kinds ...NodeKind) iter.Seq[CallableBindingNode]
}

// FileLanguageNode is the exact row used by import dispatch and file-language
// indexes.
type FileLanguageNode struct {
	ID       string
	Language string
}

// FileLanguageNodeSequencer streams file IDs and languages.
type FileLanguageNodeSequencer interface {
	FileLanguageNodesSeq() iter.Seq[FileLanguageNode]
}

// RepoNodeIdentity is the exact row used by repository-owned dependency
// indexes.
type RepoNodeIdentity struct {
	ID         string
	RepoPrefix string
}

// RepoNodeIdentitySequencer streams kind-filtered identities. Nil repository
// scope means global; a non-nil empty scope means no rows.
type RepoNodeIdentitySequencer interface {
	RepoNodeIdentitiesSeq(repoPrefixes []string, kinds ...NodeKind) iter.Seq[RepoNodeIdentity]
}

// NamedLanguageNode is the exact row used by language-gated name binders.
type NamedLanguageNode struct {
	ID       string
	Name     string
	Language string
}

// NamedLanguageNodeSequencer streams fixed name/language rows by kind.
type NamedLanguageNodeSequencer interface {
	NamedLanguageNodesSeq(kinds ...NodeKind) iter.Seq[NamedLanguageNode]
}

// QualifiedNodeIdentity is the fixed-column identity used by qualified-name
// target indexes without retaining complete Nodes.
type QualifiedNodeIdentity struct {
	ID         string
	Name       string
	QualName   string
	Language   string
	FilePath   string
	RepoPrefix string
}

// QualifiedNodeIdentitySequencer streams qualified identities by kind.
type QualifiedNodeIdentitySequencer interface {
	QualifiedNodeIdentitiesSeq(kinds ...NodeKind) iter.Seq[QualifiedNodeIdentity]
}

// FileNodeIdentity is the complete file-node state retained by resolver
// directory indexes. Keeping a value row prevents a full Node allocation per
// file and makes accidental access to unprojected fields impossible.
type FileNodeIdentity struct {
	ID          string
	FilePath    string
	RepoPrefix  string
	WorkspaceID string
	Language    string
}

// FileNodeIdentitySequencer streams file identities. A nil repository slice
// means all repositories; a non-nil empty slice means no rows.
type FileNodeIdentitySequencer interface {
	FileNodeIdentitiesSeq(repoPrefixes []string) iter.Seq[FileNodeIdentity]
}

// NodePlacement is the minimal endpoint projection used by reachability
// builders after collecting an explicit ID batch.
type NodePlacement struct {
	Kind       NodeKind
	FilePath   string
	RepoPrefix string
}

// NodePlacementBatchReader resolves an explicit ID set without decoding full
// nodes or metadata.
type NodePlacementBatchReader interface {
	NodePlacementsByIDs(ids []string) map[string]NodePlacement
}

// InEdgeIdentityBatchReader projects only the complete logical identities of
// edges targeting an explicit node-ID batch. Payload, provenance, and metadata
// stay behind the storage boundary until a caller deliberately refetches them.
type InEdgeIdentityBatchReader interface {
	GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]EdgeIdentity
}

// InEdgeIdentitiesByNodeIDs selects the compact capability when available. The
// compatibility fallback converts the ordinary incoming adjacency result.
func InEdgeIdentitiesByNodeIDs(s Store, ids []string) map[string][]EdgeIdentity {
	if s == nil || len(ids) == 0 {
		return nil
	}
	if reader, ok := s.(InEdgeIdentityBatchReader); ok {
		return reader.GetInEdgeIdentitiesByNodeIDs(ids)
	}
	return incomingEdgeIdentityMap(ids, s.GetInEdgesByNodeIDs(ids))
}

func incomingEdgeIdentityMap(ids []string, edgesByTarget map[string][]*Edge) map[string][]EdgeIdentity {
	out := make(map[string][]EdgeIdentity)
	seenTargets := make(map[string]struct{}, len(ids))
	for _, targetID := range ids {
		if targetID == "" {
			continue
		}
		if _, duplicate := seenTargets[targetID]; duplicate {
			continue
		}
		seenTargets[targetID] = struct{}{}
		for _, edge := range edgesByTarget[targetID] {
			if edge == nil || edge.To != targetID {
				continue
			}
			out[targetID] = append(out[targetID], EdgeIdentityFor(edge))
		}
	}
	return out
}

// ScopeBindingNodesSeq selects the compact capability when available. The
// compatibility fallback preserves third-party Store behavior.
func ScopeBindingNodesSeq(s Store, kinds ...NodeKind) iter.Seq[ScopeBindingNode] {
	if s == nil || len(kinds) == 0 {
		return func(func(ScopeBindingNode) bool) {}
	}
	if seq, ok := s.(ScopeBindingNodeSequencer); ok {
		return seq.ScopeBindingNodesSeq(kinds...)
	}
	return func(yield func(ScopeBindingNode) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node == nil {
				continue
			}
			if !yield(ScopeBindingNode{
				ID: node.ID, Kind: node.Kind, Name: node.Name, StartLine: node.StartLine,
			}) {
				return
			}
		}
	}
}

// CallableBindingNodesSeq selects the compact capability when available. The
// compatibility fallback preserves third-party Store behavior.
func CallableBindingNodesSeq(s Store, kinds ...NodeKind) iter.Seq[CallableBindingNode] {
	if s == nil || len(kinds) == 0 {
		return func(func(CallableBindingNode) bool) {}
	}
	if seq, ok := s.(CallableBindingNodeSequencer); ok {
		return seq.CallableBindingNodesSeq(kinds...)
	}
	return func(yield func(CallableBindingNode) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node == nil {
				continue
			}
			if !yield(CallableBindingNode{
				ID: node.ID, Kind: node.Kind, Name: node.Name, FilePath: node.FilePath,
			}) {
				return
			}
		}
	}
}

// FileLanguageNodesSeq selects the compact file-language capability.
func FileLanguageNodesSeq(s Store) iter.Seq[FileLanguageNode] {
	if s == nil {
		return func(func(FileLanguageNode) bool) {}
	}
	if seq, ok := s.(FileLanguageNodeSequencer); ok {
		return seq.FileLanguageNodesSeq()
	}
	return func(yield func(FileLanguageNode) bool) {
		for node := range s.NodesByKind(KindFile) {
			if node != nil && !yield(FileLanguageNode{ID: node.ID, Language: node.Language}) {
				return
			}
		}
	}
}

// RepoNodeIdentitiesSeq selects a compact repository/kind capability.
func RepoNodeIdentitiesSeq(s Store, repoPrefixes []string, kinds ...NodeKind) iter.Seq[RepoNodeIdentity] {
	if s == nil || len(kinds) == 0 || (repoPrefixes != nil && len(repoPrefixes) == 0) {
		return func(func(RepoNodeIdentity) bool) {}
	}
	if seq, ok := s.(RepoNodeIdentitySequencer); ok {
		return seq.RepoNodeIdentitiesSeq(repoPrefixes, kinds...)
	}
	return func(yield func(RepoNodeIdentity) bool) {
		var nodes iter.Seq[*Node]
		if repoPrefixes == nil {
			nodes = NodesByKindsSeq(s, kinds...)
		} else {
			nodes = NodesInScopeSeq(s, repoPrefixes, nil, kinds...)
		}
		for node := range nodes {
			if node != nil && !yield(RepoNodeIdentity{ID: node.ID, RepoPrefix: node.RepoPrefix}) {
				return
			}
		}
	}
}

// NamedLanguageNodesSeq selects the compact name/language capability.
func NamedLanguageNodesSeq(s Store, kinds ...NodeKind) iter.Seq[NamedLanguageNode] {
	if s == nil || len(kinds) == 0 {
		return func(func(NamedLanguageNode) bool) {}
	}
	if seq, ok := s.(NamedLanguageNodeSequencer); ok {
		return seq.NamedLanguageNodesSeq(kinds...)
	}
	return func(yield func(NamedLanguageNode) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node != nil && !yield(NamedLanguageNode{ID: node.ID, Name: node.Name, Language: node.Language}) {
				return
			}
		}
	}
}

// QualifiedNodeIdentitiesSeq selects the compact qualified identity capability.
func QualifiedNodeIdentitiesSeq(s Store, kinds ...NodeKind) iter.Seq[QualifiedNodeIdentity] {
	if s == nil || len(kinds) == 0 {
		return func(func(QualifiedNodeIdentity) bool) {}
	}
	if seq, ok := s.(QualifiedNodeIdentitySequencer); ok {
		return seq.QualifiedNodeIdentitiesSeq(kinds...)
	}
	return func(yield func(QualifiedNodeIdentity) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node != nil && !yield(QualifiedNodeIdentity{
				ID: node.ID, Name: node.Name, QualName: node.QualName,
				Language: node.Language, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix,
			}) {
				return
			}
		}
	}
}

// NodeIDsForKinds uses the existing ID-only backend capability when available.
func NodeIDsForKinds(s Store, kinds ...NodeKind) []string {
	if s == nil || len(kinds) == 0 {
		return nil
	}
	if reader, ok := s.(NodeIDsByKinds); ok {
		return reader.NodeIDsByKinds(kinds)
	}
	var ids []string
	for node := range NodesByKindsSeq(s, kinds...) {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

// FileNodeIdentitiesSeq selects the compact file capability when available.
func FileNodeIdentitiesSeq(s Store, repoPrefixes []string) iter.Seq[FileNodeIdentity] {
	if s == nil || (repoPrefixes != nil && len(repoPrefixes) == 0) {
		return func(func(FileNodeIdentity) bool) {}
	}
	if seq, ok := s.(FileNodeIdentitySequencer); ok {
		return seq.FileNodeIdentitiesSeq(repoPrefixes)
	}
	return func(yield func(FileNodeIdentity) bool) {
		var nodes iter.Seq[*Node]
		if repoPrefixes == nil {
			nodes = s.NodesByKind(KindFile)
		} else {
			nodes = NodesInScopeSeq(s, repoPrefixes, nil, KindFile)
		}
		for node := range nodes {
			if node != nil && !yield(fileNodeIdentity(node)) {
				return
			}
		}
	}
}

// NodePlacementsByIDs selects the compact batch capability when available.
func NodePlacementsByIDs(s Store, ids []string) map[string]NodePlacement {
	if s == nil || len(ids) == 0 {
		return map[string]NodePlacement{}
	}
	if reader, ok := s.(NodePlacementBatchReader); ok {
		return reader.NodePlacementsByIDs(ids)
	}
	nodes := s.GetNodesByIDs(ids)
	out := make(map[string]NodePlacement, len(nodes))
	for id, node := range nodes {
		if node == nil {
			continue
		}
		out[id] = NodePlacement{Kind: node.Kind, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix}
	}
	return out
}

func (g *Graph) ScopeBindingNodesSeq(kinds ...NodeKind) iter.Seq[ScopeBindingNode] {
	return func(yield func(ScopeBindingNode) bool) {
		for node := range g.NodesByKindsSeq(kinds...) {
			if node != nil && !yield(ScopeBindingNode{
				ID: node.ID, Kind: node.Kind, Name: node.Name, StartLine: node.StartLine,
			}) {
				return
			}
		}
	}
}

func (g *Graph) CallableBindingNodesSeq(kinds ...NodeKind) iter.Seq[CallableBindingNode] {
	return func(yield func(CallableBindingNode) bool) {
		for node := range g.NodesByKindsSeq(kinds...) {
			if node != nil && !yield(CallableBindingNode{
				ID: node.ID, Kind: node.Kind, Name: node.Name, FilePath: node.FilePath,
			}) {
				return
			}
		}
	}
}

func (g *Graph) FileLanguageNodesSeq() iter.Seq[FileLanguageNode] {
	return func(yield func(FileLanguageNode) bool) {
		for node := range g.NodesByKind(KindFile) {
			if node != nil && !yield(FileLanguageNode{ID: node.ID, Language: node.Language}) {
				return
			}
		}
	}
}

func (g *Graph) RepoNodeIdentitiesSeq(repoPrefixes []string, kinds ...NodeKind) iter.Seq[RepoNodeIdentity] {
	wanted := resolverProjectionStringSet(repoPrefixes)
	return func(yield func(RepoNodeIdentity) bool) {
		if repoPrefixes != nil && len(repoPrefixes) == 0 {
			return
		}
		for node := range g.NodesByKindsSeq(kinds...) {
			if node == nil {
				continue
			}
			if wanted != nil {
				if _, ok := wanted[node.RepoPrefix]; !ok {
					continue
				}
			}
			if !yield(RepoNodeIdentity{ID: node.ID, RepoPrefix: node.RepoPrefix}) {
				return
			}
		}
	}
}

func (g *Graph) NamedLanguageNodesSeq(kinds ...NodeKind) iter.Seq[NamedLanguageNode] {
	return func(yield func(NamedLanguageNode) bool) {
		for node := range g.NodesByKindsSeq(kinds...) {
			if node != nil && !yield(NamedLanguageNode{ID: node.ID, Name: node.Name, Language: node.Language}) {
				return
			}
		}
	}
}

func (g *Graph) QualifiedNodeIdentitiesSeq(kinds ...NodeKind) iter.Seq[QualifiedNodeIdentity] {
	return func(yield func(QualifiedNodeIdentity) bool) {
		for node := range g.NodesByKindsSeq(kinds...) {
			if node != nil && !yield(QualifiedNodeIdentity{
				ID: node.ID, Name: node.Name, QualName: node.QualName,
				Language: node.Language, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix,
			}) {
				return
			}
		}
	}
}

func (g *Graph) FileNodeIdentitiesSeq(repoPrefixes []string) iter.Seq[FileNodeIdentity] {
	wanted := resolverProjectionStringSet(repoPrefixes)
	return func(yield func(FileNodeIdentity) bool) {
		if repoPrefixes != nil && len(repoPrefixes) == 0 {
			return
		}
		for node := range g.NodesByKind(KindFile) {
			if node == nil {
				continue
			}
			if wanted != nil {
				if _, ok := wanted[node.RepoPrefix]; !ok {
					continue
				}
			}
			if !yield(fileNodeIdentity(node)) {
				return
			}
		}
	}
}

func (g *Graph) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]EdgeIdentity {
	return incomingEdgeIdentityMap(ids, g.GetInEdgesByNodeIDs(ids))
}

func (g *Graph) NodePlacementsByIDs(ids []string) map[string]NodePlacement {
	nodes := g.GetNodesByIDs(ids)
	out := make(map[string]NodePlacement, len(nodes))
	for id, node := range nodes {
		if node != nil {
			out[id] = NodePlacement{Kind: node.Kind, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix}
		}
	}
	return out
}

func fileNodeIdentity(node *Node) FileNodeIdentity {
	return FileNodeIdentity{
		ID: node.ID, FilePath: node.FilePath,
		RepoPrefix: node.RepoPrefix, WorkspaceID: node.WorkspaceID,
		Language: node.Language,
	}
}

func resolverProjectionStringSet(values []string) map[string]struct{} {
	if values == nil {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

var (
	_ ScopeBindingNodeSequencer      = (*Graph)(nil)
	_ CallableBindingNodeSequencer   = (*Graph)(nil)
	_ FileLanguageNodeSequencer      = (*Graph)(nil)
	_ RepoNodeIdentitySequencer      = (*Graph)(nil)
	_ NamedLanguageNodeSequencer     = (*Graph)(nil)
	_ QualifiedNodeIdentitySequencer = (*Graph)(nil)
	_ FileNodeIdentitySequencer      = (*Graph)(nil)
	_ NodePlacementBatchReader       = (*Graph)(nil)
	_ InEdgeIdentityBatchReader      = (*Graph)(nil)
)
