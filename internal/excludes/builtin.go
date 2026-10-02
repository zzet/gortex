// Package excludes provides a unified path-exclusion matcher used by
// both the indexer's initial walk and the watcher's live event filter.
// Patterns follow .gitignore semantics (via go-gitignore): leading '/'
// anchors at the root, trailing '/' restricts to directories, '!' negates,
// '**' matches any number of path segments.
package excludes

// Builtin is the superset of directory/file patterns that Gortex always
// excludes, regardless of user config. It merges what the indexer and
// watcher used to maintain as two divergent hardcoded lists.
//
// Users can re-include an entry by listing "!pattern" in global,
// RepoEntry, or workspace config.
var Builtin = []string{
	// Version-control metadata can contain huge object stores and working-copy
	// state. Prune all common VCS directories before the walker descends.
	".git/",
	".jj/",
	".hg/",
	".svn/",
	".bzr/",
	"_darcs/",
	".pijul/",
	".fossil-settings/",
	".terraform/",
	".gortex-cache/",
	".gortex/", // Gortex's per-repo state dir (quarantine, merkle tree)
	".claude/",
	".kiro/",
	"node_modules/",
	"vendor/",
	".venv/",
	"venv/",
	"__pycache__/",
	".mypy_cache/",
	".tox/",
	".next/",
	"target/",
	"build/",
	"dist/",
	// Package-manager + build dirs for non-JS/non-Go ecosystems. These
	// are indexed-by-default without these entries, which pollutes the
	// graph with upstream code (e.g. CocoaPods' sqlite3.c — 150k+ lines)
	// that users can't act on. Names are unambiguous — no first-party
	// project uses `Pods/` or `.dart_tool/` for its own source.
	"Pods/",       // CocoaPods (iOS/macOS)
	".gradle/",    // Gradle build cache (Android/JVM)
	".bundle/",    // Ruby Bundler cache
	".dart_tool/", // Dart/Flutter build cache
	".pub-cache/", // Dart global pub cache, occasionally vendored
	// Agent-tool state directories that document their cache subdirectory.
	// Only the cache is dropped — a tool's project config and its memory/
	// notes files stay visible. Serena (the language-server harness)
	// rewrites .serena/cache/*.pkl on every session; the .pkl extension
	// is claimed by the Pkl language, so without this entry the binary
	// caches re-entered discovery on every rewrite and burned the parse
	// budget on each reconcile.
	".serena/cache/",
	// Dependency caches a repo-local toolchain home materializes inside the
	// working tree. A harness that pins M2_HOME / store-dir at the repo
	// (CI images, benchmark runners, reproducible-build setups) lands tens
	// of thousands of upstream artifacts under these names. They hold
	// third-party jars and tarballs, never first-party source, and a repo
	// that redirects a toolchain home this way rarely thinks to .gitignore
	// the result — which is the case this list exists to cover.
	".m2/",         // Maven local repository
	".ivy2/",       // Ivy / sbt resolution cache
	".sbt/",        // sbt launcher + plugin cache
	".pnpm-store/", // pnpm content-addressable store
	".stack-work/", // Haskell Stack build tree
	// IDE workspace state: per-machine indexes, launch configs, and scratch
	// metadata. `.eclipse/` and `.metadata/` are unambiguous — no project
	// keeps its own source under either.
	".eclipse/",
	".metadata/",
	// Visual Studio / MSBuild artifacts.
	".vs/",         // Visual Studio per-solution cache
	"TestResults/", // Visual Studio / `dotnet test` output
	// MSBuild's intermediate output. `obj/` itself is deliberately NOT
	// excluded wholesale: it is first-party source elsewhere (Go's own
	// toolchain ships `cmd/internal/obj`), and a silent blanket drop is the
	// same failure this list exists to prevent. These entries name what
	// MSBuild actually writes — the per-configuration trees holding
	// regenerated C# (`*.AssemblyInfo.cs`, `*.GlobalUsings.g.cs`,
	// `*.csproj.FileListAbsolute.txt`) and the NuGet restore metadata —
	// so a first-party `obj` package stays indexed.
	//
	// `bin/` is likewise absent: MSBuild's output directory, but also
	// committed source in other ecosystems (Rails' `bin/rails`, an npm
	// package's `bin/cli.js`). .NET repos ignore both in `.gitignore`,
	// which Gortex layers from the git root down, so this list only has to
	// cover the repo that forgot to.
	"obj/[Dd]ebug/",
	"obj/[Rr]elease/",
	"obj/project.assets.json",
	"obj/project.nuget.cache",
	"**/obj/*.nuget.*", // *.csproj.nuget.{dgspec.json,g.props,g.targets}
	"*.tmp",
	// Editor scratch/backup files. Vim cycles swap suffixes backward
	// through the alphabet (.swp → .swo → .swn → ...); neovim writes a
	// .swpx auxiliary alongside .swp on some platforms.
	"*.swp",
	"*.swo",
	"*.swn",
	"*.swm",
	"*.swpx",
	"*~",
}
