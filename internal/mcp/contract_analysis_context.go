package mcp

import (
	"context"
	"reflect"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/query"
)

type contractAnalysisContextKey struct{}

// contractAnalysisContext binds independently leased, complete analysis to one
// RPC. It never replaces readerFor's core source/declaration reader.
type contractAnalysisContext struct {
	views         map[string]*graphview.ContractAnalysisView
	once          sync.Once
	registry      *contracts.Registry
	registryErr   error
	mu            sync.Mutex
	readErr       error
	graphOnce     sync.Once
	graph         graph.Reader
	graphErr      error
	composedOnce  sync.Once
	composed      graph.Reader
	composedLayer *contractIdentityLayer
	composedErr   error
}

func withContractAnalysisContext(ctx context.Context, binding *contractAnalysisContext) context.Context {
	return context.WithValue(ctx, contractAnalysisContextKey{}, binding)
}

func contractAnalysisFromContext(ctx context.Context) *contractAnalysisContext {
	binding, _ := ctx.Value(contractAnalysisContextKey{}).(*contractAnalysisContext)
	return binding
}

func (binding *contractAnalysisContext) close() {
	if binding == nil {
		return
	}
	for _, view := range binding.views {
		view.Close()
	}
}

func (binding *contractAnalysisContext) recordReadError(err error) {
	if err == nil {
		return
	}
	binding.mu.Lock()
	if binding.readErr == nil {
		binding.readErr = err
	}
	binding.mu.Unlock()
}

func (binding *contractAnalysisContext) readError() error {
	if binding == nil {
		return nil
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	return binding.readErr
}

// Every repo payload participating in a match or bridge must have consulted
// the same selected companion inputs. Independent latest attachments are not
// a coherent cross-repository snapshot, even when their canonical IDs match.
func (binding *contractAnalysisContext) checkCohort() error {
	var version, fingerprint string
	for _, view := range binding.views {
		if len(binding.views) <= 1 {
			return nil
		}
		if view == nil || view.Attachment.InputVersion == "" || view.Attachment.InputFingerprint == "" {
			return graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "selected contract companion inputs are unavailable")
		}
		if version == "" {
			version, fingerprint = view.Attachment.InputVersion, view.Attachment.InputFingerprint
		} else if version != view.Attachment.InputVersion || fingerprint != view.Attachment.InputFingerprint {
			return graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "contract analyses consulted different selected companion inputs")
		}
	}
	return nil
}

func (binding *contractAnalysisContext) validate(ctx context.Context) error {
	if err := binding.readError(); err != nil {
		return err
	}
	if binding == nil || len(binding.views) == 0 {
		return graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "selected contract analysis is unavailable")
	}
	if err := binding.checkCohort(); err != nil {
		return err
	}
	for _, view := range binding.views {
		if err := view.Validate(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (binding *contractAnalysisContext) loadRegistry(ctx context.Context) (*contracts.Registry, error) {
	binding.once.Do(func() {
		binding.registry = contracts.NewRegistry()
		if len(binding.views) == 0 {
			binding.registryErr = graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "selected contract analysis is unavailable")
			return
		}
		if err := binding.checkCohort(); err != nil {
			binding.registryErr = err
			return
		}
		repos := make([]string, 0, len(binding.views))
		for repo := range binding.views {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			view := binding.views[repo]
			reg, err := contracts.LoadRegistryFromGraphChecked(ctx, view.RegistryReader, contracts.RegistryLoadOptions{RepoPrefix: repo})
			if err != nil {
				binding.registryErr = err
				return
			}
			// nil,nil is a complete empty snapshot, not an unbuilt registry.
			if reg == nil {
				continue
			}
			for _, id := range reg.AllIDs() {
				for _, record := range reg.ByID(id) {
					binding.registry.Add(record)
				}
			}
		}
	})
	if binding.registryErr != nil {
		return nil, binding.registryErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return binding.registry, nil
}

// contractRegistryForContext is the common consumer seam. A routed checkout
// may never borrow the primary registry when no selected analysis was bound.
// Legacy unrouted callers remain unchanged until the producer is enabled.
func (s *Server) contractRegistryForContext(ctx context.Context) (*contracts.Registry, error) {
	if binding := contractAnalysisFromContext(ctx); binding != nil {
		return binding.loadRegistry(ctx)
	}
	if requestViewFromContext(ctx).routed() {
		return nil, graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "contract analysis is not bound to the selected view")
	}
	return s.effectiveContractRegistry(), nil
}

// Optional consumers omit an unbound contract section. The middleware reports
// its incomplete status without loading a primary registry or scheduling work.
func (s *Server) optionalContractRegistryForContext(ctx context.Context) *contracts.Registry {
	registry, err := s.contractRegistryForContext(ctx)
	if err != nil {
		return nil
	}
	return registry
}

func (s *Server) contractReaderForContext(ctx context.Context) (graph.Reader, error) {
	if binding := contractAnalysisFromContext(ctx); binding != nil {
		return binding.analysisReader(ctx)
	}
	return s.readerFor(ctx), nil
}

func (binding *contractAnalysisContext) shapeLookup(ctx context.Context) contracts.ShapeLookup {
	repos := make([]string, 0, len(binding.views))
	for repo := range binding.views {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return func(id string) *contracts.Shape {
		var found *contracts.Shape
		for _, repo := range repos {
			node, err := binding.views[repo].ShapeNode(ctx, id)
			if err != nil {
				binding.recordReadError(err)
				return nil
			}
			if node == nil || node.Meta == nil {
				continue
			}
			var shape *contracts.Shape
			switch value := node.Meta["shape"].(type) {
			case *contracts.Shape:
				shape = value
			case contracts.Shape:
				shape = &value
			}
			if shape == nil {
				continue
			}
			if found != nil && !reflect.DeepEqual(found, shape) {
				binding.recordReadError(graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "schema identity is ambiguous across selected contract analyses"))
				return nil
			}
			found = shape
		}
		return found
	}
}

func (binding *contractAnalysisContext) analysisNodes(ctx context.Context, kinds []graph.NodeKind) ([]*graph.Node, error) {
	byID := make(map[string]*graph.Node)
	for _, view := range binding.views {
		var nodes []*graph.Node
		var err error
		if kinds == nil {
			nodes, err = view.NodesContext(ctx, graph.ContractProjectionRowLimit)
		} else {
			nodes, err = view.NodesByKindsContext(ctx, kinds)
		}
		if err != nil {
			binding.recordReadError(err)
			return nil, err
		}
		for _, node := range nodes {
			if old := byID[node.ID]; old != nil && !reflect.DeepEqual(old, node) {
				err := graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "contract node identity is ambiguous across selected analyses")
				binding.recordReadError(err)
				return nil, err
			}
			byID[node.ID] = node
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	nodes := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, byID[id])
	}
	return nodes, nil
}

