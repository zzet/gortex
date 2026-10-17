package contracts

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Every existing router fixture keeps its expected contract assertions while
// exercising both public entry points with detached facts.
func joinRouterFixtureFacts(t *testing.T, reg *Registry, files srcMap) {
	t.Helper()
	twin := NewRegistry()
	for _, c := range reg.All() {
		c.Meta = cloneMeta(c.Meta)
		twin.Add(c)
	}
	facts := map[string]RouterFacts{}
	for path, source := range files {
		f := RouterFileFacts(path, []byte(source))
		facts[path] = f
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	var detached map[string]RouterFacts
	if err := json.Unmarshal(encoded, &detached); err != nil {
		t.Fatal(err)
	}
	JoinRouterPrefixes(twin, files.paths(), files.reader())
	JoinRouterPrefixFacts(reg, detached)
	if !reflect.DeepEqual(routerContractSnapshot(t, reg), routerContractSnapshot(t, twin)) {
		t.Fatal("facts join differs from compatibility wrapper")
	}
	after, err := json.Marshal(detached)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(after) {
		t.Fatal("join mutated cached facts")
	}
}

func routerContractSnapshot(t *testing.T, reg *Registry) []string {
	t.Helper()
	var out []string
	for _, c := range reg.All() {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

func TestRouterFactsEligibilityFlipRetainsContractFreeMount(t *testing.T) {
	files := srcMap{"main.ts": "app.use('/api', router);", "routes.ts": "const router = express.Router();"}
	facts := map[string]RouterFacts{}
	for path, source := range files {
		facts[path] = RouterFileFacts(path, []byte(source))
	}
	empty := NewRegistry()
	extractInto(t, empty, files, "repo", "ws")
	if len(empty.All()) != 0 {
		t.Fatal("pre-flip fixture must have zero contracts")
	}
	JoinRouterPrefixFacts(empty, facts)
	mountBefore, _ := json.Marshal(facts["main.ts"])
	// Only the changed route file is extracted again. No callback or original
	// mount source is available to the facts join after the first capture.
	files["routes.ts"] += "\nrouter.get('/users', listUsers);"
	facts["routes.ts"] = RouterFileFacts("routes.ts", []byte(files["routes.ts"]))
	// The independent cold comparison still has all original sources.
	cold := NewRegistry()
	extractInto(t, cold, files, "repo", "ws")
	JoinRouterPrefixes(cold, files.paths(), files.reader())
	delete(files, "main.ts")
	reg := NewRegistry()
	extractInto(t, reg, files, "repo", "ws")
	JoinRouterPrefixFacts(reg, facts)
	if !idSet(reg)["http::GET::/api/users"] {
		t.Fatalf("cached zero-contract mount was lost: %v", idSet(reg))
	}
	mountAfter, _ := json.Marshal(facts["main.ts"])
	if string(mountBefore) != string(mountAfter) {
		t.Fatal("cached mount facts mutated")
	}
	if !reflect.DeepEqual(routerContractSnapshot(t, reg), routerContractSnapshot(t, cold)) {
		t.Fatal("eligibility flip differs from cold")
	}
}

func TestRouterFactsMutableInputIsolation(t *testing.T) {
	// This pins caller-byte mutation isolation, not source backing retention.
	// Retained substring ownership is separately source-reviewed.
	src := []byte("router = APIRouter(prefix='/users')\n@router.get('/{id}')")
	facts := RouterFileFacts("users.py", src)
	before, _ := json.Marshal(facts)
	for i := range src {
		src[i] = 'x'
	}
	after, _ := json.Marshal(facts)
	if string(before) != string(after) {
		t.Fatal("facts retain mutable source input")
	}
}

func TestRouterFactsSparseContexts(t *testing.T) {
	source := strings.Repeat("// unrelated source line\n", 10000)
	f := RouterFileFacts("plain.ts", []byte(source))
	if len(f.Lines) != 0 {
		t.Fatalf("contract-free source retained %d line contexts", len(f.Lines))
	}
}

func TestRouterFactsWrapperReadsUnionOnce(t *testing.T) {
	files := srcMap{
		"main.py":   "app.include_router(router, prefix='/api')",
		"routes.py": "router = APIRouter(prefix='/users')\n@router.get('/{id}')\ndef get_user(): pass",
	}
	reg := NewRegistry()
	extractInto(t, reg, srcMap{"routes.py": files["routes.py"]}, "repo", "ws")
	reads := map[string]int{}
	reader := func(path string) []byte {
		reads[path]++
		if source, ok := files[path]; ok {
			return []byte(source)
		}
		return nil
	}
	JoinRouterPrefixes(nil, []string{"main.py"}, reader)
	if len(reads) != 0 {
		t.Fatal("nil registry read sources")
	}
	JoinRouterPrefixes(reg, []string{"main.py", "missing.py", "main.py", "missing.py"}, reader)
	if !reflect.DeepEqual(reads, map[string]int{"main.py": 1, "routes.py": 1, "missing.py": 1}) {
		t.Fatalf("union reads: %v", reads)
	}
	if !idSet(reg)["http::GET::/api/users/{p1}"] {
		t.Fatalf("contract path omitted from scan was lost: %v", idSet(reg))
	}
	JoinRouterPrefixes(reg, nil, nil) // nil reader remains harmless
}

func TestRouterFactsLineBoundariesAndScopes(t *testing.T) {
	// Literal contexts are independent of the facts parser. Line is the sparse
	// change point serving that physical source line, including blank lines.
	router := map[string]string{"express": "router", "gin/echo/chi": "router", "fiber": "router", "vapor": "router"}
	app := map[string]string{"express": "app", "gin/echo/chi": "app", "fiber": "app", "vapor": "app"}
	cases := []struct {
		name, source string
		want         []RouterLineFacts
	}{
		{"empty", "", []RouterLineFacts{{}}},
		{"single_receiver", "router.get('/x')", []RouterLineFacts{{Line: 1, Receivers: router}}},
		{"trailing_newline", "router.get('/x')\n", []RouterLineFacts{{Line: 1, Receivers: router}, {Line: 2}}},
		{"adjacent_receivers", "router.get('/x')\n// unrelated\napp.post('/y')", []RouterLineFacts{
			{Line: 1, Receivers: router}, {Line: 2}, {Line: 3, Receivers: app},
		}},
		{"multiple_decorators_functions", "@Controller('one')\nclass One {}\n@Get('/a')\n@Controller('two')\nclass Two {}\n@Get('/b')\nfn first() {\nroute()\n}\nfn second() {\nroute()\n}", []RouterLineFacts{
			{Line: 1, NestPrefix: "/one"}, {Line: 1, NestPrefix: "/one"}, {Line: 1, NestPrefix: "/one"},
			{Line: 4, NestPrefix: "/two"}, {Line: 4, NestPrefix: "/two"}, {Line: 4, NestPrefix: "/two"},
			{Line: 7, NestPrefix: "/two", Receivers: map[string]string{"actix": "first"}},
			{Line: 7, NestPrefix: "/two", Receivers: map[string]string{"actix": "first"}},
			{Line: 7, NestPrefix: "/two", Receivers: map[string]string{"actix": "first"}},
			{Line: 10, NestPrefix: "/two", Receivers: map[string]string{"actix": "second"}},
			{Line: 10, NestPrefix: "/two", Receivers: map[string]string{"actix": "second"}},
			{Line: 10, NestPrefix: "/two", Receivers: map[string]string{"actix": "second"}},
		}},
		{"multiline_decorator", "@Controller(\n 'ignored'\n)\nclass X {}\n@Get('/x')", []RouterLineFacts{{}, {}, {}, {}, {}}},
		{"nested_braces", "app.group(\"api\") {\napp.group(\"v1\") {\nv1.get(\"users\")\n}\napp.get(\"other\")\n}\napp.get(\"root\")", []RouterLineFacts{
			{}, {Line: 2, VaporPrefix: "/api"},
			{Line: 3, VaporPrefix: "/api/v1", Receivers: map[string]string{"gin/echo/chi": "v1", "fiber": "v1", "vapor": "v1"}},
			{Line: 4, VaporPrefix: "/api/v1"}, {Line: 5, VaporPrefix: "/api", Receivers: app},
			{Line: 6, VaporPrefix: "/api"}, {Line: 7, Receivers: app},
		}},
		{"nested_rails", "namespace :api do\nnamespace :v1 do\nget '/users'\nend\nget '/other'\nend\nget '/root'", []RouterLineFacts{
			{}, {Line: 2, RailsPrefix: "/api"}, {Line: 3, RailsPrefix: "/api/v1"}, {Line: 3, RailsPrefix: "/api/v1"},
			{Line: 5, RailsPrefix: "/api"}, {Line: 5, RailsPrefix: "/api"}, {Line: 7},
		}},
		{"nested_laravel", "Route::prefix('api')->group(function() {\nRoute::prefix('v1')->group(function() {\nRoute::get('/users');\n});\nRoute::get('/other');\n});\nRoute::get('/root');", []RouterLineFacts{
			{}, {Line: 2, LaravelPrefix: "/api"}, {Line: 3, LaravelPrefix: "/api/v1"}, {Line: 3, LaravelPrefix: "/api/v1"},
			{Line: 5, LaravelPrefix: "/api"}, {Line: 5, LaravelPrefix: "/api"}, {Line: 7},
		}},
		{"spring_exclusive_mapping", "@RequestMapping(\"/one\")\n@GetMapping(\"/a\")\n@RequestMapping(\"/two\")\n@GetMapping(\"/b\")", []RouterLineFacts{
			{}, {Line: 2, SpringPrefix: "/one"}, {Line: 2, SpringPrefix: "/one"}, {Line: 4, SpringPrefix: "/two"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := RouterFileFacts("fixture", []byte(tc.source))
			if f.LineCount != len(tc.want) {
				t.Fatalf("physical line count=%d want=%d", f.LineCount, len(tc.want))
			}
			for i, want := range tc.want {
				got := f.lineFacts(i + 1)
				if len(got.Receivers) == 0 {
					got.Receivers = nil
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("line %d context=%+v want=%+v", i+1, got, want)
				}
			}
			for _, line := range []int{-1, 0, f.LineCount + 1, f.LineCount + 20} {
				if !reflect.DeepEqual(f.lineFacts(line), RouterLineFacts{}) {
					t.Fatalf("invalid line %d returned context", line)
				}
			}
		})
	}
	nilFacts := RouterFileFacts("missing", nil)
	if nilFacts.LineCount != 0 || len(nilFacts.Lines) != 0 {
		t.Fatal("unreadable file retained line contexts")
	}
}

// These expected contexts pin line boundaries directly, without retaining
// a second source parser as an oracle in production or tests.
func TestRouterFactsExpectedLineContexts(t *testing.T) {
	tests := []struct {
		name, path, source, kind string
		want                     []string
	}{
		{"express", "routes.ts", "router.get('/users', list);\nconst unused = 1;", "express", []string{"router", ""}},
		{"python", "routes.py", "@api.get('/users')\ndef users(): pass", "fastapi/flask", []string{"api", ""}},
		{"spring", "Routes.java", "@RequestMapping(\"/api\")\nclass Routes {\n@GetMapping(\"/users\")\n}", "spring", []string{"", "/api", "/api", "/api"}},
		{"nest", "routes.ts", "@Controller('api')\nclass Routes {}\n@Controller('admin')\nclass Admin {}", "nest", []string{"/api", "/api", "/admin", "/admin"}},
		{"rails", "routes.rb", "namespace :api do\nget '/users'\nend\nget '/health'", "rails", []string{"", "/api", "/api", ""}},
		{"laravel", "routes.php", "Route::prefix('api')->group(function () {\nRoute::get('/users', handler);\n});\nRoute::get('/health', handler);", "laravel", []string{"", "/api", "/api", ""}},
		{"vapor", "routes.swift", "app.group(\"api\") { api in\napi.get(\"users\") { req in }\n}\napp.get(\"health\") { req in }", "vapor", []string{"", "/api", "/api", ""}},
		{"actix", "routes.rs", "fn users(cfg: &mut web::ServiceConfig) {\ncfg.route(\"/users\", web::get().to(list));\n}\nfn admin(cfg: &mut web::ServiceConfig) {", "actix", []string{"users", "users", "users", "admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := RouterFileFacts(tt.path, []byte(tt.source))
			for idx, want := range tt.want {
				line := facts.lineFacts(idx + 1)
				got := line.Receivers[tt.kind]
				switch tt.kind {
				case "spring":
					got = line.SpringPrefix
				case "nest":
					got = line.NestPrefix
				case "rails":
					got = line.RailsPrefix
				case "laravel":
					got = line.LaravelPrefix
				case "vapor":
					got = line.VaporPrefix
				}
				if got != want {
					t.Fatalf("line %d context=%q want=%q", idx+1, got, want)
				}
			}
			for _, invalid := range []int{0, facts.LineCount + 1} {
				if got := facts.lineFacts(invalid); !reflect.DeepEqual(got, RouterLineFacts{}) {
					t.Fatalf("invalid line %d has context: %+v", invalid, got)
				}
			}
		})
	}
}
