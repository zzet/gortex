package wiki

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// SemanticProviderStatus mirrors the subset of semantic.ProviderStatus
// the wiki cares about — kept local to avoid importing the semantic
// package (which pulls heavy LSP / CGo dependencies into the wiki).
type SemanticProviderStatus struct {
	Language string
	Name     string
	Status   string
}

// Inputs is the dependency bundle the Generator needs. All fields are
// optional except Graph (without a graph there is nothing to render).
type Inputs struct {
	// Graph is only read, so the caller may hand over a request-scoped
	// reader (an overlay view) instead of the base store.
	Graph             graph.Reader
	Communities       *analysis.CommunityResult
	Processes         *analysis.ProcessResult
	Hotspots          []analysis.HotspotEntry
	Cycles            []analysis.Cycle
	Contracts         []contracts.Contract
	SemanticProviders []SemanticProviderStatus
	// DocsBundle is the pre-rendered markdown of the gortex-docs
	// bundle; when set the generator writes it as changelog.md.
	DocsBundle string
}

// Result describes what was produced.
type Result struct {
	OutputDir string       `json:"output_dir"`
	Files     []FileResult `json:"files"`
	// IndexMarkdown is the top-level wiki/index.md content, suitable
	// for inline return from the MCP tool.
	IndexMarkdown string `json:"index_markdown"`
	// RepoIndexMarkdown is the per-repo index page content.
	RepoIndexMarkdown string `json:"repo_index_markdown"`
}

// Generator orchestrates all wiki pages. The set-up phase (New)
// derives the supporting lookup maps; Generate writes the markdown
// pages and flushes the writer.
type Generator struct {
	graph             graph.Reader
	communities       *analysis.CommunityResult
	processes         *analysis.ProcessResult
	hotspots          []analysis.HotspotEntry
	cycles            []analysis.Cycle
	contractList      []contracts.Contract
	semanticProviders []SemanticProviderStatus
	docsBundle        string

	opts Options

	// Derived state.
	kept             []analysis.Community
	nodeByID         map[string]*graph.Node
	commLabelByNode  map[string]string
	crossComm        map[string]map[string]int
	confDist         map[string]int
	nodeCount        int
	edgeCount        int
	fileCount        int
	symbolCount      int
	processCount     int
	contractCount    int
}

// New constructs a Generator from the inputs and options. The opts are
// normalised in place.
func New(in Inputs, opts Options) *Generator {
	opts = opts.withDefaults()
	g := &Generator{
		graph:             in.Graph,
		communities:       in.Communities,
		processes:         in.Processes,
		hotspots:          in.Hotspots,
		cycles:            in.Cycles,
		contractList:      in.Contracts,
		semanticProviders: in.SemanticProviders,
		docsBundle:        in.DocsBundle,
		opts:              opts,
	}
	g.computeDerived()
	return g
}

