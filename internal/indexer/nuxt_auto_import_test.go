package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
)

func TestNuxtAutoImports_ConfigOnlyIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, ".gitignore"), ".nuxt/\n")
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, list, "export { useRedirect } from '../old/auth';\n")
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	unrelated := filepath.Join(root, "unrelated.go")
	writeFileMk(t, unrelated, "package unrelated\nfunc Keep() {}\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "export function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	other, _ := newSQLiteIndexer(t)
	_, err = other.Index(root)
	require.NoError(t, err)
	require.NoError(t, os.Remove(unrelated))
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, list, "export { useRedirect } from '../new/auth';\n")
	_, err = other.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, other.Graph(), "pages/index.ts", "run"))
	_, err = idx.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	require.NotNil(t, idx.Graph().GetNode("unrelated.go::Keep"), "config-only scope must not consume an unrelated deletion")
}

func TestNuxtExplicitExtension(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, ".nuxt/imports.d.ts"), "export { useAuth } from '../lib/auth.js'")
	writeFileMk(t, filepath.Join(root, "lib/auth.ts"), "export function useAuth() { return 1 }")
	writeFileMk(t, filepath.Join(root, "lib/auth.js"), "export function useAuth() { return 2 }")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "export function run() { return useAuth() }")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "lib/auth.js::useAuth", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestNuxtPointSaveMetadataProof(t *testing.T) {
	root := t.TempDir()
	caller := filepath.Join(root, "pages/index.ts")
	writeFileMk(t, caller, "export function run() { return useAuth() }")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// Preparation is ignored by many watchers: the only event can be the caller save.
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, list, "export { useAuth } from '../lib/auth'")
	writeFileMk(t, filepath.Join(root, "lib/auth.ts"), "export function useAuth() { return 1 }")
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true}, idx.logger)
	require.NoError(t, err)
	err = w.patchGraph(caller, ChangeModified)
	require.NoError(t, err)
	require.Equal(t, "lib/auth.ts::useAuth", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))

	idx.contentProof = newBuildContentProof(root, gitstate.DirtySnapshot{})
	_, err = idx.refreshResolutionInputs(root, caller)
	require.NoError(t, err)
	target, _ := idx.resolveNuxtAutoImport("pages/index.ts", "useAuth")
	require.Equal(t, "lib/auth", target)
	require.False(t, idx.contentProof.isUnbounded(), "unchanged point metadata must not require a full sample")
	require.Equal(t, gitstate.BlobSHA256([]byte("export { useAuth } from '../lib/auth'")), idx.contentProof.proven()[".nuxt/imports.d.ts"])
}

func TestNuxtAutoImports_UseOnlyInstalledSnapshot(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeFileMk(t, filepath.Join(checkout, ".nuxt/imports.d.ts"), "export { useAuth } from '../live/auth';\n")
	writeFileMk(t, filepath.Join(snapshot, ".nuxt/imports.d.ts"), "export { useAuth } from '../committed/auth';\n")
	idx := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	stem, name := idx.resolveNuxtAutoImport("pages/index.ts", "useAuth")
	require.Equal(t, "committed/auth", stem)
	require.Equal(t, "useAuth", name)
	idx.SetContentSource(snapshotOf(t, t.TempDir()))
	stem, _ = idx.resolveNuxtAutoImport("pages/index.ts", "useAuth")
	require.Empty(t, stem, "missing generated metadata must not fall back to the working copy or previous snapshot")
}

