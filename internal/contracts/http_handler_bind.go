package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Handler-binding for backend route frameworks whose handler is NOT the
// same-line bare identifier the generic handlerGrp path already resolves. The
// per-file pass captures the controller class + action and resolves it against
// the nodes in scope; when the controller lives in another file (Laravel /
// Rails route files wire handlers defined under app/Http/Controllers or
// app/controllers) the per-file lookup misses and the bare identifier is
// stamped so the indexer's module-wide resolveProviderHandlers binds it across
// files — the cross-file reach a same-file regex extractor cannot match.

var (
	// Laravel array action: `[UserController::class, 'index']`.
	laravelArrayHandlerRE = regexp.MustCompile(`\[\s*([A-Za-z_][\w\\]*)::class\s*,\s*['"]([A-Za-z_]\w*)['"]`)
	// Laravel string action: `'UserController@index'` (class may be namespaced).
	laravelStringHandlerRE = regexp.MustCompile(`['"]([A-Za-z_][\w\\]*)@([A-Za-z_]\w*)['"]`)
	// Rails action: `to: 'users#index'` or `=> 'admin/users#create'`.
	railsActionRE = regexp.MustCompile(`(?:to:\s*|=>\s*)['"]([a-z_][\w/]*)#([a-z_]\w*)['"]`)
)

// backendHandlerFrameworks are the provider frameworks whose handler is bound
// by bindBackendHandler rather than the generic same-line handlerGrp path.
var backendHandlerFrameworks = map[string]bool{
	"laravel": true,
	"rails":   true,
	"spring":  true,
	"jaxrs":   true,
	"aspnet":  true,
}

func isBackendHandlerFramework(framework string) bool {
	return backendHandlerFrameworks[framework]
}

// bindBackendHandler captures and (where possible) resolves the controller
// handler a backend route dispatches to. It returns the resolved handler node
// ID when the controller method is among fileNodes (same-file frameworks like
// Spring, or — in the indexer — once the module-wide pass has the controller
// in scope), the bare action identifier to stamp for that later cross-file
// pass, and the declaring controller class so a receiver-correct method is
// chosen over any same-named action elsewhere.
func bindBackendHandler(framework, routeLine string, lineIdx int, lines []string, fileNodes []*graph.Node) (symbolID, handlerIdent, handlerClass string) {
	switch framework {
	case "laravel":
		if mm := laravelArrayHandlerRE.FindStringSubmatch(routeLine); mm != nil {
			handlerClass, handlerIdent = lastNamespaceSegment(mm[1]), mm[2]
		} else if mm := laravelStringHandlerRE.FindStringSubmatch(routeLine); mm != nil {
			handlerClass, handlerIdent = lastNamespaceSegment(mm[1]), mm[2]
		}
	case "rails":
		if mm := railsActionRE.FindStringSubmatch(routeLine); mm != nil {
			handlerClass, handlerIdent = railsControllerClass(mm[1]), mm[2]
		}
	case "spring", "jaxrs", "aspnet":
		// The verb annotation sits directly above its handler method; the
		// handler is the next method declaration in the same controller file.
		handlerIdent = forwardScanMethodName(lines, lineIdx)
	}

	if handlerIdent == "" {
		return "", "", ""
	}
	// Receiver-aware resolution first (precise — picks the action on the named
	// controller), then a bare-name fallback for single-controller files.
	if handlerClass != "" {
		if id := findMethodByNameAndReceiver(fileNodes, handlerIdent, handlerClass); id != "" {
			return id, handlerIdent, handlerClass
		}
	}
	if id := resolveHandlerIdent(fileNodes, handlerIdent); id != "" {
		return id, handlerIdent, handlerClass
	}
	return "", handlerIdent, handlerClass
}

