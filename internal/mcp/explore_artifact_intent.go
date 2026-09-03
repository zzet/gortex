package mcp

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

const (
	exploreArtifactPathLimit    = 12
	exploreArtifactProbeLimit   = 2
	exploreArtifactTextHitLimit = 16
	exploreArtifactResultLimit  = 5
	exploreArtifactSnippetLimit = 8 << 10
)

type exploreArtifactIntent struct {
	active        bool
	explicitCount int
	semantic      bool
	paths         []string
	probes        []string
}

// exploreTaskProbe is one distinctive span a task spells out, ranked by how
// unlikely it is to appear by accident. The artifact lane greps for it; the
// source lane matches it against candidate paths.
type exploreTaskProbe struct {
	value    string
	priority int
	order    int
}

type exploreArtifactHit struct {
	file        *graph.Node
	path        string
	snippet     string
	declaration string
	pathHit     bool
	contentHit  bool
	fullPath    bool
	exactBase   string
	uniqueBase  bool
	score       int
}

type exploreArtifactLane struct {
	targets []exploreTarget
	ready   bool
}

var (
	exploreArtifactPathRE       = regexp.MustCompile(`[A-Za-z0-9_@+.-]*(?:[\\/][A-Za-z0-9_@+.-]+)+|[A-Za-z0-9_@+-]+(?:\.[A-Za-z0-9_@+-]+)+`)
	exploreArtifactProbeRE      = regexp.MustCompile("`[^`\\n]{2,96}`|\\\"[^\\\"\\n]{2,96}\\\"|'[^'\\n]{2,96}'|(?:^|\\s)--?[A-Za-z][A-Za-z0-9_.-]{1,63}|\\b[A-Z][A-Z0-9_]{2,63}\\b")
	exploreArtifactPropertyRE   = regexp.MustCompile(`(?i)(?:^|[\s,;])/(?:p|property):([A-Za-z_][A-Za-z0-9_.-]{1,63})`)
	exploreArtifactAssignmentRE = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]{1,63})\s*=`)
	exploreArtifactCallRE       = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*(?:(?:::|\.)[A-Za-z_][A-Za-z0-9_]*)?\s*\(`)
)

