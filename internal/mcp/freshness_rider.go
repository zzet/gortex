package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// freshnessRiderFor returns a small structured freshness block for a
// file-reading tool whose target file has changed on disk since it was
// indexed — so an agent reading a just-edited file sees, inline, that the
// graph view may lag the working tree. It returns nil (no rider, zero
// extra tokens) for the overwhelmingly common fresh case, for non-file
// tools, and in multi-repo mode (where the legacy single-indexer's
// staleness signal is all-noise — per-repo watchers own freshness there).
//
// The check is O(1): one map lookup + one stat on the single file the
// tool targets, only for the handful of read/source tools.
func (s *Server) freshnessRiderFor(toolName string, req mcp.CallToolRequest) map[string]any {
	if s.indexer == nil && s.multiIndexer == nil {
		return nil
	}
	if os.Getenv("GORTEX_NO_FRESHNESS_RIDER") == "1" {
		return nil
	}
	raw := targetRepoFileRaw(toolName, req)
	if raw == "" {
		return nil
	}
	// Route the target to its OWNING indexer. In multi-repo mode this is the
	// per-repo sub-indexer that tracks the file's own mtimes / SHA — so the
	// staleness signal is reclaimed in the flagship deployment instead of
	// being suppressed wholesale. In single-repo mode it is the lone indexer.
	owner, repoRel := s.indexerForRel(raw)
	if owner == nil || repoRel == "" {
		return nil
	}
	state := owner.TrackedFileState(repoRel)
	mismatch := s.detectWorktreeMismatch()

	// Extractor-version staleness: which languages' extractors this binary
	// bumped since the OWNING repo was indexed. Fetched alongside the index
	// state so a graph indexed by an older extractor is flagged even when the
	// touched file's content is unchanged — per-language, so the advisory
	// names the exact languages to reindex rather than implying a full rebuild.
	var indexState graph.RepoIndexState
	var haveState bool
	// Extractor-version staleness, narrowed to THIS file's language. The
	// advisory rides on a per-file response, so a language the file is not
	// written in is noise the reader cannot act on: the reindex it asks
	// for would not touch the file in hand. Narrowing also keeps the
	// banner off repositories that hold no file of the stale language at
	// all — the comparison is against the baseline version every language
	// implicitly carries, so a bumped language is "behind" in every
	// repository indexed by an older binary, Go-only ones included.
	var staleLangs []string
	if r, ok := graph.Store(s.graph).(graph.RepoIndexStateReader); ok {
		if st, found, _ := r.GetRepoIndexState(owner.RepoPrefix()); found {
			indexState, haveState = st, true
			if fileLang := indexer.ExtractorLangForFile(repoRel); fileLang != "" &&
				slices.Contains(indexer.ExtractorVersionStaleLangs(st.ExtractorVersions), fileLang) {
				staleLangs = []string{fileLang}
			}
		}
	}

	// Whole-index "frozen" banner: when the file watcher is degraded (inotify
	// or FD exhaustion) the graph may silently lag every live edit, not just
	// this file — a condition distinct from per-file staleness. Surface it even
	// when the targeted file itself looks fresh.
	frozen := s.watchDegradedReason()

	if state == indexer.FileFresh && !mismatch && len(staleLangs) == 0 && frozen == "" {
		return nil
	}
	out := map[string]any{"file": repoRel}
	if frozen != "" {
		out["index_frozen"] = true
		out["index_frozen_hint"] = frozen
	}
	if p := owner.RepoPrefix(); p != "" {
		out["repo"] = p
	}
	switch state {
	case indexer.FileStale:
		out["stale"] = true
		out["hint"] = "this file changed on disk since it was last indexed; the graph view may lag the working tree"
		if haveState {
			if indexState.IndexedSHA != "" {
				out["indexed_sha"] = shortFreshSHA(indexState.IndexedSHA)
			}
			if indexState.Dirty {
				out["working_tree_dirty_at_index"] = true
			}
		}
	case indexer.FileMissing:
		out["missing"] = true
		out["hint"] = "this file is recorded in the graph but no longer exists on disk; it was deleted or moved since indexing"
	}
	if len(staleLangs) > 0 {
		out["extractor_stale_langs"] = staleLangs
		out["extractor_stale_hint"] = "this file's language extractor was upgraded since indexing; reindex to pick up the newer extraction (gortex index .)"
	}
	if mismatch {
		out["worktree_mismatch"] = true
		out["worktree_hint"] = "the working directory is a linked git worktree the indexed graph does not cover — results reflect another checkout"
	}
	return out
}

