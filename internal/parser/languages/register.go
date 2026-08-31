package languages

import "github.com/zzet/gortex/internal/parser"

// RegisterAll registers all available language extractors.
func RegisterAll(reg *parser.Registry) {
	reg.Register(NewGoExtractor())
	reg.Register(NewTypeScriptExtractor())
	reg.Register(NewJavaScriptExtractor())
	reg.Register(NewPythonExtractor())
	reg.Register(NewRustExtractor())
	reg.Register(NewJavaExtractor())
	reg.Register(NewRubyExtractor())
	reg.Register(NewElixirExtractor())
	reg.Register(NewCExtractor())
	reg.Register(NewCppExtractor())
	reg.Register(NewHTMLExtractor())
	// Vue single-file components — hand-written depth (carves <script>/<script
	// setup> and delegates to TS/JS). Registered before registerForestLanguages
	// so it claims .vue over the generic forest vue grammar.
	reg.Register(NewVueExtractor())
	// Svelte and Astro components — same carve-and-delegate depth as Vue.
	// Registered before registerForestLanguages so they claim .svelte / .astro
	// over the generic forest grammars.
	reg.Register(NewSvelteExtractor())
	reg.Register(NewAstroExtractor())
	// Razor / Blazor — carve @code blocks to C# + directive type refs.
	// Registered before registerForestLanguages so it claims .razor / .cshtml.
	reg.Register(NewRazorExtractor())
	// Delphi / FireMonkey form definitions (.dfm/.fmx) — object tree + component
	// type and event-handler references.
	reg.Register(NewDFMExtractor())
	reg.Register(NewCSSExtractor())
	reg.Register(NewSQLExtractor())
	reg.Register(NewKotlinExtractor())
	reg.Register(NewSwiftExtractor())
	reg.Register(NewPHPExtractor())
	reg.Register(NewScalaExtractor())
	reg.Register(NewBashExtractor())
	reg.Register(NewProtobufExtractor())
	reg.Register(NewYAMLExtractor())
	reg.Register(NewTOMLExtractor())
	reg.Register(NewHCLExtractor())
	reg.Register(NewDockerfileExtractor())
	reg.Register(NewCSharpExtractor())
	reg.Register(NewXAMLExtractor())
	// .NET solution / project files — build-graph ingestion (.sln
	// project grouping, .csproj/.fsproj/.vbproj ProjectReference +
	// PackageReference). Registered before registerForestLanguages so
	// it owns .csproj/.sln over any generic forest XML grammar.
	reg.Register(NewDotNetProjectExtractor())
	// MyBatis and Spring both share the .xml extension with the generic
	// XML extractor; they are registered before registerForestLanguages
	// (which re-claims .xml for "xml" as the default) and routed only for
	// their respective documents via the content sniff in
	// detect_content.go.
	reg.Register(NewMyBatisExtractor())
	reg.Register(NewSpringContextExtractor())
	reg.Register(NewMarkdownExtractor())
	reg.Register(NewQuartoExtractor())
	// Multimodal assets — image files and PDF documents become graph
	// nodes (DCA10). Registered before registerForestLanguages so they
	// claim their extensions over any generic forest grammar.
	reg.Register(NewImageAssetExtractor())
	reg.Register(NewPDFExtractor())
	// Office and plain-text documents — content assets indexed as streamed
	// KindDoc chunks (one per slide / sheet / windowed section). Registered
	// before registerForestLanguages so they claim their extensions.
	reg.Register(NewPptxExtractor())
	reg.Register(NewXlsxExtractor())
	reg.Register(NewTextExtractor())
	// Pure data / binary assets — recorded as metadata-only file nodes
	// (size + sha) and never parsed.
	reg.Register(NewDataAssetExtractor())
	reg.Register(NewOrgModeExtractor())
	reg.Register(NewDartExtractor())
	reg.Register(NewOCamlExtractor())
	reg.Register(NewLuaExtractor())
	// Luau (Roblox typed Lua) — hand-written depth (typed functions,
	// type aliases, generic params). Registered before
	// registerForestLanguages so it claims .luau over the generic forest
	// luau grammar (which is then skipped on the .luau collision).
	reg.Register(NewLuauExtractor())
	reg.Register(NewZigExtractor())
	reg.Register(NewHaskellExtractor())
	reg.Register(NewClojureExtractor())
	reg.Register(NewErlangExtractor())
	reg.Register(NewRExtractor())
	reg.Register(NewVerseExtractor())
	reg.Register(NewALExtractor())
	reg.Register(NewAutoHotkeyExtractor())
	reg.Register(NewAssemblyExtractor())
	reg.Register(NewGDScriptExtractor())
	// Claims the `project.godot` manifest by exact basename so Godot
	// autoload singletons become globally-nameable types; the bare
	// ".godot" extension stays unclaimed.
	reg.Register(NewGodotProjectExtractor())
	// Godot text resources (.tscn / .tres) — scene tree plus the
	// `ext_resource` references that link a scene to the scripts and
	// sub-scenes it mounts. Registered before registerForestLanguages so it
	// claims those extensions over the signature-only forest
	// `godot_resource` grammar, which emits no edges. `.godot` is left
	// unclaimed here too — GodotProjectExtractor owns the one file that
	// matters by basename, and claiming the extension would drag in the
	// engine's `.godot/` cache directory.
	reg.Register(NewGodotSceneExtractor())
	reg.Register(NewNixExtractor())
	reg.Register(NewFortranExtractor())
	reg.Register(NewSolidityExtractor())
	reg.Register(NewFSharpExtractor())
	reg.Register(NewJuliaExtractor())
	reg.Register(NewTclExtractor())
	reg.Register(NewShaderExtractor())
	reg.Register(NewPerlExtractor())
	reg.Register(NewRakuExtractor())
	reg.Register(NewCrystalExtractor())
	reg.Register(NewNimExtractor())
	reg.Register(NewPascalExtractor())
	reg.Register(NewCobolExtractor())
	reg.Register(NewJCLExtractor())
	reg.Register(NewAdaExtractor())
	reg.Register(NewPowerShellExtractor())
	reg.Register(NewVimExtractor())
	reg.Register(NewEmacsLispExtractor())
	reg.Register(NewRacketExtractor())

	// Template engines
	reg.Register(NewBladeExtractor())
	reg.Register(NewEJSExtractor())
	reg.Register(NewHandlebarsExtractor())
	reg.Register(NewJinjaExtractor())
	reg.Register(NewTwigExtractor())
	reg.Register(NewERBExtractor())
	reg.Register(NewLiquidExtractor())
	reg.Register(NewPugExtractor())

	// Build / shell
	reg.Register(NewMakefileExtractor())
	reg.Register(NewCMakeExtractor())
	reg.Register(NewBatchExtractor())

	// Blockchain / smart-contract
	reg.Register(NewMoveExtractor())
	reg.Register(NewCairoExtractor())
	reg.Register(NewNoirExtractor())
	reg.Register(NewTactExtractor())
	reg.Register(NewBallerinaExtractor())

	// Scientific / enterprise
	reg.Register(NewApexExtractor())
	reg.Register(NewABAPExtractor())
	// ABA/Sabre QIK (.qik) — mid-office script dialect (call/label/goto/
	// build_local_data_item). Regex only; claims .qik exclusively.
	reg.Register(NewQikBasicExtractor())
	reg.Register(NewMatlabExtractor())
	reg.Register(NewMathematicaExtractor())
	reg.Register(NewSASExtractor())
	reg.Register(NewStataExtractor())

	// Emerging
	reg.Register(NewMojoExtractor())
	reg.Register(NewOdinExtractor())
	reg.Register(NewVlangExtractor())
	reg.Register(NewHareExtractor())
	reg.Register(NewCarbonExtractor())
	reg.Register(NewReScriptExtractor())
	reg.Register(NewGleamExtractor())

	// Legacy / JVM / data
	reg.Register(NewCoffeeScriptExtractor())
	reg.Register(NewActionScriptExtractor())
	reg.Register(NewDExtractor())
	reg.Register(NewValaExtractor())
	reg.Register(NewGroovyExtractor())
	// MCP server config files (.mcp.json / mcp.json /
	// claude_desktop_config.json) share the .json extension with the
	// generic JSON extractor. Registered before NewJSONExtractor so its
	// basename / compound-extension claims win; it never claims the bare
	// .json extension, so package.json / tsconfig.json still route to the
	// JSON extractor.
	reg.Register(NewMCPConfigExtractor())
	// Shopify OS 2.0 JSON templates / section groups share .json with the
	// generic JSON extractor; sniff-routed only (never claims the bare
	// extension), so non-Shopify .json still routes to the JSON extractor.
	reg.Register(NewLiquidJSONExtractor())
	reg.Register(NewJSONExtractor())

	// Notebooks
	reg.Register(NewJupyterExtractor())

	// Forest-backed (alexaandru/go-sitter-forest) — signature-only.
	// Each registration adds a brand-new language not covered by the
	// hand-written extractors above. The forest framework reads the
	// grammar's bundled tags.scm when present and falls back to a
	// generic node-kind walker otherwise.
	reg.Register(NewElmExtractor())
	// Helm semantic layer (named templates, include/template calls,
	// Chart.yaml chart + dependency graph). Registered before
	// registerForestLanguages so the hand-written extractor claims
	// `.tpl` / `.gotmpl` over the generic forest gotmpl grammar (which
	// is then skipped on those extensions). `Chart.yaml` is a basename
	// claim that wins over the YAML extractor's `.yaml` extension since
	// basenames are resolved first.
	reg.Register(NewHelmExtractor())
	registerForestLanguages(reg)

	// ObjC registered last so it wins the `.m` extension over Matlab.
	reg.Register(NewObjCExtractor())
}

// TemporalInvokerConfigurable is implemented by extractors that accept a per-repo
// Temporal invoker allow-list. Only the Java extractor implements it today.
//
// NOTE: there is currently no production wiring that feeds this — main has no
// per-repo temporal config surface — so the invoker detector is reachable only
// via ConfigureTemporalJavaInvokers (tests / programmatic callers). Production
// wiring awaits a temporal-config surface on main; until then detection stays
// OFF by default (empty invoker set).
type TemporalInvokerConfigurable interface {
	SetTemporalInvokers(invokers, methods []string)
}

// ConfigureTemporalJavaInvokers installs the Java Temporal invoker config onto
// the Java extractor. No-op when invokers is empty, so the detector stays OFF
// by default; callers pass the configured list unconditionally.
func ConfigureTemporalJavaInvokers(reg *parser.Registry, invokers, methods []string) {
	if len(invokers) == 0 {
		return
	}
	if ext, ok := reg.GetByLanguage("java"); ok {
		if c, ok := ext.(TemporalInvokerConfigurable); ok {
			c.SetTemporalInvokers(invokers, methods)
		}
	}
}
