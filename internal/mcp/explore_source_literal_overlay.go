package mcp

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/testpath"
)

const (
	exploreSourceLiteralOverlayMaxHits         = 24
	exploreSourceLiteralOverlayMaxBytes        = 4 << 20
	exploreSourceLiteralOverlayMaxLineBytes    = 64 << 10
	exploreSourceLiteralOverlayMaxCoveredFiles = 256
	exploreSourceLiteralOverlayBudget          = 75 * time.Millisecond
)

// exploreSourceLiteralOverlayFile is the request-local form of an editor
// overlay. Path is already in canonical graph-path form and eligible records
// whether the request scope admits the file. Every path, including tombstones
// and out-of-scope replacements, still shadows its durable counterpart.
type exploreSourceLiteralOverlayFile struct {
	path     string
	content  string
	deleted  bool
	eligible bool
}

type exploreSourceLiteralOverlayScan struct {
	matches    []trigram.Match
	covered    map[string]struct{}
	incomplete bool
}

// effectiveExploreSourceLiteralScope intersects the caller's requested scope
// with the session's immutable workspace/repository boundary. Caller filters
// may narrow the session, never widen it.
func (s *Server) effectiveExploreSourceLiteralScope(
	ctx context.Context,
	requested query.QueryOptions,
) (query.QueryOptions, bool) {
	merged := s.localizationNodeScopeWithTests(ctx, requested, requested.ExcludeTests)
	if strings.HasPrefix(merged.WorkspaceID, unresolvedWorkspacePrefix) {
		return query.QueryOptions{}, false
	}
	effective := requested
	effective.WorkspaceID = merged.WorkspaceID
	effective.ProjectID = merged.ProjectID
	effective.RepoAllow = merged.RepoAllow
	effective.ExcludeTests = merged.ExcludeTests
	return effective, true
}

func (s *Server) resolveExploreSourceLiteralRepoPrefix(
	repoPrefix string,
	scope query.QueryOptions,
) (string, bool) {
	repoPrefix = strings.TrimSuffix(strings.TrimSpace(repoPrefix), "/")
	if s != nil && s.multiIndexer == nil && s.indexer != nil {
		actual := strings.TrimSuffix(strings.TrimSpace(s.indexer.RepoPrefix()), "/")
		if actual != "" && repoPrefix != "" && repoPrefix != actual {
			return "", false
		}
		if actual != "" && len(scope.RepoAllow) > 0 && !scope.RepoAllow[actual] {
			return "", false
		}
		return actual, true
	}
	if repoPrefix != "" {
		if len(scope.RepoAllow) > 0 && !scope.RepoAllow[repoPrefix] {
			return "", false
		}
		return repoPrefix, true
	}
	for candidate, allowed := range scope.RepoAllow {
		if !allowed {
			continue
		}
		candidate = strings.TrimSuffix(strings.TrimSpace(candidate), "/")
		if repoPrefix != "" && candidate != repoPrefix {
			return "", false
		}
		repoPrefix = candidate
	}
	if repoPrefix != "" {
		return repoPrefix, true
	}
	if s != nil && s.multiIndexer != nil {
		prefixes := s.multiIndexer.RepoPrefixes()
		if len(prefixes) == 1 {
			return prefixes[0], true
		}
		if len(prefixes) > 1 {
			return "", false
		}
	}
	if s != nil && s.indexer != nil {
		return s.indexer.RepoPrefix(), true
	}
	return "", true
}

