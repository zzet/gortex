# Supported Languages

Gortex currently indexes **256 languages**. Each language has an extractor that
walks the source, emits symbols (functions, methods, types, interfaces,
variables) into the graph, and records `imports` / `calls` edges.

Three engine tiers are used, in order of decreasing extraction depth:

- **bespoke tree-sitter** (~30 languages) — full concrete syntax tree via a
  vendored grammar with hand-tuned S-expression queries. Produces high-fidelity
  symbols, resolved call edges, ORM/contract/dataflow extraction, and accurate
  node ranges. Languages: Go, TypeScript, JavaScript, Python, Rust, Java, C#,
  Kotlin, Swift, Scala, PHP, Ruby, Elixir, C, C++, Dart, OCaml, Lua, Bash, SQL,
  HTML, CSS, Markdown, OrgMode, Protobuf, YAML, TOML, HCL, Dockerfile, Julia.
- **regex** (~60 languages) — pattern-matched line scanning with indent / brace /
  keyword block heuristics. Captures top-level symbols and imports; call edges
  vary per language. Used where no upstream tree-sitter grammar is available
  (Verse, AL, SAS, Stata, AutoHotkey, CoffeeScript) or for legacy / niche
  languages where the regex path was sufficient (ABAP, COBOL, Fortran, …).
- **forest signature-only** (~165 languages) — generic `*ts.Language`-driven
  extractor wrapping `github.com/alexaandru/go-sitter-forest`. Reads the
  grammar's bundled `tags.scm` (nvim-treesitter convention) when present and
  falls back to a node-kind heuristic walker otherwise. Emits definitions
  (function / method / type / interface / variable / constant / module) plus
  `EdgeDefines` from the file. `@reference.call` / `@reference.function`
  captures route to the enclosing function. **No** ORM / contract / dataflow /
  scope-aware resolution — graduate a language to the bespoke tier when those
  matter.

For sixteen of these languages an LSP server can additionally upgrade
edges from `ast_inferred` to `lsp_resolved` and unlock the
on-demand action tools (`get_diagnostics`, `get_code_actions`,
`apply_code_action`, `fix_all_in_file`). See **[lsp.md](lsp.md)** for the
server matrix, install commands, lifecycle knobs, and config schema.

## At a glance

