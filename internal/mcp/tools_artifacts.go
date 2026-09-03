package mcp

import (
	"context"
	"os"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/artifacts"
	"github.com/zzet/gortex/internal/config"
)

// maxArtifactContentBytes caps the file body get_artifact inlines.
const maxArtifactContentBytes = 64 * 1024

// SetArtifacts installs the `.gortex.yaml::artifacts` manifest so the
// search_artifacts / get_artifact tools can materialise it. Called by
// the server / daemon entrypoint right after NewServer.
func (s *Server) SetArtifacts(entries []config.ArtifactEntry) {
	s.artifactEntries = entries
}

// registerArtifactTools wires search_artifacts and get_artifact — the
// MCP surface over the non-code knowledge manifest.
func (s *Server) registerArtifactTools() {
	s.addTool(
		mcp.NewTool("search_artifacts",
			mcp.WithDescription("Search the `.gortex.yaml::artifacts` manifest — non-code knowledge files (DB schemas, API specs, infra configs, ADRs) tracked as first-class KindArtifact graph nodes. Each artifact carries a content hash and EdgeReferences edges to the symbols it mentions. Returns matching artifacts with {id, path, name, kind, size, content_hash, reference_count}. Use get_artifact to read one in full."),
			mcp.WithString("query", mcp.Description("Case-insensitive substring matched against the artifact name and path. Omit to list every artifact.")),
			mcp.WithString("kind", mcp.Description("Filter to one kind — schema | api | infra | doc.")),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 50).")),
			mcp.WithString("format", mcp.Description("Output format: json (default) or toon")),
		),
		s.handleSearchArtifacts,
	)
	s.addTool(
		mcp.NewTool("get_artifact",
			mcp.WithDescription("Read one manifest artifact in full — its metadata, the symbols it references, and its file content (capped at 64 KiB). Pass either id (artifact::<path>) or path."),
			mcp.WithString("id", mcp.Description("Artifact node ID — artifact::<path>. One of id / path is required.")),
			mcp.WithString("path", mcp.Description("Artifact file path. One of id / path is required.")),
			mcp.WithString("format", mcp.Description("Output format: json (default) or toon")),
		),
		s.handleGetArtifact,
	)
}

type artifactRow struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int    `json:"size"`
	Hash       string `json:"content_hash"`
	References int    `json:"reference_count"`
}

func (s *Server) handleSearchArtifacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := strings.ToLower(strings.TrimSpace(req.GetString("query", "")))
	kind := strings.ToLower(strings.TrimSpace(req.GetString("kind", "")))
	limit := max(req.GetInt("limit", 50), 1)

	s.ensureArtifacts()
	list := s.snapshotArtifacts()

	rows := make([]artifactRow, 0, len(list))
	for _, a := range list {
		if kind != "" && strings.ToLower(a.Kind) != kind {
			continue
		}
		if query != "" &&
			!strings.Contains(strings.ToLower(a.Name), query) &&
			!strings.Contains(strings.ToLower(a.Path), query) {
			continue
		}
		rows = append(rows, artifactRow{
			ID: a.ID, Path: a.Path, Name: a.Name, Kind: a.Kind,
			Size: a.Size, Hash: a.ContentHash, References: len(a.References),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"artifacts": rows,
		"total":     len(rows),
		"truncated": truncated,
	})
}

func (s *Server) handleGetArtifact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	path := strings.TrimSpace(req.GetString("path", ""))
	if id == "" && path == "" {
		return mcp.NewToolResultError("one of id or path is required"), nil
	}

	s.ensureArtifacts()
	art, ok := s.findArtifact(id, path)
	if !ok {
		return mcp.NewToolResultError("artifact not found — search_artifacts lists the manifest"), nil
	}

	type refRow struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		File string `json:"file"`
	}
	refs := make([]refRow, 0, len(art.References))
	reader := s.readerFor(ctx)
	for _, symID := range art.References {
		if n := reader.GetNode(symID); n != nil {
			refs = append(refs, refRow{ID: n.ID, Name: n.Name, Kind: string(n.Kind), File: n.FilePath})
		}
	}

	result := map[string]any{
		"id":           art.ID,
		"path":         art.Path,
		"name":         art.Name,
		"kind":         art.Kind,
		"size":         art.Size,
		"content_hash": art.ContentHash,
		"references":   refs,
	}
	// resolveFilePath blocks lexical `../` traversal but follows symlinks, so
	// the confinement check every other content-serving tool applies is
	// required here too before the bytes go into the response.
	if abs, _, err := s.resolveFilePath(ctx, art.Path); err == nil && s.guardSymlinkWithinRepo(ctx, abs) == nil {
		if data, err := os.ReadFile(abs); err == nil { //nolint:gosec // path resolved from the indexed manifest
			content := data
			truncated := false
			if len(content) > maxArtifactContentBytes {
				content = content[:maxArtifactContentBytes]
				truncated = true
			}
			result["content"] = string(content)
			result["content_truncated"] = truncated
		}
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// findArtifact resolves an artifact by node ID or by path. A path is
// matched exactly or as a suffix so a caller can pass either the
// repo-relative or the graph (repo-prefixed) form.
func (s *Server) findArtifact(id, path string) (artifacts.Artifact, bool) {
	for _, a := range s.snapshotArtifacts() {
		if id != "" && a.ID == id {
			return a, true
		}
		if path != "" && (a.Path == path || strings.HasSuffix(a.Path, "/"+path)) {
			return a, true
		}
	}
	return artifacts.Artifact{}, false
}

// ensureArtifacts materialises the manifest exactly once per daemon
// lifetime. Safe for concurrent callers.
func (s *Server) ensureArtifacts() {
	s.artifactsOnce.Do(s.materializeArtifacts)
}

func (s *Server) materializeArtifacts() {
	if len(s.artifactEntries) == 0 {
		return
	}
	var all []artifacts.Artifact
	for prefix, root := range s.collectRepoRoots("") {
		all = append(all, artifacts.Materialize(s.graph, root, s.artifactEntries, prefix)...)
	}
	s.artifactsMu.Lock()
	s.artifactList = all
	s.artifactsMu.Unlock()
}

func (s *Server) snapshotArtifacts() []artifacts.Artifact {
	s.artifactsMu.RLock()
	defer s.artifactsMu.RUnlock()
	return s.artifactList
}
