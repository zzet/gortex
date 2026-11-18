package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Cross-file route-prefix joining for HTTP contract IDs.
//
// A sub-router is frequently declared in one file and *mounted* under a
// path prefix in another. The route the sub-router declares should be
// recorded with the joined path so a provider and a consumer agree on
// the same canonical contract ID:
//
//	# users.py
//	router = APIRouter(prefix="/users")
//	@router.get("/{id}")            # declared path: /{id}
//	def get_user(id): ...
//
//	# main.py
//	app.include_router(router, prefix="/api")
//
// Without this pass the provider emits http::GET::/{p1}; the consumer
// calling /api/users/42 emits http::GET::/api/users/{p1}; the two never
// pair. JoinRouterPrefixes rewrites the provider to
// http::GET::/api/users/{p1} so the matcher sees the same ID on both
// sides.
//
// Coverage:
//   - FastAPI (full): APIRouter(prefix=...) self-prefix + cross-file
//     include_router(var, prefix=...) mounts, including nested includes
//     (a router included into an already-included router → prefixes
//     chain) and a missing prefix (no-op join).
//   - Express (partial): app.use('/api', router) mounts joined onto
//     routes declared on a receiver named app/router. Limited by the
//     existing express regex, which only recognises those two receiver
//     names.
//   - NestJS (partial): @Controller('cats') class prefix joined onto
//     the class's @Get('x') routes (single-file). Cross-module
//     RouterModule.register prefixes are out of scope.
//
// The pass is idempotent: a contract already stamped joined is skipped,
// so a re-run (incremental reindex) never double-joins.

// routePrefixJoinedMeta marks a contract whose path/ID this pass has
// already rewritten, so a second run is a no-op.
const routePrefixJoinedMeta = "route_prefix_joined"

// Frameworks whose routes participate in prefix joining. The HTTP
// extractor stamps Meta["framework"] with these values.
var routePrefixFrameworks = map[string]bool{
	"fastapi/flask": true, // FastAPI APIRouter / include_router
	"flask":         true, // Flask Blueprint url_prefix
	"express":       true, // Express app.use mounts
	"nestjs":        true, // NestJS @Controller class prefix
	"gin/echo/chi":  true, // gin RouterGroup .Group chains
	"spring":        true, // Spring class-level @RequestMapping
	"rails":         true, // Rails namespace blocks
	"laravel":       true, // Laravel Route::prefix(...)->group
	"axum":          true, // axum Router .nest mounts
	"fiber":         true, // gin/echo uppercase verbs label as fiber; same .Group model
	"vapor":         true, // Vapor .grouped(...) route groups
	"actix":         true, // Actix web::scope(...) cross-file config mounts
}

// vaporGroupedRE matches a Vapor route-group binding
// `let api = app.grouped("api")` (and chained `let v1 = api.grouped("v1")`).
// Group 1 is the new group var, group 2 the parent receiver, group 3 the
// (one or more) path-segment string literals.
var vaporGroupedRE = regexp.MustCompile(`\b(?:let|var)\s+(\w+)\s*=\s*([\w.]+)\.grouped\(\s*("[^"]*"(?:\s*,\s*"[^"]*")*)`)

// vaporClosureGroupRE matches the closure-block route-group form
// `app.group("api") {` / `routes.group("v1") { g in`. Routes registered
// inside the brace block inherit the group prefix. Group 1 is the (one or
// more) path-segment string literals.
var vaporClosureGroupRE = regexp.MustCompile(`\.group\(\s*("[^"]*"(?:\s*,\s*"[^"]*")*)\s*\)\s*\{`)

// vaporRouteReceiverRE recovers the receiver variable a Vapor route is declared
// on — `api` in `api.get("users", use: list)`.
var vaporRouteReceiverRE = regexp.MustCompile(`\b(\w+)\.(?:get|post|put|delete|patch)\(`)

// vaporStringSegRE matches a single quoted path segment inside a grouped(...)
// argument list.
var vaporStringSegRE = regexp.MustCompile(`"([^"]*)"`)

