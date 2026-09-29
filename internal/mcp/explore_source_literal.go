package mcp

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/testpath"
)

const (
	exploreSourceLiteralRecallMaxHits          = 24
	exploreSourceLiteralRecallMaxFiles         = 0
	exploreSourceLiteralRecallBudget           = 75 * time.Millisecond
	exploreSourceLiteralRecallMaxTerms         = 4
	exploreSourceLiteralRecallMaxOwnersPerTerm = 3
	exploreSourceLiteralRecallMaxFilesPerTerm  = 3
	exploreSourceLiteralCallEdgesPerSite       = 8
	exploreSourceLiteralCalleeHydrationLimit   = exploreSourceLiteralRecallMaxHits * exploreSourceLiteralCallEdgesPerSite
)

// sourceLiteralRecallBudget is the wall-clock slice one anchor of the bounded
// source-literal recall may spend. Everything the recall reports — which
// owners it mapped, and the reason it stopped — is a function of this slice,
// so tests pin it rather than racing the default against a loaded machine.
func (s *Server) sourceLiteralRecallBudget() time.Duration {
	if s != nil && s.sourceLiteralRecallBudgetOverride > 0 {
		return s.sourceLiteralRecallBudgetOverride
	}
	return exploreSourceLiteralRecallBudget
}

type exploreSourceLiteralHit struct {
	nodeID    string
	rank      int
	anchor    int
	ambiguous bool
	callee    bool
	// The exact source-match coordinate is retained request-locally so the
	// localization envelope may offer one bounded live source window without
	// repeating the content search. Raw match text is intentionally discarded.
	matchPath string
	matchLine int
	literal   string
}

type exploreSourceLiteralDiagnostic struct {
	literal        string
	rawHits        int
	mappedOwners   int
	retainedOwners int
	retainedFiles  int
	reason         string
}

type exploreSourceLiteralRecall struct {
	hits        []exploreSourceLiteralHit
	ambiguous   bool
	ownerFiles  map[string]string
	diagnostics []exploreSourceLiteralDiagnostic
}

type exploreSourceLiteralSearch struct {
	matches          []trigram.Match
	incomplete       bool
	backend          string
	owned            bool
	lookupRepoPrefix string
	err              error
}

// explorePreferredSourceLiteral picks one deterministic source-search key.
// Quoted terms have already passed the noise filter. Compact alphabetic values
// (locale/protocol/config keys) are reserved before longer prose because their
// registration sites are otherwise invisible to symbol metadata. A saturated
// compact lookup may fall back to the longest remaining term inside the same
// fixed end-to-end deadline.
func explorePreferredSourceLiteral(terms []string) string {
	best := ""
	bestLen := 0
	bestCompact := false
	for _, term := range terms {
		n := utf8.RuneCountInString(term)
		compact := exploreCompactSourceLiteral(term, n)
		if best == "" || compact && !bestCompact || compact == bestCompact && n > bestLen {
			best, bestLen, bestCompact = term, n, compact
		}
	}
	return best
}