// lastNamespaceSegment strips a PHP namespace prefix so
// `App\Http\Controllers\UserController` becomes `UserController`.
func lastNamespaceSegment(class string) string {
	if i := strings.LastIndex(class, `\`); i >= 0 {
		return class[i+1:]
	}
	return class
}

// railsControllerClass maps a Rails route controller token to its class name:
// `users` -> `UsersController`, `admin/users` -> `UsersController` (the last
// path segment is the class; the namespace is carried by the module prefix).
func railsControllerClass(token string) string {
	if i := strings.LastIndex(token, "/"); i >= 0 {
		token = token[i+1:]
	}
	return railsCamelize(token) + "Controller"
}

// railsCamelize converts a snake_case Rails identifier to CamelCase
// (`user_sessions` -> `UserSessions`).
func railsCamelize(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicodeToUpper(r))
			upper = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unicodeToUpper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}

// forwardScanMethodName walks forward from an annotation line to the first
// method declaration and returns its name — the identifier immediately before
// the first `(` on that line. Blank lines, further annotations/attributes, and
// comment lines are skipped so a stacked `@GetMapping` + `@ResponseBody` (Java)
// or `[HttpGet]` + `[Authorize]` (C#) still reaches the method below them.
func forwardScanMethodName(lines []string, fromIdx int) string {
	for i := fromIdx + 1; i < len(lines) && i < fromIdx+8; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "[") ||
			strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		paren := strings.IndexByte(t, '(')
		if paren <= 0 {
			// A declaration that wraps onto the next line still names the
			// method before any `(`; if there is no `(` yet, keep scanning.
			if name := lastIdentifier(t); name != "" {
				return name
			}
			continue
		}
		return lastIdentifier(t[:paren])
	}
	return ""
}

// lastIdentifier returns the final identifier token in a fragment — the method
// name in `public List<User> getUsers` or `suspend fun loadUser`.
func lastIdentifier(s string) string {
	end := len(s)
	for end > 0 && !isIdentByte(s[end-1]) {
		end--
	}
	start := end
	for start > 0 && isIdentByte(s[start-1]) {
		start--
	}
	if start == end {
		return ""
	}
	return s[start:end]
}

// --- Rails resources expander -------------------------------------------

var (
	railsResourcesRE = regexp.MustCompile(`(?m)^\s*(resources?)\s+:(\w+)([^\n]*)`)
	railsOnlyRE      = regexp.MustCompile(`only:\s*\[([^\]]*)\]`)
	railsExceptRE    = regexp.MustCompile(`except:\s*\[([^\]]*)\]`)
	railsSymbolRE    = regexp.MustCompile(`:(\w+)`)
)

// railsRESTRoute is one of the seven routes a `resources` declaration expands
// to: an action, its HTTP method, the path suffix after the collection base,
// and whether it addresses a member (`/:id`) rather than the collection.
type railsRESTRoute struct {
	action string
	method string
	suffix string
	member bool
}

// railsCollectionRoutes is the canonical RESTful 7-route expansion Rails
// generates for `resources :name`.
var railsCollectionRoutes = []railsRESTRoute{
	{"index", "GET", "", false},
	{"create", "POST", "", false},
	{"new", "GET", "/new", false},
	{"edit", "GET", "/edit", true},
	{"show", "GET", "", true},
	{"update", "PATCH", "", true},
	{"destroy", "DELETE", "", true},
}

// extractRailsResourceRoutes expands `resources :photos` / `resource :photo`
// (with optional only:/except: filters) into the canonical RESTful routes,
// binding each action to its controller method (receiver-aware) or stamping
// the action + controller for the module-wide cross-file pass. This is the
// implicit-route surface a path-only regex extractor never recovers — one
// `resources` line becomes seven navigable provider contracts.
func (h *HTTPExtractor) extractRailsResourceRoutes(filePath, text string, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) []Contract {
	var out []Contract
	for _, m := range railsResourcesRE.FindAllStringSubmatchIndex(text, -1) {
		kind := text[m[2]:m[3]]
		name := text[m[4]:m[5]]
		rest := text[m[6]:m[7]]
		lineNum := lineAtOffset(lines, m[0])
		singular := kind == "resource"

		plural := naivePluralize(name)
		controller := railsCamelize(plural) + "Controller"
		base := "/" + plural
		allowed := railsActionFilter(rest)

		for _, r := range railsCollectionRoutes {
			if singular && r.action == "index" {
				continue // a singular resource has no index route
			}
			if allowed != nil && !allowed[r.action] {
				continue
			}
			path := base
			if r.member && !singular {
				path += "/:id"
			}
			path += r.suffix

			normPath, origNames := NormalizeHTTPPathWithParams(path)
			c := Contract{
				ID:         fmt.Sprintf("http::%s::%s", r.method, normPath),
				Type:       ContractHTTP,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNum,
				Confidence: 0.85,
				Meta: map[string]any{
					"method":         r.method,
					"path":           normPath,
					"framework":      "rails",
					"rails_resource": name,
				},
			}
			if len(origNames) > 0 {
				c.Meta["path_param_names"] = origNames
			}
			if id := findMethodByNameAndReceiver(fileNodes, r.action, controller); id != "" {
				c.SymbolID = id
			} else {
				c.Meta["handler_ident"] = r.action
				c.Meta["handler_class"] = controller
			}
			EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)
			out = append(out, c)
		}
	}
	return out
}

var (
	laravelResourceRE            = regexp.MustCompile(`Route::(resource|apiResource)\(\s*['"]([^'"]+)['"]\s*,\s*\\?([\w\\]+)::class\s*(?:,|\))`)
	laravelResourceChainFilterRE = regexp.MustCompile(`->(only|except)\s*\(\s*([^)]*)\)`)
	laravelResourceOptionRE      = regexp.MustCompile(`(?s)^['"](\w+)['"]\s*=>\s*(.*)$`)
	laravelActionListRE          = regexp.MustCompile(`^\[\s*(?:['"]\w+['"]\s*,?\s*)*\]$`)
	laravelActionRE              = regexp.MustCompile(`['"](\w+)['"]`)
	laravelOnlyRE                = regexp.MustCompile(`->only\(\s*\[([^\]]*)\]`)
	laravelExceptRE              = regexp.MustCompile(`->except\(\s*\[([^\]]*)\]`)
)

// laravelResourceRoutes is the canonical RESTful 7-route expansion Laravel
// generates for `Route::resource`. `apiResource` drops create + edit.
var laravelResourceRoutes = []railsRESTRoute{
	{"index", "GET", "", false},
	{"create", "GET", "/create", false},
	{"store", "POST", "", false},
	{"show", "GET", "", true},
	{"edit", "GET", "/edit", true},
	{"update", "PUT", "", true},
	{"destroy", "DELETE", "", true},
}

// laravelApiResourceSkip are the actions apiResource omits (no HTML form
// views).
var laravelApiResourceSkip = map[string]bool{"create": true, "edit": true}

// extractLaravelResourceRoutes expands `Route::resource('users',
// UserController::class)` / `apiResource` into the canonical RESTful routes,
// binding each action to its controller method (receiver-aware) or stamping
// the action + controller for the module-wide cross-file pass. `->only`/
// `->except` filter the action set.
func (h *HTTPExtractor) extractLaravelResourceRoutes(filePath, text string, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) []Contract {
	var out []Contract
	source := []byte(text)
	for _, m := range laravelResourceRE.FindAllStringSubmatchIndex(text, -1) {
		// Options may contain nested arrays, calls and quoted parentheses.
		// Locate the registration's end before reading chained action filters.
		open := m[0] + strings.IndexByte(text[m[0]:m[1]], '(')
		end := balancedCloseParen(source, open)
		if end < 0 {
			continue
		}
		kind := text[m[2]:m[3]]
		name := text[m[4]:m[5]]
		controller := laravelSimpleClass(text[m[6]:m[7]])
		rest := text[end+1:]
		if stop := strings.IndexByte(rest, ';'); stop >= 0 {
			rest = rest[:stop]
		}
		lineNum := lineAtOffset(lines, m[0])
		isAPI := kind == "apiResource"
		allowed := laravelActionFilter(rest)
		if text[m[1]-1] == ',' {
			allowed = laravelResourceOptionsFilter(text[m[1]:end], rest)
		}
		base := "/" + name

		for _, r := range laravelResourceRoutes {
			if isAPI && laravelApiResourceSkip[r.action] {
				continue
			}
			if allowed != nil && !allowed[r.action] {
				continue
			}
			path := base
			if r.member {
				path += "/{id}"
			}
			path += r.suffix

			normPath, origNames := NormalizeHTTPPathWithParams(path)
			c := Contract{
				ID:         fmt.Sprintf("http::%s::%s", r.method, normPath),
				Type:       ContractHTTP,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNum,
				Confidence: 0.85,
				Meta: map[string]any{
					"method":           r.method,
					"path":             normPath,
					"framework":        "laravel",
					"laravel_resource": name,
				},
			}
			if len(origNames) > 0 {
				c.Meta["path_param_names"] = origNames
			}
			if id := findMethodByNameAndReceiver(fileNodes, r.action, controller); id != "" {
				c.SymbolID = id
			} else {
				c.Meta["handler_ident"] = r.action
				c.Meta["handler_class"] = controller
			}
			EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)
			out = append(out, c)
		}
	}
	return out
}

// laravelResourceOptionsFilter reads top-level action filters only: a nested
// names map can itself contain keys named "only" or "except".
func laravelResourceOptionsFilter(options, rest string) map[string]bool {
	options = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(options), ","))
	if options == "" {
		return laravelActionFilter(rest)
	}
	if !strings.HasPrefix(options, "[") || !strings.HasSuffix(options, "]") {
		return map[string]bool{} // Dynamic options cannot safely determine routes.
	}
	filters := map[string]string{}
	entries := strings.TrimSpace(options[1 : len(options)-1])
	for entries != "" {
		var entry string
		entry, entries = splitFirstArg(entries)
		m := laravelResourceOptionRE.FindStringSubmatch(entry)
		if m == nil {
			return map[string]bool{}
		}
		if m[1] == "only" || m[1] == "except" {
			filters[m[1]] = strings.TrimSpace(m[2])
		}
	}
	// Chained setters override the corresponding constructor options.
	for _, m := range laravelResourceChainFilterRE.FindAllStringSubmatch(rest, -1) {
		filters[m[1]] = strings.TrimSpace(m[2])
	}
	allowed := map[string]bool{}
	for _, r := range laravelResourceRoutes {
		allowed[r.action] = true
	}
	for name, value := range filters {
		if !laravelActionListRE.MatchString(value) {
			return map[string]bool{}
		}
		selected := laravelActionFilter("->" + name + "(" + value + ")")
		for action := range allowed {
			if !selected[action] {
				delete(allowed, action)
			}
		}
	}
	return allowed
}

