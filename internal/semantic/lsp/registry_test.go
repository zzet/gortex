package lsp

import (
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/semantic"
)

// TestSpecForExtension verifies the per-extension lookup table is
// populated for every claimed extension and that case is normalised.
func TestSpecForExtension(t *testing.T) {
	cases := []struct {
		ext      string
		wantName string
	}{
		{".go", "gopls"},
		{".ts", "typescript-language-server"},
		{".tsx", "typescript-language-server"},
		{".py", "pyright"},
		{".pyi", "pyright"},
		{".rs", "rust-analyzer"},
		{".cpp", "clangd"},
		{".HPP", "clangd"}, // case folding
		{".java", "jdtls"},
		{".kt", "kotlin-language-server"},
		{".cs", "omnisharp"},
		{".rb", "ruby-lsp"},
		{".php", "phpactor"},
		{".lua", "lua-language-server"},
		{".swift", "sourcekit-lsp"},
		{".hs", "haskell-language-server"},
		{".ex", "elixir-ls"},
		{".ml", "ocamllsp"},
		{".zig", "zls"},
	}
	for _, c := range cases {
		t.Run(c.ext, func(t *testing.T) {
			s := SpecForExtension(c.ext)
			if s == nil {
				t.Fatalf("no spec for %q", c.ext)
			}
			if s.Name != c.wantName {
				t.Fatalf("ext %q: got %s, want %s", c.ext, s.Name, c.wantName)
			}
		})
	}
	if SpecForExtension(".unknown_ext") != nil {
		t.Fatal("expected nil for unknown ext")
	}
	if SpecForExtension("") != nil {
		t.Fatal("empty ext should not map")
	}
}

// TestSpecForPath verifies path-based routing extracts the extension
// even from messy paths.
func TestSpecForPath(t *testing.T) {
	if s := SpecForPath("/abs/path/to/main.go"); s == nil || s.Name != "gopls" {
		t.Fatalf("unexpected spec for go path: %v", s)
	}
	if s := SpecForPath("README.md"); s != nil {
		t.Fatalf("expected nil spec for markdown, got %v", s)
	}
	if s := SpecForPath("/x.RS"); s == nil || s.Name != "rust-analyzer" {
		t.Fatalf("uppercase rust path: %v", s)
	}
}

// TestLanguageIDForPath checks per-extension languageId resolution.
func TestLanguageIDForPath(t *testing.T) {
	cases := map[string]string{
		"app.tsx":  "typescriptreact",
		"main.ts":  "typescript",
		"a.jsx":    "javascriptreact",
		"foo.py":   "python",
		"pkg/x.rs": "rust",
		"a.cpp":    "cpp",
		"a.h":      "c", // mapping wins
	}
	for path, want := range cases {
		got := LanguageIDForPath(path)
		if got != want {
			t.Errorf("path %q: got %q, want %q", path, got, want)
		}
	}
	if LanguageIDForPath("README.md") != "" {
		t.Fatal("expected empty languageId for unknown ext")
	}
}

// TestSpecByName ensures every entry in Servers is reachable by name.
func TestSpecByName(t *testing.T) {
	for _, want := range Servers {
		got := SpecByName(want.Name)
		if got == nil {
			t.Errorf("SpecByName(%q) = nil", want.Name)
			continue
		}
		if got.Name != want.Name {
			t.Errorf("SpecByName(%q).Name = %q", want.Name, got.Name)
		}
	}
	if SpecByName("nope") != nil {
		t.Fatal("unknown name should return nil")
	}
}

// TestRegistryContributesDefaultProviders proves that initialisation
// adds an entry per spec to the semantic.DefaultConfig provider list.
func TestRegistryContributesDefaultProviders(t *testing.T) {
	cfg := semantic.DefaultConfig()
	names := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		names[p.Name] = true
	}
	for _, s := range Servers {
		if !names[s.Name] {
			t.Errorf("DefaultConfig missing %q", s.Name)
		}
	}
}

// TestExtensionsCoverEverySpec asserts every spec lists at least one
// extension — a spec with no extensions can't be routed to.
func TestExtensionsCoverEverySpec(t *testing.T) {
	for _, s := range Servers {
		if len(s.Extensions) == 0 {
			t.Errorf("spec %q has no extensions", s.Name)
		}
		for _, e := range s.Extensions {
			if !strings.HasPrefix(e, ".") {
				t.Errorf("spec %q ext %q must start with '.'", s.Name, e)
			}
		}
	}
}