// vaporGroupSegments joins the string-literal path segments of a
// `.grouped("a", "b")` call into a single prefix.
func vaporGroupSegments(args string) string {
	var segs []string
	for _, m := range vaporStringSegRE.FindAllStringSubmatch(args, -1) {
		if m[1] != "" {
			segs = append(segs, m[1])
		}
	}
	return cleanPrefix(strings.Join(segs, "/"))
}

// --- FastAPI / Python -------------------------------------------------------

// pyRouterDefRE matches a router variable bound to an APIRouter that
// carries its own prefix, e.g. `router = APIRouter(prefix="/users")` or
// `users_router = APIRouter(prefix='/users', tags=[...])`. Group 1 is
// the variable name, group 2 the prefix.
var pyRouterDefRE = regexp.MustCompile(`(\w+)\s*=\s*APIRouter\(([^)]*)\)`)

// pyIncludeRouterRE matches a mount site, e.g.
// `app.include_router(router, prefix="/api")` or
// `parent.include_router(users.router, prefix='/api')`. Group 1 is the
// mounted router reference (possibly attribute-qualified), group 2 the
// argument list (scanned for prefix=).
var pyIncludeRouterRE = regexp.MustCompile(`\binclude_router\(\s*([\w.]+)\s*((?:,[^)]*)?)\)`)

// prefixKwargRE pulls prefix="..." out of a keyword-argument list.
var prefixKwargRE = regexp.MustCompile(`prefix\s*=\s*["']([^"']*)["']`)

// urlPrefixKwargRE pulls Flask's url_prefix="..." out of a Blueprint
// definition or register_blueprint call.
var urlPrefixKwargRE = regexp.MustCompile(`url_prefix\s*=\s*["']([^"']*)["']`)

// pyBlueprintDefRE matches a Flask Blueprint bound to a variable, e.g.
// `bp = Blueprint('api', __name__, url_prefix='/api')`. Group 1 is the
// variable, group 2 the argument list (scanned for url_prefix=).
var pyBlueprintDefRE = regexp.MustCompile(`(\w+)\s*=\s*Blueprint\(([^)]*)\)`)

// pyRegisterBlueprintRE matches a mount site, e.g.
// `app.register_blueprint(bp, url_prefix='/admin')`. Group 1 is the
// mounted blueprint reference, group 2 the argument list.
var pyRegisterBlueprintRE = regexp.MustCompile(`register_blueprint\(\s*([\w.]+)\s*((?:,[^)]*)?)\)`)

// flaskRouteReceiverRE recovers the blueprint/app variable from a Flask
// route line, e.g. `@bp.route("/users")` → "bp".
var flaskRouteReceiverRE = regexp.MustCompile(`@(\w+)\.route\(`)

// pyRouteReceiverRE recovers the decorator receiver variable from a
// FastAPI route line, e.g. `@router.get("/{id}")` → "router". Anchored
// to the start-of-line decorator form the Python provider pattern uses.
var pyRouteReceiverRE = regexp.MustCompile(`@(\w+)\.(?:get|post|put|delete|patch|head|options)\(`)

// --- Express / TS-JS --------------------------------------------------------

// jsUseMountRE matches `app.use('/api', router)` /
// `router.use("/v1", sub)`. Group 1 is the mount prefix, group 2 the
// mounted router variable. A `.use('/api', fn1, fn2, router)` form
// (middleware chain) is not handled — only the two-argument shape.
var jsUseMountRE = regexp.MustCompile(`\b(\w+)\.use\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\s*,\s*(\w+)\s*\)`)

// jsRouteReceiverRE recovers the receiver from an express route line,
// e.g. `router.get('/users', h)` → "router". Mirrors the express
// provider pattern's (?:app|router) anchor.
var jsRouteReceiverRE = regexp.MustCompile(`\b(app|router)\.(?:get|post|put|delete|patch|head|options|all)\(`)