// laravelActionFilter reads a chained `->only([...])` / `->except([...])`
// clause (Laravel uses quoted action names) and returns the allowed set, or
// nil when every action is allowed.
func laravelActionFilter(rest string) map[string]bool {
	if mm := laravelOnlyRE.FindStringSubmatch(rest); mm != nil {
		allowed := map[string]bool{}
		for _, s := range laravelActionRE.FindAllStringSubmatch(mm[1], -1) {
			allowed[s[1]] = true
		}
		return allowed
	}
	if mm := laravelExceptRE.FindStringSubmatch(rest); mm != nil {
		excluded := map[string]bool{}
		for _, s := range laravelActionRE.FindAllStringSubmatch(mm[1], -1) {
			excluded[s[1]] = true
		}
		allowed := map[string]bool{}
		for _, r := range laravelResourceRoutes {
			if !excluded[r.action] {
				allowed[r.action] = true
			}
		}
		return allowed
	}
	return nil
}

// laravelSimpleClass strips a leading `\` and namespace from a PHP FQCN,
// returning the bare class name.
func laravelSimpleClass(fqcn string) string {
	fqcn = strings.TrimPrefix(fqcn, "\\")
	if i := strings.LastIndex(fqcn, "\\"); i >= 0 {
		return fqcn[i+1:]
	}
	return fqcn
}

