package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	toon "github.com/toon-format/toon-go"

	"github.com/zzet/gortex/internal/tokens"
)

// Retrieval savings — the second half of the ledger.
//
// The original recording sites (read_file, get_file_summary,
// get_editing_context, get_symbol_source, batch_symbols, smart_context) all
// answer the same question: "this response stands in for reading ONE whole
// file, how much did that save?". That model leaves out every tool whose whole
// point is to replace a grep-then-read sweep across SEVERAL files — explore,
// search, relations, trace — which is where the agent actually stops paying
// for raw file reads. Those tools booked nothing at all, so a session that
// navigated entirely through the graph showed a flat ledger.
//
// This file generalises the same counterfactual from one file to the file SET
// a retrieval page stands in for:
//
//	returned = tokens of the page actually shipped
//	fullFile = Σ over the distinct files the page cites of that file's
//	           whole-file token estimate
//
// with two deliberate limits that keep the number honest:
//
//  1. Per session, a file's whole-file baseline is credited AT MOST ONCE
//     (creditedFile below). The counterfactual agent that already opened a
//     file does not pay to open it again, so neither may we. This bounds
//     everything a session can ever claim to the cost of reading its repo
//     once.
//  2. Per call, at most retrievalBaselineMaxFiles distinct files are credited.
//     A 50-hit find_usages page does not mean the caller would have opened 50
//     files — it would have opened the few hits it cared about. We cannot know
//     which, so we credit the conservative end of the range.
//
// Both limits under-report on purpose. The ledger's failure mode must be
// "Gortex looks worse than it is", never "Gortex invented savings".

// retrievalBaselineMaxFiles caps how many distinct cited files one retrieval
// call may credit. Retrieval pages cite far more files than their caller would
// have opened: search_symbols defaults to 20 hits and find_usages to 50, but an
// agent following either typically opens a handful before it has its answer.
// Crediting every cited file would let one call mint a six-figure baseline; the
// cap keeps a single observation the same order of magnitude as the read-family
// observations it sits beside in the ledger.
const retrievalBaselineMaxFiles = 8

// maxRetrievalCitationScan bounds how much of a response is swept for
// citations, so a pathological page cannot turn accounting into a hot loop.
// Mirrors maxFreshnessSweep, which bounds the same sweep for the freshness
// rider.
const maxRetrievalCitationScan = 256

// retrievalCitationColumns are the GCX1 column names that carry a
// repo-relative file path. Deliberately excludes path_abs / *_abs: absolute
// columns name the same file as their relative sibling and would be dropped by
// the absolute-path filter anyway, but naming them here would suggest they are
// a second citation.
var retrievalCitationColumns = map[string]bool{
	"path": true, "file": true, "file_path": true, "filepath": true,
	"from_path": true, "to_path": true, "relative_path": true,
}

// retrievalSavingsTools is the allow-list of legacy tool names whose response
// displaces raw file reading and therefore has a defensible whole-file
// baseline. Membership is a judgement about the counterfactual, so it is
// explicit rather than inferred:
//
//   - The six tools that already book their own observation are ABSENT. They
//     record inside their handler with a baseline they measure exactly; adding
//     them here would double-count.
//   - export_context is ABSENT: it delegates its whole retrieval to
//     handleSmartContext, which books the observation.
//   - The response.* re-cut tools (ctx_peek / ctx_slice / ctx_grep /
//     ctx_stats) are ABSENT: they re-cut a page the originating tool already
//     shipped, so the file was displaced once, not twice.
//   - Metadata, control, and write tools are ABSENT: nothing about their
//     output stands in for reading a file.
var retrievalSavingsTools = map[string]bool{
	// explore — the mandated first call of a task. Ships ranked targets with
	// full symbol bodies and redacted source windows.
	"explore":          true,
	"context_closure":  true,
	"prefetch_context": true,
	"get_repo_outline": true,

	// search — the declared grep replacement.
	"search_symbols":   true,
	"search_text":      true,
	"search_ast":       true,
	"search_artifacts": true,
	"winnow_symbols":   true,

	// read — the paths that serve file bytes without booking themselves.
	"get_artifact": true,

	// relations — "use instead of Grep to find every reference".
	"find_usages":          true,
	"get_callers":          true,
	"get_dependencies":     true,
	"get_dependents":       true,
	"find_implementations": true,
	"find_declaration":     true,
	"find_overrides":       true,
	"find_import_path":     true,
	"check_references":     true,
	"get_class_hierarchy":  true,
	"get_cluster":          true,

	// trace — path and flow pages, each row a real location the caller would
	// otherwise have had to open the file to find.
	"get_call_chain": true,
	"get_cfg":        true,
	"flow_between":   true,
	"taint_paths":    true,
	"trace_path":     true,
	"walk_graph":     true,
	"graph_query":    true,

	// change/session paths that serve real source bodies.
	"nav":             true,
	"suggest_pattern": true,
}

// recordRetrievalSavings books the file-set baseline for one retrieval call.
// tool is the LEGACY tool name (the facade forwards under it, so the ledger's
// per-tool breakdown stays comparable across surfaces). No-ops for every tool
// outside retrievalSavingsTools, for error results, and for responses that
// cite no file the session has not already been credited for.
func (s *Server) recordRetrievalSavings(ctx context.Context, tool string, res *mcp.CallToolResult) {
	if s == nil || res == nil || res.IsError || !retrievalSavingsTools[tool] {
		return
	}
	payload, ok := singleTextContent(res)
	if !ok || payload == "" {
		return
	}
	s.recordFileSetBaselineSavings(ctx, tool, citedFilesFromResult(payload), payload)
}

