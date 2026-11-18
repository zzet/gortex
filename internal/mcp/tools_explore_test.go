package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestExploreLocalizableKind(t *testing.T) {
	// Real edit targets are localizable; structural noise is not.
	localizable := []graph.NodeKind{
		graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindField, graph.KindConstant,
		graph.KindVariable,
	}
	for _, k := range localizable {
		if !exploreLocalizableKind(k) {
			t.Errorf("kind %q should be localizable", k)
		}
	}
	noise := []graph.NodeKind{
		graph.KindParam, graph.KindLocal, graph.KindClosure,
		graph.KindGenericParam, graph.KindImport, graph.KindFile,
	}
	for _, k := range noise {
		if exploreLocalizableKind(k) {
			t.Errorf("kind %q should be filtered out", k)
		}
	}
}

func exploreTestTargets() []exploreTarget {
	fn := &graph.Node{ID: "retry.go::DoWithRetry", Name: "DoWithRetry", Kind: graph.KindFunction,
		FilePath: "retry.go", StartLine: 11, EndLine: 20, Language: "go"}
	helper := &graph.Node{ID: "retry.go::Backoff", Name: "Backoff", Kind: graph.KindFunction,
		FilePath: "retry.go", StartLine: 6, EndLine: 8, Language: "go"}
	caller := &graph.Node{ID: "client.go::Fetch", Name: "Fetch", Kind: graph.KindFunction,
		FilePath: "client.go", StartLine: 4, EndLine: 6, Language: "go"}
	return []exploreTarget{
		{node: fn, score: 0.9, callers: []*graph.Node{caller}, callees: []*graph.Node{helper},
			source: "func DoWithRetry(max int) error {\n\treturn nil\n}"},
		{node: helper, score: 0.5, callers: []*graph.Node{fn},
			source: "func Backoff(n int) int {\n\treturn n\n}"},
	}
}

func TestRenderExploreShapeIsNonTerminal(t *testing.T) {
	out := (&Server{}).renderExplore("the retry backoff never fires on 429", exploreTestTargets(), 9000)

	// Ranked targets, with citeable path:line locations and an answer draft
	// before the detailed payload.
	for _, want := range []string{
		"EXPLORE — the retry backoff never fires on 429",
		"RANKED LOCALIZATION:",
		"Localization-only callers receive a separate completion contract",
		"diagnosis and change callers continue from this evidence",
		"## Answer draft",
		"FILE: retry.go:11-20  ·  SYMBOL: DoWithRetry  ·  EVIDENCE: ranked #1",
		"## Likely targets",
		"1. DoWithRetry  function  ·  retry.go:11-20",
		"2. Backoff  function  ·  retry.go:6-8",
		"^ callers: Fetch (client.go:4-6)",
		"v calls:   Backoff (retry.go:6-8)",
		"func DoWithRetry(max int) error", // full body for hot target
		"## Candidate files",
		"- retry.go  ·  Backoff, DoWithRetry",
		"— Coverage: 2 ranked candidate symbol(s) across 1 file(s)",
		"END OF LOCALIZATION — localization-only callers answer from this evidence; diagnosis and change callers proceed from it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n--- got ---\n%s", want, out)
		}
	}
	if strings.Index(out, "## Answer draft") >= strings.Index(out, "## Likely targets") {
		t.Fatalf("answer draft must precede detailed targets:\n%s", out)
	}
	for _, forbidden := range []string{"LOCALIZATION COMPLETE:", "answer now", "Do not make another localization", "REFINEMENT NEEDED", "read(operation:", "Do not call another tool"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("ordinary explore(task) must not claim terminality via %q:\n%s", forbidden, out)
		}
	}
}

func TestRenderExploreBroadWeakNeighborhoodReturnsBestSupportedDraft(t *testing.T) {
	targets := []exploreTarget{
		{node: &graph.Node{ID: "internal/embedding/nop.go::current", Name: "current", Kind: graph.KindVariable, FilePath: "internal/embedding/nop.go"}, score: 0.9},
		{node: &graph.Node{ID: "internal/analyzer/state.go::dirty", Name: "dirty", Kind: graph.KindVariable, FilePath: "internal/analyzer/state.go"}, score: 0.8},
	}
	task := "review release blockers across impact reach sqlite races mcp facade routing codex claude guidance explore ranking token cost"
	out := (&Server{}).renderExplore(task, targets, 1600)
	for _, want := range []string{"RANKED LOCALIZATION:", "with stated uncertainty", "## Answer draft", "Do not fan out or rerun broad exploration", "diagnosis and change callers continue from this evidence"} {
		if !strings.Contains(out, want) {
			t.Fatalf("weak neighborhood missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"LOCALIZATION COMPLETE:", "REFINEMENT NEEDED", "make one focused refinement", "rerun explore for", "search(operation:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("weak neighborhood must not invite broad refinement via %q:\n%s", forbidden, out)
		}
	}
}

func TestRenderExploreAnswerDraftIncludesQueryAlignedGraphNeighbor(t *testing.T) {
	head := &graph.Node{ID: "handler.go::HandleRequest", Name: "HandleRequest", Kind: graph.KindFunction, FilePath: "handler.go", StartLine: 10, EndLine: 18}
	retry := &graph.Node{ID: "retry/coordinator.go::RetryCoordinator", Name: "RetryCoordinator", Kind: graph.KindFunction, FilePath: "retry/coordinator.go", StartLine: 20, EndLine: 30}
	logger := &graph.Node{ID: "log.go::Logger", Name: "Logger", Kind: graph.KindFunction, FilePath: "log.go", StartLine: 5, EndLine: 8}
	out := (&Server{}).renderExplore(
		"locate retry coordinator implementation for transient failures",
		[]exploreTarget{{node: head, score: 1, callers: []*graph.Node{logger, retry}}},
		1600,
	)
	draftStart := strings.Index(out, "## Answer draft")
	detailsStart := strings.Index(out, "## Likely targets")
	if draftStart < 0 || detailsStart < 0 || draftStart >= detailsStart {
		t.Fatalf("invalid draft/detail ordering:\n%s", out)
	}
	draft := out[draftStart:detailsStart]
	for _, want := range []string{"SYMBOL: RetryCoordinator", "ID: retry/coordinator.go::RetryCoordinator", "retry/coordinator.go:20-30", "caller of ranked #1"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("query-aligned neighbor missing %q from answer draft:\n%s", want, draft)
		}
	}
	if strings.Contains(draft, "log.go::Logger") {
		t.Fatalf("unrelated graph neighbor must not be promoted into answer draft:\n%s", draft)
	}
}

func TestRenderExploreAnswerDraftPromotesBoundedStructuralNeighbors(t *testing.T) {
	node := func(id, name, file string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindMethod, FilePath: file, StartLine: 20}
	}
	genericHidden := node("crates/ignore/src/walk.rs::WalkBuilder.hidden", "hidden", "crates/ignore/src/walk.rs")
	standardFilters := node("crates/ignore/src/walk.rs::WalkBuilder.standard_filters", "standard_filters", "crates/ignore/src/walk.rs")
	isHidden := node("crates/ignore/src/walk.rs::Ignore.is_hidden", "is_hidden", "crates/ignore/src/walk.rs")
	matched := node("crates/ignore/src/dir.rs::Ignore.matched_dir_entry", "matched_dir_entry", "crates/ignore/src/dir.rs")
	testCaller := &graph.Node{ID: "crates/ignore/tests/gitignore_tests.rs::hidden_test", Name: "hidden_test", Kind: graph.KindFunction, FilePath: "crates/ignore/tests/gitignore_tests.rs"}
	callers := []*graph.Node{testCaller, node("crates/core/src/log.rs::trace_event", "trace_event", "crates/core/src/log.rs"), matched}
	targets := []exploreTarget{
		{node: genericHidden, callees: []*graph.Node{standardFilters}},
		{node: node("rank2", "ignore_rules", "crates/ignore/src/dir.rs")},
		{node: node("rank3", "walk_entries", "crates/ignore/src/walk.rs")},
		{node: node("rank4", "ignore", "crates/ignore/src/dir.rs")},
		{node: node("rank5", "hidden", "crates/ignore/src/dir.rs")},
		{node: node("rank6", "filter_entry", "crates/ignore/src/walk.rs")},
		{node: isHidden, callers: callers},
		{node: node("rank8", "walk_state", "crates/ignore/src/walk.rs")},
		{node: node("rank9", "path_filter", "crates/ignore/src/dir.rs")},
		{node: node("rank10", "ignore_match", "crates/ignore/src/dir.rs")},
	}
	out := (&Server{}).renderExplore("locate is_hidden scoped ignore behavior", targets, 1600)
	draft := out[strings.Index(out, "## Answer draft"):strings.Index(out, "## Likely targets")]
	for _, want := range []string{"SYMBOL: matched_dir_entry", "ID: crates/ignore/src/dir.rs::Ignore.matched_dir_entry", "caller of ranked #7"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("rank-7 structural caller missing %q from answer draft:\n%s", want, draft)
		}
	}
	for _, forbidden := range []string{"hidden_test", "trace_event", "standard_filters"} {
		if strings.Contains(draft, forbidden) {
			t.Fatalf("generic or ineligible caller %q was promoted:\n%s", forbidden, draft)
		}
	}

	buildParallel := node("crates/ignore/src/walk.rs::WalkBuilder.build_parallel", "build_parallel", "crates/ignore/src/walk.rs")
	buildWithCWD := node("crates/ignore/src/dir.rs::IgnoreBuilder.build_with_cwd", "build_with_cwd", "crates/ignore/src/dir.rs")
	getCurrentDir := node("crates/ignore/src/walk.rs::get_or_set_current_dir", "get_or_set_current_dir", "crates/ignore/src/walk.rs")
	build := node("crates/ignore/src/walk.rs::WalkBuilder.build", "build", "crates/ignore/src/walk.rs")
	targets = []exploreTarget{
		{node: node("parallel-rank1", "parallel_walk", "crates/ignore/src/walk.rs")},
		{node: buildParallel, callees: []*graph.Node{build, getCurrentDir, buildWithCWD}},
		{node: node("parallel-rank3", "multi_root", "crates/ignore/src/walk.rs")},
		{node: node("parallel-rank4", "current_dir", "crates/ignore/src/walk.rs")},
		{node: node("parallel-rank5", "standard_filters", "crates/ignore/src/walk.rs")},
		{node: node("parallel-rank6", "add_root", "crates/ignore/src/walk.rs")},
		{node: node("parallel-rank7", "ignore_rule", "crates/ignore/src/dir.rs")},
		{node: node("parallel-rank8", "walk_state", "crates/ignore/src/walk.rs")},
		{node: node("parallel-rank9", "root_path", "crates/ignore/src/dir.rs")},
		{node: node("parallel-rank10", "ignore_match", "crates/ignore/src/dir.rs")},
	}
	out = (&Server{}).renderExplore("Nondeterminism in ignore::WalkBuilder parallel multi-root walk", targets, 1600)
	draft = out[strings.Index(out, "## Answer draft"):strings.Index(out, "## Likely targets")]
	for _, want := range []string{"SYMBOL: build_with_cwd", "ID: crates/ignore/src/dir.rs::IgnoreBuilder.build_with_cwd", "callee of ranked #2"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("cross-file structural callee missing %q from answer draft:\n%s", want, draft)
		}
	}
	if strings.Contains(draft, "get_or_set_current_dir") {
		t.Fatalf("query-overlapping but parent-unrelated callee displaced structural implementation:\n%s", draft)
	}
}