func TestNuxtRestoreAndBindingEdits(t *testing.T) {
	root := t.TempDir()
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	page := filepath.Join(root, "app/page.ts")
	impl := filepath.Join(root, "lib/impl.ts")
	body := "export function run() { return useAuth() }"
	decls := "function first() { return 1 }\nfunction second() { return 2 }\nexport default "
	writeFileMk(t, list, "export { useAuth } from '../old'")
	writeFileMk(t, filepath.Join(root, "old.ts"), "export function useAuth() { return 3 }")
	writeFileMk(t, filepath.Join(root, "other.ts"), "export function useAuth() { return 4 }")
	writeFileMk(t, impl, decls+"first")
	writeFileMk(t, page, body)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old.ts::useAuth", callTargetForName(t, idx.Graph(), "app/page.ts", "run"))
	// Neither the bare call nor its target name changes: only its binding does.
	bumpMtime(t, page, "import { useAuth } from '../other'\n"+body)
	_, err = idx.IncrementalReindexPaths(root, []string{page})
	require.NoError(t, err)
	require.Equal(t, "other.ts::useAuth", callTargetForName(t, idx.Graph(), "app/page.ts", "run"))
	bumpMtime(t, page, body)
	_, err = idx.IncrementalReindexPaths(root, []string{page})
	require.NoError(t, err)
	require.Equal(t, "old.ts::useAuth", callTargetForName(t, idx.Graph(), "app/page.ts", "run"))
	idx = New(store, idx.registry, idx.config, idx.logger)
	idx.SetRootPath(root)
	idx.SetFileMtimes(store.LoadFileMtimes(""))
	require.True(t, idx.censusResolutionCurrent(root))
	writeFileMk(t, list, "export { default as useAuth } from '../lib/impl'")
	idx = New(store, idx.registry, idx.config, idx.logger)
	idx.SetFileMtimes(store.LoadFileMtimes(""))
	require.False(t, idx.censusResolutionCurrent(root))
	reindex := func(path, want string) {
		t.Helper()
		_, err := idx.IncrementalReindexPaths(root, []string{path})
		require.NoError(t, err)
		require.Equal(t, want, callTargetForName(t, idx.Graph(), "app/page.ts", "run"))
	}
	reindex(list, "lib/impl.ts::first")
	bumpMtime(t, impl, decls+"second")
	reindex(impl, "lib/impl.ts::second")
	bumpMtime(t, page, "import { useAuth } from '../old'\n"+body)
	reindex(page, "old.ts::useAuth")
	bumpMtime(t, page, body)
	reindex(page, "lib/impl.ts::second")
}

func TestNuxtAliasedImplementationArrives(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, ".nuxt/imports.d.ts"), "export { later as useLater } from '../lib'")
	writeFileMk(t, filepath.Join(root, "page.ts"), "export function run() { return useLater() }")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "unresolved::useLater", callTargetForName(t, idx.Graph(), "page.ts", "run"))
	caller := filepath.Join(root, "page.ts")
	bumpMtime(t, caller, "export const unrelated = 1\nexport function run() { return useLater() }")
	_, err = idx.incrementalPointWatcherPath(root, caller)
	require.NoError(t, err)
	require.Equal(t, "unresolved::useLater", callTargetForName(t, idx.Graph(), "page.ts", "run"))
	module := filepath.Join(root, "lib/index.ts")
	writeFileMk(t, module, "export function later() { return 1 }")
	_, err = idx.incrementalPointWatcherPath(root, module)
	require.NoError(t, err)
	require.Equal(t, "lib/index.ts::later", callTargetForName(t, idx.Graph(), "page.ts", "run"))
}

func TestNuxtWatcherDigestUsesResolvedSnapshot(t *testing.T) {
	root := t.TempDir()
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	set := func(module string) { writeFileMk(t, list, "export { useAuth } from '../"+module+"'") }
	for _, mod := range []string{"old", "new"} {
		writeFileMk(t, filepath.Join(root, mod+".ts"), "export function useAuth() {}")
	}
	page := filepath.Join(root, "app/page.ts")
	writeFileMk(t, page, "export function run() { return useAuth() }")
	set("old")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	other, _ := newSQLiteIndexer(t)
	other.SetRootPath(root)
	set("new")
	caughtUp := false
	idx.incrementalResolveFilesHook = func(files []string) []string {
		digest, err := store.TSAliasDigest("")
		require.NoError(t, err)
		require.Empty(t, digest, "a restart here must not trust the parsed but unresolved graph")
		set("old")
		_, err = other.refreshResolutionInputs(root, "")
		require.NoError(t, err)
		caughtUp = true
		return idx.resolver.ResolveFilesAndIncoming(files).DroppedFiles
	}
	_, err = idx.incrementalPointWatcherPath(root, page)
	require.NoError(t, err)
	require.True(t, caughtUp)
	require.Equal(t, "new.ts::useAuth", callTargetForName(t, idx.Graph(), "app/page.ts", "run"))
	require.False(t, idx.censusResolutionCurrent(root))
	set("new")
	require.True(t, idx.censusResolutionCurrent(root))

	// A source-only edit also replaces call rows before resolver catch-up.
	caughtUp = false
	idx.incrementalResolveFilesHook = func(files []string) []string {
		require.False(t, idx.censusResolutionCurrent(root), "unchanged inputs cannot certify unfinished rows")
		caughtUp = true
		return idx.resolver.ResolveFilesAndIncoming(files).DroppedFiles
	}
	bumpMtime(t, page, "export function secondCaller() { return useAuth() }")
	_, err = idx.incrementalPointWatcherPath(root, page)
	require.NoError(t, err)
	require.True(t, caughtUp)
	require.Equal(t, "new.ts::useAuth", callTargetForName(t, idx.Graph(), "app/page.ts", "secondCaller"))
	require.True(t, idx.censusResolutionCurrent(root))
}

