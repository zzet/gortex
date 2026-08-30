package mcp

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

const (
	exploreSyntacticAnchorMaxTerms         = 3
	exploreSyntacticAnchorFetch            = 10
	exploreSyntacticAnchorCompetitionFetch = 32
	// A qualified leaf may inspect only the first four ranked files that
	// contain an exact owner declaration.
	exploreQualifiedLeafMaxFiles = 4
)

// exploreSyntacticAnchor is a bounded, implementation-shaped clue copied
// directly from the task. CLI flags and identifier-shaped tokens survive
// prose query shaping poorly, but often name the missing implementation more
// precisely than the surrounding natural language.
type exploreSyntacticAnchor struct {
	query         string
	source        string
	terms         []string
	compact       string
	qualifiedName string
	// exactNodes carries the indexed declarations a plain task token named
	// verbatim, so the anchor lane never repeats the name lookup that found it.
	exactNodes []*graph.Node
	// mined marks an anchor read out of a pasted code block rather than spelled
	// by the requester. It retrieves and ranks like any other anchor, but the
	// gates that withhold an answer ignore it: a stack frame can name a
	// third-party symbol this repository will never contain.
	mined bool
}

// exploreSyntacticAnchors extracts only syntactically strong, unquoted clues.
// Flags lead because they are explicit user-facing identifiers; snake/camel/
// qualified identifiers follow in first-seen order. Prefix-equivalent forms
// collapse into one anchor, so --replace, replace_all, and Replacer consume a
// single bounded retrieval lane rather than three. Identifiers mined from code
// blocks come last: query shaping discards those blocks, so a stack frame or
// snippet is the only remaining spelling of the symbol it names, but prose the
// author wrote deliberately still owns the slots it can fill.
func exploreSyntacticAnchors(task string) []exploreSyntacticAnchor {
	out := make([]exploreSyntacticAnchor, 0, exploreSyntacticAnchorMaxTerms)
	tokens := exploreUnquotedCodeTokens(task)
	add := func(raw string, mined bool) {
		anchor, ok := newExploreSyntacticAnchor(raw)
		if !ok {
			return
		}
		for _, existing := range out {
			if exploreSyntacticAnchorEquivalent(existing, anchor) {
				return
			}
		}
		anchor.mined = mined
		out = append(out, anchor)
	}

	for _, token := range tokens {
		if !strings.HasPrefix(token, "--") || len(token) <= 2 {
			continue
		}
		flag := strings.TrimLeft(token, "-")
		if assignment := strings.IndexByte(flag, '='); assignment >= 0 {
			flag = flag[:assignment]
		}
		add(flag, false)
		if len(out) == exploreSyntacticAnchorMaxTerms {
			return out
		}
	}

	for lane, laneTokens := range [2][]string{tokens, exploreFencedCodeTokens(task)} {
		for _, token := range laneTokens {
			if strings.HasPrefix(token, "--") {
				continue
			}
			if exploreSyntacticAnchorRuntimeDataPath(token) || !exploreCodeShapedToken(token) {
				continue
			}
			add(token, lane > 0)
			if len(out) == exploreSyntacticAnchorMaxTerms {
				return out
			}
		}
	}
	return out
}