// TestPyreflyAndTsgoSpecs verifies the pyrefly and tsgo LSP servers are
// registered as first-class specs without disturbing the default
// Python / TypeScript routing.
func TestPyreflyAndTsgoSpecs(t *testing.T) {
	pyrefly := SpecByName("pyrefly")
	if pyrefly == nil {
		t.Fatal("pyrefly spec not registered")
	}
	if pyrefly.Command != "pyrefly" || len(pyrefly.Args) == 0 {
		t.Errorf("pyrefly invocation: %s %v", pyrefly.Command, pyrefly.Args)
	}
	if len(pyrefly.Languages) == 0 || pyrefly.Languages[0] != "python" {
		t.Errorf("pyrefly should cover python, got %v", pyrefly.Languages)
	}

	tsgo := SpecByName("tsgo")
	if tsgo == nil {
		t.Fatal("tsgo spec not registered")
	}
	if tsgo.Command != "tsgo" || len(tsgo.Args) == 0 {
		t.Errorf("tsgo invocation: %s %v", tsgo.Command, tsgo.Args)
	}

	// Default extension routing must be unchanged — the incumbents
	// register earlier in Servers and keep .py / .ts.
	if s := SpecForExtension(".py"); s == nil || s.Name != "pyright" {
		t.Errorf(".py should still route to pyright, got %v", s)
	}
	if s := SpecForExtension(".ts"); s == nil || s.Name != "typescript-language-server" {
		t.Errorf(".ts should still route to typescript-language-server, got %v", s)
	}

	// Both must be contributed to the default provider list.
	cfg := semantic.DefaultConfig()
	have := map[string]bool{}
	for _, p := range cfg.Providers {
		have[p.Name] = true
	}
	if !have["pyrefly"] || !have["tsgo"] {
		t.Errorf("pyrefly/tsgo missing from DefaultConfig providers")
	}
}

// TestClangdSpecUsesSafeEnrichmentDefaults pins clangd's built-in
// enrichment argv. Gortex needs request-driven graph signal from
// clangd, not repo clang-tidy diagnostics or a persistent background
// project index.
func TestClangdSpecUsesSafeEnrichmentDefaults(t *testing.T) {
	clangd := SpecByName("clangd")
	if clangd == nil {
		t.Fatal("clangd spec not registered")
	}

	want := []string{
		"--background-index=false",
		"--clang-tidy=false",
		"--header-insertion=never",
		"-j=1",
	}
	if got := clangd.Args; !slices.Equal(got, want) {
		t.Fatalf("clangd args = %v, want %v", got, want)
	}
}

// TestSpecWithOverrides verifies .gortex.yaml command / args / env
// overrides are applied to a copy without mutating the built-in spec —
// the path by which jdtls gets a pinned JRE and launcher args.
func TestSpecWithOverrides(t *testing.T) {
	if SpecWithOverrides(nil, "x", nil, nil) != nil {
		t.Error("nil base must return nil")
	}

	base := SpecByName("jdtls")
	if base == nil {
		t.Fatal("jdtls spec missing")
	}

	// No overrides → a copy with inherited values.
	cp := SpecWithOverrides(base, "", nil, nil)
	if cp == base {
		t.Error("must return a copy, not the same pointer")
	}
	if cp.Command != base.Command {
		t.Errorf("command should be inherited, got %q", cp.Command)
	}

	// Overrides applied.
	cp = SpecWithOverrides(base, "/opt/jdtls/bin/jdtls",
		[]string{"--jvm-arg=-Xmx4G"}, []string{"JAVA_HOME=/opt/jdk21"})
	if cp.Command != "/opt/jdtls/bin/jdtls" {
		t.Errorf("command override not applied: %q", cp.Command)
	}
	if len(cp.Args) != 1 || cp.Args[0] != "--jvm-arg=-Xmx4G" {
		t.Errorf("args override not applied: %v", cp.Args)
	}
	if len(cp.Env) != 1 || cp.Env[0] != "JAVA_HOME=/opt/jdk21" {
		t.Errorf("env override not applied: %v", cp.Env)
	}

	// The built-in spec must stay pristine.
	if len(base.Env) != 0 || base.Command != "jdtls" {
		t.Errorf("base spec mutated: command=%q env=%v", base.Command, base.Env)
	}
}
