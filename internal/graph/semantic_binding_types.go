package graph

// SemanticBindingSite identifies one source binding whose compiler-resolved
// type can be used by contract extraction. FilePath is the graph-scoped path
// (repo prefix included in multi-repo stores); Name disambiguates multiple
// bindings declared on the same line.
type SemanticBindingSite struct {
	RepoPrefix string
	FilePath   string
	Line       int
	Name       string
}

// SemanticBindingType is the compact, persistence-safe projection of a
// compiler binding. It intentionally contains no AST, token.FileSet,
// types.Object, or packages.Package references.
type SemanticBindingType struct {
	Site     SemanticBindingSite
	TypeName string
}

// SemanticBindingTypeWriter atomically replaces one repository's compiler
// binding index and supports file-scoped invalidation for incremental reindex.
// Implementations must treat an empty replacement as clearing the repository.
type SemanticBindingTypeWriter interface {
	ReplaceSemanticBindingTypes(repoPrefix string, rows []SemanticBindingType) error
	ReplaceSemanticBindingTypesForFiles(repoPrefix string, files []string, rows []SemanticBindingType) error
	DeleteSemanticBindingTypesByFiles(repoPrefix string, files []string) error
}

// SemanticBindingTypeReader resolves a deduplicated batch of binding sites.
// Missing sites are omitted from the returned map. Implementations must issue
// predicate-shaped/indexed lookups rather than one query per site.
type SemanticBindingTypeReader interface {
	SemanticBindingTypes(sites []SemanticBindingSite) (map[SemanticBindingSite]string, error)
}

// SemanticBindingFileReader returns the complete binding projection for the
// requested files, including the absence of sites removed by a new compile.
type SemanticBindingFileReader interface {
	SemanticBindingTypesForFiles(repoPrefix string, files []string) (map[SemanticBindingSite]string, error)
}

// SemanticBindingTypeStore is the full optional side-table capability.
type SemanticBindingTypeStore interface {
	SemanticBindingTypeWriter
	SemanticBindingTypeReader
}
