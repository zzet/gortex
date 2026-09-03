package mcp

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/review"
)

const (
	localizationSourceWindowRadius         = 100
	localizationSourceWindowMaxLines       = 2*localizationSourceWindowRadius + 1
	localizationSourceWindowMaxBytes       = 12 << 10
	localizationSourceWindowCandidateLimit = 64
)

// localizationSourceWindow is optional context around one exact source-literal
// match already discovered by localization. It is neither evidence nor proof:
// the ordinary evidence projection and digest remain authoritative.
type localizationSourceWindow struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	MatchLine       int    `json:"match_line"`
	Content         string `json:"content"`
	Bytes           int    `json:"bytes"`
	Lines           int    `json:"lines"`
	SecretsRedacted bool   `json:"secrets_redacted,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
	AnchorSymbol    string `json:"anchor_symbol"`
}

// localizationSourceWindowHitCollector is request-local. Callers pass nil on
// ordinary explore requests, preserving their existing behavior and memory.
type localizationSourceWindowHitCollector struct {
	hits []exploreSourceLiteralHit
}

func (collector *localizationSourceWindowHitCollector) add(hits ...exploreSourceLiteralHit) {
	if collector == nil {
		return
	}
	for _, hit := range hits {
		if len(collector.hits) >= localizationSourceWindowCandidateLimit {
			return
		}
		if strings.TrimSpace(hit.nodeID) == "" || strings.TrimSpace(hit.matchPath) == "" ||
			hit.matchLine < 1 || strings.TrimSpace(hit.literal) == "" {
			continue
		}
		duplicate := false
		for _, existing := range collector.hits {
			if existing.nodeID == hit.nodeID && existing.matchPath == hit.matchPath &&
				existing.matchLine == hit.matchLine && strings.EqualFold(existing.literal, hit.literal) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			collector.hits = append(collector.hits, hit)
		}
	}
}

// elect returns one exact hit whose source-literal candidate survived the final
// page target selection. The order is deterministic and favors settled,
// construction-aligned callsites before retrieval rank.
func (collector *localizationSourceWindowHitCollector) elect(targets []exploreTarget) (int, exploreSourceLiteralHit, bool) {
	if collector == nil || len(collector.hits) == 0 || len(targets) == 0 {
		return 0, exploreSourceLiteralHit{}, false
	}
	targetByID := make(map[string]int, len(targets))
	for index, target := range targets {
		if target.node == nil || target.node.ID == "" {
			continue
		}
		if _, exists := targetByID[target.node.ID]; !exists {
			targetByID[target.node.ID] = index
		}
	}
	bestIndex := -1
	var best exploreSourceLiteralHit
	for _, hit := range collector.hits {
		index, survives := targetByID[hit.nodeID]
		if !survives {
			continue
		}
		if !targets[index].sourceLiteral && !hit.callee {
			continue
		}
		if bestIndex < 0 || localizationSourceWindowHitBetter(index, targets[index], hit, bestIndex, targets[bestIndex], best) {
			bestIndex, best = index, hit
		}
	}
	return bestIndex, best, bestIndex >= 0
}

func localizationSourceWindowHitBetter(
	leftIndex int,
	leftTarget exploreTarget,
	left exploreSourceLiteralHit,
	rightIndex int,
	rightTarget exploreTarget,
	right exploreSourceLiteralHit,
) bool {
	if left.ambiguous != right.ambiguous {
		return !left.ambiguous
	}
	if leftTarget.sourceLiteralAligned != rightTarget.sourceLiteralAligned {
		return leftTarget.sourceLiteralAligned
	}
	if left.callee != right.callee {
		return left.callee
	}
	if leftIndex != rightIndex {
		return leftIndex < rightIndex
	}
	if left.anchor != right.anchor {
		return left.anchor < right.anchor
	}
	if left.rank != right.rank {
		return left.rank < right.rank
	}
	if left.matchPath != right.matchPath {
		return left.matchPath < right.matchPath
	}
	if left.matchLine != right.matchLine {
		return left.matchLine < right.matchLine
	}
	return strings.ToLower(left.literal) < strings.ToLower(right.literal)
}

// localizationSourceWindowForHit validates the original match coordinate
// against the live overlay-aware file view, then redacts and bounds it. It never
// searches for a moved match: stale coordinates simply produce no window.
func (s *Server) localizationSourceWindowForHit(
	ctx context.Context,
	hit exploreSourceLiteralHit,
	anchorSymbol string,
) *localizationSourceWindow {
	if s == nil || ctx == nil || ctx.Err() != nil || hit.matchLine < 1 ||
		strings.TrimSpace(hit.matchPath) == "" || strings.TrimSpace(hit.literal) == "" ||
		strings.TrimSpace(anchorSymbol) == "" {
		return nil
	}
	absPath, displayPath, err := s.resolveFilePath(ctx, hit.matchPath)
	if err != nil {
		return nil
	}
	content, startLine, _, err := s.readLinesForCtx(
		ctx, absPath, hit.matchLine, hit.matchLine, localizationSourceWindowRadius,
	)
	if err != nil || ctx.Err() != nil || content == "" || looksBinary([]byte(content)) || !utf8.ValidString(content) {
		return nil
	}
	lines := localizationSourceWindowLines(content)
	matchIndex := hit.matchLine - startLine
	if matchIndex < 0 || matchIndex >= len(lines) || !exploreTextHasExactLiteral(lines[matchIndex], hit.literal) {
		return nil
	}

	redacted, redactions := review.RedactSecrets(strings.Join(lines, "\n"))
	if !utf8.ValidString(redacted) {
		return nil
	}
	redactedLines := localizationSourceWindowLines(redacted)
	if len(redactedLines) != len(lines) || matchIndex >= len(redactedLines) ||
		!exploreTextHasExactLiteral(redactedLines[matchIndex], hit.literal) {
		return nil
	}
	if displayPath == "" {
		displayPath = hit.matchPath
	}
	window := &localizationSourceWindow{
		Path:            displayPath,
		StartLine:       startLine,
		EndLine:         startLine + len(redactedLines) - 1,
		MatchLine:       hit.matchLine,
		Content:         strings.Join(redactedLines, "\n"),
		SecretsRedacted: redactions > 0,
		AnchorSymbol:    anchorSymbol,
	}
	window.refreshSize()
	for window.Lines > localizationSourceWindowMaxLines || window.Bytes > localizationSourceWindowMaxBytes {
		if !window.shrinkOuterLine() {
			return nil
		}
	}
	return window
}

func localizationSourceWindowLines(content string) []string {
	lines := strings.Split(content, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func (window *localizationSourceWindow) clone() *localizationSourceWindow {
	if window == nil {
		return nil
	}
	copy := *window
	return &copy
}

func (window *localizationSourceWindow) refreshSize() {
	if window == nil {
		return
	}
	window.Bytes = len(window.Content)
	if window.Content == "" {
		window.Lines = 0
		return
	}
	window.Lines = strings.Count(window.Content, "\n") + 1
}

// shrinkOuterLine removes the farther outer line, preferring the trailing side
// on ties, while retaining the complete matching line.
func (window *localizationSourceWindow) shrinkOuterLine() bool {
	if window == nil || window.Content == "" {
		return false
	}
	lines := strings.Split(window.Content, "\n")
	matchIndex := window.MatchLine - window.StartLine
	if len(lines) <= 1 || matchIndex < 0 || matchIndex >= len(lines) {
		return false
	}
	leftDistance := matchIndex
	rightDistance := len(lines) - matchIndex - 1
	switch {
	case rightDistance >= leftDistance && rightDistance > 0:
		lines = lines[:len(lines)-1]
		window.EndLine--
	case leftDistance > 0:
		lines = lines[1:]
		window.StartLine++
	default:
		return false
	}
	window.Content = strings.Join(lines, "\n")
	window.Truncated = true
	window.refreshSize()
	return true
}

// localizationEnvelopePackingSourceWindow spends only bytes left in the normal
// localization envelope. It never uses the retired-read allowance and never
// removes existing evidence, outlines, bodies, or completion fields.
func localizationEnvelopePackingSourceWindow(
	envelope localizationExploreEnvelope,
	window *localizationSourceWindow,
	maxBytes int,
) localizationExploreEnvelope {
	if window == nil || maxBytes < 1 {
		return envelope
	}
	anchorPresent := false
	for _, symbol := range envelope.Symbols {
		if symbol == window.AnchorSymbol {
			anchorPresent = true
			break
		}
	}
	if !anchorPresent {
		return envelope
	}
	candidate := envelope
	candidate.SourceWindow = window.clone()
	for {
		if localizationEnvelopeFits(candidate, maxBytes) {
			return candidate
		}
		if !candidate.SourceWindow.shrinkOuterLine() {
			return envelope
		}
	}
}

func localizationShedSourceWindow(envelope *localizationExploreEnvelope) bool {
	if envelope == nil || envelope.SourceWindow == nil {
		return false
	}
	envelope.SourceWindow = nil
	return true
}
