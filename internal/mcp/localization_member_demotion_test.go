package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func memberDemotionCandidate(file, name string, kind graph.NodeKind) *rerank.Candidate {
	return &rerank.Candidate{
		Node: &graph.Node{
			ID:         "fixture/" + file + "::" + name,
			Name:       name,
			Kind:       kind,
			FilePath:   "fixture/" + file,
			RepoPrefix: "fixture",
		},
		TextRank:   0,
		VectorRank: -1,
	}
}

func memberDemotionNames(cands []*rerank.Candidate) []string {
	names := make([]string, 0, len(cands))
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			names = append(names, "")
			continue
		}
		names = append(names, cand.Node.Name)
	}
	return names
}

func memberDemotionFiles(cands []*rerank.Candidate) []string {
	files := make([]string, 0, len(cands))
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			files = append(files, "")
			continue
		}
		files = append(files, cand.Node.FilePath)
	}
	return files
}

func TestLocalizationDemotionYieldsFrontSlotToSiblingCallable(t *testing.T) {
	tests := []struct {
		name    string
		member  string
		ordered []string
	}{
		{name: "constructor", member: "DirectoryConfiguration.<init>", ordered: []string{"detect", "DirectoryConfiguration.<init>"}},
		{name: "destructor", member: "deinit", ordered: []string{"detect", "deinit"}},
		{name: "subscript", member: "subscript", ordered: []string{"detect", "subscript"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := []*rerank.Candidate{
				memberDemotionCandidate("config.swift", test.member, graph.KindMethod),
				memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
			}
			require.Equal(t, test.ordered, memberDemotionNames(demoteLocalizationFileMembers(in)))
		})
	}
}

func TestLocalizationDemotionYieldsFrontSlotToSiblingMethodForDataMembers(t *testing.T) {
	tests := []struct {
		name   string
		member string
		kind   graph.NodeKind
	}{
		{name: "field", member: "shaderSlots", kind: graph.KindField},
		{name: "enum member", member: "ShaderStage.vertex", kind: graph.KindEnumMember},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := []*rerank.Candidate{
				memberDemotionCandidate("shaders.dart", test.member, test.kind),
				memberDemotionCandidate("shaders.dart", "bindShaders", graph.KindMethod),
			}
			require.Equal(t, []string{"bindShaders", test.member}, memberDemotionNames(demoteLocalizationFileMembers(in)))
		})
	}
}

