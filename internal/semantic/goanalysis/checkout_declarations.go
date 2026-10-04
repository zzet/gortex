package goanalysis

import (
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// mapCheckoutContextDeclarations maps the handle's uses of declarations that
// live in files the handle does not carry onto the nodes the layer below
// serves for them, and returns how many objects it mapped.
//
// A whole-module load maps such a declaration through its own package's
// definitions: the defining package is a root, its TypesInfo.Defs names the
// object, and the node sits in the graph at the declaration's position. A
// checkout pass reads its node projection from the generation handle, which
// carries only the files the generation re-derived — so a use in a re-derived
// file of a declaration in an unchanged file (a call into another package, a
// type from a sibling file) found no node and was dropped: the edge lost its
// type-resolved provenance, which a clean index gives it.
//
// The object is the same one the whole-module load would map (go/types
// imports it from export data or type-checks it from source, with the same
// position), and the node is the layer below's copy of the declaration at that
// position — which is what the whole-module load would find, because the file
// is unchanged by this generation (every changed file is on the handle) and
// the layer below is the composed state of every file the generation does not
// carry. Matching reuses matchRepoDefinitionNode without syntax context: only
// declarations another file can use are asked for (package-level functions,
// types, constants and variables, methods and fields), and those are the
// kinds the context-free ranks already classify.
//
// Only objects of the main module's own packages are mapped: a whole-module
// load does not map vendored or other modules' declarations either (they go
// through the externals attribution). Objects on the handle's own files, or
// already mapped, are left alone.
func mapCheckoutContextDeclarations(
	pkgs []*packages.Package,
	fset *token.FileSet,
	paths *graphPaths,
	handleFiles map[string]struct{},
	objToNode map[types.Object]string,
	reader semantic.CheckoutDeclarationReader,
) int {
	if reader == nil || fset == nil {
		return 0
	}
	modulePath := ""
	for _, pkg := range pkgs {
		if pkg == nil {
			continue
		}
		if modulePath == "" && pkg.Module != nil && pkg.Module.Main {
			modulePath = pkg.Module.Path
		}
	}
	if modulePath == "" {
		return 0
	}
	type pendingDeclaration struct {
		obj types.Object
		pos token.Position
	}
	byPath := make(map[string][]pendingDeclaration)
	seen := make(map[types.Object]struct{})
	for _, pkg := range pkgs {
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}
		for ident, obj := range pkg.TypesInfo.Uses {
			if obj == nil || obj.Pkg() == nil || !obj.Pos().IsValid() {
				continue
			}
			if _, done := seen[obj]; done {
				continue
			}
			// Only a use the pass projects can carry the mapping into an
			// edge: resolveGoUse keeps a use only when its (line-directive
			// adjusted) position lies in a function of a handle file. A use in
			// a sibling file of a root package (parsed for its declarations,
			// never projected) must not make the layer below serve that
			// declaration's whole file: in a package of hundreds of files that
			// read was the pass's dominant cost and changed no edge.
			if !handleFileUse(ident, fset, paths, handleFiles) {
				continue
			}
			seen[obj] = struct{}{}
			if _, mapped := objToNode[obj]; mapped {
				continue
			}
			if !mainModulePackage(obj.Pkg().Path(), modulePath) || !contextDeclarationKind(obj) {
				continue
			}
			pos := fset.Position(obj.Pos())
			relPath := relativePath(pos.Filename, paths.absRoot)
			if relPath == "" || pos.Line <= 0 || !strings.HasSuffix(relPath, ".go") || excludedFromModuleLoad(relPath) {
				continue
			}
			graphPath := scopedGraphPath(paths.repoPrefix, relPath)
			if _, onHandle := handleFiles[normalizeRelPath(graphPath)]; onHandle {
				continue
			}
			byPath[graphPath] = append(byPath[graphPath], pendingDeclaration{obj: obj, pos: pos})
		}
	}
	if len(byPath) == 0 {
		return 0
	}
	files := make([]string, 0, len(byPath))
	for graphPath := range byPath {
		files = append(files, graphPath)
	}
	sort.Strings(files)
	served := reader.GetFileNodesByPaths(files)
	mapped := 0
	for _, graphPath := range files {
		fileNodes := served[graphPath]
		if len(fileNodes) == 0 {
			continue
		}
		goNodes := make([]*graph.Node, 0, len(fileNodes))
		for _, node := range fileNodes {
			if node != nil && node.Language == "go" {
				goNodes = append(goNodes, node)
			}
		}
		for _, want := range byPath[graphPath] {
			node := matchRepoDefinitionNode(goNodes, want.pos, want.obj.Name(), want.obj, goDefinitionContext{})
			if node == nil {
				continue
			}
			objToNode[want.obj] = node.ID
			mapped++
		}
	}
	return mapped
}

// handleFileUse reports whether ident's position, as resolveGoUse reads it,
// lies in one of the handle's files.
func handleFileUse(ident *ast.Ident, fset *token.FileSet, paths *graphPaths, handleFiles map[string]struct{}) bool {
	if ident == nil || !ident.Pos().IsValid() {
		return false
	}
	graphPath := paths.of(fset.Position(ident.Pos()).Filename)
	if graphPath == "" {
		return false
	}
	_, onHandle := handleFiles[normalizeRelPath(graphPath)]
	return onHandle
}

// mainModulePackage reports whether importPath is a package of the module
// whose path is modulePath.
func mainModulePackage(importPath, modulePath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

// contextDeclarationKind reports whether obj is a declaration another file can
// use by name: a function or method, a type, a constant, a package-level
// variable or a struct field. Parameters, locals and labels are file-private
// and never reach this path through a handle file's use.
func contextDeclarationKind(obj types.Object) bool {
	packageScope := obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()
	switch obj := obj.(type) {
	case *types.Func:
		// Package functions sit in the package scope; methods have no parent
		// scope. Both are declarations another file can name.
		return true
	case *types.TypeName, *types.Const:
		return packageScope
	case *types.Var:
		return obj.IsField() || packageScope
	}
	return false
}

// excludedFromModuleLoad reports whether a repository-relative path lies in a
// directory a whole-module "./..." load does not type-check as the main
// module's own package: vendor and testdata trees, and dot or underscore
// directories.
func excludedFromModuleLoad(relPath string) bool {
	for _, segment := range strings.Split(path.Dir(relPath), "/") {
		if segment == "vendor" || segment == "testdata" {
			return true
		}
		if segment != "." && segment != "" && (strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_")) {
			return true
		}
	}
	return false
}
