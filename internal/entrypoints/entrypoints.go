// Package entrypoints detects framework entry points — symbols and
// files reachable only from a framework runtime, never from
// application code. Alembic migrations, Pydantic validators, Next.js
// pages / routes, and ASP.NET host files all look unreachable to a
// call-graph walk; left
// unmarked they become dead-code false positives. Detect stamps
// Meta["entry_point"] / Meta["entry_point_kind"] so the dead-code
// analyzer treats them as live roots.
package entrypoints

import (
	"path"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// MetaEntryPoint / MetaEntryKind are the Node.Meta keys this package
// stamps. Exported so the dead-code analyzer can read them.
const (
	MetaEntryPoint = "entry_point"
	MetaEntryKind  = "entry_point_kind"
)

// Detect inspects one file's extracted nodes (and edges, for the
// annotation-driven detectors) for framework entry points and stamps
// the file node plus the relevant symbols. It returns the number of
// nodes stamped.
func Detect(relPath, lang string, nodes []*graph.Node, edges []*graph.Edge) int {
	slashed := path.Clean(strings.ReplaceAll(relPath, "\\", "/"))
	switch lang {
	case "python":
		return detectAlembic(nodes) + detectPydantic(nodes, edges)
	case "typescript", "javascript":
		return detectNextJS(slashed, nodes)
	case "csharp":
		return detectASPNet(slashed, nodes) + detectDotNetFramework(nodes, edges)
	case "java":
		return detectJava(nodes, edges)
	case "rust":
		return detectRust(slashed, nodes, edges)
	}
	return 0
}

// stamp marks a node as a framework entry point, reporting whether it
// was previously unstamped so a detector pair touching one symbol
// counts it once. The later, more specific stamp wins the Meta write.
func stamp(n *graph.Node, kind string) bool {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	first := n.Meta[MetaEntryPoint] != true
	n.Meta[MetaEntryPoint] = true
	n.Meta[MetaEntryKind] = kind
	return first
}

func isFnOrMethod(k graph.NodeKind) bool {
	return k == graph.KindFunction || k == graph.KindMethod
}

// detectAlembic flags an Alembic migration: a Python module defining
// both upgrade() and downgrade() at module level. That pair is the
// distinctive migration signature (Alembic, Flask-Migrate, …) and is
// path-independent — Alembic version directories are configurable.
func detectAlembic(nodes []*graph.Node) int {
	var hasUpgrade, hasDowngrade bool
	for _, n := range nodes {
		switch {
		case n.Kind == graph.KindFunction && n.Name == "upgrade":
			hasUpgrade = true
		case n.Kind == graph.KindFunction && n.Name == "downgrade":
			hasDowngrade = true
		}
	}
	if !hasUpgrade || !hasDowngrade {
		return 0
	}
	count := 0
	for _, n := range nodes {
		if n.Kind == graph.KindFile ||
			(n.Kind == graph.KindFunction && (n.Name == "upgrade" || n.Name == "downgrade")) {
			if stamp(n, "alembic:migration") {
				count++
			}
		}
	}
	return count
}

// pydanticCallbackDecorators are the Pydantic decorators whose target
// method is invoked by the model machinery (validation, serialization)
// rather than by application code, so it never has a written call site.
// `validator` / `root_validator` are the v1 spellings, still accepted by
// v2. `validate_call` is deliberately absent: it wraps a function that
// application code does call.
var pydanticCallbackDecorators = map[string]string{
	"field_validator":  "pydantic:validator",
	"model_validator":  "pydantic:validator",
	"validator":        "pydantic:validator",
	"root_validator":   "pydantic:validator",
	"field_serializer": "pydantic:serializer",
	"model_serializer": "pydantic:serializer",
	"computed_field":   "pydantic:computed_field",
}

const (
	pyAnnoPrefix       = "annotation::python::"
	pyImportPrefix     = "unresolved::import::"
	pydanticModuleRoot = "pydantic"
)

// detectPydantic flags Pydantic validator / serializer / computed-field
// methods from their decorator edges. Like detectJava it stamps only the
// decorated members, never the file: a model module can still hold
// genuinely dead helpers. The file must import pydantic, because
// `validator` alone is too generic a name to trust.
func detectPydantic(nodes []*graph.Node, edges []*graph.Edge) int {
	importsPydantic := false
	for _, e := range edges {
		if e.Kind != graph.EdgeImports {
			continue
		}
		mod, ok := strings.CutPrefix(e.To, pyImportPrefix)
		if ok && (mod == pydanticModuleRoot || strings.HasPrefix(mod, pydanticModuleRoot+".")) {
			importsPydantic = true
			break
		}
	}
	if !importsPydantic {
		return 0
	}

	kinds := map[string]string{} // symbol ID → entry kind
	for _, e := range edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		name, ok := strings.CutPrefix(e.To, pyAnnoPrefix)
		if !ok {
			continue
		}
		// `@pydantic.field_validator` arrives as a dotted name.
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		if kind, ok := pydanticCallbackDecorators[name]; ok {
			kinds[e.From] = kind
		}
	}

	count := 0
	for _, n := range nodes {
		if !isFnOrMethod(n.Kind) {
			continue
		}
		if kind, ok := kinds[n.ID]; ok && stamp(n, kind) {
			count++
		}
	}
	return count
}