func exploreCompactSourceLiteral(term string, runeCount int) bool {
	// Reserve priority only for two-letter alphabetic codes. Three- and
	// four-letter lowercase values are indistinguishable from common prose
	// ("test", "file", "true") and must compete by information length. They
	// remain searchable when they are the only quoted term.
	if runeCount != 2 {
		return false
	}
	lower := strings.ToLower(term)
	if _, stop := assistStopWords[lower]; stop {
		return false
	}
	switch lower {
	case "am", "an", "as", "at", "be", "by", "do", "go", "he", "hi", "if", "in", "is", "it", "me", "my", "no", "of", "oh", "ok", "on", "or", "so", "to", "up", "us", "we":
		return false
	}
	for _, r := range term {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// appendExploreSourceLiteralProseTerms fills the remaining bounded term slots
// with non-compact literals, longest first: a longer quoted value carries more
// information and greps with less noise. Compact codes keep the slots they
// already reserved.
func appendExploreSourceLiteralProseTerms(attemptTerms, terms []string) []string {
	if len(attemptTerms) >= exploreSourceLiteralRecallMaxTerms {
		return attemptTerms
	}
	seen := make(map[string]struct{}, len(attemptTerms)+len(terms))
	for _, term := range attemptTerms {
		seen[strings.ToLower(term)] = struct{}{}
	}
	prose := make([]string, 0, len(terms))
	for _, term := range terms {
		if exploreCompactSourceLiteral(term, utf8.RuneCountInString(term)) {
			continue
		}
		key := strings.ToLower(term)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		prose = append(prose, term)
	}
	sort.SliceStable(prose, func(i, j int) bool {
		return utf8.RuneCountInString(prose[i]) > utf8.RuneCountInString(prose[j])
	})
	for _, term := range prose {
		if len(attemptTerms) >= exploreSourceLiteralRecallMaxTerms {
			break
		}
		attemptTerms = append(attemptTerms, term)
	}
	return attemptTerms
}

func exploreSourceLiteralFallback(terms []string, primary string) string {
	best := ""
	bestLen := 0
	for _, term := range terms {
		if strings.EqualFold(term, primary) {
			continue
		}
		if n := utf8.RuneCountInString(term); n > bestLen {
			best, bestLen = term, n
		}
	}
	return best
}

func exploreCompactSourceLiteralTerms(terms []string) []string {
	out := make([]string, 0, min(len(terms), exploreSourceLiteralRecallMaxTerms))
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		if !exploreCompactSourceLiteral(term, utf8.RuneCountInString(term)) {
			continue
		}
		key := strings.ToLower(term)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if len(out) < exploreSourceLiteralRecallMaxTerms {
			out = append(out, term)
		}
	}
	return out
}

// retainExploreSourceLiteralOwners keeps file diversity before filling the
// remaining declaration slots. This prevents the first file's sibling methods
// from evicting an equally exact owner in a second file while retaining fixed
// per-term work and response bounds.
func retainExploreSourceLiteralOwners(recall exploreSourceLiteralRecall) (hits []exploreSourceLiteralHit, files int, reason string) {
	if len(recall.hits) == 0 {
		return nil, 0, "no_mapped_owner"
	}
	seenOwners := make(map[string]struct{}, min(len(recall.hits), exploreSourceLiteralRecallMaxOwnersPerTerm))
	seenFiles := make(map[string]struct{}, exploreSourceLiteralRecallMaxFilesPerTerm)
	selected := make([]bool, len(recall.hits))
	order := make([]int, len(recall.hits))
	for index := range recall.hits {
		order[index] = index
	}
	// A graph-resolved direct callee is more actionable than its enclosing
	// source owner. Prefer it inside the same fixed owner/file caps; ambiguity
	// remains attached to the hit and therefore cannot become terminal proof.
	sort.SliceStable(order, func(i, j int) bool {
		left, right := recall.hits[order[i]], recall.hits[order[j]]
		if left.callee != right.callee {
			return left.callee
		}
		if left.rank != right.rank {
			return left.rank < right.rank
		}
		return left.nodeID < right.nodeID
	})
	add := func(index int) {
		hit := recall.hits[index]
		if _, duplicate := seenOwners[hit.nodeID]; duplicate {
			return
		}
		seenOwners[hit.nodeID] = struct{}{}
		if file := recall.ownerFiles[hit.nodeID]; file != "" {
			seenFiles[file] = struct{}{}
		}
		selected[index] = true
		hits = append(hits, hit)
	}

	// First pass: one declaration from each distinct file.
	for _, index := range order {
		if len(hits) >= exploreSourceLiteralRecallMaxOwnersPerTerm || len(seenFiles) >= exploreSourceLiteralRecallMaxFilesPerTerm {
			break
		}
		hit := recall.hits[index]
		file := recall.ownerFiles[hit.nodeID]
		if file == "" {
			continue
		}
		if _, exists := seenFiles[file]; exists {
			continue
		}
		add(index)
	}
	// Second pass: fill remaining owner slots from already-admitted files.
	for _, index := range order {
		if len(hits) >= exploreSourceLiteralRecallMaxOwnersPerTerm {
			break
		}
		if selected[index] {
			continue
		}
		hit := recall.hits[index]
		file := recall.ownerFiles[hit.nodeID]
		if file != "" {
			if _, admitted := seenFiles[file]; !admitted && len(seenFiles) >= exploreSourceLiteralRecallMaxFilesPerTerm {
				continue
			}
		}
		add(index)
	}

	if len(hits) < len(recall.hits) {
		mappedFiles := make(map[string]struct{}, len(recall.hits))
		for _, hit := range recall.hits {
			if file := recall.ownerFiles[hit.nodeID]; file != "" {
				mappedFiles[file] = struct{}{}
			}
		}
		if len(mappedFiles) > exploreSourceLiteralRecallMaxFilesPerTerm {
			reason = "file_cap"
		} else {
			reason = "owner_cap"
		}
	}
	return hits, len(seenFiles), reason
}

// gatherExploreSourceLiteralRecall reuses the bounded raw-text path behind
// search(operation:"text") for every admitted task literal whose value is not
// already visible in a ranked declaration. Compact codes keep their reserved
// slots, then the longest remaining literals fill the bounded term budget. It
// searches one repository, maps 1-based hits to their smallest enclosing
// declarations, and returns a file-diverse owner set for the caller's
// existing batch hydration.
func (s *Server) gatherExploreSourceLiteralRecall(
	ctx context.Context,
	terms []string,
	repoPrefix string,
	scope query.QueryOptions,
) exploreSourceLiteralRecall {
	if s == nil || ctx.Err() != nil {
		return exploreSourceLiteralRecall{}
	}
	attemptTerms := exploreCompactSourceLiteralTerms(terms)
	attemptTerms = appendExploreSourceLiteralProseTerms(attemptTerms, terms)
	if len(attemptTerms) == 0 {
		return exploreSourceLiteralRecall{}
	}

	recallBudget := s.sourceLiteralRecallBudget()
	started := time.Now()
	// The end-to-end wall grows with the admitted term count so a page that
	// quotes several distinct literals can ground each of them, while a page
	// with one literal keeps the original two-slice bound.
	wallSlices := time.Duration(max(2, len(attemptTerms)))
	boundedCtx, cancelBounded := context.WithTimeout(ctx, wallSlices*recallBudget)
	defer cancelBounded()
	type literalAttempt struct {
		term       string
		search     exploreSourceLiteralSearch
		recall     exploreSourceLiteralRecall
		searchErr  error
		mappingErr error
	}
	attempt := func(term string, budget time.Duration) literalAttempt {
		result := literalAttempt{term: term}
		if term == "" || boundedCtx.Err() != nil {
			return result
		}
		attemptCtx, cancelAttempt := context.WithTimeout(boundedCtx, budget)
		defer cancelAttempt()
		searchBudget := recallBudget
		if budget < searchBudget {
			searchBudget = budget
		}
		// Two-anchor recall gives each discovery the full per-anchor slice.
		// Mapping still shares attemptCtx, so the two attempts remain inside the
		// fixed 150ms request budget without silently halving grep time again.
		searchCtx, cancelSearch := context.WithTimeout(attemptCtx, searchBudget)
		result.search = s.searchExploreSourceLiteral(searchCtx, term, repoPrefix, scope, exploreSourceLiteralRecallMaxHits)
		result.searchErr = result.search.err
		if result.searchErr == nil {
			result.searchErr = searchCtx.Err()
		}
		cancelSearch()
		if ctx.Err() != nil {
			return result
		}
		result.recall, result.mappingErr = s.mapDiscoveredExploreSourceLiteralMatches(
			attemptCtx, term, result.search, scope, result.searchErr,
		)
		return result
	}

	aggregate := exploreSourceLiteralRecall{ownerFiles: make(map[string]string)}
	attempted := make(map[string]struct{}, len(attemptTerms)+1)
	results := make([]literalAttempt, 0, len(attemptTerms)+1)
	run := func(term string, anchor int, budget time.Duration) literalAttempt {
		result := attempt(term, budget)
		attempted[strings.ToLower(term)] = struct{}{}
		retained, retainedFiles, capReason := retainExploreSourceLiteralOwners(result.recall)
		reason := capReason
		switch {
		case result.searchErr != nil:
			reason = "search_deadline"
		case result.mappingErr != nil:
			reason = "mapping_deadline"
		case result.search.backend == "none" || !result.search.owned && len(result.search.matches) == 0:
			reason = "backend_unavailable"
		case len(result.search.matches) == 0:
			reason = "no_match"
		case len(result.recall.hits) == 0:
			reason = "no_mapped_owner"
		case reason == "" && result.search.incomplete:
			reason = "search_cap"
		}
		aggregate.diagnostics = append(aggregate.diagnostics, exploreSourceLiteralDiagnostic{
			literal:        term,
			rawHits:        len(result.search.matches),
			mappedOwners:   len(result.recall.hits),
			retainedOwners: len(retained),
			retainedFiles:  retainedFiles,
			reason:         reason,
		})
		for _, hit := range retained {
			hit.anchor = anchor
			aggregate.hits = append(aggregate.hits, hit)
			if file := result.recall.ownerFiles[hit.nodeID]; file != "" {
				aggregate.ownerFiles[hit.nodeID] = file
			}
		}
		aggregate.ambiguous = aggregate.ambiguous || result.recall.ambiguous
		results = append(results, result)
		return result
	}

	attemptBudget := 2 * recallBudget
	if len(attemptTerms) > 1 {
		attemptBudget = recallBudget
	}
	for index, term := range attemptTerms {
		if boundedCtx.Err() != nil {
			break
		}
		run(term, index, attemptBudget)
	}
	// Preserve the existing selective fallback when there is only one compact
	// anchor and it produces no settled owner. Two compact anchors already spend
	// the entire bounded term budget and are aggregated directly.
	if len(attemptTerms) == 1 && len(results) == 1 && boundedCtx.Err() == nil {
		primary := results[0]
		if fallback := exploreSourceLiteralFallback(terms, primary.term); fallback != "" &&
			(len(primary.recall.hits) == 0 || primary.recall.ambiguous || primary.search.incomplete) {
			run(fallback, 1, recallBudget)
		}
	}
	for _, term := range terms {
		if _, ok := attempted[strings.ToLower(term)]; ok {
			continue
		}
		reason := "term_cap"
		if boundedCtx.Err() != nil {
			reason = "request_deadline"
		}
		aggregate.diagnostics = append(aggregate.diagnostics, exploreSourceLiteralDiagnostic{
			literal: term,
			reason:  reason,
		})
	}
	if ctx.Err() != nil {
		return exploreSourceLiteralRecall{}
	}

	if s.logger != nil {
		rawHits, mappedOwners, retainedOwners := 0, 0, 0
		reasons := make(map[string]struct{})
		for _, diagnostic := range aggregate.diagnostics {
			rawHits += diagnostic.rawHits
			mappedOwners += diagnostic.mappedOwners
			retainedOwners += diagnostic.retainedOwners
			if diagnostic.reason != "" {
				reasons[diagnostic.reason] = struct{}{}
			}
		}
		reasonKeys := make([]string, 0, len(reasons))
		for reason := range reasons {
			reasonKeys = append(reasonKeys, reason)
		}
		sort.Strings(reasonKeys)
		termRunes := 0
		backend := "none"
		owned := false
		lookupRepoPrefix := ""
		firstMatchPath := ""
		contextError := ""
		if len(results) > 0 {
			termRunes = utf8.RuneCountInString(results[0].term)
			backend = results[0].search.backend
			owned = results[0].search.owned
			lookupRepoPrefix = results[0].search.lookupRepoPrefix
			if len(results[0].search.matches) > 0 {
				firstMatchPath = results[0].search.matches[0].Path
			}
			if results[0].searchErr != nil {
				contextError = "search: " + results[0].searchErr.Error()
			}
			if results[0].mappingErr != nil {
				if contextError != "" {
					contextError += "; "
				}
				contextError += "mapping: " + results[0].mappingErr.Error()
			}
		}
		fields := []zap.Field{
			zap.Int("term_runes", termRunes),
			zap.Int("attempted_terms", len(results)),
			zap.Int("diagnostic_terms", len(aggregate.diagnostics)),
			zap.String("reasons", strings.Join(reasonKeys, ",")),
			zap.String("requested_repo_prefix", repoPrefix),
			zap.String("lookup_repo_prefix", lookupRepoPrefix),
			zap.String("first_match_path", firstMatchPath),
			zap.String("backend", backend),
			zap.Bool("owned", owned),
			zap.Int("raw_matches", rawHits),
			zap.Int("mapped_symbols", mappedOwners),
			zap.Int("retained_symbols", retainedOwners),
			zap.Bool("incomplete", aggregate.ambiguous),
			zap.String("context_error", contextError),
			zap.Duration("elapsed", time.Since(started)),
		}
		if len(aggregate.hits) == 0 || aggregate.ambiguous || contextError != "" {
			s.logger.Info("mcp: explore source literal recall incomplete", fields...)
		} else {
			s.logger.Debug("mcp: explore source literal recall", fields...)
		}
	}
	return aggregate
}

func (s *Server) mapDiscoveredExploreSourceLiteralMatches(
	ctx context.Context,
	term string,
	search exploreSourceLiteralSearch,
	scope query.QueryOptions,
	discoveryErr error,
) (exploreSourceLiteralRecall, error) {
	mappingCtx, cancelMapping := context.WithTimeout(ctx, s.sourceLiteralRecallBudget())
	recall := s.mapExploreSourceLiteralMatchesContext(mappingCtx, term, search.matches, scope)
	mappingErr := mappingCtx.Err()
	cancelMapping()
	recall.ambiguous = recall.ambiguous || search.incomplete || discoveryErr != nil || mappingErr != nil
	return recall, mappingErr
}

func (s *Server) mapExploreSourceLiteralMatches(
	term string,
	matches []trigram.Match,
	scope query.QueryOptions,
) exploreSourceLiteralRecall {
	return s.mapExploreSourceLiteralMatchesContext(context.Background(), term, matches, scope)
}

func (s *Server) mapExploreSourceLiteralMatchesContext(
	ctx context.Context,
	term string,
	matches []trigram.Match,
	scope query.QueryOptions,
) exploreSourceLiteralRecall {
	if s == nil || len(matches) == 0 || ctx.Err() != nil {
		return exploreSourceLiteralRecall{}
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return exploreSourceLiteralRecall{}
	}
	saturated := len(matches) >= exploreSourceLiteralRecallMaxHits
	if len(matches) > exploreSourceLiteralRecallMaxHits {
		matches = matches[:exploreSourceLiteralRecallMaxHits]
	}

	// Multi-repo grep stamps paths with the repository prefix. During an
	// isolated single-repo session the graph can still contain unprefixed file
	// paths, so admit one exact, scope-derived alias into the same index build.
	// Exact paths remain authoritative; this is not a fuzzy suffix match and it
	// does not add another AllNodes scan.
	repoPrefix := exploreSourceLiteralSingleRepoPrefix(scope)
	exactPaths := make([]string, 0, len(matches))
	aliasPaths := make([]string, 0, len(matches))
	exactSeen := make(map[string]struct{}, len(matches))
	aliasSeen := make(map[string]struct{}, len(matches))
	aliases := make(map[string]string, len(matches))
	for _, match := range matches {
		if !exploreTextHasExactLiteral(match.Text, term) {
			continue
		}
		if _, duplicate := exactSeen[match.Path]; !duplicate {
			exactSeen[match.Path] = struct{}{}
			exactPaths = append(exactPaths, match.Path)
		}
		if alias := exploreSourceLiteralUnprefixedPath(match.Path, repoPrefix); alias != "" {
			aliases[match.Path] = alias
			if _, duplicate := aliasSeen[alias]; !duplicate {
				aliasSeen[alias] = struct{}{}
				aliasPaths = append(aliasPaths, alias)
			}
		}
	}
	orderedPaths := make([]string, 0, len(exactPaths)+len(aliasPaths))
	orderedPaths = append(orderedPaths, exactPaths...)
	for _, alias := range aliasPaths {
		if _, isExact := exactSeen[alias]; !isExact {
			orderedPaths = append(orderedPaths, alias)
		}
	}
	indexes := s.buildFileSymbolIndexForOrderedPathsScopedContext(ctx, orderedPaths, scope)
	type mappedLiteralOwner struct {
		owner    *graph.Node
		match    trigram.Match
		rank     int
		callName string
	}
	mapped := make([]mappedLiteralOwner, 0, len(matches))
	ownerSites := make([]graph.EdgeSourceSite, 0, len(matches))
	seenSites := make(map[graph.EdgeSourceSite]struct{}, len(matches))

	// First map each exact line to its smallest enclosing declaration. When
	// the literal is syntactically inside one call on that line, retain the
	// call name as a conservative hint; graph edges remain authoritative.
	// This parser is deliberately line-bounded, so multiline or otherwise
	// uncertain shapes keep the enclosing declaration instead of guessing.
	for rank, match := range matches {
		if !exploreTextHasExactLiteral(match.Text, term) {
			continue
		}
		index := indexes[match.Path]
		if index == nil {
			index = indexes[aliases[match.Path]]
		}
		if index == nil {
			continue
		}
		owner := index.smallestEnclosing(match.Line)
		if owner == nil || owner.ID == "" || !scope.ScopeAllows(owner) {
			continue
		}
		callName, _ := exploreSourceLiteralCallName(match.Text, term)
		mapped = append(mapped, mappedLiteralOwner{owner: owner, match: match, rank: rank, callName: callName})
		if callName != "" {
			site := graph.EdgeSourceSite{From: owner.ID, Line: match.Line}
			if _, duplicate := seenSites[site]; duplicate {
				continue
			}
			seenSites[site] = struct{}{}
			ownerSites = append(ownerSites, site)
		}
	}

	siteEdges := make(map[graph.EdgeSourceSite][]graph.EdgeIdentity)
	calleeNodes := map[string]*graph.Node{}
	if len(ownerSites) > 0 {
		bounded, supported := reader.(graph.BoundedOutgoingSiteEdgeIdentityReader)
		if supported {
			projection, err := bounded.FindOutgoingSiteEdgeIdentitiesBounded(
				ctx, ownerSites, []graph.EdgeKind{graph.EdgeCalls}, exploreSourceLiteralCallEdgesPerSite,
			)
			complete := err == nil && ctx.Err() == nil
			calleeIDs := make([]string, 0, exploreSourceLiteralCalleeHydrationLimit)
			seenCallees := make(map[string]struct{}, exploreSourceLiteralCalleeHydrationLimit)
			if complete {
				for _, site := range ownerSites {
					if projection.Truncated[site] {
						complete = false
						break
					}
					for _, identity := range projection.BySite[site] {
						if identity.To == "" {
							continue
						}
						if _, duplicate := seenCallees[identity.To]; duplicate {
							continue
						}
						if len(calleeIDs) == exploreSourceLiteralCalleeHydrationLimit {
							complete = false
							break
						}
						seenCallees[identity.To] = struct{}{}
						calleeIDs = append(calleeIDs, identity.To)
					}
					if !complete {
						break
					}
				}
			}
			if complete {
				if hydrated, hydratedComplete := exploreNodesByIDsBounded(
					ctx, reader, calleeIDs, exploreSourceLiteralCalleeHydrationLimit,
				); hydratedComplete {
					siteEdges = projection.BySite
					calleeNodes = hydrated
				}
			}
		}
	}

	seen := make(map[string]int, len(mapped))
	hits := make([]exploreSourceLiteralHit, 0, len(matches))
	ownerFiles := make(map[string]string, len(matches))
	for _, item := range mapped {
		node := item.owner
		calleeResolved := false
		if item.callName != "" {
			resolved := make(map[string]*graph.Node)
			site := graph.EdgeSourceSite{From: item.owner.ID, Line: item.match.Line}
			for _, identity := range siteEdges[site] {
				callee := calleeNodes[identity.To]
				if !exploreSourceLiteralLocalCallee(item.owner, callee, item.callName, scope) {
					continue
				}
				resolved[callee.ID] = callee
			}
			if len(resolved) == 1 {
				for _, callee := range resolved {
					node = callee
					calleeResolved = true
				}
			}
		}
		if existing, duplicate := seen[node.ID]; duplicate {
			if calleeResolved {
				hits[existing].callee = true
			}
			continue
		}
		seen[node.ID] = len(hits)
		hits = append(hits, exploreSourceLiteralHit{
			nodeID: node.ID, rank: item.rank, callee: calleeResolved,
			matchPath: item.match.Path, matchLine: item.match.Line, literal: term,
		})
		ownerFiles[node.ID] = node.FilePath
	}
	// Ambiguity is a property of the term's evidence, not of having company:
	// a complete search that maps to a handful of distinct owners has settled
	// each of them — a literal registered in one file and used in another is
	// two pieces of evidence, not mutual noise. Only a truncated search, or a
	// literal so common it overflows the owner cap, leaves owners unproven.
	ambiguous := saturated || len(hits) > exploreSourceLiteralRecallMaxOwnersPerTerm
	for index := range hits {
		hits[index].ambiguous = ambiguous
	}
	return exploreSourceLiteralRecall{
		hits:       hits,
		ambiguous:  ambiguous,
		ownerFiles: ownerFiles,
	}
}

// projectExploreSourceLiteralConstructionAlignment proves only complete,
// current-view instantiation adjacency for the already-bounded literal callee
// cohort. The full per-key projection cap is intentional: a truncated page may
// consist entirely of durable edges hidden by an overlay, so truncation can
// never be treated as a positive witness.
func projectExploreSourceLiteralConstructionAlignment(
	ctx context.Context,
	reader graph.Reader,
	ids []string,
) map[string]bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if reader == nil || ctx.Err() != nil {
		return nil
	}
	boundedIDs, complete := exploreBoundedNodeIDs(ids, exploreSourceLiteralRecallMaxHits)
	if !complete || len(boundedIDs) == 0 {
		return nil
	}
	bounded, supported := reader.(graph.BoundedOutgoingEdgeIdentityReader)
	if !supported {
		return nil
	}
	projection, err := bounded.FindOutgoingEdgeIdentitiesBounded(
		ctx,
		boundedIDs,
		[]graph.EdgeKind{graph.EdgeInstantiates},
		graph.MaxBoundedAdjacencyRowsPerKey,
	)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	aligned := make(map[string]bool, len(boundedIDs))
	for _, id := range boundedIDs {
		if projection.Truncated[id] {
			continue
		}
		if len(projection.ByEndpoint[id]) > 0 {
			aligned[id] = true
		}
	}
	return aligned
}

