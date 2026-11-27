package indexer

import (
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// committedBindingCorpus stages the compiler's compact sidecar alongside its
// graph facts. It survives the node drain and is persisted before publication.
type committedBindingCorpus struct {
	*graph.Graph
	mu   sync.Mutex
	rows map[string]map[string]map[graph.SemanticBindingSite]string
}

var _ graph.SemanticBindingTypeWriter = (*committedBindingCorpus)(nil)

// The embedded shadow has no symbol FTS; the drain derives it from surviving nodes.
func (*committedBindingCorpus) BatchDeleteSymbolFTS([]string) error { return nil }

func (c *committedBindingCorpus) ReplaceSemanticBindingTypes(repo string, rows []graph.SemanticBindingType) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = make(map[string]map[string]map[graph.SemanticBindingSite]string)
	}
	c.rows[repo] = make(map[string]map[graph.SemanticBindingSite]string)
	c.addLocked(repo, rows)
	return nil
}

func (c *committedBindingCorpus) ReplaceSemanticBindingTypesForFiles(repo string, files []string, rows []graph.SemanticBindingType) error {
	if len(files) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = make(map[string]map[string]map[graph.SemanticBindingSite]string)
	}
	if c.rows[repo] == nil {
		c.rows[repo] = make(map[string]map[graph.SemanticBindingSite]string)
	}
	for _, file := range files {
		delete(c.rows[repo], file)
	}
	c.addLocked(repo, rows)
	return nil
}

func (c *committedBindingCorpus) addLocked(repo string, rows []graph.SemanticBindingType) {
	for _, row := range rows {
		site := row.Site
		site.RepoPrefix = repo
		if c.rows[repo][site.FilePath] == nil {
			c.rows[repo][site.FilePath] = make(map[graph.SemanticBindingSite]string)
		}
		c.rows[repo][site.FilePath][site] = row.TypeName
	}
}

func (c *committedBindingCorpus) DeleteSemanticBindingTypesByFiles(repo string, files []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, file := range files {
		delete(c.rows[repo], file)
	}
	return nil
}

func (c *committedBindingCorpus) SemanticBindingTypesForFiles(repo string, files []string) (map[graph.SemanticBindingSite]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[graph.SemanticBindingSite]string)
	for _, file := range files {
		for site, value := range c.rows[repo][file] {
			out[site] = value
		}
	}
	return out, nil
}

func (c *committedBindingCorpus) persist(repo string, writer graph.SemanticBindingTypeWriter) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var rows []graph.SemanticBindingType
	for _, bySite := range c.rows[repo] {
		for site, value := range bySite {
			rows = append(rows, graph.SemanticBindingType{Site: site, TypeName: value})
		}
	}
	return writer.ReplaceSemanticBindingTypes(repo, rows)
}

func (b commitLayerBase) SemanticBindingTypesForFiles(repo string, files []string) (map[graph.SemanticBindingSite]string, error) {
	reader, ok := b.Reader.(graph.SemanticBindingFileReader)
	if !ok {
		return nil, graph.ErrSemanticBindingProjectionUnsupported
	}
	return reader.SemanticBindingTypesForFiles(repo, files)
}

// A node/edge match cannot prove that a local binding kept its type, or that
// an old binding disappeared. Unknown projections retain dependent Go output;
// declared read-only context is withdrawn independently of binding comparison.
func (b *SparseGenerationBuilder) matchingContextBindings(req BuildRequest, plan buildPlan, corpus contextCorpus) (map[string]bool, error) {
	if req.committedTypecheck == nil || b.Semantic == nil || !plan.hasGoOutputCandidates() {
		return nil, nil
	}
	var files []string
	matches := make(map[string]bool)
	for _, rel := range plan.context {
		if _, declared := plan.declared[rel]; declared {
			continue
		}
		if strings.HasSuffix(rel, ".go") {
			file := builderGraphPath(req.RepoPrefix, rel)
			files = append(files, file)
			matches[file] = false
		}
	}
	if len(files) == 0 {
		return matches, nil
	}
	group := func(reader any) (map[string]map[graph.SemanticBindingSite]string, error) {
		projection, ok := reader.(graph.SemanticBindingFileReader)
		if !ok {
			return nil, graph.ErrSemanticBindingProjectionUnsupported
		}
		rows, err := projection.SemanticBindingTypesForFiles(req.RepoPrefix, files)
		if err != nil {
			return nil, err
		}
		byFile := make(map[string]map[graph.SemanticBindingSite]string)
		for site, value := range rows {
			if _, wanted := matches[site.FilePath]; !wanted || site.RepoPrefix != req.RepoPrefix {
				return nil, graph.ErrSemanticBindingProjectionIncomplete
			}
			if byFile[site.FilePath] == nil {
				byFile[site.FilePath] = make(map[graph.SemanticBindingSite]string)
			}
			byFile[site.FilePath][site] = value
		}
		return byFile, nil
	}
	carried, err := group(corpus)
	if errors.Is(err, graph.ErrSemanticBindingProjectionUnsupported) || errors.Is(err, graph.ErrSemanticBindingProjectionIncomplete) {
		return matches, nil
	}
	if err != nil {
		return nil, err
	}
	base, err := group(req.Base)
	if errors.Is(err, graph.ErrSemanticBindingProjectionUnsupported) || errors.Is(err, graph.ErrSemanticBindingProjectionIncomplete) {
		return matches, nil
	}
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		matches[file] = reflect.DeepEqual(carried[file], base[file])
	}
	return matches, nil
}
