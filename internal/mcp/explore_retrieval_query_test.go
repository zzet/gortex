package mcp

import (
	"strings"
	"testing"
)

const exploreCoordinatorLatencyTask = "Trace exact coordinator flow for a single native file mutation on a dirty worktree: how dirty identity is computed, whether each mutation re-enumerates the entire working-tree diff and closure against the pinned committed base, which generation/payload/ancestry caches exist, and whether unchanged dirty-file payloads can be safely reused. Identify minimal missing cache identity/validity guards for same base only, while preserving full closure and semantic completion before publication. No code changes."

const exploreExactLiveCommitLayerTask = "Integrate the already tested additive commitLayerBase context forwarding methods in internal/indexer/commit_layer_name_context.go. Diagnose the current commitLayerBase wrapper and optional name lookup interfaces only; candidate delegates FindNodesByNameContext and VisitNodesByNameContext to b.Reader to preserve cancellation and composed overlay masks. Need exact current declaration location and minimal mutation context; do not run broad summaries."

func TestShapeExploreRetrievalQueryBoundsLongInlineTask(t *testing.T) {
	semantic := shapeExploreQuery(exploreCoordinatorLatencyTask)
	if len(semantic) <= len(exploreCoordinatorLatencyTask) {
		t.Fatalf("semantic query was not expanded: task=%d semantic=%d query=%q", len(exploreCoordinatorLatencyTask), len(semantic), semantic)
	}
	got := shapeExploreRetrievalQuery(exploreCoordinatorLatencyTask, semantic)
	t.Logf("semantic_bytes=%d retrieval_bytes=%d retrieval=%q", len(semantic), len(got), got)
	if got == semantic || len(got) >= len(semantic) {
		t.Fatalf("retrieval query was not reduced: %q", got)
	}
	for _, anchor := range []string{"dirty", "payload", "closure"} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(anchor)) {
			t.Fatalf("retrieval query lost anchor %q: %q", anchor, got)
		}
	}
	terms := exploreConceptRecallTerms(semantic)
	if hasExploreExpansionTerms(got, terms) {
		t.Fatalf("compact primary would be repeated by expansion: retrieval=%q terms=%q", got, terms)
	}
	seen := map[string]bool{}
	for _, term := range strings.Fields(strings.ToLower(got)) {
		if seen[term] {
			t.Fatalf("duplicate retrieval term %q in %q", term, got)
		}
		seen[term] = true
	}
}

func TestShapeExploreRetrievalQueryPreservesFocusedQueries(t *testing.T) {
	for _, task := range []string{
		"commitLayerBase",
		"trace commitLayerBase ancestry closure",
		"find Store.SearchSymbolsRepoScopedContext",
	} {
		semantic := shapeExploreQuery(task)
		if got := shapeExploreRetrievalQuery(task, semantic); got != semantic {
			t.Fatalf("task=%q got=%q want=%q", task, got, semantic)
		}
	}
}

func TestShapeExploreRetrievalQueryKeepsLateDistinctiveIdentifier(t *testing.T) {
	task := strings.Repeat("Explain how the coordinator processes repeated mutation work and preserves correctness. ", 4) + "The concrete implementation anchor is commitLayerBase."
	got := shapeExploreRetrievalQuery(task, shapeExploreQuery(task))
	if !strings.Contains(strings.ToLower(got), strings.ToLower("commitLayerBase")) {
		t.Fatalf("late identifier was lost: %q", got)
	}
}

func TestShapeExploreRetrievalQueryCapsManyIdentifiersAndKeepsLateAnchor(t *testing.T) {
	task := "Explain AlphaResolver BetaResolver GammaResolver DeltaResolver EpsilonResolver ZetaResolver EtaResolver ThetaResolver " +
		"IotaResolver KappaResolver LambdaResolver MuResolver NuResolver XiResolver OmicronResolver PiResolver RhoResolver SigmaResolver " +
		strings.Repeat("while repeated coordinator mutation prose adds no useful search signal ", 8) + "LateCriticalResolver"
	semantic := shapeExploreQuery(task)
	got := shapeExploreRetrievalQuery(task, stripLeadingExploreDirective(semantic))
	if fields := strings.Fields(got); len(fields) > exploreRetrievalQueryTermCap {
		t.Fatalf("retrieval terms = %d, cap = %d: %q", len(fields), exploreRetrievalQueryTermCap, got)
	}
	if !strings.Contains(got, "LateCriticalResolver") {
		t.Fatalf("bounded retrieval lost late implementation anchor: %q", got)
	}
}

func TestShapeExploreRetrievalQueryExactLiveCommitLayerRequest(t *testing.T) {
	semantic := stripLeadingExploreDirective(shapeExploreQuery(exploreExactLiveCommitLayerTask))
	got := shapeExploreRetrievalQuery(exploreExactLiveCommitLayerTask, semantic)
	t.Logf("semantic_bytes=%d retrieval_bytes=%d terms=%d retrieval=%q", len(semantic), len(got), len(strings.Fields(got)), got)
	if fields := strings.Fields(got); len(fields) > exploreRetrievalQueryTermCap {
		t.Fatalf("retrieval terms = %d, cap = %d: %q", len(fields), exploreRetrievalQueryTermCap, got)
	}
	for _, anchor := range []string{"commitLayerBase", "FindNodesByNameContext", "VisitNodesByNameContext"} {
		if !strings.Contains(got, anchor) {
			t.Fatalf("exact live retrieval lost %q: %q", anchor, got)
		}
	}
}
