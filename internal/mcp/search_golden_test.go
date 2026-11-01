package mcp

import (
	"encoding/json"
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

var searchGoldenAbsolutePath = regexp.MustCompile(`("absolute_file_path"\s*:\s*)("(?:\\.|[^"\\])*")`)

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
			got = normalizeSearchGoldenRoots(got, roots)
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

func normalizeSearchGoldenRoots(got string, roots []string) string {
	// Rewrite only the path string token, retaining field order, whitespace,
	// exact numeric spelling and every unrelated source string byte.
	if json.Valid([]byte(got)) {
		return searchGoldenAbsolutePath.ReplaceAllStringFunc(got, func(match string) string {
			parts := searchGoldenAbsolutePath.FindStringSubmatch(match)
			var path string
			if json.Unmarshal([]byte(parts[2]), &path) != nil {
				return match
			}
			normalized := normalizeSearchGoldenPath(path, roots)
			if normalized == path {
				return match
			}
			encoded, err := json.Marshal(normalized)
			if err != nil {
				return match
			}
			return parts[1] + string(encoded)
		})
	}
	if strings.HasPrefix(got, "GCX1 ") {
		lines := strings.Split(got, "\n")
		column := -1
		for _, word := range strings.Fields(lines[0]) {
			if fields, ok := strings.CutPrefix(word, "fields="); ok {
				for i, name := range strings.Split(fields, ",") {
					if name == "path_abs" {
						column = i
					}
				}
			}
		}
		if column >= 0 {
			for i, line := range lines[1:] {
				if strings.HasPrefix(line, "#") {
					continue
				}
				fields := strings.Split(line, "\t")
				if column < len(fields) {
					fields[column] = normalizeSearchGoldenPath(fields[column], roots)
					lines[i+1] = strings.Join(fields, "\t")
				}
			}
		}
		return strings.Join(lines, "\n")
	}
	for _, root := range roots {
		got = strings.ReplaceAll(got, root, "$ROOT")
	}
	return got
}

func normalizeSearchGoldenPath(path string, roots []string) string {
	original := path
	path = strings.ReplaceAll(path, `\`, "/")
	for _, root := range roots {
		root = strings.TrimSuffix(strings.ReplaceAll(root, `\`, "/"), "/")
		if path == root {
			return "$ROOT"
		}
		if strings.HasPrefix(path, root+"/") {
			return "$ROOT" + strings.TrimPrefix(path, root)
		}
	}
	return original
}

func TestSearchGoldenRootNormalizationPreservesActualWindowsPaths(t *testing.T) {
	root := `D:\a\_temp\owned`
	for _, mode := range []string{"json", "gcx"} {
		t.Run(mode, func(t *testing.T) {
			var actual, want string
			if mode == "json" {
				value := map[string]any{"absolute_file_path": root + `\server\server.go`, "doc": "literal " + root, "unrelated": root + `-sibling\server.go`}
				data, err := json.Marshal(value)
				require.NoError(t, err)
				actual = string(data)
				value["absolute_file_path"] = "$ROOT/server/server.go"
				data, err = json.Marshal(value)
				require.NoError(t, err)
				want = string(data)
			} else {
				actual = "GCX1 tool=search_symbols fields=id,path_abs,sig\nserver::Handle\t" + root + `\server\server.go` + "\t" + `func literal("D:\code")` + "\n"
				want = "GCX1 tool=search_symbols fields=id,path_abs,sig\nserver::Handle\t$ROOT/server/server.go\t" + `func literal("D:\code")` + "\n"
			}
			require.Equal(t, want, normalizeSearchGoldenRoots(actual, []string{root}))
		})
	}
}
