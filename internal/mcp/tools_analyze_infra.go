package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// handleAnalyzeK8sResources surfaces the KindResource graph layer
// produced by the K8s manifest extractor. Returns one row per Resource
// node with kind, namespace, name, file, line, and the count of
// outgoing infra edges (depends_on / configures / mounts / exposes /
// uses_env). Useful for "which deployments live in prod" / "which
// ConfigMaps no workload references" / "what does this Resource
// reach?" queries.
//
// Filters:
//   - k8s_kind:  K8s kind filter (Deployment / Service / Ingress / …)
//   - namespace: substring match on namespace
//   - name:      substring match on resource name
func (s *Server) handleAnalyzeK8sResources(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	kindFilter := strings.TrimSpace(stringArg(args, "k8s_kind"))
	nsFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "namespace")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	type resourceRow struct {
		ID         string `json:"id"`
		K8sKind    string `json:"k8s_kind"`
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		File       string `json:"file"`
		Line       int    `json:"line"`
		DependsOn  int    `json:"depends_on"`
		Configures int    `json:"configures"`
		Mounts     int    `json:"mounts"`
		Exposes    int    `json:"exposes"`
		UsesEnv    int    `json:"uses_env"`
	}

	// Tally per-resource edge fan-out in a single pass so we don't
	// re-walk AllEdges() for every resource.
	type counts struct {
		dependsOn, configures, mounts, exposes, usesEnv int
	}
	tally := make(map[string]*counts)
	bump := func(id string, edge graph.EdgeKind) {
		c, ok := tally[id]
		if !ok {
			c = &counts{}
			tally[id] = c
		}
		switch edge {
		case graph.EdgeDependsOn:
			c.dependsOn++
		case graph.EdgeConfigures:
			c.configures++
		case graph.EdgeMounts:
			c.mounts++
		case graph.EdgeExposes:
			c.exposes++
		case graph.EdgeUsesEnv:
			c.usesEnv++
		}
	}
	for e := range edgesByKinds(s.readerFor(ctx),
		graph.EdgeDependsOn,
		graph.EdgeConfigures,
		graph.EdgeMounts,
		graph.EdgeExposes,
		graph.EdgeUsesEnv,
	) {
		bump(e.From, e.Kind)
	}

	var rows []*resourceRow
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindResource {
			continue
		}
		k8sKind, _ := n.Meta["k8s_kind"].(string)
		ns, _ := n.Meta["namespace"].(string)
		if kindFilter != "" && !strings.EqualFold(k8sKind, kindFilter) {
			continue
		}
		if nsFilter != "" && !strings.Contains(strings.ToLower(ns), nsFilter) {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(n.Name), nameFilter) {
			continue
		}
		c := tally[n.ID]
		if c == nil {
			c = &counts{}
		}
		rows = append(rows, &resourceRow{
			ID: n.ID, K8sKind: k8sKind, Namespace: ns, Name: n.Name,
			File: n.FilePath, Line: n.StartLine,
			DependsOn:  c.dependsOn,
			Configures: c.configures,
			Mounts:     c.mounts,
			Exposes:    c.exposes,
			UsesEnv:    c.usesEnv,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].K8sKind != rows[j].K8sKind {
			return rows[i].K8sKind < rows[j].K8sKind
		}
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"resources": rows,
		"total":     len(rows),
	})
}

// handleAnalyzeImages surfaces the KindImage graph layer. Returns
// one row per Image node with consumer counts (Dockerfile stages
// that build FROM it, K8s Resources that reference it). Useful for
// "which images are stale" / "which Resource pulls nginx:latest"
// queries.
//
// Filters:
//   - role:      base | stage (default: both)
//   - ref:       substring match on the image reference (registry/path)
//   - tag:       exact tag match (latest / 1.25 / sha256:…)
func (s *Server) handleAnalyzeImages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	roleFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "role")))
	refFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "ref")))
	tagFilter := strings.TrimSpace(stringArg(args, "tag"))

	type imageRow struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Role      string `json:"role,omitempty"`
		Ref       string `json:"ref,omitempty"`
		Tag       string `json:"tag,omitempty"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		Consumers int    `json:"consumers"`
	}

	consumers := make(map[string]int)
	for e := range edgesByKinds(s.readerFor(ctx), graph.EdgeDependsOn) {
		consumers[e.To]++
	}

	var rows []*imageRow
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindImage {
			continue
		}
		role, _ := n.Meta["role"].(string)
		ref, _ := n.Meta["ref"].(string)
		tag, _ := n.Meta["tag"].(string)
		if roleFilter != "" && role != roleFilter {
			continue
		}
		if refFilter != "" && !strings.Contains(strings.ToLower(ref), refFilter) {
			continue
		}
		if tagFilter != "" && tag != tagFilter {
			continue
		}
		rows = append(rows, &imageRow{
			ID: n.ID, Name: n.Name, Role: role, Ref: ref, Tag: tag,
			File: n.FilePath, Line: n.StartLine,
			Consumers: consumers[n.ID],
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Role != rows[j].Role {
			return rows[i].Role < rows[j].Role
		}
		if rows[i].Ref != rows[j].Ref {
			return rows[i].Ref < rows[j].Ref
		}
		return rows[i].Tag < rows[j].Tag
	})
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"images": rows,
		"total":  len(rows),
	})
}

// handleAnalyzeKustomize surfaces the KindKustomization graph layer.
// Returns one row per overlay with its base count and resource count
// (computed from outgoing EdgeDependsOn / EdgeReferences fan-out).
//
// Filters:
//   - dir:  substring match on the overlay's directory.
func (s *Server) handleAnalyzeKustomize(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	dirFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "dir")))

	type overlayRow struct {
		ID        string `json:"id"`
		Dir       string `json:"dir"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		DependsOn int    `json:"depends_on"`
		Resources int    `json:"resources"`
	}

	type counts struct{ deps, res int }
	tally := make(map[string]*counts)
	bump := func(id string, edge graph.EdgeKind) {
		c, ok := tally[id]
		if !ok {
			c = &counts{}
			tally[id] = c
		}
		switch edge {
		case graph.EdgeDependsOn:
			c.deps++
		case graph.EdgeReferences:
			c.res++
		}
	}
	for e := range edgesByKinds(s.readerFor(ctx), graph.EdgeDependsOn, graph.EdgeReferences) {
		bump(e.From, e.Kind)
	}

	var rows []*overlayRow
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindKustomization {
			continue
		}
		dir, _ := n.Meta["dir"].(string)
		if dir == "" {
			dir = n.Name
		}
		if dirFilter != "" && !strings.Contains(strings.ToLower(dir), dirFilter) {
			continue
		}
		c := tally[n.ID]
		if c == nil {
			c = &counts{}
		}
		rows = append(rows, &overlayRow{
			ID: n.ID, Dir: dir,
			File: n.FilePath, Line: n.StartLine,
			DependsOn: c.deps, Resources: c.res,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Dir < rows[j].Dir })
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"kustomizations": rows,
		"total":          len(rows),
	})
}