func classifyExploreArtifactIntent(task string) exploreArtifactIntent {
	var out exploreArtifactIntent
	seen := make(map[string]struct{})
	addPath := func(raw string) {
		raw = strings.Trim(raw, "`'\"()[]{}<>,;:")
		key := strings.ToLower(strings.ReplaceAll(raw, "\\", "/"))
		if !exploreArtifactFile(raw) || len(out.paths) == exploreArtifactPathLimit {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out.paths = append(out.paths, raw)
	}
	addSemanticPath := func(word string) {
		word = strings.ToLower(strings.TrimSpace(word))
		if word == "" || len(out.paths) == exploreArtifactPathLimit {
			return
		}
		if _, ok := seen[word]; ok {
			return
		}
		seen[word] = struct{}{}
		out.paths = append(out.paths, word)
	}
	for _, raw := range exploreArtifactPathRE.FindAllString(task, -1) {
		addPath(raw)
	}
	for i, field := range strings.Fields(task) {
		if i == exploreArtifactPathLimit*4 {
			break
		}
		addPath(field) // extensionless Dockerfile/Makefile/etc.
	}
	out.explicitCount = len(out.paths)

	artifactScore, sourceScore := 0, 0
	semanticPaths := make([]string, 0, 8)
	seenSemantic := make(map[string]struct{})
	for _, rawWord := range exploreArtifactWords(task) {
		word := canonicalExploreArtifactWord(rawWord)
		if exploreArtifactWord(word) {
			artifactScore++
			if _, ok := seenSemantic[word]; !ok {
				seenSemantic[word] = struct{}{}
				semanticPaths = append(semanticPaths, word)
			}
		}
		if exploreSourceWord(word) {
			sourceScore++
		}
	}
	for _, raw := range exploreArtifactPathRE.FindAllString(task, -1) {
		if exploreSourceExtension(filepath.Ext(raw)) {
			sourceScore += 2
		}
	}
	if exploreArtifactCallRE.MatchString(task) {
		sourceScore += 2
	}
	out.semantic = artifactScore >= 2
	for _, word := range semanticPaths {
		if exploreArtifactPathWord(word) {
			addSemanticPath(word)
		}
	}
	out.probes = rankedExploreArtifactProbes(task, seen)
	// "build configuration" alone is too broad to scan every artifact file.
	// Build becomes a secondary filename family only after an explicit artifact,
	// another semantic path family, or a distinctive content probe activates a
	// genuinely searchable lane. Environment/property probes such as TF_BUILD
	// also carry that family even though their separators are intentionally kept
	// intact by exploreArtifactWords.
	_, buildMentioned := seenSemantic["build"]
	for _, probe := range out.probes {
		for _, word := range strings.FieldsFunc(probe, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			if strings.EqualFold(word, "build") {
				buildMentioned = true
				break
			}
		}
	}
	if buildMentioned && (out.explicitCount > 0 || len(out.paths) > 0 || len(out.probes) > 0) {
		addSemanticPath("build")
	}
	// A model may faithfully ask for both "source/configuration files" and
	// "precise symbols" even when the supplied anchors are overwhelmingly
	// artifact-shaped. Do not let those incidental source nouns veto a lane
	// corroborated by at least three artifact terms and two distinctive probes.
	// The thresholds keep ordinary config-parser and settings-class tasks on the
	// source path.
	strongSemanticEvidence := out.semantic && artifactScore >= 3 && len(out.probes) >= 2
	artifactEligible := out.explicitCount > 0 || (out.semantic && (sourceScore == 0 || strongSemanticEvidence))
	out.active = artifactEligible && (len(out.paths) > 0 || len(out.probes) > 0)
	return out
}

func canonicalExploreArtifactWord(word string) string {
	switch strings.ToLower(word) {
	case "artifacts":
		return "artifact"
	case "deployments":
		return "deployment"
	case "pipelines":
		return "pipeline"
	case "releases":
		return "release"
	case "settings":
		return "setting"
	case "workflows":
		return "workflow"
	default:
		return strings.ToLower(word)
	}
}

// rankedExploreArtifactProbes is the artifact lane's view of the shared probe
// ranking: the values only, bounded by what a content grep can afford.
func rankedExploreArtifactProbes(task string, seen map[string]struct{}) []string {
	ranked := rankedExploreTaskProbes(task, seen, exploreArtifactProbeLimit)
	out := make([]string, 0, len(ranked))
	for _, probe := range ranked {
		out = append(out, probe.value)
	}
	return out
}

// rankedExploreTaskProbes ranks the task's distinctive spans — property
// arguments, assignments, quoted spans, flags and environment names — highest
// priority first, then earliest mention. Terms already consumed as paths are
// skipped, and every emitted probe is marked in seen so no later lane re-derives
// it. The probes are pure task text: no graph, index or file access.
func rankedExploreTaskProbes(task string, seen map[string]struct{}, limit int) []exploreTaskProbe {
	candidates := make(map[string]exploreTaskProbe)
	add := func(value string, priority, order int) {
		value = strings.TrimSpace(strings.Trim(value, "`'\""))
		if len(value) < 2 || exploreArtifactFile(value) || strings.EqualFold(value, "CI") {
			return
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return
		}
		candidate := exploreTaskProbe{value: value, priority: priority, order: order}
		if current, exists := candidates[key]; exists &&
			(current.priority > candidate.priority || (current.priority == candidate.priority && current.order <= candidate.order)) {
			return
		}
		candidates[key] = candidate
	}
	for _, match := range exploreArtifactPropertyRE.FindAllStringSubmatchIndex(task, -1) {
		if len(match) >= 4 && match[2] >= 0 {
			add(task[match[2]:match[3]], 500, match[2])
		}
	}
	for _, match := range exploreArtifactAssignmentRE.FindAllStringSubmatchIndex(task, -1) {
		if len(match) < 4 || match[2] < 0 {
			continue
		}
		value := task[match[2]:match[3]]
		priority := 220
		if exploreArtifactEnvironmentProbe(value) {
			priority = 450
		} else if exploreArtifactCamelProbe(value) {
			priority = 400
		}
		add(value, priority, match[2])
	}
	for _, match := range exploreArtifactProbeRE.FindAllStringIndex(task, -1) {
		raw := task[match[0]:match[1]]
		trimmed := strings.TrimSpace(raw)
		priority := 100
		switch {
		case strings.HasPrefix(trimmed, "`") || strings.HasPrefix(trimmed, "\"") || strings.HasPrefix(trimmed, "'"):
			priority = 350
		case strings.HasPrefix(trimmed, "-"):
			priority = 300
		case exploreArtifactEnvironmentProbe(trimmed):
			priority = 425
		}
		add(trimmed, priority, match[0])
	}
	ordered := make([]exploreTaskProbe, 0, len(candidates))
	for _, candidate := range candidates {
		ordered = append(ordered, candidate)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].priority != ordered[j].priority {
			return ordered[i].priority > ordered[j].priority
		}
		if ordered[i].order != ordered[j].order {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].value < ordered[j].value
	})
	out := make([]exploreTaskProbe, 0, limit)
	for _, candidate := range ordered {
		if len(out) == limit {
			break
		}
		seen[strings.ToLower(candidate.value)] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func exploreArtifactEnvironmentProbe(value string) bool {
	if !strings.Contains(value, "_") {
		return false
	}
	for _, r := range value {
		if unicode.IsLower(r) || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_') {
			return false
		}
	}
	return true
}

