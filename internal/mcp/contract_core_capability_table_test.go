package mcp

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// capOf reports whether r implements T.
func capOf[T any](r any) bool { _, ok := r.(T); return ok }

// contractCoreCapStatus is what a request sees of a store capability once
// the contract-core wrapper sits in front of it.
type contractCoreCapStatus string

const (
	// The wrapper implements it, filtering whatever adjacency it reads.
	capWrapper contractCoreCapStatus = "wrapper"
	// Node or content only: contractCoreSelectedReader resolves it on the
	// selected reader, verbatim.
	capSelected contractCoreCapStatus = "selected"
	// Served by fileEditingContextFor, recomputed over the filtered adjacency.
	capFiltered contractCoreCapStatus = "filtered"
	// Stored vectors: query.Engine.RefineByCosine reads the selected reader's
	// through the wrapper's SelectedVectorSearcher.
	capVectors contractCoreCapStatus = "vectors"
	// Refused; the row names why.
	capDropped contractCoreCapStatus = "dropped"
)

const (
	capReasonEdges     = "reads raw adjacency; serving it would bypass the contract edge filter"
	capReasonTraversal = "traversal engine; walks must expand through the filtered edge methods"
	capReasonBackend   = "asserted on the backend store (s.graph or the analysis store), never on a request reader"
	capReasonUnprobed  = "not referenced by the request-side packages contractCoreConsumerDirs scans: a write-side, indexer or resolver capability"
)

// contractCoreConsumerDirs are the packages request readers reach: internal/mcp
// itself and every package it hands s.readerFor(ctx) to.
var contractCoreConsumerDirs = []string{".", "../query", "../analysis", "../review", "../audit", "../analyzer"}

// contractCoreDirectProbes are direct probes of a selected-reader capability
// that need no accessor, keyed by path|capability, with the count pinned.
var contractCoreDirectProbes = map[string]struct {
	count  int
	reason string
}{
	"overlay_view.go|BoundedExactNameReader":         {1, "probes requestBaseReader, which readerFor wraps only afterwards"},
	"view_request.go|ContentSearcher":                {1, "newBaseGraphReader is handed s.graph alone"},
	"view_request.go|local:GetFileNodesByPaths":      {1, "baseGraphReader.base is s.graph alone"},
	"view_search.go|local:SearchContent":             {1, "bindSources is handed s.graph alone"},
	"../analysis/betweenness.go|NodesByKindsScanner": {1, "fallback after NodeIDsByKinds, which the wrapper serves"},
	"../analysis/deadcode.go|NodesByKindsScanner":    {1, "fallback after NodeIDsByKinds, which the wrapper serves"},
}

// contractCoreVectorProbes are the files allowed to probe graph.VectorSearcher:
// the engine refinement that also consults SelectedVectorSearcher, and the
// wrapper method itself.
var contractCoreVectorProbes = map[string]bool{"../query/engine.go": true, "contract_core_capabilities.go": true}

type contractCoreCapRow struct {
	name   string
	has    func(any) bool
	status contractCoreCapStatus
	reason string
}

type contractCoreCapName struct {
	name string
	has  func(any) bool
}

// Capabilities consumers probe through anonymous interfaces.
type (
	capMutationRevision interface{ MutationRevision() uint64 }
	capNodeContext      interface {
		GetNodeContext(context.Context, string) (*graph.Node, error)
	}
	capNodesByIDsContext interface {
		GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
	}
	capNameContext interface {
		FindNodesByNameContext(context.Context, string) ([]*graph.Node, error)
	}
	capNameContainingContext interface {
		FindNodesByNameContainingContext(context.Context, string, int) ([]*graph.Node, error)
	}
	capAllNodesLight interface{ AllNodesLight() []*graph.Node }
	capStatsContext  interface {
		StatsContext(context.Context) (graph.GraphStats, error)
	}
	capRepoMemoryEstimates interface {
		AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate
	}
	capFileNodesContext interface {
		GetFileNodesContext(context.Context, string) []*graph.Node
	}
	capNodesByIDs interface {
		GetNodesByIDs([]string) map[string]*graph.Node
	}
	capInEdgesByNodeIDsContext interface {
		GetInEdgesByNodeIDsContext(context.Context, []string, int) (map[string][]*graph.Edge, bool, error)
	}
	capBFSContext interface {
		BFSContext(context.Context, []string, graph.Direction, []graph.EdgeKind, int, int) ([]graph.BFSHop, error)
	}
	capRepoPrefixes interface{ RepoPrefixes() []string }
)