func TestExploreDraftGenericCandidateIsLanguageNeutral(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		symbol string
		source string
	}{
		{name: "rust", path: "matcher.rs", symbol: "set_replacement", source: "fn set_replacement(&mut self, value: bool) { self.replacement = value; }"},
		{name: "go", path: "matcher.go", symbol: "SetReplacement", source: "func (m *Matcher) SetReplacement(value bool) { m.replacement = value }"},
		{name: "java", path: "Matcher.java", symbol: "setReplacement", source: "void setReplacement(boolean value) { this.replacement = value; }"},
		{name: "typescript", path: "matcher.ts", symbol: "setReplacement", source: "setReplacement(value: boolean) { this.replacement = value; }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &graph.Node{ID: tc.path + "::" + tc.symbol, Name: tc.symbol, Kind: graph.KindMethod, FilePath: tc.path}
			if !exploreDraftGenericCandidate(n, tc.source) {
				t.Fatalf("%s accessor was not classified structurally", tc.name)
			}
		})
	}
	concrete := &graph.Node{ID: "matcher.go::ApplyReplacement", Name: "ApplyReplacement", Kind: graph.KindMethod, FilePath: "matcher.go"}
	concreteSource := "func (m *Matcher) ApplyReplacement(parts []Part) { for _, part := range parts { m.output = append(m.output, part.Bytes()...) } }"
	if exploreDraftGenericCandidate(concrete, concreteSource) {
		t.Fatal("multi-step concrete implementation classified as generic")
	}
}

func TestExploreAnswerDraftExactTraitMethodBypassesGenericTieBreak(t *testing.T) {
	traitMethod := &graph.Node{ID: "matcher.rs::Matcher.replace", Name: "replace", QualName: "Matcher.replace", Kind: graph.KindMethod, FilePath: "matcher.rs"}
	implementation := &graph.Node{ID: "replacer.rs::Replacer.interpolate", Name: "interpolate", QualName: "Replacer.interpolate", Kind: graph.KindMethod, FilePath: "replacer.rs"}
	task := "How does Matcher.replace choose replacement bytes?"
	if rerank.ClassifyQuery(shapeExploreQuery(task)) != rerank.QueryClassConcept {
		t.Fatalf("fixture must exercise the Concept path: %q", shapeExploreQuery(task))
	}
	entries := exploreAnswerDraft(task, []exploreTarget{
		{node: implementation, source: "fn interpolate(&self, replacement: &[u8]) { for byte in replacement { self.output.push(*byte); } }"},
		{node: traitMethod, source: "trait Matcher { fn replace(&self, replacement: &[u8]) { self.replace_at(replacement); } }"},
	})
	if len(entries) == 0 || entries[0].node.ID != traitMethod.ID || !entries[0].exact || entries[0].generic {
		t.Fatalf("exact trait method lost its anchor exemption: %#v", entries)
	}
}

func TestExploreAnswerDraftPrefersConcreteRustImplementationOverTraitDefaults(t *testing.T) {
	node := func(id, name, qual string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Name: name, QualName: qual, Kind: kind, FilePath: "crates/grep-matcher/src/lib.rs", StartLine: 20}
	}
	matcher := node("matcher::Matcher", "Matcher", "Matcher", graph.KindType)
	defaultReplace := node("matcher::Matcher.replace", "replace", "Matcher.replace", graph.KindMethod)
	setter := node("matcher::Matcher.set_replacement", "set_replacement", "Matcher.set_replacement", graph.KindMethod)
	replacer := node("matcher::Replacer", "Replacer", "Replacer", graph.KindType)
	targets := []exploreTarget{
		{node: matcher, source: "pub trait Matcher { fn replace(&self, bytes: &[u8]); }"},
		{node: defaultReplace, source: "fn replace(&self, bytes: &[u8]) { self.replace_with_captures(bytes) }"},
		{node: setter, source: "fn set_replacement(&mut self, yes: bool) -> &mut Self { self.replacement = yes; self }"},
		{node: replacer, source: "pub struct Replacer; impl Replacer { fn interpolate(&self, captures: Captures, replacement: &[u8]) { for capture in captures.iter() { output.extend_from_slice(capture.bytes()); } } }"},
	}

	conceptTask := "How does replacement byte capture interpolation produce incorrect output?"
	if rerank.ClassifyQuery(shapeExploreQuery(conceptTask)) != rerank.QueryClassConcept {
		t.Fatalf("fixture must exercise the Concept path: %q", shapeExploreQuery(conceptTask))
	}
	entries := exploreAnswerDraft(conceptTask, targets)
	if len(entries) == 0 || entries[0].node.ID != replacer.ID {
		t.Fatalf("concrete replacement implementation must precede trait defaults: %#v", entries)
	}
	for _, entry := range entries {
		if entry.node.ID == replacer.ID && entry.generic {
			t.Fatalf("concrete implementation classified as generic: %#v", entry)
		}
	}

	exact := exploreAnswerDraft("Matcher", targets)
	if len(exact) == 0 || exact[0].node.ID != matcher.ID {
		t.Fatalf("exact trait lookup must retain anchor order: %#v", exact)
	}
}