func TestNuxtNestedRepositoryUsesOwningList(t *testing.T) {
	outer, inner := t.TempDir(), t.TempDir()
	writeFileMk(t, filepath.Join(outer, ".nuxt/imports.d.ts"), "export { useAuth } from '../outer'")
	writeFileMk(t, filepath.Join(inner, ".nuxt/imports.d.ts"), "export { useAuth } from '../inner'")
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"team": {RepoPrefix: "team", RootPath: outer}, "team/store": {RepoPrefix: "team/store", RootPath: inner},
	}}
	for file, want := range map[string]string{"team/store/page.ts": "team/store/inner", "team/storefront/page.ts": "team/outer"} {
		got, _ := mi.nuxtAutoImportResolver()(file, "useAuth")
		require.Equal(t, want, got)
	}
}

// Cause the real read-version fence to reject this file on every attempt.
type movingNuxtFile struct {
	bomStripTransform
	file string
}

func (m movingNuxtFile) matches(path string) bool {
	return filepath.Base(path) == filepath.Base(m.file)
}
func (m movingNuxtFile) apply(_ string, src []byte) ([]byte, error) {
	st, err := os.Stat(m.file)
	if err != nil {
		return nil, err
	}
	next := st.ModTime().Add(time.Second)
	return src, os.Chtimes(m.file, next, next)
}

func TestNuxtResolutionRetryOnlyRestagesFailedFile(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.ts")
	writeFileMk(t, page, "export function run() { return useAuth() }")
	for _, mod := range []string{"old", "new"} {
		writeFileMk(t, filepath.Join(root, mod+".ts"), "export function useAuth() {}")
	}
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, list, "export { useAuth } from '../old'")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	original := idx.transforms.transforms
	idx.transforms.transforms = append(original[:len(original):len(original)], movingNuxtFile{file: page})
	writeFileMk(t, list, "export { useAuth } from '../new'")
	for pass := range 2 {
		res, err := idx.IncrementalReindexPaths(root, []string{list})
		require.NoError(t, err)
		require.Contains(t, res.FailedFiles, page)
		if pass == 1 {
			require.Equal(t, 1, res.StaleFileCount)
		}
	}
	idx.transforms.transforms = original
	res, err := idx.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Empty(t, res.FailedFiles)
	require.Equal(t, 1, res.StaleFileCount)
	require.Equal(t, "new.ts::useAuth", callTargetForName(t, idx.Graph(), "page.ts", "run"))
}

func TestNuxtResolutionRejectsEscapingExtends(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFileMk(t, filepath.Join(root, "tsconfig.json"), `{"extends":"./link/base.json"}`)
	writeFileMk(t, filepath.Join(outside, "base.json"), `{"compilerOptions":{"paths":{"@secret":["private"]}}}`)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	idx, _ := newSQLiteIndexer(t)
	idx.SetRootPath(root)
	in, err := idx.refreshResolutionInputs(root, "")
	require.NoError(t, err)
	require.Empty(t, idx.resolvePathAliasImport("page.ts", "@secret"))
	require.Empty(t, in.reads["link/base.json"], "an out-of-root read cannot supply a content hash")
}

