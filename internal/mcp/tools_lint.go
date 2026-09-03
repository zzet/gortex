package mcp

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/lint"
)

func (s *Server) registerLintTools() {
	s.addTool(
		mcp.NewTool("lint_file",
			mcp.WithDescription("Runs the available external linters/formatters against a file and returns normalized diagnostics — an LSP-free, on-demand syntax/lint check that complements get_diagnostics (which needs a running language server). Built-in linters: gofmt (Go), ruff (Python), shellcheck (shell). A linter that is not installed is reported under linters_skipped, never as an error. Use after an edit to confirm a file is syntactically sound, or to surface lint issues a structural check would miss."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File to lint (absolute, or repo-relative).")),
			mcp.WithString("language", mcp.Description("Override the language (go, python, shell). Inferred from the file extension when omitted.")),
			mcp.WithNumber("timeout_ms", mcp.Description("Per-linter timeout in milliseconds (default 5000).")),
		),
		s.handleLintFile,
	)
}

func (s *Server) handleLintFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	absPath, relPath, resolveErr := s.resolveFilePath(ctx, rawPath)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}

	lang := req.GetString("language", "")
	if lang == "" {
		lang = lint.LanguageForPath(absPath)
	}
	if lang == "" {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"path":            relPath,
			"language":        "",
			"healthy":         true,
			"diagnostics":     []any{},
			"linters_ran":     []any{},
			"linters_skipped": []any{},
			"note":            "no built-in linter is configured for this file type",
		})
	}

	timeout := time.Duration(req.GetInt("timeout_ms", 5000)) * time.Millisecond
	result := lint.NewRegistry().Run(ctx, absPath, lang, timeout)

	// Report diagnostics against the repo-relative path the caller passed.
	for i := range result.Diagnostics {
		result.Diagnostics[i].File = relPath
	}

	errCount, warnCount := 0, 0
	for _, d := range result.Diagnostics {
		switch d.Severity {
		case lint.SeverityError:
			errCount++
		case lint.SeverityWarning:
			warnCount++
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":            relPath,
		"language":        lang,
		"healthy":         errCount == 0,
		"diagnostics":     result.Diagnostics,
		"linters_ran":     result.Ran,
		"linters_skipped": result.Skipped,
		"summary": map[string]int{
			"errors":   errCount,
			"warnings": warnCount,
			"total":    len(result.Diagnostics),
		},
	})
}
