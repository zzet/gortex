package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// checkoutsDaemonTool is the daemon-tool relay seam these verbs share. It is
// indirected through a package var so tests can stub the daemon call — and
// assert the lowered tool name and arguments — without a running daemon.
//
// The verbs below are thin: every decision they render was made by the tool
// they called, which is the same tool an agent calls over MCP. A CLI that
// re-derived any of it would be a second answer to the same question.
var checkoutsDaemonTool = requireCheckoutTool

// requireCheckoutTool is requireDaemonTool with the relay path resolved away
// from a working copy whose binding is the thing being repaired — see
// checkoutsRelayPath. Without it these verbs are unreachable from exactly the
// directory that needs them.
func requireCheckoutTool(repoPath, tool string, args map[string]any) (json.RawMessage, error) {
	return requireDaemonTool(checkoutsRelayPath(repoPath), tool, args)
}

var (
	checkoutsIndex   string
	checkoutsFormat  string
	checkoutsFamily  string
	checkoutsConfirm bool
)

var reposFamiliesCmd = &cobra.Command{
	Use:   "families",
	Short: "List the checkout families the daemon tracks, with their corpora, working copies and views",
	Long: `Lists every checkout family the daemon holds in its catalog.

For each family: the primary corpus and its epoch, the dedicated graphs bound
to the family, every registered working copy with its mode, lifecycle state,
both reconciler clocks and their deadlines, the route serving it and whether a
build coordinator is live for it, and the named views rooted in its graphs.

The answer is the catalog's, not the filesystem's — nothing is stat'ed. Run
'gortex repos reconcile' for a fresh look.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runReposFamilies,
}

var reposSetPrimaryCmd = &cobra.Command{
	Use:   "set-primary <graph|prefix|path>",
	Short: "Make one corpus the base its family's automatic checkouts compose over",
	Long: `Moves a family's primary base corpus.

Without --confirm this previews the move: the incumbent, the family's epoch,
whether the move would be accepted, and every automatic checkout that has to
rebuild its layers over the new base. Nothing is written.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runReposSetPrimary,
}

var reposForgetCmd = &cobra.Command{
	Use:   "forget <path|prefix>",
	Short: "Remove a checkout, its corpus and everything rooted in it",
	Long: `Removes a checkout outright.

Unlike 'gortex untrack', forget never demotes the checkout into its family's
automatic lane — it is the deliberate removal. Without --confirm it previews
the closure and writes nothing.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runReposForget,
}

var reposReconcileCmd = &cobra.Command{
	Use:   "reconcile [family|prefix|path]",
	Short: "Reconcile checkout families against git and the filesystem now",
	Long: `Runs the reconciliation the janitor runs on its own schedule.

Identities are confirmed or allocated, the availability and removal clocks
move, and the build coordinators are brought in line with the verdicts. With no
argument every family the daemon knows is reconciled.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runReposReconcile,
}