| Category | Count | Languages |
|---|---|---|
| Core programming | 10 | Go, TypeScript, JavaScript, Python, Rust, Java, C#, C, C++, Kotlin |
| JVM, .NET, systems | 10 | Scala, Swift, PHP, Ruby, Groovy, F#, D, Zig, Vala, Objective-C |
| Scripting & shell | 10 | Bash, PowerShell, Batch, Perl, Raku, Lua, Tcl, VimScript, AutoHotkey, CoffeeScript |
| Functional | 8 | Haskell, OCaml, Elixir, Clojure, Erlang, Racket, Gleam, Emacs Lisp |
| Systems / emerging | 8 | Nim, Crystal, Mojo, Odin, V, Hare, Carbon, ReScript |
| Scientific & enterprise | 12 | Julia, R, MATLAB, Mathematica, SAS, Stata, Fortran, COBOL, Ada, Pascal, ABAP, Apex |
| Mobile & game | 4 | Dart, GDScript, Verse, ActionScript |
| Blockchain / smart contracts | 6 | Solidity, Move, Cairo, Noir, Tact, Ballerina |
| Template engines | 8 | Blade, EJS, Handlebars, Jinja, Twig, ERB, Liquid, Pug |
| Data, config, build | 12 | JSON, YAML, TOML, HCL/Terraform, SQL, Protobuf, Markdown, HTML, CSS, Dockerfile, Makefile, CMake |
| Niche / domain | 4 | Nix, AL (Business Central), Assembly (NASM/GAS/ARM/WLA-DX/CA65), Shaders (GLSL/HLSL) |
| Forest — frontend / templates | ~16 | Vue, Svelte, Astro, htmldjango, gotmpl, Haml, Slim, Glimmer, Razor, Templ, Tera, Mustache, Vento, SuperHTML, HEEx |
| Forest — schemas / IaC / IDLs | ~25 | GraphQL, Prisma, Jsonnet, Dhall, CUE, Pkl, Nickel, KCL, Bicep, Smithy, Cap'n Proto, Thrift, KDL, RON, TypeSpec, DBML, HJSON, HOCON, INI, JSON5, JSONC, Properties, SCFG, YANG, XML, DTD, EditorConfig, dotenv, Desktop, Devicetree, Kconfig, Linker script |
| Forest — shaders / hardware | ~14 | WGSL, GLSL, HLSL, CUDA, ISPC, VHDL, SystemVerilog, MLIR, LLVM, Jasmin, QBE, FIRRTL, PIO ASM, GDShader |
| Forest — docs / typesetting | 8 | LaTeX, Typst, AsciiDoc, Djot, Mermaid, Norg, BibTeX, PlantUML |
| Forest — functional / niche | ~26 | Agda, Idris, PureScript, Roc, Gren, Elm, Fennel, Janet, Hack, Haxe, Pony, C3, Aiken, Effekt, Eiffel, Jule, Koka, Luau, MoonBit, Motoko, Ralph, Scheme, SML, Wing, Common Lisp |
| Forest — build / DSL / testing | ~16 | Meson, Just, Beancount, Ledger, Gherkin, Hurl, Robot, Earthfile, Ninja, BitBake, Caddy, Snakemake, GN, Cooklang, Requirements, Cedar, CEL, Circom, Clarity, Rego, TLA+, Quint, Structurizr, GritQL, QL |
| Forest — DB / query | 8 | SPARQL, SurrealQL, PromQL, Kusto, SOQL, SOSL, PRQL, Turtle |
| Forest — data / lockfiles / shells / configs | ~28 | TSV, PSV, textproto, .po, PGN, todo.txt, go.mod / go.sum / go.work, Fish, Nushell, jq, Awk, Elvish, gitconfig / gitattributes / gitcommit / gitignore, Hyprlang, nftables, passwd, PEM, PoE filter, Puppet, ssh_config, sxhkdrc, tmux |
| Forest — misc | ~14 | DOT, gnuplot, GPG, Strace, VRL, Zeek, Ziggy + Schema, Starlark, SourcePawn, SCSS, RBS, OCamllex, DataWeave, USD, WIT |
| **Total** | **256** | |

## Core programming — deep extraction

Tree-sitter-backed languages with the most thorough extraction. `Meta["methods"]`
on interface nodes stores the expected method set for implementation matching.

| Language | Functions | Methods + MemberOf | Types | Interfaces | Imports | Calls | Variables |
|----------|-----------|-------------------|-------|------------|---------|-------|-----------|
| Go | Full | Full (receiver) | Full | Full + Meta["methods"] | Full | Full | Full |
| TypeScript | Full | Full | Full | Full + Meta["methods"] | Full | Full | Full |
| JavaScript | Full | Full | Full | - | Full | Full | Full |
| Python | Full | Full | Full | - | Full | Full | Partial |
| Rust | Full (+ `extern` blocks) | Full (impl blocks) | Structs/Enums/Unions/Aliases | Full + Meta["methods"] | Full (+ `extern crate`) | Full (+ macro bodies) | Consts/Statics |
| Java | Full | Full | Full | Full + Meta["methods"] | Full | Full | Fields |
| C# | Full | Full | Full | Full + Meta["methods"] | Full | Full | Fields |
| Kotlin | Full | Full | Full | Full | Full | Full | Properties |
| Scala | Full | Full | Full | Full + Meta["methods"] | Full | Full | - |
| Swift | Full | Full | Full | Full + Meta["methods"] | Full | Full | - |
| PHP | Full | Full | Full | Full | Full | Full | - |
| Ruby | Full | Full | Full | - | Full | Full | Constants |
| Elixir | Full | Full (defmodule) | Modules | - | Full | Full | Attributes |
| C | Full | - | Structs/Enums | - | Full | Full | Globals |
| C++ | Full | Full | Classes/Structs | - | Full | Full | - |
| Dart | Full | Full | Classes/Enums/Mixins/Extensions | Abstract interface | Full | Full | Full |
| OCaml | Full | Full (class) | Types/Modules | Module types | open | Full | Full |
| Lua | Full | Full (M.func/M:method) | - | - | require() | Full | Full |
| Julia | Full (long + short form, `where` syntax) | Full (qualified `Base.show`, operators) | Structs/abstract/primitive + fields | - | Full (`using`/`import`/`include`, selective lists incl. macros/operators) | Full (incl. broadcast, macro calls, `Vector{Int}` constructors) | `const` (with `member_of`) |