// contractCoreCapabilityTable lists every optional interface the SQLite store
// implements: each exported internal/graph interface plus the locally named or
// anonymous ones request code probes, named "local:" and their sorted methods
// joined by "+".
func contractCoreCapabilityTable() []contractCoreCapRow {
	rows := []contractCoreCapRow{
		// Kept by the wrapper itself.
		{"Reader", capOf[graph.Reader], capWrapper, "adjacency filtered, node reads promoted"},
		{"NodeGetter", capOf[graph.NodeGetter], capWrapper, "node reads promoted"},
		{"BoundaryReader", capOf[graph.BoundaryReader], capWrapper, "satisfied by the filtered Reader methods"},
		{"ZeroEdgeReader", capOf[graph.ZeroEdgeReader], capWrapper, "satisfied by the filtered Reader methods"},
		{"NodeLightScanner", capOf[graph.NodeLightScanner], capWrapper, "AllNodesLight forwarder"},
		{"NodeSearchKeyScanner", capOf[graph.NodeSearchKeyScanner], capWrapper, "ScanNodeSearchKeys forwarder"},
		{"NodeKindsByIDsReader", capOf[graph.NodeKindsByIDsReader], capWrapper, "GetNodeKindsByIDsContext forwarder"},
		{"NodeIDsByKinds", capOf[graph.NodeIDsByKinds], capWrapper, "ID-only kind scan forwarder; falls back to the selected reader's kind iterators"},
		{"BoundedFileNodeReader", capOf[graph.BoundedFileNodeReader], capWrapper, "conditional variant"},
		{"FilteredContainingNameReader", capOf[graph.FilteredContainingNameReader], capWrapper, "conditional variant"},
		{"ScopedNodeProjectionSequencer", capOf[graph.ScopedNodeProjectionSequencer], capWrapper, "conditional variant"},
		{"ScopedKindSummarySequencer", capOf[graph.ScopedKindSummarySequencer], capWrapper, "conditional variant"},
		{"ReadContextBinder", capOf[graph.ReadContextBinder], capWrapper, "binds the selected reader and wraps it again"},
		{"local:GetNodeContext", capOf[capNodeContext], capWrapper, "checked forwarder"},
		{"local:GetNodesByIDsContext", capOf[capNodesByIDsContext], capWrapper, "checked forwarder"},
		{"local:FindNodesByNameContext", capOf[capNameContext], capWrapper, "checked forwarder"},
		{"local:FindNodesByNameContainingContext", capOf[capNameContainingContext], capWrapper, "checked forwarder"},
		{"local:AllNodesLight", capOf[capAllNodesLight], capWrapper, "forwarder"},
		{"local:StatsContext", capOf[capStatsContext], capWrapper, "selected reader statistics"},
		{"local:GetFileNodesContext", capOf[capFileNodesContext], capWrapper, "deadline-aware file lookup forwarder (analysis diff joins); falls back to GetFileNodes as they do"},
		{"local:NodesByKind", capOf[nodesByKindReader], capWrapper, "Reader method"},
		{"local:GetNodesByIDs", capOf[capNodesByIDs], capWrapper, "Reader method"},

		// Node or content only: resolved on the selected reader.
		{"NodesByKindsScanner", capOf[graph.NodesByKindsScanner], capSelected, ""},
		{"NodesInFilesByKindFinder", capOf[graph.NodesInFilesByKindFinder], capSelected, ""},
		{"ContentSearcher", capOf[graph.ContentSearcher], capSelected, ""},
		{"FileSymbolNamesByPaths", capOf[graph.FileSymbolNamesByPaths], capSelected, ""},
		{"ChurnEnrichmentReader", capOf[graph.ChurnEnrichmentReader], capSelected, ""},
		{"CoverageEnrichmentReader", capOf[graph.CoverageEnrichmentReader], capSelected, ""},
		{"ReleaseEnrichmentReader", capOf[graph.ReleaseEnrichmentReader], capSelected, ""},
		{"BlameEnrichmentReader", capOf[graph.BlameEnrichmentReader], capSelected, ""},
		{"BoundedExactNameReader", capOf[graph.BoundedExactNameReader], capSelected, ""},
		{"local:FindNodesByNames", capOf[namesReader], capSelected, "search assist's exact-name rescue"},
		{"local:SearchContent", capOf[contentQuerier], capSelected, "composed-view content corpus"},
		{"local:GetFileNodesByPaths", capOf[fileNodesBatchReader], capSelected, "base-narrowed site prefetch"},

		// Recomputed over the filtered adjacency.
		{"FileEditingContext", capOf[graph.FileEditingContext], capFiltered, "the store reads raw imports and calls"},

		// Deliberate drops.
		{"BFSCapable", capOf[graph.BFSCapable], capDropped, capReasonTraversal},
		{"FrontierExpander", capOf[graph.FrontierExpander], capDropped, capReasonTraversal},
		{"ReachableForwardByKinds", capOf[graph.ReachableForwardByKinds], capDropped, capReasonTraversal},
		{"ClassHierarchyTraverser", capOf[graph.ClassHierarchyTraverser], capDropped, capReasonTraversal},
		{"local:BFSContext", capOf[capBFSContext], capDropped, capReasonTraversal},
		{"local:GetInEdgesByNodeIDsContext", capOf[capInEdgesByNodeIDsContext], capDropped, capReasonEdges + " (the bound counts hidden edges; callers fall back to the filtered GetInEdgesByNodeIDs and bound visible ones)"},
		{"local:GetOutEdgesByNodeIDsContext", capOf[memoContextEdgeReader], capDropped, capReasonEdges + " (the bound counts hidden edges; callers fall back to the filtered GetOutEdgesByNodeIDs)"},
		{"NodeDegreeAggregator", capOf[graph.NodeDegreeAggregator], capDropped, capReasonEdges + " (degrees count hidden endpoints; callers walk filtered edges)"},
		{"NodeDegreeByKinds", capOf[graph.NodeDegreeByKinds], capDropped, capReasonEdges + " (degrees count hidden endpoints)"},
		{"EdgeKindCounter", capOf[graph.EdgeKindCounter], capDropped, capReasonEdges + " (counts include hidden kinds, endpoints and spring.Bean calls)"},
		{"InEdgeCounter", capOf[graph.InEdgeCounter], capDropped, capReasonEdges},
		{"EdgesByKindsScanner", capOf[graph.EdgesByKindsScanner], capDropped, capReasonEdges},
		{"BoundedEdgeExistenceReader", capOf[graph.BoundedEdgeExistenceReader], capDropped, capReasonEdges},
		{"BoundedIncomingEdgeIdentityReader", capOf[graph.BoundedIncomingEdgeIdentityReader], capDropped, capReasonEdges},
		{"BoundedIncomingSourceReader", capOf[graph.BoundedIncomingSourceReader], capDropped, capReasonEdges},
		{"BoundedOutgoingEdgeIdentityReader", capOf[graph.BoundedOutgoingEdgeIdentityReader], capDropped, capReasonEdges},
		{"BoundedOutgoingSiteEdgeIdentityReader", capOf[graph.BoundedOutgoingSiteEdgeIdentityReader], capDropped, capReasonEdges},
		{"CallReferenceOutgoingReader", capOf[graph.CallReferenceOutgoingReader], capDropped, capReasonEdges},
		{"CrossRepoEdgeAggregator", capOf[graph.CrossRepoEdgeAggregator], capDropped, capReasonEdges},
		{"ExtractCandidatesScanner", capOf[graph.ExtractCandidatesScanner], capDropped, capReasonEdges},
		{"FileImportAggregator", capOf[graph.FileImportAggregator], capDropped, capReasonEdges},
		{"FileImporters", capOf[graph.FileImporters], capDropped, capReasonEdges},
		{"FileSubGraphReader", capOf[graph.FileSubGraphReader], capDropped, capReasonEdges},
		{"FileSubGraphCountReader", capOf[graph.FileSubGraphCountReader], capDropped, capReasonEdges},
		{"ThrowerErrorSurfacer", capOf[graph.ThrowerErrorSurfacer], capDropped, capReasonEdges},
		// Probed by internal/analysis on readers mcp builds from readerFor.
		{"DeadCodeCandidator", capOf[graph.DeadCodeCandidator], capDropped, capReasonEdges + " (zero-inbound candidates; FindDeadCode walks filtered edges)"},
		{"InDegreeForNodes", capOf[graph.InDegreeForNodes], capDropped, capReasonEdges + " (in-degrees count hidden endpoints)"},
		{"IfaceImplementsScanner", capOf[graph.IfaceImplementsScanner], capDropped, capReasonEdges + " (implements rows)"},
		{"NodeFanAggregator", capOf[graph.NodeFanAggregator], capDropped, capReasonEdges + " (fan counts include hidden endpoints)"},
		{"EdgeAdjacencyForKinds", capOf[graph.EdgeAdjacencyForKinds], capDropped, capReasonEdges + " (betweenness adjacency)"},
		{"CommunityCrossingsByKind", capOf[graph.CommunityCrossingsByKind], capDropped, capReasonEdges + " (crossing counts over raw calls and references)"},
		{"LightEdgeSequencer", capOf[graph.LightEdgeSequencer], capDropped, capReasonEdges + " (graph.EdgesLightSeq falls back to the filtered EdgesByKind)"},
		{"NodeLightSequencer", capOf[graph.NodeLightSequencer], capDropped, "node-only streaming; graph.NodesLightSeq falls back to the AllNodesLight forwarder, the same rows materialised"},
		{"Store", capOf[graph.Store], capDropped, "the whole backend contract; a request reader is never the store"},
		{"AnalysisGenerationStore", capOf[graph.AnalysisGenerationStore], capDropped, capReasonBackend},
		{"AnalysisQueryStore", capOf[graph.AnalysisQueryStore], capDropped, capReasonBackend},
		{"BundleFingerprintSink", capOf[graph.BundleFingerprintSink], capDropped, capReasonBackend},
		{"ContractRepoProjectionReader", capOf[graph.ContractRepoProjectionReader], capDropped, capReasonBackend},
		{"ContractStateStore", capOf[graph.ContractStateStore], capDropped, capReasonBackend},
		{"FileMetaReader", capOf[graph.FileMetaReader], capDropped, capReasonBackend},
		{"RepoIndexStateReader", capOf[graph.RepoIndexStateReader], capDropped, capReasonBackend},
		{"StructuralIntegrityAuditor", capOf[graph.StructuralIntegrityAuditor], capDropped, capReasonBackend},
		{"StructuralIntegritySnapshotter", capOf[graph.StructuralIntegritySnapshotter], capDropped, capReasonBackend},
		{"local:RepoPrefixes", capOf[capRepoPrefixes], capDropped, capReasonBackend},
		{"local:ViewGeneration", capOf[viewGenerationScoped], capDropped, "generation identity of a backend handle: read on s.graph, or on a stack root that must be the source handle itself"},
		{"local:SymbolExactHitsViewGenerations", capOf[storeExactTier], capDropped, "asserted only after a concrete *store_sqlite.Store check"},
		{"local:SymbolSearchCoreKey", capOf[storeSearchCoreKeyer], capDropped, "asserted only after a concrete *store_sqlite.Store check"},
		{"local:SymbolFTSViewGenerationRows", capOf[storeViewMatch], capDropped, "asserted only after a concrete *store_sqlite.Store check"},
		{"RepoMemoryEstimateScanner", capOf[graph.RepoMemoryEstimateScanner], capDropped, "audit provenance; reported for the selected reader by the private contractCoreRepoMemoryEstimates hook"},
		{"local:AllRepoMemoryEstimates", capOf[capRepoMemoryEstimates], capDropped, "per-repo counters reach tools through the private contractCoreRepoMemoryEstimates hook"},
		{"NodePresenceByIDsReader", capOf[graph.NodePresenceByIDsReader], capDropped, "physical presence; offered to the CSR alone by the private centralityNodePresence hook"},
		{"VectorSearcher", capOf[graph.VectorSearcher], capVectors, "node-keyed vectors; the interface also writes, so only the read reaches the engine"},
		{"local:MutationRevision", capOf[capMutationRevision], capDropped, "reported as SearchMutationRevision, which can say unknown; readerSearchMutationRevision reads either"},
	}
	for _, c := range contractCoreUnprobedCapabilities() {
		rows = append(rows, contractCoreCapRow{c.name, c.has, capDropped, capReasonUnprobed})
	}
	return rows
}