var reposExplainViewCmd = &cobra.Command{
	Use:   "explain-view <path>",
	Short: "Explain which graph answers for a path, and why",
	Long: `Walks the binding chain for one filesystem path: the checkout the path
binds to, how that checkout is served, the route and the generations behind it
— or, when no composed view answers, the step in the chain that could not be
taken and left the base corpus to answer.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runReposExplainView,
}

func init() {
	for _, cmd := range []*cobra.Command{
		reposFamiliesCmd, reposSetPrimaryCmd, reposForgetCmd,
		reposReconcileCmd, reposExplainViewCmd,
	} {
		cmd.Flags().StringVar(&checkoutsIndex, "index", ".", "repository path the daemon must track")
		cmd.Flags().StringVar(&checkoutsIndex, "repo", ".", "alias for --index")
		cmd.Flags().StringVar(&checkoutsFormat, "format", "text", "output format: text|json")
		reposCmd.AddCommand(cmd)
	}
	reposFamiliesCmd.Flags().StringVar(&checkoutsFamily, "family", "",
		"narrow to one family: a family id, a graph id, a repo prefix, or a path inside a tracked repository")
	reposSetPrimaryCmd.Flags().BoolVar(&checkoutsConfirm, "confirm", false,
		"run the move instead of previewing it")
	reposForgetCmd.Flags().BoolVar(&checkoutsConfirm, "confirm", false,
		"run the removal instead of previewing it")
}

func runReposFamilies(cmd *cobra.Command, _ []string) error {
	args := map[string]any{}
	if checkoutsFamily != "" {
		args["family"] = checkoutsFamily
	}
	raw, err := checkoutsDaemonTool(checkoutsIndex, "list_checkouts", args)
	if err != nil {
		return err
	}
	if checkoutsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload familiesPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	renderFamilies(cmd.OutOrStdout(), payload)
	return nil
}

func runReposSetPrimary(cmd *cobra.Command, args []string) error {
	toolArgs := map[string]any{"graph": args[0]}
	if checkoutsConfirm {
		toolArgs["confirm"] = true
	}
	raw, err := checkoutsDaemonTool(checkoutsIndex, "set_primary_checkout", toolArgs)
	if err != nil {
		return err
	}
	if checkoutsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload setPrimaryPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	renderSetPrimary(cmd.OutOrStdout(), payload)
	return nil
}

func runReposForget(cmd *cobra.Command, args []string) error {
	toolArgs := map[string]any{"path": args[0]}
	if checkoutsConfirm {
		toolArgs["confirm"] = true
	}
	raw, err := checkoutsDaemonTool(checkoutsIndex, "forget_checkout", toolArgs)
	if err != nil {
		return err
	}
	if checkoutsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload checkoutOutcome
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Status == "" {
		// Anything that is not one of this tool's own answers — the
		// not-tracked guidance, a shape a newer daemon added — is printed as
		// it came rather than squeezed into a summary.
		return emitDaemonJSON(cmd, raw)
	}
	renderCheckoutOutcome(cmd.OutOrStdout(), args[0], payload,
		"gortex repos forget "+args[0]+" --confirm")
	return nil
}

func runReposReconcile(cmd *cobra.Command, args []string) error {
	toolArgs := map[string]any{}
	if len(args) == 1 {
		toolArgs["family"] = args[0]
	}
	raw, err := checkoutsDaemonTool(checkoutsIndex, "reconcile_checkouts", toolArgs)
	if err != nil {
		return err
	}
	if checkoutsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload reconcilePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	renderReconcile(cmd.OutOrStdout(), payload)
	return nil
}

func runReposExplainView(cmd *cobra.Command, args []string) error {
	raw, err := checkoutsDaemonTool(checkoutsIndex, "explain_view", map[string]any{"path": args[0]})
	if err != nil {
		return err
	}
	if checkoutsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload viewBindingPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	renderViewBinding(cmd.OutOrStdout(), payload)
	return nil
}

// --- payloads -----------------------------------------------------------
//
// The shapes below mirror what the tools emit. They are deliberately partial:
// a field the renderer does not print is not decoded, and an unknown field a
// newer daemon adds is ignored rather than failing the verb.

type familiesPayload struct {
	Families              []familyPayload `json:"families"`
	TruncatedByBudget     bool            `json:"_truncated_by_budget"`
	MaxReturnedFamilies   int             `json:"_max_returned_families"`
	OriginalCountFamilies int             `json:"_original_count_families"`
}

type familyPayload struct {
	FamilyID          string            `json:"family_id"`
	CommonDir         string            `json:"common_dir"`
	State             string            `json:"state"`
	PrimaryEpoch      int64             `json:"primary_epoch"`
	PrimaryGraphID    string            `json:"primary_graph_id"`
	PrimaryRepoPrefix string            `json:"primary_repo_prefix"`
	Graphs            []graphPayload    `json:"graphs"`
	Checkouts         []checkoutPayload `json:"checkouts"`
	RefViews          []refViewPayload  `json:"ref_views"`

	TruncatedByBudget      bool `json:"_truncated_by_budget"`
	MaxReturnedGraphs      int  `json:"_max_returned_graphs"`
	OriginalCountGraphs    int  `json:"_original_count_graphs"`
	MaxReturnedCheckouts   int  `json:"_max_returned_checkouts"`
	OriginalCountCheckouts int  `json:"_original_count_checkouts"`
	MaxReturnedRefViews    int  `json:"_max_returned_ref_views"`
	OriginalCountRefViews  int  `json:"_original_count_ref_views"`
}

type graphPayload struct {
	GraphID            string `json:"graph_id"`
	RepoPrefix         string `json:"repo_prefix"`
	IsPrimary          bool   `json:"is_primary"`
	State              string `json:"state"`
	ActiveGenerationID int64  `json:"active_generation_id"`
	Served             bool   `json:"served"`
}

type checkoutPayload struct {
	CheckoutID      string        `json:"checkout_id"`
	AdminName       string        `json:"admin_name"`
	RootPath        string        `json:"root_path"`
	State           string        `json:"state"`
	DesiredMode     string        `json:"desired_mode"`
	EffectiveMode   string        `json:"effective_mode"`
	HeadRef         string        `json:"head_ref"`
	HeadCommit      string        `json:"head_commit"`
	GraphID         string        `json:"graph_id"`
	CoordinatorLive bool          `json:"coordinator_live"`
	Intents         []string      `json:"intents"`
	Transition      string        `json:"transition"`
	Availability    clockPayload  `json:"availability"`
	Removal         clockPayload  `json:"removal"`
	Evidence        evidencePyld  `json:"evidence"`
	Route           *routePayload `json:"route"`
}

type clockPayload struct {
	StartedAt int64  `json:"started_at"`
	Deadline  int64  `json:"deadline"`
	Evidence  string `json:"evidence"`
	Running   bool   `json:"running"`
}

type evidencePyld struct {
	Present          bool  `json:"present"`
	SampledAt        int64 `json:"sampled_at"`
	SampleGeneration int64 `json:"sample_generation"`
}

type routePayload struct {
	GraphID            string `json:"graph_id"`
	CommitGenerationID int64  `json:"commit_generation_id"`
	DirtyGenerationID  int64  `json:"dirty_generation_id"`
	RouteEpoch         int64  `json:"route_epoch"`
	State              string `json:"state"`
	Ready              bool   `json:"ready"`
}

type refViewPayload struct {
	SelectorKind  string `json:"selector_kind"`
	SelectorValue string `json:"selector_value"`
	State         string `json:"state"`
	ActiveTree    string `json:"active_tree"`
	DesiredTree   string `json:"desired_tree"`
	LastSelected  int64  `json:"last_selected"`
}

type dependentPayload struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// checkoutOutcome is one untrack or forget answer, previewed or executed.
type checkoutOutcome struct {
	Status          string             `json:"status"`
	Plan            string             `json:"plan"`
	Prefix          string             `json:"prefix"`
	CheckoutID      string             `json:"checkout_id"`
	GraphID         string             `json:"graph_id"`
	IsPrimary       bool               `json:"is_primary"`
	SolePrimary     bool               `json:"sole_primary"`
	ConfirmRequired bool               `json:"confirm_required"`
	Detail          string             `json:"detail"`
	Closure         []dependentPayload `json:"closure"`
	Preserved       []dependentPayload `json:"preserved"`
	Blockers        []string           `json:"blockers"`
	Demoted         bool               `json:"demoted"`
	NodesRemoved    int                `json:"nodes_removed"`
	EdgesRemoved    int                `json:"edges_removed"`
	RevokedIntents  []string           `json:"revoked_intents"`
}

type setPrimaryPayload struct {
	Status            string             `json:"status"`
	FamilyID          string             `json:"family_id"`
	GraphID           string             `json:"graph_id"`
	RepoPrefix        string             `json:"repo_prefix"`
	CurrentGraphID    string             `json:"current_graph_id"`
	CurrentRepoPrefix string             `json:"current_repo_prefix"`
	PrimaryEpoch      int64              `json:"primary_epoch"`
	Ready             bool               `json:"ready"`
	ConfirmRequired   bool               `json:"confirm_required"`
	Blockers          []string           `json:"blockers"`
	Dependents        []dependentPayload `json:"dependents"`
	Rebuilt           []string           `json:"rebuilt"`
	Stale             []string           `json:"stale"`
	Errors            []string           `json:"errors"`
}

type reconcilePayload struct {
	Status          string                   `json:"status"`
	Families        []reconcileFamilyPayload `json:"families"`
	Removed         int                      `json:"removed"`
	Coordinators    int                      `json:"coordinators"`
	Retired         int                      `json:"retired"`
	RefViewsRetired int                      `json:"ref_views_retired"`
}

type reconcileFamilyPayload struct {
	FamilyID        string                     `json:"family_id"`
	CommonDir       string                     `json:"common_dir"`
	InventoryUsable bool                       `json:"inventory_usable"`
	PrimaryGraphID  string                     `json:"primary_graph_id"`
	Code            string                     `json:"code"`
	Checkouts       []reconcileCheckoutPayload `json:"checkouts"`
}

type reconcileCheckoutPayload struct {
	AdminName   string `json:"admin_name"`
	RootPath    string `json:"root_path"`
	Action      string `json:"action"`
	State       string `json:"state"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

type viewBindingPayload struct {
	Path            string        `json:"path"`
	Matched         bool          `json:"matched"`
	FamilyID        string        `json:"family_id"`
	CheckoutID      string        `json:"checkout_id"`
	AdminName       string        `json:"admin_name"`
	RootPath        string        `json:"root_path"`
	CheckoutState   string        `json:"checkout_state"`
	EffectiveMode   string        `json:"effective_mode"`
	GraphID         string        `json:"graph_id"`
	RepoPrefix      string        `json:"repo_prefix"`
	PrimaryGraphID  string        `json:"primary_graph_id"`
	Route           *routePayload `json:"route"`
	CoordinatorLive bool          `json:"coordinator_live"`
	Composed        bool          `json:"composed"`
	Reason          string        `json:"reason"`
	Chain           []string      `json:"chain"`
}

// --- renderers ----------------------------------------------------------

func renderFamilies(w io.Writer, payload familiesPayload) {
	if len(payload.Families) == 0 {
		if payload.TruncatedByBudget {
			if payload.OriginalCountFamilies > 0 {
				fmt.Fprintf(w, "(checkout family listing truncated by response budget: showing %d of %d checkout families)\n",
					payload.MaxReturnedFamilies, payload.OriginalCountFamilies)
			} else {
				fmt.Fprintln(w, "(checkout family listing truncated by response budget; no family row fit)")
			}
			return
		}
		fmt.Fprintln(w, "(no checkout families)")
		return
	}
	for i, family := range payload.Families {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "family %s  %s  (%s)\n", family.FamilyID, family.State, family.CommonDir)
		if family.PrimaryGraphID != "" {
			fmt.Fprintf(w, "  primary: %s  %s  epoch %d\n",
				family.PrimaryRepoPrefix, family.PrimaryGraphID, family.PrimaryEpoch)
		} else {
			fmt.Fprintf(w, "  primary: (none)  epoch %d\n", family.PrimaryEpoch)
		}
		for _, graph := range family.Graphs {
			fmt.Fprintf(w, "  graph  %-24s %s  %s  primary=%t served=%t generation=%d\n",
				graph.RepoPrefix, graph.GraphID, graph.State,
				graph.IsPrimary, graph.Served, graph.ActiveGenerationID)
		}
		for _, checkout := range family.Checkouts {
			renderCheckoutRow(w, checkout)
		}
		for _, view := range family.RefViews {
			fmt.Fprintf(w, "  view   %s:%s  %s  active_tree=%s desired_tree=%s last_selected=%s\n",
				view.SelectorKind, view.SelectorValue, view.State,
				orNone(view.ActiveTree), orNone(view.DesiredTree), unixCell(view.LastSelected))
		}
		renderFamilyBudgetNotices(w, family)
	}
	if payload.TruncatedByBudget && payload.OriginalCountFamilies > payload.MaxReturnedFamilies {
		fmt.Fprintf(w, "\nresponse budget: showing %d of %d checkout families\n",
			payload.MaxReturnedFamilies, payload.OriginalCountFamilies)
	}
}

