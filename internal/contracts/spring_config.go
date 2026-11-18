package contracts

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Spring configuration-key graph. application(-profile)?.{yml,yaml,properties}
// leaf keys become KindConfigKey nodes (values REDACTED — key-only, so a secret
// in application.yml never enters the graph), and @Value("${k:default}") /
// @ConfigurationProperties(prefix) reads become EdgeReadsConfig edges to them.
// Both sides canonicalize the key the way Spring's relaxed binding does, so a
// kebab-case property file key binds to a camelCase @Value reference — letting
// "which beans read this property" resolve in one traversal over the existing
// reads_config capability surface.

// canonicalizeSpringKey applies Spring relaxed binding: per dot-separated
// segment, lowercase and drop '-' and '_'. So my-prop / myProp / MY_PROP and
// my.prop's leaf all canonicalize to the same form, letting a property-file key
// and a code reference bind regardless of which relaxed spelling each used.
func canonicalizeSpringKey(key string) string {
	segs := strings.Split(key, ".")
	for i, s := range segs {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "-", "")
		s = strings.ReplaceAll(s, "_", "")
		segs[i] = s
	}
	return strings.Join(segs, ".")
}

// springConfigKeyID is the canonical KindConfigKey node ID for a Spring property.
func springConfigKeyID(key string) string {
	return "cfg::spring::" + canonicalizeSpringKey(key)
}

// SpringConfigScope identifies the repository boundary for one binding pass.
// RepoRoot is the on-disk root used to resolve graph paths; RepoPrefix and
// WorkspaceID are persisted on synthetic nodes and form their repo-scoped ID
// namespace. An empty RepoPrefix keeps the unscoped `cfg::spring::<key>` IDs
// and is reached only by the standalone Indexer, which sets no prefix — every
// repo the daemon tracks carries one.
type SpringConfigScope struct {
	RepoPrefix  string
	RepoRoot    string
	WorkspaceID string
	// ReadSource optionally serves exact accepted graph paths. Snapshot callers
	// retain read errors independently; nil preserves the legacy disk source.
	ReadSource func(string) ([]byte, error)
}

func scopedSpringConfigKeyID(scope SpringConfigScope, key string) string {
	canonical := canonicalizeSpringKey(key)
	if scope.RepoPrefix == "" {
		return "cfg::spring::" + canonical
	}
	workspace := scope.WorkspaceID
	if workspace == "" {
		workspace = scope.RepoPrefix
	}
	return "cfg::spring::" + workspace + "::" + scope.RepoPrefix + "::" + canonical
}

func springConfigSource(scope SpringConfigScope, graphPath string) []byte {
	path := strings.ReplaceAll(graphPath, "\\", "/")
	if scope.RepoPrefix != "" {
		path = strings.TrimPrefix(path, strings.TrimSuffix(scope.RepoPrefix, "/")+"/")
	}
	path = filepath.Clean(filepath.FromSlash(path))
	if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return nil
	}
	if scope.ReadSource != nil {
		data, err := scope.ReadSource(graphPath)
		if err != nil {
			return nil
		}
		return data
	}
	data, err := os.ReadFile(filepath.Join(scope.RepoRoot, path))
	if err != nil {
		return nil
	}
	return data
}

// springConfigFile reports whether path is a Spring application config file and
// returns its profile (the `-prod` in application-prod.yml; "" for the base).
func springConfigFile(path string) (profile string, ok bool) {
	path = strings.ReplaceAll(path, "\\", "/")
	base := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		base = path[i+1:]
	}
	for _, ext := range []string{".yml", ".yaml", ".properties"} {
		if !strings.HasSuffix(base, ext) {
			continue
		}
		stem := strings.TrimSuffix(base, ext)
		switch {
		case stem == "application":
			return "", true
		case strings.HasPrefix(stem, "application-"):
			return stem[len("application-"):], true
		}
	}
	return "", false
}

// ExtractSpringConfigKeys parses every leaf key from a Spring application config
// file into a value-redacted KindConfigKey node. The node's Name is the raw
// (developer-written) key for readability; the canonical form is the ID.
func ExtractSpringConfigKeys(filePath string, src []byte, profile string) []*graph.Node {
	var keys []string
	if strings.HasSuffix(filePath, ".properties") {
		keys = parsePropertiesKeys(src)
	} else {
		keys = parseYAMLLeafKeys(src)
	}
	var out []*graph.Node
	seen := map[string]bool{}
	for _, k := range keys {
		id := springConfigKeyID(k)
		if k == "" || seen[id] {
			continue
		}
		seen[id] = true
		meta := map[string]any{
			"source":         "spring",
			"raw_key":        k,
			"value_redacted": true,
		}
		if profile != "" {
			meta["profile"] = profile
		}
		out = append(out, &graph.Node{
			ID: id, Kind: graph.KindConfigKey, Name: k,
			FilePath: filePath, StartLine: 1, Meta: meta,
		})
	}
	return out
}