func TestExploreAnswerDraftReservesOneCrossFileCausalNeighborWithinBudget(t *testing.T) {
	scope := query.QueryOptions{
		WorkspaceID: "bench", ProjectID: "ripgrep", RepoAllow: map[string]bool{"ripgrep": true},
	}
	node := func(id, name, qual, file string) *graph.Node {
		return &graph.Node{
			ID: id, Name: name, QualName: qual, Kind: graph.KindMethod,
			FilePath: file, Language: "rust", StartLine: 1, EndLine: 1,
			WorkspaceID: "bench", ProjectID: "ripgrep", RepoPrefix: "ripgrep",
		}
	}
	const causalBody = "fn build_with_cwd(&self) {\n    let ignore = self.parents_for_each_root();\n    record(\"CROSS_FILE_CAUSAL_BODY_MARKER\", ignore);\n}\n"
	causalPath := t.TempDir() + "/dir.rs"
	if err := os.WriteFile(causalPath, []byte(causalBody), 0o600); err != nil {
		t.Fatal(err)
	}
	buildParallel := node("walk::build_parallel", "build_parallel", "WalkBuilder.build_parallel", "crates/ignore/src/walk.rs")
	buildWithCWD := node("dir::build_with_cwd", "build_with_cwd", "IgnoreBuilder.build_with_cwd", causalPath)
	buildWithCWD.EndLine = 4
	sameFileBuild := node("walk::build", "build", "WalkBuilder.build", "crates/ignore/src/walk.rs")
	genericGetter := node("walk::get_current_dir", "get_current_dir", "WalkBuilder.get_current_dir", "crates/ignore/src/walk.rs")
	genericCaller := node("walk::set_parallel", "set_parallel", "WalkBuilder.set_parallel", "crates/ignore/src/walk.rs")
	targets := []exploreTarget{
		{node: buildParallel, callers: []*graph.Node{genericCaller}, callees: []*graph.Node{sameFileBuild, genericGetter, buildWithCWD}, source: "fn build_parallel(&self) { self.ignore.build_with_cwd(&self.cwd); }"},
		{node: node("walk::parallel", "parallel_walk", "WalkBuilder.parallel_walk", "crates/ignore/src/walk.rs")},
		{node: node("walk::roots", "multiple_roots", "WalkBuilder.multiple_roots", "crates/ignore/src/walk.rs")},
		{node: node("walk::filters", "standard_filters", "WalkBuilder.standard_filters", "crates/ignore/src/walk.rs")},
	}
	task := "Nondeterminism in ignore::WalkBuilder parallel multi-root walk\n" +
		"ignore::WalkBuilder appears to produce nondeterministic results for a parallel multi-root walk when one root has a scoped ignore rule that should not apply to another root.\n" +
		"The racy result suggests the per-directory ignore stack is inherited across roots."
	if strings.Contains(task, "build_with_cwd") {
		t.Fatal("fixture must not name the expected callee")
	}
	for _, target := range targets {
		if target.node != nil && target.node.ID == buildWithCWD.ID {
			t.Fatal("graph-only promoted neighbor must be absent from direct targets")
		}
	}
	if rerank.ClassifyQuery(shapeExploreQuery(task)) != rerank.QueryClassConcept {
		t.Fatalf("fixture must exercise the Concept path: %q", shapeExploreQuery(task))
	}
	entries := exploreAnswerDraft(task, targets)
	structural := 0
	for _, entry := range entries {
		if entry.structural {
			structural++
			if entry.node.ID != buildWithCWD.ID {
				t.Fatalf("reserved structural slot = %q, want cross-file causal callee %q: %#v", entry.node.ID, buildWithCWD.ID, entries)
			}
		}
	}
	if structural != 1 {
		t.Fatalf("structural quota = %d, want exactly 1: %#v", structural, entries)
	}
	if len(entries) > exploreDraftTotalLimit {
		t.Fatalf("draft exceeded bounded cardinality: %d", len(entries))
	}
	shuffledTargets := append([]exploreTarget(nil), targets...)
	shuffledTargets[0].callers = []*graph.Node{genericCaller}
	shuffledTargets[0].callees = []*graph.Node{genericGetter, buildWithCWD, sameFileBuild}
	shuffled := exploreAnswerDraft(task, shuffledTargets)
	if len(shuffled) != len(entries) {
		t.Fatalf("shuffled draft size = %d, want %d", len(shuffled), len(entries))
	}
	for i := range entries {
		if shuffled[i].node.ID != entries[i].node.ID {
			t.Fatalf("neighbor input order changed draft at %d: got %q want %q", i, shuffled[i].node.ID, entries[i].node.ID)
		}
	}

	server := &Server{}
	reads := 0
	materialized := materializeExploreStructuralSourceWithReader(
		context.Background(), task, targets, scope,
		func(ctx context.Context, n *graph.Node) string {
			reads++
			return server.manifestSymbolSource(ctx, n)
		},
	)
	if reads != 1 {
		t.Fatalf("promoted source reads = %d, want exactly one", reads)
	}
	if len(materialized) != len(targets)+1 || materialized[len(materialized)-1].node.ID != buildWithCWD.ID {
		t.Fatalf("materialized targets = %#v, want one appended graph-only boundary", materialized)
	}
	if !strings.Contains(materialized[len(materialized)-1].source, "CROSS_FILE_CAUSAL_BODY_MARKER") {
		t.Fatalf("promoted source was not read from its graph node range: %q", materialized[len(materialized)-1].source)
	}
	rendered := server.renderExplore(task, materialized, 1600)
	if !strings.Contains(rendered, "CROSS_FILE_CAUSAL_BODY_MARKER") {
		t.Fatalf("promoted cross-file boundary missing reserved rendered body:\n%s", rendered)
	}

	const budget = 1000
	result := newLocalizationExploreResultForTask(newLocalizationCompletion(true, ""), task, materialized, budget)
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected one text result: %#v", result)
	}
	if len(text) > budget*localizationEnvelopeBytesPerToken {
		t.Fatalf("serialized envelope = %d bytes, budget = %d", len(text), budget*localizationEnvelopeBytesPerToken)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(envelope.Symbols, "\n"), buildWithCWD.ID) {
		t.Fatalf("cross-file causal boundary missing from bounded envelope: %#v", envelope.Symbols)
	}
	foundSource := false
	for _, evidence := range envelope.Evidence {
		if evidence.ID == buildWithCWD.ID && strings.Contains(evidence.Source, "CROSS_FILE_CAUSAL_BODY_MARKER") {
			foundSource = true
			break
		}
	}
	if !foundSource {
		t.Fatalf("promoted cross-file boundary did not receive its reserved source slot: %#v", envelope.Evidence)
	}
}

func TestExploreStructuralSourceMaterializationHonorsReadBoundary(t *testing.T) {
	const baseTask = "Nondeterminism in ignore::WalkBuilder parallel multi-root walk\n" +
		"ignore::WalkBuilder appears to produce nondeterministic results for a parallel multi-root walk when one root has a scoped ignore rule that should not apply to another root.\n" +
		"The racy result suggests the per-directory ignore stack is inherited across roots."
	scope := query.QueryOptions{
		WorkspaceID: "bench", ProjectID: "ripgrep", RepoAllow: map[string]bool{"ripgrep": true},
	}
	makeTargets := func(neighborProject, neighborRepo string) ([]exploreTarget, *graph.Node) {
		node := func(id, name, qual, file string) *graph.Node {
			return &graph.Node{
				ID: id, Name: name, QualName: qual, Kind: graph.KindMethod,
				FilePath: file, Language: "rust", StartLine: 1, EndLine: 4,
				WorkspaceID: "bench", ProjectID: "ripgrep", RepoPrefix: "ripgrep",
			}
		}
		neighbor := node("dir::build_with_cwd", "build_with_cwd", "IgnoreBuilder.build_with_cwd", "crates/ignore/src/dir.rs")
		neighbor.ProjectID = neighborProject
		neighbor.RepoPrefix = neighborRepo
		primary := node("walk::build_parallel", "build_parallel", "WalkBuilder.build_parallel", "crates/ignore/src/walk.rs")
		return []exploreTarget{
			{node: primary, callees: []*graph.Node{neighbor}, source: "fn build_parallel(&self) { self.ignore.build_with_cwd(&self.cwd); }"},
			{node: node("walk::parallel", "parallel_walk", "WalkBuilder.parallel_walk", "crates/ignore/src/walk.rs")},
			{node: node("walk::roots", "multiple_roots", "WalkBuilder.multiple_roots", "crates/ignore/src/walk.rs")},
			{node: node("walk::filters", "standard_filters", "WalkBuilder.standard_filters", "crates/ignore/src/walk.rs")},
		}, neighbor
	}
	assertPromoted := func(task string, targets []exploreTarget, wantID string) {
		t.Helper()
		for _, entry := range exploreAnswerDraft(task, targets) {
			if entry.structural && entry.node != nil && entry.node.ID == wantID {
				return
			}
		}
		t.Fatalf("fixture did not promote %q before source guard", wantID)
	}
	assertNoRead := func(name string, ctx context.Context, task string, targets []exploreTarget, scope query.QueryOptions) {
		t.Helper()
		reads := 0
		got := materializeExploreStructuralSourceWithReader(
			ctx, task, targets, scope,
			func(context.Context, *graph.Node) string {
				reads++
				return "SHOULD_NOT_BE_READ"
			},
		)
		if reads != 0 {
			t.Fatalf("%s: source reads = %d, want zero", name, reads)
		}
		if len(got) != len(targets) {
			t.Fatalf("%s: materialized %d targets, want unchanged %d", name, len(got), len(targets))
		}
	}

	for _, explicit := range []string{
		"build_parallel",
		"WalkBuilder.build_parallel",
		"fn build_parallel(&self) -> WalkParallel",
	} {
		if exploreAllowsStructuralBody(explicit) {
			t.Errorf("explicit query %q must remain direct-only", explicit)
		}
	}

	pathTargets, pathNeighbor := makeTargets("ripgrep", "ripgrep")
	pathTask := baseTask + "\nInvestigate crates/ignore/src/walk.rs without letting one root inherit another root's scoped rules."
	if rerank.ClassifyQuery(shapeExploreQuery(pathTask)) != rerank.QueryClassConcept {
		t.Fatalf("path fixture must remain prose Concept: %q", shapeExploreQuery(pathTask))
	}
	assertPromoted(pathTask, pathTargets, pathNeighbor.ID)
	assertNoRead("directory-qualified Concept prose", context.Background(), pathTask, pathTargets, scope)

	cancelTargets, cancelNeighbor := makeTargets("ripgrep", "ripgrep")
	assertPromoted(baseTask, cancelTargets, cancelNeighbor.ID)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	assertNoRead("canceled context", canceled, baseTask, cancelTargets, scope)
	if got := (&Server{}).materializeExploreStructuralSource(canceled, baseTask, cancelTargets, scope); len(got) != len(cancelTargets) {
		t.Fatalf("production wrapper materialized source after cancellation: got %d targets, want %d", len(got), len(cancelTargets))
	}

	projectTargets, projectNeighbor := makeTargets("other-project", "ripgrep")
	assertPromoted(baseTask, projectTargets, projectNeighbor.ID)
	assertNoRead("cross-project neighbor", context.Background(), baseTask, projectTargets, scope)

	repoTargets, repoNeighbor := makeTargets("ripgrep", "other-repo")
	assertPromoted(baseTask, repoTargets, repoNeighbor.ID)
	assertNoRead("cross-repository neighbor", context.Background(), baseTask, repoTargets, scope)
}

