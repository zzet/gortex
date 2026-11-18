package indexer

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// This checkpoint contains only owned scalar clocks and bounded sparse data.
// No reader/Store/lease, generation-zero answer, or complete base census is
// retained. A current job must mint its own independently leased counterpart.
type contractPositiveNameProjection struct {
	repos   map[string]contractPositiveNameRepo
	clocks  []store_sqlite.ContractProjectionCursor
	bytes   int64
	budget  int64
	answers map[string]contractCertifiedNameAnswer
}
type contractPositiveNameRepo struct {
	prefix    []store_sqlite.ContractProjectionCursor
	suffix    contractSparseDeclarationShape
	baseNames map[string][]store_sqlite.ContractProjectionDeclaration
}

func contractPositiveChain(reader graph.Reader, depth int) ([]*store_sqlite.Store, bool) {
	if depth > graphview.MaxGenerationAncestryDepth+graphview.MaxDirtyChainDepth {
		return nil, false
	}
	switch v := reader.(type) {
	case *store_sqlite.Store:
		return []*store_sqlite.Store{v}, v != nil && v.ViewGeneration() > 0
	case *graph.OverlaidView:
		layer, ok := v.LayerReader().(*graphview.GenerationLayer)
		if !ok || layer == nil || layer.ProjectionHandle() == nil || layer.ProjectionHandle().ViewGeneration() <= 0 {
			return nil, false
		}
		chain, ok := contractPositiveChain(v.Base(), depth+1)
		if !ok {
			return nil, false
		}
		return append(chain, layer.ProjectionHandle()), true
	}
	return nil, false
}

// capturePositiveNames reads sparse dirty suffixes only. True full-base or
// opaque selection changes remain on the ordinary physical replay path.
func capturePositiveNames(ctx context.Context, readers map[string]graph.Reader, handles map[string][]*store_sqlite.Store, budget int64) (*contractPositiveNameProjection, error) {
	if budget < 4096 || budget > contractDeltaCacheBudget {
		return nil, nil
	}
	if len(readers) == 0 || len(readers) > 64 {
		return nil, nil
	}
	out := &contractPositiveNameProjection{repos: map[string]contractPositiveNameRepo{}, budget: budget, bytes: 4096 * int64(len(readers)+1)}
	if out.bytes > budget {
		return nil, nil
	}
	seen := map[int64]bool{}
	for _, repo := range slices.Sorted(maps.Keys(readers)) {
		reader := readers[repo]
		for _, h := range handles[repo] {
			if h == nil || h.ViewGeneration() <= 0 {
				return nil, nil
			}
		}
		if !contractInventoryCompositionCovered(reader, handles[repo]) {
			return nil, nil
		}
		chain, ok := contractPositiveChain(reader, 0)
		if !ok {
			return nil, nil
		}
		var prefix []store_sqlite.ContractProjectionCursor
		var suffix []store_sqlite.ContractProjectionFootprint
		dirty := false
		for _, handle := range chain {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cursor, err := handle.CaptureContractProjectionClock(ctx)
			if err != nil {
				return nil, err
			}
			row, found, err := handle.Catalog().GetViewGeneration(ctx, handle.ViewGeneration())
			if err != nil {
				return nil, err
			}
			if !found || row.State != store_sqlite.ViewGenerationReady {
				return nil, nil
			}
			if row.GenerationKind != DirtyLayerGenerationKind {
				if dirty {
					return nil, nil
				}
				prefix = append(prefix, cursor)
			} else {
				dirty = true
				footprint, err := handle.ContractProjectionFootprintContext(ctx, budget-out.bytes)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, nil
				}
				if footprint.Cursor != cursor || !contractSparseDeclarationOwnership(footprint) {
					return nil, nil
				}
				out.bytes += footprint.RetainedBytes + 512*int64(len(footprint.Declarations)+len(footprint.Files)+len(footprint.Identities))
				if out.bytes > budget {
					return nil, nil
				}
				suffix = append(suffix, footprint)
			}
			if !seen[handle.ViewGeneration()] {
				out.clocks = append(out.clocks, cursor)
				seen[handle.ViewGeneration()] = true
			}
		}
		if len(prefix) == 0 {
			return nil, nil
		}
		out.repos[repo] = contractPositiveNameRepo{prefix: prefix, suffix: sparseDeclarationShape(suffix)}
	}
	// Ownership of the bounded physical strings is transferred to the shapes;
	// old full edge/binding/producers arrays do not survive this function.
	out.budget = budget
	out.bytes = contractRetainedSize(reflect.ValueOf(out))
	if out.bytes > budget {
		return nil, nil
	}
	return out, nil
}

