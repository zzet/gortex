package indexer

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestStripBOM(t *testing.T) {
	body := []byte("package main\n")
	require.Equal(t, body, stripBOM(append([]byte{0xEF, 0xBB, 0xBF}, body...)))  // UTF-8: stripped
	// UTF-16 marks are preserved on purpose: they are the detection signal
	// parser.ParseFile uses to reject the file before a pathological parse.
	utf16LE := append([]byte{0xFF, 0xFE}, body...)
	require.Equal(t, utf16LE, stripBOM(utf16LE))
	utf16BE := append([]byte{0xFE, 0xFF}, body...)
	require.Equal(t, utf16BE, stripBOM(utf16BE))
	require.Equal(t, body, stripBOM(body))                                      // no BOM
	require.Equal(t, []byte{}, stripBOM([]byte{}))                              // empty
}

func TestTransformPipeline_BOMStripIsBuiltIn(t *testing.T) {
	p := newTransformPipeline(nil, zap.NewNop())
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte("package main\n")...)
	require.Equal(t, []byte("package main\n"), p.run("x.go", withBOM))
}

func TestTransformPipeline_NilSafe(t *testing.T) {
	var p *transformPipeline
	require.Equal(t, []byte("x"), p.run("a.go", []byte("x")))
	require.Equal(t, "", p.languageFor("a.go"))
}

func TestTransformPipeline_RuleWithNoCommandIgnored(t *testing.T) {
	p := newTransformPipeline([]config.TransformRule{{Name: "broken"}}, zap.NewNop())
	require.Len(t, p.transforms, 1) // only the built-in BOM stripper
}

func TestCommandTransform_Matches(t *testing.T) {
	c := newCommandTransform(config.TransformRule{Extensions: []string{".SVG", ".pdf"}, Command: []string{"cat"}})
	require.True(t, c.matches("logo.svg")) // case-insensitive
	require.True(t, c.matches("doc.pdf"))
	require.False(t, c.matches("main.go"))

	all := newCommandTransform(config.TransformRule{Command: []string{"cat"}}) // no extensions → all
	require.True(t, all.matches("anything.xyz"))
}

func TestCommandTransform_Apply(t *testing.T) {
	requireBin(t, "sed")
	c := newCommandTransform(config.TransformRule{
		Command: []string{"sed", "s/foo/bar/g"},
	})
	out, err := c.apply("x.txt", []byte("foo foo"))
	require.NoError(t, err)
	require.Equal(t, "bar bar", string(out))
}

func TestCommandTransform_ApplyError(t *testing.T) {
	c := newCommandTransform(config.TransformRule{Command: []string{"gortex-no-such-binary-xyz"}})
	_, err := c.apply("x.txt", []byte("data"))
	require.Error(t, err)
}

func TestTransformPipeline_FailingCommandKeepsBytes(t *testing.T) {
	// A failing transform must not drop the file — earlier bytes win.
	p := newTransformPipeline([]config.TransformRule{{
		Name: "broken", Command: []string{"gortex-no-such-binary-xyz"},
	}}, zap.NewNop())
	require.Equal(t, []byte("untouched"), p.run("a.go", []byte("untouched")))
}

// TestIndex_TransformPipeline proves a transform rewrites bytes before
// extraction: the renamed symbol is the one that lands in the graph.
func TestIndex_TransformPipeline(t *testing.T) {
	requireBin(t, "sed")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Original() {}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.Transforms = []config.TransformRule{{
		Name: "rename", Extensions: []string{".go"},
		Command: []string{"sed", "s/Original/Renamed/g"},
	}}
	idx := New(g, reg, cfg, zap.NewNop())

	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Renamed"), "transformed symbol must be indexed")
	require.Empty(t, g.FindNodesByName("Original"), "pre-transform symbol must not be indexed")
}

// TestIndex_TransformAsLanguage proves a transform can re-type a file
// whose extension is not natively indexed.
func TestIndex_TransformAsLanguage(t *testing.T) {
	requireBin(t, "cat")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "snippet.gocode"), "package main\n\nfunc Snippet() {}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.Transforms = []config.TransformRule{{
		Name: "gocode-as-go", Extensions: []string{".gocode"},
		Command: []string{"cat"}, AsLanguage: "go",
	}}
	idx := New(g, reg, cfg, zap.NewNop())

	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 1, result.FileCount)
	require.NotEmpty(t, g.FindNodesByName("Snippet"), ".gocode file must be indexed as Go")
}

func TestEffectiveLanguage(t *testing.T) {
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Transforms = []config.TransformRule{{
		Name: "pdf", Extensions: []string{".pdf"},
		Command: []string{"cat"}, AsLanguage: "markdown",
	}}
	idx := New(g, reg, cfg, zap.NewNop())

	lang, ok := idx.effectiveLanguage("main.go", nil)
	require.True(t, ok)
	require.Equal(t, "go", lang)

	lang, ok = idx.effectiveLanguage("doc.pdf", nil)
	require.True(t, ok)
	require.Equal(t, "markdown", lang)

	_, ok = idx.effectiveLanguage("mystery.zzz", nil)
	require.False(t, ok)
}

func requireBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available on PATH", name)
	}
}
