package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

const indexFailureSampleLimit = 20

// Internal baseline bookkeeping, removed before any response is encoded.
const indexHealthLivenessCeilingKey = "_index_health_liveness_ceiling"

const indexHealthLowScoreRecommendation = "Health score below 80%. Run index_repository with path \".\" to re-index the codebase."

// Registrations include repositories with no successful file nodes. Read their
// failures through the request reader so another checkout cannot supply them.
func (s *Server) readIndexFileFailures(ctx context.Context, resolved ResolvedScope) ([]indexer.FileIndexFailure, error) {
	reader := s.readerFor(ctx)
	if reader == nil {
		return nil, nil
	}
	prefixes := map[string]bool{}
	if s.multiIndexer != nil {
		for _, prefix := range s.multiIndexer.RepoPrefixes() {
			prefixes[prefix] = true
		}
	}
	if s.indexer != nil {
		prefixes[s.indexer.RepoPrefix()] = true
	}
	var failures []indexer.FileIndexFailure
	var readErr error
	scope := query.QueryOptions{WorkspaceID: resolved.WorkspaceID, ProjectID: resolved.ProjectID, RepoAllow: resolved.RepoAllow}
	for prefix := range prefixes {
		if len(resolved.RepoAllow) > 0 && !resolved.RepoAllow[prefix] {
			continue
		}
		owner := s.indexer
		if s.multiIndexer != nil {
			owner = s.multiIndexer.GetIndexer(prefix)
		}
		node := &graph.Node{RepoPrefix: prefix}
		if owner != nil {
			node.WorkspaceID, node.ProjectID = owner.WorkspaceID(), owner.ProjectID()
		}
		project := node.ProjectID
		if project == "" {
			project = prefix
		}
		if !scope.ScopeAllows(node) || (resolved.ProjectID != "" && project != resolved.ProjectID) {
			continue
		}
		rows, err := indexer.LoadFileIndexFailuresWithError(reader, prefix)
		if err != nil {
			readErr = errors.Join(readErr, fmt.Errorf("read index failure state for repository %q: %w", prefix, err))
			continue
		}
		failures = append(failures, rows...)
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Path < failures[j].Path })
	return failures, readErr
}

// Initial read failures have no graph node. Persisted ownership attributes them
// without requiring a successful prior index, or treating them as global nodes.
func scopedIndexFileFailures(failures []indexer.FileIndexFailure, resolved ResolvedScope, paths []string) []indexer.FileIndexFailure {
	opts := query.QueryOptions{WorkspaceID: resolved.WorkspaceID, ProjectID: resolved.ProjectID, RepoAllow: resolved.RepoAllow}
	paths = normalizePathPrefixes(paths)
	var scoped []indexer.FileIndexFailure
	for _, failure := range failures {
		if len(resolved.RepoAllow) > 0 && !resolved.RepoAllow[failure.RepoPrefix] {
			continue
		}
		project := failure.ProjectID
		if project == "" {
			project = failure.RepoPrefix
		}
		if resolved.ProjectID != "" && project != resolved.ProjectID {
			continue
		}
		node := &graph.Node{FilePath: failure.Path, RepoPrefix: failure.RepoPrefix, WorkspaceID: failure.WorkspaceID, ProjectID: failure.ProjectID}
		if !opts.ScopeAllows(node) {
			continue
		}
		if len(paths) > 0 && !indexFailureOverlapsPaths(failure.Path, expandPathPrefixesWithRepos(paths, []string{failure.RepoPrefix})) {
			continue
		}
		scoped = append(scoped, failure)
	}
	return scoped
}

func indexFailureOverlapsPaths(failurePath string, paths []string) bool {
	failurePath = path.Clean(failurePath)
	if failurePath == "." {
		return true // A failed standalone repository-root walk hides every subpath.
	}
	if pathMatchesAnyPrefix(failurePath, paths) {
		return true
	}
	// A failed directory walk can hide any descendant. Its warning applies
	// when a request narrows further into that directory, too.
	for _, path := range paths {
		if pathMatchesAnyPrefix(path, []string{failurePath}) {
			return true
		}
	}
	return false
}

func indexFileFailureSummary(failures []indexer.FileIndexFailure) map[string]any {
	unreadable := 0
	byRepo, unreadableByRepo := map[string]int{}, map[string]int{}
	for _, failure := range failures {
		byRepo[failure.RepoPrefix]++
		if failure.PermissionDenied {
			unreadable++
			unreadableByRepo[failure.RepoPrefix]++
		}
	}
	result := map[string]any{
		"failed_file_count":        len(failures),
		"unreadable_file_count":    unreadable,
		"failed_files_by_repo":     byRepo,
		"unreadable_files_by_repo": unreadableByRepo,
	}
	if len(failures) > 0 {
		result["failed_files"] = failures[:min(len(failures), indexFailureSampleLimit)]
		if len(failures) > indexFailureSampleLimit {
			result["failed_files_truncated"] = true
		}
	}
	return result
}