// contractCoreUnprobedCapabilities are the store interfaces no request code
// references; TestContractCoreCapabilityTableMatchesConsumers keeps it so.
func contractCoreUnprobedCapabilities() []contractCoreCapName {
	return []contractCoreCapName{
		{"AllGenerationsRepoEvicter", capOf[graph.AllGenerationsRepoEvicter]},
		{"AnalysisPageBinder", capOf[graph.AnalysisPageBinder]},
		{"AtomicVectorCorpusInstaller", capOf[graph.AtomicVectorCorpusInstaller]},
		{"BlameEnrichmentWriter", capOf[graph.BlameEnrichmentWriter]},
		{"BoundedIncomingSourceCandidateReader", capOf[graph.BoundedIncomingSourceCandidateReader]},
		{"BulkLoader", capOf[graph.BulkLoader]},
		{"CallableBindingNodeSequencer", capOf[graph.CallableBindingNodeSequencer]},
		{"CheckedAllGenerationsRepoEvicter", capOf[graph.CheckedAllGenerationsRepoEvicter]},
		{"ChurnEnrichmentWriter", capOf[graph.ChurnEnrichmentWriter]},
		{"CloneCorpusInitialization", capOf[graph.CloneCorpusInitialization]},
		{"CloneCorpusPager", capOf[graph.CloneCorpusPager]},
		{"CloneCorpusRepoReplacer", capOf[graph.CloneCorpusRepoReplacer]},
		{"CloneCorpusSignatureWriter", capOf[graph.CloneCorpusSignatureWriter]},
		{"CloneCorpusWriter", capOf[graph.CloneCorpusWriter]},
		{"CloneShingleReader", capOf[graph.CloneShingleReader]},
		{"CloneShingleWriter", capOf[graph.CloneShingleWriter]},
		{"ConfigNodeBatchEvicter", capOf[graph.ConfigNodeBatchEvicter]},
		{"ConstantValueContextReader", capOf[graph.ConstantValueContextReader]},
		{"ConstantValueProjectionReader", capOf[graph.ConstantValueProjectionReader]},
		{"ConstantValueReader", capOf[graph.ConstantValueReader]},
		{"ConstantValueRepoReplacer", capOf[graph.ConstantValueRepoReplacer]},
		{"ConstantValueWriter", capOf[graph.ConstantValueWriter]},
		{"ContentFTSBatchReplacer", capOf[graph.ContentFTSBatchReplacer]},
		{"ContentNodeReader", capOf[graph.ContentNodeReader]},
		{"ContractAttachmentReader", capOf[graph.ContractAttachmentReader]},
		{"ContractAttachmentWorkReader", capOf[graph.ContractAttachmentWorkReader]},
		{"ContractBoundaryReceiptReader", capOf[graph.ContractBoundaryReceiptReader]},
		{"ContractFileProjectionReader", capOf[graph.ContractFileProjectionReader]},
		{"ContractInputStateCohortReader", capOf[graph.ContractInputStateCohortReader]},
		{"ContractInputStateReader", capOf[graph.ContractInputStateReader]},
		{"ContractNodeBatchEvicter", capOf[graph.ContractNodeBatchEvicter]},
		{"ContractOwnerReplacer", capOf[graph.ContractOwnerReplacer]},
		{"ContractOwnerScalarLiveness", capOf[graph.ContractOwnerScalarLiveness]},
		{"ContractWorkReader", capOf[graph.ContractWorkReader]},
		{"CoordinatedBulkLoader", capOf[graph.CoordinatedBulkLoader]},
		{"CoverageEnrichmentWriter", capOf[graph.CoverageEnrichmentWriter]},
		{"CrossRepoCandidates", capOf[graph.CrossRepoCandidates]},
		{"CrossRepoFlagMarker", capOf[graph.CrossRepoFlagMarker]},
		{"CurrentGenerationRepoEvicter", capOf[graph.CurrentGenerationRepoEvicter]},
		{"DBStatReporter", capOf[graph.DBStatReporter]},
		{"DerivedContractReplacer", capOf[graph.DerivedContractReplacer]},
		{"EdgeEndpointReader", capOf[graph.EdgeEndpointReader]},
		{"EdgeIdentityBatchFinder", capOf[graph.EdgeIdentityBatchFinder]},
		{"EdgeKindEvicter", capOf[graph.EdgeKindEvicter]},
		{"EdgeMetaBatchPersister", capOf[graph.EdgeMetaBatchPersister]},
		{"EdgePersister", capOf[graph.EdgePersister]},
		{"EdgesByKindsBatchScanner", capOf[graph.EdgesByKindsBatchScanner]},
		{"EdgeTerminalStampPersister", capOf[graph.EdgeTerminalStampPersister]},
		{"EnrichmentStateStore", capOf[graph.EnrichmentStateStore]},
		{"ExactEdgeBatchRemover", capOf[graph.ExactEdgeBatchRemover]},
		{"ExistingNodeIDFinder", capOf[graph.ExistingNodeIDFinder]},
		{"ExternalCallCandidates", capOf[graph.ExternalCallCandidates]},
		{"FileBatchEvicter", capOf[graph.FileBatchEvicter]},
		{"FileIndexFailureReader", capOf[graph.FileIndexFailureReader]},
		{"FileIndexFailureWriter", capOf[graph.FileIndexFailureWriter]},
		{"FileLanguageNodeSequencer", capOf[graph.FileLanguageNodeSequencer]},
		{"FileMetaPathReader", capOf[graph.FileMetaPathReader]},
		{"FileMetaRepoReplacer", capOf[graph.FileMetaRepoReplacer]},
		{"FileMetaWriter", capOf[graph.FileMetaWriter]},
		{"FileMtimeDeleter", capOf[graph.FileMtimeDeleter]},
		{"FileMtimeReader", capOf[graph.FileMtimeReader]},
		{"FileMtimeReplacer", capOf[graph.FileMtimeReplacer]},
		{"FileMtimeWriter", capOf[graph.FileMtimeWriter]},
		{"FileNodeIdentitySequencer", capOf[graph.FileNodeIdentitySequencer]},
		{"FileReceiptPager", capOf[graph.FileReceiptPager]},
		{"FnValuePlaceholderScanner", capOf[graph.FnValuePlaceholderScanner]},
		{"FrameworkCensusEdgeSequencer", capOf[graph.FrameworkCensusEdgeSequencer]},
		{"GoMethodReceiverBatchRebinder", capOf[graph.GoMethodReceiverBatchRebinder]},
		{"GoMethodReceiverRebinder", capOf[graph.GoMethodReceiverRebinder]},
		{"ImportAdjacencyProjector", capOf[graph.ImportAdjacencyProjector]},
		{"IncomingSourceNodeChecker", capOf[graph.IncomingSourceNodeChecker]},
		{"InEdgeIdentityBatchReader", capOf[graph.InEdgeIdentityBatchReader]},
		{"LightEdgeScanner", capOf[graph.LightEdgeScanner]},
		{"LightNodeReader", capOf[graph.LightNodeReader]},
		{"MemberMethodsByType", capOf[graph.MemberMethodsByType]},
		{"MutationFanoutRecorder", capOf[graph.MutationFanoutRecorder]},
		{"MutationReceiptStore", capOf[graph.MutationReceiptStore]},
		{"MutationScopedCrossRepoCandidates", capOf[graph.MutationScopedCrossRepoCandidates]},
		{"NamedLanguageNodeSequencer", capOf[graph.NamedLanguageNodeSequencer]},
		{"NodeIDNamesByKindsSequencer", capOf[graph.NodeIDNamesByKindsSequencer]},
		{"NodeKindMembershipReader", capOf[graph.NodeKindMembershipReader]},
		{"NodeNameClassCounter", capOf[graph.NodeNameClassCounter]},
		{"NodePlacementBatchReader", capOf[graph.NodePlacementBatchReader]},
		{"NodesByKindsSequencer", capOf[graph.NodesByKindsSequencer]},
		{"NodesInFilesByKindStreamer", capOf[graph.NodesInFilesByKindStreamer]},
		{"NonContentNodeReader", capOf[graph.NonContentNodeReader]},
		{"OutgoingMetadataReader", capOf[graph.OutgoingMetadataReader]},
		{"OverlayLayerContractProjectionReader", capOf[graph.OverlayLayerContractProjectionReader]},
		{"OverlayLayerContractRepoProjectionReader", capOf[graph.OverlayLayerContractRepoProjectionReader]},
		{"OverrideDispatchCallBatchScanner", capOf[graph.OverrideDispatchCallBatchScanner]},
		{"PathlessNodeBatchEvicter", capOf[graph.PathlessNodeBatchEvicter]},
		{"PendingContractWorkReader", capOf[graph.PendingContractWorkReader]},
		{"PlannerStatsFreshener", capOf[graph.PlannerStatsFreshener]},
		{"PrefixDiagnosticsReader", capOf[graph.PrefixDiagnosticsReader]},
		{"QualifiedNodeIdentitySequencer", capOf[graph.QualifiedNodeIdentitySequencer]},
		{"ReceiverMutationScanner", capOf[graph.ReceiverMutationScanner]},
		{"RecordedEdgeReader", capOf[graph.RecordedEdgeReader]},
		{"RefFactsReader", capOf[graph.RefFactsReader]},
		{"RefFactsRebuilder", capOf[graph.RefFactsRebuilder]},
		{"RefFactsWriter", capOf[graph.RefFactsWriter]},
		{"ReleaseEnrichmentWriter", capOf[graph.ReleaseEnrichmentWriter]},
		{"RepoCapabilityEdgeScanner", capOf[graph.RepoCapabilityEdgeScanner]},
		{"RepoEdgeKindReader", capOf[graph.RepoEdgeKindReader]},
		{"RepoFilePathReader", capOf[graph.RepoFilePathReader]},
		{"RepoIndexStateWriter", capOf[graph.RepoIndexStateWriter]},
		{"RepoLanguageCountReader", capOf[graph.RepoLanguageCountReader]},
		{"RepoLanguageFileCountReader", capOf[graph.RepoLanguageFileCountReader]},
		{"RepoLanguageNameFinder", capOf[graph.RepoLanguageNameFinder]},
		{"RepoLanguageNodeSummaryReader", capOf[graph.RepoLanguageNodeSummaryReader]},
		{"RepoLanguageSymbolCounter", capOf[graph.RepoLanguageSymbolCounter]},
		{"RepoLightNodeReader", capOf[graph.RepoLightNodeReader]},
		{"RepoMetaNodeReader", capOf[graph.RepoMetaNodeReader]},
		{"RepoNamesNodeFinder", capOf[graph.RepoNamesNodeFinder]},
		{"RepoNodeIdentitySequencer", capOf[graph.RepoNodeIdentitySequencer]},
		{"RepoNodeKindIDReader", capOf[graph.RepoNodeKindIDReader]},
		{"ResolverNameScopeFinder", capOf[graph.ResolverNameScopeFinder]},
		{"ScopeBindingNodeSequencer", capOf[graph.ScopeBindingNodeSequencer]},
		{"ScopedCrossRepoCandidates", capOf[graph.ScopedCrossRepoCandidates]},
		{"ScopedEdgeKindEvicter", capOf[graph.ScopedEdgeKindEvicter]},
		{"ScopedIncomingSourceReader", capOf[graph.ScopedIncomingSourceReader]},
		{"ScopedProjectionSequencer", capOf[graph.ScopedProjectionSequencer]},
		{"ScopedSymbolBundleSearcher", capOf[graph.ScopedSymbolBundleSearcher]},
		{"SemanticBindingTypeReader", capOf[graph.SemanticBindingTypeReader]},
		{"SemanticBindingTypeStore", capOf[graph.SemanticBindingTypeStore]},
		{"SemanticBindingTypeWriter", capOf[graph.SemanticBindingTypeWriter]},
		{"SemanticNodeStampWriter", capOf[graph.SemanticNodeStampWriter]},
		{"StructuralIntegrityEventRecorder", capOf[graph.StructuralIntegrityEventRecorder]},
		{"StructuralParentEdges", capOf[graph.StructuralParentEdges]},
		{"SymbolBundleSearcher", capOf[graph.SymbolBundleSearcher]},
		{"SymbolFTSBatchDeleter", capOf[graph.SymbolFTSBatchDeleter]},
		{"SymbolFTSBatchUpserter", capOf[graph.SymbolFTSBatchUpserter]},
		{"SymbolFTSCounter", capOf[graph.SymbolFTSCounter]},
		{"SymbolFTSNormalizationState", capOf[graph.SymbolFTSNormalizationState]},
		{"SymbolFTSRepoReplacer", capOf[graph.SymbolFTSRepoReplacer]},
		{"SymbolFTSRepoResetter", capOf[graph.SymbolFTSRepoResetter]},
		{"SymbolSearcher", capOf[graph.SymbolSearcher]},
		{"TestCallProjectionScanner", capOf[graph.TestCallProjectionScanner]},
		{"TestProjectionScanner", capOf[graph.TestProjectionScanner]},
		{"UnresolvedEdgeIdentityBatchScanner", capOf[graph.UnresolvedEdgeIdentityBatchScanner]},
		{"UnresolvedEdgePager", capOf[graph.UnresolvedEdgePager]},
		{"UnresolvedEdgeTargetBatchReindexer", capOf[graph.UnresolvedEdgeTargetBatchReindexer]},
		{"UnresolvedFrontierCounter", capOf[graph.UnresolvedFrontierCounter]},
		{"UnresolvedInsertionCounter", capOf[graph.UnresolvedInsertionCounter]},
		{"ValueRefPlaceholderScanner", capOf[graph.ValueRefPlaceholderScanner]},
		{"WorkspaceSlugBackfiller", capOf[graph.WorkspaceSlugBackfiller]},
		{"WorkspaceSlugImpactBackfiller", capOf[graph.WorkspaceSlugImpactBackfiller]},
	}
}