// nextAppSpecialFiles are the Next.js App Router special filenames.
var nextAppSpecialFiles = map[string]bool{
	"page": true, "layout": true, "route": true, "loading": true,
	"error": true, "not-found": true, "template": true,
	"default": true, "global-error": true,
}

// nextEntrySymbols are exported functions the Next.js runtime calls
// directly — they have no in-app caller.
var nextEntrySymbols = map[string]bool{
	"getServerSideProps":   true,
	"getStaticProps":       true,
	"getStaticPaths":       true,
	"generateStaticParams": true,
	"generateMetadata":     true,
	"middleware":           true,
}

// detectNextJS flags Next.js pages, API routes, and App Router special
// files. App Router detection keys on the distinctive special
// filenames so a generic `app/` directory does not over-match.
func detectNextJS(relPath string, nodes []*graph.Node) int {
	ext := path.Ext(relPath)
	switch ext {
	case ".js", ".jsx", ".ts", ".tsx":
	default:
		return 0
	}
	segs := strings.Split(relPath, "/")
	base := strings.TrimSuffix(path.Base(relPath), ext)

	kind := ""
	switch {
	case slices.Contains(segs, "app") && nextAppSpecialFiles[base]:
		if base == "route" {
			kind = "nextjs:route"
		} else {
			kind = "nextjs:page"
		}
	case slices.Contains(segs, "pages"):
		if slices.Contains(segs, "api") {
			kind = "nextjs:api"
		} else {
			kind = "nextjs:page"
		}
	default:
		return 0
	}

	count := 0
	for _, n := range nodes {
		if n.Kind == graph.KindFile {
			if stamp(n, kind) {
				count++
			}
			continue
		}
		if isFnOrMethod(n.Kind) && nextEntrySymbols[n.Name] {
			if stamp(n, kind) {
				count++
			}
		}
	}
	return count
}

// detectASPNet flags an ASP.NET host file — Program.cs / Startup.cs —
// and its lifecycle methods (Main / ConfigureServices / Configure),
// which the host invokes, not application code.
func detectASPNet(relPath string, nodes []*graph.Node) int {
	switch path.Base(relPath) {
	case "Program.cs", "Startup.cs":
	default:
		return 0
	}
	count := 0
	for _, n := range nodes {
		switch {
		case n.Kind == graph.KindFile:
			if stamp(n, "aspnet:host") {
				count++
			}
		case isFnOrMethod(n.Kind) &&
			(n.Name == "Main" || n.Name == "ConfigureServices" || n.Name == "Configure"):
			if stamp(n, "aspnet:host") {
				count++
			}
		}
	}
	return count
}

