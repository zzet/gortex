package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
)

func TestBatchSymbolLocationsReuseActualIndexedOrProjectedBytes(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "indexed", true: "committed_projection"}[projected], func(t *testing.T) {
			original := "package sample\n\nfunc target() int { return 1 }\n"
			s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": original})
			path := filepath.Join(root, "sample.go")
			node := s.engine.GetSymbol("sample.go::target")
			require.NotNil(t, node)
			indexedStart, indexedEnd := node.StartLine, node.EndLine
			ctx, release, err := acquireMutationPath(t.Context(), path)
			require.NoError(t, err)
			defer release()
			current := []byte(original)
			wantStart := indexedStart
			if projected {
				current = []byte("\n\n" + original)
				_, err = s.commitFileMutation(ctx, "write_file", "", "", "sample.go", path, current, 0o644)
				require.NoError(t, err)
				wantStart += 2
			}
			lockedBytes, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, current, lockedBytes)
			plans := []plannedBatchEdit{{op: "edit_symbol", absPath: path, file: "sample.go", node: node, edit: batchEditItem{SymbolID: node.ID}}}
			buffers := map[string]*batchFileBuffer{path: {original: lockedBytes, existsBefore: true}}
			parses := parser.ParseCount()
			failed, locateErr := s.locateBatchSymbolsUnderLock(ctx, plans, buffers)
			require.NoError(t, locateErr)
			require.Equal(t, -1, failed)
			require.EqualValues(t, 0, parser.ParseCount()-parses, "matching indexed/projection bytes already own their declaration extraction")
			require.NotSame(t, node, plans[0].node, "only the local plan receives current spans")
			require.Equal(t, wantStart, plans[0].node.StartLine)
			require.Equal(t, indexedStart, node.StartLine)
			require.Equal(t, indexedEnd, node.EndLine)
		})
	}
}
