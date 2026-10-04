package mcp

import (
	"context"
	"fmt"
	"iter"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	querypkg "github.com/zzet/gortex/internal/query"
)

// registerFindFilesTool wires `find_files` — the dedicated search-by-
// file-NAME tool. File (KindFile) nodes are deliberately excluded from
// the BM25 symbol index (Indexer.shouldIndexForSearch), so neither
// search_symbols nor search_text can find a file by its name. This
// tool closes that gap: it enumerates the KindFile bucket directly and
// ranks by how well the query matches the basename (exact > prefix >
// substring), falling back to a path-substring match, with optional
// glob and fuzzy-subsequence matching.
func (s *Server) registerFindFilesTool() {
	s.addTool(
		mcp.NewTool("find_files",
			mcp.WithDescription("Find source files by NAME — the file-name counterpart of search_symbols. "+
				"Pass `query` for a basename/path match (ranked exact > prefix > substring), `glob` for a "+
				"path glob (e.g. \"internal/**/*_test.go\", \"*.proto\"), or both. Use this instead of search_symbols "+
				"kind:file (which cannot return file nodes). Returns repo-relative paths with the repo prefix and "+
				"language, ready to hand to get_file_summary / read_file. Pass format:\"gcx\" for the compact wire format."),
			mcp.WithString("query", mcp.Description("Filename or path substring to match (case-insensitive). Ranked basename-exact > prefix > substring > path-substring.")),
			mcp.WithString("glob", mcp.Description("Path glob over the repo-relative path. `*` stays within a segment, `**` crosses segments anywhere, `dir/*` is a directory prefix (whole subtree), a bare basename pattern like `*_test.go` matches basenames. ANDed with `query` when both are given.")),
			mcp.WithBoolean("fuzzy", mcp.Description("Also accept fuzzy subsequence matches of `query` against the basename (e.g. \"tcgo\" matches \"tools_coding.go\"). Lowest-ranked. Default false.")),
			mcp.WithString("path", mcp.Description("Restrict to one or more anchored sub-path prefixes (comma-separated), repo-root-relative — the monorepo-service slice.")),
			mcp.WithString("repo", mcp.Description("Restrict to a single repository prefix.")),
			mcp.WithString("project", mcp.Description("Restrict to repositories in a specific project.")),
			mcp.WithString("workspace", mcp.Description("Restrict to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) -- its repositories and paths narrow the matches. Ignored for repositories when an explicit repo / project / ref is also given.")),
			mcp.WithNumber("limit", mcp.Description("Max files to return (default 50, capped at 500).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (compact wire format), or toon.")),
		),
		s.handleFindFiles,
	)
}

// fileHit is one ranked find_files result.
type fileHit struct {
	Path     string `json:"path"`
	Repo     string `json:"repo,omitempty"`
	Language string `json:"language,omitempty"`
	ID       string `json:"id"`
	score    int
	depth    int
}

