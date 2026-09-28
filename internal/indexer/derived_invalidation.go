package indexer

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	sourceDerivedDeclFingerprintMeta     = "source_derived_decl_fingerprint"
	sourceDerivedImportFingerprintMeta   = "source_derived_import_fingerprint"
	sourceDerivedRuntimeFingerprintMeta  = "source_derived_runtime_fingerprint"
	sourceDerivedArtifactFingerprintMeta = "source_derived_artifact_fingerprint"
	// sourceDerivedHierarchyFingerprintMeta is the part of the declarations
	// fingerprint the hierarchy passes read: type-like and method nodes and
	// the declaration edges (derivedFingerprints.hierarchy).
	sourceDerivedHierarchyFingerprintMeta = "source_derived_hierarchy_fingerprint"
)

type DerivedInvalidationFlags uint32

const (
	DerivedInvalidatesDeclarations DerivedInvalidationFlags = 1 << iota
	DerivedInvalidatesImports
	DerivedInvalidatesRuntime
	DerivedInvalidatesArtifacts
	DerivedInvalidatesTests
	DerivedInvalidatesContracts
)

func (f DerivedInvalidationFlags) Has(flag DerivedInvalidationFlags) bool { return f&flag != 0 }

// ContractGroupFrontier is the exact matcher bucket affected by a contract
// refresh. Contract IDs alone are not sufficient: identical routes/topics/RPC
// names in different workspace/project boundaries must remain isolated.
type ContractGroupFrontier struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	ContractID  string `json:"contract_id,omitempty"`
}

// DerivedInvalidationPlan is the bounded work contract carried from extraction
// through reconcile. Files, TypeIDs, contract groups, and contract symbols are
// exact frontiers, not repo-wide hints.
type DerivedInvalidationPlan struct {
	Flags                  DerivedInvalidationFlags `json:"flags,omitempty"`
	Files                  []string                 `json:"files,omitempty"`
	TypeIDs                []string                 `json:"type_ids,omitempty"`
	ContractGroups         []ContractGroupFrontier  `json:"contract_groups,omitempty"`
	ContractSymbolIDs      []string                 `json:"contract_symbol_ids,omitempty"`
	ContractBridgeNodeIDs  []string                 `json:"contract_bridge_node_ids,omitempty"`
	BodyOnlyFiles          int                      `json:"body_only_files,omitempty"`
	MetadataOnlyFiles      int                      `json:"metadata_only_files,omitempty"`
	InertFiles             int                      `json:"inert_files,omitempty"`
	LegacyFallback         bool                     `json:"legacy_fallback,omitempty"`
	CSharpHierarchyChanged bool                     `json:"csharp_hierarchy_changed,omitempty"`
	// HierarchyUnchanged are the files whose hierarchy fingerprint
	// (derivedFingerprints.hierarchy) the change left as it was: their
	// inferred implements/overrides rows are the prior ones.
	HierarchyUnchanged []string `json:"hierarchy_unchanged,omitempty"`
}

// hierarchyUnchanged reports whether every file of the plan left its
// hierarchy as it was.
func (p DerivedInvalidationPlan) hierarchyUnchanged() bool {
	if len(p.Files) == 0 || len(p.HierarchyUnchanged) < len(p.Files) {
		return false
	}
	unchanged := make(map[string]struct{}, len(p.HierarchyUnchanged))
	for _, f := range p.HierarchyUnchanged {
		unchanged[f] = struct{}{}
	}
	for _, f := range p.Files {
		if _, ok := unchanged[f]; !ok {
			return false
		}
	}
	return true
}

func (p DerivedInvalidationPlan) Empty() bool {
	return p.Flags == 0 && len(p.Files) == 0 && len(p.ContractGroups) == 0 &&
		len(p.ContractSymbolIDs) == 0 && len(p.ContractBridgeNodeIDs) == 0 &&
		p.BodyOnlyFiles == 0 && p.MetadataOnlyFiles == 0 && p.InertFiles == 0 &&
		!p.CSharpHierarchyChanged
}