type exploreSourceLiteralSpan struct {
	start int
	end   int
}

// exploreSourceLiteralCallName returns the one direct call containing every
// exact quoted occurrence of term on a source line. It intentionally declines
// multiline calls, assignments, control-flow parentheses, and lines where the
// literal participates in multiple calls. Those shapes retain the enclosing
// callable and never receive callee promotion.
func exploreSourceLiteralCallName(line, term string) (string, bool) {
	spans := exploreSourceLiteralQuotedSpans(line, term)
	if len(spans) == 0 {
		return "", false
	}
	pairs := exploreSourceLiteralParenPairs(line)
	names := make(map[string]string, len(spans))
	for _, span := range spans {
		bestOpen := -1
		for _, pair := range pairs {
			if pair.start < span.start && pair.end > span.end && pair.start > bestOpen {
				bestOpen = pair.start
			}
		}
		if bestOpen < 0 {
			return "", false
		}
		name := exploreSourceLiteralIdentifierBefore(line, bestOpen)
		if name == "" || exploreSourceLiteralControlWord(name) {
			return "", false
		}
		names[strings.ToLower(name)] = name
	}
	if len(names) != 1 {
		return "", false
	}
	for _, name := range names {
		return name, true
	}
	return "", false
}

func exploreSourceLiteralQuotedSpans(line, term string) []exploreSourceLiteralSpan {
	spans := make([]exploreSourceLiteralSpan, 0, 1)
	for index := 0; index < len(line); {
		quote := line[index]
		if quote != '\'' && quote != '"' && quote != '`' {
			index++
			continue
		}
		start := index
		index++
		contentStart := index
		for index < len(line) {
			if line[index] == '\\' {
				index += 2
				continue
			}
			if line[index] == quote {
				if line[contentStart:index] == term {
					spans = append(spans, exploreSourceLiteralSpan{start: start, end: index})
				}
				index++
				break
			}
			index++
		}
	}
	return spans
}

