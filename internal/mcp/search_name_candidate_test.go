package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

func TestNameOnlySearchKeepsLiteralSubstringAndPathScope(t *testing.T) {
	for _, native := range []bool{false, true} {
		label := "reader_fallback"
		if native {
			label = "native_backend"
		}
		t.Run(label, func(t *testing.T) {
			selectedProjectionRegistry(t)
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "selected.go"), []byte("package probe\n\nfunc AlphaNeed_leHandler() {}\nfunc AlphaNeedXleHandler() {}\nfunc HiddenBody() { _ = `AbsentLiteral` }\nfunc KeyHandler() {}\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "other.go"), []byte("package probe\n\nfunc OtherNeed_leHandler() {}\n"), 0o600))
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "names.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			idx := newProjectionTestIndexer(t, store)
			idx.SetRepoPrefix("selected")
			_, err = idx.Index(root)
			require.NoError(t, err)
			count, err := store.SymbolFTSCount()
			require.NoError(t, err)
			require.Positive(t, count)
			engine := query.NewEngine(store)
			if native {
				engine.SetSearchProvider(idx.Search)
			}
			server := NewServer(engine, store, idx, nil, zap.NewNop(), nil)
			for _, tc := range []struct {
				name, query, path string
				want              []string
			}{
				{"substring_case", "name:nEeD_lE", "selected.go", []string{"AlphaNeed_leHandler"}},
				{"unicode_fold", "name:kEy", "selected.go", []string{"KeyHandler"}},
				{"underscore_literal", "name:Need_le", "", []string{"AlphaNeed_leHandler", "OtherNeed_leHandler"}},
				{"percent_literal", "name:Need%le", "", nil},
				{"asterisk_literal", "name:Need*le", "", nil},
				{"missing_does_not_relax", "name:NoSuchName", "", nil},
				{"body_literal_is_not_name", "name:AbsentLiteral", "", nil},
				{"path_excludes_other_file", "name:OtherNeed_le", "selected.go", nil},
				{"nonempty_query_keeps_name_filter", "Alpha name:Need_le", "", []string{"AlphaNeed_leHandler"}},
				{"ordinary_query", "AlphaNeed_leHandler", "selected.go", []string{"AlphaNeed_leHandler"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := map[string]any{"query": tc.query, "repo": "selected", "query_class": "symbol", "expand": "off", "limit": 10000, "max_per_file": 0, "max_bytes": 0, "format": "json"}
					if tc.path != "" {
						args["path"] = tc.path
					}
					result := callTool(t, server, "search_symbols", args)
					require.False(t, result.IsError, resultText(result))
					body := extractTextResult(t, result)
					rows, ok := body["results"].([]any)
					require.True(t, ok)
					var names []string
					for _, raw := range rows {
						row, ok := raw.(map[string]any)
						require.True(t, ok)
						name, ok := row["name"].(string)
						require.True(t, ok)
						names = append(names, name)
					}
					require.ElementsMatch(t, tc.want, names)
					require.Equal(t, float64(len(names)), body["total"])
					require.Equal(t, false, body["truncated"])
					for _, key := range []string{"filters_relaxed", "content_matches", "decomposed", "_truncated_by_budget", "_truncated_by_limit"} {
						require.NotContains(t, body, key)
					}
				})
			}
		})
	}
}