// watchDegradedReason returns the file watcher's degraded reason (inotify / FD
// exhaustion) when the index may be silently frozen, or "" when watching is
// healthy or unavailable. Read through an interface assertion so the rider
// stays decoupled from the concrete *indexer.Watcher behind the server's
// watcher field.
func (s *Server) watchDegradedReason() string {
	watcher := s.currentWatcher()
	if watcher == nil {
		return ""
	}
	if dr, ok := watcher.(interface{ DegradedReason() string }); ok {
		return dr.DegradedReason()
	}
	return ""
}

// indexerForRel routes a graph file path (as it appears in tool output or
// requests — repo-prefixed in multi-repo mode) to the per-repo indexer that
// owns it, returning that indexer and the repo-relative subpath its mtime keys
// use. In single-repo mode it returns the lone indexer with the configured
// prefix stripped. Returns (nil, "") when the path maps to no tracked repo.
func (s *Server) indexerForRel(graphPath string) (*indexer.Indexer, string) {
	graphPath = filepath.ToSlash(graphPath)
	if s.multiIndexer == nil {
		if s.indexer == nil {
			return nil, ""
		}
		rel := graphPath
		if p := s.indexer.RepoPrefix(); p != "" {
			rel = strings.TrimPrefix(rel, p+"/")
		}
		return s.indexer, rel
	}
	abs := s.multiIndexer.ResolveFilePath(graphPath)
	if abs == "" {
		return nil, ""
	}
	owner, prefix := s.multiIndexer.IndexerForFile(abs)
	if owner == nil {
		return nil, ""
	}
	return owner, strings.TrimPrefix(graphPath, prefix+"/")
}

// detectWorktreeMismatch reports (once per server, cached) whether the
// current working directory is a linked git worktree that the indexed graph
// does not cover — i.e. the agent is working in a worktree but the graph
// reflects a different checkout, so its results may not match the files on
// disk. Single-repo only; multi-repo routing owns its own worktree scoping.
func (s *Server) detectWorktreeMismatch() bool {
	s.worktreeMismatchOnce.Do(func() {
		if s.indexer == nil || s.multiIndexer != nil {
			return
		}
		cwd, err := os.Getwd()
		if err != nil || cwd == "" {
			return
		}
		if !indexer.ResolveWorktree(cwd).IsWorktree {
			return // not a linked worktree — nothing to warn about
		}
		root := s.indexer.RootPath()
		if root == "" {
			return
		}
		// The cwd is a linked worktree. If the indexed root does not contain
		// it, the graph reflects another checkout.
		cwdResolved, _ := filepath.EvalSymlinks(cwd)
		rootResolved, _ := filepath.EvalSymlinks(root)
		if cwdResolved == "" {
			cwdResolved = cwd
		}
		if rootResolved == "" {
			rootResolved = root
		}
		if !pathWithin(cwdResolved, rootResolved) {
			s.worktreeMismatch = true
		}
	})
	return s.worktreeMismatch
}

// pathWithin reports whether child is equal to or nested under parent, on a
// slash-segment boundary (so /a/bc is not "within" /a/b).
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// targetRepoFileRaw extracts the single file a read tool targets, verbatim
// (no prefix stripping) so the caller can route it to its owning repo. Returns
// "" for tools that are not file-scoped.
func targetRepoFileRaw(toolName string, req mcp.CallToolRequest) string {
	var raw string
	switch toolName {
	case "read_file", "get_file_summary", "get_editing_context":
		raw = req.GetString("path", "")
	case "get_symbol_source", "get_symbol":
		id := req.GetString("id", "")
		if i := strings.Index(id, "::"); i >= 0 {
			raw = id[:i]
		}
	default:
		return ""
	}
	if raw == "" {
		return ""
	}
	return filepath.ToSlash(raw)
}

// targetRepoRelFile extracts the repo-relative path of the single file a
// read tool targets, or "" when the tool is not file-scoped. A leading
// repo prefix is stripped so the result matches the indexer's mtime keys;
// a path that does not match is simply reported not-stale (an unknown key
// reads as fresh), so imperfect normalization is safe.
func targetRepoRelFile(toolName string, req mcp.CallToolRequest, prefix string) string {
	raw := targetRepoFileRaw(toolName, req)
	if raw == "" {
		return ""
	}
	if prefix != "" {
		raw = strings.TrimPrefix(raw, prefix+"/")
	}
	return raw
}

// isFreshnessListTool reports whether a tool returns a list of graph hits
// whose underlying files should be swept for on-disk drift / deletion.
func isFreshnessListTool(name string) bool {
	switch name {
	case "search_symbols", "find_usages", "smart_context", "get_callers":
		return true
	}
	return false
}

// freshnessPathKeys are the JSON keys whose string value names a file across
// the list-shaped tool results (search_symbols / find_usages / smart_context /
// get_callers).
var freshnessPathKeys = map[string]bool{
	"file": true, "file_path": true, "path": true, "filePath": true,
}