func exploreSourceLiteralParenPairs(line string) []exploreSourceLiteralSpan {
	stack := make([]int, 0, 4)
	pairs := make([]exploreSourceLiteralSpan, 0, 4)
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '\'', '"', '`':
			quote := line[index]
			for index++; index < len(line); index++ {
				if line[index] == '\\' {
					index++
					continue
				}
				if line[index] == quote {
					break
				}
			}
		case '(':
			stack = append(stack, index)
		case ')':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			pairs = append(pairs, exploreSourceLiteralSpan{start: open, end: index})
		}
	}
	return pairs
}

func exploreSourceLiteralIdentifierBefore(line string, end int) string {
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	start := end
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:start])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
			break
		}
		start -= size
	}
	return line[start:end]
}

func exploreSourceLiteralControlWord(name string) bool {
	switch strings.ToLower(name) {
	case "catch", "do", "for", "foreach", "if", "match", "new", "return", "sizeof", "switch", "typeof", "while", "with":
		return true
	default:
		return false
	}
}

func exploreSourceLiteralLocalCallee(owner, callee *graph.Node, callName string, scope query.QueryOptions) bool {
	if owner == nil || callee == nil || callee.ID == "" || callee.ID == owner.ID || callee.FilePath == "" || !scope.ScopeAllows(callee) {
		return false
	}
	if callee.Kind != graph.KindFunction && callee.Kind != graph.KindMethod {
		return false
	}
	if scope.ExcludeTests && exploreDraftIsTestNode(callee) {
		return false
	}
	if callee.RepoPrefix != owner.RepoPrefix {
		return false
	}
	name := callee.Name
	if separator := strings.LastIndexAny(name, ".:#/"); separator >= 0 {
		name = name[separator+1:]
	}
	return strings.EqualFold(name, callName)
}