func renderFamilyBudgetNotices(w io.Writer, family familyPayload) {
	if !family.TruncatedByBudget {
		return
	}
	renderFamilyBudgetNotice(w, "graphs", family.MaxReturnedGraphs, family.OriginalCountGraphs)
	renderFamilyBudgetNotice(w, "checkouts", family.MaxReturnedCheckouts, family.OriginalCountCheckouts)
	renderFamilyBudgetNotice(w, "ref views", family.MaxReturnedRefViews, family.OriginalCountRefViews)
}

func renderFamilyBudgetNotice(w io.Writer, label string, returned, original int) {
	if original <= returned {
		return
	}
	fmt.Fprintf(w, "  response budget: showing %d of %d %s\n", returned, original, label)
}

func renderCheckoutRow(w io.Writer, checkout checkoutPayload) {
	fmt.Fprintf(w, "  checkout %-20s %s/%s  %s\n",
		checkout.AdminName, checkout.EffectiveMode, checkout.State, checkout.RootPath)
	detail := []string{"head=" + headCell(checkout.HeadRef, checkout.HeadCommit)}
	if checkout.GraphID != "" {
		detail = append(detail, "graph="+checkout.GraphID)
	}
	if checkout.DesiredMode != checkout.EffectiveMode {
		detail = append(detail, "desired="+checkout.DesiredMode)
	}
	detail = append(detail, fmt.Sprintf("coordinator=%t", checkout.CoordinatorLive))
	if len(checkout.Intents) > 0 {
		detail = append(detail, "intents="+strings.Join(checkout.Intents, ","))
	}
	if checkout.Transition != "" {
		detail = append(detail, "transition="+checkout.Transition)
	}
	fmt.Fprintf(w, "           %s\n", strings.Join(detail, "  "))
	if checkout.Route != nil {
		fmt.Fprintf(w, "           route=%s epoch=%d %s commit=%d dirty=%d ready=%t\n",
			checkout.Route.GraphID, checkout.Route.RouteEpoch, checkout.Route.State,
			checkout.Route.CommitGenerationID, checkout.Route.DirtyGenerationID, checkout.Route.Ready)
	}
	if checkout.Availability.Running {
		fmt.Fprintf(w, "           availability: since %s deadline %s\n",
			unixCell(checkout.Availability.StartedAt), unixCell(checkout.Availability.Deadline))
	}
	if checkout.Removal.Running {
		fmt.Fprintf(w, "           removal: since %s deadline %s evidence %s\n",
			unixCell(checkout.Removal.StartedAt), unixCell(checkout.Removal.Deadline),
			orNone(checkout.Removal.Evidence))
	}
	if checkout.Evidence.Present {
		fmt.Fprintf(w, "           evidence: sampled %s generation %d\n",
			unixCell(checkout.Evidence.SampledAt), checkout.Evidence.SampleGeneration)
	}
}

