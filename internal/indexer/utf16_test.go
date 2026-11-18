package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// utf16LEWithBOM encodes s as UTF-16LE with the byte-order mark a Windows
// tooling writer puts on the file.
func utf16LEWithBOM(t *testing.T, s string) []byte {
	t.Helper()
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+len(units)*2)
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// utf16BEWithBOM encodes s as UTF-16BE with the byte-order mark.
func utf16BEWithBOM(t *testing.T, s string) []byte {
	t.Helper()
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+len(units)*2)
	out = append(out, 0xFE, 0xFF)
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

// utf16LENoBOM encodes s as UTF-16LE without a mark — the shape only the
// alternating-NUL heuristic can recognise.
func utf16LENoBOM(t *testing.T, s string) []byte {
	t.Helper()
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func TestDecodeUTF16Source(t *testing.T) {
	const csSrc = "namespace Demo;\npublic class Widget { public void Ping() { } }\n"
	// Long enough that the no-BOM heuristic has a real sample: at least
	// utf16MinNULs alternating NULs, i.e. >= 128 ASCII code units.
	const longSrc = "namespace Demo;\npublic class Widget {\n    public void Ping() { }\n    public void Pong() { }\n    public int Count() { return 2; }\n}\n"

	t.Run("LE with BOM decodes to the original UTF-8 text", func(t *testing.T) {
		require.Equal(t, csSrc, string(decodeUTF16Source(utf16LEWithBOM(t, csSrc))))
	})
	t.Run("BE with BOM decodes to the original UTF-8 text", func(t *testing.T) {
		require.Equal(t, csSrc, string(decodeUTF16Source(utf16BEWithBOM(t, csSrc))))
	})
	t.Run("LE without BOM decodes via the heuristic", func(t *testing.T) {
		require.Equal(t, longSrc, string(decodeUTF16Source(utf16LENoBOM(t, longSrc))))
	})
	t.Run("line structure is preserved 1:1", func(t *testing.T) {
		const crlf = "namespace Demo;\r\npublic class W {\r\n}\r\n"
		require.Equal(t, crlf, string(decodeUTF16Source(utf16LEWithBOM(t, crlf))))
	})
	t.Run("surrogate pairs survive the decode", func(t *testing.T) {
		const emoji = "namespace D;\n// \U0001F600 emoji\n"
		require.Equal(t, emoji, string(decodeUTF16Source(utf16LEWithBOM(t, emoji))))
	})
	t.Run("UTF-8 source passes through untouched", func(t *testing.T) {
		src := []byte(csSrc)
		out := decodeUTF16Source(src)
		require.True(t, &src[0] == &out[0], "a UTF-8 source must be returned unchanged, not re-encoded")
	})
	t.Run("plain ASCII without NULs passes through", func(t *testing.T) {
		src := []byte("package p\nfunc F() {}\n")
		out := decodeUTF16Source(src)
		require.True(t, &src[0] == &out[0])
	})
	t.Run("a short file cannot trip the no-BOM heuristic", func(t *testing.T) {
		// Below utf16MinNULs alternating NULs the heuristic must not claim
		// UTF-16: a short NUL-bearing binary prefix would decode nonsense.
		src := []byte("a\x00b\x00c\x00d\x00e\x00f\x00")
		out := decodeUTF16Source(src)
		require.True(t, &src[0] == &out[0])
	})
	t.Run("binary content with NULs at both parities passes through", func(t *testing.T) {
		src := make([]byte, 512)
		for i := range src {
			src[i] = byte(i * 7)
			if i%3 == 0 {
				src[i] = 0x00
			}
		}
		out := decodeUTF16Source(src)
		require.Equal(t, src, out)
	})
	t.Run("all-NUL content passes through", func(t *testing.T) {
		src := make([]byte, 256)
		require.Equal(t, src, decodeUTF16Source(src))
	})
	t.Run("a dangling trailing byte passes through", func(t *testing.T) {
		src := append(utf16LEWithBOM(t, csSrc), 0x41)
		out := decodeUTF16Source(src)
		require.Equal(t, src, out)
	})
	t.Run("decoded output is always valid UTF-8", func(t *testing.T) {
		require.True(t, utf8.Valid(decodeUTF16Source(utf16LEWithBOM(t, "namespace D;\n// café ñ 日本\n"))))
	})
}

// TestIndexIndexesUTF16CSharpSource is the #812 reproduction: a tracked
// UTF-16LE C# file used to vanish from the graph — no symbols, no failure,
// no warning — while absence queries read as authoritative. After the
// decode transform it carries the same nodes as its UTF-8 sibling.
func TestIndexIndexesUTF16CSharpSource(t *testing.T) {
	dir := t.TempDir()
	utf8Src := "namespace Demo;\npublic class Utf8Widget { public void Ping() { } }\n"
	utf16Src := "namespace Demo;\npublic class LegacyWidget { public void Pong() { } }\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Utf8Source.cs"), []byte(utf8Src), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "LegacyEncodedSource.cs"), utf16LEWithBOM(t, utf16Src), 0o644))

	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	var utf8Widget, legacyWidget bool
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		switch n.Name {
		case "Utf8Widget":
			utf8Widget = true
		case "LegacyWidget":
			legacyWidget = true
		}
	}
	require.True(t, utf8Widget, "the UTF-8 control class must be indexed")
	require.True(t, legacyWidget, "the UTF-16LE file must be decoded and indexed, not silently omitted")

	// The decode must not leave a silent-zero diagnostic behind: the file
	// genuinely indexed.
	require.Empty(t, LoadFileIndexFailures(g, ""),
		"a successfully decoded file must not be flagged as a silent zero")
}