// maxFreshnessSweep bounds how many distinct files one list result is swept
// for, so a pathological response can't turn the rider into a hot loop.
const maxFreshnessSweep = 256

// decorateListResultWithFreshness sweeps the file paths a list result
// references and attaches a `freshness` block naming the hits that are stale
// or missing on disk — each carrying its owning repo prefix plus (for stale)
// the indexed SHA / dirty provenance, so the signal is per-repo rather than a
// flat workspace banner. Only JSON-object payloads are touched; GCX / TOON /
// array wire formats the caller opted into pass through unchanged, and a clean
// sweep adds nothing.
func (s *Server) decorateListResultWithFreshness(res *mcp.CallToolResult) *mcp.CallToolResult {
	if res == nil || (s.indexer == nil && s.multiIndexer == nil) {
		return res
	}
	if os.Getenv("GORTEX_NO_FRESHNESS_RIDER") == "1" {
		return res
	}
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		return res
	}
	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) != nil {
		return res // GCX / TOON / array — not a JSON object
	}
	if _, exists := asObj["freshness"]; exists {
		return res
	}

	var staleFiles, missingFiles []map[string]any
	for _, p := range collectGraphFilePaths(asObj) {
		owner, repoRel := s.indexerForRel(p)
		if owner == nil || repoRel == "" {
			continue
		}
		switch owner.TrackedFileState(repoRel) {
		case indexer.FileStale:
			staleFiles = append(staleFiles, s.freshFileProvenance(owner, repoRel, true))
		case indexer.FileMissing:
			missingFiles = append(missingFiles, s.freshFileProvenance(owner, repoRel, false))
		}
	}
	if len(staleFiles) == 0 && len(missingFiles) == 0 {
		return res
	}
	rider := map[string]any{}
	if len(staleFiles) > 0 {
		rider["stale_files"] = staleFiles
		rider["stale_hint"] = "some result files changed on disk since indexing; re-read them before acting on the graph view"
	}
	if len(missingFiles) > 0 {
		rider["missing_files"] = missingFiles
		rider["missing_hint"] = "some result files are recorded in the graph but no longer on disk; they were deleted or moved since indexing"
	}
	asObj["freshness"] = rider
	body, err := json.Marshal(asObj)
	if err != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}

// freshFileProvenance builds one stale/missing entry: the repo-relative file,
// its owning repo prefix, and — when withSHA — the repo's indexed SHA and
// working-tree-dirty flag. This per-repo provenance is the multi-repo upgrade
// over a single flat staleness banner.
func (s *Server) freshFileProvenance(owner *indexer.Indexer, repoRel string, withSHA bool) map[string]any {
	entry := map[string]any{"file": repoRel}
	if p := owner.RepoPrefix(); p != "" {
		entry["repo"] = p
	}
	if withSHA {
		if r, ok := graph.Store(s.graph).(graph.RepoIndexStateReader); ok {
			if st, found, _ := r.GetRepoIndexState(owner.RepoPrefix()); found {
				if st.IndexedSHA != "" {
					entry["indexed_sha"] = shortFreshSHA(st.IndexedSHA)
				}
				if st.Dirty {
					entry["working_tree_dirty_at_index"] = true
				}
			}
		}
	}
	return entry
}

// collectGraphFilePaths walks a decoded list result and gathers the distinct
// graph-relative file paths referenced under the common path keys, bounded by
// maxFreshnessSweep. Absolute paths and directory values are skipped /
// harmless — they resolve to no tracked file.
func collectGraphFilePaths(v any) []string {
	var out []string
	seen := make(map[string]bool)
	var walk func(any)
	walk = func(node any) {
		if len(out) >= maxFreshnessSweep {
			return
		}
		switch t := node.(type) {
		case map[string]any:
			for k, val := range t {
				if sv, ok := val.(string); ok {
					if freshnessPathKeys[k] && sv != "" && !filepath.IsAbs(sv) && !seen[sv] {
						seen[sv] = true
						out = append(out, sv)
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// decorateResultWithFreshness attaches the freshness rider to a JSON-object
// tool response under the "freshness" key. Non-JSON-object payloads
// (GCX / TOON / arrays) are left untouched — a best-effort hint must never
// reshape a compact wire format the caller opted into.
func decorateResultWithFreshness(res *mcp.CallToolResult, rider map[string]any) *mcp.CallToolResult {
	if len(rider) == 0 {
		return res
	}
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		return res
	}
	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) != nil {
		return res
	}
	if _, exists := asObj["freshness"]; exists {
		return res
	}
	asObj["freshness"] = rider
	body, err := json.Marshal(asObj)
	if err != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}

// shortFreshSHA trims a git SHA to 12 chars for the rider.
func shortFreshSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
