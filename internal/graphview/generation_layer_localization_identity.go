package graphview

import (
	"context"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.OverlayLocalizationIdentityReader = (*GenerationLayer)(nil)
	_ graph.OverlayDetachedFileSummaryReader  = (*GenerationLayer)(nil)
)

func (l *GenerationLayer) DetachedFileNodeSummaries(filePath string) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for _, index := range l.detachedFileIndexes[filePath] {
			owned := l.detachedNodes[index]
			if !yield(&owned) {
				return
			}
		}
	}
}

func (l *GenerationLayer) LocalizationIdentityNodesContext(ctx context.Context, ids []string, includeTestFlag bool) ([]*graph.Node, error) {
	for _, id := range ids {
		if _, carried := l.detachedIDs[id]; !carried {
			return nil, fmt.Errorf("graphview: localization identity %q is not a carried row", id)
		}
	}
	return l.handle.NodeIdentityLocalizationSummariesContext(ctx, ids, includeTestFlag)
}
