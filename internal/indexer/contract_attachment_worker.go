package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

// ContractFollowupFile is one member of a complete accepted source census.
// Path is the indexed graph path; fingerprints/policy bind historical bytes.
type ContractFollowupFile struct {
	RepoPrefix, WorkspaceID, ProjectID        string
	Path, Language, SourceFingerprint, Policy string
}

type ContractAcceptedSource struct {
	Bytes                     []byte
	SourceFingerprint, Policy string
}

// ContractFollowupCoreFile contains complete selected declaration and outgoing
// edge evidence. The caller must use checked reads and reject truncation.
type ContractFollowupCoreFile struct {
	Nodes []*graph.Node
	Edges []*graph.Edge
}

// ContractFollowupSnapshot is captured before queue admission under the actual
// selected view handoff. Files must be the complete accepted namespace census,
// not only Work's changed files. ReadAccepted serves accepted transformed
// extractor bytes with their exact source/policy proof; it never falls back to current
// filesystem bytes. The runner owns Release on every exit.
type ContractFollowupSnapshot struct {
	Key          graph.ContractAttachmentKey
	Core         graph.Reader
	Inputs       []graph.ContractInputWitness
	Work         []graph.ContractWork
	Files        []ContractFollowupFile
	ReadAccepted func(context.Context, ContractFollowupFile) (ContractAcceptedSource, error)
	ReadCoreFile func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error)
	Release      func()
}

// ContractFollowupRequest receives an installer-reserved Building managed
// payload. Catalog retains the selected core generation used for CAS; it is
// not an actor-latest handle. Leases is the materializer's shared lease manager.
// Yield lets the existing scheduler defer to interactive load between files.
type ContractFollowupRebuildReason string

const (
	ContractFollowupColdBaseline    ContractFollowupRebuildReason = "cold_baseline"
	ContractFollowupUnknownInterval ContractFollowupRebuildReason = "unknown_interval"
	ContractFollowupChangedBoundary ContractFollowupRebuildReason = "changed_boundary"
)

type ContractFollowupRequest struct {
	Snapshot ContractFollowupSnapshot
	// Explicit full rebuild reason makes boundary-edit cost visible. Unchanged
	// identity reuses the exact attachment upstream and must schedule no worker.
	RebuildReason          ContractFollowupRebuildReason
	Payload, Catalog       *store_sqlite.Store
	Registry               *parser.Registry
	Config                 config.IndexConfig
	Logger                 *zap.Logger
	Leases                 *graphview.LeaseManager
	WorkspaceID, ProjectID string
	RepoConfigs            map[string]config.IndexConfig
	Yield                  func(context.Context) error
}

type ContractFollowupReport struct {
	Files, Records, Nodes, Edges, Shapes             int
	SourceReads, SourceBytes                         int
	RebuildReason                                    ContractFollowupRebuildReason
	ExtractDuration, EnrichDuration, PublishDuration time.Duration
	PayloadGeneration                                int64
	Published                                        bool
}

