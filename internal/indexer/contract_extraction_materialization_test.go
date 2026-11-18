package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// Use a Go suffix with an unsupported runtime language so admission cannot
// accidentally depend on the filename rather than the actual extractor set.
func ordinaryMaterializationFixture(t *testing.T, language string) (*contractConvergence, *Indexer, *contractFollowupEvidence, *contractPinnedReader, ContractFollowupFile, *int) {
	t.Helper()
	return ordinaryMaterializationFileFixture(t, language, "repo/dashboard.go")
}

func ordinaryMaterializationFileFixture(t *testing.T, language, path string) (*contractConvergence, *Indexer, *contractFollowupEvidence, *contractPinnedReader, ContractFollowupFile, *int) {
	t.Helper()
	e := extractionFileEvidence(t, nil, map[string]any{"signature": "selected declaration"})
	idx := &Indexer{graph: e, config: config.Default().Index, logger: zap.NewNop()}
	policy, err := contractFollowupPolicy(idx, language)
	require.NoError(t, err)
	file := ContractFollowupFile{Path: path, RepoPrefix: "repo", Language: language, Policy: policy}
	delete(e.files, "repo/dashboard.go")
	e.files[file.Path] = file
	const text = "package fixture\nfunc render() {}\n"
	fingerprint := contractInputHash([]byte(text))
	c := &contractConvergence{ctx: t.Context(), files: e.files, report: &ContractFollowupReport{}}
	c.snap.ReadAccepted = func(ctx context.Context, got ContractFollowupFile) (ContractAcceptedSource, error) {
		require.NoError(t, ctx.Err())
		require.Equal(t, file, got)
		return ContractAcceptedSource{Bytes: []byte(text), SourceFingerprint: fingerprint, Policy: policy}, nil
	}
	reader := &contractPinnedReader{c: c, stamps: map[string]ContractReceiptStamp{file.Path: {Accepted: true, SourceFingerprint: fingerprint}}, ledger: func() *contractUnitLedger { return e.ledger }, moved: make(map[string]bool)}
	calls := new(int)
	readCore := e.readCore
	e.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		*calls++
		rows, err := readCore(ctx, file)
		for _, node := range rows.Nodes {
			node.ID = strings.Replace(node.ID, "repo/dashboard.go", path, 1)
			node.FilePath = path
		}
		return rows, err
	}
	return c, idx, e, reader, file, calls
}

func TestContractOrdinaryNoExtractorRetainsUnitWithoutScratchImport(t *testing.T) {
	c, idx, e, reader, file, calls := ordinaryMaterializationFixture(t, "unhandled")
	_, extractors := idx.buildPerFileContractExtractors()
	require.Empty(t, extractors[file.Language], "this case must have no applicable runtime extractor")
	oracle := extractionFileEvidence(t, nil, map[string]any{"signature": "selected declaration"})
	nodes, seeds, err := oracle.loadExtractionFile(file.Path)
	require.NoError(t, err)
	require.Positive(t, oracle.writtenNodes, "the original path really imports selected rows")
	unit, err := c.extract(idx, e, reader, file, e.ledger)
	require.NoError(t, err)
	require.NotNil(t, unit)
	require.Equal(t, 1, *calls, "a checked custom reader remains observable exactly once")
	require.Equal(t, 1, c.report.Files, "the complete file unit still consumes its ordinary slice")
	require.Equal(t, 1, c.report.SourceReads)
	require.Equal(t, len("package fixture\nfunc render() {}\n"), c.report.SourceBytes)
	require.Contains(t, unit.declared, "name:render")
	require.Contains(t, unit.declared, "id:repo/dashboard.go::render")
	require.Equal(t, reader.stamps[file.Path].SourceFingerprint, unit.ledger.sources[file.Path])
	require.Equal(t, oracle.ledger.reads, unit.ledger.reads, "the original extraction question, owners and full answer digest remain identical")
	digest, ok := contractAnswerDigest(contractFileAnswer{Nodes: nodes, Edges: seeds})
	require.True(t, ok)
	require.Equal(t, digest, unit.ledger.reads[0].digest)
	require.Empty(t, unit.raw)
	require.Empty(t, unit.seedNodes)
	require.Empty(t, unit.seedEdges)
	require.Zero(t, e.writtenNodes, "ordinary files without extractor or tracker consumers need no scratch materialization")
	require.Zero(t, e.writtenEdges)
	require.Empty(t, e.scratch.GetFileNodes(file.Path))
}

