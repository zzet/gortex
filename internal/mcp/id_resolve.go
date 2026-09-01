package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// resolveNameToIDs returns the path-qualified node ids of every definition
// (function / method / type / …) whose short name equals the given bare name,
// sorted and de-duped — so a caller can pass "Bar" instead of
// "pkg/foo.go::Bar". Returns nil for an empty name or no match.
func (s *Server) resolveNameToIDs(name string) []string {
	if s.graph == nil || name == "" {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, n := range s.graph.FindNodesByName(name) {
		if n == nil || n.ID == "" || seen[n.ID] || !nodeIsDefinitionKind(n.Kind) {
			continue
		}
		seen[n.ID] = true
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

// resolveSymbolTarget resolves a tool's symbol target, which may be a full node
// id or a bare symbol name. An exact (or repo-relative) id always wins — a valid
// id is never reinterpreted as a name. A bare name that matches exactly one
// definition resolves to that id; a name matching several returns ("",
// candidates) so the caller can ask the user to disambiguate; an unmatched
// target is returned unchanged so the tool surfaces its own not-found caveat.
func (s *Server) resolveSymbolTarget(ctx context.Context, target string) (id string, candidates []string) {
	if target == "" {
		return "", nil
	}
	if r := s.resolveSymbolID(ctx, target); s.graph != nil && s.graph.GetNode(r) != nil {
		return r, nil
	}
	if !strings.Contains(target, "::") {
		if ids := s.resolveNameToIDs(target); len(ids) == 1 {
			return ids[0], nil
		} else if len(ids) > 1 {
			return "", ids
		}
	}
	return target, nil
}

// symbolDisambiguationResult renders the candidate definitions a bare name
// matched, so the agent re-calls the tool with one of the path-qualified ids.
func (s *Server) symbolDisambiguationResult(ctx context.Context, req mcp.CallToolRequest, tool, name string, candidates []string) (*mcp.CallToolResult, error) {
	out := make([]map[string]any, 0, len(candidates))
	for _, id := range candidates {
		m := map[string]any{"id": id}
		if s.graph != nil {
			if n := s.graph.GetNode(id); n != nil {
				m["name"] = n.Name
				m["kind"] = string(n.Kind)
				m["file_path"] = n.FilePath
			}
		}
		out = append(out, m)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"ambiguous":  true,
		"tool":       tool,
		"name":       name,
		"candidates": out,
		"hint":       fmt.Sprintf("%q matches %d definitions; re-call %s with one of the candidate ids", name, len(candidates), tool),
	})
}

// resolveSymbolID normalizes a possibly repo-relative symbol id to its
// canonical graph id. A full id that already names a node is returned
// unchanged (exact match first, so a valid id is never reinterpreted).
// Otherwise, when the calling session's working directory maps to a
// tracked repo, the repo prefix is prepended and tried — so a caller
// inside a repo can pass repo-relative ids (internal/x.go::Foo) instead of
// the prefixed form (gortex/internal/x.go::Foo). Falls back to the input
// id (which then surfaces the not-found caveat) when neither resolves.
// Safe for any id: a non-symbol id (memory/note/overlay) never matches a
// node, so it is returned unchanged.
func (s *Server) resolveSymbolID(ctx context.Context, id string) string {
	if id == "" || s.graph == nil || s.graph.GetNode(id) != nil {
		return id
	}
	// The cwd rung needs the multi-repo index to map a directory to a repo
	// prefix; the graphRelID rung below does not. Returning early on a nil
	// multiIndexer skipped both, which cost a server built without
	// MultiRepoOptions (cmd/gortex eval_recall.go, eval_server.go) the
	// separator normalization graphPathSpelling exists to provide: on Windows
	// the stored id is `pkga\a.go::Foo` while every agent writes
	// `pkga/a.go::Foo`, and the tool answered "symbol not found" for an
	// indexed symbol.
	if s.multiIndexer != nil {
		cwd := SessionCWDFromContext(ctx)
		if cwd != "" {
			if _, _, prefix, ok := s.multiIndexer.ScopeForCWD(cwd); ok && prefix != "" {
				if cand := prefix + "/" + id; s.graph.GetNode(cand) != nil {
					return cand
				}
			}
		}
	}
	// Fallback: anchor the id's file-path part against the repo that owns
	// the file on disk, so a repo-relative id still resolves when the
	// session cwd doesn't map to a tracked repo (e.g. a cross-repo edit).
	if rel := s.graphRelID(id); rel != id && s.graph.GetNode(rel) != nil {
		return rel
	}
	return id
}

// graphRelID normalises the file-path part of a symbol id (path::Name) to
// the graph's stored repo-prefixed form, leaving the name part untouched.
// An id without a "::" path part is returned unchanged.
func (s *Server) graphRelID(id string) string {
	parts := strings.SplitN(id, "::", 2)
	if len(parts) != 2 {
		return id
	}
	return s.graphRelPath(parts[0]) + "::" + parts[1]
}

// symbolIDArg extracts the required "id" argument and normalizes it via
// resolveSymbolID, so every symbol-id tool accepts repo-relative ids.
func (s *Server) symbolIDArg(ctx context.Context, req mcp.CallToolRequest) (string, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return "", err
	}
	return s.resolveSymbolID(ctx, id), nil
}
