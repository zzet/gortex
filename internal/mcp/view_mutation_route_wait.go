package mcp

import (
	"context"
	"github.com/zzet/gortex/internal/indexer"
)

// withMutationPublicationStamps arms the request's publication stamp
// collector for a source mutation, so every step between the tool call
// arriving and its disk commit is timed onto the edit's publication record
// (openMutationPhases absorbs them). Other tools pay nothing.
func (s *Server) withMutationPublicationStamps(ctx context.Context, tool string) context.Context {
	if s == nil || s.facades == nil || !s.facades.mutatesSource(tool) {
		return ctx
	}
	return indexer.WithPublicationStamps(ctx)
}