// csharpAnnoPrefix is the synthetic annotation node-ID prefix the C#
// extractor emits (languages.AnnotationNodeID("csharp", name)); the bare
// attribute name is the suffix. C# lets the `Attribute` suffix be elided
// at the use site ([HttpGet] == [HttpGetAttribute]), so matching strips it.
const csharpAnnoPrefix = "annotation::csharp::"

// csharpEntryClassAnnos maps a type-level C# attribute to the entry-point
// kind it confers — the framework instantiates and drives the type, so it
// has no in-application constructor / caller (ASP.NET (Core) controllers).
var csharpEntryClassAnnos = map[string]string{
	"ApiController": "aspnet:controller",
	"Controller":    "aspnet:controller",
}

// csharpEntryMethodAnnos maps a method-level C# attribute to the entry-point
// kind it confers — request handlers the routing layer invokes and test
// methods the runner invokes, neither of which has an in-app caller.
var csharpEntryMethodAnnos = map[string]string{
	"HttpGet":    "aspnet:handler",
	"HttpPost":   "aspnet:handler",
	"HttpPut":    "aspnet:handler",
	"HttpDelete": "aspnet:handler",
	"HttpPatch":  "aspnet:handler",
	"HttpHead":   "aspnet:handler",
	"Route":      "aspnet:handler",
	"Fact":       "xunit:test",
	"Theory":     "xunit:test",
	"Test":       "nunit:test",
	"TestMethod": "mstest:test",
}

// csharpControllerBases are base types whose subclasses ASP.NET drives as
// controllers even without a [Controller] / [ApiController] attribute.
var csharpControllerBases = map[string]bool{
	"Controller":     true,
	"ControllerBase": true,
}

// detectDotNetFramework flags ASP.NET (Core) controllers + action handlers
// and xUnit / NUnit / MSTest methods from the annotation and base-type edges
// the C# extractor emits. Modeled on detectJava: it stamps the individual
// framework-invoked symbols, NOT the file node — a controller file can still
// hold genuinely-dead private helpers, and only the entry symbols should be
// live roots. Edges are per-file (Detect runs during extraction), so a
// type's own EdgeAnnotated / EdgeExtends edges are in the slice.
func detectDotNetFramework(nodes []*graph.Node, edges []*graph.Edge) int {
	annos := map[string]map[string]bool{} // symbol ID → attribute names (suffix-stripped)
	controllerBase := map[string]bool{}   // type ID → extends a controller base
	for _, e := range edges {
		switch e.Kind {
		case graph.EdgeAnnotated:
			name, ok := strings.CutPrefix(e.To, csharpAnnoPrefix)
			if !ok {
				continue
			}
			name = strings.TrimSuffix(name, "Attribute")
			if annos[e.From] == nil {
				annos[e.From] = map[string]bool{}
			}
			annos[e.From][name] = true
		case graph.EdgeExtends:
			if csharpControllerBases[csharpBaseSimpleName(e.To)] {
				controllerBase[e.From] = true
			}
		}
	}

	count := 0
	for _, n := range nodes {
		switch n.Kind {
		case graph.KindType, graph.KindInterface:
			kind := ""
			for a := range annos[n.ID] {
				if k, ok := csharpEntryClassAnnos[a]; ok {
					kind = k
					break
				}
			}
			if kind == "" && controllerBase[n.ID] {
				kind = "aspnet:controller"
			}
			if kind != "" {
				if stamp(n, kind) {
					count++
				}
			}
		case graph.KindFunction, graph.KindMethod:
			for a := range annos[n.ID] {
				if k, ok := csharpEntryMethodAnnos[a]; ok {
					if stamp(n, k) {
						count++
					}
					break
				}
			}
		}
	}
	return count
}

// csharpBaseSimpleName strips the `unresolved::` marker, any ID path, and
// namespace qualification from a base-type edge target, leaving the bare
// type name (`unresolved::Microsoft.AspNetCore.Mvc.ControllerBase` →
// "ControllerBase").
func csharpBaseSimpleName(to string) string {
	to = strings.TrimPrefix(to, "unresolved::")
	if i := strings.LastIndex(to, "::"); i >= 0 {
		to = to[i+2:]
	}
	if i := strings.LastIndex(to, "."); i >= 0 {
		to = to[i+1:]
	}
	return to
}

