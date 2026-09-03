package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// LocalizationNodeScope is the storage-layer form of a request scope. It is
// intentionally independent of query.QueryOptions so graph does not import the
// query package. Empty fields mean no corresponding restriction.
type LocalizationNodeScope struct {
	WorkspaceID string
	ProjectID   string
	RepoAllow   map[string]bool
	// Kinds is an optional declaration-kind pushdown. It keeps homonym counts
	// meaningful for a caller that can only localize selected node classes.
	Kinds map[NodeKind]bool
	// ExcludeKinds is the complementary pushdown for open-ended declaration
	// sets. It lets callers exclude structural/body-internal kinds without a
	// brittle allow-list that would hide future definition kinds.
	ExcludeKinds map[NodeKind]bool
	// ExcludeTests preserves localization's production-anchor gate. SQLite
	// evaluates the legacy is_test metadata while keyset-paging bounded rows.
	ExcludeTests bool
	// ExcludeFiles applies caller-owned whole-file exclusions before LIMIT.
	// SQLite evaluates it between bounded keyset pages, avoiding a variable-sized
	// NOT IN clause and SQLite variable limits.
	ExcludeFiles map[string]bool

	// excludeFile is a request-local immutable view over an overlay's covered
	// paths. Keeping it separate avoids cloning every overlay file into
	// ExcludeFiles for each exact-name lookup. It composes through
	// withFileExcluder and is deliberately unexported: callers own ExcludeFiles.
	excludeFile func(string) bool
}

// ExcludesFile applies caller and immutable overlay exclusions consistently in
// the in-memory graph, SQLite keyset projections, and overlay composition.
func (s LocalizationNodeScope) ExcludesFile(path string) bool {
	return s.ExcludeFiles[path] || s.excludeFile != nil && s.excludeFile(path)
}

func (s LocalizationNodeScope) withFileExcluder(exclude func(string) bool) LocalizationNodeScope {
	if exclude == nil {
		return s
	}
	if s.excludeFile == nil {
		s.excludeFile = exclude
		return s
	}
	prior := s.excludeFile
	s.excludeFile = func(path string) bool { return prior(path) || exclude(path) }
	return s
}

// Allows applies the same effective workspace/project and repository rules as
// query.QueryOptions.ScopeAllows. Keep this predicate in lock-step with that
// method: SQLite projections use the same rules in SQL while the in-memory and
// overlay readers use this implementation.
func (s LocalizationNodeScope) Allows(n *Node) bool {
	if n == nil {
		return false
	}
	if len(s.Kinds) > 0 && !s.Kinds[n.Kind] {
		return false
	}
	if s.ExcludeKinds[n.Kind] {
		return false
	}
	if s.ExcludeTests {
		if isTest, _ := n.Meta["is_test"].(bool); isTest {
			return false
		}
	}
	path := n.FilePath
	if path == "" {
		path = IDFile(n.ID)
	}
	if s.ExcludesFile(path) {
		return false
	}
	if s.WorkspaceID != "" {
		workspace := n.WorkspaceID
		if workspace == "" {
			workspace = n.RepoPrefix
		}
		if workspace != s.WorkspaceID {
			return false
		}
		if s.ProjectID != "" {
			project := n.ProjectID
			if project == "" {
				project = n.RepoPrefix
			}
			if project != s.ProjectID {
				return false
			}
		}
	}
	// Empty RepoPrefix nodes are global synthetic externals and remain visible
	// under a repository narrow, matching QueryOptions.ScopeAllows.
	return len(s.RepoAllow) == 0 || n.RepoPrefix == "" || s.RepoAllow[n.RepoPrefix]
}

// BoundedNodeProjection is a deterministic, request-bounded node page. Total
// is the number of admitted rows observed up to limit+1, not a corpus-wide
// count. Truncated means that sentinel was reached. This saturation contract is
// sufficient for ambiguity decisions while bounding both memory and row work.
type BoundedNodeProjection struct {
	Nodes     []*Node
	Total     int
	Truncated bool
}

// BoundedExactNameReader is the localization exact-name projection. It is an
// optional capability instead of part of Reader so callers cannot silently
// fall back to the legacy unbounded, full-row FindNodesByName path.
type BoundedExactNameReader interface {
	FindNodesByNameBounded(context.Context, string, LocalizationNodeScope, int) (BoundedNodeProjection, error)
}