// snapshotExploreSourceLiteralOverlays consumes the exact immutable editor
// cohort pinned by request middleware, resolves each buffer to its graph path,
// and computes scope eligibility without parsing an overlay graph. A context
// that was not prepared intentionally has no overlay lane.
func (s *Server) snapshotExploreSourceLiteralOverlays(
	ctx context.Context,
	scope query.QueryOptions,
	repoPrefix string,
) ([]exploreSourceLiteralOverlayFile, map[string]struct{}, bool, bool, error) {
	covered := make(map[string]struct{})
	if s == nil || ctx == nil {
		return nil, covered, false, false, nil
	}
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || snapshot == nil {
		if OverlayViewFromContext(ctx) != nil {
			return nil, covered, true, false, fmt.Errorf("overlay view has no pinned request snapshot")
		}
		return nil, covered, false, false, nil
	}
	if snapshot.sessionID != OverlayCohortIDFromContext(ctx) {
		return nil, covered, false, false, fmt.Errorf("overlay request snapshot belongs to session %q, not %q", snapshot.sessionID, OverlayCohortIDFromContext(ctx))
	}
	if !snapshot.canonical {
		return nil, covered, true, false, fmt.Errorf("overlay request snapshot is not canonical")
	}
	fileCapacity := len(snapshot.files)
	if fileCapacity > exploreSourceLiteralOverlayMaxCoveredFiles {
		fileCapacity = exploreSourceLiteralOverlayMaxCoveredFiles
	}
	files := make([]exploreSourceLiteralOverlayFile, 0, fileCapacity)
	incomplete := false
	recordCount := 0
	for _, overlay := range snapshot.files {
		if err := ctx.Err(); err != nil {
			return nil, covered, incomplete, false, err
		}
		absPath, resolveErr := s.resolveOverlayAbsPathForRequest(ctx, overlay.Path)
		if resolveErr != nil || absPath == "" {
			incomplete = true
			continue
		}
		owner := s.pickIndexerForRequestPath(ctx, absPath)
		if owner == nil {
			incomplete = true
			continue
		}
		if repoPrefix != "" && owner.RepoPrefix() != repoPrefix {
			continue
		}
		graphPath := canonicalExploreSourceLiteralPath(overlay.Path)
		if graphPath == "" {
			incomplete = true
			continue
		}
		recordCount++
		if recordCount > exploreSourceLiteralOverlayMaxCoveredFiles {
			// The request cohort is already canonical, so this sentinel counts
			// distinct effective files rather than raw aliases.
			return nil, covered, true, true, nil
		}
		covered[graphPath] = struct{}{}
		eligible := repoNarrowAdmits(scope.RepoAllow, owner.RepoPrefix())
		if eligible && scope.WorkspaceID != "" {
			eligible = owner.WorkspaceID() == scope.WorkspaceID
		}
		if eligible && scope.ProjectID != "" {
			eligible = scope.WorkspaceID != "" && owner.ProjectID() == scope.ProjectID
		}
		if eligible && scope.ExcludeTests {
			eligible = !testpath.IsTestFile(graphPath)
		}
		files = append(files, exploreSourceLiteralOverlayFile{
			path: graphPath, content: overlay.Content, deleted: overlay.Deleted, eligible: eligible,
		})
	}
	return files, covered, incomplete, false, nil
}

// scanExploreSourceLiteralOverlays searches editor buffers without building an
// overlay graph. It keeps one representative line per file, uses a hard
// request-local byte/time envelope, and reads one sentinel hit beyond the
// caller's limit so truncation is explicit.
func scanExploreSourceLiteralOverlays(
	ctx context.Context,
	term string,
	files []exploreSourceLiteralOverlayFile,
	maxHits int,
) (exploreSourceLiteralOverlayScan, error) {
	return scanExploreSourceLiteralOverlaysWithClock(ctx, term, files, maxHits, time.Now)
}