// contractCoreNonStoreInterfaces are the internal/graph interfaces the SQLite
// store does not implement, so no request can lose them to the wrapper.
func contractCoreNonStoreInterfaces() []contractCoreCapName {
	return []contractCoreCapName{
		{"BackendResolver", capOf[graph.BackendResolver]},
		{"BelowFileRows", capOf[graph.BelowFileRows]},
		{"CommunityDetector", capOf[graph.CommunityDetector]},
		{"ComponentFinder", capOf[graph.ComponentFinder]},
		{"ConstantValueReadStatus", capOf[graph.ConstantValueReadStatus]},
		{"ContractOwnerScalarInvalidator", capOf[graph.ContractOwnerScalarInvalidator]},
		{"EdgeEndpointProvider", capOf[graph.EdgeEndpointProvider]},
		{"FileIndexFailureLayerReader", capOf[graph.FileIndexFailureLayerReader]},
		{"FileNodeReadCounter", capOf[graph.FileNodeReadCounter]},
		{"GenerationLayerIdentity", capOf[graph.GenerationLayerIdentity]},
		{"IncomingSourceNodeQuery", capOf[graph.IncomingSourceNodeQuery]},
		{"KCorer", capOf[graph.KCorer]},
		{"OverlayDetachedFileSummaryReader", capOf[graph.OverlayDetachedFileSummaryReader]},
		{"OverlayDetachedNodeReader", capOf[graph.OverlayDetachedNodeReader]},
		{"OverlayDetachedSummaryReader", capOf[graph.OverlayDetachedSummaryReader]},
		{"OverlayLayerConstantValueReader", capOf[graph.OverlayLayerConstantValueReader]},
		{"OverlayLayerEdgeEndpointReader", capOf[graph.OverlayLayerEdgeEndpointReader]},
		{"OverlayLayerNodeKindsByIDsReader", capOf[graph.OverlayLayerNodeKindsByIDsReader]},
		{"OverlayLayerOutgoingMetadataReader", capOf[graph.OverlayLayerOutgoingMetadataReader]},
		{"OverlayLayerProjectionReader", capOf[graph.OverlayLayerProjectionReader]},
		{"OverlayLayerReader", capOf[graph.OverlayLayerReader]},
		{"OverlayLayerRecordedEdgeReader", capOf[graph.OverlayLayerRecordedEdgeReader]},
		{"OverlayLayerScopedNameReader", capOf[graph.OverlayLayerScopedNameReader]},
		{"OverlayLocalizationIdentityReader", capOf[graph.OverlayLocalizationIdentityReader]},
		{"PageRanker", capOf[graph.PageRanker]},
		{"RecordedEdgeProvider", capOf[graph.RecordedEdgeProvider]},
		{"RefFactsChainSplitter", capOf[graph.RefFactsChainSplitter]},
		{"Unwrapper", capOf[graph.Unwrapper]},
	}
}

func openContractCoreCapStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "caps.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// The daemon wraps every request reader of the SQLite store. Each optional
// capability the store implements keeps exactly the status its row names.
func TestContractCoreCapabilityTable(t *testing.T) {
	store := openContractCoreCapStore(t)
	wrapped := newContractCoreEdges(store, t.Context(), nil)
	selected := contractCoreSelectedReader(wrapped)
	require.Equal(t, graph.Reader(store), selected, "the selected reader is the wrapped store")
	seen := make(map[string]bool)
	for _, row := range contractCoreCapabilityTable() {
		require.Falsef(t, seen[row.name], "duplicate row %s", row.name)
		seen[row.name] = true
		t.Run(row.name, func(t *testing.T) {
			require.Truef(t, row.has(store), "the SQLite store no longer implements %s; remove or reclassify its row", row.name)
			onWrapper := row.has(wrapped)
			switch row.status {
			case capWrapper:
				require.True(t, onWrapper, "the wrapper stopped advertising a capability it honours")
			case capSelected:
				require.False(t, onWrapper, "the wrapper advertises a selected-reader capability; reclassify the row")
				require.True(t, row.has(selected))
			case capFiltered:
				require.False(t, onWrapper, "the wrapper advertises the raw capability")
				require.Equal(t, "FileEditingContext", row.name)
				fc, ok := fileEditingContextFor(wrapped)
				require.True(t, ok)
				require.IsType(t, contractCoreEditingContext{}, fc)
			case capVectors:
				require.False(t, onWrapper, "the wrapper advertises the vector writes")
				require.Equal(t, "VectorSearcher", row.name)
				source, ok := wrapped.(interface{ SelectedVectorSearcher() graph.VectorSearcher })
				require.True(t, ok)
				require.Equal(t, graph.VectorSearcher(store), source.SelectedVectorSearcher())
			case capDropped:
				require.NotEmpty(t, row.reason, "a deliberate drop names its reason")
				require.Falsef(t, onWrapper, "the wrapper now advertises %s (%s); reclassify the row", row.name, row.reason)
			default:
				t.Fatalf("unknown status %q", row.status)
			}
		})
	}
}