// --- NestJS -----------------------------------------------------------------

// nestControllerRE matches a class-level `@Controller('cats')` /
// `@Controller("/cats")` decorator. Group 1 is the class prefix. An
// argument-less `@Controller()` carries no prefix (no-op).
var nestControllerRE = regexp.MustCompile(`@Controller\(\s*["'` + "`" + `]([^"'` + "`" + `]*)["'` + "`" + `]\s*\)`)

// --- gin / Go --------------------------------------------------------------

// ginGroupRE matches a gin RouterGroup binding, e.g.
// `v1 := r.Group("/api/v1")`. Group 1 is the child var, group 2 the parent
// router, group 3 the group prefix.
var ginGroupRE = regexp.MustCompile(`(\w+)\s*:?=\s*(\w+)\.Group\(\s*"([^"]+)"`)

// ginRouteReceiverRE recovers the router-group variable a gin/echo/chi/fiber
// route is registered on, e.g. `v1.GET("/users", h)` → "v1". The verb is
// matched case-insensitively to cover gin/echo's uppercase `.GET` and
// chi's title-case `.Get`.
var ginRouteReceiverRE = regexp.MustCompile(`(\w+)\.(?i:get|post|put|delete|patch|head|options|any|handle)\(`)

// --- axum / Rust -----------------------------------------------------------

// axumNestRE matches `.nest("/api", api_router)`. Group 1 is the mount
// prefix, group 2 the nested router variable.
var axumNestRE = regexp.MustCompile(`\.nest\(\s*"([^"]+)"\s*,\s*(\w+)\s*\)`)

// axumLetBindingRE recovers the router variable a route chain is bound to,
// e.g. `let api = Router::new().route("/users", get(list));` → "api".
var axumLetBindingRE = regexp.MustCompile(`let\s+(\w+)\s*=`)

// --- Actix / Rust ----------------------------------------------------------

// actixScopeConfigRE matches a cross-file Actix scope mount,
// `web::scope("/api").configure(api_routes)` /
// `web::scope("/api").service(users::routes)`. Group 1 is the scope prefix,
// group 2 the config-function reference the scope mounts (possibly
// path-qualified). The config function — defined in another file — registers
// the routes that inherit the scope prefix.
var actixScopeConfigRE = regexp.MustCompile(`web::scope\(\s*"([^"]+)"\s*\)\s*\.(?:configure|service)\(\s*([\w:]+)\s*\)`)

// actixFnDefRE matches a Rust function definition `fn api_routes(` / `pub fn
// api_routes(`. Group 1 is the function name. Used to recover the enclosing
// config function a route is declared in.
var actixFnDefRE = regexp.MustCompile(`\bfn\s+(\w+)\s*\(`)

// --- Spring / Rails / Laravel block-scoped prefixes ------------------------

// springClassMappingRE matches a class-level @RequestMapping("/api").
var springClassMappingRE = regexp.MustCompile(`@RequestMapping\(\s*(?:value\s*=\s*)?["']([^"']*)["']`)

// railsNamespaceRE matches a Rails `namespace :admin do` block opener.
var railsNamespaceRE = regexp.MustCompile(`\bnamespace\s+:(\w+)\s+do\b`)

// laravelPrefixRE matches a Laravel `Route::prefix('admin')` clause.
var laravelPrefixRE = regexp.MustCompile(`Route::prefix\(\s*["']([^"']+)["']\s*\)`)