func exploreSourceLiteralSingleRepoPrefix(scope query.QueryOptions) string {
	prefix := ""
	for candidate, allowed := range scope.RepoAllow {
		if !allowed {
			continue
		}
		candidate = strings.TrimSuffix(strings.TrimSpace(candidate), "/")
		if candidate == "" {
			continue
		}
		if prefix != "" && prefix != candidate {
			return ""
		}
		prefix = candidate
	}
	return prefix
}

func exploreSourceLiteralUnprefixedPath(path, repoPrefix string) string {
	marker := repoPrefix + "/"
	if repoPrefix == "" || !strings.HasPrefix(path, marker) || len(path) == len(marker) {
		return ""
	}
	return strings.TrimPrefix(path, marker)
}

func (s *Server) filterExploreSourceLiteralMatches(ctx context.Context, matches []trigram.Match) []trigram.Match {
	if OverlayViewFromContext(ctx) == nil || len(matches) == 0 {
		return matches
	}
	filtered := matches[:0:0]
	for _, match := range matches {
		if !s.overlayShadowsGraphPath(ctx, match.Path, "") {
			filtered = append(filtered, match)
		}
	}
	return filtered
}

func finalizeExploreSourceLiteralSearch(
	result exploreSourceLiteralSearch,
	overlayScan exploreSourceLiteralOverlayScan,
	covered map[string]struct{},
	maxHits int,
	overlayPresent bool,
	overlayIncomplete bool,
	err error,
) exploreSourceLiteralSearch {
	if maxHits <= 0 || maxHits > exploreSourceLiteralOverlayMaxHits {
		maxHits = exploreSourceLiteralOverlayMaxHits
	}
	if len(covered) == 0 && len(overlayScan.matches) == 0 {
		if len(result.matches) > maxHits {
			result.matches = result.matches[:maxHits]
			result.incomplete = true
		}
		result.incomplete = result.incomplete || overlayIncomplete || overlayScan.incomplete || err != nil
		result.err = err
		if overlayPresent {
			result.backend += "+overlay"
		}
		return result
	}
	merged, mergeIncomplete := mergeExploreSourceLiteralMatches(
		overlayScan.matches, result.matches, covered, maxHits,
	)
	result.matches = merged
	result.incomplete = result.incomplete || overlayIncomplete || overlayScan.incomplete || mergeIncomplete || err != nil
	result.err = err
	if overlayPresent {
		result.backend += "+overlay"
		result.owned = result.owned || len(overlayScan.matches) > 0
	}
	return result
}

