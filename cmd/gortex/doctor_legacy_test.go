package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zzet/gortex/internal/indexer"
)

// v7At mints a UUIDv7 whose embedded timestamp is t, so a fixture can place a
// checkout row and an intent on either side of a boot boundary.
func v7At(t *testing.T, when time.Time) string {
	t.Helper()
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	// uuid.NewV7 does not take a timestamp; re-stamp the 48 timestamp bits
	// from when so the decoded gap is the one this test intends.
	ms := when.UnixMilli()
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}

// TestLegacyStateClassification is the unit fixture the issue asks for: N
// entries with real intents, one tracked while the daemon was offline (only
// manual_config, same timestamps), one simulated leak (checkout at T, intent at
// T+1h), and one config entry with no intent at all.
func TestLegacyStateClassification(t *testing.T) {
	base := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

	// Two entries the user really asked for: checkout and intent seconds apart.
	cliOut := "/nonexistent/repos/cli-tracked"
	mcpOut := "/nonexistent/repos/mcp-tracked"
	// Tracked while the daemon was down: manual_config only, same timestamps.
	offlineOut := "/nonexistent/repos/offline-tracked"
	// The simulated leak: checkout row on one boot, manual_config the next.
	leakOut := "/nonexistent/repos/unattributed"
	// In config.yaml, absent from the catalog entirely.
	missingOut := "/nonexistent/repos/intentless"

	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{
		{
			FamilyID:  "fam-1",
			CommonDir: "/nonexistent/repos/.git",
			Checkouts: []indexer.CheckoutOverview{
				{CheckoutID: v7At(t, base), RootPath: cliOut, Intents: []string{"cli_track"},
					IntentDetails: []indexer.IntentOverview{{IntentID: v7At(t, base.Add(2*time.Second)), SourceKind: "cli_track"}}},
				{CheckoutID: v7At(t, base), RootPath: mcpOut, Intents: []string{"mcp_track"},
					IntentDetails: []indexer.IntentOverview{{IntentID: v7At(t, base.Add(3*time.Second)), SourceKind: "mcp_track"}}},
				{CheckoutID: v7At(t, base), RootPath: offlineOut, Intents: []string{"manual_config"},
					IntentDetails: []indexer.IntentOverview{{IntentID: v7At(t, base), SourceKind: "manual_config"}}},
				{CheckoutID: v7At(t, base), RootPath: leakOut, Intents: []string{"manual_config"},
					IntentDetails: []indexer.IntentOverview{{IntentID: v7At(t, base.Add(time.Hour)), SourceKind: "manual_config"}}},
			},
		},
	}}

	unattributed, intentless := classifyConfiguredRepos(overview,
		[]string{cliOut, mcpOut, offlineOut, leakOut, missingOut})

	if len(intentless) != 1 || intentless[0].Path != doctorPath(missingOut) {
		t.Fatalf("intentless = %+v, want exactly the config entry with no checkout", intentless)
	}

	byPath := map[string]DoctorLegacyEntry{}
	for _, e := range unattributed {
		byPath[e.Path] = e
	}
	if len(unattributed) != 2 {
		t.Fatalf("unattributed = %+v, want the offline entry and the leak only", unattributed)
	}
	if _, flagged := byPath[doctorPath(cliOut)]; flagged {
		t.Error("a cli_track entry must never be flagged unattributed")
	}
	if _, flagged := byPath[doctorPath(mcpOut)]; flagged {
		t.Error("an mcp_track entry must never be flagged unattributed")
	}

	offline, ok := byPath[doctorPath(offlineOut)]
	if !ok {
		t.Fatalf("offline entry missing from unattributed: %+v", unattributed)
	}
	if offline.LikelyAutoIndexed {
		t.Error("an entry whose intent shares its checkout's timestamp is not likely auto-indexed")
	}

	leak, ok := byPath[doctorPath(leakOut)]
	if !ok {
		t.Fatalf("leaked entry missing from unattributed: %+v", unattributed)
	}
	if !leak.LikelyAutoIndexed {
		t.Errorf("leak (intent 1h after checkout) must be likely auto-indexed; gap=%q", leak.Gap)
	}
	if leak.Gap == "" {
		t.Error("a measured gap must be reported so the reader can judge the heuristic")
	}
}

