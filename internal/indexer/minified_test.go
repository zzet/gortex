package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBinaryArtifactReason(t *testing.T) {
	// A Serena-style tool-cache pickle: a claimed extension holding a
	// binary payload — the exact file class this guard targets.
	pickle := append([]byte("\x80\x04\x95\x1a\x00"), 0x00, 0x00)
	assert.NotEqual(t, "", binaryArtifactReason(pickle))

	// A NUL byte inside the sniff window is binary, however late it lands.
	lateNUL := append([]byte(strings.Repeat("package main\n", 500)), 0x00)
	assert.NotEqual(t, "", binaryArtifactReason(lateNUL))

	// The sniff covers the first 8 KiB only (the git heuristic): a NUL
	// past the window is not caught here. The ParseFile guard shares the
	// same window by design, and tree-sitter is error-tolerant on the
	// tail — the pathological input class is a binary header, which
	// always NULs within the window.
	pastWindow := append([]byte(strings.Repeat("package main\n", 900)), 0x00)
	assert.Equal(t, "", binaryArtifactReason(pastWindow))

	// Genuine source — including embedded odd bytes but no NUL — is text.
	normal := strings.Repeat("func handle(req Request) error { return nil }\n", 60)
	assert.Equal(t, "", binaryArtifactReason([]byte(normal)))
	assert.Equal(t, "", binaryArtifactReason(nil))
}

func TestMinifiedArtifactReason(t *testing.T) {
	// A minified bundle — the whole file on one very long line.
	minified := strings.Repeat("function a(b){return b+1};", 200)
	assert.Equal(t, "minified", minifiedArtifactReason("javascript", []byte(minified)))

	// A Source Map v3 document — classified regardless of language.
	sourcemap := `{"version":3,"file":"app.js","sources":["app.ts"],"names":[],"mappings":"` +
		strings.Repeat("AAAA;", 500) + `"}`
	assert.Equal(t, "sourcemap", minifiedArtifactReason("json", []byte(sourcemap)))

	// Generated code with a sourceMappingURL link but normal line
	// lengths — a (possibly pretty-printed) bundle.
	bundled := strings.Repeat("const value = helper(x, y);\n", 90) +
		"//# sourceMappingURL=app.js.map\n"
	assert.Equal(t, "bundled", minifiedArtifactReason("javascript", []byte(bundled)))

	// Genuine, multi-line source — not an artifact.
	normal := strings.Repeat("const result = computeValue(alpha, beta);\n", 80)
	assert.Equal(t, "", minifiedArtifactReason("javascript", []byte(normal)))

	// A small file is never classified, however dense.
	assert.Equal(t, "", minifiedArtifactReason("javascript", []byte("a=1;a=1;a=1;")))

	// The line-length heuristic is gated to JS/TS/CSS — a long-lined
	// file in another language is left alone.
	longGo := "package main\n" + strings.Repeat("x", 4000)
	assert.Equal(t, "", minifiedArtifactReason("go", []byte(longGo)))

	// Ordinary Go source is never an artifact.
	goSrc := strings.Repeat("func handle(req Request) error { return nil }\n", 60)
	assert.Equal(t, "", minifiedArtifactReason("go", []byte(goSrc)))
}

func TestLooksLikeSourceMap(t *testing.T) {
	assert.True(t, looksLikeSourceMap([]byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA"}`)))
	assert.True(t, looksLikeSourceMap([]byte(`  {"version": 3, "mappings": "AAAA"}`)))
	assert.False(t, looksLikeSourceMap([]byte(`{"version":2,"sources":["a.ts"]}`)))
	assert.False(t, looksLikeSourceMap([]byte(`function f(){}`)))
	assert.False(t, looksLikeSourceMap([]byte(`{"name":"pkg","version":"3.0.0"}`)))
}