// RunContractFollowup performs no ordinary core indexing or route publication.
// All legacy contract enrichment operates on a detached private evidence graph.
func RunContractFollowup(ctx context.Context, req ContractFollowupRequest) (report ContractFollowupReport, err error) {
	snap := req.Snapshot
	if snap.Release != nil {
		defer snap.Release()
	}
	if ctx == nil || snap.Release == nil || snap.Core == nil || snap.ReadAccepted == nil || snap.ReadCoreFile == nil || req.Payload == nil || req.Catalog == nil || req.Leases == nil || req.Yield == nil {
		return report, fmt.Errorf("contract followup: incomplete selected handoff")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if req.RebuildReason != ContractFollowupColdBaseline && req.RebuildReason != ContractFollowupUnknownInterval && req.RebuildReason != ContractFollowupChangedBoundary {
		return report, fmt.Errorf("contract followup: explicit rebuild reason required")
	}
	report.RebuildReason = req.RebuildReason
	generation := req.Payload.ViewGeneration()
	if generation <= 0 {
		return report, fmt.Errorf("contract followup: payload must be isolated positive generation")
	}
	lease := req.Leases.Acquire(generation)
	defer lease.Release()
	report.PayloadGeneration = generation
	logical, err := graph.ComposeContractInputState(snap.Key.RepoPrefix, snap.Key.CheckoutID, snap.Inputs)
	if err != nil {
		return report, err
	}
	if !logical.Accepted || logical.InputVersion != snap.Key.InputVersion || logical.InputFingerprint != snap.Key.InputFingerprint {
		return report, graph.ErrContractProjectionStale
	}
	fileByPath := make(map[string]ContractFollowupFile, len(snap.Files))
	repos := make(map[string]bool)
	for _, w := range snap.Inputs {
		if w.Found && w.State.Accepted {
			repos[w.State.RepoPrefix] = true
		}
	}
	for _, file := range snap.Files {
		if file.Path == "" || file.SourceFingerprint == "" || file.Policy == "" || !repos[file.RepoPrefix] {
			return report, fmt.Errorf("contract followup: uncertified source census member %q", file.Path)
		}
		if _, duplicate := fileByPath[file.Path]; duplicate {
			return report, fmt.Errorf("contract followup: duplicate source path %q", file.Path)
		}
		if file.RepoPrefix != "" && !strings.HasPrefix(file.Path, file.RepoPrefix+"/") {
			return report, fmt.Errorf("contract followup: source namespace mismatch %q", file.Path)
		}
		fileByPath[file.Path] = file
	}
	evidence := &contractFollowupEvidence{Graph: graph.New(), ctx: ctx, core: snap.Core, files: fileByPath, readCore: snap.ReadCoreFile, allowedRepos: repos}
	logger := req.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	indexers := make(map[string]*Indexer)
	readSource := func(path string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, ok := fileByPath[path]
		if !ok {
			return nil, fmt.Errorf("contract followup: accepted source outside census %q", path)
		}
		accepted, err := snap.ReadAccepted(ctx, file)
		if err != nil {
			return nil, err
		}
		if accepted.SourceFingerprint != file.SourceFingerprint || accepted.Policy != file.Policy || contractInputHash(accepted.Bytes) != file.SourceFingerprint {
			return nil, fmt.Errorf("%w: accepted source proof %s", errContractInputsChanged, path)
		}
		report.SourceReads++
		report.SourceBytes += len(accepted.Bytes)
		return bytes.Clone(accepted.Bytes), nil
	}
	for _, file := range snap.Files {
		if prior := indexers[file.RepoPrefix]; prior != nil {
			if prior.workspaceID != file.WorkspaceID || prior.projectID != file.ProjectID {
				return report, fmt.Errorf("contract followup: inconsistent accepted namespace scope %q", file.RepoPrefix)
			}
			continue
		}
		cfg := req.Config
		if override, ok := req.RepoConfigs[file.RepoPrefix]; ok {
			cfg = override
		}
		idx := &Indexer{graph: evidence, rootPath: filepath.Join(string(filepath.Separator), "contract-followup", file.RepoPrefix), repoPrefix: file.RepoPrefix, workspaceID: file.WorkspaceID, projectID: file.ProjectID, config: cfg, logger: logger, registry: req.Registry}
		idx.contractAnalysisOnly = true
		idx.contractAcceptedFileSource = readSource
		if reader, ok := snap.Core.(graph.SemanticBindingTypeReader); ok {
			idx.contractSemanticReader = reader
		}
		indexers[file.RepoPrefix] = idx
	}
	// A certified empty own namespace still receives a complete empty snapshot.
	if indexers[snap.Key.RepoPrefix] == nil {
		idx := &Indexer{graph: evidence, rootPath: "/contract-followup", repoPrefix: snap.Key.RepoPrefix, workspaceID: req.WorkspaceID, projectID: req.ProjectID, config: req.Config, logger: logger, registry: req.Registry}
		idx.contractAnalysisOnly = true
		idx.contractAcceptedFileSource = readSource
		indexers[snap.Key.RepoPrefix] = idx
	}
	files := slices.Clone(snap.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	extractStart := time.Now()
	registry := contracts.NewRegistry()
	for _, file := range files {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		idx := indexers[file.RepoPrefix]
		if policy, err := contractFollowupPolicy(idx, file.Language); err != nil || policy != file.Policy {
			if err != nil {
				return report, err
			}
			return report, fmt.Errorf("contract followup: accepted policy mismatch %s", file.Path)
		}
		nodes, edges, err := evidence.loadFile(file.Path)
		if err != nil {
			return report, err
		}
		src, err := readSource(file.Path)
		if err != nil {
			return report, err
		}
		_, byLanguage := idx.buildPerFileContractExtractors()
		tree := contracts.ParseTreeForLang(file.Language, src)
		found := idx.collectContractRecordsForFile(file.Path, src, nodes, edges, byLanguage[file.Language], tree, evidence)
		if tree != nil {
			tree.Release()
		}
		if file.Language == "gomod" || strings.HasSuffix(file.Path, "/go.mod") || file.Path == "go.mod" {
			found = append(found, (&contracts.GoModExtractor{}).Extract(file.Path, src, nodes, edges)...)
		}
		registry.AddAllScoped(found, file.RepoPrefix, file.WorkspaceID, file.ProjectID)
		report.Files++
		if evidence.err != nil {
			return report, evidence.err
		}
	}
	report.ExtractDuration = time.Since(extractStart)
	enrichStart := time.Now()
	// All postpasses see only detached evidence and proof-bound source reads.
	prefixes := make([]string, 0, len(indexers))
	for repo := range indexers {
		prefixes = append(prefixes, repo)
	}
	sort.Strings(prefixes)
	for _, repo := range prefixes {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		idx := indexers[repo]
		local := contracts.NewRegistry()
		local.AddAll(registry.ByRepo(repo), repo)
		idx.extractDIContracts(local)
		idx.upgradeContractBareTypeRefs(local)
		idx.resolveProviderHandlers(local)
		if scans := idx.routerPrefixScanFiles(local); len(scans) > 0 {
			contracts.JoinRouterPrefixes(local, scans, idx.contractFileSrc)
		}
		contracts.BindSpringConfig(evidence, contracts.SpringConfigScope{RepoPrefix: repo, RepoRoot: idx.rootPath, WorkspaceID: idx.workspaceID, ReadSource: func(path string) ([]byte, error) {
			data, err := readSource(path)
			if err != nil {
				idx.rememberContractInputError(err)
			}
			return data, err
		}})
		idx.resolveCallReturnTypes(local)
		idx.snapshotContractShapes(local)
		idx.inlineEnvelopeShapes(local)
		if err := idx.contractInputError(); err != nil {
			return report, err
		}
		if evidence.err != nil {
			return report, evidence.err
		}
		for _, id := range registry.AllIDs() {
			registry.ReplaceByID(id, contractFollowupOtherOwners(registry.ByID(id), repo))
		}
		registry.AddAll(local.All(), repo)
	}
	own := registry.ByRepo(snap.Key.RepoPrefix)
	nodes, allOwnerEdges, missing := contractGraphRows(evidence, registry.All(), true)
	var edges []*graph.Edge
	for _, edge := range allOwnerEdges {
		if owner, _ := edge.Meta["contract_owner_repo_prefix"].(string); owner == snap.Key.RepoPrefix {
			edges = append(edges, edge)
		}
	}
	if missing != 0 {
		return report, fmt.Errorf("contract followup: %d missing selected source owners", missing)
	}
	report.Records = len(own)
	matches := contracts.Match(registry).Matched
	groups := make(map[bridgeGroupKey]bool)
	for _, match := range matches {
		if match.Provider.RepoPrefix == snap.Key.RepoPrefix || match.Consumer.RepoPrefix == snap.Key.RepoPrefix {
			groups[contractLinkBridgeGroup(match)] = true
		}
	}
	var relevant []contracts.CrossLink
	for _, match := range matches {
		if groups[contractLinkBridgeGroup(match)] {
			relevant = append(relevant, match)
			if match.Consumer.SymbolID != "" && match.Provider.SymbolID != "" {
				edges = append(edges, contractMatchEdge(match))
			}
		}
	}
	bridgeNodes, bridgeEdges := buildContractBridgeBatch(relevant)
	nodes = append(nodes, bridgeNodes...)
	edges = append(edges, bridgeEdges...)
	for _, node := range evidence.AllNodes() {
		if (node.Kind == graph.KindType || node.Kind == graph.KindInterface) && node.Meta["shape"] != nil {
			nodes = append(nodes, node)
			report.Shapes++
		} else if node.Kind == graph.KindConfigKey {
			nodes = append(nodes, node)
		}
	}
	for _, edge := range evidence.AllEdges() {
		if edge.Kind == graph.EdgeReadsConfig {
			edges = append(edges, edge)
		}
	}
	if evidence.err != nil {
		return report, evidence.err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.EnrichDuration = time.Since(enrichStart)
	publishStart := time.Now()
	for start := 0; start < len(nodes); start += contractFrontierReadBatchSize {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := req.Payload.AddBatchChecked(nodes[start:min(start+contractFrontierReadBatchSize, len(nodes))], nil); err != nil {
			return report, err
		}
	}
	for start := 0; start < len(edges); start += contractFrontierReadBatchSize {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := req.Payload.AddBatchChecked(nil, edges[start:min(start+contractFrontierReadBatchSize, len(edges))]); err != nil {
			return report, err
		}
	}
	report.Nodes = len(nodes)
	report.Edges = len(edges)
	if err := req.Payload.SetProducerState(store_sqlite.ProducerCompleteness{Producer: "graph.contracts", State: store_sqlite.ProducerStateComplete}); err != nil {
		return report, err
	}
	tokens := make([]string, 0, len(snap.Work))
	for _, w := range snap.Work {
		tokens = append(tokens, w.Token)
	}
	attachment := graph.ContractAttachment{RepoPrefix: snap.Key.RepoPrefix, CheckoutID: snap.Key.CheckoutID, InputVersion: snap.Key.InputVersion, InputFingerprint: snap.Key.InputFingerprint, PayloadGeneration: generation, CompletedTokens: tokens}
	if err := req.Catalog.PublishContractAttachmentWithInputsContext(ctx, logical, snap.Inputs, attachment, snap.Work, time.Now().Unix()); err != nil {
		return report, err
	}
	report.PublishDuration = time.Since(publishStart)
	report.Published = true
	return report, nil
}

func contractFollowupOtherOwners(rows []contracts.Contract, repo string) []contracts.Contract {
	var out []contracts.Contract
	for _, row := range rows {
		if row.RepoPrefix != repo {
			out = append(out, row)
		}
	}
	return out
}
func contractFollowupPolicy(idx *Indexer, language string) (string, error) {
	encoded, err := json.Marshal(struct {
		Config                                    any
		EventBus                                  any
		Parser, PostExtraction                    int
		ContractPolicy, RecordPolicy, MatchPolicy string
	}{idx.config, idx.eventBusBoundaries(), extractorVersionForLang(language), postExtractionPolicyVersion, contractExtractionPolicyVersion, contracts.RecordFingerprintVersion, contracts.MatchDependencyKeyVersion})
	if err != nil {
		return "", err
	}
	return contractInputHash(encoded), nil
}

// Legacy enrichment gets a private graph.Store facade. Every missing core
// lookup is checked and sticky; writes always target the detached Graph.
type contractFollowupEvidence struct {
	*graph.Graph
	ctx          context.Context
	core         graph.Reader
	files        map[string]ContractFollowupFile
	readCore     func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error)
	allowedRepos map[string]bool
	loaded       map[string]bool
	err          error
}

func (e *contractFollowupEvidence) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}
func (e *contractFollowupEvidence) detach(node *graph.Node) *graph.Node {
	if node == nil || node.ID == "" {
		e.fail(fmt.Errorf("contract followup: malformed selected node"))
		return nil
	}
	if !e.allowedRepos[node.RepoPrefix] {
		e.fail(fmt.Errorf("contract followup: unwitnessed selected namespace %s", node.RepoPrefix))
		return nil
	}
	copyNode := *node
	if node.Meta != nil {
		copyNode.Meta = contractFollowupCloneValue(reflect.ValueOf(node.Meta)).Interface().(map[string]any)
	}
	if copyNode.Kind == graph.KindType || copyNode.Kind == graph.KindInterface {
		delete(copyNode.Meta, "shape")
	}
	return &copyNode
}
func contractFollowupCloneValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(contractFollowupCloneValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), contractFollowupCloneValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(contractFollowupCloneValue(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(contractFollowupCloneValue(v.Elem()))
		return out
	default:
		return v
	}
}
func (e *contractFollowupEvidence) loadFile(path string) ([]*graph.Node, []*graph.Edge, error) {
	if e.loaded == nil {
		e.loaded = make(map[string]bool)
	}
	if e.loaded[path] {
		nodes := e.GetFileNodes(path)
		var ids []string
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		var edges []*graph.Edge
		for _, rows := range e.GetOutEdgesByNodeIDs(ids) {
			edges = append(edges, rows...)
		}
		return nodes, edges, e.err
	}
	file, ok := e.files[path]
	if !ok {
		return nil, nil, fmt.Errorf("contract followup: selected file outside accepted census %q", path)
	}
	rows, err := e.readCore(e.ctx, file)
	if err != nil {
		e.fail(err)
		return nil, nil, err
	}
	if len(rows.Nodes)+len(rows.Edges) > graph.ContractProjectionRowLimit {
		e.fail(graph.ErrContractProjectionLimit)
		return nil, nil, e.err
	}
	var nodes []*graph.Node
	fileFound := false
	for _, node := range rows.Nodes {
		if node == nil || node.FilePath != path || node.RepoPrefix != file.RepoPrefix {
			e.fail(fmt.Errorf("contract followup: selected file identity mismatch %q", path))
			return nil, nil, e.err
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge || node.Kind == graph.KindConfigKey {
			continue
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return nil, nil, e.err
		}
		nodes = append(nodes, copyNode)
		if node.Kind == graph.KindFile {
			fileFound = true
		}
	}
	if !fileFound {
		e.fail(fmt.Errorf("contract followup: accepted census file has no selected core owner %q", path))
		return nil, nil, e.err
	}
	var edges []*graph.Edge
	for _, edge := range rows.Edges {
		if edge == nil || edge.From == "" || edge.To == "" {
			e.fail(fmt.Errorf("contract followup: malformed selected edge"))
			return nil, nil, e.err
		}
		// Existing contract output must never seed an isolated new analysis.
		if edge.Kind == graph.EdgeHandlesRoute || edge.Kind == graph.EdgeMatches || edge.Kind == graph.EdgeBridges || edge.Kind == graph.EdgeReadsConfig {
			continue
		}
		if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
			if _, di := edge.Meta[graph.MetaDIBinding]; !di {
				continue
			}
		}
		copyEdge := *edge
		if edge.Meta != nil {
			copyEdge.Meta = contractFollowupCloneValue(reflect.ValueOf(edge.Meta)).Interface().(map[string]any)
		}
		edges = append(edges, &copyEdge)
	}
	e.AddBatch(nodes, edges)
	e.loaded[path] = true
	return nodes, edges, e.err
}
func (e *contractFollowupEvidence) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node, len(paths))
	for _, path := range paths {
		nodes, _, err := e.loadFile(path)
		if err != nil {
			e.fail(err)
			return nil
		}
		out[path] = nodes
	}
	return out
}
func (e *contractFollowupEvidence) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out := e.Graph.GetNodesByIDs(ids)
	var missing []string
	for _, id := range ids {
		if out[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out
	}
	found, err := graph.ContractSourceNodesContext(e.ctx, e.core, missing)
	if err != nil {
		e.fail(err)
		return nil
	}
	for _, node := range found {
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			e.fail(fmt.Errorf("contract followup: attempted to import core contract output"))
			return nil
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return nil
		}
		e.AddNode(copyNode)
		out[copyNode.ID] = copyNode
	}
	return out
}
func (e *contractFollowupEvidence) GetNode(id string) *graph.Node {
	return e.GetNodesByIDs([]string{id})[id]
}
func (e *contractFollowupEvidence) FindNodesByNames(names []string) map[string][]*graph.Node {
	count := 0
	err := graph.VisitNodesByNamesContext(e.ctx, e.core, names, func(node *graph.Node) bool {
		count++
		if count > graph.ContractProjectionRowLimit {
			e.fail(graph.ErrContractProjectionLimit)
			return false
		}
		if node == nil {
			return true
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			return true
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return false
		}
		if e.Graph.GetNode(copyNode.ID) == nil {
			e.AddNode(copyNode)
		}
		return true
	})
	if err != nil {
		e.fail(err)
	}
	if e.err != nil {
		return nil
	}
	return e.Graph.FindNodesByNames(names)
}
func (e *contractFollowupEvidence) FindNodesByName(name string) []*graph.Node {
	return e.FindNodesByNames([]string{name})[name]
}
func (e *contractFollowupEvidence) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	out, err := graph.ConstantValuesByNodeIDsContext(e.ctx, e.core, ids)
	if err != nil {
		e.fail(err)
	}
	return out, err
}