// TestUuidV7Time proves the timestamp decode the leak heuristic rests on, and
// that a non-v7 or malformed id is refused rather than silently dated to 1970.
func TestUuidV7Time(t *testing.T) {
	want := time.Date(2026, 11, 12, 7, 30, 0, 0, time.UTC)
	got, ok := uuidV7Time(v7At(t, want))
	if !ok {
		t.Fatal("a minted v7 id must decode")
	}
	if !got.Equal(want) {
		t.Errorf("decoded %v, want %v", got, want)
	}
	if _, ok := uuidV7Time("not-a-uuid"); ok {
		t.Error("a malformed id must not decode")
	}
	if _, ok := uuidV7Time(uuid.New().String()); ok {
		t.Error("a v4 id must not decode")
	}
}

// TestDedicatedWorktreeCorpora proves the listing picks dedicated worktree
// corpora and never the family's main checkout or an automatic checkout.
func TestDedicatedWorktreeCorpora(t *testing.T) {
	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID:          "fam-1",
		DisplayRemote:     "github.com/example/repo",
		PrimaryGraphID:    "graph-main",
		PrimaryRepoPrefix: "repo",
		Graphs: []indexer.GraphOverview{
			{GraphID: "graph-main", RepoPrefix: "repo", IsPrimary: true, State: "ready"},
			{GraphID: "graph-wt1", RepoPrefix: "repo~wt1", State: "ready"},
			{GraphID: "graph-wt2", RepoPrefix: "repo~wt2", State: "ready"},
		},
		Checkouts: []indexer.CheckoutOverview{
			// The family's main checkout: not a linked worktree.
			{CheckoutID: "c0", RootPath: "/r/repo", AdminName: "", EffectiveMode: "dedicated", GraphID: "graph-main"},
			{CheckoutID: "c1", RootPath: "/r/wt1", AdminName: "wt1", EffectiveMode: "dedicated", GraphID: "graph-wt1", CoordinatorLive: true},
			// A dedicated corpus that became the family primary.
			{CheckoutID: "c2", RootPath: "/r/wt2", AdminName: "wt2", EffectiveMode: "dedicated", GraphID: "graph-wt2"},
			// An automatic checkout composing over the primary: not a corpus.
			{CheckoutID: "c3", RootPath: "/r/wt3", AdminName: "wt3", EffectiveMode: "automatic"},
		},
	}}}

	got := dedicatedWorktreeCorpora(overview)
	if len(got) != 2 {
		t.Fatalf("dedicated corpora = %d (%+v), want 2 linked worktrees", len(got), got)
	}
	if got[0].Root == doctorPath("/r/repo") {
		t.Error("the family's main checkout must never be listed as a dedicated worktree corpus")
	}
	if !got[0].CoordinatorLive {
		t.Error("a live coordinator must be reported")
	}
}

// TestPrintDoctorLegacyDaemonDown proves the not-checked line renders and that
// the section stays silent about state it could not read.
func TestPrintDoctorLegacyDaemonDown(t *testing.T) {
	var buf bytes.Buffer
	printDoctorLegacy(&buf, DoctorLegacyState{Skipped: "daemon not running; legacy state not checked"})
	out := buf.String()
	if !strings.Contains(out, "daemon not running; legacy state not checked") {
		t.Errorf("missing the not-checked line:\n%s", out)
	}
	if strings.Contains(out, "dedicated worktree corpora") {
		t.Errorf("must not claim findings when the check did not run:\n%s", out)
	}
}