// JoinRouterPrefixes rewrites HTTP provider/consumer contract paths and
// IDs to include the prefix at their router's mount site.
//
// scanFiles is the universe of source files to scan for router
// definitions and mount sites (APIRouter(prefix=...), include_router,
// app.use). This must include the *mount* files, which frequently carry
// no route contracts of their own (a FastAPI main.py with only
// include_router calls) — so the file set cannot be derived from the
// contract registry alone. The route contracts being rewritten still
// come from reg.
//
// srcFor returns the raw source bytes for a file path (as stored on the
// contract / in scanFiles — i.e. repo-prefixed when the indexer uses a
// repo prefix); returning nil for a path skips files that can't be read.
// The pass mutates the registry in place via ReplaceByID.
func JoinRouterPrefixes(reg *Registry, scanFiles []string, srcFor func(filePath string) []byte) {
	if reg == nil || srcFor == nil {
		return
	}

	paths := make(map[string]bool, len(scanFiles))
	for _, path := range scanFiles {
		paths[path] = true
	}
	for _, c := range reg.All() {
		fw, _ := c.Meta["framework"].(string)
		if c.Type == ContractHTTP && routePrefixFrameworks[fw] {
			paths[c.FilePath] = true
		}
	}
	facts := make(map[string]RouterFacts, len(paths))
	for path := range paths {
		facts[path] = RouterFileFacts(path, srcFor(path))
	}
	JoinRouterPrefixFacts(reg, facts)
}

// RouterFacts contains detached, serializable router inputs for one source file.
// Callers may retain these values between jobs. Treat their maps and slices as
// immutable; JoinRouterPrefixFacts only reads them. No source bytes are retained.
type RouterFacts struct {
	SelfPrefix       map[string]string
	MountPrefix      map[string]string
	MountParent      map[string]string
	ModulePrefix     map[string]string
	ControllerModule map[string]string
	ControllerClass  string
	LineCount        int
	Lines            []RouterLineFacts
}

// RouterLineFacts records source-derived route context at a one-based line.
// Entries are sparse change points, sorted by their one-based starting Line.
type RouterLineFacts struct {
	Line          int
	Receivers     map[string]string
	NestPrefix    string
	SpringPrefix  string
	RailsPrefix   string
	LaravelPrefix string
	VaporPrefix   string
}