type exploreSourceLiteralOverlayScanner func(
	context.Context, string, []exploreSourceLiteralOverlayFile, int,
) (exploreSourceLiteralOverlayScan, error)

type exploreSourceLiteralDurableSearcher func(
	context.Context, string, string, int,
) exploreSourceLiteralSearch

func runExploreSourceLiteralLanes(
	ctx context.Context,
	term string,
	repoPrefix string,
	maxHits int,
	overlayFiles []exploreSourceLiteralOverlayFile,
	covered map[string]struct{},
	overlayIncomplete bool,
	coveredOverflow bool,
	scanOverlay exploreSourceLiteralOverlayScanner,
	searchDurable exploreSourceLiteralDurableSearcher,
) exploreSourceLiteralSearch {
	if maxHits <= 0 || maxHits > exploreSourceLiteralOverlayMaxHits {
		maxHits = exploreSourceLiteralOverlayMaxHits
	}
	if coveredOverflow {
		return exploreSourceLiteralSearch{
			backend: "overlay-covered-cap", lookupRepoPrefix: repoPrefix, incomplete: true, owned: true,
		}
	}
	if len(overlayFiles) == 0 && len(covered) == 0 {
		result := searchDurable(ctx, term, repoPrefix, maxHits+1)
		return finalizeExploreSourceLiteralSearch(
			result, exploreSourceLiteralOverlayScan{}, covered, maxHits, false,
			overlayIncomplete, ctx.Err(),
		)
	}

	overlayScan, scanErr := scanOverlay(ctx, term, overlayFiles, maxHits)
	if scanErr != nil {
		return finalizeExploreSourceLiteralSearch(
			exploreSourceLiteralSearch{backend: "none", lookupRepoPrefix: repoPrefix},
			overlayScan, covered, maxHits, true, overlayIncomplete, scanErr,
		)
	}
	if err := ctx.Err(); err != nil {
		return finalizeExploreSourceLiteralSearch(
			exploreSourceLiteralSearch{backend: "none", lookupRepoPrefix: repoPrefix},
			overlayScan, covered, maxHits, true, overlayIncomplete, err,
		)
	}
	if len(overlayScan.matches) >= maxHits {
		allProduction := true
		for _, match := range overlayScan.matches[:maxHits] {
			if testpath.IsTestFile(match.Path) {
				allProduction = false
				break
			}
		}
		if allProduction {
			overlayScan.incomplete = true // Durable existence is deliberately not probed.
			return finalizeExploreSourceLiteralSearch(
				exploreSourceLiteralSearch{backend: "none", lookupRepoPrefix: repoPrefix},
				overlayScan, covered, maxHits, true, overlayIncomplete, nil,
			)
		}
	}

	result := searchDurable(ctx, term, repoPrefix, maxHits+len(covered)+1)
	return finalizeExploreSourceLiteralSearch(
		result, overlayScan, covered, maxHits, true, overlayIncomplete, ctx.Err(),
	)
}