// TestPrintDoctorLegacyFindings renders a populated section and asserts every
// finding carries a fix the user can run today, which is what makes the
// listing actionable rather than alarming.
func TestPrintDoctorLegacyFindings(t *testing.T) {
	var buf bytes.Buffer
	printDoctorLegacy(&buf, DoctorLegacyState{
		Checked: true,
		DedicatedCorpora: []DoctorLegacyCorpus{{
			Family: "github.com/example/repo", Root: "<repo>/wt1",
			RepoPrefix: "repo~wt1", GraphID: "g1", GraphState: "ready", CoordinatorLive: true,
			Intents: []string{"cli_track"},
		}},
		Unattributed: []DoctorLegacyEntry{{Path: "<repo>/unattributed", SourceKinds: []string{"manual_config"}, LikelyAutoIndexed: true, Gap: "1h0m0s"}},
		Intentless:   []DoctorLegacyEntry{{Path: "<repo>/intentless"}},
	})
	out := buf.String()
	for _, want := range []string{
		"dedicated worktree corpora",
		"likely auto-indexed",
		"no active intent",
		"`gortex untrack <path>`",
		"`gortex track <path>`",
		// Wording discipline: the section never accuses the user of a leak.
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "leaked") {
		t.Errorf("wording must say unattributed, never leaked:\n%s", out)
	}
}