// Every exported, non-generic interface declared in internal/graph is either
// a row of the table or listed as one the store does not implement, so a new
// optional capability cannot reach the wrapper unclassified.
func TestContractCoreCapabilityTableCoversGraphInterfaces(t *testing.T) {
	store := openContractCoreCapStore(t)
	files, err := filepath.Glob(filepath.Join("..", "graph", "*.go"))
	require.NoError(t, err)
	declared := make(map[string]bool)
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || !spec.Name.IsExported() || spec.TypeParams != nil {
				return true
			}
			if _, isInterface := spec.Type.(*ast.InterfaceType); isInterface {
				declared[spec.Name.Name] = true
			}
			return true
		})
	}
	require.NotEmpty(t, declared)
	classified := make(map[string]bool)
	for _, row := range contractCoreCapabilityTable() {
		if !strings.HasPrefix(row.name, "local:") {
			classified[row.name] = true
		}
	}
	for _, c := range contractCoreNonStoreInterfaces() {
		require.Falsef(t, c.has(store), "the SQLite store now implements %s; move it into the capability table", c.name)
		require.Falsef(t, classified[c.name], "%s is listed twice", c.name)
		classified[c.name] = true
	}
	var missing, stale []string
	for name := range declared {
		if !classified[name] {
			missing = append(missing, name)
		}
	}
	for name := range classified {
		if !declared[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	require.Emptyf(t, missing, "classify these internal/graph interfaces in the contract-core capability table")
	require.Emptyf(t, stale, "these rows name no internal/graph interface")
}

// Request code (internal/mcp and every package it hands a request reader to)
// probes selected-reader capabilities only through contractCoreSelectedReader
// or at a pinned direct probe, probes nothing else through it, never bypasses
// the filtered editing context or the vector source, and references no
// capability the table calls unprobed.
func TestContractCoreCapabilityTableMatchesConsumers(t *testing.T) {
	status := make(map[string]contractCoreCapStatus)
	unprobed := make(map[string]bool)
	for _, row := range contractCoreCapabilityTable() {
		status[row.name] = row.status
		if row.reason == capReasonUnprobed {
			unprobed[row.name] = true
		}
	}
	reference := regexp.MustCompile(`\bgraph\.([A-Z][A-Za-z0-9]*)\b`)
	direct := make(map[string]int)
	probedLocal := make(map[string]bool)
	for _, probe := range contractCoreConsumerAssertions(t, contractCoreConsumerDirs) {
		where, name, slashPath := probe.where, probe.key, probe.path
		if strings.HasPrefix(name, "local:") {
			_, classified := status[name]
			require.Truef(t, classified, "%s probes an interface the SQLite store satisfies (%s); add a capability-table row", where, name)
			probedLocal[name] = true
		}
		switch status[name] {
		case capSelected:
			if key := slashPath + "|" + name; !probe.viaSelected {
				_, pinned := contractCoreDirectProbes[key]
				require.Truef(t, pinned, "%s probes %s directly; a contract-core wrapper would drop it, use contractCoreSelectedReader", where, name)
				direct[key]++
			}
		case capFiltered:
			require.Equalf(t, "contract_core_editing_context.go", filepath.Base(slashPath), "%s probes %s directly; use fileEditingContextFor", where, name)
		case capVectors:
			require.Truef(t, contractCoreVectorProbes[slashPath], "%s probes %s; the wrapper serves it only to query.Engine.RefineByCosine through SelectedVectorSearcher", where, name)
		default:
			require.Falsef(t, probe.viaSelected, "%s resolves %s through contractCoreSelectedReader, which serves node- and content-only capabilities alone", where, name)
		}
	}
	// The graph helpers probe the wrapper's checked forwarders on callers'
	// behalf; they are generic selectors, so they only keep a row alive.
	for _, probe := range contractCoreConsumerAssertions(t, []string{"../graph"}) {
		probedLocal[probe.key] = true
	}
	for _, row := range contractCoreCapabilityTable() {
		if strings.HasPrefix(row.name, "local:") {
			require.Truef(t, probedLocal[row.name], "neither request code nor a graph helper probes %s; remove its row", row.name)
		}
	}
	for _, path := range contractCoreConsumerSources(t, contractCoreConsumerDirs) {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range reference.FindAllStringSubmatch(line, -1) {
				require.Falsef(t, unprobed[m[1]], "%s:%d references %s, which the capability table calls unprobed; classify it", filepath.ToSlash(path), i+1, m[1])
			}
		}
	}
	for key, probe := range contractCoreDirectProbes {
		require.NotEmptyf(t, probe.reason, "direct probe %s names why it needs no accessor", key)
		require.Equalf(t, probe.count, direct[key], "direct probes of %s drifted (%s); re-check whether the reader can be a contract-core wrapper", key, probe.reason)
	}
}