// renderCheckoutOutcome prints one untrack or forget answer. rerun is the
// command that carries the same request through, which is what a preview is
// for: the caller reads the closure and then runs exactly that.
func renderCheckoutOutcome(w io.Writer, target string, payload checkoutOutcome, rerun string) {
	if payload.Status == "preview" {
		fmt.Fprintf(w, "preview: %s would run the %q plan\n", target, payload.Plan)
	} else {
		fmt.Fprintf(w, "%s: %s (%s plan)\n", payload.Status, target, payload.Plan)
	}
	if payload.Prefix != "" {
		fmt.Fprintf(w, "  corpus: %s\n", payload.Prefix)
	}
	if payload.IsPrimary {
		fmt.Fprintf(w, "  primary: yes  sole=%t\n", payload.SolePrimary)
	}
	renderDependentList(w, "removes", payload.Closure)
	renderDependentList(w, "keeps", payload.Preserved)
	for _, blocker := range payload.Blockers {
		fmt.Fprintf(w, "  blocked: %s\n", blocker)
	}
	if payload.Status == "preview" {
		if payload.Detail != "" {
			fmt.Fprintf(w, "  %s\n", payload.Detail)
		}
		fmt.Fprintf(w, "  run: %s\n", rerun)
		return
	}
	if len(payload.RevokedIntents) > 0 {
		fmt.Fprintf(w, "  revoked intents: %s\n", strings.Join(payload.RevokedIntents, ", "))
	}
	if payload.NodesRemoved > 0 || payload.EdgesRemoved > 0 {
		fmt.Fprintf(w, "  removed %d nodes, %d edges\n", payload.NodesRemoved, payload.EdgesRemoved)
	}
}

