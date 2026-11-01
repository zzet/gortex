package mcp

import (
	"errors"
	"strings"
)

var (
	errSymbolRangeOutsideFile = errors.New("symbol line range exceeds file length")
	errEditFragmentNotFound   = errors.New("old_source not found within the symbol")
	errEditFragmentRepeated   = errors.New("old_source appears more than once within the symbol; provide a larger fragment")
)

// symbolEdit locates old_source inside a symbol: byte offsets into the file
// content, and whether the match bridged a line-ending difference, in which
// case the replacement must adopt the region's own terminators.
type symbolEdit struct {
	start, end int
	normalized bool
}

// locateSymbolEdit finds oldSource inside the symbol on lines
// [startLine, endLine] of content: within the symbol itself, or else within
// the symbol plus the comment and blank lines directly above it, since agents
// often quote a symbol's doc comment. The fragment must occur exactly once in
// the region it matches, so a fragment repeated elsewhere in the file never
// pulls the edit into another symbol. The range must describe content: take
// it from nodeLinesIn over the same bytes, never from a stored node alone.
func locateSymbolEdit(content string, startLine, endLine int, oldSource string) (symbolEdit, error) {
	lines := strings.Split(content, "\n")
	if startLine < 1 || endLine < startLine || endLine > len(lines) {
		return symbolEdit{}, errSymbolRangeOutsideFile
	}
	offsets := make([]int, endLine)
	for i := 1; i < endLine; i++ {
		offsets[i] = offsets[i-1] + len(lines[i-1]) + 1
	}
	end := offsets[endLine-1] + len(lines[endLine-1])
	from := startLine - 1
	matches := findEOLMatches(content[offsets[from]:end], oldSource)
	if matches.count == 0 {
		above := from
		for above > 0 && isDocOrBlankLine(lines[above-1]) {
			above--
		}
		if above < from {
			from = above
			matches = findEOLMatches(content[offsets[from]:end], oldSource)
		}
	}
	switch {
	case matches.count == 0:
		return symbolEdit{}, errEditFragmentNotFound
	case matches.count > 1:
		return symbolEdit{}, errEditFragmentRepeated
	}
	span := matches.spans[0]
	return symbolEdit{
		start:      offsets[from] + span.start,
		end:        offsets[from] + span.end,
		normalized: matches.normalized,
	}, nil
}

// isDocOrBlankLine reports whether line can belong to the doc comment block
// above a declaration.
func isDocOrBlankLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "//") ||
		strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*")
}