func contractSparseDeclarationOwnership(f store_sqlite.ContractProjectionFootprint) bool {
	files := map[string]bool{}
	contextOnly := map[string]bool{}
	ids := map[string]bool{}
	rows := map[string]bool{}
	for _, mask := range f.Files {
		if mask.Mode == store_sqlite.OwnershipReplace || mask.Mode == store_sqlite.OwnershipDelete {
			files[mask.FilePath] = true
		}
		if mask.Mode == store_sqlite.OwnershipContext {
			contextOnly[mask.FilePath] = true
		}
	}
	for _, row := range f.Declarations {
		rows[row.ID] = true
	}
	for _, mask := range f.Identities {
		if mask.Kind == store_sqlite.NodeIdentityMaskReplace && !rows[mask.NodeID] {
			return false
		}
		ids[mask.NodeID] = true
	}
	for _, row := range f.Declarations {
		if row.ID == "" {
			return false
		}
		if !contextOnly[row.FilePath] && !sparseFileOwnsID(files, row.ID) && !ids[row.ID] {
			return false
		}
	}
	return true
}

func (p *contractPositiveNameProjection) sameName(other *contractPositiveNameProjection, name string) bool {
	if p == nil || other == nil || len(p.repos) != len(other.repos) {
		return false
	}
	for repo, prior := range p.repos {
		current, ok := other.repos[repo]
		if !ok || !slices.Equal(prior.prefix, current.prefix) || !positiveNameRepoEqual(prior, current, name) {
			return false
		}
	}
	return true
}

func positiveNameClocks(ctx context.Context, readers map[string]graph.Reader) ([]store_sqlite.ContractProjectionCursor, error) {
	var out []store_sqlite.ContractProjectionCursor
	seen := map[int64]bool{}
	for _, repo := range slices.Sorted(maps.Keys(readers)) {
		chain, ok := contractPositiveChain(readers[repo], 0)
		if !ok {
			return nil, nil
		}
		for _, h := range chain {
			if !seen[h.ViewGeneration()] {
				cursor, err := h.CaptureContractProjectionClock(ctx)
				if err != nil {
					return nil, err
				}
				out = append(out, cursor)
				seen[h.ViewGeneration()] = true
			}
		}
	}
	return out, ctx.Err()
}
func (c *contractConvergence) mayCarryPositiveName(read contractRecordedRead, forced ...bool) bool {
	s := c.req.delta
	if s == nil || !s.adopted || s.cache == nil || s.currentNames == nil || c.ctx.Err() != nil || read.kind != contractReadConstCandidates || len(read.args) != 2 {
		return false
	}
	// Unknown, membership and dependency-only debt deliberately keep whole
	// physical replay. The certificate does not reinterpret these obligations.
	if len(forced) == 0 || forced[0] {
		for _, work := range c.coverage.work {
			if work.Scope.Unknown || work.Scope.Deleted || len(work.Scope.Causes) == 0 {
				return false
			}
			for _, cause := range work.Scope.Causes {
				switch cause {
				case "local_boundary_changed", "handler_inputs_changed", "mount_inputs_changed", "boundary_lookup_changed":
				default:
					return false
				}
			}
		}
	}
	proof, certified := s.cache.names.certified(read.args)
	if !certified || proof.digest != read.digest || !s.cache.names.sameName(s.currentNames, read.args[0]) {
		return false
	}
	if c.snap.positiveNameCurrent == nil || !c.snap.positiveNameCurrent(c.ctx, s.currentNames) {
		return false
	}
	c.report.NameProjectionCarries++
	return true
}

type contractCertifiedNameAnswer struct {
	name, digest string
	values       []contracts.EndpointConstantCandidate
	complete     bool
}

