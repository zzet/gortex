package graphview

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// ContractAnalysisInput is captured from the selected view's accepted contract
// inputs. An attachment for the actor alone cannot certify those inputs.
type ContractAnalysisInput struct {
	RepoPrefix, CheckoutID, InputVersion, InputFingerprint string
}

// ContractAnalysisView owns an independent contract-payload lease. Core
// remains the original source/declaration reader; analysis never replaces its
// files, type nodes or readiness. RegistryReader serves checked projections,
// not generic graph traversal. ShapeNode reads analysis-owned schema facts.
type ContractAnalysisView struct {
	Attachment     graph.ContractAttachment
	Core           graph.Reader
	RegistryReader graph.Reader
	Layer          *GenerationLayer
	lease          *Lease
	inputs         *SelectedContractInputs
	closeOnce      sync.Once
}

// SearchBackend reads only this leased analysis payload's native symbol index.
func (view *ContractAnalysisView) SearchBackend() search.Backend {
	return search.NewSymbolSearcherBackend(view.Layer.handle)
}

// OpenSelectedContractAnalysis derives attachment authority from the selected
// core view instead of accepting a request-supplied fingerprint.
func (m *Materializer) OpenSelectedContractAnalysis(ctx context.Context, view *RepoView, repo, checkout string) (*ContractAnalysisView, error) {
	inputs, err := m.CaptureContractInputs(ctx, view, repo, checkout)
	if err != nil {
		return nil, err
	}
	return m.OpenContractAnalysisForInputs(ctx, view, inputs)
}

// OpenContractAnalysisForInputs binds an analysis to checked local and companion
// input captures. Those captures must come from CaptureContractInputs, optionally
// combined by ComposeSelectedContractInputs; request assertions are insufficient.
func (m *Materializer) OpenContractAnalysisForInputs(ctx context.Context, view *RepoView, inputs *SelectedContractInputs) (*ContractAnalysisView, error) {
	if m == nil || inputs == nil || inputs.store != m.Store || len(inputs.sources)+len(inputs.dependencies) == 0 {
		return nil, graph.ErrContractInputVector
	}
	if view != nil && (view.contractStore != m.Store || view.ID.RepoPrefix != inputs.State.RepoPrefix) {
		return nil, graph.ErrContractInputVector
	}
	if err := inputs.Validate(ctx); err != nil {
		return nil, err
	}
	if !inputs.State.Accepted {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "selected contract inputs are still being accepted")
	}
	var core graph.Reader = m.Store.AtGeneration(BaseCorpusGeneration)
	if view != nil {
		core = view.Reader
	}
	analysis, err := m.OpenContractAnalysis(ctx, core, ContractAnalysisInput{RepoPrefix: inputs.State.RepoPrefix, CheckoutID: inputs.State.CheckoutID, InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint})
	if err != nil {
		return nil, err
	}
	if err := inputs.Validate(ctx); err != nil {
		analysis.Close()
		return nil, err
	}
	analysis.inputs = inputs
	analysis.RegistryReader.(*contractRegistryReader).sources = inputs.SourceReaders()
	return analysis, nil
}

// Validate is the consumer's post-read input fence, independent of unrelated
// core graph clocks. A failed fence never certifies a fresh contract answer.
func (v *ContractAnalysisView) Validate(ctx context.Context) error {
	if v == nil || v.inputs == nil {
		return graph.ErrContractInputVector
	}
	if err := v.inputs.Validate(ctx); err != nil {
		return err
	}
	return v.Layer.checkContractInputRevision(ctx)
}

func (v *ContractAnalysisView) Close() {
	if v != nil {
		v.closeOnce.Do(func() { v.lease.Release() })
	}
}

func (v *ContractAnalysisView) ShapeNode(ctx context.Context, id string) (*graph.Node, error) {
	if v == nil || v.Layer == nil {
		return nil, graph.ErrContractProjectionUnsupported
	}
	p, err := v.Layer.LayerContractIDProjectionContext(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	return p.SourceNodes[id], nil
}

// NodesByKindsContext and OutEdgesContext read only the independently leased
// analysis payload. RegistryReader's generic methods deliberately remain core
// methods and must not be used to traverse contract-derived graph structure.
func (v *ContractAnalysisView) NodesByKindsContext(ctx context.Context, kinds []graph.NodeKind) ([]*graph.Node, error) {
	if v == nil || v.Layer == nil {
		return nil, graph.ErrContractProjectionUnsupported
	}
	if err := v.Layer.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	nodes, err := v.Layer.handle.NodesByKindsContext(ctx, kinds)
	if err != nil {
		return nil, err
	}
	if err := v.Layer.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	return nodes, nil
}

// NodesContext reads the complete bounded analysis payload, including source
// endpoints for independently derived configuration and dependency edges.
func (v *ContractAnalysisView) NodesContext(ctx context.Context, limit int) ([]*graph.Node, error) {
	if v == nil || v.Layer == nil {
		return nil, graph.ErrContractProjectionUnsupported
	}
	if err := v.Layer.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	nodes, truncated, err := v.Layer.handle.ContractAnalysisNodesContext(ctx, limit)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "contract node projection exceeds the requested bound")
	}
	if err := v.Layer.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (v *ContractAnalysisView) OutEdgesContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, error) {
	if v == nil || v.Layer == nil {
		return nil, graph.ErrContractProjectionUnsupported
	}
	if err := v.Layer.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	edges, truncated, err := v.Layer.handle.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "contract edge projection exceeds the requested bound")
	}
	if err := v.Layer.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	return edges, nil
}

