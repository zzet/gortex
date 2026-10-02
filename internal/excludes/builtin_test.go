package excludes

import "testing"

func TestBuiltinExcludesVersionControlMetadata(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".git/objects/pack/pack.data",
		".jj/repo/store/type",
		".hg/store/data/file.i",
		".svn/pristine/00/file.svn-base",
		".bzr/repository/pack-names",
		"_darcs/inventories/000000",
		".pijul/changes/ABC.change",
		".fossil-settings/ignore-glob",
		"nested/.jj/repo/store/type",
	} {
		if !matcher.MatchRel(path) {
			t.Errorf("Builtin should exclude VCS metadata path %q", path)
		}
	}
}

func TestBuiltinDoesNotExcludeSimilarSourcePaths(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		"jj/repository.go",
		"pijul/client.go",
		"fossil-settings/parser.go",
		"darcs/model.go",
	} {
		if matcher.MatchRel(path) {
			t.Errorf("Builtin unexpectedly excludes source path %q", path)
		}
	}
}

func TestBuiltinExcludesVisualStudioArtifacts(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".vs/Solution/v17/HierarchyCache.v1.txt",
		".vs/Solution/v18/HierarchyCache.v1.txt",
		"TestResults/run-1/results.trx",
		"App/obj/Debug/net8.0/App.AssemblyInfo.cs",
		"App/obj/Debug/net8.0/App.GlobalUsings.g.cs",
		"App/obj/Debug/net8.0/App.csproj.FileListAbsolute.txt",
		"App/obj/Release/net8.0/App.AssemblyInfo.cs",
		"App/obj/release/net8.0/App.AssemblyInfo.cs",
		"App/obj/project.assets.json",
		"App/obj/project.nuget.cache",
		"App/obj/App.csproj.nuget.g.props",
		"App/obj/App.csproj.nuget.dgspec.json",
		"obj/project.assets.json",
	} {
		if !matcher.MatchRel(path) {
			t.Errorf("Builtin should exclude MSBuild artifact %q", path)
		}
	}
}

// A harness that pins a toolchain home at the repo (CI images, benchmark
// runners) materializes these caches inside the working tree, where they
// hold upstream artifacts the walker would otherwise index.
func TestBuiltinExcludesRepoLocalToolchainCaches(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".m2/repository/org/slf4j/slf4j-api/2.0.9/slf4j-api-2.0.9.jar",
		".ivy2/cache/org.scala-lang/scala-library/ivy.xml",
		".sbt/1.0/plugins/target/config-classes/cached.class",
		".pnpm-store/v3/files/00/abcdef",
		".stack-work/dist/x86_64-linux/build/Main.hi",
		".eclipse/org.eclipse.platform_4.30/configuration/config.ini",
		".metadata/.plugins/org.eclipse.core.resources/.projects/p/.location",
		"nested/.m2/repository/junit/junit/4.13.2/junit-4.13.2.jar",
	} {
		if !matcher.MatchRel(path) {
			t.Errorf("Builtin should exclude toolchain cache path %q", path)
		}
	}
}

// Agent-tool state directories that document a cache subdirectory: the
// cache is dropped, the tool's config and notes stay visible.
func TestBuiltinExcludesAgentToolCaches(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".serena/cache/mql/raw_document_symbols.pkl",
		".serena/cache/some/other/cache.pkl",
		"nested/.serena/cache/raw_document_symbols.pkl",
	} {
		if !matcher.MatchRel(path) {
			t.Errorf("Builtin should exclude agent tool cache path %q", path)
		}
	}
}

// The cache entries are directory-anchored: a first-party file under the
// tool's own directory, or anywhere else, stays indexed.
func TestBuiltinDoesNotExcludeAgentToolState(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".serena/project.yml",
		".serena/memories/conventions.md",
		"serena/cache/handler.go", // no leading dot: first-party package
	} {
		if matcher.MatchRel(path) {
			t.Errorf("Builtin unexpectedly excludes first-party path %q", path)
		}
	}
}

// The cache entries are directory-anchored, so a first-party package or file
// that merely shares the stem stays indexed.
func TestBuiltinDoesNotExcludeSimilarToolchainNames(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		"m2/decoder.go",
		"internal/sbt/parser.go",
		"pkg/metadata/loader.go",
		"src/eclipse/render.ts",
		"tools/pnpm-store-inspector/main.go",
	} {
		if matcher.MatchRel(path) {
			t.Errorf("Builtin unexpectedly excludes first-party path %q", path)
		}
	}
}

// The MSBuild entries name what MSBuild writes rather than blanket-dropping
// `obj/` or `bin/`, both of which are committed source in other ecosystems.
func TestBuiltinDoesNotExcludeFirstPartyObjOrBin(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		"cmd/internal/obj/link.go", // Go's own toolchain
		"src/cmd/internal/obj/x86/asm6.go",
		"obj/writer.go",
		"bin/rails",
		"bin/cli.js",
		"bin/setup",
		"App/Controllers/HomeController.cs",
	} {
		if matcher.MatchRel(path) {
			t.Errorf("Builtin unexpectedly excludes first-party path %q", path)
		}
	}
}