func TestExactBindingModuleEdits(t *testing.T) {
	const page = "export function run(){return local()}"
	const defs = "function first(){}\nfunction second(){}\nexport default "
	for _, tc := range []struct{ name, initial, editPath, fresh, barrel, exp, want1, want2 string }{
		{"repoint barrel", "export function first(){};export function second(){}", "barrel.ts", "export {second as publicIt} from './impl'", "export {first as publicIt} from './impl'", "publicIt", "impl.ts::first", "impl.ts::second"},
		{"change default", defs + "first", "impl.ts", defs + "second", "", "default", "impl.ts::first", "impl.ts::second"},
		{"rename default", "export default function first(){}", "impl.ts", "export default function second(){}", "", "default", "impl.ts::first", "impl.ts::second"},
		{"add missing export", "export const other=1", "impl.ts", "export function actual(){}", "", "actual", "unresolved::local", "impl.ts::actual"},
		{"add forwarded module", "", "impl.ts", "export function actual(){}", "export {actual as publicIt} from './impl'", "publicIt", "unresolved::local", "impl.ts::actual"},
		{"add forwarded local import", "", "impl.ts", "export function actual(){}", "import {actual} from './impl'; export default actual", "default", "unresolved::local", "impl.ts::actual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			module := "impl"
			if tc.initial != "" {
				writeFileMk(t, filepath.Join(root, "impl.ts"), tc.initial)
			}
			if tc.barrel != "" {
				module = "barrel"
				writeFileMk(t, filepath.Join(root, "barrel.ts"), tc.barrel)
			}
			writeFileMk(t, filepath.Join(root, "explicit.ts"), "import {"+tc.exp+" as local} from './"+module+"'\n"+page)
			writeFileMk(t, filepath.Join(root, "generated.ts"), page)
			writeFileMk(t, filepath.Join(root, ".nuxt/imports.d.ts"), "export {"+tc.exp+" as local} from '../"+module+"'")
			idx, _ := newSQLiteIndexer(t)
			_, err := idx.Index(root)
			require.NoError(t, err)
			check := func(want string) {
				t.Helper()
				for _, file := range []string{"explicit.ts", "generated.ts"} {
					require.Equal(t, want, callTargetForName(t, idx.Graph(), file, "run"), file)
				}
			}
			check(tc.want1)
			changed := filepath.Join(root, tc.editPath)
			bumpMtime(t, changed, tc.fresh)
			_, err = idx.incrementalPointWatcherPath(root, changed)
			require.NoError(t, err)
			check(tc.want2)
		})
	}
}
func TestNuxtResolutionRetryCannotDelete(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.ts")
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, page, "export function run(){useIt()}")
	writeFileMk(t, list, "export {useIt} from '../old'")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	idx.transforms.transforms = append(idx.transforms.transforms, movingNuxtFile{file: page})
	writeFileMk(t, list, "export {useIt} from '../new'")
	result, err := idx.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Contains(t, result.FailedFiles, page)
	require.NoError(t, os.Remove(page))
	result, err = idx.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Zero(t, result.DeletedFileCount)
	require.NotNil(t, idx.Graph().GetNode("page.ts::run"))
}

func TestNuxtRemovedShadowRebinds(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.ts")
	writeFileMk(t, filepath.Join(root, ".nuxt/imports.d.ts"), "export {real as useIt} from '../lib'")
	writeFileMk(t, filepath.Join(root, "lib.ts"), "export function real(){}")
	writeFileMk(t, page, "export function run(useIt: Function){return useIt()}")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "unresolved::useIt", callTargetForName(t, idx.Graph(), "page.ts", "run"))
	bumpMtime(t, page, "export function run(){return useIt()}")
	_, err = idx.incrementalPointWatcherPath(root, page)
	require.NoError(t, err)
	require.Equal(t, "lib.ts::real", callTargetForName(t, idx.Graph(), "page.ts", "run"))
}

func TestAliasPreferredModuleArrives(t *testing.T) {
	for _, tc := range []struct{ name, alias, preferred, source string }{
		{"wildcard", `"@impl/*":["src/*","fallback/*"]`, "src/lib.ts", "export function impl(){}"},
		{"declaration", `"@impl/lib":["src/lib.d.ts","fallback/lib.ts"]`, "src/lib.d.ts", "export declare function impl(): void;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFileMk(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"paths":{`+tc.alias+`}}}`)
			writeFileMk(t, filepath.Join(root, "fallback/lib.ts"), "export function impl(){}")
			writeFileMk(t, filepath.Join(root, "page.ts"), "import {impl} from '@impl/lib'; export function run(){impl()}")
			idx, _ := newSQLiteIndexer(t)
			_, err := idx.Index(root)
			require.NoError(t, err)
			require.Equal(t, "fallback/lib.ts::impl", callTargetForName(t, idx.Graph(), "page.ts", "run"))
			preferred := filepath.Join(root, tc.preferred)
			writeFileMk(t, preferred, tc.source)
			_, err = idx.incrementalPointWatcherPath(root, preferred)
			require.NoError(t, err)
			require.Equal(t, tc.preferred+"::impl", callTargetForName(t, idx.Graph(), "page.ts", "run"))
			var imports []string
			for _, e := range idx.Graph().GetOutEdges("page.ts") {
				if e.Kind == graph.EdgeImports {
					imports = append(imports, e.To)
				}
			}
			require.ElementsMatch(t, []string{tc.preferred, tc.preferred + "::impl"}, imports)
		})
	}
}