func (s *Server) indexFileFailureWarning(ctx context.Context, resolved ResolvedScope, paths []string) map[string]any {
	allFailures, readErr := s.readIndexFileFailures(ctx, resolved)
	failures := scopedIndexFileFailures(allFailures, resolved, paths)
	if len(failures) == 0 && readErr == nil {
		return nil
	}
	warning := indexFileFailureSummary(failures)
	warning["code"] = "index_incomplete"
	warning["message"] = fmt.Sprintf("%d in-scope file(s) could not be indexed. Results may omit current code or contain stale symbols; zero matches do not prove absence. Fix the reported file access or indexing errors and reindex the affected paths.", len(failures))
	if readErr != nil {
		warning["index_state_read_error"] = readErr.Error()
		warning["message"] = "Index failure state could not be read for this scope; result completeness is unknown. Results may omit current code or retain stale symbols, and zero matches do not prove absence. " + readErr.Error()
	}
	return warning
}

func stampIndexFileFailureWarning(resp map[string]any, warning map[string]any) {
	if warning != nil {
		resp["index_complete"] = false
		resp["index_warning"] = warning
	}
}

// Keep one text block so later freshness decoration still reaches GCX headers.
// Compact and TOON output gain a body-visible qualification without changing
// their result rows or pagination fields.
func decorateIndexFileFailureResult(result *mcp.CallToolResult, warning map[string]any) *mcp.CallToolResult {
	if result == nil || warning == nil {
		return result
	}
	fields := map[string]any{"index_complete": false, "index_warning": warning}
	result = mergeResultMeta(result, fields)
	text, ok := singleTextContent(result)
	if !ok {
		return result
	}
	// GCX headers carry scalar fields; the full structured warning remains
	// on the response metadata while its qualification is visible in-band.
	gcxFields := map[string]any{
		"index_complete":        false,
		"index_warning":         "index_incomplete",
		"index_warning_message": warning["message"],
		"failed_file_count":     warning["failed_file_count"],
		"unreadable_file_count": warning["unreadable_file_count"],
	}
	if body, isGCX := injectGCXHeaderMeta(text, gcxFields); isGCX {
		return rebuildTextResult(result, body)
	}
	message, _ := warning["message"].(string)
	quotedMessage, _ := json.Marshal(message)
	return rebuildTextResult(result, strings.TrimRight(text, "\n")+"\nindex_incomplete: "+string(quotedMessage)+"\n")
}

func indexHealthFailureCounts(reader graph.Reader, totalDetected, fileNodes int, parseErrors []indexer.IndexError, failures []indexer.FileIndexFailure) (int, int) {
	if len(failures) == 0 {
		return totalDetected, max(totalDetected-len(parseErrors), 0)
	}
	parsePaths := make(map[string]bool, len(parseErrors))
	for _, failure := range parseErrors {
		parsePaths[failure.FilePath] = true
	}
	missing, additionalFailures := 0, 0
	for _, failure := range failures {
		if reader.GetNode(failure.Path) == nil {
			missing++
		}
		if !parsePaths[failure.Path] {
			additionalFailures++
		}
	}
	totalDetected = max(totalDetected, fileNodes+missing)
	return totalDetected, max(totalDetected-len(parseErrors)-additionalFailures, 0)
}

// Full indexing reports absolute paths, while the durable ledger uses graph
// paths. Normalize only against the originating indexer's exact root/prefix;
// a basename or suffix match could conflate failures in different repositories.
func indexHealthParseErrorsForCounts(parseErrors []indexer.IndexError, idx *indexer.Indexer) []indexer.IndexError {
	if idx == nil {
		return parseErrors
	}
	normalized := append([]indexer.IndexError(nil), parseErrors...)
	for i, failure := range normalized {
		file := failure.FilePath
		absolute := filepath.IsAbs(file)
		if absolute {
			if idx.RootPath() == "" {
				continue
			}
			relative, err := filepath.Rel(idx.RootPath(), file)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
				continue
			}
			file = filepath.ToSlash(relative)
		}
		file = norm.NFC.String(filepath.ToSlash(file))
		if prefix := idx.RepoPrefix(); file != "" && prefix != "" && (absolute || !strings.HasPrefix(file, prefix+"/")) {
			file = prefix + "/" + file
		}
		normalized[i].FilePath = file
	}
	return normalized
}

