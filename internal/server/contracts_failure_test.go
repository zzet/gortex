package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

type contractsFailureExtractor struct{ parser.Extractor }

func (e contractsFailureExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	if filepath.Base(path) == "failed.go" {
		return nil, errors.New("synthetic permanent parse failure")
	}
	return e.Extractor.Extract(path, src)
}

func TestContractsHTTPPreservesAnalysisStatusAndUnaffectedContracts(t *testing.T) {
	srv := mcpserver.NewMCPServer("fixture", "fixture", mcpserver.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("analyze"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(`{"by_repo":{"healthy":{"contracts":{"http":[{"id":"http::GET::/healthy","type":"http","role":"provider","repo_prefix":"healthy"}]}}},"contract_analysis":{"state":"building","fresh":false,"repositories":["changing"],"reason":"source mutation"}}`), nil
	})
	h := NewHandler(srv, graph.New(), "fixture", zap.NewNop())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/contracts", nil))
	require.Equal(t, 200, w.Code)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Contains(t, string(body["contracts"]), "/healthy")
	require.Contains(t, string(body["contract_analysis"]), "changing")
	require.Contains(t, string(body["contract_analysis"]), "source mutation")
}

func TestContractsHTTPPermanentParseFailureWithoutMutation(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"good.go", "failed.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package fixture\nfunc Handler() {}\n"), 0o644))
	}
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fixture.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	reg := parser.NewRegistry()
	reg.Register(contractsFailureExtractor{Extractor: languages.NewGoExtractor()})
	idx := indexer.New(g, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRepoPrefix("fixture")
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Len(t, indexer.LoadFileIndexFailures(g, "fixture"), 1)
	srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	registry := idx.ContractRegistry()
	require.NotNil(t, registry)
	registry.Add(contracts.Contract{ID: "http::GET::/healthy", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		RepoPrefix: "fixture", FilePath: "fixture/good.go", SymbolID: "fixture/good.go::Handler"})
	srv.SetContractRegistry(registry)
	h := NewHandler(srv.MCPServer(), g, "fixture", zap.NewNop())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/contracts", nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "/healthy")
}
