package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The DeltaWriter capability checklist.
//
// The per-save engine discovers what its store can do by type assertion: a
// capability the store lacks silently takes another code path (a whole-graph
// scan, a skipped side table, a legacy fallback). For a DeltaWriter that is
// the silent capability gap the design names as its second risk. This test
// reads every interface type assertion in the engine's packages (indexer,
// resolver, graph) back out of their sources and requires each one to be
// either implemented by *graph.DeltaWriter (verified by method set) or listed
// below with the reason the delta does without it. A capability added to the
// engine without a decision fails here; so does a listed absence that has
// since been implemented or is no longer asserted.

// editDeltaAbsentCapabilities are the asserted interfaces *graph.DeltaWriter
// deliberately does not implement, with the reason. The reason names what
// the engine does instead.
var editDeltaAbsentCapabilities = map[string]string{
	"graph.AnalysisPageBinder":                    "whole-graph analysis opts into bounded snapshots under its revision publication fence; file-delta resolver reads retain their composed snapshot semantics",
	"graph.NodeKindMembershipReader":              "optional positive-kind accelerator; checked helper uses the exact composed GetNodeKindsByIDsContext rather than forwarding a physical base membership set",
	"graph.OutgoingMetadataReader":                "checked full-metadata composition dispatches DeltaWriter explicitly and reads its selected view; the physical getter is never forwarded to a base-only store",
	"graph.OverlayLayerOutgoingMetadataReader":    "checked physical ancestry-layer adjacency, not a composed DeltaWriter; graph.GetOutEdgesByNodeIDsWithMetadataContext composes delta ownership explicitly",
	"anonymous{Counts}":                           "asserted by the chain fold on the store's stepped fold (its copy counts), never on a store or a delta",
	"anonymous{InterruptionCounts}":               "asserted by the chain fold on its stepped fold operation (interrupted transactions and page-budget reductions), never on a store or a delta",
	"graph.GenerationLayerIdentity":               "a capability of the published chain layers a delta composes over (their generation id keys the kept rows), asserted on those layers, never on the delta",
	"graph.OverlayDetachedSummaryReader":          "a capability of the immutable overlay layers a delta composes over, asserted on the layers below, never on the delta",
	"graph.RefFactsChainSplitter":                 "asserted by a delta on the view below it (its base) to split the chain's facts from the kept stack, never on the delta itself",
	"anonymous{EdgesByKindKindFirst}":             "asserted by the delta on the store at the bottom of its own stack (the kind-first scan of a whole corpus), never on the delta",
	"graph.OverlayLayerProjectionReader":          "a capability of an immutable overlay layer the delta composes over, asserted on the layers below, never on the delta",
	"graph.OverlayLayerConstantValueReader":       "checked physical-layer constant projection; DeltaWriter is the composed reader that resolves accepted file ownership, not a physical ancestry layer",
	"graph.OverlayLayerContractProjectionReader":  "checked physical-layer contract projection; DeltaWriter is the composed reader that applies visibility and sibling groups, not a physical ancestry layer",
	"graph.OverlayLayerScopedNameReader":          "a capability of the layers a delta composes over, not of a store: the engine asks it of each layer below the delta, and a DeltaWriter is never a layer below another delta",
	"graph.ReadContextBinder":                     "request-handler cancellation of long whole-store reads; the per-save engine never binds its store, and absent the binder returns the reader unchanged",
	"anonymous{Catalog}":                          "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"anonymous{CheckpointWAL}":                    "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"anonymous{Concurrent}":                       "embedder capability probe, asserted on the embedding provider, not on the store",
	"anonymous{ConcurrentCheckoutPreparation}":    "compiler-admission and off-lane preparation eligibility, asserted on semantic providers, never on a store or delta; providers without it keep preparation on the serialized lane",
	"anonymous{DeleteContentFilesForRepoNotIn}":   "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"anonymous{HasLanguage}":                      "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"anonymous{MutationRevision}":                 "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"anonymous{NodesByKindLang}":                  "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"anonymous{PurgeRepo}":                        "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"anonymous{RepoHasLanguageContext}":           "read accelerator: without it the caller takes the composed file-count fallback; forwarding the base EXISTS would miss delta additions and shadows",
	"anonymous{SearchMutationRevision}":           "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"anonymous{TokensUsed}":                       "embedder usage probe, asserted on the embedding provider",
	"anonymous{Unwrap}":                           "error or wrapper unwrapping, asserted on errors and cleanup wrappers",
	"anonymous{ViewGeneration}":                   "generation-handle identity probe of the sparse builder's derived-target check; the delta writes its generation through the handle directly",
	"anonymous{VisitNodesByNameContainingFolded}": "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.AllGenerationsRepoEvicter":             "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.AtomicVectorCorpusInstaller":           "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.BackendResolver":                       "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.BlameEnrichmentWriter":                 "node-keyed enrichment side tables (blame, churn, coverage, releases): their rows for an evicted node are the layer below's, hidden with the node; a delta writes none",
	"graph.BoundedEdgeExistenceReader":            "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedExactNameReader":                "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedFileNodeReader":                 "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedIncomingEdgeIdentityReader":     "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedIncomingSourceCandidateReader":  "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedIncomingSourceReader":           "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedOutgoingEdgeIdentityReader":     "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BoundedOutgoingSiteEdgeIdentityReader": "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.BulkLoader":                            "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.CallableBindingNodeSequencer":          "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.CheckedAllGenerationsRepoEvicter":      "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.ChurnEnrichmentWriter":                 "node-keyed enrichment side tables (blame, churn, coverage, releases): their rows for an evicted node are the layer below's, hidden with the node; a delta writes none",
	"graph.CloneCorpusInitialization":             "near-duplicate corpus maintenance; the clone pass is a whole-corpus statistic the delta does not re-derive (a documented nondeterministic class, clone_sig / similar_to)",
	"graph.CloneCorpusPager":                      "near-duplicate corpus maintenance; the clone pass is a whole-corpus statistic the delta does not re-derive (a documented nondeterministic class, clone_sig / similar_to)",
	"graph.CloneCorpusRepoReplacer":               "near-duplicate corpus maintenance; the clone pass is a whole-corpus statistic the delta does not re-derive (a documented nondeterministic class, clone_sig / similar_to)",
	"graph.CloneCorpusSignatureWriter":            "near-duplicate corpus maintenance; the clone pass is a whole-corpus statistic the delta does not re-derive (a documented nondeterministic class, clone_sig / similar_to)",
	"graph.CloneCorpusWriter":                     "near-duplicate corpus maintenance; the clone pass is a whole-corpus statistic the delta does not re-derive (a documented nondeterministic class, clone_sig / similar_to)",
	"graph.CloneShingleReader":                    "near-duplicate corpus maintenance; the clone pass is a whole-corpus statistic the delta does not re-derive (a documented nondeterministic class, clone_sig / similar_to)",
	"graph.ConstantValueRepoReplacer":             "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.ContentNodeReader":                     "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.ContractOwnerScalarInvalidator":        "optional companion of ContractOwnerReplacer; the fallback re-derives the scalars through ordinary node writes",
	"graph.ContractOwnerScalarLiveness":           "optional companion of ContractOwnerReplacer; absent, the fallback does not assume liveness and checks it through reads",
	"graph.ContractStateStore":                    "per-repository ledger read (index state, file metadata, mtimes, failures, receipts): absent, the engine treats nothing as recorded and takes its conservative path; a delta forces its explicit paths",
	"graph.CoordinatedBulkLoader":                 "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.CoverageEnrichmentWriter":              "node-keyed enrichment side tables (blame, churn, coverage, releases): their rows for an evicted node are the layer below's, hidden with the node; a delta writes none",
	"graph.CrossRepoFlagMarker":                   "cross-repository edge flagging; single-repository delta",
	"graph.CurrentGenerationRepoEvicter":          "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.EdgeEndpointReader":                    "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.EdgeIdentityBatchFinder":               "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.EdgeKindEvicter":                       "whole-graph eviction of an edge kind; not a file delta (DeltaWriter's EvictRepo-class refusal would apply)",
	"graph.EdgesByKindsBatchScanner":              "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.EdgesByKindsScanner":                   "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.ExistingNodeIDFinder":                  "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.ExternalCallCandidates":                "external-call synthesis candidate projection; absent, the synthesis scopes itself through Store reads",
	"graph.FileIndexFailureLayerReader":           "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.FileIndexFailureReader":                "per-repository ledger read (index state, file metadata, mtimes, failures, receipts): absent, the engine treats nothing as recorded and takes its conservative path; a delta forces its explicit paths",
	"graph.FileLanguageNodeSequencer":             "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.FileMetaPathReader":                    "per-repository ledger read (index state, file metadata, mtimes, failures, receipts): absent, the engine treats nothing as recorded and takes its conservative path; a delta forces its explicit paths",
	"graph.FileMetaReader":                        "per-repository ledger read (index state, file metadata, mtimes, failures, receipts): absent, the engine treats nothing as recorded and takes its conservative path; a delta forces its explicit paths",
	"graph.FileMetaRepoReplacer":                  "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.FileMtimeReader":                       "per-repository ledger read (index state, file metadata, mtimes, failures, receipts): absent, the engine treats nothing as recorded and takes its conservative path; a delta forces its explicit paths",
	"graph.FileReceiptPager":                      "poller receipt paging of the primary's watcher; a delta receives its paths from the coordinator",
	"graph.FnValuePlaceholderScanner":             "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.FrameworkCensusEdgeSequencer":          "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.GoMethodReceiverBatchRebinder":         "set-oriented Go receiver repair; the resolver's predicate fallback rebinds through Store reads and writes the delta records",
	"graph.GoMethodReceiverRebinder":              "set-oriented Go receiver repair; the resolver's predicate fallback rebinds through Store reads and writes the delta records",
	"graph.InDegreeForNodes":                      "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.IncomingSourceNodeChecker":             "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.LightEdgeScanner":                      "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.LightEdgeSequencer":                    "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.MemberMethodsByType":                   "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NamedLanguageNodeSequencer":            "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodeIDNamesByKindsSequencer":           "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodeIDsByKinds":                        "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodeLightScanner":                      "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodeLightSequencer":                    "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodeNameClassCounter":                  "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"graph.NodeSearchKeyScanner":                  "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodesByKindsScanner":                   "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.NodesByKindsSequencer":                 "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.OverlayDetachedFileSummaryReader":      "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.OverlayDetachedNodeReader":             "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.OverlayLayerEdgeEndpointReader":        "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.OverlayLayerRecordedEdgeReader":        "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.OverlayLocalizationIdentityReader":     "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.OverrideDispatchCallBatchScanner":      "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.PlannerStatsFreshener":                 "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"graph.PrefixDiagnosticsReader":               "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"graph.QualifiedNodeIdentitySequencer":        "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.ReceiverMutationScanner":               "whole-repository receiver-mutation projection; a delta's capability pass is bounded by the capability prior and reads through Store forms",
	"graph.RecordedEdgeReader":                    "served through RecordedEdgeProvider (DeltaWriter.RecordedEdges), the composed form the engine asks for via graph.RecordedEdgesOf",
	"graph.RefFactsRebuilder":                     "set-oriented SQL rebuild reads the store's own generation rows, which a delta does not hold; without it the engine computes the facts in Go through the delta's reads and writes them through RefFactsWriter",
	"graph.ReleaseEnrichmentWriter":               "node-keyed enrichment side tables (blame, churn, coverage, releases): their rows for an evicted node are the layer below's, hidden with the node; a delta writes none",
	"graph.RepoCapabilityEdgeScanner":             "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoFilePathReader":                    "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoIndexStateReader":                  "per-repository ledger read (index state, file metadata, mtimes, failures, receipts): absent, the engine treats nothing as recorded and takes its conservative path; a delta forces its explicit paths",
	"graph.RepoIndexStateWriter":                  "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.RepoLanguageCountReader":               "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoLanguageFileCountReader":           "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoLightNodeReader":                   "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoMetaNodeReader":                    "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoNodeIdentitySequencer":             "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.RepoNodeKindIDReader":                  "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.ScopeBindingNodeSequencer":             "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.ScopedIncomingSourceReader":            "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.SemanticBindingTypeReader":             "semantic (go/types) state: the enrichment stage runs over the generation handle after the delta (runEnrichment), not over the DeltaWriter",
	"graph.StructuralIntegrityEventRecorder":      "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"graph.StructuralParentEdges":                 "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.SymbolFTSNormalizationState":           "normalization reconcile rebuilds the whole repository's symbol index; the delta's documents are written in the process's mode (SymbolFTSBatchUpserter)",
	"graph.SymbolFTSRepoReplacer":                 "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.SymbolFTSRepoResetter":                 "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.SymbolSearcher":                        "search backend of the private Indexer; the delta writes symbol documents through SymbolFTSBatchUpserter and never serves a search",
	"graph.TestCallProjectionScanner":             "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.TestProjectionScanner":                 "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.UnresolvedEdgeIdentityBatchScanner":    "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.UnresolvedEdgePager":                   "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.UnresolvedEdgeTargetBatchReindexer":    "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"graph.UnresolvedFrontierCounter":             "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"graph.UnresolvedInsertionCounter":            "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"graph.ValueRefPlaceholderScanner":            "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.VectorSearcher":                        "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.WorkspaceSlugBackfiller":               "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.WorkspaceSlugImpactBackfiller":         "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"graph.callerBoundaryNodesContextStore":       "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.callerBoundaryOutgoingContextStore":    "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.containingNameContextReader":           "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.contextNodesByIDsReader":               "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.exactNameContextReader":                "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.exactNameContextVisitor":               "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"graph.exactNamesContextVisitor":              "optional exact-name read accelerator; absent, checked single-name visits read DeltaWriter.FindNodesByName through its composed view, preserving additions and shadows rather than forwarding a base-only batch",
	"graph.repoExactNamesContextVisitor":          "optional repo-qualified exact-name read accelerator; absent, checked composed-view name visits preserve DeltaWriter additions, shadows and tombstones before filtering repository ownership",
	"graph.layerExactNameContextReader":           "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.layerExactNameContextVisitor":          "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"graph.overlayStatsContextReader":             "composition contract of an overlay layer, a composed view or a reader wrapper; asserted on those objects, never on the store the engine writes",
	"indexer.GenerationBulkLoader":                "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"indexer.GenerationPayloadCopier":             "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"indexer.contextRepoWatcher":                  "watcher registration of the multi-repository indexer, not a store",
	"indexer.generationBulkCloserByID":            "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"indexer.generationReparseBulkLoader":         "initial dedicated-base reparse cache window on a qualified claimed-generation loader, never a per-save DeltaWriter; loaders without it retain ordinary generation bulk-load admission",
	"anonymous{GenerationCorrectionEpoch}":        "asserted on the daemon's store to key the per-stack caches of a corrected generation, never on a delta",
	"indexer.writeDemandReporter":                 "asserted by the lifecycle's background retirement on the daemon's shared store (write-gate demand), never on a delta",
	"indexer.mutationFanoutOwningReceiptStore":    "owning-window receipts separate sibling repositories sharing one store; a delta's working graph serves one repository and the plain receipt window is exact",
	"indexer.refFactsByTargetsContextReader":      "read accelerator: without it the caller takes the Store/Reader form, which DeltaWriter answers through the composed view (a cost, not a different answer)",
	"indexer.repoSemanticStateReleaser":           "semantic (go/types) state: the enrichment stage runs over the generation handle after the delta (runEnrichment), not over the DeltaWriter",
	"indexer.repoSemanticStateRetainer":           "semantic (go/types) state: the enrichment stage runs over the generation handle after the delta (runEnrichment), not over the DeltaWriter",
	"indexer.repositoryCleanupPayload":            "whole-repository or administrative operation (full index, repository reset, untrack or cleanup, warm start, vector corpus, bulk load windows, WAL maintenance); not on a file delta's per-save path",
	"resolver.backendResolveWorkIndicator":        "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"resolver.batchClaimingResolver":              "framework synthesizer contract, asserted on synthesizers",
	"resolver.claimEdgeVocabulary":                "framework synthesizer contract, asserted on synthesizers",
	"resolver.claimTargetVocabulary":              "framework synthesizer contract, asserted on synthesizers",
	"resolver.edgeMutationRevisioner":             "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"resolver.mutationRevisioner":                 "telemetry, cache-liveness or scheduling probe; absent, the metric or the cache shortcut is simply off",
	"resolver.scopedBackendResolver":              "whole-graph pass (ResolveAll, a cold warm-up, a whole-repository derived rebuild); a delta resolves and re-derives by its receipt frontier",
	"resolver.scopedSynthesizer":                  "framework synthesizer contract, asserted on synthesizers",

	"graph.OverlayLayerContractRepoProjectionReader": "checked physical repo contract projection; DeltaWriter exposes the composed LoadContractRepoProjectionContext and applies selected-layer visibility, never physical layer seeds",
	"graph.OverlayLayerNodeKindsByIDsReader":         "checked physical layer node-kind projection; DeltaWriter exposes composed GetNodeKindsByIDsContext with delta visibility, never a physical ancestry layer",
	"graph.Unwrapper":                                "transparent reader-wrapper capability; DeltaWriter is handled as a concrete composed reader before wrapper unwrapping, so unwrapping it would discard delta ownership",
}