func (binding *contractAnalysisContext) analysisOutEdges(ctx context.Context, ids []string) (map[string][]*graph.Edge, error) {
	combined := make(map[string][]*graph.Edge)
	seen := make(map[graph.EdgeIdentity]*graph.Edge)
	for _, view := range binding.views {
		edges, err := view.OutEdgesContext(ctx, ids, graph.ContractProjectionRowLimit)
		if err != nil {
			binding.recordReadError(err)
			return nil, err
		}
		for id, rows := range edges {
			for _, edge := range rows {
				identity := graph.EdgeIdentityFor(edge)
				if old := seen[identity]; old != nil {
					if !reflect.DeepEqual(old, edge) {
						err := graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "contract edge identity is ambiguous across selected analyses")
						binding.recordReadError(err)
						return nil, err
					}
					continue
				}
				seen[identity] = edge
				combined[id] = append(combined[id], edge)
			}
		}
	}
	return combined, nil
}

// analysisReader builds a request-local graph solely from checked analysis
// rows. It never embeds the core reader's legacy contract edges. Current core
// source identities are added only as evidence for owned contract endpoints.
func (binding *contractAnalysisContext) analysisReader(ctx context.Context) (graph.Reader, error) {
	binding.graphOnce.Do(func() {
		nodes, err := binding.analysisNodes(ctx, nil)
		if err != nil {
			binding.graphErr = err
			return
		}
		byID := make(map[string]*graph.Node, len(nodes))
		for _, node := range nodes {
			byID[node.ID] = node
		}
		for repo, view := range binding.views {
			projection, err := view.RegistryReader.(graph.ContractRepoProjectionReader).LoadContractRepoProjectionContext(ctx, repo)
			if err != nil {
				binding.graphErr = err
				return
			}
			for id, node := range projection.SourceNodes {
				byID[id] = node
			}
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		edges, err := binding.analysisOutEdges(ctx, ids)
		if err != nil {
			binding.graphErr = err
			return
		}
		var missing []string
		missingSet := make(map[string]bool)
		for _, rows := range edges {
			for _, edge := range rows {
				if byID[edge.To] == nil && !missingSet[edge.To] {
					missingSet[edge.To] = true
					missing = append(missing, edge.To)
				}
			}
		}
		for _, view := range binding.views {
			physical, err := view.Layer.LayerContractIDProjectionContext(ctx, missing)
			if err != nil {
				binding.graphErr = err
				return
			}
			for id, node := range physical.SourceNodes {
				byID[id] = node
			}
		}
		for _, id := range missing {
			if byID[id] == nil {
				binding.graphErr = graph.ErrContractProjectionIncomplete
				return
			}
		}
		analysis := graph.NewOverlayLayer()
		for _, node := range byID {
			analysis.AddNode(node.FilePath, node)
		}
		for _, rows := range edges {
			for _, edge := range rows {
				analysis.AddEdge(edge)
			}
		}
		// The overlay's iterators enumerate layer rows only with a non-nil
		// baseline. An explicit empty view keeps analysis isolated while
		// allowing bridge/framework and candidate enumeration.
		binding.graph = graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(nil, nil), analysis)
	})
	if binding.graphErr != nil {
		binding.recordReadError(binding.graphErr)
		return nil, binding.graphErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return binding.graph, nil
}

// scopeEvidence retains canonical identity while applying selectors to actual
// selected owners. A shared global canonical row need not carry each owner's
// repository or source path in its own scalar fields.
func (binding *contractAnalysisContext) scopeEvidence(ctx context.Context, node *graph.Node) []*graph.Node {
	if node == nil {
		return nil
	}
	if node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge && node.Kind != graph.KindConfigKey {
		return []*graph.Node{node}
	}
	id := node.ID
	if node.Kind == graph.KindContractBridge {
		id, _ = node.Meta["contract_id"].(string)
	}
	registry, err := binding.loadRegistry(ctx)
	if err != nil {
		binding.recordReadError(err)
		return nil
	}
	ids := []string{id}
	if node.Kind == graph.KindContractBridge {
		// RPC/service bridges may group several distinct literal contract
		// IDs; their actual selected links are the membership authority.
		reader, err := binding.analysisReader(ctx)
		if err != nil {
			binding.recordReadError(err)
			return nil
		}
		for _, edge := range reader.GetOutEdges(node.ID) {
			if edge.Kind == graph.EdgeBridges {
				ids = append(ids, edge.To)
			}
		}
	}
	var evidence []*graph.Node
	for _, id := range ids {
		for _, owner := range registry.ByID(id) {
			copyNode := *node
			copyNode.RepoPrefix, copyNode.FilePath = owner.RepoPrefix, owner.FilePath
			copyNode.WorkspaceID, copyNode.ProjectID = owner.WorkspaceID, owner.ProjectID
			evidence = append(evidence, &copyNode)
		}
	}
	if node.Kind == graph.KindConfigKey {
		reader, err := binding.analysisReader(ctx)
		if err != nil {
			binding.recordReadError(err)
			return nil
		}
		for _, edge := range reader.GetInEdges(node.ID) {
			if edge.Kind != graph.EdgeReadsConfig {
				continue
			}
			if source := reader.GetNode(edge.From); source != nil {
				copyNode := *node
				copyNode.RepoPrefix, copyNode.FilePath = source.RepoPrefix, source.FilePath
				copyNode.WorkspaceID, copyNode.ProjectID = source.WorkspaceID, source.ProjectID
				evidence = append(evidence, &copyNode)
			}
		}
	}
	return evidence
}

func (s *Server) contractScopeAllowsNode(ctx context.Context, scope ResolvedScope, node *graph.Node) bool {
	binding := contractAnalysisFromContext(ctx)
	status := contractConsumerStatusFromContext(ctx)
	if binding == nil || status == nil || status.mode != contractConsumerRequired {
		return resolvedScopeAllowsNode(scope, node)
	}
	for _, evidence := range binding.scopeEvidence(ctx, node) {
		if resolvedScopeAllowsNode(scope, evidence) {
			return true
		}
	}
	return false
}

func (s *Server) filterContractSubGraph(ctx context.Context, sg *query.SubGraph, allowed map[string]bool) *query.SubGraph {
	binding := contractAnalysisFromContext(ctx)
	status := contractConsumerStatusFromContext(ctx)
	if binding == nil || status == nil || status.mode != contractConsumerRequired || allowed == nil {
		return filterSubGraph(sg, allowed)
	}
	copyGraph := *sg
	copyGraph.Nodes = nil
	copyGraph.Edges = nil
	kept := make(map[string]bool)
	for _, node := range sg.Nodes {
		for _, evidence := range binding.scopeEvidence(ctx, node) {
			if repoNarrowAdmits(allowed, evidence.RepoPrefix) {
				copyGraph.Nodes = append(copyGraph.Nodes, node)
				kept[node.ID] = true
				break
			}
		}
	}
	for _, edge := range sg.Edges {
		if kept[edge.From] || kept[edge.To] {
			copyGraph.Edges = append(copyGraph.Edges, edge)
		}
	}
	copyGraph.TotalNodes = len(copyGraph.Nodes)
	if len(sg.Edges) > 0 || sg.TotalEdges == 0 {
		copyGraph.TotalEdges = len(copyGraph.Edges)
	}
	return &copyGraph
}