func TestConfiglessBindingFence(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.js")
	writeFileMk(t, filepath.Join(root, "lib.js"), "export function impl(){}")
	writeFileMk(t, page, "import {impl} from './lib'; export function run(){impl()}")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.True(t, idx.censusResolutionCurrent(root))
	var sawFencedCatchup bool
	idx.incrementalResolveFilesHook = func(files []string) []string {
		require.False(t, idx.censusResolutionCurrent(root), "restart must not certify incomplete bindings")
		sawFencedCatchup = true
		return idx.resolver.ResolveFilesAndIncoming(files).DroppedFiles
	}
	bumpMtime(t, page, "import {impl} from './lib'; export function changed(){impl()}")
	_, err = idx.incrementalPointWatcherPath(root, page)
	require.NoError(t, err)
	require.Equal(t, "lib.js::impl", callTargetForName(t, idx.Graph(), "page.js", "changed"))
	require.True(t, idx.censusResolutionCurrent(root))
	require.True(t, sawFencedCatchup)
	sawFencedCatchup = false
	require.NoError(t, os.Remove(filepath.Join(root, "lib.js")))
	_, _, err = idx.incrementalEvictWatcherPath("lib.js")
	require.NoError(t, err)
	require.Equal(t, "unresolved::impl", callTargetForName(t, idx.Graph(), "page.js", "changed"))
	require.True(t, sawFencedCatchup)
	require.True(t, idx.censusResolutionCurrent(root))
}

type refuseBindingInvalidation struct{ *store_sqlite.Store }

func (s refuseBindingInvalidation) ReindexEdgesApplied([]graph.EdgeReindex) bool { return false }

func TestDroppedBindingInvalidationRetriesCaller(t *testing.T) {
	root := t.TempDir()
	barrel := filepath.Join(root, "barrel.ts")
	writeFileMk(t, filepath.Join(root, "lib.ts"), "export function old(){}; export function fresh(){}")
	writeFileMk(t, barrel, "export {old as impl} from './lib'")
	writeFileMk(t, filepath.Join(root, "page.ts"), "import {impl} from './barrel'; export function run(){impl()}")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "lib.ts::old", callTargetForName(t, store, "page.ts", "run"))
	bumpMtime(t, barrel, "export {fresh as impl} from './lib'")
	idx.graph = refuseBindingInvalidation{store}
	_, err = idx.incrementalPointWatcherPath(root, barrel)
	require.ErrorContains(t, err, "binding write did not apply")
	require.False(t, idx.censusResolutionCurrent(root))
	idx.graph = store
	_, err = idx.incrementalPointWatcherPath(root, barrel)
	require.NoError(t, err)
	require.Equal(t, "lib.ts::fresh", callTargetForName(t, store, "page.ts", "run"))
	require.True(t, idx.censusResolutionCurrent(root))
}