func indexHealthParseFailureCount(value any) int {
	switch failures := value.(type) {
	case []indexer.IndexError:
		return len(failures)
	case map[string]string:
		return len(failures)
	default:
		return 0
	}
}

// Parse and file failures are read live, and every field changed here belongs
// to a new top-level map. Only the expensive liveness audit stays cached, so
// recovery restores the score without racing readers or discarding orphan caps.
func (s *Server) refreshIndexHealthFileFailures(ctx context.Context, baseline map[string]any) map[string]any {
	result := make(map[string]any, len(baseline)+8)
	for key, value := range baseline {
		if key != indexHealthLivenessCeilingKey {
			result[key] = value
		}
	}
	failures, readErr := s.readIndexFileFailures(ctx, ResolvedScope{})
	totalDetected, _ := baseline["total_detected"].(int)
	fileNodes, _ := baseline["file_node_count"].(int)
	parseErrors, _ := baseline["parse_failures"].([]indexer.IndexError)
	if s.indexer != nil {
		parseErrors = s.indexer.ParseErrors()
	}
	if len(parseErrors) > 0 {
		result["parse_failures"] = parseErrors
	} else {
		delete(result, "parse_failures")
	}
	totalDetected, successfullyIndexed := indexHealthFailureCounts(s.readerFor(ctx), totalDetected, fileNodes, indexHealthParseErrorsForCounts(parseErrors, s.indexer), failures)
	result["total_detected"] = totalDetected
	result["successfully_indexed"] = successfullyIndexed
	if totalDetected > 0 {
		healthScore := math.Round(float64(successfullyIndexed)/float64(totalDetected)*1000) / 10
		if ceiling, ok := baseline[indexHealthLivenessCeilingKey].(float64); ok {
			healthScore = min(healthScore, ceiling)
		}
		if len(failures) > 0 {
			healthScore = min(healthScore, 99.9)
		}
		result["health_score"] = healthScore
	}
	for key, value := range indexFileFailureSummary(failures) {
		result[key] = value
	}
	result["index_complete"] = len(failures) == 0 && readErr == nil
	result["status"] = "ready"
	recommendation, _ := baseline["recommendation"].(string)
	if score, ok := result["health_score"].(float64); ok {
		if score >= 80 {
			recommendation = strings.TrimSpace(strings.ReplaceAll(recommendation, indexHealthLowScoreRecommendation, ""))
		} else if !strings.Contains(recommendation, indexHealthLowScoreRecommendation) {
			recommendation = strings.TrimSpace(indexHealthLowScoreRecommendation + " " + recommendation)
		}
	}
	if recommendation == "" {
		delete(result, "recommendation")
	} else {
		result["recommendation"] = recommendation
	}
	if len(failures) > 0 {
		result["status"] = "degraded"
		result["recommendation"] = strings.TrimSpace("Some files could not be indexed (see failed_files). Results can omit current code or retain stale symbols. Fix the reported file access or indexing errors and reindex the affected paths. " + recommendation)
	}
	if readErr != nil {
		result["status"] = "degraded"
		result["index_state_read_error"] = readErr.Error()
		result["recommendation"] = strings.TrimSpace("Index failure state could not be read; index completeness is unknown. " + readErr.Error() + ". " + recommendation)
	}
	if section := pendingProjectionHealth(pendingFileProjections); section != nil {
		result["pending_projection"] = section
	}
	return result
}

// pendingProjectionHealth reports the pending projections: live entries and
// their estimated size, what publishing refused and why, and how many
// retired entries disagreed with the store. It is nil until the registry has
// done anything, and it reads only in-memory state.
func pendingProjectionHealth(p *pendingProjections) map[string]any {
	snap := p.snap.Load()
	p.mu.Lock()
	refused := make(map[string]int64, len(p.refused))
	for reason, n := range p.refused {
		refused[string(reason)] = n
	}
	p.mu.Unlock()
	published := p.published.Load()
	if len(snap.entries) == 0 && published == 0 && len(refused) == 0 {
		return nil
	}
	tombstones := 0
	for _, e := range snap.entries {
		if e.tombstone {
			tombstones++
		}
	}
	return map[string]any{
		"enabled":               p.enabled,
		"files":                 len(snap.entries) - tombstones,
		"tombstones":            tombstones,
		"bytes":                 snap.bytes,
		"published":             published,
		"refused":               refused,
		"retired":               p.retired.Load(),
		"external_writes":       p.externalWrites.Load(),
		"projection_divergence": p.divergence.Load(),
	}
}
