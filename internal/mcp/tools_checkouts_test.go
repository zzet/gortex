package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// The administrative tools are tested through their handlers against a real
// sqlite store, a real git repository and a real linked worktree. What is
// being asserted is what each tool decides — which rows it reports, and
// whether it wrote anything — and a stubbed catalog would remove exactly that.

// adminToolHandler is the shape every handler under test has.
type adminToolHandler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)

// callAdminTool runs one handler and returns its JSON payload.
func callAdminTool(t *testing.T, handler adminToolHandler, args map[string]any) []byte {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "tool failed: %+v", res.Content)
	return []byte(extractTextFromContent(t, res.Content))
}

// checkoutAdminFixture is one tracked repository and the linked worktree
// beside it, with the catalog rows the daemon made of them.
type checkoutAdminFixture struct {
	srv      *Server
	mi       *indexer.MultiIndexer
	catalog  *store_sqlite.Catalog
	dir      string
	main     string
	worktree string
	prefix   string
}

func newCheckoutAdminFixture(t *testing.T) *checkoutAdminFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	srv, mi, catalog, dir := newCatalogMCPServer(t)

	main := filepath.Join(dir, "main-repo")
	gitInitWorktreeRepo(t, main)
	worktree := filepath.Join(dir, "wt")
	gitInit(t, main, "worktree", "add", "-q", "-b", "task", worktree)

	prefix, status := trackRepoPrefix(t, srv, map[string]any{"path": main})
	require.Equal(t, "tracked", status)

	fixture := &checkoutAdminFixture{
		srv: srv, mi: mi, catalog: catalog, dir: dir,
		main: main, worktree: worktree, prefix: prefix,
	}
	// The worktree beside the tracked repository is the family's other
	// checkout. A reconciliation is what mints its identity.
	callAdminTool(t, srv.handleReconcileCheckouts, map[string]any{})
	return fixture
}

// activateWorktree wakes the family's automatic checkout — what a session
// selecting its view does — and waits for its coordinator to come up, so a
// test that needs a live coordinator to count has one. Registration only mints
// the identity; the coordinator is started on demand.
func (f *checkoutAdminFixture) activateWorktree(t *testing.T, familyID string) {
	t.Helper()
	ctx := context.Background()
	checkouts, err := f.catalog.ListCheckouts(ctx, familyID)
	require.NoError(t, err)
	var automatic string
	for i := range checkouts {
		if checkouts[i].EffectiveMode == store_sqlite.CheckoutModeAutomatic {
			automatic = checkouts[i].CheckoutID
		}
	}
	require.NotEmpty(t, automatic, "the family has no automatic checkout to activate")
	require.True(t, f.srv.lifecycle.ActivateCheckout(automatic, "test select"))
	deadline := time.Now().Add(30 * time.Second)
	for f.srv.lifecycle.LiveCoordinators(familyID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the activated worktree never ran a coordinator")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// quiesce stops the family's build loops.
//
// A live coordinator repoints the route it finds at the layers it builds
// itself: it clears the working-tree slot and puts the route back to pending
// for the length of the rebuild. A test that installs a route by hand and
// then reads it has to stop the loops first, or it races that rebuild.
//
// Closing writes nothing to the catalog — it is about the goroutines — and it
// returns only once a cycle already in flight has finished, so no coordinator
// can touch a route after it.
func (f *checkoutAdminFixture) quiesce(t *testing.T) {
	t.Helper()
	require.NoError(t, f.srv.lifecycle.Close())
}

// families reads the listing tool's answer.
func (f *checkoutAdminFixture) families(t *testing.T, args map[string]any) indexer.FamiliesOverview {
	t.Helper()
	var overview indexer.FamiliesOverview
	require.NoError(t, json.Unmarshal(callAdminTool(t, f.srv.handleListCheckouts, args), &overview))
	return overview
}

// checkoutNamed finds one family's checkout by the name git administers it as.
func checkoutNamed(t *testing.T, family indexer.FamilyOverview, adminName string) indexer.CheckoutOverview {
	t.Helper()
	for _, checkout := range family.Checkouts {
		if checkout.AdminName == adminName {
			return checkout
		}
	}
	t.Fatalf("family %s holds no checkout administered as %q", family.FamilyID, adminName)
	return indexer.CheckoutOverview{}
}

func checkoutBudgetOverview(checkoutCount int) indexer.FamiliesOverview {
	checkouts := make([]indexer.CheckoutOverview, 0, checkoutCount)
	for i := 0; i < checkoutCount; i++ {
		adminName := fmt.Sprintf("wt-%03d", i)
		effectiveMode := string(store_sqlite.CheckoutModeAutomatic)
		graphID := ""
		if i == 0 {
			adminName = gitstate.MainAdminName
			effectiveMode = string(store_sqlite.CheckoutModeDedicated)
			graphID = "graph-primary"
		}
		root := fmt.Sprintf("/tmp/high-cardinality-checkout-family/worktrees/%s/%s",
			adminName, strings.Repeat("representative-path-segment/", 3))
		checkouts = append(checkouts, indexer.CheckoutOverview{
			CheckoutID:      fmt.Sprintf("checkout-%03d", i),
			Incarnation:     fmt.Sprintf("incarnation-%03d", i),
			AdminName:       adminName,
			RootPath:        root,
			GitDir:          root + "/.git",
			State:           string(store_sqlite.CheckoutStateReady),
			DesiredMode:     effectiveMode,
			EffectiveMode:   effectiveMode,
			HeadRef:         fmt.Sprintf("refs/heads/feature-%03d", i),
			HeadCommit:      strings.Repeat("a", 40),
			HeadTree:        strings.Repeat("b", 40),
			LastAccessible:  1_788_000_000 + int64(i),
			LastSeen:        1_788_000_100 + int64(i),
			GraphID:         graphID,
			CoordinatorLive: i == 0,
			Evidence: indexer.EvidenceOverview{
				Present:                     true,
				RootPathIdentity:            root,
				RootVolumeKind:              "local",
				NearestExistingAncestorPath: filepath.Dir(root),
				SampledAt:                   1_788_000_000 + int64(i),
				SampleGeneration:            int64(i + 1),
			},
		})
	}
	return indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID:          "family-high-cardinality",
		CommonDir:         "/tmp/high-cardinality-checkout-family/main/.git",
		State:             "family_ready",
		PrimaryEpoch:      7,
		PrimaryGraphID:    "graph-primary",
		PrimaryRepoPrefix: "repo-high-cardinality",
		Graphs: []indexer.GraphOverview{{
			GraphID: "graph-primary", RepoPrefix: "repo-high-cardinality",
			IsPrimary: true, State: "ready", Served: true,
		}},
		Checkouts: checkouts,
	}}}
}