func exploreArtifactCamelProbe(value string) bool {
	hasLower, hasUpper := false, false
	for _, r := range value {
		hasLower = hasLower || unicode.IsLower(r)
		hasUpper = hasUpper || unicode.IsUpper(r)
	}
	return hasLower && hasUpper
}

const (
	exploreArtifactNames      = "|dockerfile|makefile|justfile|gemfile|brewfile|cargo.toml|cargo.lock|go.mod|go.sum|package.json|package-lock.json|pnpm-lock.yaml|yarn.lock|pom.xml|directory.build.props|directory.build.targets|tsconfig.json|"
	exploreArtifactExtensions = "|.cfg|.conf|.config|.csproj|.editorconfig|.env|.fsproj|.gradle|.hcl|.ini|.json|.lock|.manifest|.props|.properties|.sln|.targets|.tf|.toml|.vbproj|.xml|.yaml|.yml|"
	exploreArtifactWordsSet   = "|artifact|artifacts|build|ci|configuration|config|coverage|deploy|deployment|environment|infra|infrastructure|manifest|package|pipeline|release|setting|settings|workflow|workflows|"
	exploreArtifactPathsSet   = "|ci|coverage|deployment|manifest|pipeline|release|workflow|workflows|"
	exploreSourceWordsSet     = "|callee|caller|class|constructor|function|handler|implementation|interface|method|parser|resolver|struct|symbol|trait|type|"
	exploreSourceExtsSet      = "|.c|.cc|.cpp|.cs|.dart|.ex|.exs|.go|.h|.hpp|.java|.js|.jsx|.kt|.lua|.php|.py|.rb|.rs|.scala|.swift|.ts|.tsx|"
)

func exploreInSet(set, value string) bool {
	return strings.Contains(set, "|"+strings.ToLower(value)+"|")
}

func exploreArtifactFile(value string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")))
	return exploreInSet(exploreArtifactNames, base) || base == ".env" || strings.HasPrefix(base, ".env.") || exploreInSet(exploreArtifactExtensions, filepath.Ext(base))
}

func exploreArtifactWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
}

func exploreArtifactWord(word string) bool     { return exploreInSet(exploreArtifactWordsSet, word) }
func exploreArtifactPathWord(word string) bool { return exploreInSet(exploreArtifactPathsSet, word) }
func exploreSourceWord(word string) bool       { return exploreInSet(exploreSourceWordsSet, word) }
func exploreSourceExtension(ext string) bool   { return exploreInSet(exploreSourceExtsSet, ext) }

const exploreArtifactToolMetadataRoots = "|.claude|.codegraph|.codex|.flow|.gitnexus|.graphify|.serena|graphify-out|"