func TestLocalizationDemotionKeepsTypesAndOrdinaryCallablesInOneLeadingTier(t *testing.T) {
	in := []*rerank.Candidate{
		memberDemotionCandidate("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod),
		memberDemotionCandidate("config.swift", "shaderSlots", graph.KindField),
		memberDemotionCandidate("config.swift", "DirectoryConfiguration", graph.KindType),
		memberDemotionCandidate("config.swift", "deinit", graph.KindMethod),
		memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
		memberDemotionCandidate("config.swift", "reload", graph.KindMethod),
	}
	require.Equal(t, []string{
		"DirectoryConfiguration", "detect", "reload",
		"DirectoryConfiguration.<init>", "deinit",
		"shaderSlots",
	}, memberDemotionNames(demoteLocalizationFileMembers(in)))
}

func TestLocalizationDemotionPermutesOnlyInsideEachFilesOwnSlots(t *testing.T) {
	in := []*rerank.Candidate{
		memberDemotionCandidate("config.swift", "shaderSlots", graph.KindField),
		memberDemotionCandidate("cache.dart", "ShaderCache.<init>", graph.KindMethod),
		memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
		nil,
		memberDemotionCandidate("cache.dart", "warm", graph.KindMethod),
		{Node: &graph.Node{ID: "fixture::floating", Name: "floating", Kind: graph.KindField}},
		memberDemotionCandidate("config.swift", "reload", graph.KindMethod),
	}
	files := memberDemotionFiles(in)
	ids := make(map[string]int, len(in))
	for _, cand := range in {
		if cand != nil && cand.Node != nil {
			ids[cand.Node.ID]++
		}
	}

	out := demoteLocalizationFileMembers(in)

	require.Equal(t, len(in), len(out), "the window keeps the same number of slots")
	require.Equal(t, files, memberDemotionFiles(out), "no candidate moves into another file's slot")
	outIDs := make(map[string]int, len(out))
	for _, cand := range out {
		if cand != nil && cand.Node != nil {
			outIDs[cand.Node.ID]++
		}
	}
	require.Equal(t, ids, outIDs, "the window keeps the same candidate set")
	require.Equal(t, []string{
		"detect", "warm", "reload", "", "ShaderCache.<init>", "floating", "shaderSlots",
	}, memberDemotionNames(out))
	require.Equal(t, memberDemotionNames(out), memberDemotionNames(demoteLocalizationFileMembers(out)), "the reorder is idempotent")
}

func memberDemotionSignalCandidate(file, name string, kind graph.NodeKind, signal string) *rerank.Candidate {
	candidate := memberDemotionCandidate(file, name, kind)
	candidate.Signals = map[string]float64{signal: 1}
	return candidate
}

func memberDemotionWindowFiles(cands []*rerank.Candidate, maxSymbols int) map[string]int {
	counts := make(map[string]int, maxSymbols)
	for index, cand := range cands {
		if index >= maxSymbols || cand == nil || cand.Node == nil {
			continue
		}
		counts[cand.Node.FilePath]++
	}
	return counts
}

func TestLocalizationSiblingLiftTradesAMemberSlotForItsOrdinarySibling(t *testing.T) {
	tests := []struct {
		name   string
		member string
		kind   graph.NodeKind
		lifted string
	}{
		{name: "constructor", member: "DirectoryConfiguration.<init>", kind: graph.KindMethod, lifted: "userInboundEventTriggered"},
		{name: "field", member: "shaderSlots", kind: graph.KindField, lifted: "applyPointLights"},
		{name: "enum member", member: "ShaderStage.vertex", kind: graph.KindEnumMember, lifted: "applyPointLights"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := []*rerank.Candidate{
				memberDemotionCandidate("config.swift", test.member, test.kind),
				memberDemotionCandidate("cache.swift", "warm", graph.KindMethod),
				memberDemotionCandidate("config.swift", test.lifted, graph.KindMethod),
			}
			before := memberDemotionWindowFiles(in, 2)

			out := liftLocalizationSiblingCallables("directory configuration detection is broken", in, 2, nil)

			require.Equal(t, []string{test.lifted, "warm", test.member}, memberDemotionNames(out))
			require.Equal(t, before, memberDemotionWindowFiles(out, 2), "the file keeps exactly the slots it won")
			require.Equal(t, memberDemotionFiles(in), memberDemotionFiles(out), "no candidate crosses into another file's slot")
		})
	}
}

func TestLocalizationSiblingLiftKeepsProvenAndCitedMemberSlots(t *testing.T) {
	tests := []struct {
		name   string
		task   string
		member *rerank.Candidate
		lift   func(cands []*rerank.Candidate) []*rerank.Candidate
	}{
		{
			name:   "source literal",
			task:   "directory configuration detection is broken",
			member: memberDemotionSignalCandidate("config.swift", "shaderSlots", graph.KindField, exploreSourceLiteralSignal),
		},
		{
			name:   "exact content",
			task:   "directory configuration detection is broken",
			member: memberDemotionSignalCandidate("config.swift", "shaderSlots", graph.KindField, exploreContentRecallExactSignal),
		},
		{
			name:   "syntactic anchor",
			task:   "directory configuration detection is broken",
			member: memberDemotionSignalCandidate("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod, exploreSyntacticAnchorSignal),
		},
		{
			name:   "identifier the task cites",
			task:   "shaderSlots is never reset",
			member: memberDemotionCandidate("config.swift", "shaderSlots", graph.KindField),
		},
		{
			name:   "protected anchor owner",
			task:   "directory configuration detection is broken",
			member: memberDemotionCandidate("config.swift", "shaderSlots", graph.KindField),
			lift: func(cands []*rerank.Candidate) []*rerank.Candidate {
				return liftLocalizationSiblingCallables(
					"directory configuration detection is broken", cands, 2,
					map[int]string{0: cands[0].Node.ID},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := []*rerank.Candidate{
				test.member,
				memberDemotionCandidate("cache.swift", "warm", graph.KindMethod),
				memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
			}
			lift := test.lift
			if lift == nil {
				lift = func(cands []*rerank.Candidate) []*rerank.Candidate {
					return liftLocalizationSiblingCallables(test.task, cands, 2, nil)
				}
			}
			require.Equal(t, memberDemotionNames(in), memberDemotionNames(lift(in)))
		})
	}
}

func TestLocalizationSiblingLiftIsANoOpWithoutAComparableSibling(t *testing.T) {
	member := memberDemotionCandidate("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod)
	tail := func(extra ...*rerank.Candidate) []*rerank.Candidate {
		return append([]*rerank.Candidate{member, memberDemotionCandidate("cache.swift", "warm", graph.KindMethod)}, extra...)
	}
	distant := make([]*rerank.Candidate, 0, localizationSiblingLiftReach+1)
	for i := 0; i < localizationSiblingLiftReach; i++ {
		distant = append(distant, memberDemotionCandidate("cache.swift", fmt.Sprintf("warm%d", i), graph.KindMethod))
	}
	distant = append(distant, memberDemotionCandidate("config.swift", "detect", graph.KindMethod))

	tests := []struct {
		name string
		in   []*rerank.Candidate
	}{
		{name: "sibling never retrieved", in: tail(memberDemotionCandidate("cache.swift", "reload", graph.KindMethod))},
		{name: "only members below the cut", in: tail(memberDemotionCandidate("config.swift", "root", graph.KindField))},
		{name: "sibling past the reach", in: tail(distant...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, memberDemotionNames(test.in), memberDemotionNames(
				liftLocalizationSiblingCallables("directory configuration detection is broken", test.in, 2, nil),
			))
		})
	}
}

func memberDemotionTarget(file, name string, kind graph.NodeKind) exploreTarget {
	return exploreTarget{node: memberDemotionCandidate(file, name, kind).Node}
}

func memberDemotionTargetNames(targets []exploreTarget) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.node == nil {
			names = append(names, "")
			continue
		}
		names = append(names, target.node.Name)
	}
	return names
}