func TestExploreAnswerDraftCapsAndDeduplicates(t *testing.T) {
	node := func(id, name string, line int) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, FilePath: id + ".go", StartLine: line}
	}
	deep := node("deep", "DeepTarget", 40)
	targets := []exploreTarget{
		{node: node("alpha", "Alpha", 10), callers: []*graph.Node{deep}},
		{node: node("beta", "Beta", 20)},
		{node: node("gamma", "Gamma", 30)},
		{node: deep},
		{node: node("epsilon", "Epsilon", 50)},
		{node: node("zeta", "Zeta", 60)},
	}
	entries := exploreAnswerDraft("locate DeepTarget implementation", targets)
	if len(entries) != exploreDraftTotalLimit {
		t.Fatalf("draft size = %d, want %d: %#v", len(entries), exploreDraftTotalLimit, entries)
	}
	if entries[0].node.ID != "deep" {
		t.Fatalf("exact query-aligned target must lead the draft, got %q", entries[0].node.ID)
	}
	seen := map[string]int{}
	for _, entry := range entries {
		seen[exploreDraftNodeKey(entry.node)]++
	}
	if seen["deep"] != 1 {
		t.Fatalf("aligned node must be promoted exactly once, got %d: %#v", seen["deep"], entries)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("draft node %q appears %d times", id, count)
		}
	}
}

func TestExploreDirectoryQualifiedDraftAndBodiesRejectSameBasename(t *testing.T) {
	node := func(id, name, path, source string) exploreTarget {
		return exploreTarget{node: &graph.Node{ID: id, Name: name, QualName: name, Kind: graph.KindFunction, FilePath: path, Language: "go", StartLine: 10}, source: source}
	}
	wrong := node("pkg/wrong/config.go::Load", "Load", "pkg/wrong/config.go", "func Load() string {\n\treturn \"WRONG_FULL_BODY_MARKER\"\n}")
	rightLoad := node("pkg/right/config.go::Load", "Load", "pkg/right/config.go", "func Load() string {\n\treturn \"RIGHT_LOAD_BODY_MARKER\"\n}")
	rightSave := node("pkg/right/config.go::Save", "Save", "pkg/right/config.go", "func Save() string {\n\treturn \"RIGHT_SAVE_BODY_MARKER\"\n}")
	targets := []exploreTarget{wrong, rightLoad, rightSave}
	entries := exploreAnswerDraft("pkg/right/config.go", targets)
	if len(entries) < 3 || entries[0].node.ID != rightLoad.node.ID || entries[1].node.ID != rightSave.node.ID {
		t.Fatalf("strict path anchors did not preserve same-file retrieval order: %#v", entries)
	}
	if entries[0].exact != true || entries[1].exact != true || entries[2].exact {
		t.Fatalf("same-basename path leaked into exact draft anchors: %#v", entries)
	}
	preferred := explorePreferredFullBodyIDs("pkg/right/config.go", targets, entries, exploreFullBodyLimit)
	if len(preferred) != 2 || preferred[0] != exploreDraftNodeKey(rightLoad.node) || preferred[1] != exploreDraftNodeKey(rightSave.node) {
		t.Fatalf("explicit path full-body order = %#v", preferred)
	}
	out := (&Server{}).renderExplore("pkg/right/config.go", targets, 1600)
	for _, marker := range []string{"RIGHT_LOAD_BODY_MARKER", "RIGHT_SAVE_BODY_MARKER"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("exact same-file body %q missing:\n%s", marker, out)
		}
	}
	if strings.Contains(out, "WRONG_FULL_BODY_MARKER") {
		t.Fatalf("same-basename body was promoted:\n%s", out)
	}
}

func TestExploreDraftExactAnchorUsesTokenBoundaries(t *testing.T) {
	n := &graph.Node{Name: "Get", QualName: "Client.Get", FilePath: "runtime/get.go"}
	if exploreDraftExactAnchor("target offset runtime user backend", n) {
		t.Fatal("short identifier must not match inside unrelated words")
	}
	if !exploreDraftExactAnchor("trace Client.Get behavior", n) {
		t.Fatal("qualified identifier token sequence should be an exact anchor")
	}
	if !exploreDraftExactAnchor("inspect get.go", n) {
		t.Fatal("path basename should be an exact anchor")
	}
	generic := &graph.Node{Name: "write", QualName: "Writer.write", FilePath: "runtime/output.go"}
	if exploreDraftExactAnchor("fix write behavior", generic) {
		t.Fatal("unqualified generic method must not become an exact anchor")
	}
	if !exploreDraftExactAnchor("fix Writer.write behavior", generic) {
		t.Fatal("qualified generic method should remain an exact anchor")
	}
	plainGeneric := &graph.Node{Name: "write", QualName: "write", FilePath: "runtime/output.go"}
	if exploreDraftExactAnchor("fix write behavior", plainGeneric) {
		t.Fatal("duplicated unqualified qualname must not bypass the generic-name guard")
	}
	convert := &graph.Node{Name: "Convert", QualName: "Convert", FilePath: "runtime/value.cs"}
	if exploreDraftExactAnchor("review Convert behavior", convert) {
		t.Fatal("unqualified cross-language generic method must not become an exact anchor")
	}
	convert.QualName = "Thing.Convert"
	if !exploreDraftExactAnchor("review Thing.Convert behavior", convert) {
		t.Fatal("qualified cross-language generic method should remain an exact anchor")
	}
}

func TestExploreBroadWellAlignedNeighborhoodGetsExplicitAnswerReadyContract(t *testing.T) {
	targets := []exploreTarget{{
		node:                  &graph.Node{ID: "internal/mcp/facade_tools.go::registerFacadeTools", Name: "registerFacadeTools", Kind: graph.KindMethod, FilePath: "internal/mcp/facade_tools.go"},
		score:                 1.2,
		conceptImplementation: true,
		source:                "func registerFacadeTools() { registerRoutingSchema() }",
	}}
	task := "investigate mcp facade tool routing schema registration operation dispatch surface architecture integration behavior"
	if !exploreAnswerReady(task, targets) {
		t.Fatal("well-aligned hydrated implementation should produce answer_ready for explore(localize)")
	}
	if !exploreSingleQualifiedImplementation(task, targets) {
		t.Fatal("one hydrated implementation above the confidence threshold should be recognized as the only qualified result")
	}
	localized := exploreLocalizationCompletion(true, false, task, targets, "")
	if localized.State != localizationStateLocalized || localized.RequiredAction != "continue_task" {
		t.Fatalf("single qualified implementation should produce a non-terminal localized cue: %#v", localized)
	}
	if contract := localizationContractFor(localized); contract.Terminal || contract.Completion.Enforceable {
		t.Fatalf("single-result cue must not create a terminal contract: %#v", contract)
	}
	if artifact := exploreLocalizationCompletion(true, true, task, targets, ""); artifact.State != localizationStateAnswerReady {
		t.Fatalf("artifact terminality must retain its existing contract: %#v", artifact)
	}
	competitor := exploreTarget{
		node: &graph.Node{
			ID:       "internal/mcp/facade_tools.go::dispatchFacadeRoutingSchema",
			Name:     "dispatchFacadeRoutingSchema",
			Kind:     graph.KindFunction,
			FilePath: "internal/mcp/facade_tools.go",
		},
		source: "func dispatchFacadeRoutingSchema() { dispatchOperation() }",
	}
	multiple := append(targets, competitor)
	if exploreSingleQualifiedImplementation(task, multiple) {
		t.Fatal("a second implementation above the same confidence threshold must prevent the single-result cue")
	}
	if completion := exploreLocalizationCompletion(true, false, task, multiple, ""); completion.State != localizationStateAnswerReady {
		t.Fatalf("multiple qualified implementations must retain the existing completion path: %#v", completion)
	}
	metadataOnly := targets[0]
	metadataOnly.conceptImplementation = false
	metadataOnly.source = ""
	if exploreAnswerReady(task, []exploreTarget{metadataOnly}) {
		t.Fatal("metadata alignment without a hydrated implementation must remain nonterminal")
	}
	out := (&Server{}).renderExplore(task, targets, 1600)
	if !strings.Contains(out, "RANKED LOCALIZATION:") || strings.Contains(out, "LOCALIZATION COMPLETE:") {
		t.Fatalf("ordinary explore(task) rendering must stay non-terminal:\n%s", out)
	}
	for _, forbidden := range []string{"exact-ID source read", "REFINEMENT", "rerun explore", "read(operation:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("well-aligned neighborhood must not invite another call via %q:\n%s", forbidden, out)
		}
	}
}