func TestCheckoutOverviewPayloadPreservesFamilyEnvelopeUnderDefaultBudget(t *testing.T) {
	overview := checkoutBudgetOverview(257)
	unbudgeted, err := json.Marshal(overview)
	require.NoError(t, err)
	require.Greater(t, len(unbudgeted), defaultMaxBytes, "fixture must cross the default response budget")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	renderReq, payload := prepareCheckoutOverviewResponse(req, overview)
	require.Zero(t, effectiveBudget(renderReq), "the common formatter must not apply a second budget")
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), defaultMaxBytes)

	root, ok := payload.(map[string]any)
	require.True(t, ok, "an oversized typed overview must become a detached budgeted map")
	require.Equal(t, true, root[budgetTruncatedKey])
	families, ok := root["families"].([]any)
	require.True(t, ok)
	require.Len(t, families, 1, "the family envelope must survive nested checkout trimming")
	family, ok := families[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "family-high-cardinality", family["family_id"])
	require.Equal(t, true, family[budgetTruncatedKey])
	require.Equal(t, 257, family["_original_count_checkouts"])

	checkouts, ok := family["checkouts"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, checkouts, "the cap has room for a useful stable prefix")
	require.Less(t, len(checkouts), 257)
	require.Equal(t, len(checkouts), family["_max_returned_checkouts"])
	first, ok := checkouts[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, gitstate.MainAdminName, first["admin_name"], "the catalog's stable @main-first order must survive")
	require.Len(t, overview.Families[0].Checkouts, 257, "budgeting must not mutate the lifecycle read model")

	again, againTrimmed := applyCheckoutOverviewBudget(overview, defaultMaxBytes)
	require.True(t, againTrimmed)
	againEncoded, err := json.Marshal(again)
	require.NoError(t, err)
	require.Equal(t, encoded, againEncoded, "unchanged input must produce a byte-stable prefix and metadata")

	// Ride the shared formatter/budget path too: the pre-budgeted nested
	// answer must not be reinterpreted as one oversized top-level family and
	// erased on the second guard.
	srv, _ := setupTestServer(t)
	renderReq.Params.Name = "list_checkouts"
	renderArgs := renderReq.GetArguments()
	renderArgs["format"] = "json"
	renderReq.Params.Arguments = renderArgs
	res, err := srv.respondJSONOrTOON(context.Background(), renderReq, payload)
	require.NoError(t, err)
	require.False(t, res.IsError)
	wire := []byte(extractTextFromContent(t, res.Content))
	require.LessOrEqual(t, len(wire), defaultMaxBytes)
	var formatted map[string]any
	require.NoError(t, json.Unmarshal(wire, &formatted))
	require.Len(t, formatted["families"], 1)
}