func exploreArtifactPathEligible(intent exploreArtifactIntent, path string) bool {
	normalized := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(path), "\\", "/"), "./")
	root, _, _ := strings.Cut(normalized, "/")
	if !exploreInSet(exploreArtifactToolMetadataRoots, root) {
		return true
	}
	// Tool/session metadata is not implementation evidence merely because it
	// repeats issue vocabulary. It remains searchable when the task names that
	// path or basename explicitly, which preserves real configuration work.
	for index, explicit := range intent.paths {
		if index >= intent.explicitCount {
			break
		}
		explicit = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(explicit), "\\", "/"), "./")
		if strings.EqualFold(explicit, normalized) ||
			(!strings.Contains(explicit, "/") && strings.EqualFold(filepath.Base(explicit), filepath.Base(normalized))) {
			return true
		}
	}
	return false
}

// gatherExploreArtifactLane reuses search(files)' graph file nodes and
// search(text)'s trigram backend. The inactive path returns before either I/O.
func (s *Server) gatherExploreArtifactLane(ctx context.Context, intent exploreArtifactIntent, scope query.QueryOptions) exploreArtifactLane {
	if !intent.active || (len(intent.paths) == 0 && len(intent.probes) == 0) || s == nil || ctx.Err() != nil {
		return exploreArtifactLane{}
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return exploreArtifactLane{}
	}
	files := make([]*exploreArtifactHit, 0, 64)
	byPath := make(map[string]*exploreArtifactHit)
	for node := range reader.NodesByKind(graph.KindFile) {
		if node == nil || !s.nodeInSessionScope(ctx, node) || !scope.ScopeAllows(node) {
			continue
		}
		rel := repoRelativePath(node)
		if !exploreArtifactPathEligible(intent, rel) {
			continue
		}
		hit := &exploreArtifactHit{file: node, path: rel}
		files = append(files, hit)
		key := strings.ToLower(strings.ReplaceAll(rel, "\\", "/"))
		byPath[key] = hit
		if node.RepoPrefix != "" && !strings.HasPrefix(key, strings.ToLower(node.RepoPrefix)+"/") {
			byPath[strings.ToLower(node.RepoPrefix)+"/"+key] = hit
		}
	}
	files = rankExploreArtifactPathHits(files, intent)
	kept := make(map[*graph.Node]*exploreArtifactHit, len(files))
	for _, hit := range files {
		if hit.pathHit {
			kept[hit.file] = hit
		}
	}

	for _, probe := range intent.probes {
		if ctx.Err() != nil || (s.multiIndexer == nil && s.indexer == nil) {
			break
		}
		var matches []trigram.Match
		if s.multiIndexer != nil && scope.RepoAllow != nil {
			matches = s.multiIndexer.GrepTextForRepos(probe, scope.RepoAllow, exploreArtifactTextHitLimit)
		} else if s.multiIndexer != nil {
			matches = s.multiIndexer.GrepText(probe, exploreArtifactTextHitLimit)
		} else {
			matches = s.indexer.GrepText(probe, exploreArtifactTextHitLimit)
		}
		enriched, _ := s.enrichTextMatchesContext(ctx, matches, scope)
		for _, match := range enriched {
			hit := byPath[strings.ToLower(strings.ReplaceAll(match.Path, "\\", "/"))]
			if hit == nil || !exploreArtifactFile(hit.path) {
				continue
			}
			if !hit.contentHit {
				hit.score += 5
			}
			hit.contentHit = true
			hit.declaration = match.SymbolID
			if hit.snippet == "" {
				hit.snippet = truncateExploreArtifactSnippet(match.Text)
			}
			kept[hit.file] = hit
		}
	}
	results := make([]*exploreArtifactHit, 0, len(kept))
	for _, hit := range kept {
		results = append(results, hit)
	}
	results = selectExploreArtifactResults(results, exploreArtifactResultLimit)
	ids := make([]string, 0, len(results))
	for _, hit := range results {
		if hit.declaration != "" {
			ids = append(ids, hit.declaration)
		}
	}
	declarations := reader.GetNodesByIDs(ids)
	lane := exploreArtifactLane{targets: make([]exploreTarget, 0, len(results))}
	for _, hit := range results {
		node := hit.file
		if declaration := declarations[hit.declaration]; declaration != nil {
			node = declaration
		}
		lane.targets = append(lane.targets, exploreTarget{node: node, source: hit.snippet, score: float64(hit.score), exactContent: hit.contentHit})
	}
	runnerUp := 0
	if len(results) > 1 {
		runnerUp = results[1].score
	}
	if len(results) > 0 {
		lane.ready = exploreArtifactTerminal(intent, results[0], runnerUp)
	}
	return lane
}

