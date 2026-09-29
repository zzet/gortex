package indexer

import (
	"context"
	"sort"
)

// editDeltaDependents lists, repository-relative and sorted, the unchanged
// files that may bind a name the change now defines, and which the delta
// therefore re-derives from source beside the changed files:
//
//   - the files whose references park on a placeholder of a name the change
//     defines (the sparse closure walk's introduced-name arm);
//   - the importers of a changed file that introduces an importable name — a
//     caller written against a function the edit adds, bound until now to the
//     package's external placeholder (the walk's reverse arm, restricted to
//     the present changed files' own file nodes; a deleted file's importers
//     the engine re-derives itself, deletion_importers.go).
//
// The resolver's incoming leg re-binds such a reference without re-deriving
// its file, but a whole index derives the file's type-resolved facts against
// the new declaration, and the go/types pass enriches only what the
// generation carries. A body or comment edit defines no new name, so it has no
// such dependent. Both arms are bounded by the walk's cap.
//
// The reverse dependents of a declaration whose shape changed (the callers of
// a function whose signature changed) are deliberately not re-derived: the
// per-save engine re-derives a file's own rows but not every derived family
// recorded at it (an inferred implements edge of a type in the file is lost),
// so re-deriving those callers costs rows the incoming leg keeps.
func (b *SparseGenerationBuilder) editDeltaDependents(ctx context.Context, req BuildRequest, plan buildPlan) ([]string, error) {
	present := make(map[string]struct{}, len(plan.indexed))
	for _, rel := range plan.indexed {
		present[rel] = struct{}{}
	}
	deleted := make(map[string]struct{}, len(plan.deleted))
	for _, rel := range plan.deleted {
		deleted[rel] = struct{}{}
	}
	if len(present)+len(deleted) == 0 {
		return nil, nil
	}
	limit, _ := b.builderClosureCap(req)
	walk := &closureWalk{
		b:       b,
		ctx:     ctx,
		req:     req,
		limit:   limit,
		deleted: deleted,
		seeds:   make(map[string]struct{}, len(present)+len(deleted)),
		chosen:  make(map[string]struct{}),
	}
	seeds := make([]string, 0, len(present)+len(deleted))
	for rel := range present {
		seeds = append(seeds, builderGraphPath(req.RepoPrefix, rel))
	}
	for rel := range deleted {
		seeds = append(seeds, builderGraphPath(req.RepoPrefix, rel))
	}
	sort.Strings(seeds)
	for _, graphPath := range seeds {
		walk.seeds[graphPath] = struct{}{}
	}
	dependents := make(map[string]struct{})
	declared := make(map[string]struct{})
	target := walk.collectIntroduced(present, dependents, declared)
	if walk.err != nil {
		return nil, walk.err
	}
	seedNodeIDs, err := builderSemanticSeedNodeIDs(ctx, req, seeds, deleted, target)
	if err != nil {
		return nil, err
	}
	// A present changed file's own node is a reverse seed exactly when the
	// change introduces a name an importer can reach
	// (builderSemanticSeedNodeIDs). A deleted file's importers are the
	// engine's own (deletion_importers.go): it re-derives them in the pass.
	var fileSeeds []string
	for _, id := range seedNodeIDs.reverse {
		rel, owned := builderRelPath(req.RepoPrefix, id)
		if !owned {
			continue
		}
		if _, isPresent := present[rel]; isPresent {
			fileSeeds = append(fileSeeds, id)
		}
	}
	if err := b.collectDependents(ctx, req, fileSeeds, dependents); err != nil {
		return nil, err
	}
	var out []string
	for _, graphPath := range walk.admitAll(dependents) {
		if rel, owned := builderRelPath(req.RepoPrefix, graphPath); owned {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}
