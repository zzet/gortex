package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const editProbeScenario = "edit-and-probe"
const editProbeMean = 15 * time.Second
const editProbePeriod = 25 * time.Millisecond
const editProbeMaxEditors = 32
const editProbeFixedEditorModel = "fixed_editor_per_ordinal_plus_one_separate_prober_per_edit"

type editProbeStep struct {
	ProbeMode       string  `json:"probe_mode,omitempty"`
	Ordinal         int     `json:"ordinal"`
	ActivateNS      int64   `json:"activate_ns"`
	DueNS           int64   `json:"due_ns"`
	Editor          int     `json:"editor,omitempty"`
	Route           string  `json:"route,omitempty"`
	Marker          string  `json:"marker"`
	MutationID      string  `json:"mutation_id"`
	Request         request `json:"request"`
	RequestHash     string  `json:"request_sha256"`
	BeforeSymbol    string  `json:"before_symbol,omitempty"`
	Deleted         bool    `json:"deleted,omitempty"`
	TargetSymbol    string  `json:"target_symbol,omitempty"`
	FollowingSymbol string  `json:"following_symbol,omitempty"`
	ExpectedLineGap int     `json:"expected_line_gap,omitempty"`
	Relocated       bool    `json:"relocated,omitempty"`
	DestinationFile string  `json:"destination_file,omitempty"`
	ExpectedRefusal bool    `json:"expected_refusal,omitempty"`
	Sender          string  `json:"sender,omitempty"`
	After           *int    `json:"after,omitempty"`
	AfterEvent      string  `json:"after_event,omitempty"`
	File            string  `json:"file,omitempty"`
}

type editProbeRoute struct {
	ID       string `json:"id"`
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	File     string `json:"file"`
	Callee   string `json:"callee"`
	Kind     string `json:"kind"`
	Tool     string `json:"tool"`
	Language string `json:"language"`
	Mode     string `json:"mode"`
}

type editProbePlan struct {
	GateModes       bool             `json:"gate_modes,omitempty"`
	Version         int              `json:"version"`
	Arrival         string           `json:"arrival"`
	MeanNS          int64            `json:"mean_interarrival_ns"`
	ProbePeriodNS   int64            `json:"probe_period_ns"`
	Count           int              `json:"count"`
	Editors         int              `json:"editors,omitempty"`
	Readers         int              `json:"readers,omitempty"`
	NoEditWindow    int64            `json:"no_edit_window_ns,omitempty"`
	Seed            int64            `json:"seed"`
	Kind            string           `json:"kind"`
	Tool            string           `json:"tool"`
	Language        string           `json:"language"`
	Mode            string           `json:"mode"`
	Sender          string           `json:"sender,omitempty"`
	Repo            string           `json:"repo"`
	Commit          string           `json:"commit"`
	File            string           `json:"file"`
	Callee          string           `json:"callee"`
	SessionModel    string           `json:"session_model"`
	NotSelected     []string         `json:"not_selected_not_tested"`
	Steps           []editProbeStep  `json:"steps"`
	Routes          []editProbeRoute `json:"routes,omitempty"`
	LinkedWorktrees int              `json:"linked_worktrees,omitempty"`
}

type editProbeArrival struct {
	DueNS  int64
	Editor int
}

// Each editor offers its own open-loop Poisson stream with the single-editor
// mean. Editor 0 keeps the original seeding, so one editor offers exactly the
// original arrivals. Streams merge by due time (ties by editor) and the merge
// order is the global ordinal: every ordinal belongs to one fixed editor.
func editProbeArrivals(seed int64, count, editors int, mean time.Duration) ([]editProbeArrival, error) {
	if mean <= 0 {
		return nil, errors.New("edit offered mean must be positive")
	}
	var merged []editProbeArrival
	for editor := range editors {
		rng := rand.New(rand.NewPCG(uint64(seed)^0xa0761d6478bd642f^uint64(editor)*0x9e3779b97f4a7c15, uint64(seed)^0xe7037ed1a0b428db))
		due := int64(editProbePeriod)
		for range count {
			interval := rng.ExpFloat64() * float64(mean)
			if math.IsNaN(interval) || math.IsInf(interval, 0) || interval >= float64(math.MaxInt64) {
				return nil, errors.New("poisson interval overflow")
			}
			delta := max(int64(1), int64(interval))
			if due > math.MaxInt64-delta {
				return nil, errors.New("poisson offered-plan duration overflow")
			}
			due += delta
			merged = append(merged, editProbeArrival{DueNS: due, Editor: editor})
		}
	}
	slices.SortFunc(merged, func(a, b editProbeArrival) int {
		return cmp.Or(cmp.Compare(a.DueNS, b.DueNS), cmp.Compare(a.Editor, b.Editor))
	})
	return merged[:count], nil
}