// recordFileSetBaselineSavings is recordFileBaselineSavings generalised from
// one file to the set a page stands in for: same resolve/stat/estimate
// discipline per file, summed into a single observation so one call books one
// row. Files already credited to this session, and files past the per-call cap,
// contribute nothing.
func (s *Server) recordFileSetBaselineSavings(ctx context.Context, tool string, relPaths []string, payload string) {
	if len(relPaths) == 0 || payload == "" {
		return
	}
	stats := s.tokenStatsFor(ctx)
	if stats == nil {
		return
	}
	var (
		fullFile int64
		credited int
		language string
		attrPath string
	)
	for _, rel := range relPaths {
		if credited >= retrievalBaselineMaxFiles {
			break
		}
		abs, err := s.resolveGraphPath(rel)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		// Claim the file before counting it: a file already credited to this
		// session was already paid for by whichever call surfaced it first.
		if !stats.creditFile(abs) {
			continue
		}
		fullFile += int64(tokens.EstimateFromSample(int(info.Size()), fileHeadSample(abs)))
		credited++
		if attrPath == "" {
			attrPath = rel
			language = s.detectLanguageForPath(ctx, abs, rel)
		}
	}
	if credited == 0 || fullFile <= 0 {
		return
	}
	stats.record(s.fileAttributionNode(attrPath, language), tool, tokens.CachedCountInt64(payload), fullFile)
}

// baselineSampleBytes bounds the calibration read per credited file. The
// existing recorders can hand EstimateFromSample a slice of the very file they
// are pricing; a retrieval page cannot — its own text is a dense citation table
// whose chars-per-token ratio is nothing like the source it stands in for, and
// calibrating on it would mis-price every baseline. So read a bounded head of
// each file instead. Files at or under this size are counted exactly.
const baselineSampleBytes = 8192

// fileHeadSample returns up to baselineSampleBytes of a file, for use as the
// chars-per-token calibration sample for that same file. Returns "" on any
// read error, which makes EstimateFromSample fall back to its chars/4
// heuristic rather than skipping the file.
func fileHeadSample(abs string) string {
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, baselineSampleBytes)
	n, err := f.Read(buf)
	if n <= 0 || (err != nil && err != io.EOF) {
		return ""
	}
	return string(buf[:n])
}

// citedFilesFromResult extracts the distinct repo-relative files a tool
// response cites, across every wire format Gortex emits. Absolute paths are
// skipped: they name the same file as their relative sibling column (path_abs
// next to path), and an absolute path outside any tracked repo is not a
// citation this ledger can attribute.
func citedFilesFromResult(payload string) []string {
	text := strings.TrimSpace(payload)
	if text == "" {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || filepath.IsAbs(p) || seen[p] || len(out) >= maxRetrievalCitationScan {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if strings.HasPrefix(text, "GCX1") {
		collectGCXCitedPaths(text, add)
		return out
	}
	switch text[0] {
	case '{':
		var asObj map[string]any
		if json.Unmarshal([]byte(text), &asObj) == nil {
			for _, p := range collectGraphFilePaths(asObj) {
				add(p)
			}
			return out
		}
	case '[':
		var asArr []any
		if json.Unmarshal([]byte(text), &asArr) == nil {
			for _, p := range collectGraphFilePaths(asArr) {
				add(p)
			}
			return out
		}
	}
	// TOON is the third wire format Gortex emits, and it is the DEFAULT shape
	// of several retrieval tools (search_text, get_repo_outline, nav). Decoding
	// it costs a parse of a payload we already hold; a decode failure just
	// means this call books nothing.
	if len(text) <= maxToonCitationBytes {
		if decoded, derr := toon.Decode([]byte(text)); derr == nil {
			for _, p := range collectGraphFilePaths(decoded) {
				add(p)
			}
		}
	}
	return out
}

// maxToonCitationBytes bounds the TOON decode attempt so a very large text
// response cannot turn accounting into the expensive part of the call.
const maxToonCitationBytes = 1 << 20

// collectGCXCitedPaths walks the GCX1 compact wire format: one or more blocks,
// each a `GCX1 tool=… fields=a,b,c …` header followed by tab-separated rows.
// Path columns are located by name from the header, so a schema change adds or
// drops citations rather than silently shifting them by one column.
func collectGCXCitedPaths(text string, add func(string)) {
	var cols []int
	width := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "GCX1") {
			cols, width = gcxPathColumns(line)
			continue
		}
		if len(cols) == 0 || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		// A line that does not carry the block's full column count is not a
		// row: GCX1 payloads can be followed by prose riders (the momentum
		// note, a warming banner), and reading column N out of a sentence
		// would credit a "file" that never existed.
		if len(fields) != width {
			continue
		}
		for _, idx := range cols {
			add(fields[idx])
		}
	}
}

// gcxPathColumns returns the indexes of the path-bearing columns declared by a
// GCX1 header line, in column order, together with the block's total column
// count.
func gcxPathColumns(header string) ([]int, int) {
	spec := ""
	for _, tok := range strings.Fields(header) {
		if rest, ok := strings.CutPrefix(tok, "fields="); ok {
			spec = rest
			break
		}
	}
	if spec == "" {
		return nil, 0
	}
	names := strings.Split(spec, ",")
	var cols []int
	for i, name := range names {
		if retrievalCitationColumns[strings.TrimSpace(name)] {
			cols = append(cols, i)
		}
	}
	sort.Ints(cols)
	return cols, len(names)
}
