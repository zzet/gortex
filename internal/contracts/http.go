package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/parser"

	"github.com/zzet/gortex/internal/graph"
)

// HTTPExtractor detects HTTP route provider and consumer patterns across
// multiple languages using regex matching on source text.
type HTTPExtractor struct {
	// ClientAliases are project-defined wrapped HTTP-client function
	// names (e.g. "apiGet", "apiPost", "client.request"). Calls to any
	// of these are treated as HTTP consumer contracts even though they
	// don't match the built-in fetch/axios heuristics. Empty disables
	// the alias pass. Sourced from index.http_client_aliases. See
	// detectClientAliasConsumers for the supported call shapes.
	ClientAliases []string
}

var (
	_ Extractor           = (*HTTPExtractor)(nil)
	_ TreeAwareExtractor  = (*HTTPExtractor)(nil)
	_ StoreAwareExtractor = (*HTTPExtractor)(nil)
)

// SupportedLanguages returns the languages this extractor can analyse.
func (h *HTTPExtractor) SupportedLanguages() []string {
	return []string{
		"go", "typescript", "javascript", "python",
		"java", "kotlin", "dart", "swift",
		"rust", "csharp", "ruby", "php", "elixir", "scala",
		// File-based routing: page files in these frameworks carry the route
		// in their path, so the extractor must see them even though they are
		// not otherwise HTTP-bearing languages.
		"astro", "svelte", "vue",
		// YAML: Drupal *.routing.yml route declarations (a FrameworkRoutePass
		// filters to the routing files by name).
		"yaml",
	}
}

// httpPattern describes a single regex pattern that matches an HTTP route
// declaration or call.
type httpPattern struct {
	re        *regexp.Regexp
	role      Role
	method    string // HTTP method (empty = extract from match)
	methodGrp int    // capture group index for method when not fixed
	pathGrp   int    // capture group index for path
	// handlerGrp is the capture group for the handler identifier on the
	// provider side (e.g. `listUsers` in `r.GET("/users", listUsers)`).
	// 0 = not captured. When set and the capture resolves to a function
	// node in the same file, the Contract's SymbolID is the handler, not
	// the enclosing registration function — so "trace a request" queries
	// land on the business logic instead of setupRoutes().
	handlerGrp int
	framework  string
	confidence float64
	languages  []string // empty = all
}

// Compiled patterns -----------------------------------------------------------