func makeEditProbePlan(c Corpus, seed int64, count int) (editProbePlan, error) {
	return makeEditProbeTrafficPlan(c, seed, count, 1)
}

func planEditProbe(c Corpus, o runOptions) (editProbePlan, error) {
	if c.EditSender == editProbeRestart && wantsGaugeSeries(o) {
		return editProbePlan{}, errors.New("live memory gauges and profiles are unsupported for the in-run restart sender: its process and health session are replaced")
	}
	mean := o.EditMean
	if mean == 0 {
		mean = editProbeMean
	}
	p, err := makeEditProbeTrafficPlanWithMean(c, o.Seed, o.EditCount, o.Editors, mean)
	if err != nil {
		return p, err
	}
	if o.EditGateModes {
		if p.Count != 100 || p.MeanNS != int64(editProbeMean) || p.Mode != "json" || p.Kind != "add_intrafile_call" || p.Tool != "edit.file" {
			return editProbePlan{}, errors.New("five-mode gate requires 100 call-addition edits with unchanged JSON edit.file ACKs")
		}
		p.GateModes = true
		modes := []string{"json", "toon", "compact", "facade", "cli"}
		for i := range p.Steps {
			p.Steps[i].ProbeMode = modes[i%len(modes)]
		}
	}
	if o.Agents == 0 {
		return p, nil
	}
	if c.EditSender == editProbeRestart {
		return editProbePlan{}, errors.New("the restart cell has no pure readers")
	}
	if o.Agents < 0 || o.Agents > editProbeMaxReaders {
		return editProbePlan{}, fmt.Errorf("edit-and-probe accepts 0..%d pure readers", editProbeMaxReaders)
	}
	// Readers first run a same-run no-edit window; the offered edit schedule,
	// relative to the edit origin after that window, is unchanged.
	p.Readers, p.NoEditWindow = o.Agents, int64(editProbeNoEditWindow)
	return p, nil
}

// A linked plan has one logical repository and separately identified checkouts.
// The runtime supplies the expanded private corpora; reporting starts from the
// one pinned primary. Both must describe the same immutable source fixture.
func editProbeLinkedCorpora(corpora []Corpus, linked int) ([]Corpus, error) {
	if linked < 1 || linked > 15 || len(corpora) != 1 && len(corpora) != linked+1 {
		return nil, errors.New("linked edit-probe routes require one primary and at most 15 linked checkouts")
	}
	primary := corpora[0]
	out := []Corpus{primary}
	for i := 1; i <= linked; i++ {
		label := fmt.Sprintf("linked-%d", i)
		if primary.Label == label {
			return nil, errors.New("primary label collides with a linked checkout identity")
		}
		c := primary
		c.Label = label
		if len(corpora) > 1 && corpora[i] != c {
			return nil, errors.New("linked checkout differs from the pinned primary fixture")
		}
		out = append(out, c)
	}
	return out, nil
}