func renderDependentList(w io.Writer, label string, dependents []dependentPayload) {
	for _, dep := range dependents {
		fmt.Fprintf(w, "  %s %s: %s\n", label, dep.Kind, dep.Detail)
	}
}

func renderSetPrimary(w io.Writer, payload setPrimaryPayload) {
	if payload.Status == "preview" {
		fmt.Fprintf(w, "preview: %s would become the primary corpus of family %s\n",
			payload.RepoPrefix, payload.FamilyID)
	} else {
		fmt.Fprintf(w, "primary set: %s is the base corpus of family %s\n",
			payload.RepoPrefix, payload.FamilyID)
	}
	if payload.CurrentGraphID != "" {
		fmt.Fprintf(w, "  currently: %s  %s\n", payload.CurrentRepoPrefix, payload.CurrentGraphID)
	}
	fmt.Fprintf(w, "  graph: %s  epoch %d\n", payload.GraphID, payload.PrimaryEpoch)
	for _, dep := range payload.Dependents {
		fmt.Fprintf(w, "  rebuilds %s: %s\n", dep.Kind, dep.Detail)
	}
	for _, blocker := range payload.Blockers {
		fmt.Fprintf(w, "  blocked: %s\n", blocker)
	}
	if payload.Status == "preview" {
		fmt.Fprintf(w, "  ready: %t\n", payload.Ready)
		fmt.Fprintln(w, "  run: gortex repos set-primary "+payload.GraphID+" --confirm")
		return
	}
	if len(payload.Rebuilt) > 0 {
		fmt.Fprintf(w, "  rebuilt %d checkouts\n", len(payload.Rebuilt))
	}
	for _, stale := range payload.Stale {
		fmt.Fprintf(w, "  stale: checkout %s kept its old route\n", stale)
	}
	for _, e := range payload.Errors {
		fmt.Fprintf(w, "  error: %s\n", e)
	}
}