func TestCheckoutOverviewPayloadMaxBytesZeroIsExhaustive(t *testing.T) {
	overview := checkoutBudgetOverview(257)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "list_checkouts"
	req.Params.Arguments = map[string]any{"format": "json", "max_bytes": float64(0)}

	renderReq, payload := prepareCheckoutOverviewResponse(req, overview)
	full, ok := payload.(indexer.FamiliesOverview)
	require.True(t, ok, "the opt-out must preserve the typed exhaustive answer")
	require.Len(t, full.Families, 1)
	require.Len(t, full.Families[0].Checkouts, 257)

	srv, _ := setupTestServer(t)
	res, err := srv.respondJSONOrTOON(context.Background(), renderReq, payload)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var wire indexer.FamiliesOverview
	require.NoError(t, json.Unmarshal([]byte(extractTextFromContent(t, res.Content)), &wire))
	require.Len(t, wire.Families, 1)
	require.Len(t, wire.Families[0].Checkouts, 257,
		"the shared formatter must honor the exhaustive opt-out too")
}

func TestCheckoutOverviewMaxTokensPreservesFamilyAndDecoration(t *testing.T) {
	const maxTokens = 6_000
	overview := checkoutBudgetOverview(257)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "list_checkouts"
	req.Params.Arguments = map[string]any{
		"format":     "json",
		"max_tokens": float64(maxTokens),
	}

	renderReq, payload := prepareCheckoutOverviewResponse(req, overview)
	require.Zero(t, effectiveBudget(renderReq))
	require.NotContains(t, renderReq.GetArguments(), "max_tokens",
		"the common formatter must not reserve or decorate a second time")

	srv, _ := setupTestServer(t)
	res, err := srv.respondJSONOrTOON(context.Background(), renderReq, payload)
	require.NoError(t, err)
	require.False(t, res.IsError)
	wire := []byte(extractTextFromContent(t, res.Content))
	require.LessOrEqual(t, len(wire), tokensToBytes(maxTokens))

	var root map[string]any
	require.NoError(t, json.Unmarshal(wire, &root))
	require.Equal(t, true, root[budgetTruncatedKey])
	require.Equal(t, true, root["_truncated_by_tokens"])
	require.Equal(t, float64(maxTokens), root["_max_tokens"])
	families := root["families"].([]any)
	require.Len(t, families, 1, "token budgeting must trim nested rows, not the family envelope")
	family := families[0].(map[string]any)
	checkouts := family["checkouts"].([]any)
	require.NotEmpty(t, checkouts)
	require.Less(t, len(checkouts), 257)
	require.Equal(t, float64(257), family["_original_count_checkouts"])
	require.Equal(t, float64(len(checkouts)), family["_max_returned_checkouts"])
}

func TestCheckoutOverviewFieldsProjectBeforeBudget(t *testing.T) {
	overview := checkoutBudgetOverview(257)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "list_checkouts"
	req.Params.Arguments = map[string]any{
		"fields": "family_id",
		"format": "json",
	}

	renderReq, payload := prepareCheckoutOverviewResponse(req, overview)
	require.NotContains(t, renderReq.GetArguments(), "fields",
		"the common formatter must not project an already-projected payload")
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.Less(t, len(encoded), defaultMaxBytes)
	require.NotContains(t, string(encoded), budgetTruncatedKey,
		"a sparse fieldset that already fits must not be unnecessarily pre-trimmed")

	srv, _ := setupTestServer(t)
	res, err := srv.respondJSONOrTOON(context.Background(), renderReq, payload)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal([]byte(extractTextFromContent(t, res.Content)), &root))
	families := root["families"].([]any)
	require.Len(t, families, 1)
	require.Equal(t, map[string]any{"family_id": "family-high-cardinality"}, families[0])
}