// TestLegacyStateJSONShape pins the JSON key the issue names, and that an
// unchecked section says so instead of rendering an empty (and therefore
// falsely reassuring) result.
func TestLegacyStateJSONShape(t *testing.T) {
	payload := map[string]any{"legacy_state": DoctorLegacyState{Skipped: "daemon not running; legacy state not checked"}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	state, ok := back["legacy_state"]
	if !ok {
		t.Fatalf("legacy_state key missing: %s", raw)
	}
	if state["checked"] != false {
		t.Errorf("checked = %v, want false when the daemon is down", state["checked"])
	}
	if state["skipped"] == "" {
		t.Error("a skipped check must say why")
	}
}

// stubCensus replaces the census seam for one test and returns the arguments
// the collector actually sent, so a test can assert on the REQUEST and not
// only on how the response was interpreted.
func stubCensus(t *testing.T, raw string, err error) func() map[string]any {
	t.Helper()
	orig := doctorCensusTool
	origEnv := doctorLegacyEnv
	origPaths := doctorConfiguredPaths
	var got map[string]any
	t.Cleanup(func() { doctorCensusTool = orig; doctorLegacyEnv = origEnv; doctorConfiguredPaths = origPaths })
	// doctorEnvironment() does a live control handshake. Without this stub the
	// collector would take its "daemon not running" arm on a host with no
	// daemon and every assertion below would be vacuous.
	doctorLegacyEnv = func() DoctorEnvironment { return DoctorEnvironment{DaemonRunning: true} }
	doctorCensusTool = func(tool string, args map[string]any) (json.RawMessage, error) {
		if tool != "list_checkouts" {
			t.Errorf("census asked for tool %q, want list_checkouts", tool)
		}
		got = args
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	}
	return func() map[string]any { return got }
}

// TestLegacyStateCensusDisablesTruncation proves the census opts out of the
// response budget. An empty argument map let list_checkouts apply its 40,000
// byte default, whose budgeter drops nested checkout/graph rows and whole
// families -- and FamiliesOverview unmarshals silently past the missing rows.
func TestLegacyStateCensusDisablesTruncation(t *testing.T) {
	args := stubCensus(t, `{"families":[]}`, nil)
	_ = collectLegacyState(t.TempDir())

	got := args()
	if got == nil {
		t.Fatal("census was never called")
	}
	v, ok := got["max_bytes"]
	if !ok {
		t.Fatal("census omitted max_bytes, so the daemon applies its truncating default")
	}
	if n, _ := v.(int); n != 0 {
		t.Errorf("max_bytes = %v, want 0 (explicit opt-out of the byte cap)", v)
	}
}

// TestLegacyStateTruncatedCensusFailsClosed proves a trimmed listing is never
// read as a complete one. A partial inventory makes a tracked repository in
// another workspace look absent, which classification reports as intentless.
func TestLegacyStateTruncatedCensusFailsClosed(t *testing.T) {
	stubCensus(t, `{"families":[],"_truncated_by_budget":true}`, nil)
	home := t.TempDir()
	state := collectLegacyState(home)

	if state.Checked {
		t.Error("a truncated census must not be reported as checked")
	}
	if !strings.Contains(state.Skipped, "truncated") {
		t.Errorf("skip reason must name truncation, got %q", state.Skipped)
	}
	if len(state.Intentless) != 0 {
		t.Errorf("a truncated census must not produce intentless findings, got %+v", state.Intentless)
	}
}

// TestLegacyStateCensusErrorIsNotAFinding proves a daemon that refuses the
// listing is "not checked", never "nothing found".
func TestLegacyStateCensusErrorIsNotAFinding(t *testing.T) {
	stubCensus(t, "", errFakeCensus{})
	state := collectLegacyState(t.TempDir())
	if state.Checked {
		t.Error("a failed census must not be reported as checked")
	}
	if len(state.DedicatedCorpora)+len(state.Unattributed)+len(state.Intentless) != 0 {
		t.Error("a failed census must not produce findings")
	}
}

type errFakeCensus struct{}

func (errFakeCensus) Error() string { return "daemon refused list_checkouts" }

// TestLegacyStateSeparateWorkspaces proves the census is trusted as
// daemon-wide: a repository tracked in another workspace is present in the
// inventory and must be classified from that row, not reported as intentless.
func TestLegacyStateSeparateWorkspaces(t *testing.T) {
	now := time.Now()
	otherRoot := "/elsewhere/other-repo"
	overview := map[string]any{
		"families": []any{
			map[string]any{
				"family_id":      "fam-main",
				"display_remote": "github.com/acme/main",
				"state":          "ready",
				"checkouts": []any{
					map[string]any{
						"checkout_id": uuid.NewString(), "root_path": "/r/repo",
						"effective_mode": "dedicated", "graph_id": "g-main",
						"intents": []string{"cli_track"},
					},
				},
				"graphs": []any{map[string]any{"graph_id": "g-main", "state": "ready"}},
			},
			map[string]any{
				"family_id":      "fam-other",
				"display_remote": "github.com/acme/other",
				"state":          "ready",
				"checkouts": []any{
					map[string]any{
						"checkout_id": v7At(t, now), "root_path": otherRoot,
						"effective_mode": "dedicated", "graph_id": "g-other",
						"intents": []string{"cli_track"},
					},
				},
				"graphs": []any{map[string]any{"graph_id": "g-other", "state": "ready"}},
			},
		},
	}
	raw, _ := json.Marshal(overview)
	stubCensus(t, string(raw), nil)

	// A daemon-wide config lists both repos, including the one in the other
	// workspace. Stub the reader: the real one resolves the platform config
	// path and would answer from the developer's own config.
	doctorConfiguredPaths = func(string) []string { return []string{"/r/repo", otherRoot} }

	state := collectLegacyState(t.TempDir())
	if !state.Checked {
		t.Fatalf("complete census must be checked, skipped=%q", state.Skipped)
	}
	for _, e := range state.Intentless {
		if e.Path == doctorPath(otherRoot) {
			t.Errorf("%s is tracked in another workspace and must not be intentless", otherRoot)
		}
	}
}

// TestDedicatedCorporaRedactIdentity proves --redact covers the family remote
// and the repo prefix, not just the root path. The issue requires prefix/path
// redaction; a redacted report that still prints `github.com/acme/private`
// has disclosed the repository it was supposed to hide.
func TestDedicatedCorporaRedactIdentity(t *testing.T) {
	origPath, origLabel := doctorPath, doctorLabel
	t.Cleanup(func() { doctorPath, doctorLabel = origPath, origLabel })
	doctorLabel = newDoctorLabelRedactor()

	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID:      "fam",
		DisplayRemote: "github.com/acme/private-remote",
		State:         "ready",
		Graphs: []indexer.GraphOverview{
			{GraphID: "g1", RepoPrefix: "acme/private-prefix", State: "ready"},
		},
		Checkouts: []indexer.CheckoutOverview{
			{CheckoutID: "c1", RootPath: "/home/u/code/private-remote-wt", AdminName: "wt1",
				EffectiveMode: "dedicated", GraphID: "g1"},
		},
	}}}

	got := dedicatedWorktreeCorpora(overview)
	if len(got) != 1 {
		t.Fatalf("corpora = %d, want 1", len(got))
	}
	c := got[0]
	if strings.Contains(c.Family, "acme") || strings.Contains(c.Family, "private") {
		t.Errorf("family remote leaked: %q", c.Family)
	}
	if strings.Contains(c.RepoPrefix, "acme") || strings.Contains(c.RepoPrefix, "private") {
		t.Errorf("repo prefix leaked: %q", c.RepoPrefix)
	}
	if c.Family == "" || c.RepoPrefix == "" {
		t.Error("redaction must still leave a stable pseudonym, not emptiness")
	}

	// The human rendering must not reintroduce the raw prefix either.
	var buf bytes.Buffer
	printDoctorLegacy(&buf, DoctorLegacyState{Checked: true, DedicatedCorpora: got})
	if strings.Contains(buf.String(), "acme/private-prefix") {
		t.Errorf("human rendering printed the raw prefix:\n%s", buf.String())
	}
}