### Rust specifics

`mod` is a namespace rather than a dependency, so it indexes as a package node
(the C++/C# namespace precedent) and items inside it carry `Meta["scope_mod"]`
while keeping flat ids. Two same-named types in one file both survive via the
shared id-disambiguation helper.

Rust does not parse macro arguments as expressions — every `macro_invocation`
holds an opaque token tree — so calls written inside one are recovered by a
token scan and labelled `Meta["via_macro"]`; filter on it to keep only
grammar-certain call edges. Pattern and cfg-predicate macros (`matches!`,
`cfg!`) are excluded, since their bodies hold patterns rather than calls. This
matters most for tests, where the call a `#[test]` exercises normally sits
inside an `assert*!`.

Entry points are stamped for `fn main` in a Cargo binary target, async-runtime
mains (`#[tokio::main]` and friends), test/bench harness attributes, and FFI
exports (`#[no_mangle]`, `#[wasm_bindgen]`, pyo3, napi). Declarations inside an
`extern` block are stamped `rust:ffi-import` — their body lives in another
language, so a missing definition is not a missing implementation.

Recent extraction refinements (each covered by a per-feature CI golden): Java `@interface` annotation types index as interfaces and participate in implementation matching; Java `new T(){…}` and C# `new { … }` anonymous classes/types become synthetic type nodes with an `extends` edge (to the instantiated type, or to `object` for C#); JS/TS arrow-valued class fields (`x = () => {…}`) are emitted as callable methods; JS/TS named imports emit one `imports` edge per binding (alias-aware via `Edge.Alias`) and barrel re-exports emit `re_exports` edges; JS/TS cross-file imports resolve onto the target file/symbol for relative specifiers (`./x`, `../x`) and for `tsconfig.json` / `jsconfig.json` `compilerOptions.paths` / `baseUrl` path aliases (`@/lib/x`), so callers / usages / blast-radius span aliased imports the same as relative ones; chained / factory-call receivers (`New().Build()`) carry an inferred `receiver_type`. See [features.md](features.md) and [architecture.md](architecture.md) for the edge semantics.

### Julia specifics

`module` / `baremodule` index as `KindType` nodes (the graph's `KindModule`
is reserved for ecosystem packages) and carry the module's `export` list in
`Meta["exports"]` — including exported macros and operators, recorded
verbatim (`export @m, ⊗` records `@m` and `⊗`) — and the Julia 1.11
`public` list in `Meta["public"]` (public-without-reexport, operators
included); definitions, constants and
nested modules inside a module get `member_of` edges to it.
Node ids stay flat — the enclosing module rides on `Meta["scope_mod"]`, the
Rust `mod` convention — and two definitions that would collide on one id
(`f` in module `A` and `f` in module `B`) separate through the shared
line-suffix helper. Qualified method definitions (`function Base.show(...)`,
`function Base.:+`) become `KindMethod` with `Meta["receiver"]`, mirroring
the Lua `M.func` convention. Julia has no constructor keyword, so a callable
named after a type is that type's constructor and takes the cross-language
`<Type>.<init>` spelling Java, C# and Swift already emit — outer, short and
inner forms alike. Struct fields are `KindField` nodes with `member_of` into
the struct; supertypes (`struct X <: Y`) emit `extends` edges with the full
right-hand path in `Meta["base_path"]` (python extractor convention).

`using M: a, b` / `import M as Alias` keep the module as the import target,
including when the path is dotted or relative (`using A.B: x`, `import
..Up: q`). A selective list additionally emits one edge per binding to
`unresolved::import::<module>::<name>` — the per-binding shape JS/TS
already uses, bounded by the same cap so one statement cannot dwarf a
file's graph — and a rename rides on `Edge.Alias` plus `Meta["alias"]`,
because the SQLite edges table has no alias column and Meta is the half
that survives the round-trip. A module alias is applied at extraction time, so `import Foo as F`
followed by `F.process(x)` records a call to `Foo.process`: the same target
the unaliased spelling produces. Nothing in the resolver reads the import
`Meta` — the edge targets are the consumable surface. All imports including
`include("file.jl")` target `unresolved::import::<path>`.
Calls attribute to the enclosing function-like definition (long form, short
form, macro, or nested closure) and cover qualified (`Mod.f`),
parametric-constructor (`Vector{Int}(xs)` → `unresolved::Vector{Int}`), and
broadcast (`f.(x)`, `Meta["broadcast"]`) callees, plus bare and
module-qualified macro invocations (`Base.@time` → `unresolved::Base.time`,
`Meta["macro"]`). A callee chained onto a call result (`get(cfg).run(x)`)
has no decodable receiver and records its method name (`unresolved::run`);
quoted operators normalise (`Base.:+` and `Base.:(==)` →
`unresolved::Base.+` / `unresolved::Base.==`). Docstrings attach as
`Meta["doc"]` to long and short definitions, types, modules and constants,
but only when the string sits immediately above the documented object —
the adjacency Julia itself enforces, where a blank line or an own-line
comment detaches the string and leaves the definition undocumented. A
string at the top of a function body is executable code, not
documentation. The explicit `@doc "text" object` form (which is what
Julia lowers every docstring to) attaches the same way, with the text
taken from inside the macro call. The stored text is the first PROSE
paragraph, skipping the
indented signature block Julia's convention opens a docstring with.

What is **not** covered:

- **Calls inside anonymous functions and do-blocks**
  (`double = x -> f(x)`, `map(xs) do y g(y) end`) attribute to the
  ENCLOSING function — source locality outranks a closure node for the
  graph's consumers, so the closure itself mints no node. A short-form
  definition nested in a block (`nested() = 1`) is still its own node.
- **`@enum` members** are not extracted — the macro generates the enum
  type and its member constants at runtime.
- **`@.`** records no macro edge (a target named `.` is meaningless);
  calls inside its arguments edge normally.
- **Operator calls in infix position** (`a + b`) — dispatch on operators is
  not statically attributable. Explicit call syntax (`Base.:+(a, b)`) is a
  normal call.
- **Typed constant declarations** (`const X::Int = 1`) mint no variable node.
- **Parametric constructor definitions** (`Box{T}(x) where T = …`) are
  extracted as plain functions named `Box{T}` — not with the
  `<Type>.<init>` constructor spelling, and not bound to `Box`.
- **Callable-object definitions** (`(f::Box)(x) = …`) are not extracted.
- **Calls inside string interpolation** (`"$(f(x))"`) are not walked.
- **Two methods of one name on one physical line**
  (`g(x) = h(x); g(y) = k(y)`) collapse onto one node — line numbers cannot
  separate them — though each body's call edges are preserved.
- **Call and macro edges are extraction-side facts**: targets are
  `unresolved::` names, and binding them to definitions in another file —
  including qualified calls into another file's module, and constructor
  call sites to `<Type>.<init>` — is resolver work, not attempted here.

## Data, config, build

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| JSON | `.json`, `.json5`, `.jsonc` | Top-level keys |
| YAML | `.yaml`, `.yml` | Top-level keys |
| TOML | `.toml` | Tables, key-value pairs |
| HCL / Terraform | `.tf`, `.tfvars`, `.hcl` | Resource / data / module / variable / output blocks |
| SQL | `.sql` | Tables (with columns), views, functions, indexes, triggers |
| Protobuf | `.proto` | Messages (with fields), services + RPCs, enums, imports |
| Markdown | `.md` | Headings, local file links, code-block languages |
| HTML | `.html`, `.htm` | Script / link references, element IDs |
| CSS | `.css` | Class selectors, ID selectors, custom properties, `@import` |
| Dockerfile | `Dockerfile`, `Containerfile`, `Dockerfile.<suffix>`, `.dockerfile` | `FROM` (base images), `ENV` / `ARG` variables |
| Makefile | `Makefile`, `GNUmakefile`, `.mk`, `.make` | Targets, `define…endef`, `VAR = …`, `include` / `-include` |
| CMake | `CMakeLists.txt`, `.cmake` | `function(…)`, `macro(…)`, `add_library`, `add_executable`, `include(…)`, `set(…)` |

### SQL coverage boundary

A `.sql` file is read by two independent passes, and knowing which one
produced a symbol explains what you can ask of it.

- **The tree-sitter DDL walk** runs on every `.sql` file and emits a
  `type` node per `CREATE TABLE` / `CREATE VIEW`, a `function` node per
  `CREATE FUNCTION`, and `variable` nodes for indexes and triggers.
  Schema-qualified names are indexed under the object's own name
  (`identity.kyc_submissions` → `kyc_submissions`), with the schema on
  `meta.schema` and the full name on `meta.qualified_name`; the same
  table name under two schemas stays two symbols. The walk descends
  through parse errors, so statements following a dollar-quoted
  PL/pgSQL body are still extracted.
- **Migration ingest** additionally runs on files under `migrate/` or
  `migrations/`, and on `gortex db schema` dumps anywhere, emitting the
  canonical `table` / `column` / `migration` nodes plus `provides`
  edges. This pass is regex-based: it reads `CREATE TABLE` and ignores
  `ALTER` / `DROP`, so a column added by a later migration is not on
  the table.

What is **not** covered:

- **Query edges from application code are opt-in.** Table reads and
  writes found in string-literal SQL are gated behind the `sql`
  coverage domain, which is off by default because literal-scanning is
  noisy. With it off there are no `queries` edges, and the analyzers
  built on them (`sql_call_sites`, `orphan_tables`,
  `unreferenced_tables`) return an empty result annotated with
  `query_edges: 0` — an empty layer, not an all-clear.
- **Dynamic SQL is invisible** — queries assembled by concatenation or
  a query builder have no literal to read.
- **Schema deltas are not modeled.** The graph holds the shape each
  migration declares, not the accumulated result of replaying them.

## Template engines

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Blade (Laravel) | `.blade`, `.blade.php` | `@section` / `@yield` / `@component` / `@include`; `@extends` → import |
| EJS | `.ejs` | JS `function` / arrow inside `<% … %>`; `include('x')` → import |
| Handlebars / Mustache | `.hbs`, `.handlebars`, `.mustache` | `{{#block}}` as function; `{{> partial}}` → import; helper calls as edges |
| Jinja | `.jinja`, `.jinja2`, `.j2` | `{% block %}` / `{% macro %}`; `extends` / `include` / `import` / `from … import` |
| Twig | `.twig` | Same shape as Jinja |
| ERB | `.erb`, `.rhtml`, `.html.erb`, `.js.erb`, `.css.erb`, `.json.erb` | Ruby `def` / `class` inside `<% … %>`; `render 'x'` → import |
| Liquid | `.liquid` | `{% capture %}` as function; `{% assign %}` as variable; `{% include/render %}` → import |
| Pug | `.pug`, `.jade` | `mixin` / `block NAME` as function; `extends` / `include` → import |

## Blockchain / smart contracts

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Solidity | `.sol` | Contracts, functions, events, modifiers, structs |
| Move (Sui/Aptos) | `.move` | `module`, `fun` / `public fun` / `entry fun`, `struct`, `use X::Y` |
| Cairo (StarkNet) | `.cairo` | `fn`, `struct` / `enum` / `trait` / `mod`, `use X::Y` |
| Noir (Aztec) | `.nr` | `fn`, `struct` / `trait` / `impl` / `mod`, `use dep::X::Y` |
| Tact (TON) | `.tact` | `contract` / `trait` / `message` / `struct`, `fun` / `receive` / `init`, `import "X"` |
| Ballerina | `.bal` | `function`, `service NAME on …`, `type NAME record {…}`, `class`, `import X/Y` |

## Scientific & enterprise

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| R | `.r`, `.R` | Function defs; `library` / `require` / `source` |
| MATLAB | `.mlx` | `function` (end-terminated), `classdef`, `import a.b.c` |
| Mathematica | `.wl`, `.wls`, `.nb` | `name[args_] := body`, `SetDelayed`, `Get[…]` / `Needs[…]` |
| SAS | `.sas` | `proc` / `%macro` as function, `data` as variable, `%include` / `libname` |
| Stata | `.do`, `.ado` | `program define`, `local` / `global`, `use` / `do` / `include` |
| Fortran | `.f`, `.f90`, `.f95`, `.f03`, `.f08` | `subroutine` / `function` / `module`, `use X` |
| COBOL | `.cob`, `.cbl`, `.cpy` | Programs, paragraphs, sections, `COPY` |
| Ada | `.ada`, `.adb`, `.ads` | Packages, procedures, functions, `with` |
| Pascal / Delphi | `.pas`, `.pp`, `.dpr` | Units, procedures, functions, classes |
| ABAP (SAP) | `.abap` | `FORM` / `FUNCTION` / `METHOD` / `CLASS…DEFINITION`, `INCLUDE` |
| Apex (Salesforce) | `.cls`, `.trigger`, `.apex` | Classes, triggers, methods |

## Emerging languages

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Mojo | `.mojo`, `.🔥` | `fn` / `def`, `struct` / `trait`, `from … import` / `import` |
| Odin | `.odin` | `name :: proc`, `name :: struct` / `enum` / `union`, `import "X"` / `foreign import` |
| V | `.v`, `.vsh` | `fn`, `struct` / `interface` / `enum` / `type`, `import`, `module` |
| Hare | `.ha` | `[export] fn`, `type X = struct` / `union` / `enum`, `use X;` |
| Carbon | `.carbon` | `fn`, `class` / `interface` / `adapter` / `choice`, `import` |
| ReScript | `.res`, `.resi` | `let` (function / variable), `type`, `module`, `open` / `include` |
| Gleam | `.gleam` | `[pub] fn`, `[pub] type`, `import X/Y` / `import X.{Y}` |

## Scripting & shell

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Bash / Zsh | `.sh`, `.bash`, `.zsh` | Function defs, `source` / `.` |
| PowerShell | `.ps1`, `.psm1`, `.psd1` | `function`, `class`, `using` |
| Batch | `.bat`, `.cmd` | `:LABEL` as function, `call :LABEL` / `goto` as call edges |
| Perl | `.pl`, `.pm`, `.t` | `sub`, `package`, `use` / `require` |
| Raku | `.raku`, `.rakumod`, `.p6` | `sub`, `class`, `use` |
| Lua | `.lua` | Full tree-sitter (see core matrix) |
| Tcl | `.tcl` | `proc`, `package require`, `source` |
| VimScript | `.vim`, `.vimrc` | `function`, `command`, `source` |
| AutoHotkey | `.ahk`, `.ahk1`, `.ahk2` | Hotkeys, labels, functions (v1 + v2) |
| CoffeeScript | `.coffee` | `name = (args) ->` / `=>`, `class`, `require 'X'` |

## Functional

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Haskell | `.hs`, `.lhs` | Full (see core matrix) |
| OCaml | `.ml`, `.mli` | Full (see core matrix) |
| Clojure | `.clj`, `.cljs`, `.cljc`, `.edn` | `defn`, `defrecord` / `deftype`, `defprotocol`, `require` / `use` |
| Erlang | `.erl`, `.hrl` | Functions, `-type` / `-record`, `-import` |
| Elixir | `.ex`, `.exs` | Full (see core matrix) |
| Racket | `.rkt`, `.ss` | `define`, `struct`, `require` |
| F# | `.fs`, `.fsi`, `.fsx` | `let`, `type`, `module`, `open` |
| Emacs Lisp | `.el` | `defun`, `defvar`, `defmacro`, `require` |

## Systems, mobile, game, niche

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| D | `.d`, `.di` | `struct` / `class` / `interface` / `enum` / `union` / `template`, `import X.Y` |
| Zig | `.zig`, `.zon` | Structs / enums / unions, `@import`, functions, globals |
| Nim | `.nim`, `.nims`, `.nimble` | `proc` / `func` / `method` / `iterator` / `template` / `macro`, type defs, `import` |
| Crystal | `.cr` | `def`, `class`, `module`, `require` |
| Vala | `.vala`, `.vapi` | `namespace` / `class` / `interface` / `struct` / `enum`, methods, `using X;` |
| Groovy / Gradle | `.groovy`, `.gvy`, `.gy`, `.gradle` | Classes, `def`, imports |
| Objective-C(++) | `.m`, `.mm` | `@interface` / `@protocol` / `@implementation`, method selectors, `#import` / `@import` |
| ActionScript | `.as` | `package`, classes, interfaces, `function`, `import X.Y.*;` |
| Dart | `.dart` | Full (see core matrix) |
| Swift | `.swift` | Full (see core matrix) |
| GDScript | `.gd`, `project.godot` | `func`, `class`, signals; receiver-typed calls — a script's funcs are methods of its `class_name`, and `Notify.event()` / typed locals / params / `self` bind by receiver, so a project-global class resolves across directories. A call whose bind contradicts its stated receiver is demoted to the speculative tier, so a caller list holds only calls that class can receive. `[autoload]` singletons in `project.godot` and `const X = preload("res://…")` aliases both bind to the script they name, including one with no `class_name`. Class-body initialiser calls (`@onready var ui = Hud.build()`) are attributed to the file. A method referenced without parentheses — `connect(_on_x)`, `Callable(self, "_on_x")` — is a usage, so `rename` rewrites the signal wiring instead of silently breaking it. `preload("res://…")` binds to the file. |
| Godot resources | `.tscn`, `.tres` | Scene tree nodes keyed by NodePath, `[ext_resource]` → script / sub-scene / asset, per-node `script =` / `instance=ExtResource(…)`, `[connection … method="…"]` → the handler on the target node's script |
| Verse (UEFN) | `.verse` | `class` / `struct` / `enum` / `interface`, functions with specifier blocks, `using { /Path }` |
| Nix | `.nix` | Attribute sets, functions, `import` / `<nixpkgs>` |
| AL (Business Central) | `.al` | Tables, pages, codeunits, procedures |
| Assembly | `.asm`, `.s`, `.S`, `.nasm`, `.masm`, `.inc`, `.a65` | Labels as functions; `call` / `jsr` / `bl` / `jmp` as edges; NASM/MASM/GAS/WLA-DX/CA65/ARM |
| Shaders | `.glsl`, `.vert`, `.frag`, `.hlsl`, `.compute` | Functions, uniforms, `#include` |

## Extension collisions

A few extensions conflict across languages; the registration order in
`internal/parser/languages/register.go` decides which extractor wins.

| Extension | Registered as | Alternative |
|-----------|---------------|-------------|
| `.m` | Objective-C | MATLAB (uses `.mlx` instead) |
| `.v` | V | Verilog / Coq (not yet supported) |
| `.d` | D | D import files (`.di`) |
| `.as` | ActionScript | AssemblyScript (not supported) |

## Adding a language

Three paths, in order of decreasing effort:

1. **Bespoke tree-sitter** (deep extraction). Add a new sub-package under
   [`internal/parser/tsitter/`](../internal/parser/tsitter/) wrapping the C
   grammar, then a hand-tuned extractor under
   [`internal/parser/languages/`](../internal/parser/languages/) that compiles
   per-language S-expression queries. Use [`golang.go`](../internal/parser/languages/golang.go)
   as a reference. Justified for languages where you need ORM / contract /
   dataflow / scope-aware call resolution.

2. **Regex** (simple structural). Use [`nim.go`](../internal/parser/languages/nim.go)
   or [`abap.go`](../internal/parser/languages/abap.go) as templates. Pick this
   when no upstream grammar exists and signature-only is acceptable. Shared
   helpers in [`helpers_indent.go`](../internal/parser/languages/helpers_indent.go)
   (`findBlockEnd`, `findIndentedBlockEnd`, `findKeywordBlockEnd`, `lineAt`).

3. **Forest signature-only** (cheapest, broadest). If the language already has
   a grammar in [`alexaandru/go-sitter-forest`](https://github.com/alexaandru/go-sitter-forest),
   add `github.com/alexaandru/go-sitter-forest/<lang>` to `go.mod` and append
   one row to `forestLanguages` in
   [`forest_registrations.go`](../internal/parser/languages/forest_registrations.go):
   `{"<name>", []string{".<ext>"}, <pkg>.GetLanguage, <pkg>.GetQuery}`.
   `registerForestLanguages` skips the row at runtime if the name or any
   extension is already claimed by a hand-written extractor. The framework
   reads the grammar's bundled `tags.scm` when present and falls back to a
   generic node-kind walker otherwise — see
   [`internal/parser/forest/`](../internal/parser/forest/) for the
   implementation.

All three paths must ship a `_test.go` with at least a happy-path and
empty-input case.