func TestRenderExplorePacksStrongestDraftBody(t *testing.T) {
	targets := []exploreTarget{
		{node: &graph.Node{ID: "generic.go::Current", Name: "Current", Kind: graph.KindFunction, FilePath: "generic.go", StartLine: 1}, source: "func Current() {\n\tgenericOne()\n}"},
		{node: &graph.Node{ID: "generic.go::State", Name: "State", Kind: graph.KindFunction, FilePath: "generic.go", StartLine: 10}, source: "func State() {\n\tgenericTwo()\n}"},
		{node: &graph.Node{ID: "retry.go::DeepTarget", Name: "DeepTarget", Kind: graph.KindFunction, FilePath: "retry.go", StartLine: 20}, source: "func DeepTarget() {\n\tdecisiveBody()\n}"},
	}
	out := (&Server{}).renderExplore("locate DeepTarget implementation", targets, 9000)
	draftStart := strings.Index(out, "## Answer draft")
	if draftStart < 0 {
		t.Fatalf("missing answer draft:\n%s", out)
	}
	if first := strings.Index(out[draftStart:], "SYMBOL:"); first < 0 || !strings.HasPrefix(out[draftStart+first:], "SYMBOL: DeepTarget") {
		t.Fatalf("query-aligned target must lead the answer draft:\n%s", out)
	}
	if !strings.Contains(out, "decisiveBody()") {
		t.Fatalf("query-aligned direct target must receive a full source body:\n%s", out)
	}
}

func TestRenderExploreDefaultBudgetRetainsTopBody(t *testing.T) {
	targets := exploreTestTargets()
	for i := 0; i < 4; i++ {
		targets = append(targets, exploreTarget{node: &graph.Node{
			ID:        fmt.Sprintf("internal/retry/candidate_%d.go::Candidate%d", i, i),
			Name:      fmt.Sprintf("Candidate%d", i),
			Kind:      graph.KindFunction,
			FilePath:  fmt.Sprintf("internal/retry/candidate_%d.go", i),
			StartLine: 10 + i,
		}})
	}
	out := (&Server{}).renderExplore("the retry backoff never fires on 429", targets, exploreDefaultBudgetTokens)
	if !strings.Contains(out, "func DoWithRetry(max int) error") {
		t.Fatalf("compact answer draft must not displace the top source body at the default budget:\n%s", out)
	}
	draftStart := strings.Index(out, "## Answer draft")
	detailsStart := strings.Index(out, "## Likely targets")
	if draftStart < 0 || detailsStart < 0 {
		t.Fatalf("missing draft/detail sections:\n%s", out)
	}
	if rows := strings.Count(out[draftStart:detailsStart], "- FILE:"); rows > exploreDraftTotalLimit {
		t.Fatalf("answer draft has %d rows, limit %d:\n%s", rows, exploreDraftTotalLimit, out[draftStart:detailsStart])
	}
}

func TestRenderExploreBudgetTruncation(t *testing.T) {
	// A budget below the body cost forces demotion to signatures, but every
	// candidate's LOCATION must still be listed (file-hit/symbol-hit never
	// depend on the budget) and the truncation must be reported honestly.
	out := (&Server{}).renderExplore("task", exploreTestTargets(), exploreMinBudgetTokens)
	for _, want := range []string{
		"1. DoWithRetry  function  ·  retry.go:11-20",
		"2. Backoff  function  ·  retry.go:6-8",
		"— Coverage:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("truncated render missing %q", want)
		}
	}
}

func TestRenderExploreLimitsFullBodiesToTopTargets(t *testing.T) {
	// One target past the full-body limit, each with an unmistakable body
	// marker: bodies must flow to every slot inside the limit and stop at the
	// first one past it, whatever the limit is tuned to.
	var targets []exploreTarget
	for index := 1; index <= exploreFullBodyLimit+1; index++ {
		name := fmt.Sprintf("Cand%02d", index)
		targets = append(targets, exploreTarget{
			node: &graph.Node{
				Name: name, Kind: graph.KindFunction,
				FilePath:  fmt.Sprintf("cand_%02d.go", index),
				StartLine: 1, EndLine: 4, Language: "go",
			},
			source: fmt.Sprintf("func %s() int {\n\treturn 9900 + %d\n}", name, index),
		})
	}
	out := (&Server{}).renderExplore("task", targets, exploreDefaultBudgetTokens)
	if exploreDefaultBudgetTokens != 1600 {
		t.Fatalf("default explore budget=%d want 1600", exploreDefaultBudgetTokens)
	}
	if !strings.Contains(out, fmt.Sprintf("%d. Cand%02d  function", exploreFullBodyLimit+1, exploreFullBodyLimit+1)) {
		t.Fatal("candidate header past the body limit was dropped")
	}
	if !strings.Contains(out, fmt.Sprintf("return 9900 + %d", exploreFullBodyLimit)) {
		t.Fatalf("candidate %d inside the body limit lost its full body", exploreFullBodyLimit)
	}
	if strings.Contains(out, fmt.Sprintf("return 9900 + %d", exploreFullBodyLimit+1)) {
		t.Fatal("candidate past the body limit unexpectedly retained a full body")
	}
}

func TestExploreHelpers(t *testing.T) {
	if got := clampInt(5, 1, 3); got != 3 {
		t.Errorf("clampInt hi: got %d", got)
	}
	if got := clampInt(0, 2, 10); got != 2 {
		t.Errorf("clampInt lo: got %d", got)
	}
	if got := truncateOneLine("a\n b\tc", 100); got != "a b c" {
		t.Errorf("truncateOneLine collapse: %q", got)
	}
	if got := truncateOneLine(strings.Repeat("x", 10), 4); got != "xxxx…" {
		t.Errorf("truncateOneLine cap: %q", got)
	}
	if got := firstLines("1\n2\n3\n4", 2); got != "1\n2" {
		t.Errorf("firstLines: %q", got)
	}
	if got := dedupStrings([]string{"b", "a", "b"}); strings.Join(got, ",") != "a,b" {
		t.Errorf("dedupStrings: %v", got)
	}
	got := exploreConceptRecallTerms("Find the code responsible for case insensitive alternation literal prefixes")
	joined := strings.Join(got, ",")
	for _, want := range []string{"case", "insensitive", "alternation", "literal", "prefix"} {
		if !strings.Contains(joined, want) {
			t.Errorf("concept recall terms missing %q: %v", want, got)
		}
	}
	for _, noise := range []string{"find", "code"} {
		if strings.Contains(joined, noise) {
			t.Errorf("concept recall terms retained generic %q: %v", noise, got)
		}
	}
	if hasExploreExpansionTerms("case insensitive alternation", []string{"case", "alternation"}) {
		t.Fatal("subset concept bag should not trigger duplicate retrieval")
	}
	if hasExploreExpansionTerms("case insensitive alternations", []string{"case", "alternation"}) {
		t.Fatal("FTS-equivalent concept root should not trigger duplicate retrieval")
	}
	if !hasExploreExpansionTerms("case insensitive alternation", []string{"case", "prefix"}) {
		t.Fatal("genuinely new concept term should trigger expansion retrieval")
	}

	semantic := &rerank.Candidate{Node: &graph.Node{ID: "semantic"}, TextRank: -1, VectorRank: 0}
	textDuplicate := &rerank.Candidate{Node: &graph.Node{ID: "semantic"}, TextRank: 2, VectorRank: -1}
	textOnly := &rerank.Candidate{Node: &graph.Node{ID: "text"}, TextRank: 0, VectorRank: -1}
	merged := mergeExploreCandidates([]*rerank.Candidate{semantic}, []*rerank.Candidate{textDuplicate, textOnly}, 80)
	if len(merged) != 2 || merged[0].Node.ID != "semantic" || merged[0].VectorRank != 0 || merged[0].TextRank != 82 {
		t.Fatalf("candidate merge lost hybrid ranks or expansion provenance: %#v", merged)
	}
	if semantic.TextRank != -1 {
		t.Fatalf("candidate merge mutated its primary input: %#v", semantic)
	}

	primaryText := &rerank.Candidate{Node: &graph.Node{ID: "primary"}, TextRank: 0, VectorRank: -1}
	primaryMerged := mergeExploreCandidates([]*rerank.Candidate{primaryText}, []*rerank.Candidate{{Node: primaryText.Node, TextRank: 0, VectorRank: -1}}, 80)
	if primaryMerged[0].TextRank != 0 {
		t.Fatalf("expansion rank displaced primary query rank: %#v", primaryMerged[0])
	}

	window := make([]*rerank.Candidate, 0, 11)
	for i := 0; i < 10; i++ {
		window = append(window, &rerank.Candidate{Node: &graph.Node{ID: fmt.Sprintf("text-%d", i)}, TextRank: i, VectorRank: -1})
	}
	window = append(window, &rerank.Candidate{Node: &graph.Node{ID: "vector-only"}, TextRank: -1, VectorRank: 0})
	bounded := limitExploreCandidates(window, 5)
	foundVector := false
	for _, candidate := range bounded {
		foundVector = foundVector || candidate.Node.ID == "vector-only"
	}
	if !foundVector {
		t.Fatalf("bounded candidate union dropped top vector-only evidence: %#v", bounded)
	}
}