func TestApplyCheckoutOverviewBudgetTrimsGraphsAndRefViewsInsideFamily(t *testing.T) {
	const rows = 96
	cases := []struct {
		name  string
		key   string
		build func() indexer.FamiliesOverview
	}{
		{
			name: "graphs",
			key:  "graphs",
			build: func() indexer.FamiliesOverview {
				graphs := make([]indexer.GraphOverview, 0, rows)
				for i := 0; i < rows; i++ {
					graphs = append(graphs, indexer.GraphOverview{
						GraphID: fmt.Sprintf("graph-%03d-%s", i, strings.Repeat("g", 80)),
						RepoPrefix: fmt.Sprintf("repo-%03d/%s", i,
							strings.Repeat("representative-prefix/", 6)),
						State: "ready", Served: true,
					})
				}
				return indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
					FamilyID: "family-graphs", CommonDir: "/tmp/graphs/.git", Graphs: graphs,
				}}}
			},
		},
		{
			name: "ref_views",
			key:  "ref_views",
			build: func() indexer.FamiliesOverview {
				views := make([]indexer.RefViewOverview, 0, rows)
				for i := 0; i < rows; i++ {
					views = append(views, indexer.RefViewOverview{
						RefViewID: fmt.Sprintf("view-%03d-%s", i, strings.Repeat("v", 80)),
						GraphID:   "graph-primary", SelectorKind: "git_ref",
						SelectorValue: fmt.Sprintf("refs/heads/feature-%03d/%s", i,
							strings.Repeat("representative-branch/", 6)),
						State: "ready", ActiveTree: strings.Repeat("c", 40),
					})
				}
				return indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
					FamilyID: "family-ref-views", CommonDir: "/tmp/ref-views/.git", RefViews: views,
				}}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overview := tc.build()
			payload, trimmed := applyCheckoutOverviewBudget(overview, 8_000)
			require.True(t, trimmed)
			encoded, err := json.Marshal(payload)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 8_000)

			root := payload.(map[string]any)
			families := root["families"].([]any)
			require.Len(t, families, 1)
			family := families[0].(map[string]any)
			nested := family[tc.key].([]any)
			require.NotEmpty(t, nested)
			require.Less(t, len(nested), rows)
			require.Equal(t, rows, family["_original_count_"+tc.key])
			require.Equal(t, len(nested), family["_max_returned_"+tc.key])
		})
	}
}

func TestApplyCheckoutOverviewBudgetPreservesCheckoutAndGraphHeads(t *testing.T) {
	const budget = 8_000
	const rows = 24
	overview := checkoutBudgetOverview(rows)
	graphs := make([]indexer.GraphOverview, 0, rows)
	for i := 0; i < rows; i++ {
		graphs = append(graphs, indexer.GraphOverview{
			GraphID: fmt.Sprintf("graph-%03d-%s", i, strings.Repeat("g", 80)),
			RepoPrefix: fmt.Sprintf("repo-%03d/%s", i,
				strings.Repeat("representative-prefix/", 5)),
			IsPrimary: i == 0,
			State:     "ready",
			Served:    true,
		})
	}
	overview.Families[0].Graphs = graphs

	payload, trimmed := applyCheckoutOverviewBudget(overview, budget)
	require.True(t, trimmed)
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), budget)

	root := payload.(map[string]any)
	family := root["families"].([]any)[0].(map[string]any)
	checkouts := family["checkouts"].([]any)
	budgetedGraphs := family["graphs"].([]any)
	require.NotEmpty(t, checkouts, "the primary checkout census must keep its identifying head")
	require.NotEmpty(t, budgetedGraphs, "a second nested collection must keep its identifying head too")
	require.Equal(t, gitstate.MainAdminName, checkouts[0].(map[string]any)["admin_name"])
	require.Equal(t, graphs[0].GraphID, budgetedGraphs[0].(map[string]any)["graph_id"])
	require.Less(t, len(checkouts), rows)
	require.Less(t, len(budgetedGraphs), rows)
	require.Equal(t, rows, family["_original_count_checkouts"])
	require.Equal(t, rows, family["_original_count_graphs"])

	// Metadata for a collection that became complete is removed while fitting.
	// The freed bytes must be offered back to the earlier, higher-priority
	// checkout prefix. Pin the fixed point: its very next stable row cannot fit
	// in the final representation, even after all metadata cleanup has settled.
	nextCheckoutJSON, err := json.Marshal(overview.Families[0].Checkouts[len(checkouts)])
	require.NoError(t, err)
	var nextCheckout map[string]any
	require.NoError(t, json.Unmarshal(nextCheckoutJSON, &nextCheckout))
	family["checkouts"] = append(checkouts, nextCheckout)
	family["_max_returned_checkouts"] = len(checkouts) + 1
	withNextCheckout, err := json.Marshal(root)
	require.NoError(t, err)
	require.Greater(t, len(withNextCheckout), budget,
		"budgeting left enough cleaned-up headroom for the next higher-priority checkout")
}