func (gen *Generator) computeDerived() {
	gen.nodeByID = make(map[string]*graph.Node)
	gen.commLabelByNode = make(map[string]string)
	gen.crossComm = make(map[string]map[string]int)
	gen.confDist = make(map[string]int)

	// Workspace filter helper — applied to nodes and edges so the
	// stats reflect just the active workspace when one is set.
	wsKeep := func(n *graph.Node) bool {
		if gen.opts.WorkspaceID == "" || n == nil {
			return true
		}
		ws := n.WorkspaceID
		if ws == "" {
			ws = n.RepoPrefix
		}
		return ws == gen.opts.WorkspaceID
	}

	if gen.graph != nil {
		allNodes := gen.graph.AllNodes()
		fileSeen := make(map[string]bool)
		for _, n := range allNodes {
			if !wsKeep(n) {
				continue
			}
			gen.nodeByID[n.ID] = n
			gen.nodeCount++
			switch n.Kind {
			case graph.KindFile:
				if !fileSeen[n.FilePath] {
					fileSeen[n.FilePath] = true
					gen.fileCount++
				}
			case graph.KindImport, graph.KindPackage, graph.KindModule:
				// not a symbol
			default:
				if n.FilePath != "" && !fileSeen[n.FilePath] {
					fileSeen[n.FilePath] = true
					gen.fileCount++
				}
				gen.symbolCount++
			}
		}
		for _, e := range gen.graph.AllEdges() {
			// Edge-level workspace filter: skip when either endpoint
			// is outside the workspace.
			if gen.opts.WorkspaceID != "" {
				from := gen.nodeByID[e.From]
				to := gen.nodeByID[e.To]
				if from == nil || to == nil {
					continue
				}
			}
			gen.edgeCount++
			label := strings.TrimSpace(e.Origin)
			if label == "" {
				label = strings.TrimSpace(e.ConfidenceLabel)
			}
			if label == "" {
				label = "unlabeled"
			}
			gen.confDist[label]++
		}
	}

	// Filter and sort communities just like skills.Generator does.
	if gen.communities != nil {
		var candidates []analysis.Community
		for _, c := range gen.communities.Communities {
			if c.Size < gen.opts.MinCommunity {
				continue
			}
			candidates = append(candidates, c)
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Size != candidates[j].Size {
				return candidates[i].Size > candidates[j].Size
			}
			return candidates[i].ID < candidates[j].ID
		})
		if gen.opts.MaxCommunities > 0 && len(candidates) > gen.opts.MaxCommunities {
			candidates = candidates[:gen.opts.MaxCommunities]
		}
		gen.kept = candidates

		// Per-node label lookup (community label for any member).
		labelByID := make(map[string]string)
		for _, c := range gen.communities.Communities {
			label := c.Label
			if label == "" {
				label = c.ID
			}
			labelByID[c.ID] = label
		}
		for nid, cid := range gen.communities.NodeToComm {
			if lbl, ok := labelByID[cid]; ok {
				gen.commLabelByNode[nid] = lbl
			}
		}

		// Cross-community map (calls only, like skills generator).
		if gen.graph != nil {
			for _, e := range gen.graph.AllEdges() {
				if e.Kind != graph.EdgeCalls {
					continue
				}
				from := gen.communities.NodeToComm[e.From]
				to := gen.communities.NodeToComm[e.To]
				if from == "" || to == "" || from == to {
					continue
				}
				if gen.crossComm[from] == nil {
					gen.crossComm[from] = make(map[string]int)
				}
				gen.crossComm[from][to]++
			}
		}
	}

	if gen.processes != nil {
		gen.processCount = len(gen.processes.Processes)
	}
	gen.contractCount = len(gen.contractList)
}