func (p *DerivedInvalidationPlan) Merge(other DerivedInvalidationPlan) {
	if p == nil {
		return
	}
	p.Flags |= other.Flags
	p.BodyOnlyFiles += other.BodyOnlyFiles
	p.MetadataOnlyFiles += other.MetadataOnlyFiles
	p.InertFiles += other.InertFiles
	p.LegacyFallback = p.LegacyFallback || other.LegacyFallback
	p.CSharpHierarchyChanged = p.CSharpHierarchyChanged || other.CSharpHierarchyChanged
	p.Files = appendUniqueSorted(p.Files, other.Files...)
	p.TypeIDs = appendUniqueSorted(p.TypeIDs, other.TypeIDs...)
	p.HierarchyUnchanged = appendUniqueSorted(p.HierarchyUnchanged, other.HierarchyUnchanged...)
	p.ContractGroups = mergeContractGroups(p.ContractGroups, other.ContractGroups...)
	p.ContractSymbolIDs = appendUniqueSorted(p.ContractSymbolIDs, other.ContractSymbolIDs...)
	p.ContractBridgeNodeIDs = appendUniqueSorted(p.ContractBridgeNodeIDs, other.ContractBridgeNodeIDs...)
}

func mergeContractGroups(dst []ContractGroupFrontier, values ...ContractGroupFrontier) []ContractGroupFrontier {
	seen := make(map[string]ContractGroupFrontier, len(dst)+len(values))
	for _, group := range append(append([]ContractGroupFrontier(nil), dst...), values...) {
		if group.ContractID == "" {
			continue
		}
		seen[contractGroupKey(group)] = group
	}
	out := make([]ContractGroupFrontier, 0, len(seen))
	for _, group := range seen {
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool {
		return contractGroupKey(out[i]) < contractGroupKey(out[j])
	})
	return out
}

func contractGroupKey(group ContractGroupFrontier) string {
	return group.WorkspaceID + "\x00" + group.ProjectID + "\x00" + group.ContractID
}

