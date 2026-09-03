package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/semantic"
)

// ServerSpec describes one LSP server's invocation, the languages it
// covers, and the file extensions that route requests to it. Specs are
// the source of truth for both default-config registration (used by
// `gortex daemon` / `gortex mcp` / `gortex server`) and the runtime
// router (which dials a per-extension provider on first touch).
type ServerSpec struct {
	// Name is the unique identifier used in config (e.g. "tsserver",
	// "pyright", "rust-analyzer", "clangd", "jdtls").
	Name string
	// Command is the executable the daemon spawns (e.g. "rust-analyzer",
	// "typescript-language-server", "pyright-langserver").
	Command string
	// Args is the argv tail. Most servers take stdio; servers that need
	// `--stdio` or similar carry it here.
	Args []string
	// Languages is the set of internal language codes this server
	// handles. Used by the manager's per-language priority routing.
	Languages []string
	// Extensions is the set of file extensions (with leading dot) that
	// route to this server. Drives the runtime router.
	Extensions []string
	// LanguageIDs maps an extension to the LSP `languageId` to use in
	// `textDocument/didOpen`. When an extension isn't listed, the first
	// entry of Languages is used as a fallback.
	LanguageIDs map[string]string
	// Priority is the default priority used when no user override
	// exists. Lower wins. Reserved 1-3 for user-tuned overrides; default
	// LSP servers use 5.
	Priority int
	// Daemon hints whether the LSP server is intended to be kept alive
	// across enrich passes. All known LSP servers benefit from this;
	// the field is here for explicit future tuning.
	Daemon bool
	// MaxParallel caps concurrent LSP requests for this server. 10 is a
	// safe starting point — gopls / rust-analyzer happily serve 10
	// in-flight requests without backpressure.
	MaxParallel int
	// AlternativeCommands lists fallback executables to try when the
	// primary `Command` is not on PATH. The first one that resolves
	// wins. Useful for ecosystems with multiple bundled drivers
	// (e.g. `tsserver` from `npm i -g typescript`, vs
	// `typescript-language-server`).
	AlternativeCommands []ServerAlt
	// Env carries extra KEY=VALUE environment entries for the server
	// subprocess, appended to the daemon's own environment. Empty for
	// every built-in spec; populated only when a user overrides a
	// server via .gortex.yaml (e.g. pinning JAVA_HOME for jdtls).
	Env []string
	// InitializationOptions carries server-specific initialization
	// parameters sent in the "initialize" request. For jdtls this
	// includes Maven/Gradle import settings so the server can resolve
	// project dependencies instead of falling back to an "invisible
	// project" with only JRE on the classpath. Nil/empty for servers
	// that don't need it (gopls, rust-analyzer, etc.).
	InitializationOptions json.RawMessage `json:"initializationOptions,omitempty"`
	// Connect, when non-nil, switches this spec from spawn mode to
	// passive-attach: instead of starting a subprocess via Command +
	// Args, Gortex dials the configured network endpoint and uses
	// the resulting net.Conn for JSON-RPC framing. The intended use
	// is IDE-coexistence — share the LSP server the user's editor is
	// already running, rather than spawning a duplicate 200-500 MB
	// subprocess per language.
	Connect *ConnectSpec
	// UseWorkspaceBundleGemfile, when true, makes the router set
	// BUNDLE_GEMFILE to the spawned workspace's own Gemfile (when one
	// exists) in the server's environment. ruby-lsp runs a `bundle install`
	// for a "composed bundle" on spawn unless BUNDLE_GEMFILE is already set;
	// pointing it at the project Gemfile skips that install for enrichment.
	UseWorkspaceBundleGemfile bool
	// NeedsCompileDB marks a server that cannot produce full semantic
	// signal without a compilation database in the workspace. clangd is
	// the case: with no compile_commands.json every didOpen becomes a
	// header-less fallback translation unit — a full preamble + AST
	// rebuild per file — so the hover / hierarchy sweep pays a per-file
	// rebuild for little return, and opening a header directly makes it a
	// standalone TU. When this is set and no database is found, enrichment
	// degrades to reference confirmation: it skips the sweep and header
	// files and warns with the remediation.
	NeedsCompileDB bool
	// DefaultSweepMode overrides the global demand-gated default of the
	// per-file hover / call-hierarchy sweep for this server. Empty keeps
	// the "demand" default; "full" makes the server sweep every file
	// unless the operator sets an explicit sweep mode (config or the
	// GORTEX_LSP_SWEEP env), both of which still win. Set for a server
	// whose net-new edges come mostly from call hierarchy the demand gate
	// skips — rust-analyzer, whose ambiguous receivers resolve through
	// std-library types the graph never indexes, so its recall lives in
	// the full sweep rather than in cheaper static confirmation.
	DefaultSweepMode string
	// NoDidOpen marks a server that answers position requests (hover,
	// references, call hierarchy) for files that were never opened with
	// `textDocument/didOpen`, reading them from its own workspace load
	// instead. For such a server the enrichment pass skips the didOpen /
	// didClose document lifecycle entirely. The point is throughput, not
	// tidiness: a scheduler that runs read-only requests concurrently but
	// treats document notifications as exclusive writes (csharp-ls) drains
	// every in-flight read on each didOpen, so a sweep that interleaves
	// opens with its queries serializes to single-request throughput no
	// matter how many requests it keeps in flight. The
	// GORTEX_LSP_OPEN_DOCS env override wins over this flag both ways.
	NoDidOpen bool
	// NoHeavyRequests marks a server whose FindReferences machinery leaks
	// memory per request until process exit — csharp-ls holds ~10MB and a
	// set of OS handles per textDocument/references round trip and ~0.7MB
	// per callHierarchy/incomingCalls, with nothing released short of a
	// restart, so a full sweep collapses the server long before it
	// completes. For such a server the enrichment pass skips both request
	// classes: ambiguous edges are confirmed through textDocument/definition
	// at their call sites (a clean position request) and dispatch fan-out
	// stays with the graph-side interface-dispatch synthesis. The
	// GORTEX_LSP_HEAVY env override wins over this flag both ways — an
	// operator running a server build without the leak sets it "on" to
	// restore the heavy legs.
	NoHeavyRequests bool
	// CheckoutWorkspaceWeight is how many slots one live per-checkout
	// workspace of this server charges against the checkout workspace budget
	// (semantic's `checkout_lsp_max_workspaces`). Zero and one both mean one
	// slot — the ordinary 200-500MB subprocess the budget is denominated in.
	// A server documented as gigabytes-resident carries
	// heavyCheckoutWorkspaceWeight instead, so the budget bounds approximate
	// memory rather than instance count. Unrelated to NoHeavyRequests, which
	// is about a per-request leak on one code path; a server may carry both,
	// for reasons that have nothing to do with each other.
	CheckoutWorkspaceWeight int
	// ProjectReady, when non-nil, reports whether a workspace has the
	// project setup this server needs to resolve anything at all — e.g.
	// node_modules for tsserver, whose every cross-file / import lookup
	// returns "no package metadata" without it. When it returns
	// (false, remediation) the enrichment pass is SKIPPED entirely (not
	// merely degraded like NeedsCompileDB): such a server spends minutes
	// indexing and hovering to produce zero semantic signal, and the
	// tree-sitter floor still covers those files. This is the general form
	// of NeedsCompileDB for servers whose failure is a missing dependency
	// tree rather than a missing compilation database.
	ProjectReady func(root string) (ready bool, remediation string)
}

