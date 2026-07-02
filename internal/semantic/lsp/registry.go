package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
}

// Servers is the canonical list of LSP servers Gortex knows how to
// route. Order is meaningful only for the seed config — the runtime
// router uses the per-extension lookup table.
//
// Adding a new server: add the entry here and add its file extensions
// to extToSpec at init time. The default config picks it up
// automatically; users override priority / command via .gortex.yaml.
// JdtlsTrustBuildEnv, when set to a truthy value ("1" / "true"), opts jdtls
// into build-backed resolution (Maven/Gradle import + autobuild) for the
// indexed repository. OFF by default: import and autobuild RUN the indexed
// project's own build — Gradle build scripts execute during import, autobuild
// compiles the sources — which is code execution from the indexed repo. Only
// enable it for repositories you trust.
const JdtlsTrustBuildEnv = "GORTEX_LSP_JDTLS_TRUST_BUILD"

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
		Priority:    5,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		// tsgo is the native-Go TypeScript compiler (the
		// @typescript/native-preview package). Its LSP mode is offered
		// at a lower precedence than typescript-language-server so the
		// default routing is unchanged for anyone who has both — a
		// user opts in by raising its priority in .gortex.yaml.
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
		Priority:    6,
		Daemon:      true,
		MaxParallel: 10,
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
	},
	{
		Name:    "clangd",
		Command: "clangd",
		// Gortex uses clangd for request-driven graph evidence during
		// enrichment, not lint diagnostics or a persistent project index.
		// Keep clang-tidy and background indexing off by default so broad
		// repo .clang-tidy configs and large C++ trees do not dominate CPU
		// or crash-loop the enrichment subprocess. Users can opt back in via
		// a semantic.providers override in .gortex.yaml.
		Args: []string{
			"--background-index=false",
			"--clang-tidy=false",
			"--header-insertion=never",
			"-j=1",
		},
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
			// intelephense ships as a Node CLI.
			{Command: "intelephense", Args: []string{"--stdio"}},
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

// extToSpec resolves a file extension (with leading dot, lower case) to
// its preferred LSP spec. Built once at init from `Servers`.
var extToSpec map[string]*ServerSpec

// nameToSpec resolves a server name (e.g. "rust-analyzer") to its spec.
var nameToSpec map[string]*ServerSpec

func init() {
	extToSpec = make(map[string]*ServerSpec, 64)
	nameToSpec = make(map[string]*ServerSpec, len(Servers))
	for i := range Servers {
		spec := &Servers[i]
		nameToSpec[spec.Name] = spec
		for _, ext := range spec.Extensions {
			lower := strings.ToLower(ext)
			// First registration wins; entries earlier in `Servers`
			// take precedence when extensions overlap. (Used to keep
			// gopls authoritative for `.go` even if a future entry
			// claims it.)
			if _, exists := extToSpec[lower]; !exists {
				extToSpec[lower] = spec
			}
		}
	}

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

// SpecForExtension returns the ServerSpec preferred for the given file
// extension (with or without leading dot). Returns nil when no spec
// covers the extension.
func SpecForExtension(ext string) *ServerSpec {
	if ext == "" {
		return nil
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return extToSpec[strings.ToLower(ext)]
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