func rankExploreArtifactPathHits(files []*exploreArtifactHit, intent exploreArtifactIntent) []*exploreArtifactHit {
	exactBasenames := make(map[string]int)
	for _, hit := range files {
		if hit == nil {
			continue
		}
		explicitScore, semanticScore, explicitBonus := 0, 0, 0
		for i, term := range intent.paths {
			score, ok := scoreFilenameMatch(term, filepath.Base(hit.path), hit.path, false)
			if !ok {
				continue
			}
			hit.pathHit = true
			if i >= intent.explicitCount {
				if score > semanticScore {
					semanticScore = score
				}
				continue
			}
			if score > explicitScore {
				explicitScore = score
			}
			normalizedTerm := strings.TrimPrefix(strings.ReplaceAll(term, "\\", "/"), "./")
			normalizedHit := strings.TrimPrefix(strings.ReplaceAll(hit.path, "\\", "/"), "./")
			switch {
			case strings.Contains(normalizedTerm, "/") && strings.EqualFold(normalizedTerm, normalizedHit):
				hit.fullPath = true
				explicitBonus = 20
			case strings.EqualFold(filepath.Base(term), filepath.Base(hit.path)):
				hit.exactBase = strings.ToLower(filepath.Base(term))
				exactBasenames[hit.exactBase]++
				explicitBonus = 20
			}
		}
		hit.score += explicitScore + semanticScore + explicitBonus
	}
	for _, hit := range files {
		if hit != nil {
			hit.uniqueBase = hit.exactBase != "" && exactBasenames[hit.exactBase] == 1
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return exploreArtifactHitLess(files[i], files[j]) })
	if len(files) > exploreArtifactPathLimit {
		files = files[:exploreArtifactPathLimit]
	}
	return files
}

func selectExploreArtifactResults(results []*exploreArtifactHit, limit int) []*exploreArtifactHit {
	sort.SliceStable(results, func(i, j int) bool { return exploreArtifactHitLess(results[i], results[j]) })
	if limit <= 0 || len(results) <= limit {
		return results
	}
	selected := make([]*exploreArtifactHit, 0, limit)
	deferred := make([]*exploreArtifactHit, 0)
	exactBaseCounts := make(map[string]int)
	for _, hit := range results {
		if hit == nil {
			continue
		}
		if hit.exactBase != "" && !hit.fullPath && exactBaseCounts[hit.exactBase] >= 2 {
			deferred = append(deferred, hit)
			continue
		}
		selected = append(selected, hit)
		if hit.exactBase != "" {
			exactBaseCounts[hit.exactBase]++
		}
		if len(selected) == limit {
			return selected
		}
	}
	for _, hit := range deferred {
		selected = append(selected, hit)
		if len(selected) == limit {
			break
		}
	}
	return selected
}

func exploreArtifactHitLess(left, right *exploreArtifactHit) bool {
	if left == nil || right == nil {
		return right == nil && left != nil
	}
	if left.score != right.score {
		return left.score > right.score
	}
	leftDepth := strings.Count(strings.Trim(strings.ReplaceAll(left.path, "\\", "/"), "/"), "/")
	rightDepth := strings.Count(strings.Trim(strings.ReplaceAll(right.path, "\\", "/"), "/"), "/")
	if leftDepth != rightDepth {
		return leftDepth < rightDepth
	}
	return left.path < right.path
}

func truncateExploreArtifactSnippet(snippet string) string {
	if len(snippet) <= exploreArtifactSnippetLimit {
		return snippet
	}
	return strings.ToValidUTF8(snippet[:exploreArtifactSnippetLimit], "")
}

func exploreArtifactTerminal(intent exploreArtifactIntent, best *exploreArtifactHit, runnerUp int) bool {
	if !intent.active || best == nil {
		return false
	}
	if best.fullPath || best.uniqueBase {
		return true
	}
	return (intent.semantic || intent.explicitCount > 0) && len(intent.probes) > 0 && best.pathHit && best.contentHit && best.score-runnerUp >= 5
}
