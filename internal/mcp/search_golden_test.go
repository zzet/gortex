package mcp

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

var updateSearchGolden = flag.Bool("update-search-golden", false, "rewrite the search_symbols / search_text goldens")

// searchGoldenCursor matches a page cursor, which names a sequence minted at
// random on every call.
var searchGoldenCursor = regexp.MustCompile(`"next_cursor":"[A-Za-z0-9_-]+"`)

// searchGoldenFixture is a small module whose names share prefixes, so exact,
// prefix and concept queries rank different candidate sets, and whose bodies
// repeat literals across files, so text queries hit several paths.
var searchGoldenFixture = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"config/config.go": `package config

// Config holds the server settings.
type Config struct {
	Port int
	Host string
}

// LoadConfig reads the settings file at path.
func LoadConfig(path string) (*Config, error) {
	return &Config{Port: 8080, Host: "localhost"}, nil
}

// Validate reports whether the settings are usable.
func (c *Config) Validate() error { return nil }
`,
	"server/server.go": `package server

import "example.com/app/config"

// Server answers requests.
type Server struct {
	cfg *config.Config
}

// NewServer builds a server from cfg.
func NewServer(cfg *config.Config) *Server { return &Server{cfg: cfg} }

// HandleRequest serves one request.
func (s *Server) HandleRequest(path string) string {
	if err := s.cfg.Validate(); err != nil {
		return "invalid config"
	}
	return "handled " + path
}

// HandleHealth answers the health probe.
func (s *Server) HandleHealth() string { return "handled health" }
`,
	"main.go": `package main

import (
	"example.com/app/config"
	"example.com/app/server"
)

func main() {
	cfg, _ := config.LoadConfig("app.yaml")
	srv := server.NewServer(cfg)
	_ = srv.HandleRequest("/")
	_ = srv.HandleHealth()
}
`,
}

// TestSearchOutputsMatchGoldens pins the exact bytes search_symbols and
// search_text answer with on a quiet checkout, in every output mode. Work that
// lets search read through just-edited files must leave this path untouched:
// with nothing pending, the answer is byte-for-byte what it was before. The
// only rewrites are the checkout's temporary root, which reads as $ROOT, and
// the random page cursor, which reads as $CURSOR.
//
// Regenerate with -update-search-golden, on a tree whose search code is the
// reference.
func TestSearchOutputsMatchGoldens(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range searchGoldenFixture {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	// The store and engine are wired as the daemon wires them, so symbol
	// queries rank through the store's full-text index, not the substring
	// fallback a bare engine takes.
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "search.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	_, err = idx.Index(dir)
	require.NoError(t, err)
	requireSymbolFTS(t, store)
	eng := query.NewEngine(store)
	eng.SetSearchProvider(idx.Search)
	srv := NewServer(eng, store, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	// The resolved spelling goes first: on macOS it is the temporary root
	// with a /private prefix.
	roots := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		roots = []string{resolved, dir}
	}

	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"symbols_exact", "search_symbols", map[string]any{"query": "HandleRequest"}},
		{"symbols_prefix", "search_symbols", map[string]any{"query": "Handle"}},
		{"symbols_concept", "search_symbols", map[string]any{"query": "load settings config"}},
		{"symbols_limit", "search_symbols", map[string]any{"query": "Handle", "limit": 1}},
		{"symbols_kind", "search_symbols", map[string]any{"query": "Config", "kind": "type"}},
		{"symbols_missing", "search_symbols", map[string]any{"query": "NoSuchSymbolAnywhere"}},
		{"symbols_json", "search_symbols", map[string]any{"query": "Handle", "format": "json"}},
		{"symbols_compact", "search_symbols", map[string]any{"query": "Handle", "compact": true}},
		{"symbols_toon", "search_symbols", map[string]any{"query": "Handle", "format": "toon"}},
		{"symbols_gcx", "search_symbols", map[string]any{"query": "Handle", "format": "gcx"}},
		{"text_literal", "search_text", map[string]any{"query": "handled"}},
		{"text_limit", "search_text", map[string]any{"query": "config", "limit": 2}},
		{"text_path", "search_text", map[string]any{"query": "config", "path": "server"}},
		{"text_regexp", "search_text", map[string]any{"query": `Handle[A-Z][a-z]+\(`, "regexp": true}},
		{"text_missing", "search_text", map[string]any{"query": "no such literal anywhere"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, srv, tc.tool, tc.args)
			require.NotEmpty(t, res.Content)
			got := res.Content[0].(mcplib.TextContent).Text
			for _, root := range roots {
				got = strings.ReplaceAll(got, root, "$ROOT")
			}
			got = searchGoldenCursor.ReplaceAllLiteralString(got, `"next_cursor":"$CURSOR"`)
			golden := filepath.Join("testdata", "search_golden", tc.name+".txt")
			if *updateSearchGolden {
				require.NoError(t, os.MkdirAll(filepath.Dir(golden), 0o755))
				require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			require.Equal(t, string(want), got)
		})
	}
}