func (s *Server) handleFindFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	debug := s.logger != nil && s.logger.Core().Enabled(zap.DebugLevel)
	var start, phase time.Time
	var scopeMS, preparationMS, streamMS, sortMS, responseMS float64
	var visited, scoped, pathAccepted, matched int
	var projectionSupported, projectionUsed bool
	var readerType, baseType string
	if debug {
		start = time.Now()
		defer func() {
			s.logger.Debug("find_files phases", zap.String("query", req.GetString("query", "")), zap.String("path", req.GetString("path", "")), zap.String("repo", req.GetString("repo", "")),
				zap.String("reader_type", readerType), zap.String("base_reader_type", baseType), zap.Bool("node_projection_supported", projectionSupported), zap.Bool("node_projection_used", projectionUsed),
				zap.Float64("scope_ms", scopeMS), zap.Float64("iterator_preparation_ms", preparationMS), zap.Float64("stream_filter_ms", streamMS), zap.Float64("sort_ms", sortMS), zap.Float64("response_ms", responseMS), zap.Float64("handler_total_ms", float64(time.Since(start))/float64(time.Millisecond)),
				zap.Int("visited", visited), zap.Int("scoped", scoped), zap.Int("path_accepted", pathAccepted), zap.Int("matched", matched), zap.Error(ctx.Err()))
		}()
	}
	reader := s.readerFor(ctx)
	if debug {
		readerType = fmt.Sprintf("%T", reader)
		baseType = fmt.Sprintf("%T", s.requestBaseReader(ctx))
		_, projectionSupported = reader.(graph.ScopedNodeProjectionSequencer)
	}
	if reader == nil {
		return mcp.NewToolResultError("find_files: no graph available"), nil
	}
	query := strings.TrimSpace(req.GetString("query", ""))
	glob := strings.TrimSpace(req.GetString("glob", ""))
	if query == "" && glob == "" {
		return mcp.NewToolResultError("find_files: pass `query` (a filename/path substring) and/or `glob` (a path glob)"), nil
	}
	// Compile and bound the glob before anything walks the file set: the
	// matcher runs per candidate, ahead of `limit`, so pattern size is a
	// multiplier on the whole scan rather than on one call. The counts come
	// off the normalised pattern, because that is what the matcher sees —
	// reading them off the raw string let a native-separator glob count as
	// one segment here and expand to many at match time.
	compiledGlob := compileGlob(glob)
	if compiledGlob.tooComplex() {
		return mcp.NewToolResultError(fmt.Sprintf(
			"find_files: `glob` is too large (%d bytes, %d segments); the limits are %d bytes and %d segments",
			len(compiledGlob.pattern), compiledGlob.segmentCount(), maxGlobBytes, maxGlobSegments)), nil
	}
	fuzzy := req.GetBool("fuzzy", false)
	if debug {
		phase = time.Now()
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentLocate)
	if debug {
		scopeMS = float64(time.Since(phase)) / float64(time.Millisecond)
	}
	if errResult != nil {
		return errResult, nil
	}
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	pathFilter := s.resolvePathFilter(req, fieldQuery{})
	scopeOpts := querypkg.QueryOptions{
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}

	if debug {
		phase = time.Now()
	}
	var files iter.Seq[*graph.Node]
	if projection, ok := reader.(graph.ScopedNodeProjectionSequencer); ok && len(resolved.RepoAllow) > 0 {
		// ScopeAllows also admits unowned nodes. Keep one projection so its
		// ID order matches the original kind traversal for equal-path ties.
		if debug {
			projectionUsed = true
		}
		repos := []string{""}
		for repo, allowed := range resolved.RepoAllow {
			if allowed && repo != "" {
				repos = append(repos, repo)
			}
		}
		sort.Strings(repos)
		files = projection.NodesInScopeSeq(repos, nil, graph.KindFile)
	} else {
		files = reader.NodesByKind(graph.KindFile)
	}

	if debug {
		preparationMS = float64(time.Since(phase)) / float64(time.Millisecond)
		phase = time.Now()
	}
	hits := make([]fileHit, 0, 64)
	for n := range files {
		if debug {
			visited++
		}
		if n == nil {
			continue
		}
		if !s.nodeInSessionScope(ctx, n) {
			continue
		}
		if !scopeOpts.ScopeAllows(n) {
			continue
		}
		if debug {
			scoped++
		}
		rel := repoRelativePath(n)
		if len(pathFilter) > 0 && !pathMatchesAnyPrefix(rel, pathFilter) {
			continue
		}
		if debug {
			pathAccepted++
		}
		base := path.Base(rel)

		score := 0
		if glob != "" {
			if !compiledGlob.match(rel) {
				continue
			}
			score += 5
		}
		if query != "" {
			sc, ok := scoreFilenameMatch(query, base, rel, fuzzy)
			if !ok {
				continue
			}
			score += sc
		}

		if debug {
			matched++
		}
		hits = append(hits, fileHit{
			Path:     rel,
			Repo:     n.RepoPrefix,
			Language: n.Language,
			ID:       n.ID,
			score:    score,
			depth:    strings.Count(rel, "/"),
		})
	}

	if debug {
		streamMS = float64(time.Since(phase)) / float64(time.Millisecond)
		phase = time.Now()
	}
	// Highest score first; tie-break toward shallower paths, then
	// lexical — so a top-level match outranks a deeply-nested one and
	// identical inputs produce identical output across restarts.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].depth != hits[j].depth {
			return hits[i].depth < hits[j].depth
		}
		return hits[i].Path < hits[j].Path
	})
	if debug {
		sortMS = float64(time.Since(phase)) / float64(time.Millisecond)
	}
	total := len(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}

	if debug {
		phase = time.Now()
	}
	result, err := s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"query":     query,
		"glob":      glob,
		"files":     hits,
		"count":     len(hits),
		"truncated": total > len(hits),
	}, resolved)
	if debug {
		responseMS = float64(time.Since(phase)) / float64(time.Millisecond)
	}
	return result, err
}

// scoreFilenameMatch ranks how well query matches a file's basename or
// repo-relative path. The score tiers (basename-exact > basename-prefix
// > basename-substring > path-substring > fuzzy) keep the most specific
// match at the top. Returns (0, false) when nothing matches.
func scoreFilenameMatch(query, base, rel string, fuzzy bool) (int, bool) {
	q := strings.ToLower(query)
	b := strings.ToLower(base)
	r := strings.ToLower(rel)
	switch {
	case b == q:
		return 100, true
	case strings.HasPrefix(b, q):
		return 70, true
	case strings.Contains(b, q):
		return 50, true
	case strings.Contains(r, q):
		return 30, true
	}
	if fuzzy && isSubsequence(q, b) {
		return 10, true
	}
	return 0, false
}

// isSubsequence reports whether every rune of needle appears in haystack
// in order (a cheap fzf-style filename match). Both are expected to be
// lowercased by the caller.
func isSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	i := 0
	for j := 0; j < len(haystack) && i < len(needle); j++ {
		if haystack[j] == needle[i] {
			i++
		}
	}
	return i == len(needle)
}