// TestLabelRedactorIsStable proves two findings about the same family render as
// the same pseudonym, so a redacted report still reads as a diagnosis.
func TestLabelRedactorIsStable(t *testing.T) {
	red := newDoctorLabelRedactor()
	a := red("github.com/acme/one")
	b := red("github.com/acme/one")
	c := red("github.com/acme/two")
	if a != b {
		t.Errorf("same identifier got %q then %q; redaction must be stable", a, b)
	}
	if a == c {
		t.Error("distinct identifiers must not collide")
	}
	if red("") != "" {
		t.Error("an empty identifier stays empty")
	}
}

// TestLegacyStateRootsAreRedacted covers the P2 finding that a daemon-wide
// census can name a checkout root the report has no business naming.
//
// doctorPath rewrites paths under the current checkout and under home. The
// census is daemon-wide: a root can live in a sibling home, or outside home
// entirely, and doctorPath returned those verbatim -- leaking the name of a
// repository the user never asked the report to name.
func TestLegacyStateRootsAreRedacted(t *testing.T) {
	origPath, origLabel, origRoot := doctorPath, doctorLabel, doctorRoot
	t.Cleanup(func() { doctorPath, doctorLabel, doctorRoot = origPath, origLabel, origRoot })

	// Identity on purpose: doctorPath is not the guard under test here.
	doctorPath = func(p string) string { return p }
	doctorLabel = newDoctorLabelRedactor()
	doctorRoot = newDoctorRootRedactor()

	// Roots that doctorPath leaves alone: a sibling home, an outside-home
	// path, and a path under home that still names another checkout.
	roots := []string{
		"/home/other/code/some-other-repo",
		"/srv/checkouts/another-repo",
		"/home/u/code/private-remote-wt",
	}

	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID:      "fam",
		DisplayRemote: "github.com/acme/private-remote",
		Graphs: []indexer.GraphOverview{
			{GraphID: "g1", RepoPrefix: "acme/private-prefix", State: "ready"},
		},
		Checkouts: []indexer.CheckoutOverview{
			{CheckoutID: "c1", RootPath: roots[0], AdminName: "wt1",
				EffectiveMode: "dedicated", GraphID: "g1"},
			{CheckoutID: "c2", RootPath: roots[1], AdminName: "wt2",
				EffectiveMode: "dedicated", GraphID: "g1"},
		},
	}}}

	corpora := dedicatedWorktreeCorpora(overview)
	if len(corpora) != 2 {
		t.Fatalf("corpora = %d, want 2", len(corpora))
	}

	state := DoctorLegacyState{Checked: true, DedicatedCorpora: corpora}
	redactLegacyRoots(&state)

	for i, c := range state.DedicatedCorpora {
		for _, leak := range roots {
			if strings.Contains(c.Root, leak) {
				t.Errorf("corpus %d root leaked %q: %q", i, leak, c.Root)
			}
		}
		if !strings.HasPrefix(c.Root, "<root-") {
			t.Errorf("corpus %d root is not an alias: %q", i, c.Root)
		}
	}

	// The human rendering must not reintroduce the root either.
	var buf bytes.Buffer
	printDoctorLegacy(&buf, state)
	for _, leak := range roots {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("human rendering leaked %q", leak)
		}
	}
}

// TestLegacyStateRootAliasIsStable proves two findings about the same root
// render as the same alias, so a redacted report still reads as a diagnosis.
func TestLegacyStateRootAliasIsStable(t *testing.T) {
	red := newDoctorRootRedactor()
	a := red("/home/other/code/repo")
	b := red("/home/other/code/repo")
	c := red("/home/other/code/other-repo")
	if a != b {
		t.Errorf("same root got %q then %q; the alias must be stable", a, b)
	}
	if a == c {
		t.Error("distinct roots must not collide")
	}
	if red("") != "" {
		t.Error("an empty root stays empty")
	}
}

