package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// repeatedFragmentSource has the same body in two functions, so a fragment
// matched against the wrong range edits the wrong function.
const repeatedFragmentSource = "package sample\n\nfunc first() error {\n\treturn nil\n}\n\n// second documents itself.\nfunc second() error {\n\treturn nil\n}\n"

func TestLocateSymbolEdit(t *testing.T) {
	src := repeatedFragmentSource
	secondAt := strings.Index(src, "func second")
	crlf := strings.ReplaceAll(src, "\n", "\r\n")

	cases := []struct {
		name       string
		content    string
		start, end int
		fragment   string
		want       string // the bytes the span covers
		after      int    // the span starts at or after this offset
		normalized bool
		err        error
	}{
		{name: "first", content: src, start: 3, end: 5, fragment: "return nil", want: "return nil"},
		{name: "second", content: src, start: 8, end: 10, fragment: "return nil", want: "return nil", after: secondAt},
		{name: "doc comment above", content: src, start: 8, end: 10, fragment: "// second documents itself.\nfunc second", want: "// second documents itself.\nfunc second"},
		{name: "crlf region", content: crlf, start: 8, end: 10, fragment: "error {\n\treturn nil", want: "error {\r\n\treturn nil", normalized: true},
		{name: "repeated inside the symbol", content: "package p\n\nfunc both() {\n\tx++\n\tx++\n}\n", start: 3, end: 6, fragment: "x++", err: errEditFragmentRepeated},
		{name: "outside the symbol", content: src, start: 3, end: 5, fragment: "func second", err: errEditFragmentNotFound},
		{name: "range past the end", content: src, start: 8, end: 20, fragment: "return nil", err: errSymbolRangeOutsideFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := locateSymbolEdit(tc.content, tc.start, tc.end, tc.fragment)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, tc.content[got.start:got.end])
			require.GreaterOrEqual(t, got.start, tc.after)
			require.Equal(t, tc.normalized, got.normalized)
		})
	}
}

// TestSymbolEditFollowsLineShiftPastRepeatedFragment covers the edit that
// lands in the wrong function: another session's edit shifted the file, so
// the stored range of second now covers first's body, where the fragment
// also occurs exactly once. Resolving the range against the bytes read under
// the lock keeps the edit in second.
func TestSymbolEditFollowsLineShiftPastRepeatedFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	src := strings.Replace(repeatedFragmentSource, "// second documents itself.\n", "", 1)
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	ctx := context.Background()

	node := srv.engineFor(ctx).GetSymbol("sample.go::second")
	require.NotNil(t, node)
	require.Equal(t, [2]int{7, 9}, [2]int{node.StartLine, node.EndLine})

	shifted := strings.Replace(src, "func first", "// a\n// b\n// c\nfunc first", 1)
	secondAt := strings.Index(shifted, "func second")
	stale, err := locateSymbolEdit(shifted, node.StartLine, node.EndLine, "\treturn nil")
	require.NoError(t, err)
	require.Less(t, stale.start, secondAt, "the stored range alone lands the edit in first")

	start, end, err := srv.nodeLinesIn(ctx, node, []byte(shifted))
	require.NoError(t, err)
	require.Equal(t, [2]int{10, 12}, [2]int{start, end})
	edit, err := locateSymbolEdit(shifted, start, end, "\treturn nil")
	require.NoError(t, err)
	require.Greater(t, edit.start, secondAt)
}
