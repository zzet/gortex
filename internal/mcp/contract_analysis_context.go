package mcp

import (
	"context"
	"reflect"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graphview"
)

type contractAnalysisContextKey struct{}

// contractAnalysisContext binds independently leased, complete analysis to one
// RPC. It never replaces readerFor's core source/declaration reader.
type contractAnalysisContext struct {
	views       map[string]*graphview.ContractAnalysisView
	once        sync.Once
	registry    *contracts.Registry
	registryErr error
	mu          sync.Mutex
	readErr     error
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

func (binding *contractAnalysisContext) loadRegistry(ctx context.Context) (*contracts.Registry, error) {
	binding.once.Do(func() {
		binding.registry = contracts.NewRegistry()
		if len(binding.views) == 0 {
			binding.registryErr = graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "selected contract analysis is unavailable")
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
