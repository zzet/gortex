package indexer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Only background contract evidence uses this namespace dispatcher. A dedicated
// root may exclude mutable0 for its own repo while companions still select0.
// Each delegated reader is the actual source captured by SelectedContractInputs.
type contractCapturedCore struct {
	graph.Reader
	readers map[string]graph.Reader
}

func (c *contractCapturedCore) repoForID(id string) (string, error) {
	repo := ""
	found := false
	for candidate := range c.readers {
		if candidate != "" && strings.HasPrefix(id, candidate+"/") && (!found || len(candidate) > len(repo)) {
			repo = candidate
			found = true
		}
	}
	if !found {
		if _, ok := c.readers[""]; ok {
			return "", nil
		}
		return "", fmt.Errorf("contract capture: identity outside admitted cohort %q", id)
	}
	return repo, nil
}
func (c *contractCapturedCore) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	out := graph.ContractFileProjection{SourceNodes: make(map[string]*graph.Node)}
	groups := make(map[string][]string)
	for _, id := range ids {
		repo, e := c.repoForID(id)
		if e != nil {
			return graph.ContractFileProjection{}, e
		}
		groups[repo] = append(groups[repo], id)
	}
	for repo, ids := range groups {
		nodes, e := graph.ContractSourceNodesContext(ctx, c.readers[repo], ids)
		if e != nil {
			return graph.ContractFileProjection{}, e
		}
		for id, node := range nodes {
			if node.RepoPrefix != repo {
				return graph.ContractFileProjection{}, graph.ErrContractProjectionIncomplete
			}
			out.SourceNodes[id] = node
		}
	}
	return out, ctx.Err()
}
func (c *contractCapturedCore) LayerContractFileProjectionContext(ctx context.Context, repo string, files []string) (graph.ContractFileProjection, error) {
	reader := c.readers[repo]
	if reader == nil {
		return graph.ContractFileProjection{}, graph.ErrContractProjectionUnsupported
	}
	return graph.CompleteContractFileProjection(ctx, reader, repo, files)
}

// Keep the singleton visitor on the same admitted source dispatcher. The graph
// helper selects this capability before considering the plural visitor.
func (c *contractCapturedCore) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return c.VisitNodesByNamesContext(ctx, []string{name}, yield)
}
func (c *contractCapturedCore) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	repos := make([]string, 0, len(c.readers))
	for repo := range c.readers {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	stopped := false
	for _, repo := range repos {
		if err := graph.VisitNodesByNamesInRepoContext(ctx, c.readers[repo], names, repo, func(node *graph.Node) bool {
			if node == nil || node.RepoPrefix != repo {
				return true
			}
			stopped = !yield(node)
			return !stopped
		}); err != nil {
			return err
		}
		if stopped {
			break
		}
	}
	return ctx.Err()
}
func (c *contractCapturedCore) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	byRepo := make(map[string][]string)
	fileRepo := make(map[string][]graph.ConstantFileKey)
	for _, id := range ids {
		repo, e := c.repoForID(id)
		if e != nil {
			return graph.ConstantValueProjection{}, e
		}
		byRepo[repo] = append(byRepo[repo], id)
	}
	for _, file := range files {
		fileRepo[file.RepoPrefix] = append(fileRepo[file.RepoPrefix], file)
	}
	out := graph.ConstantValueProjection{Rows: make(map[string]graph.ScopedConstantValueRow), Nodes: make(map[string]string), Files: make(map[graph.ConstantFileKey]graph.FileMetaRow)}
	for repo, reader := range c.readers {
		checked, ok := reader.(graph.ConstantValueProjectionReader)
		if !ok {
			return graph.ConstantValueProjection{}, graph.ErrConstantProjectionUnsupported
		}
		p, e := checked.ReadConstantValueProjectionContext(ctx, byRepo[repo], fileRepo[repo])
		if e != nil {
			return graph.ConstantValueProjection{}, e
		}
		for id, row := range p.Rows {
			if row.RepoPrefix != repo {
				return graph.ConstantValueProjection{}, graph.ErrConstantProjectionIncomplete
			}
			out.Rows[id] = row
		}
		for id, path := range p.Nodes {
			out.Nodes[id] = path
		}
		for key, row := range p.Files {
			out.Files[key] = row
		}
	}
	return out, ctx.Err()
}
func (c *contractCapturedCore) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	out := make(map[graph.SemanticBindingSite]string)
	byRepo := make(map[string][]graph.SemanticBindingSite)
	for _, site := range sites {
		byRepo[site.RepoPrefix] = append(byRepo[site.RepoPrefix], site)
	}
	for repo, sites := range byRepo {
		reader, ok := c.readers[repo].(graph.SemanticBindingTypeReader)
		if !ok {
			return nil, fmt.Errorf("contract capture: selected semantic evidence unsupported for %q", repo)
		}
		rows, e := reader.SemanticBindingTypes(sites)
		if e != nil {
			return nil, e
		}
		for site, value := range rows {
			out[site] = value
		}
	}
	return out, nil
}