// BindSpringConfig emits Spring config-key nodes from application config files
// in one explicit repository/workspace (read below scope.RepoRoot) and a
// reads_config edge from each scoped bean carrying spring_config_keys Meta
// (stamped by the Java extractor on a
// @Value / @ConfigurationProperties bean) to the key it reads — with relaxed
// canonicalization so spelling differences still bind, and a `*` suffix
// (@ConfigurationProperties prefix) fanning out to every key under the prefix.
// Returns the number of nodes + edges added.
func BindSpringConfig(g graph.Store, scope SpringConfigScope) int {
	if g == nil || scope.RepoRoot == "" {
		return 0
	}

	type target struct {
		desired string
		legacy  string
	}
	projectedFiles := graph.ReadRepoFilePaths(g, scope.RepoPrefix, scope.WorkspaceID, []string{"java"}, []string{".yml", ".yaml", ".properties"})
	if len(projectedFiles) == 0 {
		return 0
	}
	currentFiles := make(map[string]struct{}, len(projectedFiles))
	desiredNodes := make(map[string]*graph.Node)
	legacyIDs := make(map[string]struct{})
	byPrefix := make(map[string][]target)
	for _, filePath := range projectedFiles {
		profile, ok := springConfigFile(filePath)
		if !ok {
			continue
		}
		currentFiles[filePath] = struct{}{}
		src := springConfigSource(scope, filePath)
		if src == nil {
			continue
		}
		for _, node := range ExtractSpringConfigKeys(filePath, src, profile) {
			legacyID := node.ID
			node.ID = scopedSpringConfigKeyID(scope, node.Name)
			node.RepoPrefix = scope.RepoPrefix
			node.WorkspaceID = scope.WorkspaceID
			if _, duplicate := desiredNodes[node.ID]; !duplicate {
				desiredNodes[node.ID] = node
			}
			if legacyID != node.ID {
				legacyIDs[legacyID] = struct{}{}
			}
			canonical := canonicalizeSpringKey(node.Name)
			if dot := strings.LastIndexByte(canonical, '.'); dot >= 0 {
				prefix := canonical[:dot]
				byPrefix[prefix] = append(byPrefix[prefix], target{desired: node.ID, legacy: legacyID})
			}
		}
	}
	for prefix, targets := range byPrefix {
		sort.Slice(targets, func(i, j int) bool {
			if targets[i].desired != targets[j].desired {
				return targets[i].desired < targets[j].desired
			}
			return targets[i].legacy < targets[j].legacy
		})
		unique := targets[:0]
		for _, candidate := range targets {
			if len(unique) == 0 || unique[len(unique)-1] != candidate {
				unique = append(unique, candidate)
			}
		}
		byPrefix[prefix] = unique
	}

	lookupIDs := make([]string, 0, len(desiredNodes)+len(legacyIDs))
	for id := range desiredNodes {
		lookupIDs = append(lookupIDs, id)
	}
	for id := range legacyIDs {
		lookupIDs = append(lookupIDs, id)
	}
	sort.Strings(lookupIDs)
	existingNodes := g.GetNodesByIDs(lookupIDs)
	pendingNodes := make([]*graph.Node, 0, len(desiredNodes))
	for _, id := range lookupIDs {
		if node := desiredNodes[id]; node != nil && existingNodes[id] == nil {
			pendingNodes = append(pendingNodes, node)
		}
	}

	readers := springReaderNodes(g, scope)
	readerIDs := make([]string, 0, len(readers))
	legacyTargets := make(map[string]map[string]struct{}, len(readers))
	desiredEdges := make([]*graph.Edge, 0)
	seenDesired := make(map[string]struct{})
	emit := func(reader *graph.Node, candidate target, raw string) {
		identity := reader.ID + "\x00" + candidate.desired
		if _, duplicate := seenDesired[identity]; duplicate {
			return
		}
		seenDesired[identity] = struct{}{}
		desiredEdges = append(desiredEdges, &graph.Edge{
			From: reader.ID, To: candidate.desired, Kind: graph.EdgeReadsConfig,
			FilePath: reader.FilePath, Line: reader.StartLine,
			Meta: map[string]any{"via": "spring_value", "raw_key": raw},
		})
		if candidate.legacy != candidate.desired {
			targets := legacyTargets[reader.ID]
			if targets == nil {
				targets = make(map[string]struct{})
				legacyTargets[reader.ID] = targets
			}
			targets[candidate.legacy] = struct{}{}
		}
	}
	for _, reader := range readers {
		readerIDs = append(readerIDs, reader.ID)
		for _, key := range springConfigKeysOf(reader) {
			if strings.HasSuffix(key, "*") {
				prefix := canonicalizeSpringKey(strings.TrimSuffix(strings.TrimSuffix(key, "*"), "."))
				for _, candidate := range byPrefix[prefix] {
					emit(reader, candidate, key)
				}
				continue
			}
			emit(reader, target{
				desired: scopedSpringConfigKeyID(scope, key),
				legacy:  springConfigKeyID(key),
			}, key)
		}
	}

	endpoints := make([]graph.EdgeEndpoint, 0, len(desiredEdges))
	sites := make([]graph.EdgeSite, 0, len(desiredEdges))
	for _, edge := range desiredEdges {
		endpoints = append(endpoints, graph.EdgeEndpoint{From: edge.From, To: edge.To})
		sites = append(sites, graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: edge.Kind})
	}
	candidates := graph.LookupEdgeCandidates(g, endpoints, sites)
	pendingEdges := make([]*graph.Edge, 0, len(desiredEdges))
	for _, edge := range desiredEdges {
		exists := false
		for _, existing := range candidates.Site(edge.From, edge.Line, edge.Kind) {
			if existing != nil && existing.To == edge.To && existing.FilePath == edge.FilePath {
				exists = true
				break
			}
		}
		if !exists {
			pendingEdges = append(pendingEdges, edge)
		}
	}

	if scope.RepoPrefix != "" && len(readerIDs) > 0 {
		outgoing := g.GetOutEdgesByNodeIDs(readerIDs)
		var staleEdges []*graph.Edge
		for _, readerID := range readerIDs {
			for _, edge := range outgoing[readerID] {
				if edge == nil || edge.Kind != graph.EdgeReadsConfig {
					continue
				}
				if _, stale := legacyTargets[readerID][edge.To]; stale {
					staleEdges = append(staleEdges, edge)
				}
			}
		}
		if remover, ok := g.(graph.ExactEdgeBatchRemover); ok && len(staleEdges) > 0 {
			remover.RemoveEdgesExact(staleEdges)
		}
	}

	if scope.RepoPrefix != "" && len(legacyIDs) > 0 {
		staleNodes := make([]string, 0, len(legacyIDs))
		for id := range legacyIDs {
			node := existingNodes[id]
			if node == nil || node.Kind != graph.KindConfigKey || node.Meta == nil || node.Meta["source"] != "spring" {
				continue
			}
			_, currentFile := currentFiles[node.FilePath]
			currentScope := node.RepoPrefix == scope.RepoPrefix && (scope.WorkspaceID == "" || node.WorkspaceID == scope.WorkspaceID)
			if currentFile || currentScope {
				staleNodes = append(staleNodes, id)
			}
		}
		sort.Strings(staleNodes)
		if evicter, ok := g.(graph.ConfigNodeBatchEvicter); ok && len(staleNodes) > 0 {
			evicter.EvictConfigNodesByIDs(staleNodes)
		}
	}

	if len(pendingNodes) > 0 || len(pendingEdges) > 0 {
		g.AddBatch(pendingNodes, pendingEdges)
	}
	return len(pendingNodes) + len(pendingEdges)
}

