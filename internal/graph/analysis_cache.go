package graph

import (
	"context"
	"errors"
)

var (
	// ErrAnalysisGenerationCorrupt reports a poisoned active generation. The
	// store clears the active pointer before returning it, so callers may log
	// the diagnostic and safely recompute.
	ErrAnalysisGenerationCorrupt = errors.New("analysis generation is corrupt")
	// ErrAnalysisGenerationInactive prevents direct reads of stale or
	// superseded generations after the active pointer has moved or cleared.
	ErrAnalysisGenerationInactive = errors.New("analysis generation is not active")
)

// AnalysisComponent is one independently sealed part of a durable analysis
// generation. A generation is invisible until every required component is
// sealed and activation succeeds under the graph mutation gate.
type AnalysisComponent string

const (
	AnalysisComponentNodes       AnalysisComponent = "nodes"
	AnalysisComponentCommunities AnalysisComponent = "communities"
	AnalysisComponentProcesses   AnalysisComponent = "processes"
	AnalysisComponentConcepts    AnalysisComponent = "concepts"
	AnalysisComponentAdjacency   AnalysisComponent = "adjacency_csr"
	AnalysisComponentLeiden      AnalysisComponent = "leiden_state"
)

// AnalysisBlobComponent is deliberately narrower than AnalysisComponent:
// row-addressable results are normalized, while dense algorithm state remains
// a compact versioned blob.
type AnalysisBlobComponent string

const (
	AnalysisBlobAdjacency AnalysisBlobComponent = "adjacency_csr"
	AnalysisBlobLeiden    AnalysisBlobComponent = "leiden_state"
)

// AnalysisMetric selects a normalized node score and its matching keyset
// index. Values outside this closed set must be rejected rather than
// interpolated into SQL.
type AnalysisMetric string

const (
	AnalysisMetricPageRank  AnalysisMetric = "pagerank"
	AnalysisMetricAuthority AnalysisMetric = "authority"
	AnalysisMetricHub       AnalysisMetric = "hub"
)

// AnalysisConceptDirection selects outgoing or incoming ranked relations.
type AnalysisConceptDirection string

const (
	AnalysisConceptForward AnalysisConceptDirection = "forward"
	AnalysisConceptReverse AnalysisConceptDirection = "reverse"
)

// AnalysisGenerationHeader is the small, eagerly loaded manifest for one
// immutable analysis snapshot. GraphRevision returned by
// LoadActiveAnalysisHeader is a process-local load receipt; the persisted
// build revision is diagnostic only and is never compared across restarts.
type AnalysisGenerationHeader struct {
	GenerationID              int64
	FormatVersion             uint32
	GraphRevision             uint64
	CreatedAtUnix             int64
	NodeCount                 int
	CommunityCount            int
	ProcessCount              int
	ConceptCount              int
	PageRankMax               float64
	AuthorityMax              float64
	HubMax                    float64
	Modularity                float64
	ProcessesTruncated        bool
	ProcessesTruncationReason string
}

type AnalysisNodeMetric struct {
	RowID       int64
	NodeID      string
	CommunityID string
	PageRank    float64
	Authority   float64
	Hub         float64
}

type AnalysisMetricCursor struct {
	Score float64
	RowID int64
}

type AnalysisCommunitySummary struct {
	ID       string
	Label    string
	Hub      string
	ParentID string
	Size     int
	Cohesion float64
	Files    []string
}

type AnalysisProcessSummary struct {
	ID         string
	Name       string
	EntryPoint string
	StepCount  int
	Score      float64
	Truncated  bool
	Files      []string
}

type AnalysisProcessStep struct {
	ProcessID string
	NodeID    string
	Ordinal   int
	Depth     int
}

// AnalysisMembership retains one row per process step, including repeated nodes.
type AnalysisMembership struct {
	NodeID      string
	CommunityID string
	ProcessID   string
}

type AnalysisProcessMembership struct {
	NodeID    string
	ProcessID string
}

type AnalysisConcept struct {
	Token        string
	InVocabulary bool
}