func (s *Server) searchExploreSourceLiteralDurable(
	ctx context.Context,
	term string,
	repoPrefix string,
	durableLimit int,
) exploreSourceLiteralSearch {
	result := exploreSourceLiteralSearch{backend: "none", lookupRepoPrefix: repoPrefix}
	if s.multiIndexer != nil {
		durable := s.multiIndexer.GrepLiteralForRepoBounded(
			ctx, repoPrefix, term, durableLimit, exploreSourceLiteralRecallMaxFiles,
		)
		if durable.Owned {
			return exploreSourceLiteralSearch{
				matches:          s.filterExploreSourceLiteralMatches(ctx, durable.Matches),
				incomplete:       durable.Incomplete || len(durable.Matches) >= durableLimit,
				backend:          "multi",
				owned:            true,
				lookupRepoPrefix: durable.RepoPrefix,
			}
		}
		if durable.Configured {
			return exploreSourceLiteralSearch{backend: "multi-unresolved", lookupRepoPrefix: repoPrefix}
		}
	}
	if s.indexer != nil {
		matches, incomplete := s.indexer.GrepLiteralBounded(
			ctx, term, durableLimit, exploreSourceLiteralRecallMaxFiles,
		)
		return exploreSourceLiteralSearch{
			matches:          s.filterExploreSourceLiteralMatches(ctx, matches),
			incomplete:       incomplete || len(matches) >= durableLimit,
			backend:          "direct",
			owned:            true,
			lookupRepoPrefix: s.indexer.RepoPrefix(),
		}
	}
	return result
}