// javaAnnoPrefix is the synthetic annotation node-ID prefix the Java
// extractor emits (languages.AnnotationNodeID("java", name)); the bare
// annotation name is the suffix.
const javaAnnoPrefix = "annotation::java::"

// javaEntryClassAnnos maps a class/interface-level annotation to the
// entry-point kind it confers. These mark a *type* the framework
// instantiates and drives (Spring stereotypes, JAX-RS resources,
// annotated servlets / websocket endpoints).
var javaEntryClassAnnos = map[string]string{
	"RestController": "spring:controller",
	"Controller":     "spring:controller",
	"Service":        "spring:bean",
	"Component":      "spring:bean",
	"Repository":     "spring:bean",
	"Configuration":  "spring:config",
	"Path":           "jaxrs:resource",
	"WebServlet":     "servlet:endpoint",
	"ServerEndpoint": "websocket:endpoint",
}

// javaEntryMethodAnnos maps a method-level annotation to the entry-point
// kind it confers. These mark a *method* the framework invokes directly
// — request handlers, lifecycle callbacks, scheduled jobs, test cases —
// which therefore has no in-application caller.
var javaEntryMethodAnnos = map[string]string{
	"RequestMapping":    "spring:handler",
	"GetMapping":        "spring:handler",
	"PostMapping":       "spring:handler",
	"PutMapping":        "spring:handler",
	"DeleteMapping":     "spring:handler",
	"PatchMapping":      "spring:handler",
	"EventListener":     "spring:listener",
	"Scheduled":         "spring:scheduled",
	"Bean":              "spring:bean",
	"PostConstruct":     "lifecycle:init",
	"PreDestroy":        "lifecycle:destroy",
	"Test":              "junit:test",
	"ParameterizedTest": "junit:test",
	"RepeatedTest":      "junit:test",
	"BeforeEach":        "junit:fixture",
	"AfterEach":         "junit:fixture",
	"BeforeAll":         "junit:fixture",
	"AfterAll":          "junit:fixture",
	"GET":               "jaxrs:handler",
	"POST":              "jaxrs:handler",
	"PUT":               "jaxrs:handler",
	"DELETE":            "jaxrs:handler",
	"HEAD":              "jaxrs:handler",
}

// detectJava flags Java framework entry points from annotation edges:
// Spring stereotypes / request handlers, JAX-RS resources, annotated
// servlets, JUnit test methods, lifecycle callbacks, and the JVM
// `main` method. Unlike the path-based detectors it stamps the
// individual annotated symbols, NOT the file node — a Spring controller
// file can still hold genuinely-dead private helpers, and only the
// framework-invoked members should be treated as live roots.
func detectJava(nodes []*graph.Node, edges []*graph.Edge) int {
	// symbol ID → set of annotation names applied to it.
	annos := map[string]map[string]bool{}
	for _, e := range edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		name, ok := strings.CutPrefix(e.To, javaAnnoPrefix)
		if !ok {
			continue
		}
		if annos[e.From] == nil {
			annos[e.From] = map[string]bool{}
		}
		annos[e.From][name] = true
	}

	count := 0
	for _, n := range nodes {
		switch n.Kind {
		case graph.KindType, graph.KindInterface:
			for a := range annos[n.ID] {
				if kind, ok := javaEntryClassAnnos[a]; ok {
					if stamp(n, kind) {
						count++
					}
					break
				}
			}
		case graph.KindFunction, graph.KindMethod:
			kind := ""
			for a := range annos[n.ID] {
				if k, ok := javaEntryMethodAnnos[a]; ok {
					kind = k
					break
				}
			}
			if kind == "" && n.Name == "main" {
				kind = "java:main"
			}
			if kind != "" {
				if stamp(n, kind) {
					count++
				}
			}
		}
	}
	return count
}
