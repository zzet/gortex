package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// shiftTestServerMain inserts two lines above main in the setupTestServer
// fixture without reindexing, moving helper from line 11 to line 13. The old
// range then covers main's closing brace.
func shiftTestServerMain(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "main.go")
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	shifted := strings.Replace(string(src), "func main() {", "// one\n// two\nfunc main() {", 1)
	require.NoError(t, os.WriteFile(path, []byte(shifted), 0o644))
}

func TestReadNodeSourceResolvesCurrentLines(t *testing.T) {
	srv, dir := setupTestServer(t)
	ctx := context.Background()
	mainGo := filepath.Join(dir, "main.go")
	stored := srv.engineFor(ctx).GetSymbol("main.go::helper")
	require.NotNil(t, stored)
	require.Equal(t, 11, stored.StartLine)
	// The handler's copy, which a later reindex does not update.
	node := *stored

	read := func() (nodeSource, uint64, error) {
		before := parser.ParseCount()
		src, err := srv.readNodeSource(ctx, &node, mainGo)
		return src, parser.ParseCount() - before, err
	}

	src, parses, err := read()
	require.NoError(t, err)
	require.Equal(t, [2]int{11, 11}, [2]int{src.startLine, src.endLine})
	require.Zero(t, parses, "bytes the index recorded reuse the stored range without parsing")

	shiftTestServerMain(t, dir)
	src, parses, err = read()
	require.NoError(t, err)
	require.Equal(t, [2]int{13, 13}, [2]int{src.startLine, src.endLine})
	require.Equal(t, "func helper() {}", strings.Split(src.content, "\n")[src.startLine-1])
	require.EqualValues(t, 1, parses, "unrecorded bytes are parsed once to locate the symbol")

	// A reindex that commits after the handler read node records these
	// bytes, so they match the index while node still holds the old range.
	require.NoError(t, srv.indexer.IndexFile(mainGo))
	src, _, err = read()
	require.NoError(t, err)
	require.Equal(t, [2]int{13, 13}, [2]int{src.startLine, src.endLine})
	require.Equal(t, "func helper() {}", strings.Split(src.content, "\n")[src.startLine-1])

	require.NoError(t, os.WriteFile(mainGo, []byte("package main\n\nfunc main() {}\n"), 0o644))
	_, _, err = read()
	require.ErrorIs(t, err, errSymbolRemoved)
}

// TestReadNodeSourcePairsPositionalIDs covers IDs derived from where a
// declaration sits: a redeclared Go init gains _L<line> while the first keeps
// the bare ID, and a TypeScript object member carries @<line>.
func TestReadNodeSourcePairsPositionalIDs(t *testing.T) {
	srv, dir := setupTestServer(t)
	ctx := context.Background()
	write := func(name, src string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
		return path
	}
	symbol := func(id string) *graph.Node {
		node := srv.engineFor(ctx).GetSymbol(id)
		require.NotNil(t, node, id)
		return node
	}
	firstLine := func(node *graph.Node, path string) (string, error) {
		src, err := srv.readNodeSource(ctx, node, path)
		if err != nil {
			return "", err
		}
		return strings.Split(src.content, "\n")[src.startLine-1], nil
	}

	const inits = "package main\n\nfunc init() { first() }\n\nfunc init() { second() }\n"
	initsGo := write("inits.go", inits)
	require.NoError(t, srv.indexer.IndexFile(initsGo))
	first, second := symbol("inits.go::init"), symbol("inits.go::init_L5")

	write("inits.go", strings.Replace(inits, "package main\n", "package main\n\n// shifted\n", 1))
	for node, want := range map[*graph.Node]string{first: "func init() { first() }", second: "func init() { second() }"} {
		got, err := firstLine(node, initsGo)
		require.NoError(t, err, node.ID)
		require.Equal(t, want, got, node.ID)
	}

	// A third init above the others takes the bare ID and renumbers the rest,
	// so neither ID nor position says which stored init is which.
	write("inits.go", strings.Replace(inits, "package main\n", "package main\n\nfunc init() { third() }\n", 1))
	for _, node := range []*graph.Node{first, second} {
		_, err := firstLine(node, initsGo)
		require.ErrorIs(t, err, errSymbolAmbiguous, node.ID)
	}

	const members = "export const handlers = {\n  onClick: () => 1,\n};\n"
	membersTS := write("members.ts", members)
	require.NoError(t, srv.indexer.IndexFile(membersTS))
	onClick := symbol("members.ts::handlers.onClick@2")
	write("members.ts", "// one\n// two\n"+members)
	got, err := firstLine(onClick, membersTS)
	require.NoError(t, err)
	require.Equal(t, "  onClick: () => 1,", got)
}

func TestDeclarationBase(t *testing.T) {
	for local, want := range map[string]string{
		"init":               "init",
		"init_L12":           "init",
		"v2_L7":              "v2",
		"handlers.onClick@4": "handlers.onClick",
		"Run#local:err@+3":   "Run#local:err",
		"Base64":             "Base64",
		"Point.X":            "Point.X",
	} {
		require.Equal(t, want, declarationBase(local), local)
	}
}