func TestApplyCheckoutOverviewBudgetPreservesEachFamilyCheckoutHead(t *testing.T) {
	const familyCount = 3
	var overview indexer.FamiliesOverview
	for familyIndex := 0; familyIndex < familyCount; familyIndex++ {
		family := checkoutBudgetOverview(12).Families[0]
		family.FamilyID = fmt.Sprintf("family-%d", familyIndex)
		family.CommonDir = fmt.Sprintf("/tmp/family-%d/.git", familyIndex)
		overview.Families = append(overview.Families, family)
	}

	payload, trimmed := applyCheckoutOverviewBudget(overview, 12_000)
	require.True(t, trimmed)
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 12_000)

	root := payload.(map[string]any)
	families := root["families"].([]any)
	require.Len(t, families, familyCount, "family shells fit and must not be traded for another family's tail")
	for familyIndex, rawFamily := range families {
		family := rawFamily.(map[string]any)
		require.Equal(t, fmt.Sprintf("family-%d", familyIndex), family["family_id"])
		checkouts := family["checkouts"].([]any)
		require.NotEmpty(t, checkouts, "family %d lost its identifying checkout", familyIndex)
		require.Equal(t, gitstate.MainAdminName, checkouts[0].(map[string]any)["admin_name"])
	}
}

func TestCheckoutOverviewTOONHonorsNormalAndTinyBudgets(t *testing.T) {
	overview := checkoutBudgetOverview(257)

	t.Run("normal keeps family", func(t *testing.T) {
		const budget = 8_000
		req := mcplib.CallToolRequest{}
		req.Params.Name = "list_checkouts"
		req.Params.Arguments = map[string]any{"format": "toon", "max_bytes": float64(budget)}
		_, payload := prepareCheckoutOverviewResponseWithSizer(req, overview, checkoutOverviewTOONSize)
		res, err := returnTOON(payload)
		require.NoError(t, err)
		text := extractTextFromContent(t, res.Content)
		require.LessOrEqual(t, len(text), budget)
		require.Contains(t, text, "family-high-cardinality")
		require.Contains(t, text, budgetTruncatedKey)
		require.Contains(t, text, "_original_count_checkouts: 257")
		require.Contains(t, text, "_max_returned_checkouts:")
		require.Contains(t, text, "admin_name: @main")
	})

	for _, budget := range []int{1, 16, 64} {
		t.Run(fmt.Sprintf("tiny-%d", budget), func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = "list_checkouts"
			req.Params.Arguments = map[string]any{"format": "toon", "max_bytes": float64(budget)}
			renderReq, payload := prepareCheckoutOverviewResponseWithSizer(req, overview, checkoutOverviewTOONSize)
			// The shared renderer is deliberately budget-disabled afterward;
			// this format-aware preparation already chose the structural floor.
			require.Zero(t, effectiveBudget(renderReq))
			res, err := returnTOON(payload)
			require.NoError(t, err)
			text := extractTextFromContent(t, res.Content)
			require.NotEmpty(t, text)
			_, err = toon.Decode([]byte(text))
			require.NoError(t, err, "the scalar floor must remain a valid TOON document")
			// Like JSON, TOON preserves its shortest valid structured response
			// when the caller's cap is smaller than that document. At realistic
			// caps the exact encoded-size fitter is a hard ceiling.
			if len(text) > budget {
				decoded, decodeErr := toon.Decode([]byte(text))
				require.NoError(t, decodeErr)
				root := decoded.(map[string]any)
				require.Equal(t, true, root[budgetTruncatedKey])
				require.Empty(t, root["families"])
			}
		})
	}
}

func TestCheckoutOverviewTOONMaxTokensPreservesFamilyAndDecoration(t *testing.T) {
	const maxTokens = 2_000
	overview := checkoutBudgetOverview(257)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "list_checkouts"
	req.Params.Arguments = map[string]any{
		"format":     "toon",
		"max_tokens": float64(maxTokens),
	}

	_, payload := prepareCheckoutOverviewResponseWithSizer(req, overview, checkoutOverviewTOONSize)
	res, err := returnTOON(payload)
	require.NoError(t, err)
	text := extractTextFromContent(t, res.Content)
	require.LessOrEqual(t, len(text), tokensToBytes(maxTokens))

	require.Contains(t, text, budgetTruncatedKey+": true")
	require.Contains(t, text, "_truncated_by_tokens: true")
	require.Contains(t, text, fmt.Sprintf("_max_tokens: %d", maxTokens))
	require.Contains(t, text, "family-high-cardinality")
	require.Contains(t, text, "_original_count_checkouts: 257")
	require.Contains(t, text, "admin_name: @main")
}

func TestCheckoutOverviewJSONKeepsDocumentedScalarFloor(t *testing.T) {
	overview := checkoutBudgetOverview(257)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "list_checkouts"
	req.Params.Arguments = map[string]any{"format": "json", "max_bytes": float64(1)}
	renderReq, payload := prepareCheckoutOverviewResponse(req, overview)

	srv, _ := setupTestServer(t)
	res, err := srv.respondJSONOrTOON(context.Background(), renderReq, payload)
	require.NoError(t, err)
	wire := []byte(extractTextFromContent(t, res.Content))
	require.Greater(t, len(wire), 1, "JSON keeps its documented valid scalar-skeleton floor")
	var root map[string]any
	require.NoError(t, json.Unmarshal(wire, &root), "the floor must remain valid JSON")
	require.Equal(t, true, root[budgetTruncatedKey])
	require.Equal(t, float64(0), root["_max_returned_families"])
	require.Equal(t, float64(1), root["_original_count_families"])
	require.Empty(t, root["families"])
}

