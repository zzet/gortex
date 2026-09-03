package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/docs"
	"github.com/zzet/gortex/internal/wiki"
)

// registerWikiTools registers `generate_wiki` and `generate_docs`.
// Wired from server.go alongside the other tool registries.
func (s *Server) registerWikiTools() {
	s.addTool(
		mcp.NewTool("generate_wiki",
			mcp.WithDescription("Generate a markdown wiki of the indexed graph: per-community pages, process pages with Mermaid sequenceDiagrams, contracts, hotspots, cycles, semantic. Output is written under output_dir (default 'wiki'); the per-repo index page content is returned inline for quick preview."),
			mcp.WithString("output_dir", mcp.Description("Output directory (default: wiki)")),
			mcp.WithString("format", mcp.Description("markdown (default) | html")),
			mcp.WithString("repo", mcp.Description("Per-repo slug under wiki/ (default: 'repo')")),
			mcp.WithString("project", mcp.Description("Project label (multi-repo mode hint)")),
			mcp.WithString("workspace", mcp.Description("Restrict emitted nodes to this WorkspaceID")),
			mcp.WithNumber("min_community", mcp.Description("Minimum community size to document (default: 3)")),
			mcp.WithNumber("max_communities", mcp.Description("Max communities to document (default: 20)")),
			mcp.WithBoolean("no_processes", mcp.Description("Skip process pages")),
			mcp.WithBoolean("no_contracts", mcp.Description("Skip contracts page")),
			mcp.WithBoolean("no_docs", mcp.Description("Skip docs bundle inside the wiki")),
			mcp.WithBoolean("force", mcp.Description("Suppress 'already exists' diagnostics (writer is always idempotent)")),
			mcp.WithBoolean("wikilinks", mcp.Description("Use [[wikilink]] style links")),
			mcp.WithBoolean("enhance", mcp.Description("Enrich narrative sections via the daemon's configured LLM provider (no-op when no provider is configured)")),
		),
		s.handleGenerateWiki,
	)

	s.addTool(
		mcp.NewTool("generate_docs",
			mcp.WithDescription("Generate a 'living docs' bundle: recent file changes, per-author ownership, stale code older than 365 days, blame summary. Returns markdown (default) or JSON inline; pass output_path to write to disk instead."),
			mcp.WithString("output_path", mcp.Description("Path to write the bundle to; omit to return inline")),
			mcp.WithString("format", mcp.Description("markdown (default) | json")),
			mcp.WithString("since", mcp.Description("Window for recent changes (Go duration string, e.g. 24h, 7d). Default 168h.")),
			mcp.WithNumber("top", mcp.Description("Cap each section's row count (default: 20)")),
			mcp.WithString("include", mcp.Description("Comma-separated section list: recent,ownership,stale,blame")),
			mcp.WithString("path_prefix", mcp.Description("Filter ownership/stale rows to this file prefix")),
			mcp.WithString("workspace", mcp.Description("Restrict nodes to this WorkspaceID")),
			mcp.WithBoolean("run_blame", mcp.Description("Re-run git blame across indexed repos before rendering")),
		),
		s.handleGenerateDocs,
	)
}