// springReaderNodes returns the repository/workspace-scoped nodes carrying a
// spring_config_keys hint through one backend projection.
func springReaderNodes(g graph.Store, scope SpringConfigScope) []*graph.Node {
	return graph.ReadRepoNodesByKindsWithMetaKey(g, scope.RepoPrefix, scope.WorkspaceID, []graph.NodeKind{
		graph.KindField, graph.KindType, graph.KindInterface, graph.KindMethod,
	}, "spring_config_keys")
}

// springConfigKeysOf coerces the spring_config_keys Meta value into a slice.
func springConfigKeysOf(n *graph.Node) []string {
	switch t := n.Meta["spring_config_keys"].(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// parsePropertiesKeys returns the keys of a .properties file (left of the first
// '=' or ':' on each non-comment line).
func parsePropertiesKeys(src []byte) []string {
	var keys []string
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		sep := strings.IndexAny(line, "=:")
		if sep <= 0 {
			continue
		}
		if key := strings.TrimSpace(line[:sep]); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// parseYAMLLeafKeys returns the dotted path of every leaf (value-bearing) key in
// a YAML document, tracking nesting by indentation. Sequence items and block
// scalars are skipped — only the mapping keys that name a property are emitted.
func parseYAMLLeafKeys(src []byte) []string {
	type frame struct {
		indent int
		key    string
	}
	var stack []frame
	var keys []string
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		ci := strings.IndexByte(trimmed, ':')
		if ci < 0 {
			continue
		}
		indent := len(line) - len(trimmed)
		key := strings.TrimSpace(trimmed[:ci])
		val := strings.TrimSpace(trimmed[ci+1:])
		// Pop frames at or deeper than this indent — we've left their scope.
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parts := make([]string, 0, len(stack)+1)
		for _, f := range stack {
			parts = append(parts, f.key)
		}
		parts = append(parts, key)
		dotted := strings.Join(parts, ".")
		if val != "" && val != "|" && val != ">" && val != "|-" && val != ">-" {
			keys = append(keys, dotted)
		} else {
			stack = append(stack, frame{indent: indent, key: key})
		}
	}
	return keys
}
