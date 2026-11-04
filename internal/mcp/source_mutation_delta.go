package mcp

import (
	"bytes"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

type fileSymbolDelta struct {
	State     string                 `json:"state"`
	Reason    string                 `json:"reason,omitempty"`
	Counts    *fileSymbolDeltaCounts `json:"counts"`
	Symbols   []fileSymbolDeltaEntry `json:"symbols"`
	Truncated bool                   `json:"truncated"`
}
type fileSymbolDeltaCounts struct {
	Before       int `json:"before"`
	After        int `json:"after"`
	Added        int `json:"added"`
	Removed      int `json:"removed"`
	Changed      int `json:"changed"`
	TotalChanges int `json:"total_changes"`
}
type fileSymbolDeltaEntry struct {
	Change    string         `json:"change"`
	ID        string         `json:"id"`
	Kind      graph.NodeKind `json:"kind"`
	StartLine int            `json:"start_line"`
	EndLine   int            `json:"end_line"`
	Signature string         `json:"signature"`
}

// buildFileSymbolDelta compares caller-owned complete symbol sets extracted from
// the supplied decoded bytes. baselineKnown is the caller's baseline authority;
// this pure helper cannot discover omitted nodes or authenticate an extraction.
// It validates the represented spans, but does not parse, read stores, or hash.
func buildFileSymbolDelta(before, after []byte, old, new []*graph.Node, baselineKnown bool) fileSymbolDelta {
	unavailable := func(reason string) fileSymbolDelta { return fileSymbolDelta{State: "unavailable", Reason: reason} }
	if !baselineKnown {
		return unavailable("baseline_unknown")
	}
	previous, oldFile, err := fileSymbolSnapshots(before, old)
	if err != nil {
		return unavailable("before: " + err.Error())
	}
	current, newFile, err := fileSymbolSnapshots(after, new)
	if err != nil {
		return unavailable("after: " + err.Error())
	}
	if oldFile != "" && newFile != "" && oldFile != newFile {
		return unavailable("mixed_file_evidence")
	}
	counts := &fileSymbolDeltaCounts{Before: len(previous), After: len(current)}
	entries := make([]fileSymbolDeltaEntry, 0)
	for id, next := range current {
		prior, exists := previous[id]
		change := ""
		switch {
		case !exists:
			counts.Added++
			change = "added"
		case !reflect.DeepEqual(prior.node, next.node) || !bytes.Equal(prior.body, next.body):
			counts.Changed++
			change = "changed"
		}
		if change != "" {
			entries = append(entries, fileSymbolPreview(next.node, change))
		}
	}
	for id, prior := range previous {
		if _, exists := current[id]; !exists {
			counts.Removed++
			entries = append(entries, fileSymbolPreview(prior.node, "removed"))
		}
	}
	counts.TotalChanges = counts.Added + counts.Removed + counts.Changed
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Change < entries[j].Change
	})
	truncated := len(entries) > 20
	if truncated {
		entries = entries[:20]
	}
	return fileSymbolDelta{State: "available", Counts: counts, Symbols: entries, Truncated: truncated}
}

type fileSymbolSnapshot struct {
	node       *graph.Node
	body       []byte
	start, end int
}

func fileSymbolSnapshots(content []byte, nodes []*graph.Node) (map[string]fileSymbolSnapshot, string, error) {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, "", fmt.Errorf("source_not_decoded_utf8")
	}
	offsets := []int{0}
	for i, c := range content {
		if c == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	lineEnd := func(line int) int {
		if line < len(offsets) {
			return offsets[line] - 1
		}
		return len(content)
	}
	result := make(map[string]fileSymbolSnapshot, len(nodes))
	file := ""
	spans := make([]fileSymbolSnapshot, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			return nil, "", fmt.Errorf("nil_symbol")
		}
		if strings.TrimSpace(node.ID) == "" || node.Kind == "" || node.FilePath == "" {
			return nil, "", fmt.Errorf("incomplete_symbol_identity")
		}
		if node.Stub || node.Origin != "" {
			return nil, "", fmt.Errorf("nonlocal_symbol")
		}
		if _, exists := result[node.ID]; exists {
			return nil, "", fmt.Errorf("duplicate_symbol_id")
		}
		if file == "" {
			file = node.FilePath
		} else if file != node.FilePath {
			return nil, "", fmt.Errorf("mixed_file_evidence")
		}
		if signature, exists := node.Meta["signature"]; exists {
			if _, ok := signature.(string); !ok {
				return nil, "", fmt.Errorf("invalid_signature")
			}
		}
		if node.StartLine < 1 || node.EndLine < node.StartLine || node.EndLine > len(offsets) {
			return nil, "", fmt.Errorf("invalid_line_span")
		}
		start, end := offsets[node.StartLine-1], lineEnd(node.EndLine)
		columns := node.StartColumn != 0 || node.EndColumn != 0
		if node.StartColumn < 0 || node.EndColumn < 0 {
			return nil, "", fmt.Errorf("invalid_column_span")
		}
		if columns {
			if node.StartColumn > lineEnd(node.StartLine)-start || node.EndColumn > end-offsets[node.EndLine-1] {
				return nil, "", fmt.Errorf("invalid_column_span")
			}
			start += node.StartColumn
			end = offsets[node.EndLine-1] + node.EndColumn
			if start < len(content) && !utf8.RuneStart(content[start]) || end < len(content) && !utf8.RuneStart(content[end]) {
				return nil, "", fmt.Errorf("column_splits_utf8")
			}
		}
		if start >= end {
			return nil, "", fmt.Errorf("empty_or_reversed_span")
		}
		snapshot := fileSymbolSnapshot{node: node, body: content[start:end], start: start, end: end}
		spans = append(spans, snapshot)
		result[node.ID] = snapshot
	}
	// Unknown columns mean a complete inclusive line range, not invented token
	// offsets. Nested members can share their parent's lines. Equal or crossing
	// ranges, even with columns, cannot establish distinct symbol ownership.
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	for i, span := range spans {
		for _, other := range spans[i+1:] {
			if other.start >= span.end {
				break
			}
			strictlyContains := span.start <= other.start && span.end >= other.end && (span.start < other.start || span.end > other.end)
			if !strictlyContains {
				return nil, "", fmt.Errorf("ambiguous_symbol_spans")
			}
		}
	}
	return result, file, nil
}

func fileSymbolPreview(node *graph.Node, change string) fileSymbolDeltaEntry {
	signature, _ := node.Meta["signature"].(string)
	return fileSymbolDeltaEntry{Change: change, ID: node.ID, Kind: node.Kind, StartLine: node.StartLine, EndLine: node.EndLine, Signature: signature}
}