// editDeltaCapabilityPackages are the engine packages whose assertions the
// checklist reads, relative to internal/indexer.
var editDeltaCapabilityPackages = map[string]string{
	"indexer":  ".",
	"resolver": "../resolver",
	"graph":    "../graph",
}

type editDeltaAssertion struct {
	name    string
	methods []string
	sites   []string
}

func TestDeltaWriterCapabilityChecklistIsComplete(t *testing.T) {
	interfaces := map[string][]string{} // qualified name -> method names
	embeds := map[string][]string{}     // qualified name -> embedded qualified names
	asserted := map[string]*editDeltaAssertion{}
	fset := token.NewFileSet()
	for pkg, dir := range editDeltaCapabilityPackages {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.TypeSpec:
					iface, ok := node.Type.(*ast.InterfaceType)
					if !ok {
						return true
					}
					qualified := pkg + "." + node.Name.Name
					methods, embedded := editDeltaInterfaceMethods(pkg, iface)
					interfaces[qualified] = methods
					embeds[qualified] = embedded
				case *ast.TypeAssertExpr:
					if node.Type == nil {
						return true
					}
					var qualified string
					var anonMethods []string
					switch typ := node.Type.(type) {
					case *ast.Ident:
						qualified = pkg + "." + typ.Name
					case *ast.SelectorExpr:
						if x, ok := typ.X.(*ast.Ident); ok {
							qualified = x.Name + "." + typ.Sel.Name
						}
					case *ast.InterfaceType:
						anonMethods, _ = editDeltaInterfaceMethods(pkg, typ)
						sort.Strings(anonMethods)
						qualified = "anonymous{" + strings.Join(anonMethods, ",") + "}"
						interfaces[qualified] = anonMethods
					}
					if qualified == "" {
						return true
					}
					rec := asserted[qualified]
					if rec == nil {
						rec = &editDeltaAssertion{name: qualified}
						asserted[qualified] = rec
					}
					rec.sites = append(rec.sites, fset.Position(node.Pos()).String())
				}
				return true
			})
		}
	}

	var resolve func(name string, seen map[string]bool) ([]string, bool)
	resolve = func(name string, seen map[string]bool) ([]string, bool) {
		if seen[name] {
			return nil, true
		}
		seen[name] = true
		methods, ok := interfaces[name]
		if !ok {
			return nil, false
		}
		out := append([]string(nil), methods...)
		for _, embedded := range embeds[name] {
			more, ok := resolve(embedded, seen)
			if !ok {
				continue
			}
			out = append(out, more...)
		}
		return out, true
	}
	writer := reflect.TypeOf((*graph.DeltaWriter)(nil))
	var implemented, absent []string
	unlisted := map[string]*editDeltaAssertion{}
	for name, rec := range asserted {
		methods, ok := resolve(name, map[string]bool{})
		if !ok || len(methods) == 0 {
			// Not an interface of the engine's packages (a concrete type, an
			// error type, a builtin): nothing a store could implement.
			continue
		}
		rec.methods = methods
		has := true
		for _, m := range methods {
			if _, found := writer.MethodByName(m); !found {
				has = false
				break
			}
		}
		if has {
			implemented = append(implemented, name)
			if _, listed := editDeltaAbsentCapabilities[name]; listed {
				t.Errorf("%s is listed as absent but *graph.DeltaWriter implements it; remove it from the list", name)
			}
			continue
		}
		absent = append(absent, name)
		if _, listed := editDeltaAbsentCapabilities[name]; !listed {
			unlisted[name] = rec
		}
	}
	for name := range editDeltaAbsentCapabilities {
		if _, ok := asserted[name]; !ok {
			t.Errorf("%s is listed as absent but no engine package asserts it any more; remove it from the list", name)
		}
	}
	sort.Strings(implemented)
	sort.Strings(absent)
	t.Logf("DeltaWriter implements %d asserted capabilities: %s", len(implemented), strings.Join(implemented, ", "))
	t.Logf("DeltaWriter lacks %d asserted capabilities (each listed with its reason)", len(absent))
	names := make([]string, 0, len(unlisted))
	for name := range unlisted {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := unlisted[name]
		t.Errorf("%s (methods %v) is asserted by the engine at %s and *graph.DeltaWriter neither implements it nor lists why",
			name, rec.methods, rec.sites[0])
	}
}

// editDeltaInterfaceMethods returns an interface literal's method names and
// its embedded interfaces, qualified.
func editDeltaInterfaceMethods(pkg string, iface *ast.InterfaceType) ([]string, []string) {
	var methods, embedded []string
	if iface.Methods == nil {
		return nil, nil
	}
	for _, field := range iface.Methods.List {
		if len(field.Names) > 0 {
			for _, n := range field.Names {
				methods = append(methods, n.Name)
			}
			continue
		}
		switch typ := field.Type.(type) {
		case *ast.Ident:
			embedded = append(embedded, pkg+"."+typ.Name)
		case *ast.SelectorExpr:
			if x, ok := typ.X.(*ast.Ident); ok {
				embedded = append(embedded, x.Name+"."+typ.Sel.Name)
			}
		}
	}
	return methods, embedded
}
