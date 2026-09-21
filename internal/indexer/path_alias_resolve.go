package indexer

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/parser/tsalias"
	"github.com/zzet/gortex/internal/resolver"
)

// resolvePathAliasImport is the resolver.PathAliasResolver installed on
// this single-repo Indexer's resolver. It expands a JS/TS tsconfig /
// jsconfig `compilerOptions.paths` alias (or a `baseUrl`-rooted bare
// specifier) declared in the importing file's nearest-ancestor config to
// the repo-prefixed, extension-stripped file stem it targets. Returns ""
// when no alias applies.
//
// The collection comes from the tree this Indexer is indexing — the installed
// content source when there is one, the working copy otherwise — so a
// generation built from a committed tree expands its aliases through that
// tree's tsconfig / jsconfig rather than through whatever the checkout holds.
// Reading the checkout here would not leave an edge missing: it would land a
// present, WRONG edge, on the file the checkout's config names. The
// working-copy collection is still loaded once, lazily, and shared via the
// package-level cache keyed on repo root path; a snapshot's is memoised on the
// Indexer that owns the source (see Indexer.tsAliasCollection).
func (idx *Indexer) resolvePathAliasImport(callerFile, specifier string) string {
	return resolveTSPathAlias(idx.tsAliasCollection(), idx.repoPrefix, callerFile, specifier)
}

// pathAliasResolver builds the resolver.PathAliasResolver for the
// multi-repo resolvers. It defers to tsAliasMapFor, which locates the
// importing file's repo and its nearest-ancestor tsconfig/jsconfig scope,
// then re-prefixes the resolved repo-relative target so it lines up with
// graph FilePaths.
func (mi *MultiIndexer) pathAliasResolver() resolver.PathAliasResolver {
	return func(callerFile, specifier string) string {
		m, prefix := mi.tsAliasMapFor(callerFile)
		return prefixTSAliasTarget(prefix, tsalias.Resolve(m, specifier))
	}
}

// resolveTSPathAlias expands a JS/TS import specifier against a tsconfig
// alias collection. callerFile is the importing file's repo-prefixed
// graph path; repoPrefix is the prefix re-attached to the repo-relative
// result so it matches graph FilePaths. Returns "" when the collection is
// empty, the caller is outside the prefix, or the specifier matches no
// alias.
func resolveTSPathAlias(coll *tsalias.Collection, repoPrefix, callerFile, specifier string) string {
	if coll == nil || specifier == "" {
		return ""
	}
	rel := callerFile
	if repoPrefix != "" {
		if !strings.HasPrefix(callerFile, repoPrefix+"/") {
			return ""
		}
		rel = strings.TrimPrefix(callerFile, repoPrefix+"/")
	}
	return prefixTSAliasTarget(repoPrefix, tsalias.Resolve(coll.FindForFile(path.Dir(rel)), specifier))
}

// prefixTSAliasTarget re-attaches repoPrefix to a repo-relative tsalias
// target so it matches graph FilePaths, passing through an empty target.
func prefixTSAliasTarget(repoPrefix, target string) string {
	if target == "" || repoPrefix == "" {
		return target
	}
	return repoPrefix + "/" + target
}