func newExploreSyntacticAnchor(raw string) (exploreSyntacticAnchor, bool) {
	raw = strings.Trim(raw, " \t\r\n()[]{}<>,.;:'\"")
	// Stack traces commonly suffix an otherwise exact source anchor with a
	// decimal line/column (`tree.go:637` or `tree.go:637:12`). Those coordinates
	// are not identifier terms; retaining them makes both lexical lookup and
	// node matching miss the named file.
	for {
		colon := strings.LastIndexByte(raw, ':')
		if colon < 0 || colon == len(raw)-1 {
			break
		}
		digits := true
		for _, r := range raw[colon+1:] {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if !digits {
			break
		}
		raw = raw[:colon]
	}
	if raw == "" {
		return exploreSyntacticAnchor{}, false
	}
	seen := make(map[string]struct{})
	terms := make([]string, 0, 4)
	for _, token := range rerank.Tokenize(raw) {
		term := strings.ToLower(strings.TrimSpace(token))
		if len(term) < 3 {
			continue
		}
		if _, duplicate := seen[term]; duplicate {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return exploreSyntacticAnchor{}, false
	}
	compact := strings.Join(terms, "")
	if len(compact) < 4 || exploreSyntacticAnchorNoise(compact) {
		return exploreSyntacticAnchor{}, false
	}
	queryTerms := append([]string(nil), terms...)
	if len(terms) > 1 {
		queryTerms = append(queryTerms, strings.Join(terms, "_"), compact)
	}
	query := strings.Join(queryTerms, " ")
	qualifiedName := ""
	if strings.Contains(raw, "::") {
		query = strings.Join(strings.Fields(strings.ReplaceAll(raw, "::", " ")), " ")
		qualifiedName = strings.ReplaceAll(strings.Join(strings.Fields(raw), ""), "::", ".")
	} else if dotted, ok := exploreDottedQualifiedMention(raw); ok {
		query = strings.Join(strings.Fields(strings.ReplaceAll(raw, ".", " ")), " ")
		qualifiedName = dotted
	}
	return exploreSyntacticAnchor{
		query:         query,
		source:        strings.ToLower(raw),
		terms:         terms,
		compact:       compact,
		qualifiedName: qualifiedName,
	}, true
}

func exploreSyntacticAnchorNoise(compact string) bool {
	switch compact {
	case "csharp", "debug", "default", "dotnet", "example", "file", "files", "github", "gitlab", "golang", "help", "javascript", "kotlin", "linux", "macos", "option", "options", "path", "python", "rust", "test", "tests", "typescript", "verbose", "version", "windows":
		return true
	default:
		return false
	}
}

func exploreSyntacticAnchorEquivalent(left, right exploreSyntacticAnchor) bool {
	if left.compact == right.compact {
		return true
	}
	leftQualified := left.qualifiedName != ""
	rightQualified := right.qualifiedName != ""
	if leftQualified != rightQualified {
		qualified, plain := left, right
		if !leftQualified {
			qualified, plain = right, left
		}
		// An explicitly qualified member is complementary to its owner, not a
		// spelling variant of it. Keep both retrieval lanes so a task naming
		// HipChatHandler and HipChatHandler::buildContent can surface the class
		// context and the requested method independently.
		if strings.HasPrefix(qualified.compact, plain.compact) {
			return false
		}
	}
	shorter, longer := left.compact, right.compact
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 5 && strings.HasPrefix(longer, shorter)
}

func exploreUnquotedCodeTokens(task string) []string {
	var masked strings.Builder
	masked.Grow(len(task))
	var quote rune
	escaped := false
	for _, r := range task {
		if quote != 0 {
			if escaped {
				escaped = false
				masked.WriteRune(' ')
				continue
			}
			if r == '\\' && quote == '"' {
				escaped = true
				masked.WriteRune(' ')
				continue
			}
			if r == quote {
				quote = 0
			}
			masked.WriteRune(' ')
			continue
		}
		if r == '"' || r == '`' {
			quote = r
			masked.WriteRune(' ')
			continue
		}
		masked.WriteRune(r)
	}
	return strings.FieldsFunc(masked.String(), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' && r != ':' && r != '-'
	})
}

const (
	exploreSourceRangeSignal      = "explore_source_range"
	exploreSourceRangeMaxAnchors  = 4
	exploreSourceRangeContextSize = 320
	exploreSourceRangeMaxSpan     = 512
	exploreSourceRangeMaxAliases  = 4
)

var (
	exploreSourceRangeLineRE   = regexp.MustCompile(`(?i)\blines?\s+([0-9]{1,8})(?:(?:\s+to\s+|\s*[-–—]\s*)([0-9]{1,8}))?`)
	exploreSourceRangeInlineRE = regexp.MustCompile(`^\s*:([0-9]{1,8})(?:[-–—]([0-9]{1,8}))?`)
)

// exploreSourceRangeSpecs pairs an explicit source path with the line citation
// immediately following it. GitHub issue bodies render these as a path and a
// later "Lines X to Y" fragment; stack traces commonly use path:line instead.
// Both shapes are bounded and ignored unless the path has a source extension.
func exploreSourceRangeSpecs(task string) []rangeSpec {
	matches := exploreArtifactPathRE.FindAllStringIndex(task, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, exploreSourceRangeMaxAnchors)
	out := make([]rangeSpec, 0, exploreSourceRangeMaxAnchors)
	for index, match := range matches {
		path := strings.Trim(task[match[0]:match[1]], "`'\"()[]{}<>,;")
		normalizedPath := strings.ReplaceAll(path, "\\", "/")
		if !exploreSourceExtension(filepath.Ext(normalizedPath)) {
			continue
		}
		tailEnd := len(task)
		if index+1 < len(matches) {
			tailEnd = matches[index+1][0]
		}
		if limit := match[1] + exploreSourceRangeContextSize; tailEnd > limit {
			tailEnd = limit
		}
		start, end, ok := exploreSourceRangeLines(task[match[1]:tailEnd])
		if !ok {
			continue
		}
		key := strings.ToLower(normalizedPath) + ":" + strconv.Itoa(start) + "-" + strconv.Itoa(end)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, rangeSpec{File: path, StartLine: start, EndLine: end})
		if len(out) == exploreSourceRangeMaxAnchors {
			break
		}
	}
	return out
}

func exploreSourceRangeLines(tail string) (int, int, bool) {
	match := exploreSourceRangeInlineRE.FindStringSubmatch(tail)
	if match == nil {
		match = exploreSourceRangeLineRE.FindStringSubmatch(tail)
	}
	if len(match) < 2 {
		return 0, 0, false
	}
	start, err := strconv.Atoi(match[1])
	if err != nil || start <= 0 {
		return 0, 0, false
	}
	end := start
	if len(match) > 2 && match[2] != "" {
		if parsed, parseErr := strconv.Atoi(match[2]); parseErr == nil && parsed >= start {
			end = parsed
		}
	}
	if end-start >= exploreSourceRangeMaxSpan {
		end = start + exploreSourceRangeMaxSpan - 1
	}
	return start, end, true
}

// exploreSourceRangeDefinitions chooses the narrowest editable declaration at
// each cited line. Anonymous closures are implementation detail: when a cited
// line falls inside one, the enclosing named method/function remains the useful
// localization symbol (for example FingersCrossedHandler::flushBuffer).
func exploreSourceRangeDefinitions(index *fileSymbolIndex, start, end int) []*graph.Node {
	if index == nil || index.saturated {
		return nil
	}
	if end < start {
		end = start
	}
	if end-start >= exploreSourceRangeMaxSpan {
		end = start + exploreSourceRangeMaxSpan - 1
	}
	seen := make(map[string]struct{})
	out := make([]*graph.Node, 0, 2)
	for line := start; line <= end; line++ {
		var best *graph.Node
		bestSpan := int(^uint(0) >> 1)
		for _, node := range index.syms {
			if node == nil {
				continue
			}
			if node.StartLine > line {
				break
			}
			if node.Kind == graph.KindClosure || node.EndLine < line {
				continue
			}
			span := node.EndLine - node.StartLine
			if best == nil || span < bestSpan {
				best = node
				bestSpan = span
			}
		}
		if best == nil {
			best = index.smallestEnclosing(line)
		}
		if best == nil || best.ID == "" {
			continue
		}
		if _, duplicate := seen[best.ID]; duplicate {
			continue
		}
		seen[best.ID] = struct{}{}
		out = append(out, best)
	}
	return out
}