func TestDroppedEvictionAndCatchupRetry(t *testing.T) {
	for _, action := range []string{"evict", "save"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			module := filepath.Join(root, "lib.js")
			page := filepath.Join(root, "page.js")
			writeFileMk(t, module, "export function impl(){}")
			writeFileMk(t, page, "import {impl} from './lib'; export function run(){impl()}")
			idx, store := newSQLiteIndexer(t)
			_, err := idx.Index(root)
			require.NoError(t, err)
			caller, want := "run", "unresolved::impl"
			if action == "evict" {
				require.NoError(t, os.Remove(module))
				idx.graph = refuseBindingInvalidation{store}
				_, _, err = idx.incrementalEvictWatcherPath("lib.js")
				idx.graph = store
			} else {
				caller, want = "changed", "lib.js::impl"
				bumpMtime(t, page, "import {impl} from './lib'; export function changed(){impl()}")
				original := idx.resolver
				idx.resolver = resolver.New(refuseBindingInvalidation{store})
				_, err = idx.incrementalPointWatcherPath(root, page)
				idx.resolver = original
			}
			require.ErrorContains(t, err, "binding write did not apply")
			require.False(t, idx.censusResolutionCurrent(root))
			_, err = idx.IncrementalReindexPaths(root, []string{module})
			require.NoError(t, err)
			require.Equal(t, want, callTargetForName(t, store, "page.js", caller))
			require.True(t, idx.censusResolutionCurrent(root))
		})
	}
}

func TestDroppedGoBindingRetriesUnchangedCaller(t *testing.T) {
	root := t.TempDir()
	lib, page := filepath.Join(root, "lib.go"), filepath.Join(root, "page.go")
	writeFileMk(t, lib, "package p\nfunc Impl(){}")
	writeFileMk(t, page, "package p\nfunc Run(){Impl()}")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	bumpMtime(t, page, "package p\nfunc Changed(){Impl()}")
	original := idx.resolver
	idx.resolver = resolver.New(refuseBindingInvalidation{store})
	_, err = idx.incrementalPointWatcherPath(root, page)
	require.ErrorContains(t, err, "binding write did not apply")
	require.False(t, idx.censusResolutionCurrent(root))
	idx.resolver = original
	_, err = idx.IncrementalReindexPaths(root, []string{lib})
	require.NoError(t, err)
	require.Equal(t, "lib.go::Impl", callTargetForName(t, store, "page.go", "Changed"))
	require.True(t, idx.censusResolutionCurrent(root))
}

type refuseGuardWrite struct{ *store_sqlite.Store }

func (s refuseGuardWrite) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	for _, move := range batch {
		if move.OldTo == "other/lib.ts::lookup" && graph.IsUnresolvedTarget(move.Edge.To) {
			return false
		}
	}
	return s.Store.ReindexEdgesApplied(batch)
}

func TestDroppedGuardWriteCannotCertify(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "other/lib.ts"), "export function lookup(){}")
	page := filepath.Join(root, "page.ts")
	writeFileMk(t, page, "function run(){const ns = {other:1}; ns.lookup()}")
	idx, store := newSQLiteIndexer(t)
	original := idx.resolver
	idx.resolver = resolver.New(refuseGuardWrite{store})
	_, err := idx.Index(root)
	require.ErrorContains(t, err, "binding write did not apply")
	require.False(t, idx.censusResolutionCurrent(root))
	idx.resolver = original
	_, err = idx.IncrementalReindexPaths(root, []string{page})
	require.NoError(t, err)
	require.True(t, graph.IsUnresolvedTarget(callTargetForName(t, store, "page.ts", "run")))
}

func TestDroppedFullBindingWriteCannotCertify(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0") // resolve against SQLite, not the in-memory staging graph
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "lib.js"), "export function impl(){}")
	writeFileMk(t, filepath.Join(root, "page.js"), "import {impl} from './lib'; export function run(){impl()}")
	idx, store := newSQLiteIndexer(t)
	original := idx.resolver
	idx.resolver = resolver.New(refuseBindingInvalidation{store})
	_, err := idx.Index(root)
	require.ErrorContains(t, err, "binding write did not apply")
	require.False(t, idx.censusResolutionCurrent(root))
	idx.resolver = original
	_, err = idx.IncrementalReindexPaths(root, []string{filepath.Join(root, "lib.js")})
	require.NoError(t, err)
	require.Equal(t, "lib.js::impl", callTargetForName(t, store, "page.js", "run"))
	require.True(t, idx.censusResolutionCurrent(root))
}

func TestEvictAfterCheckoutMoves(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "checkout")
	writeFileMk(t, filepath.Join(root, "lib.ts"), "export function impl(){}")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
	removed, _ := idx.EvictFile("lib.ts")
	require.Positive(t, removed)
	require.Nil(t, store.GetNode("lib.ts::impl"))
	digest, err := store.TSAliasDigest("")
	require.NoError(t, err)
	require.Empty(t, digest)
}
