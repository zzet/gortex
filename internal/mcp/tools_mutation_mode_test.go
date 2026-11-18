package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

const executableScriptSrc = "#!/usr/bin/env python3\n\ndef keep():\n    return 1\n\n\ndef drop():\n    return 2\n"

// executableScriptServer indexes one executable Python script and returns the
// server, the script's absolute path, and the ID of its `drop` function.
func executableScriptServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tool.py")
	require.NoError(t, os.WriteFile(path, []byte(executableScriptSrc), 0o755))
	require.NoError(t, os.Chmod(path, 0o755))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	sg := srv.engineFor(context.Background()).GetFileSymbols("tool.py")
	require.NotNil(t, sg, "tool.py must be indexed")
	for _, n := range sg.Nodes {
		if n != nil && n.Name == "drop" {
			return srv, path, n.ID
		}
	}
	t.Fatal("symbol drop not found in tool.py")
	return nil, "", ""
}

// The atomic commit applies the mode it is given to the replacement file, so
// every caller must pass the file's existing mode. scaffold and
// safe_delete_symbol must leave an executable script executable.
func TestMutationsPreserveExecutableMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes do not apply on Windows")
	}
	for _, tc := range []struct {
		name string
		call func(*Server, string) (*mcplib.CallToolResult, error)
	}{
		{"safe_delete_symbol", func(srv *Server, id string) (*mcplib.CallToolResult, error) {
			return srv.handleSafeDeleteSymbol(context.Background(), mcplib.CallToolRequest{Params: mcplib.CallToolParams{
				Name: "safe_delete_symbol", Arguments: map[string]any{"id": id, "dry_run": false, "force": true},
			}})
		}},
		{"scaffold", func(srv *Server, id string) (*mcplib.CallToolResult, error) {
			return srv.handleScaffold(context.Background(), mcplib.CallToolRequest{Params: mcplib.CallToolParams{
				Name: "scaffold", Arguments: map[string]any{"id": id, "new_name": "extra", "dry_run": false},
			}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, path, id := executableScriptServer(t)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			res, err := tc.call(srv, id)
			require.NoError(t, err)
			require.False(t, res.IsError, "%+v", res.Content)

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotEqual(t, string(before), string(after), "%s must have written the file", tc.name)
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o755), info.Mode().Perm(),
				"%s must keep the script's executable mode", tc.name)
		})
	}
}