// TestFacadeExploreDemotesRepeatedDataLeafNames reproduces the reported
// localization failure through the public facade: many unrelated declarations
// named `client` used to consume the whole result head, excluding the callable
// definition that explains the area. One best data-leaf match may remain, but
// repeated same-name leaves must not crowd out a differently-named code target.
func TestFacadeExploreDemotesRepeatedDataLeafNames(t *testing.T) {
	g := graph.New()
	// Retrieval ordering, stated rather than ranked: every query in this
	// fixture retrieves the 30 short exact-name `client` declarations
	// first and the differently-named callable last. That is the shape a
	// ranker produces for this task -- the literal name matches win the
	// head, the callable that actually explains the area matches on prose
	// and lands behind them -- and it is the shape explore's over-fetch
	// window must retain so name diversification can promote it.
	ob := newOrderedBackend()
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("pkg/service%d.go::client", i)
		n := &graph.Node{
			ID: id, Name: "client", Kind: graph.KindVariable,
			FilePath: fmt.Sprintf("pkg/service%d.go", i), Language: "go",
			Meta: map[string]any{"signature": "var client *Transport"},
		}
		g.AddNode(n)
		ob.putDefault(id)
	}
	relevant := &graph.Node{
		ID: "pkg/coordinator.go::TransportCoordinator", Name: "TransportCoordinator",
		Kind: graph.KindFunction, FilePath: "pkg/coordinator.go", Language: "go",
		Meta: map[string]any{"signature": "func TransportCoordinator()"},
	}
	g.AddNode(relevant)
	ob.putDefault(relevant.ID)

	eng := query.NewEngine(g)
	eng.SetSearch(ob)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	task := "trace how the client is coordinated"
	searchQuery := shapeExploreQuery(task)
	const maxSymbols = 6
	queryClass := rerank.ClassifyQuery(searchQuery)
	raw := eng.SearchSymbolsRanked(searchQuery, exploreCandidateFetchLimit(maxSymbols, queryClass), query.QueryOptions{}, srv.buildRerankContext(context.Background(), searchQuery))
	rawHead := make([]string, 0, 6)
	relevantRetrieved := false
	for _, candidate := range raw {
		if candidate != nil && candidate.Node != nil && candidate.Node.ID == relevant.ID {
			relevantRetrieved = true
		}
		if len(rawHead) == 6 {
			continue
		}
		if candidate == nil || candidate.Node == nil || !exploreLocalizableKind(candidate.Node.Kind) || !exploreCodeDefinitionKind(candidate.Node.Kind) {
			continue
		}
		rawHead = append(rawHead, candidate.Node.ID)
	}
	if len(rawHead) != 6 {
		t.Fatalf("fixture produced only %d raw localization candidates: %v", len(rawHead), rawHead)
	}
	if !relevantRetrieved {
		rawIDs := make([]string, 0, len(raw))
		for _, candidate := range raw {
			if candidate != nil && candidate.Node != nil {
				rawIDs = append(rawIDs, candidate.Node.ID)
			}
		}
		t.Fatalf("fixture's relevant callable fell outside the concept-query over-fetch window: query=%q raw=%v", searchQuery, rawIDs)
	}
	for _, id := range rawHead {
		if id == relevant.ID {
			t.Fatalf("fixture no longer reproduces crowd-out before explore diversification: %v", rawHead)
		}
	}
	req := mcpgo.CallToolRequest{}
	// Omit operation deliberately: the facade default must route to task.
	req.Params.Arguments = map[string]any{
		"task": task, "options": map[string]any{"max_symbols": maxSymbols},
	}
	result, err := srv.handleFacade(context.Background(), "explore", req)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("explore failed: %#v", result)
	}
	text, ok := result.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("unexpected explore result content: %#v", result.Content[0])
	}
	out := text.Text
	if strings.Contains(out, "get_symbol_source") || strings.Contains(out, "batch_symbols") || strings.Contains(out, "search_text") || strings.Contains(out, "find_files") {
		t.Fatalf("explore emitted unavailable legacy follow-up guidance:\n%s", out)
	}
	relevantAt := strings.Index(out, "TransportCoordinator")
	if relevantAt < 0 {
		t.Fatalf("callable target was crowded out by repeated data leaves:\n%s", out)
	}
	firstClient := strings.Index(out, ". client  variable")
	if firstClient < 0 {
		t.Fatalf("fixture did not surface its best literal data-leaf match:\n%s", out)
	}
	secondClient := strings.Index(out[firstClient+1:], ". client  variable")
	if secondClient >= 0 && firstClient+1+secondClient < relevantAt {
		t.Fatalf("repeated generic data leaves still rank ahead of the callable target:\n%s", out)
	}
}

func TestExploreFileDiversificationIsStrictlyConceptOnly(t *testing.T) {
	if !exploreShouldDiversifyByFile(rerank.QueryClassConcept) {
		t.Fatal("Concept query must retain bounded per-file diversification")
	}
	for _, class := range []rerank.QueryClass{rerank.QueryClassSymbol, rerank.QueryClassSignature} {
		if exploreShouldDiversifyByFile(class) {
			t.Fatalf("explicit query class %v must preserve retrieval order", class)
		}
	}
}

func TestDiversifyRepeatedExploreNamesIsConceptOnlyAndStable(t *testing.T) {
	leaf1 := &rerank.Candidate{Node: &graph.Node{ID: "a", Name: "client", Kind: graph.KindVariable}}
	leaf2 := &rerank.Candidate{Node: &graph.Node{ID: "b", Name: "client", Kind: graph.KindField}}
	validate1 := &rerank.Candidate{Node: &graph.Node{ID: "v1", Name: "Validate", Kind: graph.KindMethod}}
	validate2 := &rerank.Candidate{Node: &graph.Node{ID: "v2", Name: "Validate", Kind: graph.KindMethod}}
	validate3 := &rerank.Candidate{Node: &graph.Node{ID: "v3", Name: "Validate", Kind: graph.KindFunction}}
	callable := &rerank.Candidate{Node: &graph.Node{ID: "c", Name: "FacadeRegistry", Kind: graph.KindFunction}}
	input := []*rerank.Candidate{leaf1, leaf2, validate1, validate2, validate3, callable}

	concept := diversifyRepeatedExploreNames(append([]*rerank.Candidate(nil), input...), rerank.QueryClassConcept)
	want := []*rerank.Candidate{leaf1, validate1, validate2, callable, leaf2, validate3}
	for i := range want {
		if concept[i] != want[i] {
			t.Fatalf("concept diversification[%d]=%s want %s", i, concept[i].Node.ID, want[i].Node.ID)
		}
	}
	symbol := diversifyRepeatedExploreNames(append([]*rerank.Candidate(nil), input...), rerank.QueryClassSymbol)
	for i := range input {
		if symbol[i] != input[i] {
			t.Fatalf("identifier lookup order changed at %d", i)
		}
	}
	if got := exploreCandidateFetchLimit(6, rerank.QueryClassConcept); got != 48 {
		t.Fatalf("concept fetch limit=%d want 48", got)
	}
	if got := exploreCandidateFetchLimit(6, rerank.QueryClassSymbol); got != 24 {
		t.Fatalf("symbol fetch limit=%d want 24", got)
	}
}

func TestStripLeadingExploreDirective(t *testing.T) {
	for input, want := range map[string]string{
		"Audit and fix MCP facade impact handling":         "MCP facade impact handling",
		"Please validate operation schema discoverability": "operation schema discoverability",
		"How does Validate work":                           "How does Validate work",
		"fix impact":                                       "fix impact",
	} {
		if got := stripLeadingExploreDirective(input); got != want {
			t.Errorf("stripLeadingExploreDirective(%q)=%q want %q", input, got, want)
		}
	}
}

func TestExploreLocalizationExactTargetPrefersRareTaskCoverage(t *testing.T) {
	method := func(id, name, qualName, file string) exploreTarget {
		return exploreTarget{node: &graph.Node{
			ID: id, Name: name, QualName: qualName, Kind: graph.KindMethod,
			FilePath: file, Meta: map[string]any{"search_qual_name": qualName},
		}}
	}
	targets := []exploreTarget{
		method("regex/matcher.rs::RegexMatcherBuilder.case_insensitive", "case_insensitive", "RegexMatcherBuilder.case_insensitive", "regex/matcher.rs"),
		method("pcre/matcher.rs::RegexMatcherBuilder.case_insensitive", "case_insensitive", "RegexMatcherBuilder.case_insensitive", "pcre/matcher.rs"),
		method("ignore/walk.rs::WalkBuilder.case_insensitive", "case_insensitive", "WalkBuilder.case_insensitive", "ignore/walk.rs"),
		method("regex/literal.rs::Extractor.extract_alternation", "extract_alternation", "Extractor.extract_alternation", "regex/literal.rs"),
	}
	task := "case-insensitive literal alternation optimization causes missed matches"
	if got, want := exploreLocalizationExactTarget(task, targets), targets[3].node.ID; got != want {
		t.Fatalf("exact target=%q want rare conjunctive implementation %q", got, want)
	}
}