// ConnectSpec carries the transport coordinates for passive LSP attach
// — Gortex dials an already-running LSP server (typically the one the
// user's IDE has started) instead of spawning its own. When the dial
// fails and FallbackSpawn is false (the default), Gortex refuses the
// language for that workspace rather than silently spinning up a second
// instance behind the user's back.
type ConnectSpec struct {
	// Network is the dial network — "tcp" or "unix".
	Network string
	// Address is the dial address — host:port for tcp, socket path
	// for unix.
	Address string
	// FallbackSpawn, when true, lets ensureClient fall back to
	// spawning a subprocess (using the spec's Command + Args) if the
	// dial fails after backoff. Default false: if the IDE's server
	// is unreachable, the language is unavailable rather than racing
	// the IDE for a port.
	FallbackSpawn bool
}

// Validate reports any obvious misconfiguration in a ConnectSpec.
// Returns nil for a usable spec; otherwise an error describing the
// missing field. A nil receiver is treated as "not configured" (no
// error — the spec just stays in spawn mode).
func (c *ConnectSpec) Validate() error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.Network) == "" && strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("lsp.connect: network and address are required")
	}
	if strings.TrimSpace(c.Network) == "" {
		return fmt.Errorf("lsp.connect: network is required (tcp or unix)")
	}
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("lsp.connect: address is required")
	}
	switch strings.ToLower(strings.TrimSpace(c.Network)) {
	case "tcp", "tcp4", "tcp6", "unix":
	default:
		return fmt.Errorf("lsp.connect: unsupported network %q (want tcp or unix)", c.Network)
	}
	return nil
}