func memberDemotionTargetFiles(targets []exploreTarget) []string {
	files := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.node == nil {
			files = append(files, "")
			continue
		}
		files = append(files, target.node.FilePath)
	}
	return files
}

func TestLocalizationEvidenceDemotionSeatsSiblingCallableBehindFoldedOwner(t *testing.T) {
	owner := memberDemotionTarget("config.swift", "DirectoryConfiguration", graph.KindType)
	owner.foldedOwner = true
	constructor := memberDemotionTarget("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod)
	constructor.conceptImplementation = true
	in := []exploreTarget{owner, constructor, memberDemotionTarget("config.swift", "detect", graph.KindMethod)}

	out := demoteLocalizationEvidenceMembers("directory configuration detection is broken", "", in)

	require.Equal(t, []string{
		"DirectoryConfiguration", "detect", "DirectoryConfiguration.<init>",
	}, memberDemotionTargetNames(out))
}

func TestLocalizationEvidenceDemotionSeatsSiblingCallableAheadOfLeadingField(t *testing.T) {
	field := memberDemotionTarget("lighting.dart", "shaderSlots", graph.KindField)
	field.conceptImplementation = true
	in := []exploreTarget{
		field,
		memberDemotionTarget("lighting.dart", "LightingInfo", graph.KindType),
		memberDemotionTarget("lighting.dart", "bindShaders", graph.KindMethod),
	}

	out := demoteLocalizationEvidenceMembers("lighting info shader slots are wrong", "", in)

	require.Equal(t, []string{"LightingInfo", "bindShaders", "shaderSlots"}, memberDemotionTargetNames(out))
}

func TestLocalizationEvidenceDemotionNeedsASameFileSiblingOnThePage(t *testing.T) {
	in := []exploreTarget{
		memberDemotionTarget("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod),
		memberDemotionTarget("cache.swift", "warm", graph.KindMethod),
		memberDemotionTarget("cache.swift", "ShaderCache", graph.KindType),
	}

	require.Equal(t, memberDemotionTargetNames(in), memberDemotionTargetNames(
		demoteLocalizationEvidenceMembers("directory configuration detection is broken", "", in),
	), "a member with no sibling of its own file on the page keeps its seat")
}

func TestLocalizationEvidenceDemotionKeepsReservedSeats(t *testing.T) {
	proven := memberDemotionTarget("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod)
	proven.divergentDefaultOwner = true
	projected := memberDemotionTarget("cache.swift", "slots", graph.KindField)
	projected.typedAnchorProjection = true
	tests := []struct {
		name       string
		task       string
		requiredID string
		in         []exploreTarget
	}{
		{
			name: "graph proven constructor",
			task: "directory configuration detection is broken",
			in: []exploreTarget{
				proven,
				memberDemotionTarget("config.swift", "detect", graph.KindMethod),
			},
		},
		{
			name: "typed anchor field",
			task: "shader cache slots are wrong",
			in: []exploreTarget{
				projected,
				memberDemotionTarget("cache.swift", "warm", graph.KindMethod),
			},
		},
		{
			name: "identifier the task cites",
			task: "shaderSlots is never reset",
			in: []exploreTarget{
				memberDemotionTarget("config.swift", "shaderSlots", graph.KindField),
				memberDemotionTarget("config.swift", "detect", graph.KindMethod),
			},
		},
		{
			name:       "prescribed refinement symbol",
			task:       "directory configuration detection is broken",
			requiredID: "fixture/config.swift::DirectoryConfiguration.<init>",
			in: []exploreTarget{
				memberDemotionTarget("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod),
				memberDemotionTarget("config.swift", "detect", graph.KindMethod),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, memberDemotionTargetNames(test.in), memberDemotionTargetNames(
				demoteLocalizationEvidenceMembers(test.task, test.requiredID, test.in),
			))
		})
	}
}

func TestLocalizationEvidenceDemotionKeepsCrossFileOrderAndSelection(t *testing.T) {
	in := []exploreTarget{
		memberDemotionTarget("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod),
		memberDemotionTarget("cache.dart", "slots", graph.KindField),
		memberDemotionTarget("config.swift", "detect", graph.KindMethod),
		memberDemotionTarget("cache.dart", "warm", graph.KindMethod),
		{},
		memberDemotionTarget("config.swift", "root", graph.KindField),
	}
	files := memberDemotionTargetFiles(in)
	ids := make(map[string]int, len(in))
	for _, target := range in {
		if target.node != nil {
			ids[target.node.ID]++
		}
	}

	out := demoteLocalizationEvidenceMembers("directory configuration detection is broken", "", in)

	require.Equal(t, len(in), len(out), "the projection keeps the same number of rows")
	require.Equal(t, files, memberDemotionTargetFiles(out), "no row moves into another file's slot")
	outIDs := make(map[string]int, len(out))
	for _, target := range out {
		if target.node != nil {
			outIDs[target.node.ID]++
		}
	}
	require.Equal(t, ids, outIDs, "the draft's selection is unchanged")
	require.Equal(t, []string{
		"detect", "warm", "DirectoryConfiguration.<init>", "slots", "", "root",
	}, memberDemotionTargetNames(out))
}

func newIndexedSwiftMemberDemotionServer(t *testing.T) *Server {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Sources"), 0o755))
	files := map[string]string{
		"DirectoryConfiguration.swift": `
import Foundation

/// Directory configuration for the shader cache.
public struct DirectoryConfiguration {
    /// Root directory scanned when directory configuration detection runs.
    let root: String
    /// Shader slots reserved by the directory configuration.
    let shaderSlots: Int

    /// Creates a directory configuration.
    public init(root: String, shaderSlots: Int) {
        self.root = root
        self.shaderSlots = shaderSlots
    }

    deinit {
    }

    subscript(index: Int) -> String {
        return root
    }

    /// Detects the directory configuration for a nested path.
    public func detect(path: String) -> Bool {
        return !path.isEmpty
    }

    public func reload(path: String) -> Bool {
        return detect(path: path)
    }
}
`,
		"ShaderCache.swift": `
import Foundation

public struct ShaderCache {
    let slots: Int

    public init(slots: Int) {
        self.slots = slots
    }

    public func warm(configuration: DirectoryConfiguration) -> Bool {
        return configuration.detect(path: "/tmp")
    }
}
`,
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, "Sources", name), []byte(content), 0o644))
	}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	registry := parser.NewRegistry()
	registry.Register(languages.NewSwiftExtractor())
	idx := indexer.New(store, registry, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRepoPrefix("swift-fixture")
	idx.SetWorkspaceID("swift-fixture")
	idx.SetProjectID("swift-fixture")
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)

	engine := query.NewEngine(store)
	engine.SetSearchProvider(idx.Search)
	return NewServer(engine, store, idx, nil, zap.NewNop(), nil)
}

func TestIndexedSwiftMemberDemotionAppliesOnlyToTheLocalizePage(t *testing.T) {
	const (
		task     = "directory configuration detection is broken"
		fieldID  = "swift-fixture/Sources/DirectoryConfiguration.swift::DirectoryConfiguration.root"
		methodID = "swift-fixture/Sources/DirectoryConfiguration.swift::DirectoryConfiguration.detect"
	)
	server := newIndexedSwiftMemberDemotionServer(t)
	call := func(localize bool) *mcpgo.CallToolResult {
		request := mcpgo.CallToolRequest{}
		request.Params.Arguments = map[string]any{
			"task":          task,
			"localize":      localize,
			"max_symbols":   8,
			"token_budget":  2400,
			"repository_id": "swift-fixture",
		}
		result, err := server.handleExplore(context.Background(), request)
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.NotEmpty(t, result.Content)
		return result
	}

	localized, ok := call(true).Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(localized.Text), &envelope))
	fieldRank, methodRank := -1, -1
	for index, evidence := range envelope.Evidence {
		switch evidence.ID {
		case fieldID:
			fieldRank = index
		case methodID:
			methodRank = index
		}
	}
	require.GreaterOrEqual(t, methodRank, 0, "the sibling method stays in the window: %#v", envelope.Evidence)
	require.GreaterOrEqual(t, fieldRank, 0, "the demoted field stays in the window: %#v", envelope.Evidence)
	require.Less(t, methodRank, fieldRank, "localization seats the sibling method ahead of the field: %#v", envelope.Evidence)

	// The non-localize lane keeps whatever order ranking produced, so this
	// fixture's field — whose doc comment carries every task term — still leads
	// its sibling method there.
	plain, plainOK := call(false).Content[0].(mcpgo.TextContent)
	require.True(t, plainOK)
	plainField := strings.Index(plain.Text, "id: "+fieldID)
	plainMethod := strings.Index(plain.Text, "id: "+methodID)
	require.GreaterOrEqual(t, plainField, 0, "explore lists the ranked field: %s", plain.Text)
	require.GreaterOrEqual(t, plainMethod, 0, "explore lists the ranked method: %s", plain.Text)
	require.Less(t, plainField, plainMethod, "the non-localize page is untouched by localization demotion: %s", plain.Text)
}

