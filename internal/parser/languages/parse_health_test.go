package languages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

func TestNativeExtractionCarriesCheckedSyntaxVerdict(t *testing.T) {
	for _, test := range []struct {
		language, path, clean string
		extractor             parser.Extractor
	}{
		{"go", "source.go", "package p\nfunc Good() {}\n", NewGoExtractor()},
		{"python", "source.py", "def good():\n    return 1\n", NewPythonExtractor()},
		{"typescript", "source.ts", "export function good(): number { return 1; }\n", NewTypeScriptExtractor()},
		{"c", "source.c", "int good(void) { return 1; }\n", NewCExtractor()},
		{"java", "P.java", "class P { int good() { return 1; } }\n", NewJavaExtractor()},
		{"rust", "source.rs", "fn good() -> i32 { 1 }\n", NewRustExtractor()},
	} {
		for _, source := range []string{test.clean, "@@@ <<<<<\n"} {
			t.Run(test.language+map[bool]string{true: "/clean", false: "/errors"}[source == test.clean], func(t *testing.T) {
				raw := []byte(source)
				tree, err := parser.ParseFile(raw, astquery.DefaultLanguageResolver(test.language))
				require.NoError(t, err)
				proof := parser.NewParseTree(tree, raw, test.language)
				defer proof.Release()
				count := proof.CountParseErrors()
				if source == test.clean {
					require.Zero(t, count)
				} else {
					require.Positive(t, count)
				}
				result, err := test.extractor.Extract(test.path, raw)
				require.NoError(t, err)
				defer result.ReleaseTree()
				require.True(t, result.ParseChecked, "unknown default cannot certify a clean projection")
				require.Equal(t, count, result.ParseErrors, "ERROR/MISSING count must match the original syntax gate")
			})
		}
	}
}

func TestPinnedGrammarHasErrorIncludesMissingNode(t *testing.T) {
	src := []byte("int good(void) { return 1 }\n")
	tree, err := parser.ParseFile(src, astquery.DefaultLanguageResolver("c"))
	require.NoError(t, err)
	defer tree.Close()
	root := tree.RootNode()
	missing := false
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		if node == nil {
			return
		}
		missing = missing || node.IsMissing()
		for i := 0; i < int(node.ChildCount()); i++ {
			visit(node.Child(i))
		}
	}
	visit(root)
	require.True(t, missing, "fixture must contain a real pinned grammar MISSING node")
	require.True(t, root.HasError(), "the clean fast path may skip walks only when HasError is false")
}