// RouterFileFacts derives pure router facts even when the file has no contracts.
// Invoke it during extraction for every supported source file, including mount
// and module files, rather than waiting for an eligible HTTP contract to appear.
func RouterFileFacts(filePath string, src []byte) RouterFacts {
	f := RouterFacts{
		SelfPrefix: map[string]string{}, MountPrefix: map[string]string{}, MountParent: map[string]string{},
		ModulePrefix: map[string]string{}, ControllerModule: map[string]string{},
	}
	if src == nil {
		return f
	}
	text := string(src)
	// FastAPI: router = APIRouter(prefix="/users")
	for _, m := range pyRouterDefRE.FindAllStringSubmatch(text, -1) {
		varName := m[1]
		if pm := prefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
			f.SelfPrefix[varName] = cleanPrefix(pm[1])
		} else {
			// APIRouter() with no prefix still registers the var so
			// a later mount join knows it exists (no-op self-prefix).
			if _, ok := f.SelfPrefix[varName]; !ok {
				f.SelfPrefix[varName] = ""
			}
		}
	}

	// FastAPI: app.include_router(router, prefix="/api")
	for _, m := range pyIncludeRouterRE.FindAllStringSubmatch(text, -1) {
		child := lastAttr(m[1])
		prefix := ""
		if pm := prefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
			prefix = cleanPrefix(pm[1])
		}
		// A router can be mounted once; if seen twice keep the
		// first (deterministic).
		if _, ok := f.MountPrefix[child]; !ok {
			f.MountPrefix[child] = prefix
		}
		// Record the mount receiver as the parent so nested
		// includes chain (parent.include_router(child, ...)).
		parent := receiverOfInclude(text, m[0])
		if parent != "" && parent != child {
			f.MountParent[child] = parent
		}
	}

	// Flask: bp = Blueprint('api', __name__, url_prefix='/api')
	for _, m := range pyBlueprintDefRE.FindAllStringSubmatch(text, -1) {
		varName := m[1]
		if pm := urlPrefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
			f.SelfPrefix[varName] = cleanPrefix(pm[1])
		} else if _, ok := f.SelfPrefix[varName]; !ok {
			f.SelfPrefix[varName] = ""
		}
	}

	// Flask: app.register_blueprint(bp, url_prefix='/admin')
	for _, m := range pyRegisterBlueprintRE.FindAllStringSubmatch(text, -1) {
		child := lastAttr(m[1])
		prefix := ""
		if pm := urlPrefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
			prefix = cleanPrefix(pm[1])
		}
		if _, ok := f.MountPrefix[child]; !ok {
			f.MountPrefix[child] = prefix
		}
	}

	// gin: v1 := r.Group("/api/v1"); admin := v1.Group("/admin")
	for _, m := range ginGroupRE.FindAllStringSubmatch(text, -1) {
		child, parent, prefix := m[1], m[2], cleanPrefix(m[3])
		f.SelfPrefix[child] = prefix
		if parent != "" && parent != child {
			f.MountParent[child] = parent
		}
	}

	// axum: app.nest("/api", api_router)
	for _, m := range axumNestRE.FindAllStringSubmatch(text, -1) {
		prefix, child := cleanPrefix(m[1]), m[2]
		if _, ok := f.MountPrefix[child]; !ok {
			f.MountPrefix[child] = prefix
		}
	}

	// Actix: web::scope("/api").configure(api_routes) — the mounted
	// config function (in this or another file) registers the routes
	// that inherit the scope prefix.
	for _, m := range actixScopeConfigRE.FindAllStringSubmatch(text, -1) {
		prefix, child := cleanPrefix(m[1]), lastPathSeg(m[2])
		if _, ok := f.MountPrefix[child]; !ok {
			f.MountPrefix[child] = prefix
		}
	}

	// Express: app.use('/api', router)
	for _, m := range jsUseMountRE.FindAllStringSubmatch(text, -1) {
		parent, prefix, child := m[1], cleanPrefix(m[2]), m[3]
		if _, ok := f.MountPrefix[child]; !ok {
			f.MountPrefix[child] = prefix
		}
		if parent != "" && parent != child {
			f.MountParent[child] = parent
		}
	}

	// Vapor: let api = app.grouped("api"); let v1 = api.grouped("v1")
	for _, m := range vaporGroupedRE.FindAllStringSubmatch(text, -1) {
		child, parent, prefix := m[1], lastAttr(m[2]), vaporGroupSegments(m[3])
		f.SelfPrefix[child] = prefix
		if parent != "" && parent != child {
			f.MountParent[child] = parent
		}
	}

	f.ModulePrefix, f.ControllerModule = nestFileModuleFacts(text)
	if idx := strings.Index(text, "@Controller"); idx >= 0 {
		f.ControllerClass = strings.Clone(firstSubmatch(nestControllerClassRE, text[idx:]))
	}
	f.SelfPrefix = detachRouterMap(f.SelfPrefix)
	f.MountPrefix = detachRouterMap(f.MountPrefix)
	f.MountParent = detachRouterMap(f.MountParent)
	f.LineCount = strings.Count(text, "\n") + 1
	f.Lines = routerLineFacts(text)
	return f
}