// contractCoreConsumerSources lists the non-test sources of dirs.
func contractCoreConsumerSources(t *testing.T, dirs []string) []string {
	t.Helper()
	var sources []string
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(filepath.FromSlash(dir), "*.go"))
		require.NoError(t, err)
		require.NotEmptyf(t, files, "consumer package %s has no sources", dir)
		for _, path := range files {
			if !strings.HasSuffix(path, "_test.go") {
				sources = append(sources, path)
			}
		}
	}
	return sources
}

// contractCoreAssertion is one type assertion or type-switch case in request
// code, keyed as its capability-table row is named: the graph interface name,
// or "local:" plus the sorted method names of a locally named or anonymous
// interface whose every method the SQLite store has.
type contractCoreAssertion struct {
	path, where, key string
	viaSelected      bool
}

// contractCoreIfaces resolves interface method names: local holds one consumer
// package's interface declarations (any scope), graph those of internal/graph.
type contractCoreIfaces struct {
	local map[string][]*ast.InterfaceType
	graph map[string][]*ast.InterfaceType
}

func collectContractCoreIfaces(t *testing.T, files []*ast.File) map[string][]*ast.InterfaceType {
	t.Helper()
	ifaces := make(map[string][]*ast.InterfaceType)
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok {
				if iface, isInterface := spec.Type.(*ast.InterfaceType); isInterface {
					ifaces[spec.Name.Name] = append(ifaces[spec.Name.Name], iface)
				}
			}
			return true
		})
	}
	return ifaces
}

func parseContractCorePackage(t *testing.T, fset *token.FileSet, paths []string) []*ast.File {
	t.Helper()
	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, file)
	}
	return files
}