// OpenContractAnalysis pins before rechecking the head. A replaced attachment
// cannot be retired between selection and lease acquisition; bounded reselect
// never changes the caller's accepted-input identity or deadline.
func (m *Materializer) OpenContractAnalysis(ctx context.Context, core graph.Reader, input ContractAnalysisInput) (*ContractAnalysisView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.Store == nil || m.Catalog == nil || m.Leases == nil || core == nil || input.InputVersion == "" || input.InputFingerprint == "" {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "selected contract inputs are not yet certified")
	}
	reader := m.contractAttachmentReader
	if reader == nil {
		reader, _ = any(m.Store).(graph.ContractAttachmentReader)
	}
	if reader == nil {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "contract analysis attachment is unavailable")
	}
	key := graph.ContractAttachmentKey{RepoPrefix: input.RepoPrefix, CheckoutID: input.CheckoutID, InputVersion: input.InputVersion, InputFingerprint: input.InputFingerprint}
	for attempt := 0; attempt < 2; attempt++ {
		attachment, err := reader.GetContractAttachmentContext(ctx, key)
		if err != nil {
			return nil, err
		}
		if attachment == nil || attachment.PayloadGeneration <= 0 || attachment.RepoPrefix != input.RepoPrefix || attachment.CheckoutID != input.CheckoutID || attachment.InputVersion != input.InputVersion || attachment.InputFingerprint != input.InputFingerprint {
			return nil, NewViewError(CodeRequiredCapabilityIncomplete, "contract analysis is pending for the selected inputs")
		}
		lease := m.Leases.Acquire(attachment.PayloadGeneration)
		current, err := reader.GetContractAttachmentContext(ctx, key)
		if err != nil {
			lease.Release()
			return nil, err
		}
		if !reflect.DeepEqual(attachment, current) {
			lease.Release()
			continue
		}
		handle, layer, _, err := m.openGeneration(ctx, attachment.PayloadGeneration)
		if err != nil {
			lease.Release()
			return nil, err
		}
		states, err := handle.ProducerStates()
		if err != nil {
			lease.Release()
			return nil, err
		}
		complete := false
		for _, state := range states {
			if state.Producer == string(CapContracts) && state.State == store_sqlite.ProducerStateComplete {
				complete = true
			}
		}
		if !complete {
			lease.Release()
			return nil, NewViewError(CodeRequiredCapabilityIncomplete, "contract analysis payload is not certified complete")
		}
		if err := ctx.Err(); err != nil {
			lease.Release()
			return nil, err
		}
		// An analysis snapshot is a separate tier, never a whole-file overlay
		// that could suppress current declarations at the same source paths.
		if len(layer.covered) != 0 || len(layer.contextPaths) != 0 {
			lease.Release()
			return nil, fmt.Errorf("contract analysis generation %d contains core file ownership masks", attachment.PayloadGeneration)
		}
		copyAttachment := *attachment
		copyAttachment.CompletedTokens = slices.Clone(attachment.CompletedTokens)
		return &ContractAnalysisView{Attachment: copyAttachment, Core: core, Layer: layer, RegistryReader: &contractRegistryReader{Reader: core, layer: layer}, lease: lease}, nil
	}
	return nil, NewViewError(CodeViewBuilding, "contract attachment changed while selecting analysis")
}

// contractRegistryReader deliberately starts each projection from analysis,
// not old core contract rows. Core contributes only checked source identities;
// same-ID canonical ownership and shapes stay in the complete analysis tier.
type contractRegistryReader struct {
	graph.Reader
	layer   *GenerationLayer
	sources map[string]graph.Reader
}

var _ graph.ContractRepoProjectionReader = (*contractRegistryReader)(nil)
var _ graph.ContractFileProjectionReader = (*contractRegistryReader)(nil)
var _ graph.OverlayLayerContractRepoProjectionReader = (*contractRegistryReader)(nil)
var _ graph.OverlayLayerContractProjectionReader = (*contractRegistryReader)(nil)

func (r *contractRegistryReader) LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return r.layer.LayerContractRepoProjectionContext(ctx, repo)
}
func (r *contractRegistryReader) LayerContractFileProjectionContext(ctx context.Context, repo string, paths []string) (graph.ContractFileProjection, error) {
	return r.layer.LayerContractFileProjectionContext(ctx, repo, paths)
}
func (r *contractRegistryReader) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := r.layer.LayerContractIDProjectionContext(ctx, ids)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	var missing []string
	for _, id := range ids {
		if p.SourceNodes[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		if p.SourceNodes == nil {
			p.SourceNodes = make(map[string]*graph.Node)
		}
		if r.sources == nil {
			core, err := graph.ContractSourceNodesContext(ctx, r.Reader, missing)
			if err != nil {
				return graph.ContractFileProjection{}, err
			}
			for id, node := range core {
				if node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge {
					p.SourceNodes[id] = node
				}
			}
		} else {
			for repo, reader := range r.sources {
				core, err := graph.ContractSourceNodesContext(ctx, reader, missing)
				if err != nil {
					return graph.ContractFileProjection{}, err
				}
				for id, node := range core {
					if node.RepoPrefix == repo && node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge {
						if old := p.SourceNodes[id]; old != nil && !reflect.DeepEqual(old, node) {
							return graph.ContractFileProjection{}, graph.ErrContractProjectionIncomplete
						}
						p.SourceNodes[id] = node
					}
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return p, nil
}
func (r *contractRegistryReader) LoadContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractRepoProjection(ctx, r, repo)
}
func (r *contractRegistryReader) LoadContractFileProjectionContext(ctx context.Context, repo string, paths []string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractFileProjection(ctx, r, repo, paths)
}
func (r *contractRegistryReader) LoadContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractIDProjection(ctx, r, ids)
}