func TestExploreLocalizationExactTargetIsStableAndHonorsExplicitAnchor(t *testing.T) {
	head := exploreTarget{node: &graph.Node{ID: "a.go::Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: "common.go"}}
	second := exploreTarget{node: &graph.Node{ID: "b.go::Beta", Name: "Beta", Kind: graph.KindFunction, FilePath: "common.go"}}
	if got := exploreLocalizationExactTarget("alpha beta common behavior", []exploreTarget{head, second}); got != head.node.ID {
		t.Fatalf("equal evidence must retain retrieval order, got %q", got)
	}
	setter := exploreTarget{node: &graph.Node{
		ID: "regex/matcher.rs::RegexMatcherBuilder.case_insensitive", Name: "case_insensitive",
		QualName: "RegexMatcherBuilder.case_insensitive", Kind: graph.KindMethod, FilePath: "regex/matcher.rs",
		Meta: map[string]any{"search_qual_name": "RegexMatcherBuilder.case_insensitive"},
	}}
	if got := exploreLocalizationExactTarget("inspect RegexMatcherBuilder.case_insensitive", []exploreTarget{head, setter}); got != setter.node.ID {
		t.Fatalf("explicit qualified anchor must win, got %q", got)
	}
}

func TestExploreAnswerReadyRequiresQualifiedSymbolAnchor(t *testing.T) {
	wrong := &graph.Node{
		ID: "repo/pkg/other.go::Other.Validate", Name: "Validate", Kind: graph.KindMethod,
		FilePath: "pkg/other.go", StartLine: 10, EndLine: 20,
	}
	if exploreAnswerReady("Client.Validate", []exploreTarget{{node: wrong}}) {
		t.Fatal("same-name method in a different qualifier must not be terminal")
	}

	right := &graph.Node{
		ID: "repo/pkg/client.go::Client.Validate", Name: "Validate", Kind: graph.KindMethod,
		FilePath: "pkg/client.go", StartLine: 10, EndLine: 20,
	}
	if !exploreAnswerReady("Client.Validate", []exploreTarget{{node: right}}) {
		t.Fatal("fully-qualified symbol anchor should be terminal")
	}
}

func TestExploreExplicitPathAnchorDoesNotCrossSameBasename(t *testing.T) {
	n := &graph.Node{
		ID: "repo/pkg/b/config.go::Load", Name: "Load", Kind: graph.KindFunction,
		FilePath: "pkg/b/config.go", StartLine: 1, EndLine: 5,
	}
	if exploreAnswerReady("pkg/a/config.go", []exploreTarget{{node: n}}) {
		t.Fatal("directory-qualified path must not fall back to the matching basename")
	}
	if !exploreAnswerReady("pkg/b/config.go", []exploreTarget{{node: n}}) {
		t.Fatal("full normalized repository-relative path should be terminal")
	}
	if !exploreLocalizationExplicitAnchor("config.go", n) {
		t.Fatal("basename fallback should remain available without a directory")
	}
}

func TestLocalizationEvidenceReservesExactBeforePromotedNeighbor(t *testing.T) {
	primary := &graph.Node{
		ID: "repo/walk.go::WalkBuilder.build_parallel", Name: "build_parallel", Kind: graph.KindMethod,
		FilePath: "walk.go", StartLine: 10, EndLine: 20,
	}
	exact := &graph.Node{
		ID: "repo/walk.go::WalkBuilder.build_serial", Name: "build_serial", Kind: graph.KindMethod,
		FilePath: "walk.go", StartLine: 30, EndLine: 40,
	}
	promoted := &graph.Node{
		ID: "repo/ignore.go::IgnoreBuilder.build_with_cwd", Name: "build_with_cwd", Kind: graph.KindMethod,
		FilePath: "ignore.go", StartLine: 50, EndLine: 60,
	}
	targets := []exploreTarget{
		{node: primary, callees: []*graph.Node{promoted}},
		{node: exact},
	}
	selected := localizationEvidenceTargets("parallel multi-root walk build_with_cwd", exact.ID, targets)
	if len(selected) != 2 {
		t.Fatalf("selected evidence = %d, want 2", len(selected))
	}
	if selected[0].node.ID != primary.ID || selected[1].node.ID != exact.ID {
		t.Fatalf("selected evidence = [%s, %s], want primary then exact", selected[0].node.ID, selected[1].node.ID)
	}

	result := newLocalizationExploreResultForTask(
		newLocalizationCompletion(false, exact.ID),
		"parallel multi-root walk build_with_cwd",
		targets,
		512,
	)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil || len(wire.Content) == 0 {
		t.Fatalf("decode tool result: %v", err)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(wire.Content[0].Text), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Symbols) != 2 || envelope.Symbols[0] != primary.ID || envelope.Symbols[1] != exact.ID {
		t.Fatalf("symbols = %v, want primary and exact", envelope.Symbols)
	}
	if len(envelope.Evidence) != 2 || envelope.Evidence[1].ID != exact.ID {
		t.Fatalf("exact evidence missing: %+v", envelope.Evidence)
	}
	if len(envelope.Files) != 2 || envelope.Files[0] != "walk.go" || envelope.Files[1] != "walk.go" {
		t.Fatalf("files = %v, want one positional file per primary/exact row", envelope.Files)
	}
}

func TestLocalizationEnvelopeEnforcesSerializedBudgetWithLongMetadata(t *testing.T) {
	primary := &graph.Node{
		ID: "repo/primary.go::Primary", Name: "Primary", Kind: graph.KindFunction,
		FilePath: "primary.go", StartLine: 1, EndLine: 10,
	}
	exact := &graph.Node{
		ID: "repo/exact.go::Exact", Name: "Exact", Kind: graph.KindFunction,
		FilePath: "exact.go", StartLine: 1, EndLine: 10,
	}
	longMetadata := strings.Repeat("quoted-\"-slash-\\-metadata-", 24)
	neighbors := make([]*graph.Node, 0, 12)
	for i := 0; i < 12; i++ {
		neighbors = append(neighbors, &graph.Node{ID: fmt.Sprintf("repo/neighbor-%02d-%s", i, longMetadata)})
	}
	targets := []exploreTarget{
		{node: primary, callers: neighbors, callees: neighbors, source: longMetadata},
		{node: exact, callers: neighbors, callees: neighbors, source: longMetadata},
	}
	for i := 0; i < 10; i++ {
		targets = append(targets, exploreTarget{node: &graph.Node{
			ID:   fmt.Sprintf("repo/optional-%02d.go::%s", i, longMetadata),
			Name: longMetadata, Kind: graph.KindFunction,
			FilePath: fmt.Sprintf("optional/%02d/%s.go", i, longMetadata), StartLine: 1, EndLine: 2,
		}, source: longMetadata})
	}

	const budget = 512
	result := newLocalizationExploreResultForTask(newLocalizationCompletion(false, exact.ID), "", targets, budget)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil || len(wire.Content) == 0 {
		t.Fatalf("decode tool result: %v", err)
	}
	text := wire.Content[0].Text
	if len(text) > budget*localizationEnvelopeBytesPerToken {
		t.Fatalf("serialized envelope = %d bytes, budget = %d", len(text), budget*localizationEnvelopeBytesPerToken)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Symbols) < 2 || envelope.Symbols[0] != primary.ID || envelope.Symbols[1] != exact.ID {
		t.Fatalf("mandatory symbols not retained: %v", envelope.Symbols)
	}
	if len(envelope.Evidence) < 2 || envelope.Evidence[0].ID != primary.ID || envelope.Evidence[1].ID != exact.ID {
		t.Fatalf("mandatory evidence not retained: %+v", envelope.Evidence)
	}
	for _, evidence := range envelope.Evidence {
		if len(evidence.Callers) > localizationMaxNeighborIDs || len(evidence.Callees) > localizationMaxNeighborIDs {
			t.Fatalf("neighbor metadata not capped: callers=%d callees=%d", len(evidence.Callers), len(evidence.Callees))
		}
		if len([]rune(evidence.Signature)) > localizationMaxSignatureRunes {
			t.Fatalf("signature metadata not capped: %d", len([]rune(evidence.Signature)))
		}
	}
	if got := compactLocalizationField(longMetadata, localizationMaxSignatureRunes); len([]rune(got)) > localizationMaxSignatureRunes {
		t.Fatalf("signature compaction = %d runes", len([]rune(got)))
	}
}

func TestLocalizationEnvelopeIncludesBoundedStructuralNeighbor(t *testing.T) {
	buildParallel := &graph.Node{ID: "crates/ignore/src/walk.rs::WalkBuilder.build_parallel", Name: "build_parallel", QualName: "WalkBuilder.build_parallel", Kind: graph.KindMethod, FilePath: "crates/ignore/src/walk.rs"}
	buildWithCWD := &graph.Node{ID: "crates/ignore/src/dir.rs::IgnoreBuilder.build_with_cwd", Name: "build_with_cwd", QualName: "IgnoreBuilder.build_with_cwd", Kind: graph.KindMethod, FilePath: "crates/ignore/src/dir.rs"}
	unrelated := &graph.Node{ID: "crates/core/src/log.rs::trace_event", Name: "trace_event", Kind: graph.KindFunction, FilePath: "crates/core/src/log.rs"}
	targets := []exploreTarget{
		{node: buildParallel, callees: []*graph.Node{unrelated, buildWithCWD}},
		{node: &graph.Node{ID: "crates/ignore/src/walk.rs::WalkParallel", Name: "WalkParallel", Kind: graph.KindType, FilePath: "crates/ignore/src/walk.rs"}},
	}
	result := newLocalizationExploreResultForTask(
		newLocalizationCompletion(true, ""),
		"nondeterministic WalkBuilder parallel multi-root walk build_parallel",
		targets,
		1600,
	)
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected one text result: %#v", result)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode localization envelope: %v", err)
	}
	if len(envelope.Evidence) > len(targets) {
		t.Fatalf("promoted evidence exceeded direct target bound: %#v", envelope.Evidence)
	}
	joinedSymbols := strings.Join(envelope.Symbols, "\n")
	if !strings.Contains(joinedSymbols, buildWithCWD.ID) {
		t.Fatalf("structural implementation missing from terminal symbols: %#v", envelope.Symbols)
	}
	if strings.Contains(joinedSymbols, unrelated.ID) {
		t.Fatalf("unrelated graph neighbor leaked into terminal symbols: %#v", envelope.Symbols)
	}
	joinedFiles := strings.Join(envelope.Files, "\n")
	if !strings.Contains(joinedFiles, buildWithCWD.FilePath) {
		t.Fatalf("structural implementation file missing: %#v", envelope.Files)
	}
}

func TestRefinementEnvelopeAuthorizesSerializedEvidenceAndOmitsSource(t *testing.T) {
	buildParallel := &graph.Node{ID: "crates/ignore/src/walk.rs::WalkBuilder.build_parallel", Name: "build_parallel", QualName: "WalkBuilder.build_parallel", Kind: graph.KindMethod, FilePath: "crates/ignore/src/walk.rs"}
	buildWithCWD := &graph.Node{ID: "crates/ignore/src/dir.rs::IgnoreBuilder.build_with_cwd", Name: "build_with_cwd", QualName: "IgnoreBuilder.build_with_cwd", Kind: graph.KindMethod, FilePath: "crates/ignore/src/dir.rs"}
	unrelated := &graph.Node{ID: "crates/core/src/log.rs::trace_event", Name: "trace_event", Kind: graph.KindFunction, FilePath: "crates/core/src/log.rs"}
	targets := []exploreTarget{
		{node: buildParallel, source: "fn build_parallel(&self) {}", callees: []*graph.Node{unrelated, buildWithCWD}},
		{node: &graph.Node{ID: "crates/ignore/src/walk.rs::WalkParallel", Name: "WalkParallel", Kind: graph.KindType, FilePath: "crates/ignore/src/walk.rs"}, source: "struct WalkParallel;"},
	}
	result, authorized, _ := buildLocalizationExploreResultForTask(
		newLocalizationRefinementCompletion(buildParallel.ID),
		"nondeterministic WalkBuilder parallel multi-root walk build_parallel",
		targets,
		1600,
	)
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected one text result: %#v", result)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode localization envelope: %v", err)
	}
	if !strings.Contains(envelope.Completion.RequiredAction, buildParallel.ID) {
		t.Fatalf("refinement action did not name preferred symbol: %#v", envelope.Completion)
	}
	if envelope.Completion.ExactSymbol != "" {
		t.Fatalf("uncertain refinement advertised exact symbol: %#v", envelope.Completion)
	}
	if strings.Join(authorized, "\n") != strings.Join(envelope.Symbols, "\n") {
		t.Fatalf("authorization differs from serialized symbols: authorized=%#v symbols=%#v", authorized, envelope.Symbols)
	}
	if len(authorized) > 12 {
		t.Fatalf("refinement authorization exceeded cap: %d", len(authorized))
	}
	if !strings.Contains(strings.Join(authorized, "\n"), buildWithCWD.ID) {
		t.Fatalf("serialized structural target was not authorized: %#v", authorized)
	}
	if strings.Contains(strings.Join(authorized, "\n"), unrelated.ID) {
		t.Fatalf("neighbor hint became a refinement target: %#v", authorized)
	}
	for _, evidence := range envelope.Evidence {
		if evidence.Source != "" {
			t.Fatalf("needs-refinement duplicated source for %s", evidence.ID)
		}
	}
}