type AnalysisConceptRelation struct {
	Token        string
	RelatedToken string
	Rank         int
}

type AnalysisConceptQueryResult struct {
	Concepts  []AnalysisConcept
	Relations []AnalysisConceptRelation
}

type AnalysisBlob struct {
	Component AnalysisBlobComponent
	Payload   []byte
}

// AnalysisGenerationStore writes immutable generations in bounded chunks.
// Every mutating method re-checks expectedRevision while holding the same
// mutation gate used by graph writes. Partial generations remain invisible.
type AnalysisGenerationStore interface {
	AnalysisMutationRevision() uint64
	CommitAnalysisSnapshot(expectedRevision uint64, install func()) bool
	BeginAnalysisGeneration(expectedRevision uint64, header AnalysisGenerationHeader) (generationID int64, accepted bool, err error)
	AppendAnalysisNodes(expectedRevision uint64, generationID int64, nodes []AnalysisNodeMetric) (accepted bool, err error)
	AppendAnalysisCommunities(expectedRevision uint64, generationID int64, communities []AnalysisCommunitySummary) (accepted bool, err error)
	AppendAnalysisProcesses(expectedRevision uint64, generationID int64, processes []AnalysisProcessSummary, steps []AnalysisProcessStep) (accepted bool, err error)
	AppendAnalysisConcepts(expectedRevision uint64, generationID int64, concepts []AnalysisConcept, relations []AnalysisConceptRelation) (accepted bool, err error)
	PutAnalysisBlob(expectedRevision uint64, generationID int64, blob AnalysisBlob) (accepted bool, err error)
	SealAnalysisComponent(expectedRevision uint64, generationID int64, component AnalysisComponent, expectedRows int) (accepted bool, err error)
	ActivateAnalysisGeneration(expectedRevision uint64, generationID int64) (accepted bool, err error)
	AbortAnalysisGeneration(generationID int64) error
	// PruneAnalysisGenerations returns the number of rows deleted by committed
	// chunks, including when a later chunk returns an error.
	PruneAnalysisGenerations(ctx context.Context, keep, batch int) (removed int64, err error)
}

// AnalysisQueryStore exposes bounded point, batch, top-N, and keyset-paged
// reads. Empty ID/token inputs always mean an empty result, never an unbounded
// scan.
type AnalysisQueryStore interface {
	LoadActiveAnalysisHeader(formatVersion uint32) (AnalysisGenerationHeader, bool, error)
	AnalysisNodeMetrics(generationID int64, nodeIDs []string) ([]AnalysisNodeMetric, error)
	ListAnalysisNodeMetrics(generationID int64, limit int, cursorNodeID string) ([]AnalysisNodeMetric, string, error)
	TopAnalysisNodeMetrics(generationID int64, metric AnalysisMetric, limit int, cursor *AnalysisMetricCursor) ([]AnalysisNodeMetric, *AnalysisMetricCursor, error)
	ListAnalysisCommunitySummaries(generationID int64, limit int, cursorID string) ([]AnalysisCommunitySummary, string, error)
	AnalysisCommunityMembers(generationID int64, communityID string, limit int, cursorNodeID string) ([]AnalysisNodeMetric, string, error)
	ListAnalysisProcessSummaries(generationID int64, limit int, cursorID string) ([]AnalysisProcessSummary, string, error)
	AnalysisProcessSteps(generationID int64, processID string, limit int, cursorOrdinal int) ([]AnalysisProcessStep, int, error)
	AnalysisProcessesForNodes(generationID int64, nodeIDs []string) ([]AnalysisProcessMembership, error)
	AnalysisMembershipsContext(ctx context.Context, generationID int64, nodeIDs []string) ([]AnalysisMembership, error)
	AnalysisConcepts(generationID int64, tokens []string, direction AnalysisConceptDirection) (AnalysisConceptQueryResult, error)
	ListAnalysisConcepts(generationID int64, limit int, cursorToken string) (AnalysisConceptQueryResult, string, error)
	LoadAnalysisBlob(generationID int64, component AnalysisBlobComponent) ([]byte, bool, error)
}