// searchExploreSourceLiteral mirrors search_text's literal backend while
// deliberately refusing an unscoped multi-repository fan-out. The caller's
// session locality supplies repoPrefix in normal operation. maxHits is the
// caller's own recall cap — every caller is responsible for a bound the rest
// of its response budget can absorb.
func (s *Server) searchExploreSourceLiteral(
	ctx context.Context,
	term string,
	repoPrefix string,
	scope query.QueryOptions,
	maxHits int,
) exploreSourceLiteralSearch {
	if err := ctx.Err(); err != nil {
		return exploreSourceLiteralSearch{backend: "none", lookupRepoPrefix: repoPrefix, err: err}
	}
	if maxHits <= 0 || maxHits > exploreSourceLiteralOverlayMaxHits {
		maxHits = exploreSourceLiteralOverlayMaxHits
	}
	// A request routed to a view of its own answers out of that view's bytes
	// or not at all. The lanes below read the canonical checkout, whose lines
	// the view may have renamed or deleted (view_source_literal.go).
	if view := requestViewFromContext(ctx); view.readsOwnCheckout() {
		return s.searchSourceLiteralInView(ctx, view, term, maxHits)
	}
	var scopeOK bool
	scope, scopeOK = s.effectiveExploreSourceLiteralScope(ctx, scope)
	if !scopeOK {
		return exploreSourceLiteralSearch{backend: "session-scope-mismatch", lookupRepoPrefix: repoPrefix}
	}
	resolvedRepoPrefix, prefixOK := s.resolveExploreSourceLiteralRepoPrefix(repoPrefix, scope)
	if !prefixOK {
		return exploreSourceLiteralSearch{backend: "multi-ambiguous-scope", lookupRepoPrefix: repoPrefix}
	}
	repoPrefix = resolvedRepoPrefix
	overlayFiles, covered, overlayIncomplete, coveredOverflow, overlayErr := s.snapshotExploreSourceLiteralOverlays(ctx, scope, repoPrefix)
	if overlayErr != nil {
		return exploreSourceLiteralSearch{
			backend: "overlay", lookupRepoPrefix: repoPrefix, incomplete: true, owned: true, err: overlayErr,
		}
	}
	return runExploreSourceLiteralLanes(
		ctx, term, repoPrefix, maxHits, overlayFiles, covered,
		overlayIncomplete, coveredOverflow,
		scanExploreSourceLiteralOverlays, s.searchExploreSourceLiteralDurable,
	)
}
