package mcp

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
)

func (s *Server) noteAcknowledgementParser(ctx context.Context, scope string) {
	if a := editAcknowledgementFrom(ctx, s); a != nil {
		a.mu.Lock()
		if a.parserScope == "" {
			a.parserScope = scope
		} else if a.parserScope != scope {
			a.parserScope = "mixed_observed_parser_paths"
		}
		a.mu.Unlock()
	}
}

// Observe only explicit parser calls. go/format's internal parse+format work
// remains unclassified, rather than reporting the combined call as pure parse.
func parseAcknowledgedGoFile(a *editAcknowledgement, fset *token.FileSet, filename string, source []byte, mode parser.Mode) (*ast.File, error) {
	lap := beginEditAcknowledgementLap(a, editAckParseGate)
	defer lap.finish()
	return parser.ParseFile(fset, filename, source, mode)
}