// TestIndexIndexesBOMLessUTF16Source covers the heuristic half: no BOM,
// detection purely from alternating NULs.
func TestIndexIndexesBOMLessUTF16Source(t *testing.T) {
	dir := t.TempDir()
	src := "namespace Demo;\npublic class NoBomWidget {\n    public void Ping() { }\n    public void Pong() { }\n    public int Count() { return 3; }\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "NoBom.cs"), utf16LENoBOM(t, src), 0o644))

	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	var found bool
	for _, n := range g.AllNodes() {
		if n != nil && n.Name == "NoBomWidget" {
			found = true
		}
	}
	require.True(t, found, "a BOM-less UTF-16LE file must be recognised and indexed")
}

// TestPrepareCoordinateStableRefusesUTF16 pins the coordinate contract:
// rename recovery edits the raw file, so a decoded buffer (whose byte
// offsets no longer describe the bytes on disk) must be refused there.
func TestPrepareCoordinateStableRefusesUTF16(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	idx := newContentSourceIndexer(t, g)

	src := utf16LEWithBOM(t, "namespace Demo;\npublic class W { }\n")
	_, err := idx.transforms.prepareCoordinateStable(filepath.Join(dir, "W.cs"), src)
	require.Error(t, err, "a coordinate-stable preparation must refuse UTF-16 — its output cannot anchor an edit of the raw file")
}

// TestSilentZeroLedgerLifecycle pins the backstop's lifecycle: a queued
// silent zero survives the success-path receipt clear and lands in the
// ledger at flush, and an explicit success note clears it.
func TestSilentZeroLedgerLifecycle(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	// Index a real file so the pass machinery (state load, receipts) is
	// exercised around the pending set.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package p\nfunc F() {}\n"), 0o644))
	_, err := idx.Index(dir)
	require.NoError(t, err)

	idx.noteSilentZeroExtraction("broken.go")
	// The pass-end flush must translate the pending set into ledger rows.
	idx.flushFileIndexFailures()
	rows := LoadFileIndexFailures(g, "")
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Error, "no symbols")

	// A later pass whose receipt recording succeeds clears the row, and a
	// file that no longer silent-zeros is never re-queued: the ledger
	// empties without a separate recovery path.
	idx.noteFileIndexFailure(filepath.Join(dir, "broken.go"), nil)
	idx.flushFileIndexFailures()
	require.Empty(t, LoadFileIndexFailures(g, ""))
}

// TestSilentZeroExtraction pins the detector: only a partial-health file
// node with zero symbols counts — a legitimate empty file and a healthy
// result must both stay out of the ledger.
func TestSilentZeroExtraction(t *testing.T) {
	partialFile := &graph.Node{
		ID: "a.cs", Kind: graph.KindFile, Name: "a.cs", FilePath: "a.cs",
		Meta: map[string]any{"parse_health": "partial"},
	}
	require.True(t, silentZeroExtraction(&parser.ExtractionResult{Nodes: []*graph.Node{partialFile}}),
		"a partial-health file node with zero symbols is the #812 shape")

	healthyFile := &graph.Node{ID: "a.cs", Kind: graph.KindFile, Name: "a.cs", FilePath: "a.cs"}
	require.False(t, silentZeroExtraction(&parser.ExtractionResult{Nodes: []*graph.Node{healthyFile}}),
		"a clean file node is not a silent zero")

	emptyFile := &graph.Node{
		ID: "empty.go", Kind: graph.KindFile, Name: "empty.go", FilePath: "empty.go",
	}
	require.False(t, silentZeroExtraction(&parser.ExtractionResult{Nodes: []*graph.Node{emptyFile}}),
		"an empty file legitimately yields zero symbols; it must not be flagged")

	require.False(t, silentZeroExtraction(nil))
	require.False(t, silentZeroExtraction(&parser.ExtractionResult{}))
}

// TestUTF16DecodeTransform_SkipsAssetExtensions pins the review's minor:
// the decoder must not run on content destined for an AssetExtractor
// (image / PDF / office / data) — that content is binary on purpose, and
// feeding it to the decoder is where the one heuristic misdetection came
// from. Source extensions still decode.
func TestUTF16DecodeTransform_SkipsAssetExtensions(t *testing.T) {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	p := newTransformPipeline(nil, reg, zap.NewNop())

	payload := utf16LEWithBOM(t, "código declarado en UTF-16")
	// The BOM stripper is always on, so an asset file loses the mark; the
	// pinned point is that the payload is NOT transcoded.
	stripped := stripBOM(payload)

	require.Equal(t, stripped, p.run("report.pdf", payload),
		"a PDF-bound payload must reach its asset extractor untranslated")
	require.Equal(t, stripped, p.run("photo.png", payload),
		"an image-bound payload must reach its asset extractor untranslated")
	require.Equal(t, stripped, p.run("sheet.xlsx", payload),
		"an office-bound payload must reach its asset extractor untranslated")
	require.NotEqual(t, payload, p.run("src.cs", payload),
		"a source extension still decodes")
	require.Equal(t, "código declarado en UTF-16", string(p.run("src.cs", payload)))
}

// TestRegistryAssetExtensions sanity-checks the skip set the decoder is
// built from: asset-bearing extensions are in it, source extensions are not.
func TestRegistryAssetExtensions(t *testing.T) {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	ext := reg.AssetExtensions()
	require.NotEmpty(t, ext)
	for _, a := range []string{".pdf", ".png", ".jpg", ".xlsx", ".pptx"} {
		require.Contains(t, ext, a, "%s is an asset extension", a)
	}
	for _, s := range []string{".go", ".cs", ".ts"} {
		require.NotContains(t, ext, s, "%s is a source extension", s)
	}
}