// Routed plans reuse each corpus's existing finite plan. Every corpus sees
// the same global ordinals/arrivals; each fixed editor takes only its own
// corpus's request at that ordinal, so no secondary stream is reseeded.
func planEditProbeRoutes(corpora []Corpus, o runOptions) (editProbePlan, error) {
	if len(corpora) == 0 {
		return editProbePlan{}, errors.New("edit-and-probe requires a pinned corpus")
	}
	if o.EditRepos < 0 {
		return editProbePlan{}, errors.New("invalid edit repository count")
	}
	linked := o.Worktrees > 0
	if !linked && o.EditRepos <= 1 {
		return planEditProbe(corpora[0], o)
	}
	if linked {
		if o.EditRepos > 1 {
			return editProbePlan{}, errors.New("six repositories and linked checkouts are separate edit-probe cells")
		}
		var err error
		corpora, err = editProbeLinkedCorpora(corpora, o.Worktrees)
		if err != nil {
			return editProbePlan{}, err
		}
	} else if o.EditRepos != 6 || len(corpora) != 6 {
		return editProbePlan{}, errors.New("six-repository planning requires exactly six distinct pinned corpora")
	}
	if o.Editors < len(corpora) || o.EditCount != 100 || o.Agents != 0 {
		return editProbePlan{}, errors.New("routed planning requires at least one fixed editor per checkout, 100 edits and no pure readers")
	}
	plans := make([]editProbePlan, len(corpora))
	routes := make([]editProbeRoute, len(corpora))
	labels, sources := map[string]bool{}, map[string]bool{}
	for i, c := range corpora {
		if c.Label == "" || labels[c.Label] || !linked && sources[filepath.Clean(c.Source)] {
			return editProbePlan{}, errors.New("six-repository routes require distinct corpus labels and source repositories")
		}
		labels[c.Label], sources[filepath.Clean(c.Source)] = true, true
		p, err := planEditProbe(c, o)
		if err != nil {
			return editProbePlan{}, fmt.Errorf("route %s: %w", c.Label, err)
		}
		if p.Kind != "add_intrafile_call" || p.Tool != "edit.file" || p.Language != "go" || p.Mode != "json" || p.Sender != "" {
			return editProbePlan{}, errors.New("six-repository planning currently supports ordinary Go add_intrafile_call/edit.file JSON fixtures only; native, lifecycle, move and other output cells are unsupported")
		}
		plans[i] = p
		logicalRepo := c.Label
		if linked {
			logicalRepo = corpora[0].Label
		}
		routes[i] = editProbeRoute{ID: c.Label, Repo: logicalRepo, Commit: c.Commit, File: p.File, Callee: p.Callee, Kind: p.Kind, Tool: p.Tool, Language: p.Language, Mode: p.Mode}
	}
	out := plans[0]
	out.Routes = routes
	if linked {
		out.LinkedWorktrees = o.Worktrees
	}
	out.Steps = append([]editProbeStep(nil), out.Steps...)
	for i, step := range out.Steps {
		route := step.Editor % len(plans)
		out.Steps[i] = plans[route].Steps[i]
		out.Steps[i].Route = routes[route].ID
	}
	out.NotSelected = nil
	for _, limitation := range plans[0].NotSelected {
		if limitation != "several editors share one repository; 6-repository and linked-worktree routing are not implemented" {
			out.NotSelected = append(out.NotSelected, limitation)
		}
	}
	out.NotSelected = append(out.NotSelected, "routed cells support ordinary Go add_intrafile_call/edit.file JSON only; reference and live campaign acceptance remains separate")
	return out, nil
}

func editProbeRouteOperations(plan editProbePlan, corpora []Corpus) ([]request, error) {
	if len(corpora) == 0 {
		return nil, errors.New("edit-and-probe operations require a pinned corpus")
	}
	if len(plan.Routes) == 0 {
		for _, step := range plan.Steps {
			if step.Route != "" {
				return nil, errors.New("edit route has no pinned route metadata")
			}
		}
		return editProbeOperations(plan, corpora[0]), nil
	}
	if plan.LinkedWorktrees > 0 {
		var err error
		corpora, err = editProbeLinkedCorpora(corpora, plan.LinkedWorktrees)
		if err != nil {
			return nil, err
		}
	}
	if len(plan.Routes) != len(corpora) {
		return nil, errors.New("edit route corpus denominator differs from pinned metadata")
	}
	byRoute := make(map[string]Corpus, len(corpora))
	for i, c := range corpora {
		route := plan.Routes[i]
		logicalRepo := c.Label
		if plan.LinkedWorktrees > 0 {
			logicalRepo = corpora[0].Label
		}
		if _, duplicate := byRoute[route.ID]; duplicate || route.ID != c.Label || route.Repo != logicalRepo || route.Commit != c.Commit || route.File != c.File {
			return nil, errors.New("edit route metadata differs from its pinned corpus")
		}
		byRoute[route.ID] = c
	}
	var operations []request
	for _, step := range plan.Steps {
		c, found := byRoute[step.Route]
		if !found || step.Route == "" || step.Editor < 0 || step.Editor >= plan.Editors || step.Route != plan.Routes[step.Editor%len(plan.Routes)].ID {
			return nil, errors.New("offered edit has an unknown or foreign repository route")
		}
		operations = append(operations, step.Request)
		for _, oracle := range editProbeOracles(c, step) {
			operations = append(operations, oracle.call)
		}
	}
	return operations, nil
}