func renderReconcile(w io.Writer, payload reconcilePayload) {
	if len(payload.Families) == 0 {
		fmt.Fprintln(w, "(no families reconciled)")
		return
	}
	for _, family := range payload.Families {
		fmt.Fprintf(w, "family %s  inventory_usable=%t  primary=%s\n",
			family.FamilyID, family.InventoryUsable, orNone(family.PrimaryGraphID))
		if family.Code != "" {
			fmt.Fprintf(w, "  code: %s\n", family.Code)
		}
		for _, checkout := range family.Checkouts {
			fmt.Fprintf(w, "  %-20s %-26s %s\n",
				checkout.AdminName, checkout.Action, checkout.RootPath)
			if checkout.Detail != "" {
				fmt.Fprintf(w, "    %s\n", checkout.Detail)
			}
		}
	}
	fmt.Fprintf(w, "\n%d families, %d removed, %d coordinators live, %d generations retired, %d view generations retired\n",
		len(payload.Families), payload.Removed, payload.Coordinators,
		payload.Retired, payload.RefViewsRetired)
}

func renderViewBinding(w io.Writer, payload viewBindingPayload) {
	fmt.Fprintf(w, "path: %s\n", payload.Path)
	if payload.Matched {
		fmt.Fprintf(w, "checkout: %s (%s)  %s/%s\n",
			payload.AdminName, payload.CheckoutID, payload.EffectiveMode, payload.CheckoutState)
		fmt.Fprintf(w, "family: %s\n", payload.FamilyID)
	} else {
		fmt.Fprintln(w, "checkout: (none)")
	}
	if payload.GraphID != "" || payload.RepoPrefix != "" {
		fmt.Fprintf(w, "answers from: %s  %s\n", orNone(payload.RepoPrefix), orNone(payload.GraphID))
	}
	if payload.Route != nil {
		fmt.Fprintf(w, "route: %s epoch=%d %s commit=%d dirty=%d ready=%t\n",
			payload.Route.GraphID, payload.Route.RouteEpoch, payload.Route.State,
			payload.Route.CommitGenerationID, payload.Route.DirtyGenerationID, payload.Route.Ready)
	}
	fmt.Fprintf(w, "composed: %t  coordinator: %t\n", payload.Composed, payload.CoordinatorLive)
	if payload.Reason != "" {
		fmt.Fprintf(w, "reason: %s\n", payload.Reason)
	}
	for _, step := range payload.Chain {
		fmt.Fprintf(w, "  - %s\n", step)
	}
}

// unixCell renders a unix timestamp for a table cell.
func unixCell(seconds int64) string {
	if seconds <= 0 {
		return "(never)"
	}
	return time.Unix(seconds, 0).Local().Format("2006-01-02 15:04:05")
}

// headCell renders where a checkout's HEAD sits. A detached HEAD has no ref to
// name, so the commit the sample resolved is its identity — reporting "(none)"
// there would read as a checkout whose HEAD was never sampled at all.
func headCell(ref, commit string) string {
	if ref != "" {
		return ref
	}
	if commit != "" {
		return "detached@" + shortSHA(commit)
	}
	return "(none)"
}

// orNone renders an empty string as an explicit placeholder, so a blank column
// is never mistaken for a missing one.
func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}