// ServerAlt is one fallback command form for a server.
type ServerAlt struct {
	Command string
	Args    []string
	// InitializationOptions overrides the spec-level InitializationOptions
	// when this alternative wins on PATH. The spec-level blob is keyed to
	// the primary command, so an alternative from a different vendor
	// (e.g. intelephense standing in for phpactor) can carry its own
	// options. Empty means "inherit the spec's InitializationOptions".
	InitializationOptions json.RawMessage `json:"initializationOptions,omitempty"`
	// InitOptionsFunc, when non-nil, is consulted at initialize time with
	// the resolved workspace root so an alternative can root a per-repo
	// cache path inside Gortex's cache home — a path only known at resolve
	// time, not as a baked literal. It wins over InitializationOptions when
	// it returns a non-empty result.
	InitOptionsFunc func(repoRoot string) json.RawMessage `json:"-"`
}

// Servers is the canonical list of LSP servers Gortex knows how to
// route. Order is meaningful only for the seed config — the runtime
// router uses the per-extension lookup table.
//
// Adding a new server: add the entry here and its file extensions are
// collected into the per-extension table at init time. The default
// config picks it up automatically; users override priority / command
// via .gortex.yaml.
// JdtlsTrustBuildEnv, when set to a truthy value ("1" / "true"), opts jdtls
// into build-backed resolution (Maven/Gradle import + autobuild) for the
// indexed repository. OFF by default: import and autobuild RUN the indexed
// project's own build — Gradle build scripts execute during import, autobuild
// compiles the sources — which is code execution from the indexed repo. Only
// enable it for repositories you trust.
const JdtlsTrustBuildEnv = "GORTEX_LSP_JDTLS_TRUST_BUILD"

// heavyCheckoutWorkspaceWeight is what a language server whose resident set is
// measured in gigabytes charges against the per-checkout workspace budget. The
// budget's unit is one ordinary server — the 200-500MB subprocess most specs
// here spawn — and a Roslyn or Eclipse JDT workspace is an order of magnitude
// past that, so counting it as one workspace is what let four of them add
// ~20GB of language servers to a worktree-heavy day.
//
// Two, rather than the true ratio: the weight bounds the worst case, it does
// not model the footprint. At the shipped budget of four it halves that worst
// case while still admitting one heavy server with two ordinary ones beside
// it. A weight that refused the heavy server outright would cost a Java or C#
// checkout its whole language-server stage, and rationing what a checkout may
// hold is what the budget is for — denying it is not. An operator with the
// memory for a third heavy workspace raises `checkout_lsp_max_workspaces`,
// which buys weighted room rather than instance count.
const heavyCheckoutWorkspaceWeight = 2

// jdtlsSafeInitOptions keeps jdtls in a no-build mode: JRE-only classpath, with
// Maven/Gradle import and autobuild DISABLED. Resolution is more limited (jdtls
// falls back to an "invisible project"), but indexing an untrusted Java repo
// never executes its build. The default; opt into the build variant via
// JdtlsTrustBuildEnv.
const jdtlsSafeInitOptions = `{
	"settings": {
		"java": {
			"import": {
				"maven": {"enabled": false},
				"gradle": {"enabled": false}
			},
			"autobuild": {"enabled": false}
		}
	},
	"bundles": [],
	"workspaceFolders": "auto"
}`

// jdtlsBuildInitOptions enables Maven/Gradle import + autobuild so jdtls
// resolves real project dependencies. WARNING: import and autobuild execute the
// indexed project's build (arbitrary code for Gradle). Sent only when the
// operator has explicitly trusted the indexed repo via JdtlsTrustBuildEnv.
const jdtlsBuildInitOptions = `{
	"settings": {
		"java": {
			"import": {
				"maven": {"enabled": true},
				"gradle": {"enabled": true}
			},
			"autobuild": {"enabled": true}
		}
	},
	"bundles": [],
	"workspaceFolders": "auto"
}`

