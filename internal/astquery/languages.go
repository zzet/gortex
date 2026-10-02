package astquery

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	bashlang "github.com/zzet/gortex/internal/parser/tsitter/bash"
	clang "github.com/zzet/gortex/internal/parser/tsitter/c"
	cpplang "github.com/zzet/gortex/internal/parser/tsitter/cpp"
	csharplang "github.com/zzet/gortex/internal/parser/tsitter/csharp"
	elixirlang "github.com/zzet/gortex/internal/parser/tsitter/elixir"
	golang "github.com/zzet/gortex/internal/parser/tsitter/golang"
	javalang "github.com/zzet/gortex/internal/parser/tsitter/java"
	jslang "github.com/zzet/gortex/internal/parser/tsitter/javascript"
	kotlinlang "github.com/zzet/gortex/internal/parser/tsitter/kotlin"
	mqllang "github.com/zzet/gortex/internal/parser/tsitter/mql"
	phplang "github.com/zzet/gortex/internal/parser/tsitter/php"
	pylang "github.com/zzet/gortex/internal/parser/tsitter/python"
	rubylang "github.com/zzet/gortex/internal/parser/tsitter/ruby"
	rustlang "github.com/zzet/gortex/internal/parser/tsitter/rust"
	scalalang "github.com/zzet/gortex/internal/parser/tsitter/scala"
	tsxlang "github.com/zzet/gortex/internal/parser/tsitter/tsx"
	tslang "github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// DefaultLanguageResolver maps the language strings stored on
// KindFile nodes to their tree-sitter binding. The mapping covers
// every language the bundled detectors reference plus a handful of
// extras for raw-pattern queries. Languages that fall outside this
// list return nil — the engine then skips the matching targets
// (ast-grep behaves identically when its grammar isn't available).
//
// To extend: add the import + entry here. The detector definitions
// reference languages by string, not by binding pointer, so adding a
// language here doesn't require touching detectors.go.
func DefaultLanguageResolver(name string) *sitter.Language {
	switch name {
	case "go":
		return golang.GetLanguage()
	case "python":
		return pylang.GetLanguage()
	case "javascript":
		return jslang.GetLanguage()
	case "typescript":
		// The plain .ts grammar handles JSX-free TypeScript. .tsx
		// targets get retagged as "tsx" upstream so JSX-using
		// detectors compile against the TSX grammar below.
		return tslang.GetLanguage()
	case "tsx":
		// Superset of the TS grammar that exposes JSX nodes
		// (jsx_element, jsx_attribute, …). Picked for .tsx targets
		// so detectors that look for JSX shapes can compile cleanly.
		return tsxlang.GetLanguage()
	case "ruby":
		return rubylang.GetLanguage()
	case "java":
		return javalang.GetLanguage()
	case "kotlin":
		return kotlinlang.GetLanguage()
	case "mql":
		// MQL4 (build 600+) and MQL5 share the tree-sitter-mql5 grammar,
		// which is tree-sitter-cpp plus MQL5 rule extensions — cpp-shaped
		// patterns (call_expression, function_definition, …) apply as-is.
		return mqllang.GetLanguage()
	case "scala":
		return scalalang.GetLanguage()
	case "rust":
		return rustlang.GetLanguage()
	case "elixir":
		return elixirlang.GetLanguage()
	case "php":
		return phplang.GetLanguage()
	case "c":
		return clang.GetLanguage()
	case "cpp", "c++":
		return cpplang.GetLanguage()
	case "csharp", "c#":
		return csharplang.GetLanguage()
	case "bash", "shell", "sh":
		return bashlang.GetLanguage()
	}
	return nil
}