// railsActionFilter reads an only:/except: clause and returns the set of
// allowed actions, or nil when every action is allowed.
func railsActionFilter(rest string) map[string]bool {
	if mm := railsOnlyRE.FindStringSubmatch(rest); mm != nil {
		allowed := map[string]bool{}
		for _, s := range railsSymbolRE.FindAllStringSubmatch(mm[1], -1) {
			allowed[s[1]] = true
		}
		return allowed
	}
	if mm := railsExceptRE.FindStringSubmatch(rest); mm != nil {
		excluded := map[string]bool{}
		for _, s := range railsSymbolRE.FindAllStringSubmatch(mm[1], -1) {
			excluded[s[1]] = true
		}
		allowed := map[string]bool{}
		for _, r := range railsCollectionRoutes {
			if !excluded[r.action] {
				allowed[r.action] = true
			}
		}
		return allowed
	}
	return nil
}

// naivePluralize is a light inflector for Rails resource tokens: it leaves an
// already-plural token alone, applies the `y`->`ies` rule, and otherwise
// appends `s`. Enough for the conventional resource names; the exact-noun
// edge cases (person->people) are not the point of route extraction.
func naivePluralize(s string) string {
	if strings.HasSuffix(s, "s") {
		return s
	}
	if strings.HasSuffix(s, "y") && len(s) >= 2 && !isVowelByte(s[len(s)-2]) {
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

func isVowelByte(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return true
	}
	return false
}

// findMethodByNameAndReceiver resolves a controller action precisely: a method
// named `name` whose receiver / enclosing type is `receiver`. This is what
// makes `[UserController::class, 'index']` bind to UserController's index and
// not some other controller's same-named action — the receiver-correct
// resolution a name-only regex match cannot achieve.
func findMethodByNameAndReceiver(fileNodes []*graph.Node, name, receiver string) string {
	for _, n := range fileNodes {
		if n.Kind != graph.KindMethod || n.Name != name {
			continue
		}
		if recv, _ := n.Meta["receiver"].(string); recv == receiver {
			return n.ID
		}
		// Some extractors record the enclosing type on the ID
		// (`file::Controller.action`) rather than a receiver Meta key.
		if strings.HasSuffix(n.ID, "::"+receiver+"."+name) || strings.HasSuffix(n.ID, "."+receiver+"."+name) {
			return n.ID
		}
	}
	return ""
}