// JoinRouterPrefixFacts rewrites routes using a complete file-facts snapshot.
// Include contract-free mount/module files. Missing facts act like unreadable
// files in JoinRouterPrefixes; this function never reads or reparses sources.
func JoinRouterPrefixFacts(reg *Registry, facts map[string]RouterFacts) {
	if reg == nil {
		return
	}

	// Global maps merged across every contract file. Router var names
	// commonly collide ("router"), so cross-file resolution is
	// best-effort within a workspace: when exactly one file declares a
	// self-prefix or mount for a var, use it. This favours the common
	// monorepo / single-service layout the feature targets.
	globalSelf := map[string]string{}   // var -> self prefix (FastAPI)
	globalMount := map[string]string{}  // var -> mount prefix
	globalParent := map[string]string{} // var -> parent router var
	selfConflict := map[string]bool{}   // var seen with >1 distinct self prefix
	mountConflict := map[string]bool{}  // var seen with >1 distinct mount prefix

	// First pass: parse every scan file plus every route-contract file
	// (union) and merge router facts. Mount files (a FastAPI main.py
	// that only calls include_router) carry no route contracts, so they
	// must be reached via scanFiles; route-contract files supply the
	// APIRouter(prefix=...) self-prefixes.
	paths := make([]string, 0, len(facts))
	for path := range facts {
		paths = append(paths, path)
	}
	sort.Strings(paths) // deterministic precedence for ambiguous parent/module names
	for _, path := range paths {
		f := facts[path]
		for v, p := range f.SelfPrefix {
			if existing, ok := globalSelf[v]; ok {
				if existing != p && existing != "" && p != "" {
					selfConflict[v] = true
				}
				if existing == "" && p != "" {
					globalSelf[v] = p
				}
			} else {
				globalSelf[v] = p
			}
		}
		for v, p := range f.MountPrefix {
			if existing, ok := globalMount[v]; ok {
				if existing != p {
					mountConflict[v] = true
				}
			} else {
				globalMount[v] = p
			}
		}
		for child, parent := range f.MountParent {
			if _, ok := globalParent[child]; !ok {
				globalParent[child] = parent
			}
		}
	}

	nestModulePrefix := joinNestModuleFacts(paths, facts)

	// chainPrefix resolves the full mount prefix for a router var,
	// walking parent includes so a router mounted into a router
	// inherits both prefixes. Self-prefix is the router's own
	// APIRouter(prefix=...); it is prepended by the per-route join, not
	// here, because only the route's owning router contributes its self
	// prefix.
	var chainPrefix func(varName string, seen map[string]bool) string
	chainPrefix = func(varName string, seen map[string]bool) string {
		if seen[varName] {
			return "" // cycle guard
		}
		seen[varName] = true
		mount := ""
		if mp, ok := globalMount[varName]; ok && !mountConflict[varName] {
			mount = mp
		}
		// The parent router's own mount prefix prepends this one. The
		// parent's *self* prefix also applies, since the parent is a
		// real router whose routes sit under parentSelf+parentMount.
		if parent, ok := globalParent[varName]; ok {
			parentChain := chainPrefix(parent, seen)
			parentSelf := ""
			if ps, ok := globalSelf[parent]; ok && !selfConflict[parent] {
				parentSelf = ps
			}
			return joinPaths(parentChain, parentSelf, mount)
		}
		return mount
	}

	// Second pass: rewrite each route contract.
	type rewrite struct {
		oldID string
		list  []Contract
	}
	var rewrites []rewrite
	processed := map[string]bool{} // contract ID -> already collected

	for _, c := range reg.All() {
		if c.Type != ContractHTTP || c.Meta == nil {
			continue
		}
		fw, _ := c.Meta["framework"].(string)
		if !routePrefixFrameworks[fw] {
			continue
		}
		// Idempotency: skip contracts already joined.
		if joined, _ := c.Meta[routePrefixJoinedMeta].(bool); joined {
			continue
		}
		if processed[c.ID] {
			continue
		}

		prefix := prefixForRoute(c, fw, facts, globalSelf, globalMount, selfConflict, mountConflict, chainPrefix, nestModulePrefix)
		if prefix == "" {
			continue
		}

		// Rewrite every contract entry recorded for this ID (provider
		// and any same-ID consumer) consistently.
		items := reg.ByID(c.ID)
		changed := false
		for i := range items {
			if items[i].Type != ContractHTTP || items[i].Meta == nil {
				continue
			}
			ifw, _ := items[i].Meta["framework"].(string)
			if !routePrefixFrameworks[ifw] {
				continue
			}
			if joined, _ := items[i].Meta[routePrefixJoinedMeta].(bool); joined {
				continue
			}
			rp := prefixForRoute(items[i], ifw, facts, globalSelf, globalMount, selfConflict, mountConflict, chainPrefix, nestModulePrefix)
			if rp == "" {
				continue
			}
			rawPath, _ := items[i].Meta["path"].(string)
			joinedRaw := joinPaths(rp, rawPath)
			newPath, names := NormalizeHTTPPathWithParams(joinedRaw)
			method, _ := items[i].Meta["method"].(string)
			if method == "" {
				method = "ANY"
			}
			newID := fmt.Sprintf("http::%s::%s", method, newPath)
			// Clone the Meta map before mutating: ByID returns value
			// copies whose Meta maps still alias the registry's other
			// secondary indexes (maps are reference types). Mutating in
			// place would leak the rewrite into stale index entries
			// before ReplaceByID purges them.
			items[i].Meta = cloneMeta(items[i].Meta)
			if newID == items[i].ID {
				// Already canonical (prefix was empty after norm).
				items[i].Meta[routePrefixJoinedMeta] = true
				changed = true
				continue
			}
			items[i].ID = newID
			items[i].Meta["path"] = newPath
			if len(names) > 0 {
				items[i].Meta["path_param_names"] = names
			}
			// Prepend the joined prefix to the route's developer-written path
			// (which keeps the original param names the re-normalized path
			// has already collapsed) and re-classify against the joined path.
			prevOrig, ok := items[i].Meta["original_path"].(string)
			if !ok || prevOrig == "" {
				prevOrig = rawPath
			}
			items[i].Meta["original_path"] = joinPaths(rp, prevOrig)
			items[i].Meta["route_kind"] = RouteKindForPath(newPath)
			items[i].Meta[routePrefixJoinedMeta] = true
			changed = true
		}
		if changed {
			processed[c.ID] = true
			rewrites = append(rewrites, rewrite{oldID: c.ID, list: items})
		}
	}

	// Apply rewrites. ReplaceByID keys the byID bucket on the OLD id,
	// so it can't be used directly when the rewrite *changes* the ID
	// (it would file the new contract under the stale key and the
	// matcher, which buckets on the byID key, would never pair it).
	// Instead clear the old bucket (purging every secondary index) and
	// re-Add each contract under its own — possibly rewritten — ID.
	for _, rw := range rewrites {
		reg.ReplaceByID(rw.oldID, nil)
		for _, c := range rw.list {
			reg.Add(c)
		}
	}
}