var checkoutOverviewBudgetBenchmarkSink any

func BenchmarkCheckoutOverviewBudget257(b *testing.B) {
	overview := checkoutBudgetOverview(257)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		payload, trimmed := applyCheckoutOverviewBudget(overview, defaultMaxBytes)
		if !trimmed {
			b.Fatal("representative overview unexpectedly fit without trimming")
		}
		checkoutOverviewBudgetBenchmarkSink = payload
	}
}

// TestListCheckoutsReportsTheFamilyItsGraphsAndItsCheckouts is the read model
// through the tool: one family, its primary corpus, and both working copies
// with the mode each is served in.
func TestListCheckoutsReportsTheFamilyItsGraphsAndItsCheckouts(t *testing.T) {
	f := newCheckoutAdminFixture(t)

	overview := f.families(t, map[string]any{})
	require.Len(t, overview.Families, 1)
	family := overview.Families[0]
	assert.NotEmpty(t, family.FamilyID)
	assert.Equal(t, f.prefix, family.PrimaryRepoPrefix)
	assert.NotEmpty(t, family.PrimaryGraphID)

	require.Len(t, family.Graphs, 1, "only the tracked repository has a corpus")
	assert.True(t, family.Graphs[0].IsPrimary)
	assert.True(t, family.Graphs[0].Served, "the primary corpus is indexed in this process")

	require.Len(t, family.Checkouts, 2, "the family holds the main worktree and the linked one")
	primary := checkoutNamed(t, family, gitstate.MainAdminName)
	assert.Equal(t, string(store_sqlite.CheckoutModeDedicated), primary.EffectiveMode)
	assert.Equal(t, family.PrimaryGraphID, primary.GraphID)
	assert.True(t, primary.Evidence.Present, "the track sampled the root")
	assert.Contains(t, primary.Intents, string(store_sqlite.IntentSourceMCPTrack))

	linked := checkoutNamed(t, family, "wt")
	assert.Equal(t, string(store_sqlite.CheckoutModeAutomatic), linked.EffectiveMode)
	assert.Empty(t, linked.GraphID, "an automatic checkout owns no corpus")
	assert.False(t, linked.Availability.Running, "a reachable root starts no clock")
	assert.False(t, linked.Removal.Running)

	// The filter takes any of the selectors an administrator has to hand.
	byPath := f.families(t, map[string]any{"family": f.main})
	require.Len(t, byPath.Families, 1)
	assert.Equal(t, family.FamilyID, byPath.Families[0].FamilyID)
}

// TestExplainViewWalksTheBindingChain covers the three answers the diagnostic
// has to tell apart: a routed worktree, a checkout served from its own corpus,
// and a path no checkout contains.
func TestExplainViewWalksTheBindingChain(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	// What is under test is the chain the binding walks, not the builder that
	// fills it, so the builder is stopped before the chain is laid out by hand.
	f.quiesce(t)
	ctx := context.Background()
	family := f.families(t, map[string]any{}).Families[0]
	linked := checkoutNamed(t, family, "wt")

	// A route with both slots filled is what makes a composed view answer.
	// The generations are seeded rather than built.
	commitGen := seedGeneration(t, f.catalog, family.PrimaryGraphID, linked.CheckoutID, "commit")
	dirtyGen := seedGeneration(t, f.catalog, family.PrimaryGraphID, linked.CheckoutID, "dirty")
	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         linked.CheckoutID,
		GraphID:            family.PrimaryGraphID,
		CommitGenerationID: commitGen,
		DirtyGenerationID:  dirtyGen,
		RouteEpoch:         1,
		State:              store_sqlite.RouteActive,
	}))

	routed := explainView(t, f.srv, f.worktree)
	assert.True(t, routed.Matched)
	assert.Equal(t, "wt", routed.AdminName)
	assert.Equal(t, string(store_sqlite.CheckoutModeAutomatic), routed.EffectiveMode)
	assert.True(t, routed.Composed, "both layers are published, so the composed view answers")
	assert.Empty(t, routed.Reason)
	require.NotNil(t, routed.Route)
	assert.Equal(t, commitGen, routed.Route.CommitGenerationID)
	assert.Equal(t, dirtyGen, routed.Route.DirtyGenerationID)
	assert.True(t, routed.Route.Ready)
	assert.Equal(t, family.PrimaryGraphID, routed.PrimaryGraphID)

	dedicated := explainView(t, f.srv, f.main)
	assert.True(t, dedicated.Matched)
	assert.Equal(t, string(store_sqlite.CheckoutModeDedicated), dedicated.EffectiveMode)
	assert.False(t, dedicated.Composed)
	assert.Contains(t, dedicated.Reason, "dedicated")
	assert.Equal(t, family.PrimaryGraphID, dedicated.GraphID)
	assert.Nil(t, dedicated.Route)

	unknown := explainView(t, f.srv, t.TempDir())
	assert.False(t, unknown.Matched)
	assert.False(t, unknown.Composed)
	assert.Contains(t, unknown.Reason, "no registered checkout")
	assert.Empty(t, unknown.CheckoutID)
}