// TestLegacyStateUnattributedIsManualConfigOnly covers the P2 finding that a
// checkout tracked purely by project_membership was reported as unattributed.
//
// project_membership is known provenance: the repository belongs to a project
// the user tracked. Reporting it as unattributed mislabelled a healthy repo
// and offered it the auto-indexed heuristic and a remediation.
func TestLegacyStateUnattributedIsManualConfigOnly(t *testing.T) {
	origPath := doctorPath
	t.Cleanup(func() { doctorPath = origPath })
	doctorPath = func(p string) string { return p }

	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID: "fam",
		Checkouts: []indexer.CheckoutOverview{
			{CheckoutID: "c1", RootPath: "/cfg/manual-only", Intents: []string{"manual_config"}},
			{CheckoutID: "c2", RootPath: "/cfg/project-only", Intents: []string{"project_membership"}},
			{CheckoutID: "c3", RootPath: "/cfg/mixed", Intents: []string{"manual_config", "project_membership"}},
			{CheckoutID: "c4", RootPath: "/cfg/cli", Intents: []string{"cli_track"}},
			{CheckoutID: "c5", RootPath: "/cfg/mcp", Intents: []string{"mcp_track"}},
		},
	}}}

	configured := []string{
		"/cfg/manual-only",
		"/cfg/project-only",
		"/cfg/mixed",
		"/cfg/cli",
		"/cfg/mcp",
	}

	unattributed, intentless := classifyConfiguredRepos(overview, configured)

	var got []string
	for _, u := range unattributed {
		got = append(got, u.Path)
	}
	if len(got) != 1 || got[0] != "/cfg/manual-only" {
		t.Errorf("unattributed = %v, want only [/cfg/manual-only]", got)
	}
	if len(intentless) != 0 {
		t.Errorf("intentless = %v, want none; every configured path has an intent", intentless)
	}
}

// TestLegacyStateCollectorRedactsRoots drives the REAL collector end to end --
// census stubbed, config stubbed, the whole collectLegacyState path -- and
// asserts that no root it reports survives in either output format.
//
// The reviewer reproduced the leak through the real collector and redactor,
// so the regression has to run through the same path: a root in a sibling
// home, and a root outside home entirely, must not appear in the JSON and
// must not appear in the human rendering.
func TestLegacyStateCollectorRedactsRoots(t *testing.T) {
	origPath, origLabel, origRoot := doctorPath, doctorLabel, doctorRoot
	t.Cleanup(func() { doctorPath, doctorLabel, doctorRoot = origPath, origLabel, origRoot })

	// The redacted run: path redactor is the report-wide one, label and root
	// are the per-run pseudonyms.
	doctorPath = newDoctorPathRedactor("/home/u/current-checkout", "/home/u")
	doctorLabel = newDoctorLabelRedactor()
	doctorRoot = newDoctorRootRedactor()

	siblingHome := "/home/other/code/sibling-repo"
	outsideHome := "/srv/checkouts/outside-repo"

	// Build the census as JSON so the roots are real values, not text
	// spliced into a literal.
	censusBytes, err := json.Marshal(map[string]any{
		"families": []any{map[string]any{
			"family_id":      "fam",
			"display_remote": "github.com/acme/private-remote",
			"state":          "ready",
			"graphs": []any{map[string]any{
				"graph_id": "g1", "repo_prefix": "acme/private-prefix", "state": "ready",
			},
				"checkouts": []any{
					map[string]any{"checkout_id": "c1", "root_path": siblingHome, "admin_name": "wt1", "effective_mode": "dedicated", "graph_id": "g1"},
					map[string]any{"checkout_id": "c2", "root_path": outsideHome, "admin_name": "wt2", "effective_mode": "dedicated", "graph_id": "g1"},
				},
			},
		}}})
	if err != nil {
		t.Fatalf("census marshal: %v", err)
	}

	stubCensus(t, string(censusBytes), nil)

	origPaths := doctorConfiguredPaths
	t.Cleanup(func() { doctorConfiguredPaths = origPaths })
	doctorConfiguredPaths = func(home string) []string { return []string{siblingHome, outsideHome} }

	state := collectLegacyState(t.TempDir())
	if !state.Checked {
		t.Fatalf("collector did not check: %s", state.Skipped)
	}
	if len(state.DedicatedCorpora) != 2 {
		t.Fatalf("corpora = %d, want 2", len(state.DedicatedCorpora))
	}
	if len(state.Unattributed) != 2 && len(state.Intentless) != 2 {
		t.Fatalf("configured roots not classified: unattributed=%d intentless=%d",
			len(state.Unattributed), len(state.Intentless))
	}

	// JSON: marshal the section exactly as doctor does.
	raw, err := json.Marshal(map[string]any{"legacy_state": state})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{siblingHome, outsideHome, "acme/private-prefix", "private-remote"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("JSON leaked %q:\n%s", leak, raw)
		}
	}

	// Human rendering.
	var buf bytes.Buffer
	printDoctorLegacy(&buf, state)
	for _, leak := range []string{siblingHome, outsideHome, "acme/private-prefix", "private-remote"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("human rendering leaked %q:\n%s", leak, buf.String())
		}
	}
}