// prefixForRoute computes the full joined prefix (mount chain + the
// route's own router self-prefix, or NestJS controller prefix) to
// prepend to a single route contract's declared path. Returns "" when
// no prefix applies (plain join no-op).
func prefixForRoute(
	c Contract,
	framework string,
	facts map[string]RouterFacts,
	globalSelf, globalMount map[string]string,
	selfConflict, mountConflict map[string]bool,
	chainPrefix func(string, map[string]bool) string,
	nestModulePrefix map[string]string,
) string {
	f := facts[c.FilePath]
	line := f.lineFacts(c.Line)
	switch framework {
	case "fastapi/flask", "flask", "express":
		varName := line.Receivers[framework]
		if varName == "" {
			return ""
		}
		mount := chainPrefix(varName, map[string]bool{})
		self := ""
		if framework == "fastapi/flask" || framework == "flask" {
			if ps, ok := globalSelf[varName]; ok && !selfConflict[varName] {
				self = ps
			}
		}
		return joinPaths(mount, self)
	case "gin/echo/chi", "fiber", "vapor":
		// gin RouterGroup / Vapor route-group chain: the route's group var
		// carries its own .Group / .grouped prefix, parents contribute theirs
		// via the mount chain.
		varName := line.Receivers[framework]
		mount, self := "", ""
		if varName != "" {
			mount = chainPrefix(varName, map[string]bool{})
			if ps, ok := globalSelf[varName]; ok && !selfConflict[varName] {
				self = ps
			}
		}
		if framework == "vapor" {
			// Vapor's closure-block group form `app.group("api") { ... }`
			// binds no group var, so its prefix is recovered by balancing
			// braces upward from the route line over the enclosing blocks.
			closure := line.VaporPrefix
			joined := joinPaths(closure, mount, self)
			if joined != "" {
				return joined
			}
			if varName == "" {
				return ""
			}
		}
		return joinPaths(mount, self)
	case "axum":
		// axum: the route's router var is mounted under a .nest prefix.
		varName := line.Receivers[framework]
		if varName == "" {
			return ""
		}
		return chainPrefix(varName, map[string]bool{})
	case "actix":
		// actix: a route's enclosing config function is mounted under a
		// web::scope(...) prefix (possibly in another file). Single-file
		// inline scopes are already joined by the extractor, so this only
		// adds the cross-file scope-config mount prefix.
		fnName := line.Receivers[framework]
		if fnName == "" {
			return ""
		}
		return chainPrefix(fnName, map[string]bool{})
	case "nestjs":
		// Class-level @Controller('cats') prefix, found by scanning
		// upward from the route line for the nearest preceding
		// decorator in the same file, prefixed by the cross-module
		// RouterModule mount path of the controller's module (if any).
		ctrlPrefix := line.NestPrefix
		if len(nestModulePrefix) > 0 {
			if mp := nestModulePrefix[f.ControllerClass]; mp != "" {
				return joinPaths(mp, ctrlPrefix)
			}
		}
		return ctrlPrefix
	case "spring":
		return line.SpringPrefix
	case "rails":
		return line.RailsPrefix
	case "laravel":
		return line.LaravelPrefix
	}
	return ""
}