func TestContractOrdinaryMaterializationKeepsActiveAndTrackerRows(t *testing.T) {
	for _, mode := range []string{"active_extractor", "router_census", "go_mod", "spring_config", "java_method", "di_edge", "spring_call", "preloaded_private_rows", "preloaded_extraction", "preloaded_type_shape", "preloaded_interface_shape"} {
		t.Run(mode, func(t *testing.T) {
			language := "unhandled"
			if mode == "active_extractor" {
				language = "go"
			}
			path := "repo/dashboard.go"
			if mode == "router_census" {
				path = "repo/dashboard.ts"
			}
			if mode == "go_mod" {
				path = "repo/go.mod"
			}
			c, idx, e, reader, file, calls := ordinaryMaterializationFileFixture(t, language, path)
			readCore := e.readCore
			e.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
				rows, err := readCore(ctx, file)
				if err != nil {
					return rows, err
				}
				switch mode {
				case "spring_config":
					rows.Nodes[1].Meta["spring_config_keys"] = []string{"service.port"}
				case "java_method":
					rows.Nodes[1].Kind, rows.Nodes[1].Language = graph.KindMethod, "java"
				case "di_edge":
					rows.Edges = []*graph.Edge{{From: rows.Nodes[1].ID, To: "repo/provider.go::Service", Kind: graph.EdgeProvides, FilePath: file.Path, Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "TOKEN"}}}
				case "spring_call":
					rows.Edges = []*graph.Edge{{From: rows.Nodes[1].ID, To: "repo/provider.go::Bean", Kind: graph.EdgeCalls, FilePath: file.Path, Meta: map[string]any{"via": "spring.Bean"}}}
				}
				return rows, nil
			}
			preloaded := strings.HasPrefix(mode, "preloaded_")
			var privateAnswer string
			if preloaded {
				var nodes []*graph.Node
				var err error
				switch mode {
				case "preloaded_private_rows":
					nodes, _, err = e.loadFile(file.Path)
				case "preloaded_extraction":
					nodes, _, err = e.loadExtractionFile(file.Path)
				default:
					leave := e.guard.enter()
					rows, readErr := e.readCore(e.ctx, file)
					leave()
					require.NoError(t, readErr)
					nodes, err = e.loadFileNodesPrefetched(file.Path, rows.Nodes, true)
				}
				require.NoError(t, err)
				nodes[1].Meta["signature"] = "private enriched declaration"
				if mode == "preloaded_type_shape" || mode == "preloaded_interface_shape" {
					nodes[1].Kind = graph.KindType
					if mode == "preloaded_interface_shape" {
						nodes[1].Kind = graph.KindInterface
					}
					nodes[1].Meta["shape"] = "private shape"
				}
				e.AddNode(nodes[1])
				var ok bool
				privateAnswer, ok = contractAnswerDigest(contractFileAnswer{Nodes: nodes})
				require.True(t, ok)
			}
			before := *calls
			unit, err := c.extract(idx, e, reader, file, e.ledger)
			require.NoError(t, err)
			require.NotNil(t, unit)
			require.Positive(t, e.writtenNodes, "active extractors and tracker/private state must keep the original import")
			require.NotEmpty(t, e.scratch.GetFileNodes(file.Path))
			if preloaded {
				wantCalls := before
				if mode == "preloaded_type_shape" || mode == "preloaded_interface_shape" {
					wantCalls++ // node-only loaded state still reads selected edges
					require.True(t, e.outputIDs[file.Path+"::render"])
				}
				require.Equal(t, wantCalls, *calls, "private loaded rows must not be replaced by a detached core reread")
				require.Equal(t, "private enriched declaration", e.scratch.GetNode(file.Path + "::render").Meta["signature"])
				require.Contains(t, unit.ledger.reads, contractRecordedRead{kind: contractReadExtractionFile, args: []string{file.Path}, digest: privateAnswer, owners: []string{file.Path}}, "extraction must answer from the actual private rows, including shape metadata")
			} else {
				require.Equal(t, before+1, *calls, "fallback must import the same checked answer without rereading a custom reader")
			}
			switch mode {
			case "spring_config":
				require.True(t, e.springIDs[file.Path+"::render"])
			case "java_method":
				require.True(t, e.javaMethodIDs[file.Path+"::render"])
			case "di_edge":
				require.Len(t, e.diEdges, 1)
			case "spring_call":
				require.Len(t, e.outputEdges, 1)
			}
		})
	}
}

func TestContractOrdinaryNoExtractorKeepsCheckedFailures(t *testing.T) {
	for _, mode := range []string{"checked_error", "malformed_node", "malformed_edge", "unencodable_metadata", "row_limit", "nonseed_edge_byte_limit", "source_mismatch", "policy_mismatch", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			c, idx, e, reader, file, _ := ordinaryMaterializationFixture(t, "unhandled")
			readCore := e.readCore
			e.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
				rows, err := readCore(ctx, file)
				switch mode {
				case "checked_error":
					return rows, graph.ErrContractProjectionStale
				case "malformed_node":
					rows.Nodes[1].FilePath = "repo/foreign.go"
				case "malformed_edge":
					rows.Edges = []*graph.Edge{{From: rows.Nodes[1].ID}}
				case "unencodable_metadata":
					rows.Nodes[1].Meta["unsupported"] = func() {}
				case "row_limit":
					rows.Nodes = make([]*graph.Node, graph.ContractProjectionRowLimit+1)
				case "nonseed_edge_byte_limit":
					rows.Edges = []*graph.Edge{{From: rows.Nodes[1].ID, To: "repo/target.go::Call", Kind: graph.EdgeCalls, FilePath: file.Path, Origin: strings.Repeat("x", contractFollowupCompactLimit)}}
				}
				return rows, err
			}
			switch mode {
			case "source_mismatch":
				c.snap.ReadAccepted = func(context.Context, ContractFollowupFile) (ContractAcceptedSource, error) {
					return ContractAcceptedSource{Bytes: []byte("changed"), SourceFingerprint: reader.stamps[file.Path].SourceFingerprint, Policy: file.Policy}, nil
				}
			case "policy_mismatch":
				file.Policy += "changed"
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				c.ctx, e.ctx = ctx, ctx
			}
			unit, err := c.extract(idx, e, reader, file, e.ledger)
			require.Error(t, err)
			require.Nil(t, unit)
			if mode == "row_limit" || mode == "nonseed_edge_byte_limit" {
				require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
			}
		})
	}
}