func makeEditProbeTrafficPlan(c Corpus, seed int64, count, editors int) (editProbePlan, error) {
	return makeEditProbeTrafficPlanWithMean(c, seed, count, editors, editProbeMean)
}

func makeEditProbeTrafficPlanWithMean(c Corpus, seed int64, count, editors int, mean time.Duration) (editProbePlan, error) {
	if err := validateCorpus(c); err != nil {
		return editProbePlan{}, err
	}
	if count != 1 && count != 100 {
		return editProbePlan{}, errors.New("edit-and-probe requires 100 edits, or explicit 1-edit diagnostic")
	}
	if editors < 1 || editors > editProbeMaxEditors {
		return editProbePlan{}, fmt.Errorf("edit-and-probe requires 1..%d fixed editors", editProbeMaxEditors)
	}
	if count == 1 && editors > 1 {
		return editProbePlan{}, errors.New("the 1-edit diagnostic has exactly one editor")
	}
	mode, err := editProbeOutputMode(c)
	if err != nil {
		return editProbePlan{}, err
	}
	tool, err := editProbeTool(c)
	if err != nil {
		return editProbePlan{}, err
	}
	kind := c.EditKind
	if kind == "" {
		kind = "add_intrafile_call"
	}
	if kind != "add_intrafile_call" && kind != "append_leaf" && kind != "rename" && kind != "delete" && kind != "modify_body" && kind != "modify_caller_body" && kind != "change_signature" && kind != "add_crossfile_call" && kind != "relocate" {
		return editProbePlan{}, errors.New("unsupported edit kind")
	}
	if mode == "compact" && kind != "add_intrafile_call" && kind != "add_crossfile_call" {
		return editProbePlan{}, fmt.Errorf("compact output cannot check %s: compact text omits node IDs, edge rows, signatures and freshness, so it proves only a call addition's presence", kind)
	}
	if mode == "cli" && (kind == "rename" || kind == "delete" || kind == "relocate") {
		return editProbePlan{}, fmt.Errorf("CLI probes cannot check %s: an expected absence must be the exact scoped not-found refusal, and the CLI reports a tool refusal only as an exit status and stderr text", kind)
	}

	if kind != "relocate" && (c.EditDestination != "" || c.EditDestinationContent != "") {
		return editProbePlan{}, errors.New("edit_destination applies only to the explicit move fixture")
	}
	if c.EditExpect != "" && tool != "refactor.move" {
		return editProbePlan{}, errors.New("edit_expect refusal applies only to TypeScript/Python relocation")
	}
	if kind == "relocate" && tool != "refactor.move" {
		return editProbePlan{}, errors.New("relocation requires the explicit Go move adapter")
	}
	// Each write carries whole-file content precomputed in ordinal order. With
	// several editors an out-of-order write silently drops another editor's
	// marker, and the next ordinal's write restores it; a batch's old_string
	// precondition fails visibly instead.
	if tool == "edit.write" && editors > 1 {
		return editProbePlan{}, errors.New("edit.write with several editors would silently drop a concurrent editor's marker: use edit.file or edit.batch")
	}
	if err := editProbeNativeSender(c, kind, tool, count, editors); err != nil {
		return editProbePlan{}, err
	}
	language := "go"
	if filepath.Ext(c.File) == ".ts" {
		language = "typescript"
	}
	if filepath.Ext(c.File) == ".py" {
		language = "python"
	}
	if (filepath.Ext(c.File) != ".go" && filepath.Ext(c.File) != ".ts" && filepath.Ext(c.File) != ".py") || c.EditAnchor == "" || strings.Count(c.EditTemplate, "{{marker}}") != 1 {
		return editProbePlan{}, errors.New("edit-and-probe cell requires Go or explicit TypeScript/Python fixture/stable anchor/one marker declaration")
	}
	selectedCallee := c.Symbol
	if kind == "add_crossfile_call" {
		if _, _, err := editProbeCrossFileCallee(c); err != nil {
			return editProbePlan{}, err
		}
		selectedCallee = c.EditCallee
		if prefix := editProbeCrossFileImport(c); prefix != "" && !strings.HasPrefix(c.EditInitialContent, prefix) {
			return editProbePlan{}, errors.New("crossfile fixture requires exact leading import")
		}
	} else if c.EditCallee != "" {
		return editProbePlan{}, errors.New("edit_callee applies only to explicit crossfile fixture")
	}
	separator := strings.LastIndex(selectedCallee, "::")
	if separator < 0 {
		return editProbePlan{}, errors.New("initial intrafile cell requires one plain callee symbol")
	}
	callee := selectedCallee[separator+2:]
	if !token.IsIdentifier(callee) {
		return editProbePlan{}, errors.New("initial intrafile cell requires one plain callee symbol")
	}
	probeMarker := "GortexEditProbeDeclaration"
	templateCallee := callee
	templateSource := strings.ReplaceAll(c.EditTemplate, "{{marker}}", probeMarker)
	if kind == "change_signature" {
		templateSource = editProbeSignaturePlain(c.File, templateSource)
	}
	if kind == "append_leaf" {
		templateCallee = ""
	}
	switch language {
	case "typescript":
		valid, correct := editProbeTypeScriptFixture(templateSource, probeMarker, templateCallee)
		if !valid || !correct {
			return editProbePlan{}, errors.New("TypeScript template must be a numeric zero-argument marker function returning the intrafile call")
		}
	case "python":
		valid, correct := editProbePythonFixture(templateSource, probeMarker, templateCallee)
		if !valid || !correct {
			return editProbePlan{}, errors.New("python template must be a top-level marker with one indented return of the intrafile call")
		}
	default:
		parsed, err := parser.ParseFile(token.NewFileSet(), "template.go", "package probe\n"+strings.ReplaceAll(c.EditTemplate, "{{marker}}", probeMarker), 0)
		if err != nil {
			return editProbePlan{}, fmt.Errorf("edit template is not standalone Go declarations: %w", err)
		}
		declaration, call := false, false
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == probeMarker && fn.Body != nil {
				declaration = true
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if expr, ok := n.(*ast.CallExpr); ok {
						if name, ok := expr.Fun.(*ast.Ident); ok && name.Name == callee {
							call = true
						}
					}
					return true
				})
			}
		}
		if !declaration || (kind != "append_leaf" && !call) || (kind == "append_leaf" && !editProbeLeafSource(c.File, strings.ReplaceAll(c.EditTemplate, "{{marker}}", probeMarker), probeMarker)) {
			return editProbePlan{}, errors.New("template must declare the unique marker function and actually call the selected intrafile callee")
		}
	}
	native := "native save/create/delete/hotfile/restart"
	if c.EditSender != "" {
		native = "other native senders run as their own cells"
	}
	p := editProbePlan{Version: 1, Arrival: "precomputed_open_loop_poisson", MeanNS: int64(mean), ProbePeriodNS: int64(editProbePeriod), Count: count, Seed: seed, Kind: kind, Tool: tool, Language: language, Mode: mode, Repo: c.Label, Commit: c.Commit, File: c.File, Callee: callee, SessionModel: "one_editor_plus_one_separate_prober_per_edit", NotSelected: []string{"other edit kinds/tools/languages/output modes", native, "largest-file/reference configurations require their own reviewed manifest"}}
	if kind == "change_signature" && language != "go" {
		p.NotSelected = append(p.NotSelected, "parameter detail is proved by exact source; native TS/Python signature metadata omits it and graph freshness uses the relative line witness")
	}
	if c.EditExpect == "refusal" {
		p.NotSelected = append(p.NotSelected, "TypeScript/Python relocation is offered only as the product's explicit refusal; Go relocation is the supported move cell")
	}
	p.Sender = c.EditSender
	if editors > 1 {
		p.Editors, p.SessionModel = editors, editProbeFixedEditorModel
		p.NotSelected = append(p.NotSelected, "several editors share one repository; 6-repository and linked-worktree routing are not implemented")
	}
	arrivals, err := editProbeArrivals(seed, count, editors, mean)
	if c.EditSender == editProbeHotFile {
		arrivals, err = editProbeHotFileArrivals(seed, count, mean)
	}
	if err != nil {
		return editProbePlan{}, err
	}
	pkg := ""
	if c.EditSender == editProbeCreateDelete {
		parsed, err := parser.ParseFile(token.NewFileSet(), c.File, c.EditInitialContent, parser.PackageClauseOnly)
		if err != nil {
			return editProbePlan{}, err
		}
		pkg = parsed.Name.Name
	}
	writeContent := c.EditInitialContent
	for i, arrival := range arrivals {
		due := arrival.DueNS
		marker := editProbeMarker(seed, i)
		id := fmt.Sprintf("load-edit-%016x-%03d", uint64(seed), i)
		r := editRequest(c, marker)
		if tool == "edit.symbol" {
			r = editProbeSymbolRequest(c, marker)
		}
		if (tool == "edit.write" || tool == "edit.batch") && kind != "rename" && kind != "delete" && kind != "modify_body" && kind != "modify_caller_body" && kind != "change_signature" {
			if strings.Contains(c.EditInitialContent, marker) {
				return editProbePlan{}, errors.New("planned marker already exists in write fixture")
			}
			if tool == "edit.batch" {
				r, writeContent = editProbeBatchRequest(c, writeContent, marker, fmt.Sprintf("load-batch-%016x-%03d", uint64(seed), i))
			} else {
				r, writeContent = editProbeWriteRequest(c, writeContent, marker)
			}
		}
		beforeSymbol := ""
		targetSymbol, followingSymbol := "", ""
		expectedGap := 0
		if kind == "rename" || kind == "delete" || kind == "modify_body" || kind == "modify_caller_body" || kind == "change_signature" {
			oldName := marker + "Before"
			if c.EditSender == editProbeEditorSave {
				oldName = editProbePlaceholder(seed, i)
			}
			beforeTemplate := c.EditTemplate
			if kind == "modify_body" || kind == "modify_caller_body" || kind == "change_signature" {
				beforeTemplate = c.EditBeforeTemplate
				if strings.Count(beforeTemplate, "{{marker}}") != 1 {
					return editProbePlan{}, errors.New("body change requires one explicit before-template declaration")
				}
			}
			before := strings.TrimSpace(strings.ReplaceAll(beforeTemplate, "{{marker}}", oldName))
			if strings.Count(c.EditInitialContent, before) != 1 || strings.Contains(c.EditInitialContent, marker+"(") {
				return editProbePlan{}, errors.New("rename/delete requires each exact original declaration and absent new declaration")
			}
			replacement := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", marker))
			if kind == "delete" {
				replacement = ""
			}
			if kind == "modify_body" || kind == "modify_caller_body" || kind == "change_signature" {
				replacement = strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", oldName))
				following := marker + "After"
				nextDeclaration := editProbeFollowingDeclaration(c.File, following)
				if kind == "change_signature" {
					nextDeclaration = editProbeFollowingCaller(c.File, following, oldName)
				}
				if strings.Count(c.EditInitialContent, before+"\n\n"+nextDeclaration) != 1 || strings.Count(replacement, "\n") <= strings.Count(before, "\n") {
					return editProbePlan{}, errors.New("body fixture requires adjacent retained follower and actual body line growth")
				}
				beforeValid := editProbeLeafSource(c.File, before, oldName)
				if kind == "modify_caller_body" {
					beforeValid = editProbeFixtureSource(c.File, before, oldName, "Alternate") && editProbeLeafSource(c.File, c.EditInitialContent, "Alternate")
				}
				if kind == "change_signature" {
					beforeValid = editProbeFixtureSource(c.File, before, oldName, callee) && editProbeSignatureHeaders(c.File, before, replacement, oldName)
				}
				if !beforeValid {
					return editProbePlan{}, errors.New("before body is not the declared numeric leaf or Alternate caller")
				}
				targetSymbol = c.File + "::" + oldName
				followingSymbol = c.File + "::" + following
				expectedGap = strings.Count(replacement, "\n") + 2
			}
			beforeSymbol = c.File + "::" + oldName
			switch tool {
			case "edit.file":
				r = request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file", "target": map[string]any{"file": c.File}, "match": before, "replacement": replacement}}
			case "edit.symbol":
				r = request{Tool: "edit", Operation: "symbol", Arguments: map[string]any{"operation": "symbol", "target": map[string]any{"symbol": beforeSymbol}, "match": before, "replacement": replacement}}
			case "edit.write", "edit.batch":
				next := strings.Replace(writeContent, before, replacement, 1)
				if tool == "edit.write" {
					r = request{Tool: "edit", Operation: "write", Arguments: map[string]any{"operation": "write", "target": map[string]any{"file": c.File}, "content": next}}
				} else {
					r = editProbeBatchContentRequest(c, writeContent, next, fmt.Sprintf("load-batch-%016x-%03d", uint64(seed), i))
				}
				writeContent = next
			}
		}
		if kind == "relocate" {
			beforeName := marker + "Before"
			before := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", beforeName))
			if strings.Count(c.EditInitialContent, before) != 1 || strings.Contains(c.EditDestinationContent, beforeName) {
				return editProbePlan{}, errors.New("move fixture requires exact original declaration and absent destination identity")
			}
			beforeSymbol = c.File + "::" + beforeName
			targetSymbol = c.EditDestination + "::" + beforeName
			r = request{Tool: "refactor", Operation: "move", Arguments: map[string]any{"operation": "move", "target": map[string]any{"symbol": beforeSymbol}, "destination": c.EditDestination}}
		}
		sender, after, afterEvent := editProbeNativeStep(c.EditSender, i)
		file, deleted := "", kind == "delete"
		if c.EditSender == editProbeCreateDelete {
			r, file, beforeSymbol = editProbeLifecycleStep(c, pkg, seed, i)
			deleted = file != "" && beforeSymbol != ""
		}
		if sender != "" {
			if r.Tool != "native" {
				r = editProbeNativeRequest(c.File, sender, r)
			}
			id = ""
		} else {
			if tool != "edit.batch" && tool != "refactor.move" {
				r.Arguments["options"] = map[string]any{"mutation_id": id}
			}
			r = editProbeWithOutput(r, mode)
		}
		hash, err := hashJSON(r)
		if err != nil {
			return editProbePlan{}, err
		}
		p.Steps = append(p.Steps, editProbeStep{Ordinal: i, ActivateNS: due - int64(editProbePeriod), DueNS: due, Editor: arrival.Editor, Marker: marker, MutationID: id, Request: r, RequestHash: hash, BeforeSymbol: beforeSymbol, Deleted: deleted, TargetSymbol: targetSymbol, FollowingSymbol: followingSymbol, ExpectedLineGap: expectedGap, Relocated: kind == "relocate", DestinationFile: c.EditDestination, ExpectedRefusal: c.EditExpect == "refusal", Sender: sender, After: after, AfterEvent: afterEvent, File: file})
	}
	return p, nil
}