// TestSetPrimaryCheckoutPreviewsBeforeItMoves proves the preview writes
// nothing and the confirm moves the family's base.
func TestSetPrimaryCheckoutPreviewsBeforeItMoves(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	ctx := context.Background()
	before := f.families(t, map[string]any{}).Families[0]

	worktreePrefix, status := trackRepoPrefix(t, f.srv, map[string]any{"path": f.worktree})
	require.Equal(t, "tracked", status)
	require.NotEqual(t, f.prefix, worktreePrefix, "the worktree gets a corpus of its own")

	var preview struct {
		Status          string `json:"status"`
		Ready           bool   `json:"ready"`
		ConfirmRequired bool   `json:"confirm_required"`
		GraphID         string `json:"graph_id"`
		CurrentGraphID  string `json:"current_graph_id"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleSetPrimaryCheckout, map[string]any{"graph": worktreePrefix}),
		&preview))
	assert.Equal(t, "preview", preview.Status)
	assert.True(t, preview.ConfirmRequired)
	assert.True(t, preview.Ready)
	assert.Equal(t, before.PrimaryGraphID, preview.CurrentGraphID)
	assert.Equal(t, indexer.GraphIDFor(worktreePrefix), preview.GraphID)

	graphs, err := f.catalog.ListDedicatedGraphs(ctx, before.FamilyID)
	require.NoError(t, err)
	assert.Equal(t, before.PrimaryGraphID, primaryGraphOf(graphs), "a preview writes nothing")

	var confirmed struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleSetPrimaryCheckout,
			map[string]any{"graph": worktreePrefix, "confirm": true}),
		&confirmed))
	assert.Equal(t, "primary_set", confirmed.Status)

	graphs, err = f.catalog.ListDedicatedGraphs(ctx, before.FamilyID)
	require.NoError(t, err)
	assert.Equal(t, indexer.GraphIDFor(worktreePrefix), primaryGraphOf(graphs))
}

// TestForgetCheckoutPreviewsThenRemovesIt proves forget never runs without an
// explicit confirm, and that the confirm takes the corpus with it.
func TestForgetCheckoutPreviewsThenRemovesIt(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	ctx := context.Background()
	family := f.families(t, map[string]any{}).Families[0]

	worktreePrefix, _ := trackRepoPrefix(t, f.srv, map[string]any{"path": f.worktree})
	require.NotNil(t, f.mi.GetMetadata(worktreePrefix))

	var preview struct {
		Status          string `json:"status"`
		Plan            string `json:"plan"`
		Prefix          string `json:"prefix"`
		ConfirmRequired bool   `json:"confirm_required"`
		Closure         []struct {
			Kind string `json:"kind"`
		} `json:"closure"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleForgetCheckout, map[string]any{"path": f.worktree}),
		&preview))
	assert.Equal(t, "preview", preview.Status)
	assert.Equal(t, "forget", preview.Plan)
	assert.True(t, preview.ConfirmRequired)
	assert.Equal(t, worktreePrefix, preview.Prefix)
	assert.NotEmpty(t, preview.Closure, "the preview names what goes")
	require.NotNil(t, f.mi.GetMetadata(worktreePrefix), "a preview writes nothing")

	var confirmed struct {
		Status string `json:"status"`
		Plan   string `json:"plan"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleForgetCheckout,
			map[string]any{"path": f.worktree, "confirm": true}),
		&confirmed))
	assert.Equal(t, "forgotten", confirmed.Status)
	assert.Equal(t, "forget", confirmed.Plan)
	assert.Nil(t, f.mi.GetMetadata(worktreePrefix), "the corpus is gone")

	checkouts, err := f.catalog.ListCheckouts(ctx, family.FamilyID)
	require.NoError(t, err)
	for _, checkout := range checkouts {
		assert.NotEqual(t, "wt", checkout.AdminName, "the identity is gone")
	}
}

// TestUntrackDemotesAWorktreeWithoutAConfirm is the other half of the untrack
// rule: a plan that keeps the checkout is not destructive, so it runs.
func TestUntrackDemotesAWorktreeWithoutAConfirm(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	ctx := context.Background()
	family := f.families(t, map[string]any{}).Families[0]

	worktreePrefix, _ := trackRepoPrefix(t, f.srv, map[string]any{"path": f.worktree})
	require.NotNil(t, f.mi.GetMetadata(worktreePrefix))

	var payload struct {
		Status  string `json:"status"`
		Plan    string `json:"plan"`
		Demoted bool   `json:"demoted"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleUntrackRepository, map[string]any{"path": f.worktree}),
		&payload))
	assert.Equal(t, "demoted", payload.Status)
	assert.Equal(t, "demote", payload.Plan)
	assert.True(t, payload.Demoted)

	checkouts, err := f.catalog.ListCheckouts(ctx, family.FamilyID)
	require.NoError(t, err)
	found := false
	for _, checkout := range checkouts {
		if checkout.AdminName != "wt" {
			continue
		}
		found = true
		assert.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)
	}
	assert.True(t, found, "the demoted checkout keeps its identity")
}