func appendUniqueSorted(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, value := range dst {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type derivedFingerprints struct {
	declarations string
	imports      string
	runtime      string
	artifacts    string
	// hierarchy is optional (rows stamped before it have none): the
	// declarations the implements/overrides inference and the entry-point
	// hierarchy read. A save that leaves it unchanged moves no type's
	// hierarchy, whatever else it declares.
	hierarchy string
}

// isHierarchyNodeKind reports the declaration kinds the hierarchy passes read:
// types and their methods, not free functions or fields.
func isHierarchyNodeKind(kind graph.NodeKind) bool {
	switch strings.ToLower(string(kind)) {
	case "type", "interface", "class", "trait", "struct", "enum", "method":
		return true
	default:
		return false
	}
}

func (f derivedFingerprints) complete() bool {
	return f.declarations != "" && f.imports != "" && f.runtime != "" && f.artifacts != ""
}

func isDeclarationNodeKind(kind graph.NodeKind) bool {
	switch strings.ToLower(string(kind)) {
	case "type", "interface", "class", "trait", "struct", "enum", "function", "method", "field":
		return true
	default:
		return false
	}
}

func isImportNodeKind(kind graph.NodeKind) bool {
	value := strings.ToLower(string(kind))
	return value == "import" || value == "module" || value == "package"
}

func edgeKindContains(kind graph.EdgeKind, needles ...string) bool {
	value := strings.ToLower(string(kind))
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func isDeclarationEdgeKind(kind graph.EdgeKind) bool {
	return edgeKindContains(kind, "extend", "implement", "override", "compose", "inherit", "satisf")
}

func isImportEdgeKind(kind graph.EdgeKind) bool {
	return edgeKindContains(kind, "import", "export", "depend", "module", "reexport")
}

func isRuntimeDerivedEdgeKind(kind graph.EdgeKind) bool {
	return edgeKindContains(kind,
		"call", "read", "write", "emit", "publish", "subscrib", "dispatch", "register",
		"route", "endpoint", "handler", "invoke", "message", "channel", "event", "config")
}

func stampDerivedFingerprints(result *parser.ExtractionResult, fingerprints derivedFingerprints) {
	if result == nil || !fingerprints.complete() {
		return
	}
	for _, node := range result.Nodes {
		if node == nil || node.Kind != graph.KindFile {
			continue
		}
		if node.Meta == nil {
			node.Meta = map[string]any{}
		}
		node.Meta[sourceDerivedDeclFingerprintMeta] = fingerprints.declarations
		node.Meta[sourceDerivedImportFingerprintMeta] = fingerprints.imports
		node.Meta[sourceDerivedRuntimeFingerprintMeta] = fingerprints.runtime
		node.Meta[sourceDerivedArtifactFingerprintMeta] = fingerprints.artifacts
		if fingerprints.hierarchy != "" {
			node.Meta[sourceDerivedHierarchyFingerprintMeta] = fingerprints.hierarchy
		}
		return
	}
}

func storedDerivedFingerprints(nodes []*graph.Node) derivedFingerprints {
	for _, node := range nodes {
		if node == nil || node.Kind != graph.KindFile || node.Meta == nil {
			continue
		}
		return derivedFingerprints{
			declarations: stringMeta(node.Meta, sourceDerivedDeclFingerprintMeta),
			imports:      stringMeta(node.Meta, sourceDerivedImportFingerprintMeta),
			runtime:      stringMeta(node.Meta, sourceDerivedRuntimeFingerprintMeta),
			artifacts:    stringMeta(node.Meta, sourceDerivedArtifactFingerprintMeta),
			hierarchy:    stringMeta(node.Meta, sourceDerivedHierarchyFingerprintMeta),
		}
	}
	return derivedFingerprints{}
}

func stringMeta(meta map[string]any, key string) string {
	value, _ := meta[key].(string)
	return value
}

func derivedFingerprintSideExists(nodes []*graph.Node) bool {
	for _, node := range nodes {
		if node != nil && node.Kind == graph.KindFile {
			return true
		}
	}
	return false
}

func containsCSharpHierarchyNode(nodes []*graph.Node) bool {
	for _, node := range nodes {
		if node == nil || !strings.EqualFold(node.Language, "csharp") {
			continue
		}
		if node.Kind == graph.KindType || node.Kind == graph.KindInterface {
			return true
		}
	}
	return false
}

func derivedPlanForDelta(prior, fresh derivedFingerprints, semanticChanged bool, graphPath string, priorNodes, freshNodes []*graph.Node) DerivedInvalidationPlan {
	plan := DerivedInvalidationPlan{Files: []string{graphPath}}
	if !prior.complete() || !fresh.complete() {
		plan.Flags = DerivedInvalidatesDeclarations | DerivedInvalidatesImports | DerivedInvalidatesRuntime | DerivedInvalidatesArtifacts
		plan.LegacyFallback = (derivedFingerprintSideExists(priorNodes) && !prior.complete()) ||
			(derivedFingerprintSideExists(freshNodes) && !fresh.complete())
	} else {
		if prior.declarations != fresh.declarations {
			plan.Flags |= DerivedInvalidatesDeclarations
		}
		if prior.imports != fresh.imports {
			plan.Flags |= DerivedInvalidatesImports
		}
		if prior.runtime != fresh.runtime {
			plan.Flags |= DerivedInvalidatesRuntime
		}
		if prior.artifacts != fresh.artifacts {
			plan.Flags |= DerivedInvalidatesArtifacts
		}
	}
	if semanticChanged && looksLikeTestPath(graphPath) {
		plan.Flags |= DerivedInvalidatesTests
	}
	if semanticChanged && plan.Flags == 0 {
		plan.BodyOnlyFiles = 1
	}
	for _, nodes := range [][]*graph.Node{priorNodes, freshNodes} {
		for _, node := range nodes {
			if node != nil && isTypeFrontierNodeKind(node.Kind) {
				plan.TypeIDs = append(plan.TypeIDs, node.ID)
			}
		}
	}
	plan.TypeIDs = appendUniqueSorted(nil, plan.TypeIDs...)
	if prior.complete() && fresh.complete() && prior.hierarchy != "" && prior.hierarchy == fresh.hierarchy {
		// The declarations moved, but none the implements/overrides
		// inference reads.
		plan.HierarchyUnchanged = []string{graphPath}
	}
	plan.CSharpHierarchyChanged = plan.Flags.Has(DerivedInvalidatesDeclarations) &&
		(containsCSharpHierarchyNode(priorNodes) || containsCSharpHierarchyNode(freshNodes))
	return plan
}

func isTypeFrontierNodeKind(kind graph.NodeKind) bool {
	switch strings.ToLower(string(kind)) {
	case "type", "interface", "class", "trait", "struct", "enum":
		return true
	default:
		return false
	}
}

func looksLikeTestPath(path string) bool {
	value := strings.ToLower(path)
	return strings.Contains(value, "/test/") || strings.Contains(value, "/tests/") ||
		strings.Contains(value, "_test.") || strings.Contains(value, ".test.") ||
		strings.Contains(value, ".spec.") || strings.HasSuffix(value, "test")
}
