package indexer

import (
	"bytes"

	"github.com/zzet/gortex/internal/parser"
)

// Tuning constants for build-artifact detection. The thresholds are
// deliberately conservative — a false positive silently drops a real
// source file, which is worse than indexing the odd bundle.
const (
	// minifiedMinBytes is the floor below which a file is never
	// classified as an artifact — a tiny file is cheap to index and
	// too small to be a meaningful bundle.
	minifiedMinBytes = 2048
	// minifiedAvgLineLen is the average line length above which a
	// JS/TS/CSS file is treated as minified. Hand-written code averages
	// well under 150 chars/line even when dense; a minified bundle
	// averages thousands.
	minifiedAvgLineLen = 500
)

// minifiableLang gates the line-length and bundle-marker heuristics to
// the languages minification actually targets. Sourcemap detection is
// language-agnostic and runs regardless.
var minifiableLang = map[string]bool{
	"javascript": true,
	"typescript": true,
	"css":        true,
}

// binaryArtifactReason classifies src as a binary payload — not text any
// grammar can consume — and returns a short human reason, or "" when src
// could still be genuine source. The sniff is the same NUL-prefix tell
// git uses and shares one definition with the parser's ParseFile guard
// (parser.LooksBinary), so a file the indexer skips as binary is exactly
// the file the parse guard would refuse.
//
// There is deliberately no config gate on this class: unlike a minified
// bundle, NUL-bearing bytes can never parse as text, so "index it anyway"
// is never a meaningful user choice — only a slower way to burn the parse
// budget. Tool caches under a claimed extension are the
// reported case: Serena rewrites .serena/cache/*.pkl, the .pkl extension
// belongs to the Pkl language, and every reconcile re-fed the binary into
// the fallback chunker for a ~15s timeout with zero nodes.
func binaryArtifactReason(src []byte) string {
	if !parser.LooksBinary(src) {
		return ""
	}
	// A UTF-16 text source is NUL-interleaved like any binary payload;
	// name it distinctly so a text file is not mislabelled as binary in
	// index_health telemetry.
	if len(src) >= 2 && ((src[0] == 0xFF && src[1] == 0xFE) || (src[0] == 0xFE && src[1] == 0xFF)) {
		return "utf-16 text source (NUL-interleaved; nothing a text grammar can extract)"
	}
	return "binary source (NUL byte within the first 8 KiB)"
}

// minifiedArtifactReason classifies src as a build artifact that
// should not be indexed as source — a sourcemap, a minified bundle, or
// generated code carrying a sourcemap link. It returns a short reason
// ("sourcemap" / "minified" / "bundled") or "" when src is genuine
// source. Detection is high-precision by design: every heuristic keys
// on a signal that does not occur in hand-written code.
func minifiedArtifactReason(lang string, src []byte) string {
	if len(src) < minifiedMinBytes {
		return ""
	}
	if looksLikeSourceMap(src) {
		return "sourcemap"
	}
	if !minifiableLang[lang] {
		return ""
	}
	// A sourceMappingURL annotation is emitted only by build tooling —
	// a hand-written source file never carries one.
	if bytes.Contains(src, []byte("//# sourceMappingURL=")) ||
		bytes.Contains(src, []byte("//@ sourceMappingURL=")) {
		return "bundled"
	}
	if avgLineLength(src) > minifiedAvgLineLen {
		return "minified"
	}
	return ""
}

// looksLikeSourceMap reports whether src is a Source Map v3 document —
// a JSON object carrying "version":3 plus a "mappings" or "sources"
// member. Only the head of the file is inspected.
func looksLikeSourceMap(src []byte) bool {
	head := bytes.TrimLeft(src, " \t\r\n")
	if len(head) == 0 || head[0] != '{' {
		return false
	}
	if len(head) > 4096 {
		head = head[:4096]
	}
	hasVersion := bytes.Contains(head, []byte(`"version":3`)) ||
		bytes.Contains(head, []byte(`"version": 3`))
	hasMap := bytes.Contains(head, []byte(`"mappings"`)) ||
		bytes.Contains(head, []byte(`"sources"`))
	return hasVersion && hasMap
}

// avgLineLength returns the mean number of bytes per line in src.
func avgLineLength(src []byte) int {
	lines := bytes.Count(src, []byte{'\n'}) + 1
	return len(src) / lines
}