// methods adds iface's method names, embedded interfaces included, to into.
// It reports false when an embedded interface cannot be resolved.
func (s contractCoreIfaces) methods(iface *ast.InterfaceType, inGraph bool, into map[string]bool) bool {
	for _, field := range iface.Methods.List {
		if _, isFunc := field.Type.(*ast.FuncType); isFunc {
			for _, name := range field.Names {
				into[name.Name] = true
			}
			continue
		}
		var decls []*ast.InterfaceType
		embeddedInGraph := inGraph
		switch typ := field.Type.(type) {
		case *ast.Ident:
			if typ.Name == "error" {
				into["Error"] = true
				continue
			}
			if inGraph {
				decls = s.graph[typ.Name]
			} else {
				decls = s.local[typ.Name]
			}
		case *ast.SelectorExpr:
			if pkg, ok := typ.X.(*ast.Ident); ok && pkg.Name == "graph" {
				decls, embeddedInGraph = s.graph[typ.Sel.Name], true
			}
		}
		if len(decls) != 1 || !s.methods(decls[0], embeddedInGraph, into) {
			return false
		}
	}
	return true
}

// contractCoreConsumerAssertions finds every interface probe in request code:
// graph interfaces by name, and locally named or anonymous interfaces the
// SQLite store satisfies by method name.
func contractCoreConsumerAssertions(t *testing.T, dirs []string) []contractCoreAssertion {
	t.Helper()
	fset := token.NewFileSet()
	graphPaths, err := filepath.Glob(filepath.Join("..", "graph", "*.go"))
	require.NoError(t, err)
	var graphSources []string
	for _, path := range graphPaths {
		if !strings.HasSuffix(path, "_test.go") {
			graphSources = append(graphSources, path)
		}
	}
	graphIfaces := collectContractCoreIfaces(t, parseContractCorePackage(t, fset, graphSources))
	store := reflect.TypeOf((*store_sqlite.Store)(nil))
	byDir := make(map[string][]string)
	for _, path := range contractCoreConsumerSources(t, dirs) {
		byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], path)
	}
	var out []contractCoreAssertion
	for _, paths := range byDir {
		files := parseContractCorePackage(t, fset, paths)
		scope := contractCoreIfaces{local: collectContractCoreIfaces(t, files), graph: graphIfaces}
		lines := make(map[string][]string)
		for _, path := range paths {
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			lines[path] = strings.Split(string(body), "\n")
		}
		for i, file := range files {
			slashPath := filepath.ToSlash(paths[i])
			record := func(x, typ ast.Expr) {
				var key string
				var iface *ast.InterfaceType
				switch typ := typ.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := typ.X.(*ast.Ident); ok && pkg.Name == "graph" && len(graphIfaces[typ.Sel.Name]) > 0 {
						key = typ.Sel.Name
					}
				case *ast.Ident:
					if decls := scope.local[typ.Name]; len(decls) > 0 {
						require.Lenf(t, decls, 1, "%s: interface name %s is declared more than once; the guard cannot tell which one is probed", slashPath, typ.Name)
						iface = decls[0]
					}
				case *ast.InterfaceType:
					iface = typ
				}
				line := fset.Position(x.Pos()).Line
				where := slashPath + ":" + strconv.Itoa(line)
				if iface != nil {
					names := make(map[string]bool)
					require.Truef(t, scope.methods(iface, false, names), "%s: cannot resolve the probed interface's embedded methods", where)
					sorted := make([]string, 0, len(names))
					for name := range names {
						if _, ok := store.MethodByName(name); !ok {
							return
						}
						sorted = append(sorted, name)
					}
					if len(sorted) == 0 {
						return
					}
					sort.Strings(sorted)
					key = "local:" + strings.Join(sorted, "+")
				}
				if key == "" {
					return
				}
				out = append(out, contractCoreAssertion{
					path:        slashPath,
					where:       where,
					key:         key,
					viaSelected: strings.Contains(lines[paths[i]][line-1], "contractCoreSelectedReader("),
				})
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.TypeAssertExpr:
					if node.Type != nil {
						record(node.X, node.Type)
					}
				case *ast.TypeSwitchStmt:
					var guard ast.Expr
					switch assign := node.Assign.(type) {
					case *ast.AssignStmt:
						guard = assign.Rhs[0]
					case *ast.ExprStmt:
						guard = assign.X
					}
					x := guard.(*ast.TypeAssertExpr).X
					for _, clause := range node.Body.List {
						for _, typ := range clause.(*ast.CaseClause).List {
							record(x, typ)
						}
					}
				}
				return true
			})
		}
	}
	require.NotEmpty(t, out)
	return out
}

// contractCoreSelectedReader keeps every request shape's own answer, so a
// request reads with the runtime exactly the node and content capabilities it
// reads without it, and BindReadContext keeps the variant it binds.
func TestContractCoreSelectedCapabilitiesFollowEachReaderShape(t *testing.T) {
	store := openContractCoreCapStore(t)
	store.AddBatch([]*graph.Node{{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"}}, nil)
	shapes := map[string]graph.Reader{
		"store":         store,
		"bound_store":   store.BindReadContext(t.Context()),
		"memory_graph":  graph.New(),
		"overlaid_view": graph.NewOverlaidView(store, graph.NewOverlayLayer()),
		"base_narrowed": newBaseGraphReader(store, "repo"),
	}
	rows := contractCoreCapabilityTable()
	for name, inner := range shapes {
		t.Run(name, func(t *testing.T) {
			wrapped := newContractCoreEdges(inner, t.Context(), nil)
			bound := graph.BindReadContext(wrapped, t.Context())
			for _, row := range rows {
				switch row.status {
				case capSelected:
					require.Equalf(t, row.has(inner), row.has(contractCoreSelectedReader(wrapped)), "%s", row.name)
					require.Equalf(t, row.has(inner), row.has(contractCoreSelectedReader(bound)), "%s after binding", row.name)
				case capFiltered:
					_, ok := fileEditingContextFor(wrapped)
					require.Equalf(t, row.has(inner), ok, "%s", row.name)
				case capVectors:
					for _, r := range []graph.Reader{wrapped, bound} {
						source := r.(interface{ SelectedVectorSearcher() graph.VectorSearcher })
						require.Equalf(t, row.has(inner), source.SelectedVectorSearcher() != nil, "%s", row.name)
					}
				case capWrapper:
					require.Equalf(t, row.has(wrapped), row.has(bound), "%s after binding", row.name)
				}
			}
		})
	}
}
