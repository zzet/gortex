package indexer

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

type refFactsByTargetsContextReader interface {
	LoadRefFactsByTargetsContext(context.Context, string, []string) (map[string][]graph.RefFact, error)
}

func loadRefFactsByTargetsContext(
	ctx context.Context,
	reader graph.RefFactsReader,
	repoPrefix string,
	targetIDs []string,
) (map[string][]graph.RefFact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if contextual, ok := reader.(refFactsByTargetsContextReader); ok {
		out, err := contextual.LoadRefFactsByTargetsContext(ctx, repoPrefix, targetIDs)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return out, err
	}
	out, err := reader.LoadRefFactsByTargets(repoPrefix, targetIDs)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return out, err
}

func (a ancestryRefFacts) LoadRefFactsByTargetsContext(
	ctx context.Context, repoPrefix string, targetIDs []string,
) (map[string][]graph.RefFact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[string][]graph.RefFact{}
	seen := map[refFactIdentity]struct{}{}
	var firstErr error
	for _, handle := range a.handles {
		byFile, err := loadRefFactsByTargetsContext(ctx, handle, repoPrefix, targetIDs)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for file, facts := range byFile {
			for _, fact := range facts {
				key := identifyRefFact(fact)
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				out[file] = append(out[file], fact)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, firstErr
}

func (b commitLayerBase) LoadRefFactsByTargetsContext(
	ctx context.Context, repoPrefix string, targetIDs []string,
) (map[string][]graph.RefFact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(b.facts.handles) > 0 {
		return b.facts.LoadRefFactsByTargetsContext(ctx, repoPrefix, targetIDs)
	}
	if b.corpus == nil {
		return map[string][]graph.RefFact{}, nil
	}
	return loadRefFactsByTargetsContext(ctx, b.corpus, repoPrefix, targetIDs)
}