func (s *Server) handleGenerateWiki(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	opts := wiki.Options{
		OutputDir:      stringArgOrDefault(args, "output_dir", "wiki"),
		Format:         stringArgOrDefault(args, "format", "markdown"),
		Wikilinks:      boolArgValue(args, "wikilinks"),
		Repo:           stringArgOrDefault(args, "repo", "repo"),
		Project:        stringArg(args, "project"),
		WorkspaceID:    stringArg(args, "workspace"),
		MinCommunity:   intArgOrDefault(args, "min_community", 3),
		MaxCommunities: intArgOrDefault(args, "max_communities", 20),
		NoProcesses:    boolArgValue(args, "no_processes"),
		NoContracts:    boolArgValue(args, "no_contracts"),
		NoDocs:         boolArgValue(args, "no_docs"),
		Force:          boolArgValue(args, "force"),
	}

	// output_dir becomes the writer root and `repo` becomes a path segment
	// under it, so both steer where files land. Confine them the same way
	// export_graph confines its own output arguments.
	if err := s.guardGeneratedOutputPath(ctx, opts.OutputDir); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.guardGeneratedOutputPath(ctx, filepath.Join(opts.OutputDir, opts.Repo)); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	g := s.graph
	if g == nil {
		return mcp.NewToolResultError("wiki: graph is not initialised"), nil
	}
	// reader serves the node / edge reads the wiki renders, so an
	// overlay-active caller documents its own buffers. The community,
	// process, hotspot and cycle passes below build their own indexes over a
	// graph.Store, so those four sections are computed over the base corpus
	// even when the request carries an overlay view. A routed view never
	// reaches here: the wiki writes files, so the mutation gate refuses it
	// before the handler runs.
	reader := s.readerFor(ctx)
	communities := analysis.DetectCommunities(g)
	processes := analysis.DiscoverProcesses(g)
	hotspots := analysis.FindHotspots(g, communities, 0)
	cycles := analysis.DetectCycles(g, communities, "")

	var contractList []contracts.Contract
	if reg := s.effectiveContractRegistry(); reg != nil {
		contractList = reg.All()
	}

	// Optional docs bundle inside the wiki.
	var docsMarkdown string
	if !opts.NoDocs {
		bundle, err := docs.Generate(docs.Deps{Graph: reader, History: s.docsHistoryProvider()}, docs.Options{
			WorkspaceID: opts.WorkspaceID,
		})
		if err == nil {
			docsMarkdown = docs.RenderMarkdown(bundle)
		}
	}

	// LLM narrative enhancement reuses the daemon's already-constructed
	// provider; a no-op when no provider is configured.
	if boolArgValue(args, "enhance") && s.llmService != nil && s.llmService.Enabled() {
		opts.Enhance = true
		opts.Enhancer = wiki.NewClaudeCLIEnhancer(s.llmService.Provider(), wiki.NewEnhanceCache(wiki.DefaultEnhanceCacheDir()))
	}

	gen := wiki.New(wiki.Inputs{
		Graph:       reader,
		Communities: communities,
		Processes:   processes,
		Hotspots:    hotspots,
		Cycles:      cycles,
		Contracts:   contractList,
		DocsBundle:  docsMarkdown,
	}, opts)
	result, _, err := gen.Generate(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"output_dir":            result.OutputDir,
		"files":                 result.Files,
		"index_md_content":      result.IndexMarkdown,
		"repo_index_md_content": result.RepoIndexMarkdown,
		"file_count":            len(result.Files),
	})
}

func (s *Server) handleGenerateDocs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	g := s.readerFor(ctx)
	if g == nil {
		return mcp.NewToolResultError("docs: graph is not initialised"), nil
	}

	include := stringArg(args, "include")
	var sections []string
	if include != "" {
		for _, part := range strings.Split(include, ",") {
			p := strings.TrimSpace(strings.ToLower(part))
			if p != "" {
				sections = append(sections, p)
			}
		}
	}
	sinceDur, _ := parseDurationArg(args, "since")

	opts := docs.Options{
		Since:        sinceDur,
		Top:          intArgOrDefault(args, "top", 20),
		Sections:     sections,
		PathPrefix:   stringArg(args, "path_prefix"),
		WorkspaceID:  stringArg(args, "workspace"),
		IncludeBlame: boolArgValue(args, "run_blame"),
	}

	bundle, err := docs.Generate(docs.Deps{Graph: g, History: s.docsHistoryProvider()}, opts)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	format := stringArgOrDefault(args, "format", "markdown")
	output := ""
	if format == "json" {
		data, jerr := docs.RenderJSON(bundle)
		if jerr != nil {
			return mcp.NewToolResultError(jerr.Error()), nil
		}
		output = string(data)
	} else {
		output = docs.RenderMarkdown(bundle)
	}

	outputPath := stringArg(args, "output_path")
	if outputPath != "" {
		if err := s.guardGeneratedOutputPath(ctx, outputPath); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := writeWikiFile(outputPath, []byte(output)); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("write %q: %v", outputPath, err)), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"output_path": outputPath,
			"bytes":       len(output),
			"sections":    bundle.Sections,
		})
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"content":  output,
		"bytes":    len(output),
		"sections": bundle.Sections,
		"format":   format,
	})
}
