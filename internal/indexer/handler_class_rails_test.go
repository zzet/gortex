package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// Class-scoped handler selection applies to PHP contracts only; Rails keeps
// its name-based binding, including compact `Admin::UsersController` classes
// that have no standalone type node.
func TestResolveProviderHandlersRailsKeepsNameBinding(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		controllers  map[string]string
		want         string
	}{
		{"compact_namespace", "admin/users#index",
			map[string]string{"app/controllers/admin/users_controller.rb": "class Admin::UsersController < ApplicationController\n  def index\n  end\nend\n"},
			"a/app/controllers/admin/users_controller.rb::Admin::UsersController.index"},
		{"nested_namespace", "admin/users#index",
			map[string]string{"app/controllers/admin/users_controller.rb": "module Admin\n  class UsersController < ApplicationController\n    def index\n    end\n  end\nend\n"},
			"a/app/controllers/admin/users_controller.rb::UsersController.index"},
		{"inherited_action", "users#index",
			map[string]string{
				"app/controllers/users_controller.rb": "class UsersController < BaseController\nend\n",
				"app/controllers/base_controller.rb":  "class BaseController\n  def index\n  end\nend\n",
			},
			"a/app/controllers/base_controller.rb::BaseController.index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			core := graph.New()
			route := "Rails.application.routes.draw do\n  get '/users', to: '" + tc.target + "'\nend\n"
			files := map[string]string{"config/routes.rb": route}
			for file, source := range tc.controllers {
				files[file] = source
			}
			for file, source := range files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(source), 0o600))
				parsed, err := languages.NewRubyExtractor().Extract("a/"+file, []byte(source))
				require.NoError(t, err)
				for _, node := range parsed.Nodes {
					node.RepoPrefix = "a"
				}
				core.AddBatch(parsed.Nodes, parsed.Edges)
			}
			idx := &Indexer{graph: core, rootPath: root, repoPrefix: "a", logger: zap.NewNop()}
			reg := contracts.NewRegistry()
			for _, c := range (&contracts.HTTPExtractor{}).Extract("a/config/routes.rb", []byte(route), nil, nil) {
				c.RepoPrefix = "a"
				reg.Add(c)
			}
			idx.resolveProviderHandlers(reg)
			var got []string
			for _, c := range reg.All() {
				got = append(got, c.SymbolID)
			}
			require.Equal(t, []string{tc.want}, got)
		})
	}
}