func TestRefinementEnvelopeAndHostDigestShareBoundedAuthorization(t *testing.T) {
	allowed := []string{
		"repo/storage/level.go::StorageLevel.Normalize",
		"repo/storage/batch_gate.go::BatchGate.FlushPending",
		"repo/storage/batch_gate.go::BatchGate.ClearPending",
	}
	longPathSegment := strings.Repeat("very-long-path-segment-", 8)
	targets := make([]exploreTarget, 0, localizationReplayEvidenceLimit)
	visibleIDs := make([]string, 0, localizationReplayEvidenceLimit)
	for index := 0; index < localizationReplayEvidenceLimit; index++ {
		file := fmt.Sprintf("src/%s/%02d.go", longPathSegment, index)
		id := fmt.Sprintf(
			"repo/%s/%02d.go::Unrelated%02d.%s",
			longPathSegment, index, index, strings.Repeat("LongMethod", 6),
		)
		name := fmt.Sprintf("Unrelated%02d", index)
		qualName := fmt.Sprintf("Unrelated%02d.%s", index, strings.Repeat("LongMethod", 6))
		switch index {
		case 0:
			id, name, qualName, file = allowed[0], "Normalize", "StorageLevel.Normalize", "repo/storage/level.go"
		case 2:
			id, name, qualName, file = allowed[1], "FlushPending", "BatchGate.FlushPending", "repo/storage/batch_gate.go"
		case 8:
			id, name, qualName, file = allowed[2], "ClearPending", "BatchGate.ClearPending", "repo/storage/batch_gate.go"
		}
		node := &graph.Node{
			ID: id, Name: name, QualName: qualName, Kind: graph.KindMethod,
			FilePath: file, StartLine: index + 1, EndLine: index + 2,
		}
		targets = append(targets, exploreTarget{node: node, source: "func " + name + "() {}"})
		visibleIDs = append(visibleIDs, id)
	}
	routes := map[string]localizationRefinementRoute{
		allowed[0]: {enforceable: true},
		allowed[1]: {enforceable: true},
		allowed[2]: {enforceable: true},
	}
	result, packed, _, digest := buildLocalizationRefinementResultForTask(
		allowed[0], "", targets, exploreMaxBudgetTokens, routes,
	)
	if result == nil || result.IsError || digest == nil || packed.State != localizationStateNeedsRefinement {
		t.Fatalf("packed refinement = result=%#v completion=%#v digest=%#v", result, packed, digest)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("refinement result content = %#v", result.Content)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode refinement envelope: %v", err)
	}
	if len(envelope.Evidence) != len(visibleIDs) || len(envelope.Files) != len(visibleIDs) || len(envelope.Symbols) != len(visibleIDs) {
		t.Fatalf("visible cardinality changed: files=%d symbols=%d evidence=%d want=%d", len(envelope.Files), len(envelope.Symbols), len(envelope.Evidence), len(visibleIDs))
	}
	for index, want := range visibleIDs {
		row := envelope.Evidence[index]
		if envelope.Symbols[index] != want || row.ID != want || envelope.Files[index] != row.File || row.Rank != index+1 {
			t.Fatalf("visible row %d changed or diverged: want=%q row=%#v", index+1, want, row)
		}
	}
	if len(digest.Evidence) >= len(envelope.Evidence) {
		t.Fatalf("fixture did not force retained byte shedding: retained=%d visible=%d", len(digest.Evidence), len(envelope.Evidence))
	}
	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("refinement result omitted authenticated host envelope")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || host.Evidence == nil {
		t.Fatalf("host localization envelope = %#v", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	wantAllowed := strings.Join(allowed, "\n")
	if strings.Join(packed.AllowedSymbols, "\n") != wantAllowed ||
		strings.Join(envelope.Completion.AllowedSymbols, "\n") != wantAllowed ||
		strings.Join(host.Contract.Completion.AllowedSymbols, "\n") != wantAllowed {
		t.Fatalf("completion authorization diverged: packed=%#v visible=%#v host=%#v", packed.AllowedSymbols, envelope.Completion.AllowedSymbols, host.Contract.Completion.AllowedSymbols)
	}
	if len(host.Evidence.Evidence) < len(allowed) {
		t.Fatalf("host retained %d rows, want %d authorized rows", len(host.Evidence.Evidence), len(allowed))
	}
	for index, want := range allowed {
		row := host.Evidence.Evidence[index]
		if row.ID != want || host.Evidence.Symbols[index] != want || host.Evidence.Files[index] != row.File || row.Rank != index+1 {
			t.Fatalf("host authorized row %d diverged: want=%q row=%#v", index+1, want, row)
		}
	}
	encoded, err := json.Marshal(host.Evidence)
	if err != nil || len(encoded) > localizationDigestMaxBytes || len(host.Evidence.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("host digest bytes=%d final=%d err=%v", len(encoded), len(host.Evidence.finalResponse), err)
	}
}