func contractNameQuestionKey(args []string) string {
	encoded, _ := json.Marshal(args)
	return contractInputHash(encoded)
}
func (p *contractPositiveNameProjection) certified(args []string) (contractCertifiedNameAnswer, bool) {
	if p == nil || len(args) != 2 {
		return contractCertifiedNameAnswer{}, false
	}
	answer, ok := p.answers[contractNameQuestionKey(args)]
	return answer, ok
}
func (p *contractPositiveNameProjection) recordName(key, name, digest string, values ...[]contracts.EndpointConstantCandidate) {
	if p == nil || len(p.answers) >= graph.ContractProjectionRowLimit {
		return
	}
	charge := int64(512 + 2*(len(key)+len(name)+len(digest)))
	if len(values) > 0 {
		for _, value := range values[0] {
			charge += 128 + 2*int64(len(value.ID)+len(value.FilePath))
		}
	}
	if old, exists := p.answers[key]; exists {
		if old.name != name || old.digest != digest {
			delete(p.answers, key)
		}
		return
	}
	if charge > p.budget-p.bytes {
		return
	}
	if p.answers == nil {
		p.answers = map[string]contractCertifiedNameAnswer{}
	}
	answer := contractCertifiedNameAnswer{name: strings.Clone(name), digest: strings.Clone(digest)}
	if len(values) > 0 {
		answer.complete = true
		if values[0] != nil {
			answer.values = make([]contracts.EndpointConstantCandidate, len(values[0]))
		}
		for i, v := range values[0] {
			answer.values[i] = contracts.EndpointConstantCandidate{ID: strings.Clone(v.ID), FilePath: strings.Clone(v.FilePath)}
		}
	}
	p.answers[strings.Clone(key)] = answer
	p.bytes += charge
}
func (c *contractConvergence) observePositiveName(name, repo string, out []contracts.EndpointConstantCandidate) {
	if c.req.delta == nil || c.req.delta.currentNames == nil {
		return
	}
	p := c.req.delta.currentNames
	if int64(len(name)+len(repo)) > p.budget-p.bytes {
		return
	}
	digest, ok := contractAnswerDigest(out)
	if !ok {
		return
	}
	p.recordName(contractNameQuestionKey([]string{name, repo}), name, digest, out)
}
func (s *contractDeltaSession) completeNameProjection(c *contractConvergence) *contractPositiveNameProjection {
	p := s.currentNames
	if p == nil || c.snap.positiveNameCurrent == nil || !c.snap.positiveNameCurrent(c.ctx, p) {
		return nil
	}
	if s.adopted && s.cache != nil && s.cache.names != nil {
		for key, answer := range s.cache.names.answers {
			if s.cache.names.sameName(p, answer.name) {
				if answer.complete {
					p.recordName(key, answer.name, answer.digest, answer.values)
				} else {
					p.recordName(key, answer.name, answer.digest)
				}
			}
		}
	}
	if c.snap.positiveNameComplete == nil || !c.snap.positiveNameComplete(c.ctx, p, s.cache) {
		return nil
	}
	return p
}

func positiveNameRepoEqual(old, current contractPositiveNameRepo, name string) bool {
	if old.suffix.sameNameProjection(current.suffix, name) {
		return true
	}
	prefix, complete := old.baseNames[name]
	if !complete {
		return false
	}
	oldRows := positiveNameIterator{suffix: old.suffix.names[name], prefix: prefix, shape: old.suffix}
	newRows := positiveNameIterator{suffix: current.suffix.names[name], prefix: prefix, shape: current.suffix}
	for {
		a, aok := oldRows.next()
		b, bok := newRows.next()
		if aok != bok {
			return false
		}
		if !aok {
			return true
		}
		if a != b {
			return false
		}
	}
}

type positiveNameIterator struct {
	suffix, prefix []store_sqlite.ContractProjectionDeclaration
	shape          contractSparseDeclarationShape
	upper, lower   int
}

func (i *positiveNameIterator) next() (store_sqlite.ContractProjectionDeclaration, bool) {
	if i.upper < len(i.suffix) {
		row := i.suffix[i.upper]
		i.upper++
		return row, true
	}
	for i.lower < len(i.prefix) {
		row := i.prefix[i.lower]
		i.lower++
		if !sparseFileOwnsID(i.shape.files, row.ID) && !i.shape.ids[row.ID] {
			return row, true
		}
	}
	return store_sqlite.ContractProjectionDeclaration{}, false
}