// TestLegacyStateUnattributedAllowsDuplicateManualConfigRows covers the P2
// finding that the guard counted intent ROWS instead of classifying the
// source-kind set.
//
// CheckoutOverview.Intents carries one entry per active tracking row. The
// catalog's upsert key is (checkout_id, source_kind, source_locator), so the
// same kind can appear twice with different locators - a checkout recorded
// under an old root and then under a new one yields two manual_config
// entries. There is no CLI, MCP or project attribution, so it is still
// unattributed and the warning must still be raised.
func TestLegacyStateUnattributedAllowsDuplicateManualConfigRows(t *testing.T) {
	origPath := doctorPath
	t.Cleanup(func() { doctorPath = origPath })
	doctorPath = func(p string) string { return p }

	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID: "fam",
		Checkouts: []indexer.CheckoutOverview{
			{CheckoutID: "c1", RootPath: "/cfg/two-manual", Intents: []string{"manual_config", "manual_config"}},
			{CheckoutID: "c2", RootPath: "/cfg/three-manual", Intents: []string{"manual_config", "manual_config", "manual_config"}},
		},
	}}}

	configured := []string{"/cfg/two-manual", "/cfg/three-manual"}
	unattributed, intentless := classifyConfiguredRepos(overview, configured)

	if len(unattributed) != 2 {
		t.Errorf("unattributed = %d, want 2; duplicate manual_config rows are still unattributed", len(unattributed))
	}
	if len(intentless) != 0 {
		t.Errorf("intentless = %v, want none", intentless)
	}
}

// TestLegacyStateUnattributedExcludesMixedIntentRows keeps the earlier fix
// intact now that duplicates are allowed: a manual_config row alongside any
// attributed kind is NOT unattributed.
func TestLegacyStateUnattributedExcludesMixedIntentRows(t *testing.T) {
	origPath := doctorPath
	t.Cleanup(func() { doctorPath = origPath })
	doctorPath = func(p string) string { return p }

	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{{
		FamilyID: "fam",
		Checkouts: []indexer.CheckoutOverview{
			{CheckoutID: "c1", RootPath: "/cfg/manual-plus-project", Intents: []string{"manual_config", "project_membership"}},
			{CheckoutID: "c2", RootPath: "/cfg/manual-plus-cli", Intents: []string{"manual_config", "cli_track"}},
			{CheckoutID: "c3", RootPath: "/cfg/manual-plus-mcp", Intents: []string{"manual_config", "mcp_track"}},
			{CheckoutID: "c4", RootPath: "/cfg/manual-project-cli", Intents: []string{"manual_config", "project_membership", "cli_track"}},
		},
	}}}

	configured := []string{"/cfg/manual-plus-project", "/cfg/manual-plus-cli", "/cfg/manual-plus-mcp", "/cfg/manual-project-cli"}
	unattributed, _ := classifyConfiguredRepos(overview, configured)

	if len(unattributed) != 0 {
		t.Errorf("unattributed = %v, want none; any attributed kind disqualifies the row", unattributed)
	}
}
