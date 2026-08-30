package mcp

import (
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// validateRequestBeforeView performs bounded-input admission before view
// resolution. A ref selector may start a cold extraction, so validations that
// are cheaper than that extraction must not live only in the leaf handler.
//
// The leaf handlers keep their own checks as defense in depth for direct calls
// in tests and internal consumers. This seam protects the request path that can
// materialize a worktree/ref view before reaching those handlers.
func (s *Server) validateRequestBeforeView(req *mcp.CallToolRequest) error {
	if req == nil {
		return nil
	}
	consumer := req.Params.Name
	if isFacadeToolName(consumer) {
		spec, ok := s.viewFacadeOperation(req)
		if !ok {
			return nil
		}
		consumer = spec.Legacy
	}

	switch consumer {
	case "read_file", "get_editing_context":
		_, err := parseFidelityGlobs(requestStringArgument(req, "fidelity_globs"))
		if err != nil {
			return fmt.Errorf("%s: %w", consumer, err)
		}
	case "find_files":
		glob := strings.TrimSpace(requestStringArgument(req, "glob"))
		compiled := compileGlob(glob)
		if compiled.tooComplex() {
			return fmt.Errorf(
				"find_files: `glob` is too large (%d bytes, %d segments); the limits are %d bytes and %d segments",
				len(compiled.pattern), compiled.segmentCount(), maxGlobBytes, maxGlobSegments)
		}
	}
	return nil
}

// requestStringArgument reads the legacy top-level shape and the compact
// facade's options object. It deliberately does not coerce non-strings: schema
// and argument validation own type errors, while this seam owns bounded work.
func requestStringArgument(req *mcp.CallToolRequest, name string) string {
	if req == nil {
		return ""
	}
	args, _ := req.Params.Arguments.(map[string]any)
	if value, ok := args[name].(string); ok {
		return value
	}
	options, _ := args["options"].(map[string]any)
	value, _ := options[name].(string)
	return value
}
