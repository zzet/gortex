package lsp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

const vueTSPluginName = "@vue/typescript-plugin"

// vueTSPluginLocation returns the directory tsserver should probe for the Vue
// plugin (it looks in <location>/node_modules), or "" when there is none. A
// plugin the project installed wins over the one bundled with a
// vue-language-server on PATH.
//
// shortcut: only the repo root's node_modules is probed, a plugin installed in
// one workspace package of a monorepo is missed; walk up from the file if that
// comes up.
func vueTSPluginLocation(root string) string {
	candidates := []string{root, filepath.Join(root, "node_modules", "@vue", "language-server")}
	if bin, err := lookPath("vue-language-server"); err == nil {
		if real, err := filepath.EvalSymlinks(bin); err == nil {
			// <package>/bin/vue-language-server.js
			candidates = append(candidates, filepath.Dir(filepath.Dir(real)))
		}
	}
	for _, dir := range candidates {
		if fileExists(filepath.Join(dir, "node_modules", "@vue", "typescript-plugin", "package.json")) {
			return dir
		}
	}
	return ""
}

// projectTSServer returns the project's own tsserver.js, or "" without one.
// The plugin runs inside tsserver and has to match the project's TypeScript; a
// global install is no substitute, and TypeScript 7 ships no tsserver at all.
func projectTSServer(root string) string {
	path := filepath.Join(root, "node_modules", "typescript", "lib", "tsserver.js")
	if !fileExists(path) {
		return ""
	}
	return path
}

// vueWorkspaceGate refuses a workspace the plugin cannot work in. Without the
// plugin the server accepts .vue files and never publishes anything, which
// reads as "no errors".
func vueWorkspaceGate(root string) error {
	if vueTSPluginLocation(root) == "" {
		return fmt.Errorf("%s not found: add it to the project or install vue-language-server", vueTSPluginName)
	}
	if projectTSServer(root) == "" {
		return fmt.Errorf("no tsserver in %s/node_modules/typescript: install the project's dependencies", root)
	}
	return nil
}

// vueInitOptions loads the plugin into typescript-language-server for root.
func vueInitOptions(root string) json.RawMessage {
	location := vueTSPluginLocation(root)
	if location == "" {
		return nil
	}
	opts, err := json.Marshal(map[string]any{
		"plugins": []map[string]any{{
			"name":      vueTSPluginName,
			"location":  location,
			"languages": []string{"vue"},
		}},
		"tsserver": map[string]string{"path": projectTSServer(root)},
	})
	if err != nil {
		return nil
	}
	return opts
}