// effectiveInitializationOptions returns the InitializationOptions to send for
// spec. jdtls is upgraded from its safe (no-build) defaults to build-backed
// options ONLY when the operator opted in via JdtlsTrustBuildEnv; every other
// server gets its spec options unchanged.
func effectiveInitializationOptions(spec *ServerSpec) json.RawMessage {
	if spec == nil {
		return nil
	}
	if spec.Command == "jdtls" && envTruthy(JdtlsTrustBuildEnv) {
		return json.RawMessage(jdtlsBuildInitOptions)
	}
	return spec.InitializationOptions
}

// envTruthy reports whether environment variable name is set to "1" or "true".
func envTruthy(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true")
}

var Servers = []ServerSpec{
	{
		Name:        "gopls",
		Command:     "gopls",
		Args:        []string{"-remote=auto"},
		Languages:   []string{"go"},
		Extensions:  []string{".go"},
		LanguageIDs: map[string]string{".go": "go"},
		Priority:    3,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		Name:      "typescript-language-server",
		Command:   "typescript-language-server",
		Args:      []string{"--stdio"},
		Languages: []string{"typescript", "javascript"},
		Extensions: []string{
			".ts", ".tsx", ".mts", ".cts",
			".js", ".jsx", ".mjs", ".cjs",
		},
		LanguageIDs: map[string]string{
			".ts":  "typescript",
			".tsx": "typescriptreact",
			".mts": "typescript",
			".cts": "typescript",
			".js":  "javascript",
			".jsx": "javascriptreact",
			".mjs": "javascript",
			".cjs": "javascript",
		},
		// Fallback behind tsgo: serves TS/JS wherever the native
		// compiler's binary is not installed. Raise its priority in
		// .gortex.yaml to prefer it over tsgo again.
		Priority:     6,
		Daemon:       true,
		MaxParallel:  10,
		ProjectReady: tsProjectReady,
	},
	{
		// tsgo is the native-Go TypeScript compiler's LSP mode. It is
		// the default TS/JS routing winner when its binary is on PATH:
		// native process, no node/tsserver/typingsInstaller tree, much
		// faster startup. typescript-language-server serves as the
		// fallback when tsgo is not installed.
		Name:      "tsgo",
		Command:   "tsgo",
		Args:      []string{"--lsp", "--stdio"},
		Languages: []string{"typescript", "javascript"},
		Extensions: []string{
			".ts", ".tsx", ".mts", ".cts",
			".js", ".jsx", ".mjs", ".cjs",
		},
		LanguageIDs: map[string]string{
			".ts":  "typescript",
			".tsx": "typescriptreact",
			".mts": "typescript",
			".cts": "typescript",
			".js":  "javascript",
			".jsx": "javascriptreact",
			".mjs": "javascript",
			".cjs": "javascript",
		},
		Priority:     5,
		Daemon:       true,
		MaxParallel:  10,
		ProjectReady: tsProjectReady,
	},
	{
		Name:       "pyright",
		Command:    "pyright-langserver",
		Args:       []string{"--stdio"},
		Languages:  []string{"python"},
		Extensions: []string{".py", ".pyi"},
		LanguageIDs: map[string]string{
			".py":  "python",
			".pyi": "python",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 10,
		AlternativeCommands: []ServerAlt{
			// jedi-language-server / pylsp are common alternates.
			{Command: "jedi-language-server"},
			{Command: "pylsp"},
		},
	},
	{
		// pyrefly is Meta's Rust-based Python type checker (the Pyre
		// successor). Offered at a lower precedence than pyright so the
		// default routing is unchanged for anyone who has both — a user
		// opts in by raising its priority in .gortex.yaml.
		Name:       "pyrefly",
		Command:    "pyrefly",
		Args:       []string{"lsp"},
		Languages:  []string{"python"},
		Extensions: []string{".py", ".pyi"},
		LanguageIDs: map[string]string{
			".py":  "python",
			".pyi": "python",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		Name:       "rust-analyzer",
		Command:    "rust-analyzer",
		Languages:  []string{"rust"},
		Extensions: []string{".rs"},
		LanguageIDs: map[string]string{
			".rs": "rust",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 8,
		// Rust method calls bind overwhelmingly to standard-library
		// receiver types the graph never indexes, so static confirmation
		// leaves rust-analyzer's net-new call-hierarchy edges on the table;
		// its recall lives in the full sweep. Default to it (operator config
		// / GORTEX_LSP_SWEEP still override).
		DefaultSweepMode: sweepModeFull,
	},
	{
		Name:    "clangd",
		Command: "clangd",
		// `--background-index` keeps a project-wide symbol index hot in
		// the daemon, which is essential for cross-file references and
		// type-hierarchy precision in large C++ trees. `--header-insertion=never`
		// avoids tactical edits when we only want graph signal.
		// `--clang-tidy=false` disables lint matchers during enrichment:
		// Gortex consumes semantic graph signal, not clang-tidy diagnostics,
		// and a broad repo `.clang-tidy` can crash clangd mid-pass — which,
		// with reconnect, becomes a crash→reconnect→reindex loop that pins the
		// server at high CPU for the whole pass.
		Args:      []string{"--background-index", "--header-insertion=never", "--clang-tidy=false"},
		Languages: []string{"c", "cpp", "objc", "objcpp"},
		Extensions: []string{
			".c", ".h",
			".cc", ".cpp", ".cxx", ".c++",
			".hh", ".hpp", ".hxx", ".h++",
			".m", ".mm",
		},
		LanguageIDs: map[string]string{
			".c":   "c",
			".h":   "c",
			".cc":  "cpp",
			".cpp": "cpp",
			".cxx": "cpp",
			".c++": "cpp",
			".hh":  "cpp",
			".hpp": "cpp",
			".hxx": "cpp",
			".h++": "cpp",
			".m":   "objective-c",
			".mm":  "objective-cpp",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
		// clangd's hover / call- and type-hierarchy signal needs a
		// compilation database; without one it rebuilds a fallback AST
		// per didOpen, so the enrichment pass degrades to reference
		// confirmation instead of driving that churn.
		NeedsCompileDB: true,
	},
	{
		Name:       "jdtls",
		Command:    "jdtls",
		Languages:  []string{"java"},
		Extensions: []string{".java"},
		LanguageIDs: map[string]string{
			".java": "java",
		},
		Priority:    6, // jdtls is heavyweight; lower priority than scip-java.
		Daemon:      true,
		MaxParallel: 6,
		// An Eclipse JDT workspace on a JVM is gigabytes resident on a real
		// Java tree, so a per-checkout instance of it costs the workspace
		// budget several ordinary servers' worth of slots.
		CheckoutWorkspaceWeight: heavyCheckoutWorkspaceWeight,
		// InitializationOptions for jdtls. SAFE BY DEFAULT (jdtlsSafeInitOptions):
		// no Maven/Gradle import and no autobuild, so indexing an UNTRUSTED Java
		// repo never runs its build. Build-backed resolution — which EXECUTES the
		// repo's own build (Gradle scripts run on import; autobuild compiles) — is
		// opt-in via GORTEX_LSP_JDTLS_TRUST_BUILD; see effectiveInitializationOptions.
		InitializationOptions: json.RawMessage(jdtlsSafeInitOptions),
	},
	{
		Name:       "kotlin-language-server",
		Command:    "kotlin-language-server",
		Languages:  []string{"kotlin"},
		Extensions: []string{".kt", ".kts"},
		LanguageIDs: map[string]string{
			".kt":  "kotlin",
			".kts": "kotlin",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "omnisharp",
		Command:    "omnisharp",
		Args:       []string{"-lsp"},
		Languages:  []string{"csharp"},
		Extensions: []string{".cs"},
		LanguageIDs: map[string]string{
			".cs": "csharp",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
		// Both Roslyn workspaces answer position requests for files that
		// were never didOpen'd, and csharp-ls's request scheduler treats
		// every didOpen / didClose as an exclusive write that drains all
		// in-flight reads — an enrichment pass that interleaves opens
		// serializes to single-request throughput. Skip the lifecycle.
		NoDidOpen: true,
		// csharp-ls leaks per-request on the FindReferences path
		// (references, incomingCalls) until process exit; confirm through
		// definition at call sites instead and leave dispatch fan-out to
		// the interface-dispatch synthesizer.
		NoHeavyRequests: true,
		// A different kind of heavy, and the reason both flags sit here:
		// either Roslyn driver holds the whole solution's compilations in
		// memory and runs gigabytes resident on a large one, so a per-checkout
		// workspace of it costs several budget slots.
		CheckoutWorkspaceWeight: heavyCheckoutWorkspaceWeight,
		// csharp-ls is a Roslyn stdio LSP (`dotnet tool install csharp-ls`)
		// that speaks plain LSP with no args. Current versions discover
		// solutions on their own (recursive .sln/.slnx glob, most-projects
		// heuristic since 0.19.0); gortex still pins the resolved solution
		// at spawn via csharpSolutionArgs for determinism, to name the
		// same file the targeted pre-restore uses, and as the operator
		// override when the server's heuristic picks wrong. It is far more
		// commonly installed than OmniSharp on dev machines, so try it when
		// omnisharp is not on PATH.
		AlternativeCommands: []ServerAlt{
			{Command: "csharp-ls"},
		},
	},
	{
		Name:       "ruby-lsp",
		Command:    "ruby-lsp",
		Languages:  []string{"ruby"},
		Extensions: []string{".rb", ".rake"},
		LanguageIDs: map[string]string{
			".rb":   "ruby",
			".rake": "ruby",
		},
		Priority:                  5,
		Daemon:                    true,
		MaxParallel:               6,
		UseWorkspaceBundleGemfile: true,
		AlternativeCommands: []ServerAlt{
			{Command: "solargraph", Args: []string{"stdio"}},
		},
	},
	{
		Name:       "phpactor",
		Command:    "phpactor",
		Args:       []string{"language-server"},
		Languages:  []string{"php"},
		Extensions: []string{".php"},
		LanguageIDs: map[string]string{
			".php": "php",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
		AlternativeCommands: []ServerAlt{
			// intelephense ships as a Node CLI. Pin its index cache under
			// Gortex's cache home per repo so it doesn't write to its
			// default global location outside the engine's isolation.
			{Command: "intelephense", Args: []string{"--stdio"}, InitOptionsFunc: intelephenseInitOptions},
		},
	},
	{
		Name:       "lua-language-server",
		Command:    "lua-language-server",
		Languages:  []string{"lua"},
		Extensions: []string{".lua"},
		LanguageIDs: map[string]string{
			".lua": "lua",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "sourcekit-lsp",
		Command:    "sourcekit-lsp",
		Languages:  []string{"swift"},
		Extensions: []string{".swift"},
		LanguageIDs: map[string]string{
			".swift": "swift",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "haskell-language-server",
		Command:    "haskell-language-server-wrapper",
		Args:       []string{"--lsp"},
		Languages:  []string{"haskell"},
		Extensions: []string{".hs", ".lhs"},
		LanguageIDs: map[string]string{
			".hs":  "haskell",
			".lhs": "haskell",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "elixir-ls",
		Command:    "elixir-ls",
		Languages:  []string{"elixir"},
		Extensions: []string{".ex", ".exs"},
		LanguageIDs: map[string]string{
			".ex":  "elixir",
			".exs": "elixir",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "ocamllsp",
		Command:    "ocamllsp",
		Languages:  []string{"ocaml"},
		Extensions: []string{".ml", ".mli"},
		LanguageIDs: map[string]string{
			".ml":  "ocaml",
			".mli": "ocaml",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "zls",
		Command:    "zls",
		Languages:  []string{"zig"},
		Extensions: []string{".zig", ".zon"},
		LanguageIDs: map[string]string{
			".zig": "zig",
			".zon": "zig",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "terraform-ls",
		Command:    "terraform-ls",
		Args:       []string{"serve"},
		Languages:  []string{"hcl"},
		Extensions: []string{".tf", ".tfvars"},
		LanguageIDs: map[string]string{
			".tf":     "terraform",
			".tfvars": "terraform-vars",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "yaml-language-server",
		Command:    "yaml-language-server",
		Args:       []string{"--stdio"},
		Languages:  []string{"yaml"},
		Extensions: []string{".yaml", ".yml"},
		LanguageIDs: map[string]string{
			".yaml": "yaml",
			".yml":  "yaml",
		},
		Priority:    7,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "vscode-json-language-server",
		Command:    "vscode-json-language-server",
		Args:       []string{"--stdio"},
		Languages:  []string{"json"},
		Extensions: []string{".json", ".jsonc"},
		LanguageIDs: map[string]string{
			".json":  "json",
			".jsonc": "jsonc",
		},
		Priority:    7,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "bash-language-server",
		Command:    "bash-language-server",
		Args:       []string{"start"},
		Languages:  []string{"bash"},
		Extensions: []string{".sh", ".bash"},
		LanguageIDs: map[string]string{
			".sh":   "shellscript",
			".bash": "shellscript",
		},
		Priority:    7,
		Daemon:      true,
		MaxParallel: 4,
	},
}

// extToSpecs resolves a file extension (with leading dot, lower case)
// to every spec covering it, ordered by Priority (registration order
// breaks ties). Built once at init from `Servers`.
var extToSpecs map[string][]*ServerSpec

// nameToSpec resolves a server name (e.g. "rust-analyzer") to its spec.
var nameToSpec map[string]*ServerSpec

func init() {
	extToSpecs = make(map[string][]*ServerSpec, 64)
	nameToSpec = make(map[string]*ServerSpec, len(Servers))
	for i := range Servers {
		spec := &Servers[i]
		nameToSpec[spec.Name] = spec
		for _, ext := range spec.Extensions {
			lower := strings.ToLower(ext)
			extToSpecs[lower] = append(extToSpecs[lower], spec)
		}
	}
	// Priority orders the candidates per extension; the stable sort
	// keeps registration order among equals (gopls stays authoritative
	// for `.go` even if a future same-priority entry claims it).
	for _, specs := range extToSpecs {
		sort.SliceStable(specs, func(i, j int) bool { return specs[i].Priority < specs[j].Priority })
	}

	// Contribute the per-language checkout workspace weights, so the cap that
	// admits per-checkout language servers charges a gigabytes-resident one
	// more than a 200-500MB one instead of counting both as a workspace.
	semantic.RegisterCheckoutWorkspaceWeights(CheckoutWorkspaceWeights)

	// Contribute every known LSP spec to semantic.DefaultConfig()
	// so the daemon picks them up automatically when the binary is
	// on PATH. Each spec becomes a disabled-only-when-binary-missing
	// ProviderConfig at the spec's default priority.
	semantic.RegisterDefaultProviders(func() []semantic.ProviderConfig {
		out := make([]semantic.ProviderConfig, 0, len(Servers))
		for i := range Servers {
			s := &Servers[i]
			out = append(out, semantic.ProviderConfig{
				Name:        s.Name,
				Command:     s.Command,
				Args:        s.Args,
				Languages:   s.Languages,
				Priority:    s.Priority,
				Enabled:     true,
				Daemon:      s.Daemon,
				MaxParallel: s.MaxParallel,
			})
		}
		return out
	})
}

// CheckoutWorkspaceWeights maps a language code to the slots one live
// per-checkout workspace in that language charges against the checkout
// workspace budget. Only the languages a heavy server covers appear: an absent
// language weighs one, which is the unit the budget counts.
//
// A language several specs cover takes the largest weight among them. Which
// spec a checkout actually spawns depends on what is installed on the machine,
// and admission happens before the spawn — so the budget is charged for the
// heaviest server that could answer for the language rather than the lightest.
func CheckoutWorkspaceWeights() map[string]int {
	out := make(map[string]int, 2)
	for i := range Servers {
		spec := &Servers[i]
		if spec.CheckoutWorkspaceWeight <= 1 {
			continue
		}
		for _, language := range spec.Languages {
			if spec.CheckoutWorkspaceWeight > out[language] {
				out[language] = spec.CheckoutWorkspaceWeight
			}
		}
	}
	return out
}

// SpecForExtension returns the ServerSpec preferred for the given file
// extension (with or without leading dot): the highest-priority spec
// whose binary is actually installed. When several specs cover one
// extension (tsgo / typescript-language-server, pyright / pyrefly) the
// lower Priority number wins among the installed ones — `.ts` routes
// to tsgo when its binary is on PATH and falls back to
// typescript-language-server otherwise. When none of the covering
// servers is installed the priority winner is returned unchanged, so
// the spawn path reports the missing binary as before. Returns nil
// when no spec covers the extension.
func SpecForExtension(ext string) *ServerSpec {
	specs := SpecsForExtension(ext)
	if len(specs) == 0 {
		return nil
	}
	for _, s := range specs {
		if specCommandAvailable(s) {
			return s
		}
	}
	return specs[0]
}

// SpecsForExtension returns every ServerSpec covering the extension in
// priority order (lower Priority number first), without consulting
// binary availability. Callers that hold their own availability source
// (e.g. the Router) walk this list and pick the first live entry.
func SpecsForExtension(ext string) []*ServerSpec {
	if ext == "" {
		return nil
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return extToSpecs[strings.ToLower(ext)]
}

// lookPath is swappable in tests so extension-preference cases can
// simulate which server binaries are installed.
var lookPath = exec.LookPath

var (
	cmdAvailMu    sync.Mutex
	cmdAvailCache map[string]bool
)

// specCommandAvailable reports whether the spec's command (or one of
// its alternatives) resolves on PATH. Passive connect specs count as
// available — they dial instead of spawn. Results are cached for the
// process lifetime, matching Router.specAvailable: installing a server
// mid-daemon takes a restart to notice.
func specCommandAvailable(s *ServerSpec) bool {
	if s == nil {
		return false
	}
	if s.Connect != nil {
		return s.Connect.Validate() == nil
	}
	if commandOnPath(s.Command) {
		return true
	}
	for _, alt := range s.AlternativeCommands {
		if commandOnPath(alt.Command) {
			return true
		}
	}
	return false
}

func commandOnPath(cmd string) bool {
	if cmd == "" {
		return false
	}
	cmdAvailMu.Lock()
	defer cmdAvailMu.Unlock()
	if v, ok := cmdAvailCache[cmd]; ok {
		return v
	}
	if cmdAvailCache == nil {
		cmdAvailCache = make(map[string]bool)
	}
	_, err := lookPath(cmd)
	cmdAvailCache[cmd] = err == nil
	return err == nil
}

// resetCommandAvailabilityCache clears the per-command PATH cache.
// Test-only: pairs with swapping lookPath.
func resetCommandAvailabilityCache() {
	cmdAvailMu.Lock()
	cmdAvailCache = nil
	cmdAvailMu.Unlock()
}

// SpecForPath returns the ServerSpec covering the file's extension.
// Returns nil when no spec covers it.
func SpecForPath(path string) *ServerSpec {
	return SpecForExtension(filepath.Ext(path))
}

// SpecByName returns the ServerSpec with the given name, or nil.
func SpecByName(name string) *ServerSpec {
	return nameToSpec[name]
}

// SpecWithOverrides returns a copy of base with non-empty command /
// args / env overrides applied — the path by which .gortex.yaml tunes
// a heavyweight server (notably pinning a JRE + jdtls launcher args).
// base is never mutated, so the package-global Servers slice stays
// pristine across daemons and tests.
func SpecWithOverrides(base *ServerSpec, command string, args, env []string) *ServerSpec {
	return SpecWithOverridesConnect(base, command, args, env, nil)
}

// SpecWithOverridesConnect is SpecWithOverrides plus an optional
// passive-attach block. When connect is non-nil and validates, the
// returned spec switches from spawn to dial — the Router constructs
// the LSP client by dialing the configured endpoint instead of running
// the spec's Command. A nil connect leaves spawn behaviour untouched.
func SpecWithOverridesConnect(base *ServerSpec, command string, args, env []string, connect *ConnectSpec) *ServerSpec {
	if base == nil {
		return nil
	}
	cp := *base
	if command != "" {
		cp.Command = command
	}
	if len(args) > 0 {
		cp.Args = append([]string(nil), args...)
	}
	if len(env) > 0 {
		cp.Env = append([]string(nil), env...)
	}
	if connect != nil {
		// Shallow-copy so callers can't mutate our cached spec by
		// retaining a handle to their input struct.
		c := *connect
		cp.Connect = &c
	}
	return &cp
}

// LanguageIDForPath returns the LSP `languageId` for the given file
// path using the preferred spec's mapping. Returns an empty string
// when no spec covers the extension.
func LanguageIDForPath(path string) string {
	spec := SpecForPath(path)
	if spec == nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(path))
	if id, ok := spec.LanguageIDs[ext]; ok {
		return id
	}
	if len(spec.Languages) > 0 {
		return spec.Languages[0]
	}
	return ""
}

// AllSpecs returns the canonical list of known specs in registration
// order.
func AllSpecs() []*ServerSpec {
	out := make([]*ServerSpec, 0, len(Servers))
	for i := range Servers {
		out = append(out, &Servers[i])
	}
	return out
}