// BoundedFileNodeReader projects one file's scoped identity, kind, and source
// location rows without transferring retrieval payloads. It is optional rather
// than part of Reader so localization callers can fail closed instead of
// silently falling back to the legacy full-row GetFileNodes path.
type BoundedFileNodeReader interface {
	FindFileNodesBounded(context.Context, string, LocalizationNodeScope, int) (BoundedNodeProjection, error)
}

var (
	_ BoundedExactNameReader = (*Graph)(nil)
	_ BoundedFileNodeReader  = (*Graph)(nil)
)

// FindNodesByNameBounded reads at most limit+1 matching pointers while still
// counting every scoped homonym. The bounded sorted insertion keeps memory
// proportional to the response cap even for names shared by tens of thousands
// of declarations. This legacy in-memory backend takes one read lock per shard,
// so concurrent mutation may produce a weak cross-shard snapshot; each returned
// page still obeys its cap and Total is never smaller than len(Nodes). SQLite,
// the release backend, provides a single read-transaction snapshot.
func (g *Graph) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	if g == nil || name == "" || limit <= 0 {
		return BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	pageSize := limit + 1
	kept := make([]*Node, 0, pageSize)
	total := 0
	for _, shard := range g.shards {
		if err := ctx.Err(); err != nil {
			return BoundedNodeProjection{}, err
		}
		shard.mu.RLock()
		for i, node := range shard.byName[name] {
			if i&127 == 0 {
				if err := ctx.Err(); err != nil {
					shard.mu.RUnlock()
					return BoundedNodeProjection{}, err
				}
			}
			if !scope.Allows(node) {
				continue
			}
			total++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
		shard.mu.RUnlock()
	}

	truncated := len(kept) > limit || total > limit
	if len(kept) > limit {
		kept = kept[:limit]
	}
	if total > pageSize {
		total = pageSize
	}
	return BoundedNodeProjection{Nodes: kept, Total: total, Truncated: truncated}, nil
}

// FindFileNodesBounded is the in-memory reference implementation of the
// lightweight file projection. It retains only limit+1 pointers while scanning
// the file buckets, applies scope before the cap, and returns a deterministic
// ID-ordered page. As with FindNodesByNameBounded, concurrent writes can yield a
// weak cross-shard snapshot; the page invariants remain intact.
func (g *Graph) FindFileNodesBounded(
	ctx context.Context,
	filePath string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	if g == nil || filePath == "" || limit <= 0 {
		return BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	pageSize := limit + 1
	kept := make([]*Node, 0, pageSize)
	total := 0
	for _, shard := range g.shards {
		if err := ctx.Err(); err != nil {
			return BoundedNodeProjection{}, err
		}
		shard.mu.RLock()
		for index, node := range shard.byFile[filePath] {
			if index&127 == 0 {
				if err := ctx.Err(); err != nil {
					shard.mu.RUnlock()
					return BoundedNodeProjection{}, err
				}
			}
			if !scope.Allows(node) {
				continue
			}
			total++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
		shard.mu.RUnlock()
	}

	truncated := len(kept) > limit || total > limit
	if len(kept) > limit {
		kept = kept[:limit]
	}
	kept = localizationNodeSummaries(kept)
	if total > pageSize {
		total = pageSize
	}
	return BoundedNodeProjection{Nodes: kept, Total: total, Truncated: truncated}, nil
}

func localizationNodeSummary(node *Node) *Node {
	if node == nil {
		return nil
	}
	return &Node{
		ID: node.ID, Kind: node.Kind, Name: node.Name, QualName: node.QualName,
		FilePath: node.FilePath, StartLine: node.StartLine, EndLine: node.EndLine,
		StartColumn: node.StartColumn, EndColumn: node.EndColumn, Language: node.Language,
		RepoPrefix: node.RepoPrefix, WorkspaceID: node.WorkspaceID, ProjectID: node.ProjectID,
	}
}

// localizationNodeSummaries copies only the retained response page onto a fresh,
// exact-capacity backing array. Callers first rank raw immutable graph pointers,
// so discarded candidates never allocate summary objects, and a saturated raw
// sentinel cannot remain reachable behind the returned slice length.
func localizationNodeSummaries(nodes []*Node) []*Node {
	summaries := make([]*Node, len(nodes))
	for index, node := range nodes {
		summaries[index] = localizationNodeSummary(node)
	}
	return summaries
}

func insertBoundedLocalizationNode(nodes []*Node, node *Node, limit int) []*Node {
	if node == nil || limit <= 0 {
		return nodes
	}
	position := sort.Search(len(nodes), func(i int) bool { return nodes[i].ID >= node.ID })
	if position < len(nodes) && nodes[position].ID == node.ID {
		return nodes
	}
	if position >= limit {
		return nodes
	}
	nodes = append(nodes, nil)
	copy(nodes[position+1:], nodes[position:])
	nodes[position] = node
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes
}

const (
	overlayExactNameInspectionLimit = 4096
	overlayDetachedShadowLimit      = 256
)

// BoundedLocalizationLimitError reports that a bounded projection cannot
// preserve its completeness contract within a hard work or allocation limit.
// Callers must not use a partial result when this error is returned.
type BoundedLocalizationLimitError struct {
	Resource string
	Limit    int
}

func (e *BoundedLocalizationLimitError) Error() string {
	return fmt.Sprintf("bounded localization %s exceeds limit %d", e.Resource, e.Limit)
}

var (
	_ BoundedExactNameReader = (*OverlaidView)(nil)
	_ BoundedFileNodeReader  = (*OverlaidView)(nil)
	// ErrBoundedLocalizationUnavailable lets MCP localization fail closed when
	// a third-party Reader has not implemented the bounded projection. Falling
	// back to FindNodesByName would silently restore the unbounded allocation.
	ErrBoundedLocalizationUnavailable = errors.New("bounded localization projection unavailable")
)

// FindNodesByNameBounded merges a bounded base page with the request overlay.
// It inflates the base cap only by the exact set of base identities the layer
// can shadow, so filtering a replacement/tombstone cannot leave a short page.
func (v *OverlaidView) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	if v == nil || name == "" || limit <= 0 {
		return BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	var baseReader BoundedExactNameReader
	if v.base != nil {
		var ok bool
		baseReader, ok = v.base.(BoundedExactNameReader)
		if !ok {
			return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
		}
	}

	var (
		removedIDs   []string
		overlayNamed []*Node
	)
	if v.layer != nil {
		removedIDs = v.layer.RemovedIDsForName(name)
		overlayNamed = v.layer.NodesByName(name)
	}
	removedCount, overlayNodeCount := len(removedIDs), len(overlayNamed)
	if removedCount > overlayExactNameInspectionLimit ||
		overlayNodeCount > overlayExactNameInspectionLimit-removedCount {
		return BoundedNodeProjection{}, &BoundedLocalizationLimitError{
			Resource: "overlay exact-name entries",
			Limit:    overlayExactNameInspectionLimit,
		}
	}

	const maxInt = int(^uint(0) >> 1)
	if limit == maxInt {
		return BoundedNodeProjection{}, &BoundedLocalizationLimitError{
			Resource: "exact-name page capacity",
			Limit:    maxInt - 1,
		}
	}
	pageSize := limit + 1
	kept := make([]*Node, 0, pageSize)
	overlayCount := 0

	// Exclude whole-file replacements and tombstones in the base reader without
	// cloning a potentially repository-sized entries map into every request.
	baseScope := scope
	if v.layer != nil {
		baseScope = baseScope.withFileExcluder(v.layer.HasFile)
	}

	var detachedShadowIDs map[string]struct{}
	shadowCapacity := removedCount + overlayNodeCount
	if shadowCapacity > overlayDetachedShadowLimit {
		shadowCapacity = overlayDetachedShadowLimit
	}
	addDetachedShadow := func(id, path string) error {
		if baseReader == nil || id == "" || baseScope.ExcludesFile(path) {
			return nil
		}
		if _, exists := detachedShadowIDs[id]; exists {
			return nil
		}
		if len(detachedShadowIDs) >= overlayDetachedShadowLimit {
			return &BoundedLocalizationLimitError{
				Resource: "detached overlay shadow identities",
				Limit:    overlayDetachedShadowLimit,
			}
		}
		if detachedShadowIDs == nil {
			detachedShadowIDs = make(map[string]struct{}, shadowCapacity)
		}
		detachedShadowIDs[id] = struct{}{}
		return nil
	}

	inspected := 0
	checkInspection := func() error {
		inspected++
		if inspected&127 == 0 {
			return ctx.Err()
		}
		return nil
	}
	if v.layer != nil {
		for _, id := range removedIDs {
			if err := checkInspection(); err != nil {
				return BoundedNodeProjection{}, err
			}
			if err := addDetachedShadow(id, IDFile(id)); err != nil {
				return BoundedNodeProjection{}, err
			}
		}
		for _, node := range overlayNamed {
			if err := checkInspection(); err != nil {
				return BoundedNodeProjection{}, err
			}
			if node == nil {
				continue
			}
			path := node.FilePath
			if path == "" {
				path = IDFile(node.ID)
			}
			if err := addDetachedShadow(node.ID, path); err != nil {
				return BoundedNodeProjection{}, err
			}
			if !scope.Allows(node) {
				continue
			}
			overlayCount++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	if baseReader == nil {
		truncated := overlayCount > limit
		if len(kept) > limit {
			kept = kept[:limit]
		}
		total := overlayCount
		if total > pageSize {
			total = pageSize
		}
		kept = kept[:len(kept):len(kept)]
		return BoundedNodeProjection{Nodes: kept, Total: total, Truncated: truncated}, nil
	}
	if limit > maxInt-len(detachedShadowIDs) {
		return BoundedNodeProjection{}, &BoundedLocalizationLimitError{
			Resource: "exact-name shadow compensation",
			Limit:    maxInt - limit,
		}
	}
	baseLimit := limit + len(detachedShadowIDs)
	basePage, err := baseReader.FindNodesByNameBounded(ctx, name, baseScope, baseLimit)
	if err != nil {
		return BoundedNodeProjection{}, err
	}

	visible := overlayCount
	for index, node := range basePage.Nodes {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return BoundedNodeProjection{}, err
			}
		}
		if node == nil {
			continue
		}
		if _, shadowed := detachedShadowIDs[node.ID]; shadowed {
			continue
		}
		path := node.FilePath
		if path == "" {
			path = IDFile(node.ID)
		}
		if baseScope.ExcludesFile(path) {
			continue
		}
		visible++
		kept = insertBoundedLocalizationNode(kept, node, pageSize)
	}
	// Saturation of the shadow-inflated base page proves at least limit+1
	// visible rows: no more than len(detachedShadowIDs) returned identities can
	// vanish, while whole-file shadows were excluded inside the base reader.
	if basePage.Truncated && visible <= limit {
		visible = limit + 1
	}
	if visible > pageSize {
		visible = pageSize
	}
	truncated := visible > limit
	if len(kept) > limit {
		kept = kept[:limit]
	}
	kept = kept[:len(kept):len(kept)]
	return BoundedNodeProjection{Nodes: kept, Total: visible, Truncated: truncated}, nil
}

// FindFileNodesBounded preserves file-overlay replacement semantics: an
// overlaid file is answered exclusively from its request-local replacement (or
// as empty for a tombstone), while an untouched file delegates to the bounded
// base capability. It never merges stale base declarations into an overlay.
func (v *OverlaidView) FindFileNodesBounded(
	ctx context.Context,
	filePath string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	if v == nil || filePath == "" || limit <= 0 {
		return BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	if v.layer != nil && v.layer.HasFile(filePath) {
		pageSize := limit + 1
		kept := make([]*Node, 0, pageSize)
		total := 0
		for index, node := range v.layer.FileNodes(filePath) {
			if index&127 == 0 {
				if err := ctx.Err(); err != nil {
					return BoundedNodeProjection{}, err
				}
			}
			if !scope.Allows(node) {
				continue
			}
			total++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
		truncated := total > limit
		if len(kept) > limit {
			kept = kept[:limit]
		}
		kept = localizationNodeSummaries(kept)
		if total > pageSize {
			total = pageSize
		}
		return BoundedNodeProjection{Nodes: kept, Total: total, Truncated: truncated}, nil
	}

	if v.base == nil {
		return BoundedNodeProjection{}, nil
	}
	baseReader, ok := v.base.(BoundedFileNodeReader)
	if !ok {
		return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
	}
	return baseReader.FindFileNodesBounded(ctx, filePath, scope, limit)
}