var httpPatterns = []httpPattern{
	// ---- Go providers (high confidence, framework-specific) ----
	// Go 1.22+ stdlib mux: mux.HandleFunc("METHOD /path", h). The
	// method is embedded in the pattern as a prefix and must be
	// split out so the resulting contract ID matches the consumer
	// side's http::METHOD::path shape.
	{
		re:         regexp.MustCompile(`(?:Handle|HandleFunc)\(\s*["` + "`" + `](GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(/[^"` + "`" + `]*)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "net/http",
		confidence: 0.95,
		languages:  []string{"go"},
	},
	// Legacy net/http HandleFunc with pattern-only path. Requires the
	// captured path to start with "/" (no leading verb), so the Go
	// 1.22+ "METHOD /path" form above doesn't double-match and emit
	// a bogus http::ANY::/VERB path contract alongside the canonical
	// http::VERB::/path one.
	{
		re:         regexp.MustCompile(`(?:Handle|HandleFunc)\(\s*["` + "`" + `](/[^"` + "`" + `]*)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		method:     "ANY",
		pathGrp:    1,
		handlerGrp: 2,
		framework:  "net/http",
		confidence: 0.9,
		languages:  []string{"go"},
	},
	{
		// Match router/group method calls but not http.Get/http.Post (stdlib consumers).
		re:         regexp.MustCompile(`(?:^|[^/])\b(?:r|g|e|router|group|api|v1|mux|app)\.(Get|Post|Put|Delete|Patch|Head|Options)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "gin/echo/chi",
		confidence: 0.9,
		languages:  []string{"go"},
	},
	{
		// Path must start with "/". Otherwise the bare `.VERB(` anchor
		// matches verb names that appear inside string literals (e.g.
		// the prefilter marker `[]byte(".GET(")` in this very file),
		// and the path capture overshoots until the next quote — emitting
		// contracts whose "path" is a chunk of source code.
		re:         regexp.MustCompile(`\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\(\s*["` + "`" + `](/[^"` + "`" + `]*)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "fiber",
		confidence: 0.9,
		languages:  []string{"go"},
	},

	// ---- TS/JS providers ----
	{
		re:         regexp.MustCompile(`(?:app|router)\.(get|post|put|delete|patch|head|options|all)\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "express",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},
	{
		// Fastify instance verbs (the instance is conventionally `fastify`,
		// which the express receiver set does not cover).
		re:         regexp.MustCompile(`fastify\.(get|post|put|delete|patch|head|options|all)\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "fastify",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},
	{
		// Koa-router's `.del` alias for DELETE (express uses `.delete`).
		re:         regexp.MustCompile(`(?:router|app)\.del\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		method:     "DELETE",
		pathGrp:    1,
		handlerGrp: 2,
		framework:  "koa",
		confidence: 0.85,
		languages:  []string{"typescript", "javascript"},
	},
	{
		re:         regexp.MustCompile(`@(Get|Post|Put|Delete|Patch|Head|Options|All)\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "nestjs",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},

	// ---- Python providers ----
	{
		re:         regexp.MustCompile(`@\w+\.(get|post|put|delete|patch|head|options)\(\s*["']([^"']+)["']`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "fastapi/flask",
		confidence: 0.9,
		languages:  []string{"python"},
	},
	// Flask @route is handled by the node-aware extractFlaskDecoratorRoutes
	// pass (it expands methods=[...] and resolves the view), not the
	// per-line table.
	// Django path/re_path/url routing is handled by the node-aware
	// extractDjangoRoutes pass (it resolves the view handler), not the
	// per-line table.

	// ---- Java providers ----
	{
		re:         regexp.MustCompile(`@(Get|Post|Put|Delete|Patch)Mapping\(\s*(?:value\s*=\s*)?["']([^"']+)["']`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "spring",
		confidence: 0.9,
		languages:  []string{"java", "kotlin"},
	},
	{
		re:         regexp.MustCompile(`@RequestMapping\(\s*(?:value\s*=\s*)?["']([^"']+)["']`),
		role:       RoleProvider,
		method:     "ANY",
		pathGrp:    1,
		framework:  "spring",
		confidence: 0.9,
		languages:  []string{"java", "kotlin"},
	},
	{
		re:         regexp.MustCompile(`@(GET|POST|PUT|DELETE|PATCH)\s+@Path\(\s*["']([^"']+)["']`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "jaxrs",
		confidence: 0.9,
		languages:  []string{"java", "kotlin"},
	},

	// ---- Go consumers ----
	{
		re:         regexp.MustCompile(`http\.(Get|Post|Head)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "net/http",
		confidence: 0.9,
		languages:  []string{"go"},
	},
	{
		re:         regexp.MustCompile(`http\.NewRequest\(\s*["` + "`" + `](\w+)["` + "`" + `]\s*,\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "net/http",
		confidence: 0.9,
		languages:  []string{"go"},
	},

	// ---- TS/JS consumers ----
	// fetch with explicit `method: '<VERB>'` in the options object —
	// tried first so the generic GET pattern below doesn't steal the
	// match.
	{
		re:         regexp.MustCompile(`fetch\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `][^)]*?method\s*:\s*["'](\w+)["']`),
		role:       RoleConsumer,
		methodGrp:  2,
		pathGrp:    1,
		framework:  "fetch",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},
	{
		re:         regexp.MustCompile(`fetch\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`),
		role:       RoleConsumer,
		method:     "GET",
		pathGrp:    1,
		framework:  "fetch",
		confidence: 0.7,
		languages:  []string{"typescript", "javascript"},
	},
	// Axios. The optional `<...>` between the method name and the
	// opening paren is TypeScript's generic form — axios.post<Resp, Req>(...)
	// — which the enrichment layer uses to pin response / request
	// types. `[^<>(),]` inside the generic keeps the matcher fast and
	// stops greedy consumption from crossing the path argument.
	{
		re:         regexp.MustCompile(`axios\.(get|post|put|delete|patch|head|options)(?:<[^<>()]*>)?\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "axios",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},

	// ---- Python / Java consumers ----
	// Python (requests/httpx/aiohttp/urllib3) and JVM (OkHttp/RestTemplate/
	// WebClient) HTTP-client consumers are detected by the import-gated
	// detectClientLibConsumers pass (http_client_libs.go), which binds the call
	// receiver to a resolved library import instead of matching a bare call-text
	// substring — so a local variable named `requests` or a non-HTTP `obj.get`
	// accessor no longer mints a spurious consumer contract.

	// ---- Dart consumers ----
	// Dio (the dominant HTTP client in modern Flutter apps). Matches
	// identifiers like `dio`, `_dio`, `apiDio` etc. invoking a method
	// with a string-literal path.
	{
		re:         regexp.MustCompile(`\b_?\w*[Dd]io\.(get|post|put|delete|patch|head)\(\s*['"]([^'"]+)['"]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "dio",
		confidence: 0.9,
		languages:  []string{"dart"},
	},
	// package:http functional API — http.get(Uri.parse('/x')) or
	// http.post('/x'). The regex captures either the string inside
	// Uri.parse or the direct literal argument.
	{
		re:         regexp.MustCompile(`\bhttp\.(get|post|put|delete|patch|head)\(\s*(?:Uri\.parse\(\s*)?['"]([^'"]+)['"]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "package:http",
		confidence: 0.8,
		languages:  []string{"dart"},
	},

	// ---- Rust providers ----
	// Axum: `Router::new().route("/users", get(handler))` and
	// `Router::new().route("/users/:id", post(create).delete(remove))`.
	// The method comes from the `get|post|...` function call; the
	// path is the first string literal in `.route(`.
	// Axum `.route("/path", <chain>)` provider routes — including the
	// chained-method form `get(h1).post(h2)` — are handled by the Rust route
	// pass (http_rust.go), which emits one Contract per method in the chain.

	// Actix-web macro form: `#[get("/path")]` / `#[post("/path")]`.
	{
		re:         regexp.MustCompile(`#\[(get|post|put|delete|patch|head|options)\(\s*"([^"]+)"`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "actix",
		confidence: 0.95,
		languages:  []string{"rust"},
	},
	// Rocket macro form: `#[get("/path")]` (same syntax as Actix —
	// the detection code can't tell them apart from the route line
	// alone; tag as "rust" and let repo context disambiguate).
	// Covered by the Actix regex above.

	// ---- Rust consumers ----
	// reqwest consumers (`client.get("/users")`) are detected by the
	// import-gated detectClientLibConsumers pass (http_client_libs.go): the call
	// receiver must bind to a `use reqwest::…` import, so an unregistered crate
	// like surf / hyper never mints a consumer contract.

	// ---- C# ASP.NET providers ----
	// Attribute routing: `[HttpGet("/path")]`, `[HttpPost]` +
	// `[Route("path")]`. First form is the clean one.
	{
		re:         regexp.MustCompile(`\[Http(Get|Post|Put|Delete|Patch|Head|Options)\(\s*"([^"]+)"`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "aspnet",
		confidence: 0.95,
		languages:  []string{"csharp"},
	},
	// Minimal APIs: `app.MapGet("/path", handler)`.
	{
		re:         regexp.MustCompile(`\b(?:app|routes?)\.Map(Get|Post|Put|Delete|Patch|Head|Options)\(\s*"([^"]+)"`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "aspnet",
		confidence: 0.9,
		languages:  []string{"csharp"},
	},

	// ---- C# consumers ----
	// HttpClient: `client.GetAsync("/path")`, `PostAsync`, etc.
	{
		re:         regexp.MustCompile(`\b\w+\.(Get|Post|Put|Delete|Patch|Head|Options)(?:Async|String)?\(\s*"([^"]+)"`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "httpclient",
		confidence: 0.7,
		languages:  []string{"csharp"},
	},

	// ---- Ruby on Rails providers ----
	// Explicit route: `get '/users', to: 'users#index'`,
	// `post '/users' => 'users#create'`.
	{
		re:         regexp.MustCompile(`(?m)^\s*(get|post|put|patch|delete|head|options)\s+['"]([^'"]+)['"]\s*(?:,|=>)`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "rails",
		confidence: 0.9,
		languages:  []string{"ruby"},
	},

	// ---- Ruby consumers ----
	// Faraday / HTTParty / Net::HTTP / RestClient consumers are detected by the
	// import-gated detectClientLibConsumers pass (http_client_libs.go), which
	// requires a `require 'faraday'` (etc.) and binds the receiver to it — so an
	// arbitrary `obj.get('...')` accessor is no longer a consumer.

	// ---- PHP Laravel providers ----
	// `Route::get('/path', ...)`, `Route::post('/path', [Controller::class, 'method'])`.
	{
		re:         regexp.MustCompile(`Route::(get|post|put|patch|delete|head|options)\(\s*['"]([^'"]+)['"]`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "laravel",
		confidence: 0.95,
		languages:  []string{"php"},
	},
	// Symfony attribute routing: `#[Route("/path", methods: ["POST"])]`.
	{
		re:         regexp.MustCompile(`#\[Route\(\s*['"]([^'"]+)['"][^)]*methods:\s*\[\s*['"](\w+)['"]`),
		role:       RoleProvider,
		methodGrp:  2,
		pathGrp:    1,
		framework:  "symfony",
		confidence: 0.9,
		languages:  []string{"php"},
	},

	// ---- PHP consumers ----
	// Guzzle consumers (`$client->get('/path')`, `$client->request('POST', …)`)
	// are detected by the import-gated detectClientLibConsumers pass
	// (http_client_libs.go): the variable must be constructed from a
	// `use GuzzleHttp\…` import before its verb calls become consumers.

	// ---- Elixir Phoenix providers ----
	// `get "/users", UserController, :index` inside router.ex scope.
	{
		re:         regexp.MustCompile(`(?m)^\s*(get|post|put|patch|delete|head|options)\s+"([^"]+)"\s*,`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "phoenix",
		confidence: 0.9,
		languages:  []string{"elixir"},
	},

	// ---- Dart providers (shelf_router) ----
	// `router.get('/path', handler)` and Dart's cascade form
	// `..get('/path', handler)` — the latter dominates in
	// idiomatic shelf_router code.
	{
		re:         regexp.MustCompile(`(?:\.\.|\b\w*[Rr]outer\.)(get|post|put|delete|patch|head|options)\(\s*['"]([^'"]+)['"]\s*,\s*(\w+)?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "shelf_router",
		confidence: 0.85,
		languages:  []string{"dart"},
	},

	// ---- Swift providers (Vapor / Hummingbird) ----
	// `app.get("users") { req in ... }` (trailing-closure handler) and
	// `app.get("users", use: listUsers)` (use: labelled handler). The
	// receiver is anchored to the conventional names (app / routes /
	// router) — same trade-off express makes for route-group vars.
	{
		// Receiver is captured (group 1) — not just app/routes/router — so a
		// route declared on a `.grouped(...)` group var carries its receiver
		// for the prefix-join pass. The `use:` label binds the handler.
		re:         regexp.MustCompile(`\b(\w+)\.(get|post|put|delete|patch)\(\s*"([^"]+)"(?:[^{)]*?\buse:\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  2,
		pathGrp:    3,
		handlerGrp: 4,
		framework:  "vapor",
		confidence: 0.85,
		languages:  []string{"swift"},
	},

	// ---- Swift consumers (Alamofire) ----
	// `AF.request("https://…", method: .get)` /
	// `session.request("…", method: .post)`.
	{
		re:         regexp.MustCompile(`\b(?:AF|\w*[Ss]ession)\.request\(\s*"([^"]+)"[^{)]*?\bmethod:\s*\.(get|post|put|delete|patch)`),
		role:       RoleConsumer,
		methodGrp:  2,
		pathGrp:    1,
		framework:  "alamofire",
		confidence: 0.8,
		languages:  []string{"swift"},
	},
}

// httpPrefilterMarkers is the per-language substring prefilter.
// Files whose language appears here must contain at least one
// marker before the ~30 HTTP regexes run — otherwise we skip the
// file entirely. See the gRPC reference implementation for the
// pattern's origin. Languages whose HTTP patterns hinge on
// bare keywords (python's `path(`, ruby/elixir's top-level `get`,
// `post`) are intentionally absent: any marker tight enough to
// reject non-HTTP files would also reject legitimate HTTP files,
// so the regex scan carries the whole cost.
var httpPrefilterMarkers = map[string][][]byte{
	// Phase 2 of spec-contract-extraction.md removed the "go" entry.
	// The AST detector for Go routes (detectGoRoutesAST) doesn't
	// self-match its own marker source, so the workaround that
	// motivated those markers — and the Fiber `[]byte(".GET(")`
	// bug it was paired with — is structurally impossible. Keeping
	// the prefilter for non-Go languages where the regex still
	// runs.
	"typescript": httpTsJsMarkers,
	"javascript": httpTsJsMarkers,
	"java":       httpJvmMarkers,
	"kotlin":     httpJvmMarkers,
	"dart": {
		[]byte("dio."), // lowercased dio instance
		[]byte("Dio."), // PascalCase Dio class
		[]byte("http."),
		[]byte("Router"),  // shelf_router `Router()` / `router.`
		[]byte("..get("),  // shelf_router cascade form
		[]byte("..post("), // idem
		[]byte("..put("),  // idem
		[]byte("..delete("),
		[]byte("..patch("),
		[]byte("..head("),
		[]byte("..options("),
	},
	// rust: the reqwest consumer pattern is `\w+\.(get|post|...)(`,
	// and those verbs are universal Rust method calls. Any marker
	// tight enough to reject non-HTTP files would also reject
	// reqwest consumers, so we leave rust out and run the regex
	// scan unconditionally.
	"csharp": {
		[]byte("[Http"),      // attribute routing
		[]byte(".Map"),       // minimal APIs
		[]byte("GetAsync("),  // HttpClient consumer idiom
		[]byte("PostAsync("), // idem
		[]byte("PutAsync("),
		[]byte("DeleteAsync("),
		[]byte("PatchAsync("),
	},
	"php": {
		[]byte("Route::"),
		[]byte("#[Route"),
		[]byte("->get("),
		[]byte("->post("),
		[]byte("->put("),
		[]byte("->delete("),
		[]byte("->patch("),
	},
	"swift": {
		[]byte(".get("),
		[]byte(".post("),
		[]byte(".put("),
		[]byte(".delete("),
		[]byte(".patch("),
		[]byte(".request("),
	},
}

var httpTsJsMarkers = [][]byte{
	[]byte("fetch("),
	[]byte("axios"),
	[]byte("@Get("),
	[]byte("@Post("),
	[]byte("@Put("),
	[]byte("@Delete("),
	[]byte("@Patch("),
	[]byte("@Head("),
	[]byte("@Options("),
	[]byte("app."),
	[]byte("router."),
	[]byte("fastify."), // Fastify instance verbs / fastify.route({...})
	[]byte("server."),  // Hapi server.route({...})
	[]byte(".route("),  // object-config + chained route forms
}

var httpJvmMarkers = [][]byte{
	[]byte("Mapping"), // @GetMapping / @PostMapping / @RequestMapping
	[]byte("@Path"),   // JAX-RS
	[]byte("HttpClient"),
	[]byte("RestTemplate"),
	[]byte("WebClient"),
}

// Extract scans src for HTTP route patterns and returns contracts.
// For Go files this lazily parses the source to get the same AST
// enrichment ExtractWithTree provides — keeps Extract() callers
// (notably legacy tests) on parity with the indexer's tree-aware
// path. Other languages skip the parse since BodyFacts only ships
// for Go in phase 1.
func (h *HTTPExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	tree := ParseTreeForLang(detectLanguage(filePath), src)
	defer tree.Release()
	return h.extract(filePath, src, nodes, edges, tree, nil, "")
}

// ExtractWithTree is the tree-aware variant: enrichment uses BodyFacts
// (AST-based) when tree is non-nil and the language has a registered
// factory, falling back to the regex enricher otherwise. Implements
// TreeAwareExtractor.
func (h *HTTPExtractor) ExtractWithTree(
	filePath string,
	src []byte,
	nodes []*graph.Node,
	edges []*graph.Edge,
	tree *parser.ParseTree,
) []Contract {
	return h.extract(filePath, src, nodes, edges, tree, nil, "")
}

// ExtractWithStore is the store-aware variant: in addition to the tree-aware
// enrichment, Go route path arguments are resolved graph-wide through the
// constant store, so a const-referenced or composite-literal path now mints a
// route. Falls back to a lazily-parsed tree when none is supplied (mirrors
// Extract). Implements StoreAwareExtractor.
func (h *HTTPExtractor) ExtractWithStore(
	filePath string,
	src []byte,
	nodes []*graph.Node,
	edges []*graph.Edge,
	tree *parser.ParseTree,
	store EndpointConstStore,
	repoPrefix string,
) []Contract {
	if tree == nil {
		tree = ParseTreeForLang(detectLanguage(filePath), src)
		defer tree.Release()
	}
	return h.extract(filePath, src, nodes, edges, tree, store, repoPrefix)
}

func (h *HTTPExtractor) extract(
	filePath string,
	src []byte,
	nodes []*graph.Node,
	edges []*graph.Edge,
	tree *parser.ParseTree,
	store EndpointConstStore,
	repoPrefix string,
) []Contract {
	lang := detectLanguage(filePath)
	if markers, ok := httpPrefilterMarkers[lang]; ok && !srcHasAnyMarker(src, markers) {
		// A configured client-alias call (e.g. apiGet) carries none of
		// the fetch/axios/app. markers, so the prefilter would skip the
		// file and the alias pass would never run. Keep the file alive
		// when it mentions an alias name; the alias pass below is the
		// only work that will then fire. File-based route files (a Next.js
		// page.tsx, a SvelteKit +server.ts) and React Router modules carry
		// none of those markers either — their route is path/JSX-derived —
		// so keep them alive too.
		if !srcMentionsAnyAlias(src, h.ClientAliases) && !isFileBasedRouteFile(filePath) &&
			!hasReactRouterMarkers(src) && !srcMentionsClientLib(src, httpClientLibraries[lang]) {
			return nil
		}
	}

	text := string(src)
	lines := strings.Split(text, "\n")

	// Pre-sort file nodes by start line for enclosing-function lookup.
	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})

	var out []Contract

	// C# ASP.NET attribute routing needs class context: a controller's
	// class-level [Route("api/[controller]")] prefix and its verb-less
	// [Route("...")] method routes. Scanned once, consumed below.
	var csControllers []csController
	var csVerbless []csVerblessRoute
	if lang == "csharp" {
		csControllers, csVerbless = csharpScanControllerRoutes(lines, fileNodes)
	}

	// File-based routing, Django/DRF/Flask, Rails resources, Express/Fastify
	// object routes — every structural framework route pass — run through the
	// FrameworkRoutePass registry (framework_registry.go) below. React Router
	// routes are JSX/object-config derived.
	if (lang == "typescript" || lang == "javascript") && hasReactRouterMarkers(src) {
		out = append(out, h.extractReactRouterRoutes(filePath, text, lines, fileNodes, lang, tree)...)
	}

	// Go AST-based route detection (Phase 2 of spec-contract-extraction.md).
	// When a parse tree is available, walk it for route registrations
	// instead of running the Go entries in httpPatterns. Structurally
	// distinguishes `[]byte(".GET(")` from `.GET("/users", h)` —
	// eliminates the Fiber self-reflexive bug.
	if lang == "go" && tree != nil && tree.Tree() != nil {
		root := tree.Tree().RootNode()
		matches := detectGoRoutesAST(root, src, filePath, repoPrefix, store)
		for _, rm := range matches {
			c := buildGoRouteContract(rm, filePath, fileNodes, lines, lang, tree, text, src)
			out = append(out, c)
		}
	}

	for _, pat := range httpPatterns {
		if !patternMatchesLang(pat, lang) {
			continue
		}
		// Phase 2: skip the Go PROVIDER regex entries when a tree is
		// available — the AST loop above already handled them. The
		// Go consumer regexes (http.Get / http.Post) still run since
		// the AST detector only covers route registrations, not
		// HTTP client calls. The Go entries stay in httpPatterns
		// for the no-tree code path (the indexer's incremental
		// re-walk can't always get a tree).
		if lang == "go" && tree != nil && tree.Tree() != nil && pat.role == RoleProvider {
			continue
		}
		for _, m := range pat.re.FindAllStringSubmatchIndex(text, -1) {
			lineNum := lineAtOffset(lines, m[0])
			method := pat.method
			path := ""

			if pat.methodGrp > 0 {
				method = strings.ToUpper(text[m[pat.methodGrp*2]:m[pat.methodGrp*2+1]])
			}
			path = text[m[pat.pathGrp*2]:m[pat.pathGrp*2+1]]

			// Consumer literals that point at a filesystem location, a
			// config file, or a static asset are not HTTP API consumers —
			// drop them before they mint a spurious consumer contract. Only
			// rooted "/..." literals are gated; relative and
			// template-interpolated client calls keep their existing
			// behaviour so legitimate dynamic consumers are not lost.
			if pat.role == RoleConsumer && strings.HasPrefix(path, "/") &&
				(!IsLikelyHTTPRouteLiteral(path, "") || IsStaticAssetPath(path)) {
				continue
			}

			// Go's net/http stdlib mux treats a trailing slash as a
			// subtree match — `mux.HandleFunc("POST /v1/tools/", h)`
			// serves every POST under /v1/tools/. Without this fix
			// NormalizeHTTPPath strips the trailing slash and the ID
			// becomes "http::POST::/v1/tools", which never pairs with
			// per-route consumers like `fetch('/v1/tools/${name}')`
			// (normalised to /v1/tools/{p1}). Append a parametric tail
			// so the subtree shape matches parametric consumers in the
			// same workspace. Limit to net/http: gin/echo/chi/fiber
			// treat trailing slash as a distinct literal route, not a
			// subtree.
			subtree := false
			if pat.role == RoleProvider && lang == "go" && pat.framework == "net/http" &&
				len(path) > 1 && strings.HasSuffix(path, "/") {
				path = strings.TrimRight(path, "/") + "/{rest}"
				subtree = true
			}

			if pat.role == RoleProvider && lang == "csharp" {
				path = csharpJoinControllerRoute(path, csControllers, lineNum, csharpActionName(fileNodes, lineNum))
			}
			normPath, origNames := NormalizeHTTPPathWithParams(path)
			contractID := fmt.Sprintf("http::%s::%s", method, normPath)

			symbolID := findEnclosingSymbol(fileNodes, lineNum)

			// Provider patterns that also capture the handler identifier
			// re-point SymbolID at the actual handler function in the
			// same file. Two forms handled:
			//   1. Bare handler:  r.GET("/users", listUsers)
			//      → handlerGrp captures "listUsers", resolve directly.
			//   2. Middleware-wrapped: mux.HandleFunc("POST /x",
			//      WithAuth(auth, h.CreateTuck)) — handlerGrp grabs
			//      "WithAuth" which is a wrapper. Walk forward from
			//      the end of the handlerGrp match, through the rest
			//      of the call's balanced parens, and pick the LAST
			//      identifier (or method reference like h.CreateTuck)
			//      that resolves to a function in this file. That's
			//      the innermost handler — what "trace a request"
			//      actually wants to land on.
			var handlerIdent, handlerTrail, handlerClass string
			if pat.handlerGrp > 0 && pat.role == RoleProvider {
				gStart := m[pat.handlerGrp*2]
				gEnd := m[pat.handlerGrp*2+1]
				if gStart >= 0 && gEnd > gStart {
					handlerName := text[gStart:gEnd]
					handlerIdent = handlerName
					// Capture the trail from the actual handler argument so a
					// later module-wide pass can enumerate handler
					// candidates — the narrow `\w+` regex capture
					// above stops at the first `.` in `h.ServeArchive`
					// and misses wrappers in `WithAuth(h.Foo)`.
					// Balance from the route call's opening paren, but start the
					// returned text at the captured handler token. The preceding
					// method/path literal is not a handler reference; treating its
					// words as names both imports unrelated nodes and can bind a
					// same-named function when the actual handler is unresolved.
					handlerTrail = callTrailSliceFrom(text, m[0], gStart)
					if hID := resolveHandlerIdent(fileNodes, handlerName); hID != "" {
						symbolID = hID
					} else if hID := findInnermostResolvableHandler(fileNodes, handlerTrail); hID != "" {
						symbolID = hID
					}
				}
			}

			// Inline arrow/function route handlers (Express/Fastify/Koa) have
			// no named symbol to bind. The JS/TS extractor materialises a
			// synthetic handler node anchored to the route call line and
			// attributes the handler body's application calls to it; anchor the
			// route Contract's SymbolID to that node so the route connects
			// through the anonymous handler to the services it invokes.
			if symbolID == "" && pat.role == RoleProvider &&
				(lang == "typescript" || lang == "javascript") &&
				strings.Contains(lines[lineNum-1], "=>") {
				symbolID = filePath + "::express-handler@" + strconv.Itoa(lineNum)
			}

			// Backend frameworks (Laravel / Rails / Spring / JAX-RS / ASP.NET)
			// bind the controller action the route dispatches to: same-file for
			// annotation-preceded handlers, and a stamped identifier that the
			// indexer's module-wide pass resolves cross-file for route files
			// wiring controllers in sibling files. Receiver-aware, so the bound
			// action is the named controller's, not a same-named one elsewhere.
			if pat.role == RoleProvider && isBackendHandlerFramework(pat.framework) {
				if sid, hIdent, hClass := bindBackendHandler(pat.framework, lines[lineNum-1], lineNum-1, lines, fileNodes); hIdent != "" {
					if sid != "" {
						symbolID = sid
					}
					handlerIdent = hIdent
					handlerClass = hClass
				}
			}

			meta := map[string]any{
				"method":    method,
				"path":      normPath,
				"framework": pat.framework,
			}
			// Preserve the developer-written parameter names alongside
			// the positional path (e.g. "/v1/sessions/{id}" exposes
			// path_param_names=["id"] while path stays "{p1}"). The
			// dashboard shows these for readability; drift detection
			// uses them to flag mismatches when a provider renames a
			// slot but consumers haven't picked it up.
			if len(origNames) > 0 {
				meta["path_param_names"] = origNames
			}
			// Keep the raw handler identifier + the full call-trail
			// so a later module-wide pass can look handlers up
			// globally when file-scoped resolution failed. The
			// trail carries every candidate (wrappers + inner
			// handler) so we can pick the innermost-resolvable one
			// across repos.
			if handlerIdent != "" {
				meta["handler_ident"] = handlerIdent
			}
			if handlerClass != "" {
				// The declaring controller — lets the module-wide pass pick the
				// receiver-correct action when several controllers share a verb.
				meta["handler_class"] = handlerClass
			}
			if handlerTrail != "" {
				meta["handler_trail"] = handlerTrail
			}
			if subtree {
				meta["subtree"] = true
			}

			c := Contract{
				ID:         contractID,
				Type:       ContractHTTP,
				Role:       pat.role,
				SymbolID:   symbolID,
				FilePath:   filePath,
				Line:       lineNum,
				Meta:       meta,
				Confidence: pat.confidence,
			}

			// Second pass: pull request/response types, query params,
			// and status codes out of the handler body (provider) or
			// the call-site window (consumer). The enricher mutates
			// c.Meta in place and sets "schema_source". When a parse
			// tree is available the AST overlay runs after the regex
			// pass and overrides Meta keys it can confidently produce.
			EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)

			out = append(out, c)
		}
	}

	out = append(out, h.csharpVerblessContracts(filePath, lines, fileNodes, csControllers, csVerbless, lang, tree)...)

	// Structural framework route passes — Django/DRF/Flask, Rails resources,
	// file-based routes, Express/Fastify object forms — run through the
	// FrameworkRoutePass registry. Each pass is language-filtered, has a cheap
	// Detect pre-filter, and is crash-isolated, so one panicking pass does not
	// abort the rest. A new framework registers via RegisterFrameworkRoutePass
	// with no edits here.
	out = append(out, runFrameworkRoutePasses(&RouteExtractCtx{
		FilePath: filePath, Src: src, Text: text, Lines: lines,
		FileNodes: fileNodes, Lang: lang, Tree: tree, H: h,
	})...)

	// Configurable HTTP-client wrapper aliases. Calls to a
	// project-named wrapper (e.g. apiGet('/users')) become consumer
	// contracts even though no built-in fetch/axios pattern matched.
	// Scoped to the TS/JS family — the alias mechanism mirrors the
	// fetch/axios consumer heuristics, which are TS/JS only. Stays
	// hard-wired: its gate reads h.ClientAliases (instance state), not src.
	if len(h.ClientAliases) > 0 && (lang == "typescript" || lang == "javascript") {
		out = append(out, h.detectClientAliasConsumers(filePath, text, lines, fileNodes, lang, tree)...)
	}

	// Import-gated HTTP client-library consumers for the languages whose client
	// surface is bound to a resolved import rather than a call-text substring
	// (python/rust/ruby/php/java/kotlin/scala). Runs after the regex / framework
	// passes so the canonical contract IDs match the provider side.
	out = append(out, h.detectClientLibConsumers(filePath, lang, src, lines, fileNodes, tree, store, repoPrefix)...)

	// Preserve the developer-written path and stamp the per-reference route
	// kind on every HTTP contract.
	for i := range out {
		if out[i].Type == ContractHTTP {
			stampHTTPRouteShape(out[i].Meta)
		}
	}

	return out
}

// RouteKindForPath classifies a normalized route path by its shape: a
// catch-all/subtree route is "wildcard", one with path parameters is
// "parametric", and a fully literal route is "static".
func RouteKindForPath(normPath string) string {
	switch {
	case strings.Contains(normPath, "{rest}") || strings.Contains(normPath, "*"):
		return "wildcard"
	case strings.Contains(normPath, "{"):
		return "parametric"
	default:
		return "static"
	}
}

// OriginalRoutePath reconstructs the developer-written path from a
// positional-normalized path ("/v1/sessions/{p1}") and its captured original
// parameter names (["id"]) → "/v1/sessions/{id}".
func OriginalRoutePath(normPath string, names []string) string {
	out := normPath
	for i, name := range names {
		out = strings.Replace(out, fmt.Sprintf("{p%d}", i+1), "{"+name+"}", 1)
	}
	return out
}

// stampHTTPRouteShape records the developer-facing original_path and the
// per-reference route_kind on an HTTP contract's Meta, derived from its
// normalized path and original parameter names.
func stampHTTPRouteShape(meta map[string]any) {
	if meta == nil {
		return
	}
	normPath, _ := meta["path"].(string)
	if normPath == "" {
		return
	}
	names, _ := meta["path_param_names"].([]string)
	meta["original_path"] = OriginalRoutePath(normPath, names)
	meta["route_kind"] = RouteKindForPath(normPath)
}

// detectLanguage infers the language from a file extension.
func detectLanguage(filePath string) string {
	switch {
	case strings.HasSuffix(filePath, ".go"):
		return "go"
	case strings.HasSuffix(filePath, ".ts"), strings.HasSuffix(filePath, ".tsx"):
		return "typescript"
	case strings.HasSuffix(filePath, ".js"), strings.HasSuffix(filePath, ".jsx"):
		return "javascript"
	case strings.HasSuffix(filePath, ".py"):
		return "python"
	case strings.HasSuffix(filePath, ".java"):
		return "java"
	case strings.HasSuffix(filePath, ".kt"), strings.HasSuffix(filePath, ".kts"):
		return "kotlin"
	case strings.HasSuffix(filePath, ".dart"):
		return "dart"
	case strings.HasSuffix(filePath, ".rs"):
		return "rust"
	case strings.HasSuffix(filePath, ".swift"):
		return "swift"
	case strings.HasSuffix(filePath, ".cs"):
		return "csharp"
	case strings.HasSuffix(filePath, ".rb"):
		return "ruby"
	case strings.HasSuffix(filePath, ".php"):
		return "php"
	case strings.HasSuffix(filePath, ".scala"), strings.HasSuffix(filePath, ".sc"):
		return "scala"
	case strings.HasSuffix(filePath, ".ex"), strings.HasSuffix(filePath, ".exs"):
		return "elixir"
	case strings.HasSuffix(filePath, ".astro"):
		return "astro"
	case strings.HasSuffix(filePath, ".svelte"):
		return "svelte"
	case strings.HasSuffix(filePath, ".vue"):
		return "vue"
	default:
		return ""
	}
}

// patternMatchesLang returns true if the pattern applies to the given language.
func patternMatchesLang(p httpPattern, lang string) bool {
	if len(p.languages) == 0 {
		return true
	}
	for _, l := range p.languages {
		if l == lang {
			return true
		}
	}
	return false
}

// lineAtOffset returns the 1-based line number for the given byte offset.
func lineAtOffset(lines []string, offset int) int {
	pos := 0
	for i, l := range lines {
		end := pos + len(l) + 1 // +1 for newline
		if offset < end {
			return i + 1
		}
		pos = end
	}
	return len(lines)
}

// filterFileNodes returns only nodes that belong to the given file.
func filterFileNodes(filePath string, nodes []*graph.Node) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n.FilePath == filePath {
			out = append(out, n)
		}
	}
	return out
}

// findEnclosingSymbol returns the ID of the nearest function/method that
// encloses the given line number.  Falls back to "" if none found.
//
// Strict containment (StartLine ≤ line ≤ EndLine) is preferred, but some
// language extractors (notably Dart's tree-sitter path) report EndLine as
// the signature line rather than the closing brace, so a call on the very
// next line wouldn't match. When strict containment fails, fall back to
// the closest-preceding symbol whose EndLine ≥ (line - closeProximity) —
// the call is most likely inside its body. "" still means nothing's even
// near enough.
func findEnclosingSymbol(sortedNodes []*graph.Node, line int) string {
	best := ""
	bestStart := 0
	for _, n := range sortedNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine <= line && n.EndLine >= line && n.StartLine >= bestStart {
			best = n.ID
			bestStart = n.StartLine
		}
	}
	if best != "" {
		return best
	}
	// Fallback: the closest function/method whose declaration precedes
	// the line — tolerates off-by-N EndLine reports from extractors that
	// don't compute the closing brace.
	fallback := ""
	fallbackStart := 0
	for _, n := range sortedNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine <= line && n.StartLine > fallbackStart {
			fallback = n.ID
			fallbackStart = n.StartLine
		}
	}
	return fallback
}

// findFunctionByName returns the ID of a function or method declared in the
// same file with the given short name (e.g. "listUsers"). Used by the HTTP
// provider extractor to re-point a contract's SymbolID at its handler
// function when the pattern captures it.
func findFunctionByName(fileNodes []*graph.Node, name string) string {
	for _, n := range fileNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.Name == name {
			return n.ID
		}
	}
	return ""
}

// resolveHandlerIdent resolves a handler identifier captured by a
// provider-pattern regex. Accepts bare "listUsers" (function name)
// and method-expression "h.CreateTuck" (dot-qualified) — the latter
// common when routes are registered on a receiver. The method-name
// after the dot is used for the lookup, so `h.CreateTuck` resolves
// to a method CreateTuck in the same file regardless of receiver
// variable name.
func resolveHandlerIdent(fileNodes []*graph.Node, ident string) string {
	if ident == "" {
		return ""
	}
	if i := strings.LastIndex(ident, "."); i >= 0 {
		ident = ident[i+1:]
	}
	return findFunctionByName(fileNodes, ident)
}

// callTrailSlice returns the byte slice that starts at the HandleFunc
// call's opening "(" (found by the regex at matchStart) and ends at
// the matching balanced close ")". Used to scan past a middleware
// wrapper for an inner handler identifier. Returns empty when the
// call can't be balanced (which only happens on truncated or invalid
// source — production files are fine).
func callTrailSlice(src string, matchStart int) string {
	return callTrailSliceFrom(src, matchStart, 0)
}

// callTrailSliceFrom keeps the complete call boundary while omitting arguments
// before contentStart. The HTTP extractor supplies its handler capture offset;
// other callers retain the full argument slice through callTrailSlice.
func callTrailSliceFrom(src string, matchStart, contentStart int) string {
	// Seek forward from matchStart to the first '(' — that's the
	// opening paren of the HandleFunc call. The regex's m[0] lands
	// at the start of the "HandleFunc" token.
	openIdx := -1
	for i := matchStart; i < len(src); i++ {
		if src[i] == '(' {
			openIdx = i
			break
		}
		if src[i] == '\n' {
			return ""
		}
	}
	if openIdx < 0 {
		return ""
	}
	depth := 0
	i := openIdx
	for i < len(src) {
		switch src[i] {
		case '(':
			depth++
			i++
		case ')':
			depth--
			if depth == 0 {
				start := max(openIdx+1, contentStart)
				if start > i {
					return ""
				}
				return src[start:i]
			}
			i++
		case '"', '\'', '`':
			q := src[i]
			i++
			for i < len(src) && src[i] != q {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				i++
			}
			if i < len(src) {
				i++
			}
		default:
			i++
		}
	}
	return ""
}

// handlerCandidateRE captures every bare identifier or `recv.Method`
// style expression in the call-trail. Tight enough to skip keywords
// like "context" or "nil" only by not resolving them to a file-local
// function — the caller filters via findFunctionByName.
var handlerCandidateRE = regexp.MustCompile(`\b([A-Za-z_]\w*(?:\.\w+)?)\b`)

// HandlerCandidatesInTrail enumerates every identifier / receiver.method
// reference inside a HandleFunc call-trail in source order. The
// indexer's cross-file resolution pass uses this to pick the
// innermost-resolvable handler (last candidate that resolves to a
// real function or method globally).
func HandlerCandidatesInTrail(trail string) []string {
	matches := handlerCandidateRE.FindAllStringSubmatch(trail, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 && m[1] != "" {
			out = append(out, m[1])
		}
	}
	return out
}

// findInnermostResolvableHandler walks the call trail and returns the
// LAST identifier that resolves to a function or method declared in
// the same file. For `WithAuth(auth, h.CreateTuck)` this is
// `h.CreateTuck` (resolves to CreateTuck method); WithAuth and auth
// fail to resolve (not file-local). Returns "" if no candidate
// resolves.
func findInnermostResolvableHandler(fileNodes []*graph.Node, trail string) string {
	matches := handlerCandidateRE.FindAllStringSubmatch(trail, -1)
	var best string
	for _, m := range matches {
		if id := resolveHandlerIdent(fileNodes, m[1]); id != "" {
			best = id
		}
	}
	return best
}