func scanExploreSourceLiteralOverlaysWithClock(
	ctx context.Context,
	term string,
	files []exploreSourceLiteralOverlayFile,
	maxHits int,
	now func() time.Time,
) (exploreSourceLiteralOverlayScan, error) {
	result := exploreSourceLiteralOverlayScan{
		covered: make(map[string]struct{}, len(files)),
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	term = strings.TrimSpace(term)
	if term == "" || len(files) == 0 {
		return result, nil
	}

	hitLimit := exploreSourceLiteralOverlayMaxHits
	if maxHits > 0 && maxHits < hitLimit {
		hitLimit = maxHits
	}
	ordered := make([]exploreSourceLiteralOverlayFile, 0, len(files))
	for _, file := range files {
		file.path = canonicalExploreSourceLiteralPath(file.path)
		if file.path == "" {
			continue
		}
		result.covered[file.path] = struct{}{}
		if file.eligible && !file.deleted {
			ordered = append(ordered, file)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return exploreSourceLiteralPathLess(ordered[i].path, ordered[j].path)
	})

	if now == nil {
		now = time.Now
	}
	deadline := now().Add(exploreSourceLiteralOverlayBudget)
	inputBytes := 0
	for _, file := range ordered {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !now().Before(deadline) {
			result.incomplete = true
			break
		}
		scanner := bufio.NewScanner(strings.NewReader(file.content))
		scanner.Buffer(make([]byte, 4096), exploreSourceLiteralOverlayMaxLineBytes)
		line := 0
		for scanner.Scan() {
			line++
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if !now().Before(deadline) {
				result.incomplete = true
				break
			}
			lineBytes := scanner.Bytes()
			if len(lineBytes) > exploreSourceLiteralOverlayMaxBytes-inputBytes {
				result.incomplete = true
				break
			}
			inputBytes += len(lineBytes)
			text := scanner.Text()
			if !exploreOverlayLiteralHasBoundary(text, term) {
				continue
			}
			result.matches = append(result.matches, trigram.Match{
				Path: file.path,
				Line: line,
				Text: strings.Clone(text),
			})
			break
		}
		if scanner.Err() != nil {
			result.incomplete = true
		}
		if result.incomplete && !now().Before(deadline) {
			break
		}
		if len(result.matches) > hitLimit {
			result.incomplete = true
			result.matches = result.matches[:hitLimit]
			break
		}
	}
	return result, nil
}

// mergeExploreSourceLiteralMatches masks durable rows for every covered
// overlay path, gives overlay rows precedence, then deduplicates and sorts the
// combined cohort before applying the result cap.
func mergeExploreSourceLiteralMatches(
	overlay []trigram.Match,
	durable []trigram.Match,
	covered map[string]struct{},
	maxHits int,
) (matches []trigram.Match, incomplete bool) {
	limit := exploreSourceLiteralOverlayMaxHits
	if maxHits > 0 && maxHits < limit {
		limit = maxHits
	}
	type rankedMatch struct {
		match   trigram.Match
		overlay bool
	}
	ranked := make([]rankedMatch, 0, len(overlay)+len(durable))
	seen := make(map[string]struct{}, len(overlay)+len(durable))
	appendUnique := func(match trigram.Match, fromOverlay bool) {
		match.Path = canonicalExploreSourceLiteralPath(match.Path)
		if match.Path == "" {
			return
		}
		key := match.Path + "\x00" + strconv.Itoa(match.Line)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		match.Text = strings.Clone(match.Text)
		ranked = append(ranked, rankedMatch{match: match, overlay: fromOverlay})
	}
	for _, match := range overlay {
		appendUnique(match, true)
	}
	for _, match := range durable {
		canonical := canonicalExploreSourceLiteralPath(match.Path)
		if _, shadowed := covered[canonical]; shadowed {
			continue
		}
		appendUnique(match, false)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		leftTest, rightTest := testpath.IsTestFile(left.match.Path), testpath.IsTestFile(right.match.Path)
		if leftTest != rightTest {
			return !leftTest
		}
		if left.overlay != right.overlay {
			return left.overlay
		}
		if left.match.Path != right.match.Path {
			return left.match.Path < right.match.Path
		}
		if left.match.Line != right.match.Line {
			return left.match.Line < right.match.Line
		}
		return left.match.Text < right.match.Text
	})
	matches = make([]trigram.Match, 0, len(ranked))
	for _, rankedMatch := range ranked {
		matches = append(matches, rankedMatch.match)
	}
	if len(matches) > limit {
		matches = matches[:limit]
		incomplete = true
	}
	return matches, incomplete
}

func exploreSourceLiteralPathLess(left, right string) bool {
	leftTest, rightTest := testpath.IsTestFile(left), testpath.IsTestFile(right)
	if leftTest != rightTest {
		return !leftTest
	}
	return left < right
}

func canonicalExploreSourceLiteralPath(candidate string) string {
	return canonicalOverlayGraphPath(candidate)
}

func exploreOverlayLiteralHasBoundary(text, term string) bool {
	textRunes := []rune(text)
	termRunes := []rune(strings.TrimSpace(term))
	if len(termRunes) == 0 || len(textRunes) < len(termRunes) {
		return false
	}
	for start := 0; start+len(termRunes) <= len(textRunes); start++ {
		matched := true
		for offset := range termRunes {
			if textRunes[start+offset] != termRunes[offset] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		end := start + len(termRunes)
		leftOK := !exploreOverlayIdentifierRune(termRunes[0]) || start == 0 ||
			!exploreOverlayIdentifierRune(textRunes[start-1])
		rightOK := !exploreOverlayIdentifierRune(termRunes[len(termRunes)-1]) || end == len(textRunes) ||
			!exploreOverlayIdentifierRune(textRunes[end])
		if leftOK && rightOK {
			return true
		}
	}
	return false
}

func exploreOverlayIdentifierRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) ||
		unicode.IsMark(r) || unicode.Is(unicode.Pc, r)
}