func TestIndexedSwiftMemberDemotionKeepsAnEditableSiblingInATightWindow(t *testing.T) {
	const task = "directory configuration detection is broken"
	server := newIndexedSwiftMemberDemotionServer(t)
	request := mcpgo.CallToolRequest{}
	request.Params.Arguments = map[string]any{
		"task":          task,
		"localize":      true,
		"max_symbols":   3,
		"token_budget":  2400,
		"repository_id": "swift-fixture",
	}
	result, err := server.handleExplore(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text, ok := result.Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))

	names := make([]string, 0, len(envelope.Evidence))
	for _, evidence := range envelope.Evidence {
		names = append(names, evidence.Name)
	}
	// Ranking hands this file's three slots to its type, constructor and a
	// field; the sibling methods are retrieved but lose the cut, and no later
	// stage can recover a candidate the cut dropped.
	require.Equal(t, []string{"DirectoryConfiguration", "detect", "reload"}, names,
		"unexpected tight-window page: %#v", envelope.Evidence)
}

func TestIndexedSwiftMemberDemotionOrdersTheLocalizationPageLeadingFile(t *testing.T) {
	const (
		task = "directory configuration detection is broken"
		file = "swift-fixture/Sources/DirectoryConfiguration.swift"
	)
	server := newIndexedSwiftMemberDemotionServer(t)
	request := mcpgo.CallToolRequest{}
	request.Params.Arguments = map[string]any{
		"task":          task,
		"localize":      true,
		"max_symbols":   8,
		"token_budget":  2400,
		"repository_id": "swift-fixture",
	}
	result, err := server.handleExplore(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text, ok := result.Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))

	leading := make([]string, 0, len(envelope.Evidence))
	for _, evidence := range envelope.Evidence {
		if evidence.File == file {
			leading = append(leading, evidence.Name)
		}
	}
	// Owner folding seats the type ahead of its members; the members that follow
	// are ordered by tier, and every one of them stays on the page.
	require.Equal(t, []string{
		"DirectoryConfiguration", "detect", "reload",
		"DirectoryConfiguration.<init>", "deinit",
		"root", "shaderSlots",
	}, leading, "unexpected leading-file order: %#v", envelope.Evidence)
}