// TestReconcileCheckoutsReportsWhatItDecided covers both scopes of the
// force-reconcile verb.
func TestReconcileCheckoutsReportsWhatItDecided(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	family := f.families(t, map[string]any{}).Families[0]

	var all struct {
		Status   string `json:"status"`
		Families []struct {
			FamilyID        string `json:"family_id"`
			InventoryUsable bool   `json:"inventory_usable"`
			PrimaryGraphID  string `json:"primary_graph_id"`
			Checkouts       []struct {
				AdminName string `json:"admin_name"`
				Action    string `json:"action"`
			} `json:"checkouts"`
		} `json:"families"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts, map[string]any{}), &all))
	assert.Equal(t, "reconciled", all.Status)
	require.Len(t, all.Families, 1)
	assert.Equal(t, family.FamilyID, all.Families[0].FamilyID)
	assert.True(t, all.Families[0].InventoryUsable)
	assert.Equal(t, family.PrimaryGraphID, all.Families[0].PrimaryGraphID)
	assert.Len(t, all.Families[0].Checkouts, 2)

	var one struct {
		Families []struct {
			FamilyID string `json:"family_id"`
		} `json:"families"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts, map[string]any{"family": f.worktree}), &one))
	require.Len(t, one.Families, 1)
	assert.Equal(t, family.FamilyID, one.Families[0].FamilyID)
}

// TestReconcileOneFamilyReportsItsLiveCoordinators pins the count the verb
// renders as "%d coordinators live". A scope that leaves it out of the answer
// renders a daemon running build loops as one running none.
func TestReconcileOneFamilyReportsItsLiveCoordinators(t *testing.T) {
	f := newCheckoutAdminFixture(t)
	family := f.families(t, map[string]any{}).Families[0]

	// The worktree is dormant until selected; wake it so there is a live
	// coordinator for the reconcile to count.
	f.activateWorktree(t, family.FamilyID)

	var all struct {
		Coordinators int `json:"coordinators"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts, map[string]any{}), &all))
	require.Positive(t, all.Coordinators, "the linked worktree runs no coordinator to count")

	var one struct {
		Coordinators int `json:"coordinators"`
	}
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, f.srv.handleReconcileCheckouts,
			map[string]any{"family": family.FamilyID}), &one))
	assert.Equal(t, all.Coordinators, one.Coordinators,
		"the family holding every live coordinator reports none")
}

// explainView runs the diagnostic for one path.
func explainView(t *testing.T, srv *Server, path string) indexer.ViewBinding {
	t.Helper()
	var binding indexer.ViewBinding
	require.NoError(t, json.Unmarshal(
		callAdminTool(t, srv.handleExplainView, map[string]any{"path": path}), &binding))
	return binding
}

// seedGeneration writes one published generation so a route may name it.
func seedGeneration(t *testing.T, catalog *store_sqlite.Catalog, graphID, checkoutID, kind string) int64 {
	t.Helper()
	id, err := catalog.CreateViewGeneration(context.Background(), store_sqlite.ViewGeneration{
		OwnerKind:      "checkout",
		GraphID:        graphID,
		CheckoutID:     checkoutID,
		GenerationKind: kind,
		State:          store_sqlite.ViewGenerationReady,
	})
	require.NoError(t, err)
	return id
}

// primaryGraphOf reports which of a family's graphs is the base.
func primaryGraphOf(graphs []store_sqlite.DedicatedGraph) string {
	for _, dedicated := range graphs {
		if dedicated.IsPrimaryBase {
			return dedicated.GraphID
		}
	}
	return ""
}