func (p editProbePlan) budget(deadline time.Duration, builds int) (time.Duration, error) {
	if len(p.Steps) == 0 || deadline <= 0 || builds < 1 || builds > 2 {
		return 0, errors.New("invalid finite edit-probe budget")
	}
	tail := p.Steps[len(p.Steps)-1].DueNS
	if int64(deadline) > math.MaxInt64/int64(builds) {
		return 0, errors.New("edit-probe deadline overflow")
	}
	drain := int64(deadline) * int64(builds)
	if p.NoEditWindow < 0 || tail > math.MaxInt64-drain || tail+drain > math.MaxInt64-p.NoEditWindow {
		return 0, errors.New("edit-probe run budget overflow")
	}
	return time.Duration(p.NoEditWindow + tail + drain), nil
}

func editProbeOperations(plan editProbePlan, c Corpus) []request {
	var operations []request
	for _, step := range plan.Steps {
		operations = append(operations, step.Request)
		for _, oracle := range editProbeOracles(c, step) {
			operations = append(operations, oracle.call)
		}
	}
	return operations
}

// Each finite edit task waits against the same absolute origin. A blocked ACK
// cannot shift another offered arrival. Callbacks must honor the common context;
// all owned tasks join before the caller tears down sessions or the daemon.
func runEditProbeArrivals(ctx context.Context, p editProbePlan, origin time.Time, activate, dispatch func(context.Context, editProbeStep), sleep func(context.Context, time.Duration) error) error {
	var workers sync.WaitGroup
	for _, step := range p.Steps {
		workers.Go(func() {
			if sleep(ctx, time.Until(origin.Add(time.Duration(step.ActivateNS)))) != nil {
				return
			}
			activate(ctx, step)
			if sleep(ctx, time.Until(origin.Add(time.Duration(step.DueNS)))) != nil {
				return
			}
			dispatch(ctx, step)
		})
	}
	workers.Wait()
	return ctx.Err()
}