// lastPathSeg returns the final `::`-separated segment of a Rust path, e.g.
// "users::routes" -> "routes", "routes" -> "routes".
func lastPathSeg(ref string) string {
	if i := strings.LastIndex(ref, "::"); i >= 0 {
		return ref[i+2:]
	}
	return ref
}

// railsLineOpensBlock reports whether a Ruby routes line opens a do-block
// (namespace / resources / scope / draw).
func railsLineOpensBlock(trimmed string) bool {
	return strings.HasSuffix(trimmed, " do") ||
		strings.HasSuffix(trimmed, "do") && strings.Contains(trimmed, " do") ||
		strings.Contains(trimmed, " do |") ||
		strings.Contains(trimmed, " do;")
}

// receiverOfInclude returns the receiver variable in front of an
// `.include_router(` call given the matched fragment, e.g. for
// `parent.include_router(child, ...)` returns "parent". Returns "" for
// a bare `include_router(...)` (an imported function call with no
// receiver) — those default to the implicit app and don't chain.
func receiverOfInclude(text, fragment string) string {
	i := strings.Index(text, fragment)
	if i <= 0 {
		return ""
	}
	// Walk backwards over the receiver expression: <recv>.include_router
	j := i
	// Skip a leading '.' if the fragment started at include_router.
	if j > 0 && text[j-1] == '.' {
		j--
	}
	end := j
	for j > 0 {
		ch := text[j-1]
		if isIdentByte(ch) || ch == '.' {
			j--
			continue
		}
		break
	}
	recv := text[j:end]
	return lastAttr(recv)
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// lastAttr returns the final attribute of a dotted reference, e.g.
// "users.router" -> "router", "router" -> "router".
func lastAttr(ref string) string {
	if i := strings.LastIndex(ref, "."); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// cloneMeta returns a shallow copy of a contract Meta map so a rewrite
// doesn't mutate the map still aliased by the registry's other indexes.
func cloneMeta(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cleanPrefix normalises a raw prefix string: trims surrounding
// whitespace, drops a trailing slash, and ensures it is either empty or
// starts with a slash. "/api/" -> "/api"; "api" -> "/api"; "" -> "".
func cleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	return p
}

// joinPaths concatenates path segments, skipping empties and collapsing
// duplicate slashes. joinPaths("/api", "/users", "/{id}") ->
// "/api/users/{id}"; joinPaths("", "/users") -> "/users".
func joinPaths(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = strings.TrimRight(p, "/")
		b.WriteString(p)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}