// exploreSourceRangeGraphPathAliases returns a bounded exact-to-suffix lookup
// order. Benchmark and issue text often prefixes paths with an isolated repo
// label (for example monolog/src/... while a lone repo indexes src/...). The
// exact path always wins; suffixes are only fallback lookup keys.
func exploreSourceRangeGraphPathAliases(graphPath string) []string {
	clean := filepath.ToSlash(filepath.Clean(graphPath))
	if clean == "." || clean == "/" || clean == "" {
		return nil
	}
	clean = strings.TrimPrefix(clean, "./")
	out := make([]string, 0, exploreSourceRangeMaxAliases)
	for len(out) < exploreSourceRangeMaxAliases && clean != "" {
		out = append(out, clean)
		slash := strings.IndexByte(clean, '/')
		if slash < 0 || slash+1 >= len(clean) {
			break
		}
		clean = clean[slash+1:]
	}
	return out
}

// exploreSourceRangeScopedGraphPathAliases maps a task's logical repo label to
// the sole request-scoped graph prefix without consulting the filesystem. Replay
// workspaces commonly index monolog/src/... as monolog-1800/src/.... A resolved
// exact spelling remains authoritative; the scoped spelling is only a bounded
// fallback. Multiple-repo scopes and paths resolved to a foreign repo never use
// that fallback.
func exploreSourceRangeScopedGraphPathAliases(
	rawPath, resolvedGraphPath, resolvedRepoPrefix string,
	scope query.QueryOptions,
) []string {
	resolved := exploreSourceRangeGraphPathAliases(resolvedGraphPath)
	if resolvedRepoPrefix != "" && len(scope.RepoAllow) > 0 &&
		!repoNarrowAdmits(scope.RepoAllow, resolvedRepoPrefix) {
		return nil
	}
	repoPrefix := exploreSourceLiteralSingleRepoPrefix(scope)
	normalizedRaw := strings.ReplaceAll(strings.TrimSpace(rawPath), "\\", "/")
	if exploreSourceRangeTraversalPath(normalizedRaw) {
		return nil
	}
	if repoPrefix == "" {
		return resolved
	}
	if exploreSourceRangeAbsolutePath(normalizedRaw) {
		if len(resolved) == 0 {
			return nil
		}
		return resolved[:1]
	}
	raw := filepath.ToSlash(filepath.Clean(normalizedRaw))
	raw = strings.TrimPrefix(raw, "./")
	if raw == "" || raw == "." || raw == ".." || strings.HasPrefix(raw, "../") {
		return resolved
	}

	relative := raw
	if strings.HasPrefix(relative, repoPrefix+"/") {
		relative = strings.TrimPrefix(relative, repoPrefix+"/")
	} else if slash := strings.IndexByte(relative, '/'); slash > 0 &&
		exploreSourceRangeLogicalRepoLabel(repoPrefix, relative[:slash]) {
		relative = relative[slash+1:]
	}
	scopedPath := repoPrefix + "/" + relative
	out := make([]string, 0, exploreSourceRangeMaxAliases)
	seen := make(map[string]struct{}, exploreSourceRangeMaxAliases)
	add := func(path string) {
		if path == "" || len(out) == exploreSourceRangeMaxAliases {
			return
		}
		if _, duplicate := seen[path]; duplicate {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	if len(resolved) > 0 {
		add(resolved[0])
	}
	add(scopedPath)
	return out
}

func exploreSourceRangeAbsolutePath(path string) bool {
	if path == "" {
		return false
	}
	if filepath.IsAbs(filepath.FromSlash(path)) || strings.HasPrefix(path, "//") {
		return true
	}
	return len(path) >= 2 && path[1] == ':' &&
		((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z'))
}

func exploreSourceRangeTraversalPath(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func exploreSourceRangeLogicalRepoLabel(repoPrefix, label string) bool {
	if strings.EqualFold(repoPrefix, label) {
		return true
	}
	if len(repoPrefix) <= len(label)+1 || repoPrefix[len(label)] != '-' ||
		!strings.EqualFold(repoPrefix[:len(label)], label) {
		return false
	}
	for _, r := range repoPrefix[len(label)+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// exploreSourceRangeIndex chooses the exact indexed path when present. If it is
// absent, exactly one suffix alias may resolve; multiple suffix hits are
// deliberately rejected so an explicit citation never promotes an ambiguous
// same-named file.
func exploreSourceRangeIndex(indexes map[string]*fileSymbolIndex, aliases []string) *fileSymbolIndex {
	if len(aliases) == 0 {
		return nil
	}
	if exact := indexes[aliases[0]]; exact != nil {
		return exact
	}
	var resolved *fileSymbolIndex
	for _, alias := range aliases[1:] {
		index := indexes[alias]
		if index == nil {
			continue
		}
		if resolved != nil && resolved != index {
			return nil
		}
		resolved = index
	}
	return resolved
}

// promoteExploreSourceRangeCandidates puts task-cited declarations ahead of
// semantic neighbours without reranking the completed search. Existing
// candidates retain their scores; missing exact declarations are admitted with
// neutral ranks. The syntactic marker protects and labels this task-spelled
// evidence through the existing bounded selection and rendering policy.
func (s *Server) promoteExploreSourceRangeCandidates(
	ctx context.Context,
	task string,
	ordinary []*rerank.Candidate,
	reader graph.Reader,
	scope query.QueryOptions,
) []*rerank.Candidate {
	specs := exploreSourceRangeSpecs(task)
	if s == nil || reader == nil || len(specs) == 0 || ctx.Err() != nil {
		return ordinary
	}
	type resolvedRange struct {
		spec       rangeSpec
		graphPaths []string
	}
	resolved := make([]resolvedRange, 0, len(specs))
	orderedPaths := make([]string, 0, len(specs)*exploreSourceRangeMaxAliases)
	seenPaths := make(map[string]struct{}, len(specs)*exploreSourceRangeMaxAliases)
	for _, spec := range specs {
		absPath, relPath, err := s.resolveFilePath(ctx, spec.File)
		resolvedGraphPath := ""
		resolvedRepoPrefix := ""
		if err == nil {
			resolvedGraphPath = s.resolveOverlayGraphPathForRequest(ctx, relPath, absPath)
			if s.multiIndexer != nil {
				resolvedRepoPrefix = matchedRepoPrefix(s.multiIndexer, relPath)
			}
		}
		graphPaths := exploreSourceRangeScopedGraphPathAliases(
			spec.File, resolvedGraphPath, resolvedRepoPrefix, scope,
		)
		if len(graphPaths) == 0 {
			continue
		}
		resolved = append(resolved, resolvedRange{spec: spec, graphPaths: graphPaths})
		for _, graphPath := range graphPaths {
			if _, duplicate := seenPaths[graphPath]; !duplicate {
				seenPaths[graphPath] = struct{}{}
				orderedPaths = append(orderedPaths, graphPath)
			}
		}
	}
	if len(resolved) == 0 {
		return ordinary
	}
	indexes := s.buildFileSymbolIndexForOrderedPathsScopedReaderContext(ctx, reader, orderedPaths, scope)
	if ctx.Err() != nil {
		return ordinary
	}
	exactNodes := make([]*graph.Node, 0, len(resolved))
	seenNodes := make(map[string]struct{}, len(resolved))
	for _, item := range resolved {
		if ctx.Err() != nil {
			return ordinary
		}
		index := exploreSourceRangeIndex(indexes, item.graphPaths)
		for _, node := range exploreSourceRangeDefinitions(index, item.spec.StartLine, item.spec.EndLine) {
			if node == nil || !exploreLocalizableKind(node.Kind) || !scope.ScopeAllows(node) ||
				!s.nodeInSessionScope(ctx, node) {
				continue
			}
			if _, duplicate := seenNodes[node.ID]; duplicate {
				continue
			}
			seenNodes[node.ID] = struct{}{}
			exactNodes = append(exactNodes, node)
			if len(exactNodes) == exploreSourceRangeMaxAnchors {
				break
			}
		}
		if len(exactNodes) == exploreSourceRangeMaxAnchors {
			break
		}
	}
	if ctx.Err() != nil || len(exactNodes) == 0 {
		return ordinary
	}
	ordinaryByID := make(map[string]*rerank.Candidate, len(ordinary))
	for _, candidate := range ordinary {
		if candidate != nil && candidate.Node != nil {
			ordinaryByID[candidate.Node.ID] = candidate
		}
	}
	out := make([]*rerank.Candidate, 0, len(ordinary)+len(exactNodes))
	promoted := make(map[string]struct{}, len(exactNodes))
	for _, node := range exactNodes {
		candidate := &rerank.Candidate{Node: node, TextRank: -1, VectorRank: -1}
		if existing := ordinaryByID[node.ID]; existing != nil {
			clone := *existing
			candidate = &clone
		}
		markExploreSyntacticAnchorCandidate(candidate)
		candidate.Signals[exploreSourceRangeSignal] = 1
		out = append(out, candidate)
		promoted[node.ID] = struct{}{}
	}
	for _, candidate := range ordinary {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		if _, exact := promoted[candidate.Node.ID]; exact {
			continue
		}
		out = append(out, candidate)
	}
	if ctx.Err() != nil {
		return ordinary
	}
	return out
}

func exploreSyntacticAnchorRuntimeDataPath(token string) bool {
	lower := strings.ToLower(strings.Trim(token, "-_.:()[]{}<>,;'\""))
	for _, suffix := range []string{".txt", ".log", ".csv", ".tsv"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func exploreCodeShapedToken(token string) bool {
	token = strings.Trim(token, "-_.:")
	first, _ := utf8.DecodeRuneInString(token)
	if token == "" || len(token) > 128 || (!unicode.IsLetter(first) && first != '_') {
		return false
	}
	lower := strings.ToLower(token)
	for _, suffix := range []string{".ai", ".com", ".dev", ".io", ".net", ".org"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	// Route-registration examples such as r.GET("/users/:id") describe
	// runtime input, not implementation anchors. They otherwise consume the
	// three-slot syntactic budget ahead of a later stack-trace file/symbol.
	if dot := strings.LastIndex(token, "."); dot >= 0 && dot < len(token)-1 {
		member := token[dot+1:]
		if member == strings.ToUpper(member) {
			switch member {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE":
				return false
			}
		}
	}
	if strings.Contains(token, "_") || strings.Contains(token, "::") || strings.Contains(token, ".") {
		return true
	}
	var previous rune
	for _, r := range token {
		if previous != 0 && (unicode.IsLower(previous) || unicode.IsDigit(previous)) && unicode.IsUpper(r) {
			return true
		}
		previous = r
	}
	return false
}

func exploreSyntacticAnchorMatchesNode(anchor exploreSyntacticAnchor, node *graph.Node) bool {
	if node == nil {
		return false
	}
	return exploreSyntacticAnchorMatchesIdentifier(anchor, node.Name) ||
		exploreSyntacticAnchorMatchesIdentifier(anchor, node.QualName) ||
		(anchor.qualifiedName != "" && exploreSyntacticAnchorMatchesIdentifier(anchor, node.ID)) ||
		exploreSyntacticAnchorMatchesPath(anchor, node)
}

// exploreTaskQualifiedSyntacticAnchorMatchesNode checks the untouched task so a
// late Owner::member anchor survives long-report query shaping and truncation.
// Only explicit qualified members use this route; ordinary prose and bare
// identifiers continue to rely on the shaped query.
func exploreTaskQualifiedSyntacticAnchorMatchesNode(task string, node *graph.Node) bool {
	if node == nil || node.Kind == graph.KindType || node.Kind == graph.KindInterface ||
		utf8.RuneCountInString(task) < shapeMinReportChars || !strings.ContainsAny(task, "\r\n") {
		return false
	}
	for _, anchor := range exploreSyntacticAnchors(task) {
		if anchor.qualifiedName != "" &&
			(exploreSyntacticAnchorMatchesIdentifier(anchor, node.ID) ||
				exploreSyntacticAnchorMatchesIdentifier(anchor, node.QualName)) {
			return true
		}
	}
	return false
}

func exploreSyntacticAnchorMatchesPath(anchor exploreSyntacticAnchor, node *graph.Node) bool {
	if node == nil || !strings.ContainsAny(anchor.source, ".\\/") {
		return false
	}
	want := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(anchor.source), "\\", "/"))
	got := strings.ToLower(strings.ReplaceAll(nodeDisplayPath(node), "\\", "/"))
	return want != "" && got != "" && (got == want || strings.HasSuffix(got, "/"+want))
}

func exploreSyntacticAnchorMatchesIdentifier(anchor exploreSyntacticAnchor, identifier string) bool {
	identifierTerms := rerank.Tokenize(identifier)
	if len(identifierTerms) == 0 {
		return false
	}
	compact := strings.ToLower(strings.Join(identifierTerms, ""))
	if strings.HasPrefix(compact, anchor.compact) ||
		(len(compact) >= 5 && strings.HasPrefix(anchor.compact, compact)) {
		return true
	}
	for _, anchorTerm := range anchor.terms {
		matched := false
		for _, identifierTerm := range identifierTerms {
			identifierTerm = strings.ToLower(identifierTerm)
			if strings.HasPrefix(identifierTerm, anchorTerm) ||
				(len(identifierTerm) >= 4 && strings.HasPrefix(anchorTerm, identifierTerm)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func exploreSyntacticAnchorEligibleNode(node *graph.Node) bool {
	if node == nil {
		return false
	}
	if isTest, _ := node.Meta["is_test"].(bool); isTest {
		return false
	}
	switch node.Kind {
	case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindMacro:
		return true
	default:
		return false
	}
}

func exploreSyntacticAnchorNodeStrength(anchor exploreSyntacticAnchor, node *graph.Node) int {
	if !exploreSyntacticAnchorEligibleNode(node) || !exploreSyntacticAnchorMatchesNode(anchor, node) {
		return 0
	}
	identifierTerms := rerank.Tokenize(node.Name)
	strength := 0
	if exploreSyntacticAnchorMatchesPath(anchor, node) {
		strength = 4
	}
	for _, anchorTerm := range anchor.terms {
		best := 0
		for _, rawIdentifierTerm := range identifierTerms {
			identifierTerm := strings.ToLower(rawIdentifierTerm)
			switch {
			case identifierTerm == anchorTerm:
				best = max(best, 3)
			case strings.HasPrefix(identifierTerm, anchorTerm):
				suffix := strings.TrimPrefix(identifierTerm, anchorTerm)
				if suffix == "r" || suffix == "er" || suffix == "or" || suffix == "impl" {
					best = max(best, 2)
				} else {
					best = max(best, 1)
				}
			case len(identifierTerm) >= 4 && strings.HasPrefix(anchorTerm, identifierTerm):
				best = max(best, 1)
			}
		}
		strength += best
	}
	switch node.Kind {
	case graph.KindFunction, graph.KindMethod:
		strength += 3
	case graph.KindMacro:
		strength += 2
	case graph.KindType:
		strength++
	}
	if len(identifierTerms) > 1 {
		strength++
	}
	return strength
}

func exploreSyntacticAnchorCandidate(
	anchor exploreSyntacticAnchor,
	candidates []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
) *rerank.Candidate {
	return exploreSyntacticAnchorCandidateWithTie(
		anchor, candidates, scope, usedIDs, usedFiles, false,
	)
}

func exploreSyntacticAnchorCompetingCandidate(
	anchor exploreSyntacticAnchor,
	candidates []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
) *rerank.Candidate {
	return exploreSyntacticAnchorCandidateWithTie(
		anchor, candidates, scope, usedIDs, usedFiles, true,
	)
}

func exploreSyntacticAnchorCandidateWithTie(
	anchor exploreSyntacticAnchor,
	candidates []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
	preferImplementation bool,
) *rerank.Candidate {
	var best *rerank.Candidate
	bestStrength := 0
	bestDiverse := false
	bestSpan := 0
	bestNameWidth := 0
	for _, candidate := range candidates {
		if candidate == nil || !exploreSyntacticAnchorEligibleNode(candidate.Node) ||
			!scope.ScopeAllows(candidate.Node) || !exploreSyntacticAnchorMatchesNode(anchor, candidate.Node) {
			continue
		}
		if _, used := usedIDs[candidate.Node.ID]; used {
			continue
		}
		strength := exploreSyntacticAnchorNodeStrength(anchor, candidate.Node)
		_, repeatedFile := usedFiles[candidate.Node.FilePath]
		diverse := !repeatedFile
		span := max(0, candidate.Node.EndLine-candidate.Node.StartLine)
		nameWidth := len(strings.TrimSpace(candidate.Node.Name))
		if best == nil || strength > bestStrength ||
			(strength == bestStrength && diverse && !bestDiverse) ||
			(preferImplementation && strength == bestStrength && diverse == bestDiverse && nameWidth < bestNameWidth) ||
			(preferImplementation && strength == bestStrength && diverse == bestDiverse && nameWidth == bestNameWidth && span > bestSpan) {
			best = candidate
			bestStrength = strength
			bestDiverse = diverse
			bestSpan = span
			bestNameWidth = nameWidth
		}
	}
	return best
}

// A bare one-term flag can match several equally strong compound callables.
// Fetch the existing bounded lexical lane only for that ambiguity class; the
// ordinary selector remains unchanged for every other anchor.
func exploreSyntacticAnchorNeedsLexicalCompetition(anchor exploreSyntacticAnchor, candidate *rerank.Candidate) bool {
	if candidate == nil || candidate.Node == nil || len(anchor.terms) != 1 ||
		exploreSyntacticAnchorMatchesPath(anchor, candidate.Node) {
		return false
	}
	switch candidate.Node.Kind {
	case graph.KindFunction, graph.KindMethod:
	default:
		return false
	}
	identifierTerms := rerank.Tokenize(candidate.Node.Name)
	if len(identifierTerms) < 2 {
		return false
	}
	for _, term := range identifierTerms {
		if strings.EqualFold(term, anchor.terms[0]) {
			return true
		}
	}
	return false
}

// Ambiguous one-term flags such as --replace often have many prefix-sharing
// helpers. Widen only that bounded lexical lane so the concrete implementation
// (for example replace_all) can compete with generic helpers such as
// replace_separator; ordinary identifiers retain the smaller fetch.
func exploreSyntacticAnchorFetchLimit(anchor exploreSyntacticAnchor, candidate *rerank.Candidate) int {
	if exploreSyntacticAnchorNeedsLexicalCompetition(anchor, candidate) {
		return exploreSyntacticAnchorCompetitionFetch
	}
	return exploreSyntacticAnchorFetch
}

func exploreSyntacticAnchorReusesProtected(
	anchor exploreSyntacticAnchor,
	candidates []*rerank.Candidate,
	usedIDs map[string]struct{},
) string {
	qualifiedMember := anchor.qualifiedName != ""
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		if qualifiedMember && (candidate.Node.Kind == graph.KindType || candidate.Node.Kind == graph.KindInterface) {
			continue
		}
		if _, used := usedIDs[candidate.Node.ID]; used && exploreSyntacticAnchorMatchesNode(anchor, candidate.Node) {
			return candidate.Node.ID
		}
	}
	return ""
}

// exploreExactQualifiedAnchorCandidate resolves the parser's exact member name
// before the bounded lexical lane. Graph nodes commonly store only the terminal
// method Name while the ID tail carries Owner.member, so query both exact forms.
// If those exact point lookups cannot select a qualified node, one final lane
// may inspect up to four already-ranked files that contain an exact owner
// declaration. It never performs a substring or corpus-wide scan.
func (s *Server) exploreExactQualifiedAnchorCandidate(
	ctx context.Context,
	anchor exploreSyntacticAnchor,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
) *rerank.Candidate {
	if s == nil || anchor.qualifiedName == "" || ctx.Err() != nil {
		return nil
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return nil
	}
	names := []string{anchor.qualifiedName}
	if dot := strings.LastIndexByte(anchor.qualifiedName, '.'); dot >= 0 && dot < len(anchor.qualifiedName)-1 {
		names = []string{anchor.qualifiedName[dot+1:], anchor.qualifiedName}
	}
	candidates := make([]*rerank.Candidate, 0, exploreSyntacticAnchorFetch)
	seen := make(map[string]struct{}, exploreSyntacticAnchorFetch)
	for _, name := range names {
		remaining := exploreSyntacticAnchorFetch - len(candidates)
		if remaining <= 0 {
			break
		}
		page, ok := boundedLocalizationExactName(
			ctx,
			reader,
			name,
			s.localizationNodeScope(ctx, scope, graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface, graph.KindMacro),
			remaining,
		)
		if !ok {
			return nil
		}
		for _, node := range page.Nodes {
			if node == nil || !s.nodeInSessionScope(ctx, node) {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			candidates = append(candidates, &rerank.Candidate{Node: node, VectorRank: -1})
			if len(candidates) == exploreSyntacticAnchorFetch {
				break
			}
		}
		if len(candidates) == exploreSyntacticAnchorFetch {
			break
		}
	}
	if candidate := exploreSyntacticAnchorCandidate(anchor, candidates, scope, usedIDs, usedFiles); candidate != nil {
		return candidate
	}
	return s.exploreQualifiedLeafCandidate(ctx, reader, anchor, ordinary, scope, usedIDs, usedFiles)
}

// gatherExploreSyntacticAnchorCandidates performs a tiny lexical retrieval for
// each anchor not represented by the ordinary over-fetch pool. Only after that
// identifier lane misses do we invoke the existing request-local source scan.
// One diverse node per anchor is retained; no source body is persisted.
func markExploreSyntacticAnchorCandidate(candidate *rerank.Candidate) {
	if candidate == nil {
		return
	}
	// Candidate clones elsewhere in the reranking pipeline may share the same
	// signal map. Copy before tagging so provenance cannot leak to an unselected
	// sibling through that shallow clone.
	signals := make(map[string]float64, len(candidate.Signals)+2)
	for name, value := range candidate.Signals {
		signals[name] = value
	}
	// The concept marker preserves the row in existing bounded selectors; the
	// dedicated marker survives into rendered provenance so PRIMARY confidence
	// can distinguish an explicit task-spelled anchor from a semantic neighbour.
	signals[exploreConceptComplementSignal] = 1
	signals[exploreSyntacticAnchorSignal] = 1
	candidate.Signals = signals
}

// exploreAnchorPoolCandidate prefers the already-ranked object for a node the
// ordinary pool holds, so protecting an anchor never discards its channel
// ranks, and reports whether the pool owned it.
func exploreAnchorPoolCandidate(
	ordinary []*rerank.Candidate,
	candidate *rerank.Candidate,
) (*rerank.Candidate, bool) {
	if candidate == nil || candidate.Node == nil {
		return candidate, false
	}
	for _, existing := range ordinary {
		if existing != nil && existing.Node != nil && existing.Node.ID == candidate.Node.ID {
			return existing, true
		}
	}
	return candidate, false
}

func (s *Server) gatherExploreSyntacticAnchorCandidatesCollecting(
	ctx context.Context,
	task string,
	ordinary []*rerank.Candidate,
	eng *query.Engine,
	scope query.QueryOptions,
	rctx *rerank.Context,
	collector *localizationSourceWindowHitCollector,
) ([]*rerank.Candidate, map[int]string) {
	if s == nil || s.graph == nil || eng == nil || ctx.Err() != nil {
		return nil, nil
	}
	anchors := s.exploreTaskAnchors(ctx, task, ordinary, scope)
	if len(anchors) == 0 {
		return nil, nil
	}

	protected := make(map[int]string, len(anchors))
	usedIDs := make(map[string]struct{}, len(anchors))
	usedFiles := make(map[string]struct{}, len(anchors))
	addProtected := func(index int, candidate *rerank.Candidate) {
		protected[index] = candidate.Node.ID
		usedIDs[candidate.Node.ID] = struct{}{}
		if candidate.Node.FilePath != "" {
			usedFiles[candidate.Node.FilePath] = struct{}{}
		}
		markExploreSyntacticAnchorCandidate(candidate)
	}

	additions := make([]*rerank.Candidate, 0, len(anchors))
	missed := make([]int, 0, len(anchors))
	// Owners admitted alongside a resolved qualified member are recorded past
	// the anchor list: they belong to no task token of their own.
	ownerIndex := len(anchors)
	anchorOpts := scope
	anchorOpts.SkipInnerRerank = true
	anchorOpts.SkipVectorChannel = true
	for index, anchor := range anchors {
		if ctx.Err() != nil {
			break
		}
		reused := exploreSyntacticAnchorReusesProtected(anchor, ordinary, usedIDs)
		if reused == "" {
			reused = exploreSyntacticAnchorReusesProtected(anchor, additions, usedIDs)
		}
		if reused != "" {
			protected[index] = reused
			continue
		}
		if exactCandidate := s.exploreExactAnchorCandidate(ctx, anchor, ordinary, scope, usedIDs, usedFiles); exactCandidate != nil {
			protectedCandidate, pooled := exploreAnchorPoolCandidate(ordinary, exactCandidate)
			if !pooled {
				additions = append(additions, protectedCandidate)
			}
			addProtected(index, protectedCandidate)
			// A resolved member without its declaring type reads as a free
			// function. Admit the owner only into a slot no task token claims,
			// so the anchor budget stays capped regardless of resolution order.
			if ownerIndex < exploreSyntacticAnchorMaxTerms {
				owner := s.exploreQualifiedAnchorOwnerCandidate(
					ctx, anchor, protectedCandidate.Node, scope, usedIDs,
				)
				if owner != nil {
					ownerCandidate, ownerPooled := exploreAnchorPoolCandidate(ordinary, owner)
					if !ownerPooled {
						additions = append(additions, ownerCandidate)
					}
					addProtected(ownerIndex, ownerCandidate)
					ownerIndex++
				}
			}
			continue
		}
		ordinaryCandidate := exploreSyntacticAnchorCandidate(anchor, ordinary, scope, usedIDs, usedFiles)
		compete := exploreSyntacticAnchorNeedsLexicalCompetition(anchor, ordinaryCandidate)
		if ordinaryCandidate != nil && exploreSyntacticAnchorNodeStrength(anchor, ordinaryCandidate.Node) >= 4 && !compete {
			addProtected(index, ordinaryCandidate)
			continue
		}
		rows := eng.GatherSymbolCandidates(anchor.query, exploreSyntacticAnchorFetchLimit(anchor, ordinaryCandidate), anchorOpts, rctx)
		combined := rows
		if ordinaryCandidate != nil {
			combined = append([]*rerank.Candidate{ordinaryCandidate}, rows...)
		}
		candidate := exploreSyntacticAnchorCandidate(anchor, combined, scope, usedIDs, usedFiles)
		if compete {
			candidate = exploreSyntacticAnchorCompetingCandidate(anchor, combined, scope, usedIDs, usedFiles)
		}
		if candidate == nil {
			missed = append(missed, index)
			continue
		}
		if _, pooled := exploreAnchorPoolCandidate(ordinary, candidate); !pooled {
			additions = append(additions, candidate)
		}
		addProtected(index, candidate)
	}
	if len(missed) == 0 || ctx.Err() != nil {
		return additions, protected
	}

	repoPrefix := ""
	if len(scope.RepoAllow) == 1 {
		for prefix, allowed := range scope.RepoAllow {
			if allowed {
				repoPrefix = prefix
			}
		}
	}
	if repoPrefix == "" {
		repoPrefix, _ = s.sessionLocality(ctx)
	}
	sourceTerms := make([]string, 0, len(missed))
	for _, index := range missed {
		anchor := anchors[index]
		sourceTerms = append(sourceTerms, anchor.source)
	}
	recall := s.gatherExploreSourceLiteralRecall(ctx, sourceTerms, repoPrefix, scope)
	if len(recall.hits) == 0 {
		return additions, protected
	}
	ids := make([]string, 0, len(recall.hits))
	for _, hit := range recall.hits {
		ids = append(ids, hit.nodeID)
	}
	nodes := s.readerFor(ctx).GetNodesByIDs(ids)
	for localIndex, anchorIndex := range missed {
		var fallback *graph.Node
		var selected *graph.Node
		selectedRank := 0
		for _, hit := range recall.hits {
			if hit.anchor != localIndex {
				continue
			}
			node := nodes[hit.nodeID]
			if !exploreSyntacticAnchorEligibleNode(node) || !scope.ScopeAllows(node) {
				continue
			}
			if _, used := usedIDs[node.ID]; used {
				continue
			}
			if fallback == nil {
				fallback = node
				selectedRank = hit.rank
			}
			if _, repeatedFile := usedFiles[node.FilePath]; !repeatedFile {
				selected = node
				selectedRank = hit.rank
				break
			}
		}
		if selected == nil {
			selected = fallback
		}
		if selected == nil {
			continue
		}
		for _, hit := range recall.hits {
			if hit.anchor == localIndex && hit.nodeID == selected.ID && hit.rank == selectedRank {
				collector.add(hit)
				break
			}
		}
		sourceRank := 1.0
		if selectedRank > 0 {
			sourceRank = 1 / float64(selectedRank+1)
		}
		candidate := &rerank.Candidate{
			Node: selected, TextRank: selectedRank, VectorRank: -1,
			Signals: map[string]float64{
				exploreSourceLiteralSignal:         sourceRank,
				exploreSourceLiteralCoverageSignal: 1,
			},
		}
		additions = append(additions, candidate)
		addProtected(anchorIndex, candidate)
	}
	return additions, protected
}

// exploreReservedAnchorIndexes orders task-shaped anchors first and
// graph-resolved reservations after them. Anchors admitted by exact graph name,
// and owners admitted alongside a resolved qualified member, are recorded past
// the task-shaped list, so they can only be reserved by identifier — this pure
// step has no graph with which to re-derive them.
func exploreReservedAnchorIndexes(anchors []exploreSyntacticAnchor, protected map[int]string) []int {
	indexes := make([]int, 0, len(anchors)+len(protected))
	for index := range anchors {
		indexes = append(indexes, index)
	}
	resolved := make([]int, 0, len(protected))
	for index, id := range protected {
		if id != "" && index >= len(anchors) {
			resolved = append(resolved, index)
		}
	}
	sort.Ints(resolved)
	return append(indexes, resolved...)
}

// reserveExploreSyntacticAnchorCandidates keeps every discovered anchor owner
// inside the final window while preserving the semantic head and the relative
// order of all candidates that do not need promotion.
func reserveExploreSyntacticAnchorCandidates(
	task string,
	candidates []*rerank.Candidate,
	protected map[int]string,
	maxSymbols int,
) []*rerank.Candidate {
	anchors := exploreSyntacticAnchors(task)
	if (len(anchors) == 0 && len(protected) == 0) || len(candidates) == 0 || maxSymbols <= 0 {
		return candidates
	}
	if maxSymbols > len(candidates) {
		maxSymbols = len(candidates)
	}

	usedIDs := make(map[string]struct{}, len(anchors))
	usedFiles := make(map[string]struct{}, len(anchors))
	reservationIDs := make([]string, 0, len(anchors)+len(protected))
	for _, index := range exploreReservedAnchorIndexes(anchors, protected) {
		id := protected[index]
		if id == "" && index < len(anchors) {
			if candidate := exploreSyntacticAnchorCandidate(anchors[index], candidates, query.QueryOptions{}, usedIDs, usedFiles); candidate != nil {
				id = candidate.Node.ID
			}
		}
		if id == "" {
			continue
		}
		for _, candidate := range candidates {
			if candidate == nil || candidate.Node == nil || candidate.Node.ID != id {
				continue
			}
			usedIDs[id] = struct{}{}
			usedFiles[candidate.Node.FilePath] = struct{}{}
			reservationIDs = append(reservationIDs, id)
			break
		}
	}
	if len(reservationIDs) == 0 {
		return candidates
	}

	// Rebuild only the order, never the candidate objects: semantic head first,
	// then one owner per anchor, then every unreserved row in original order.
	// This is simpler and safer than repeated in-place moves, where promoting a
	// later anchor can shift an earlier reservation back outside the window.
	result := make([]*rerank.Candidate, 0, len(candidates))
	admitted := make(map[string]struct{}, len(reservationIDs)+1)
	appendCandidate := func(candidate *rerank.Candidate) {
		if candidate == nil || candidate.Node == nil {
			return
		}
		if _, duplicate := admitted[candidate.Node.ID]; duplicate {
			return
		}
		admitted[candidate.Node.ID] = struct{}{}
		result = append(result, candidate)
	}
	appendCandidate(candidates[0])
	for _, id := range reservationIDs {
		if len(result) >= maxSymbols {
			break
		}
		for _, candidate := range candidates {
			if candidate != nil && candidate.Node != nil && candidate.Node.ID == id {
				appendCandidate(candidate)
				break
			}
		}
	}
	for _, candidate := range candidates {
		appendCandidate(candidate)
	}
	return result
}

func exploreSyntacticAnchorMatchesTargetSource(anchor exploreSyntacticAnchor, target exploreTarget, lowerSource string) bool {
	if exploreSyntacticAnchorMatchesNode(anchor, target.node) {
		return true
	}
	for _, term := range anchor.terms {
		if !strings.Contains(lowerSource, term) {
			return false
		}
	}
	return true
}

// exploreSyntacticAnchorEvidenceReady prevents a broad semantic neighbor from
// terminating localization while a distinctive implementation clue is absent.
// Every retained anchor the requester spelled needs one production declaration
// with hydrated source; fields, tests, abstract declarations, and metadata-only
// hits cannot prove it.
func exploreSyntacticAnchorEvidenceReady(task string, targets []exploreTarget) bool {
	anchors := exploreSyntacticAnchors(task)
	covered := make([]bool, len(anchors))
	remaining := 0
	for index, anchor := range anchors {
		if anchor.mined {
			covered[index] = true
			continue
		}
		remaining++
	}
	if remaining == 0 {
		return true
	}
	for _, target := range targets {
		if !exploreSyntacticAnchorEligibleNode(target.node) || strings.TrimSpace(target.source) == "" {
			continue
		}
		declaration := strings.ToLower(target.source)
		for index, anchor := range anchors {
			if covered[index] || !exploreSyntacticAnchorMatchesTargetSource(anchor, target, declaration) {
				continue
			}
			if target.node.Kind == graph.KindType &&
				(strings.Contains(declaration, "interface ") || strings.Contains(declaration, "trait ") || strings.Contains(declaration, "protocol ")) {
				continue
			}
			covered[index] = true
			remaining--
		}
		if remaining == 0 {
			return true
		}
	}
	return remaining == 0
}

// exploreAuthoredSyntacticAnchorCount counts the anchors the requester spelled.
// Gates that withhold an answer read this count, never the mined total.
func exploreAuthoredSyntacticAnchorCount(anchors []exploreSyntacticAnchor) int {
	authored := 0
	for _, anchor := range anchors {
		if !anchor.mined {
			authored++
		}
	}
	return authored
}

func exploreSyntacticAnchorTargetMatchesAnchors(anchors []exploreSyntacticAnchor, target exploreTarget) int {
	if !exploreSyntacticAnchorEligibleNode(target.node) || strings.TrimSpace(target.source) == "" {
		return 0
	}
	lowerSource := strings.ToLower(target.source)
	matched := 0
	for _, anchor := range anchors {
		if exploreSyntacticAnchorMatchesTargetSource(anchor, target, lowerSource) {
			matched++
		}
	}
	return matched
}
