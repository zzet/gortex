package languages

import (
	"path"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Drupal hook detection. A Drupal module implements a hook by defining a
// function named `{module}_{hook_suffix}` (or by a `@Implements hook_X`
// docblock). The hook itself is a contract, not a declared symbol, so an
// implementation has nothing to point at. This pass flags hook
// implementations and wires each to a synthetic `hook_X` node via
// EdgeImplements, so `find_implementations hook_X` lists every module that
// implements it across `.module` / `.inc` files.

// drupalModuleExts are the Drupal PHP file extensions whose function names
// follow the `{module}_{hook}` convention.
var drupalModuleExts = map[string]bool{
	".module": true, ".install": true, ".inc": true,
	".theme": true, ".profile": true, ".engine": true,
}

// drupalImplementsRE extracts the hook name from an `Implements hook_X`
// docblock line (the authoritative signal).
var drupalImplementsRE = regexp.MustCompile(`(?i)Implements\s+(hook_\w+)`)

// drupalKnownHookSuffixes is the hook-name set (without the `hook_` prefix)
// the name-pattern detector recognises when there is no `@Implements`
// docblock. Curated to the common, distinctive Drupal core hooks so an ordinary
// `{module}_helper()` function is not mistaken for a hook. Variable-segment
// hook families (hook_form_FORM_ID_alter, hook_preprocess_HOOK, …) are matched
// separately by drupalHookPatterns.
var drupalKnownHookSuffixes = map[string]bool{
	// Node entity lifecycle + access.
	"node_insert": true, "node_update": true, "node_delete": true,
	"node_load": true, "node_view": true, "node_presave": true, "node_access": true,
	"node_predelete": true, "node_prepare_form": true, "node_access_records": true,
	"node_grants": true, "node_links_alter": true,
	// Generic entity lifecycle + access + view.
	"entity_insert": true, "entity_update": true, "entity_delete": true,
	"entity_presave": true, "entity_load": true, "entity_view": true,
	"entity_create": true, "entity_predelete": true, "entity_access": true,
	"entity_create_access": true, "entity_field_access": true,
	"entity_view_alter": true, "entity_base_field_info": true,
	"entity_bundle_field_info": true, "entity_type_alter": true,
	"entity_type_build": true, "entity_operation": true, "entity_operation_alter": true,
	"entity_extra_field_info": true,
	// User entity lifecycle + access.
	"user_login": true, "user_logout": true, "user_insert": true, "user_delete": true,
	"user_update": true, "user_presave": true, "user_load": true,
	"user_cancel": true, "user_format_name_alter": true,
	// Forms.
	"form_alter": true,
	// Menu, routing, links, local tasks.
	"menu": true, "menu_alter": true, "menu_links_discovered_alter": true,
	"menu_local_tasks_alter": true, "menu_local_actions_alter": true,
	"local_tasks_alter": true,
	// Theme, render, page attachments.
	"theme": true, "theme_registry_alter": true, "theme_suggestions_alter": true,
	"page_attachments": true, "page_attachments_alter": true,
	"page_top": true, "page_bottom": true, "element_info_alter": true,
	"library_info_alter": true, "library_info_build": true, "js_settings_alter": true,
	"js_alter": true, "css_alter": true,
	// Permissions / access / authorization.
	"permission": true, "permissions": true,
	// Module / install lifecycle.
	"init": true, "boot": true, "install": true, "uninstall": true,
	"schema": true, "schema_alter": true, "requirements": true,
	"modules_installed": true, "modules_uninstalled": true,
	"update_dependencies": true, "update_last_removed": true,
	"install_tasks": true, "install_tasks_alter": true,
	// Cron, queue, batch.
	"cron": true, "queue_info_alter": true,
	// Help, mail, messaging.
	"help": true, "mail": true, "mail_alter": true,
	// Tokens.
	"token_info": true, "token_info_alter": true, "tokens": true, "tokens_alter": true,
	// Views.
	"views_data": true, "views_data_alter": true, "views_query_alter": true,
	"views_pre_render": true, "views_post_render": true, "views_pre_execute": true,
	// Fields / widgets / formatters.
	"field_info_alter": true, "field_widget_info_alter": true,
	"field_formatter_info_alter": true, "field_widget_complete_form_alter": true,
	// Blocks.
	"block_access": true, "block_alter": true, "block_build_alter": true,
	"block_view_alter": true,
	// Comments.
	"comment_insert": true, "comment_update": true, "comment_delete": true,
	"comment_presave": true, "comment_publish": true, "comment_unpublish": true,
	// Taxonomy / file / search.
	"taxonomy_term_insert": true, "taxonomy_term_update": true, "taxonomy_term_delete": true,
	"file_download": true, "file_url_alter": true,
	"search_info": true, "ranking": true,
	// Language / locale.
	"language_switch_links_alter": true, "language_types_info": true,
	// Cache / config / data.
	"cache_flush": true, "rebuild": true,
	"config_schema_info_alter": true, "data_type_info_alter": true,
	// Toolbar / contextual / batch.
	"toolbar": true, "toolbar_alter": true, "contextual_links_view_alter": true,
	"batch_alter": true,
	// Mail / cron / system.
	"system_info_alter": true, "flush_caches": true,
}

// drupalHookPattern matches a Drupal hook family whose name carries a variable
// segment (a form id, theme hook, or entity type), which an exact-suffix lookup
// cannot capture. The match is anchored to keep it precise: a function name's
// suffix must start with prefix, end with suffix (when set), and have a
// non-empty variable middle — `{module}_form_user_login_form_alter` matches
// `hook_form_FORM_ID_alter`, but `{module}_form_alter` (the bare hook) is left
// to the exact set. canonical is the placeholder hook name reported, so every
// implementation of a family wires to one synthetic node.
type drupalHookPattern struct {
	prefix    string // required leading segment, e.g. "form_"
	suffix    string // required trailing segment (may be empty), e.g. "_alter"
	canonical string // reported hook name, e.g. "hook_form_FORM_ID_alter"
}

// drupalHookPatterns are the variable-segment hook families, checked only when
// the exact-suffix lookup misses.
var drupalHookPatterns = []drupalHookPattern{
	// hook_form_FORM_ID_alter / hook_form_BASE_FORM_ID_alter.
	{prefix: "form_", suffix: "_alter", canonical: "hook_form_FORM_ID_alter"},
	// hook_preprocess_HOOK (template preprocess for a specific theme hook).
	{prefix: "preprocess_", canonical: "hook_preprocess_HOOK"},
	// hook_process_HOOK.
	{prefix: "process_", canonical: "hook_process_HOOK"},
	// hook_theme_suggestions_HOOK / hook_theme_suggestions_HOOK_alter.
	{prefix: "theme_suggestions_", suffix: "_alter", canonical: "hook_theme_suggestions_HOOK_alter"},
	{prefix: "theme_suggestions_", canonical: "hook_theme_suggestions_HOOK"},
	// hook_field_widget_WIDGET_TYPE_form_alter / hook_field_widget_complete_WIDGET_TYPE_form_alter.
	{prefix: "field_widget_", suffix: "_form_alter", canonical: "hook_field_widget_WIDGET_TYPE_form_alter"},
}

// captureDrupalHooks flags hook-implementation functions and wires them to a
// synthetic hook node. Runs at the tail of Extract.
func captureDrupalHooks(result *parser.ExtractionResult, filePath string) {
	if result == nil {
		return
	}
	ext := strings.ToLower(path.Ext(filePath))
	moduleFile := drupalModuleExts[ext]
	module := ""
	if moduleFile {
		base := path.Base(filePath)
		module = strings.TrimSuffix(base, path.Ext(base))
	}

	hookNodes := map[string]bool{}
	var add []*graph.Node
	for _, n := range result.Nodes {
		if n == nil || n.Kind != graph.KindFunction {
			continue
		}
		hook := drupalHookFor(n, module, moduleFile)
		if hook == "" {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["drupal_hook"] = hook
		hookID := "drupal::hook::" + hook
		if !hookNodes[hook] {
			hookNodes[hook] = true
			add = append(add, &graph.Node{
				ID: hookID, Kind: graph.KindInterface, Name: hook,
				FilePath: filePath, StartLine: n.StartLine, EndLine: n.EndLine,
				Meta: map[string]any{"drupal_hook_def": true, "type_flavor": "hook"},
			})
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: n.ID, To: hookID, Kind: graph.EdgeImplements,
			FilePath: filePath, Line: n.StartLine,
			Meta: map[string]any{"drupal_hook": hook},
		})
	}
	result.Nodes = append(result.Nodes, add...)
}

// drupalHookFor returns the hook a function implements: from an `@Implements`
// docblock first, then the `{module}_{known_hook}` name pattern.
func drupalHookFor(n *graph.Node, module string, moduleFile bool) string {
	if n.Meta != nil {
		if doc, _ := n.Meta["doc"].(string); doc != "" {
			if m := drupalImplementsRE.FindStringSubmatch(doc); m != nil {
				return strings.ToLower(m[1])
			}
		}
	}
	if moduleFile && module != "" && strings.HasPrefix(n.Name, module+"_") {
		suffix := n.Name[len(module)+1:]
		if drupalKnownHookSuffixes[suffix] {
			return "hook_" + suffix
		}
		if hook := drupalHookFromPattern(suffix); hook != "" {
			return hook
		}
	}
	return ""
}

// drupalHookFromPattern resolves a variable-segment hook family (a function
// suffix like `form_user_login_form_alter` or `preprocess_node`) to its
// canonical placeholder hook name, or "" when no family matches. Each match
// requires a non-empty variable middle so a bare hook (already in the exact
// set) does not double-match here.
func drupalHookFromPattern(suffix string) string {
	for _, p := range drupalHookPatterns {
		if !strings.HasPrefix(suffix, p.prefix) {
			continue
		}
		mid := suffix[len(p.prefix):]
		if p.suffix != "" {
			if !strings.HasSuffix(mid, p.suffix) {
				continue
			}
			mid = mid[:len(mid)-len(p.suffix)]
		}
		if mid == "" {
			continue
		}
		return p.canonical
	}
	return ""
}