func positivePrefixReader(ctx context.Context, reader graph.Reader) (graph.Reader, error) {
	for depth := 0; depth <= graphview.MaxGenerationAncestryDepth+graphview.MaxDirtyChainDepth; depth++ {
		overlay, ok := reader.(*graph.OverlaidView)
		if !ok {
			return reader, nil
		}
		layer, ok := overlay.LayerReader().(*graphview.GenerationLayer)
		if !ok || layer == nil {
			return nil, graph.ErrContractProjectionIncomplete
		}
		row, found, err := layer.ProjectionHandle().Catalog().GetViewGeneration(ctx, layer.ProjectionHandle().ViewGeneration())
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, graph.ErrContractProjectionIncomplete
		}
		if row.GenerationKind != DirtyLayerGenerationKind {
			return reader, nil
		}
		reader = overlay.Base()
	}
	return nil, graph.ErrContractProjectionLimit
}
func completePositivePrefixNames(ctx context.Context, readers map[string]graph.Reader, p *contractPositiveNameProjection, old *contractPositiveNameProjection) bool {
	if p == nil {
		return false
	}
	names := map[string]bool{}
	for _, answer := range p.answers {
		names[answer.name] = true
	}
	for repo, current := range p.repos {
		current.baseNames = map[string][]store_sqlite.ContractProjectionDeclaration{}
		var missing []string
		for name := range names {
			if old != nil {
				if prior, ok := old.repos[repo]; ok && slices.Equal(prior.prefix, current.prefix) {
					if rows, present := prior.baseNames[name]; present {
						// Owned immutable rows may share backing across exclusive actor checkpoints.
						// The new checkpoint is still charged for every retained byte.
						charge := int64(256+2*len(name)) + contractRetainedSize(reflect.ValueOf(rows))
						if charge > p.budget-p.bytes {
							return false
						}
						p.bytes += charge
						current.baseNames[strings.Clone(name)] = rows
						continue
					}
				}
			}
			charge := int64(256 + 2*len(name))
			if charge > p.budget-p.bytes {
				return false
			}
			p.bytes += charge
			current.baseNames[strings.Clone(name)] = nil
			missing = append(missing, name)
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			prefix, err := positivePrefixReader(ctx, readers[repo])
			if err != nil || prefix == nil {
				return false
			}
			complete := true
			count := 0
			err = graph.VisitNodeDeclarationsByNamesContext(ctx, prefix, missing, func(node *graph.Node) bool {
				count++
				if node == nil || count > graph.ContractProjectionRowLimit {
					complete = false
					return false
				}
				if _, requested := current.baseNames[node.Name]; !requested {
					complete = false
					return false
				}
				charge := int64(512 + 2*(len(node.ID)+len(node.Name)+len(node.Kind)+len(node.FilePath)+len(node.RepoPrefix)))
				if charge > p.budget-p.bytes {
					complete = false
					return false
				}
				p.bytes += charge
				row := store_sqlite.ContractProjectionDeclaration{ID: strings.Clone(node.ID), Name: strings.Clone(node.Name), Kind: strings.Clone(string(node.Kind)), FilePath: strings.Clone(node.FilePath), RepoPrefix: strings.Clone(node.RepoPrefix)}
				current.baseNames[node.Name] = append(current.baseNames[node.Name], row)
				return true
			})
			if err != nil || !complete {
				return false
			}
		}
		p.repos[repo] = current
	}
	clocks, err := positiveNameClocks(ctx, readers)
	return err == nil && slices.Equal(clocks, p.clocks)
}

// This door is enabled only during fresh accepted extraction, never forced
// ledger replay. It supplies a current-projection-proven typed value while the
// caller still records the ordinary read, owners and current source/R0/R1 proof.
func (c *contractConvergence) positiveCandidateValue(name, repo string) ([]contracts.EndpointConstantCandidate, bool) {
	if len(name)+len(repo) > 1<<20 {
		return nil, false
	}
	s := c.req.delta
	if s == nil || !s.adopted || s.cache == nil || s.currentNames == nil || c.ctx.Err() != nil {
		return nil, false
	}
	answer, ok := s.cache.names.certified([]string{name, repo})
	if !ok || !answer.complete || !s.cache.names.sameName(s.currentNames, name) || c.snap.positiveNameCurrent == nil || !c.snap.positiveNameCurrent(c.ctx, s.currentNames) {
		return nil, false
	}
	var out []contracts.EndpointConstantCandidate
	if answer.values != nil {
		out = make([]contracts.EndpointConstantCandidate, len(answer.values))
	}
	copy(out, answer.values)
	c.report.NameProjectionValueHits++
	return out, true
}