// Generate renders every page and flushes the writer. The writer
// returned by Files() is the same one that wrote disk so a caller can
// pre-extract the in-memory payloads for testing.
func (gen *Generator) Generate(ctx context.Context) (*Result, *Writer, error) {
	if gen.graph == nil {
		return nil, nil, fmt.Errorf("wiki: graph is required")
	}

	repoSlug := gen.opts.Repo
	if repoSlug == "" {
		repoSlug = "repo"
	}
	repoDir := repoSlug

	writer := NewWriter(gen.opts.OutputDir)

	// Per-repo index.
	repoIndex, err := gen.renderRepoIndex(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: render repo index: %w", err)
	}
	writer.Write(path.Join(repoDir, "index.md"), []byte(repoIndex))

	// Top-level index.
	topIndex := gen.renderTopLevelIndex()
	writer.Write("index.md", []byte(topIndex))

	// Architecture.
	arch, err := gen.renderArchitecturePage(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: render architecture: %w", err)
	}
	writer.Write(path.Join(repoDir, "architecture.md"), []byte(arch))

	// Community pages.
	for _, c := range gen.kept {
		body, err := gen.renderCommunityPage(ctx, c)
		if err != nil {
			return nil, nil, fmt.Errorf("wiki: render community %q: %w", c.ID, err)
		}
		writer.Write(path.Join(repoDir, gen.communityPagePath(c)), []byte(body))
	}

	// Process pages.
	if !gen.opts.NoProcesses && gen.processes != nil {
		for _, p := range gen.processes.Processes {
			body, err := gen.renderProcessPage(ctx, p)
			if err != nil {
				return nil, nil, fmt.Errorf("wiki: render process %q: %w", p.ID, err)
			}
			writer.Write(path.Join(repoDir, processPagePath(p)), []byte(body))
		}
	}

	// Contracts.
	if !gen.opts.NoContracts {
		body, err := gen.renderContractsPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("wiki: render contracts: %w", err)
		}
		writer.Write(path.Join(repoDir, "contracts", "api-surface.md"), []byte(body))
	}

	// Analysis pages.
	hotspots, _ := gen.renderHotspotsPage(ctx)
	writer.Write(path.Join(repoDir, "analysis", "hotspots.md"), []byte(hotspots))

	cycles, _ := gen.renderCyclesPage(ctx)
	writer.Write(path.Join(repoDir, "analysis", "cycles.md"), []byte(cycles))

	sem, _ := gen.renderSemanticPage(ctx)
	writer.Write(path.Join(repoDir, "analysis", "semantic.md"), []byte(sem))

	// Mermaid asset (community graph).
	mer := RenderCommunityGraph(gen.graph, gen.communities, CommunityGraphOpts{
		MinSize: gen.opts.MinCommunity,
		Max:     gen.opts.MaxCommunities,
	})
	writer.Write(path.Join(repoDir, "_assets", "community-graph.mermaid"), []byte(mer))

	// Reserved layout — keep an empty marker so the directory survives
	// `git add` and signals the multi-repo extension point.
	writer.Write(path.Join("_workspace", "README.md"),
		[]byte("# Workspace pages\n\nReserved for cross-repo wiki pages. Single-repo mode leaves this directory empty.\n"))

	// Docs bundle.
	if !gen.opts.NoDocs && gen.docsBundle != "" {
		writer.Write(path.Join(repoDir, "changelog.md"), []byte(gen.docsBundle))
	}

	// HTML wrapper (Phase 5).
	if gen.opts.Format == "html" {
		html := RenderHTMLIndex(repoSlug, gen.opts)
		writer.Write(path.Join(repoDir, "index.html"), []byte(html))
	}

	manifest, err := writer.Flush()
	if err != nil {
		return nil, nil, err
	}
	return &Result{
		OutputDir:         gen.opts.OutputDir,
		Files:             manifest,
		IndexMarkdown:     topIndex,
		RepoIndexMarkdown: repoIndex,
	}, writer, nil
}

// buildFileSymbolMap mirrors skills.Generator.buildFileSymbolMap.
func (gen *Generator) buildFileSymbolMap(c analysis.Community) map[string][]string {
	out := make(map[string][]string)
	for _, mid := range c.Members {
		n := gen.nodeByID[mid]
		if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		out[n.FilePath] = append(out[n.FilePath], n.Name)
	}
	// Stable sort each per-file list.
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// findEntryPoints mirrors skills.Generator.findEntryPoints.
func (gen *Generator) findEntryPoints(c analysis.Community) []string {
	if gen.processes == nil {
		return nil
	}
	memberSet := make(map[string]bool, len(c.Members))
	for _, m := range c.Members {
		memberSet[m] = true
	}
	var eps []string
	seen := make(map[string]bool)
	for _, p := range gen.processes.Processes {
		if memberSet[p.EntryPoint] && !seen[p.EntryPoint] {
			seen[p.EntryPoint] = true
			eps = append(eps, p.EntryPoint)
		}
	}
	if len(eps) > 5 {
		eps = eps[:5]
	}
	return eps
}

// communityLabel returns a human-readable label for an ID, defaulting
// to the ID itself.
func (gen *Generator) communityLabel(id string) string {
	if gen.communities == nil {
		return id
	}
	for _, c := range gen.communities.Communities {
		if c.ID == id {
			if c.Label != "" {
				return c.Label
			}
			return id
		}
	}
	return id
}
